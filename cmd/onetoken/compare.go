package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"onetoken/internal/battery"
	"onetoken/internal/collector"
	"onetoken/internal/config"
	"onetoken/internal/detector"
	"onetoken/internal/fingerprint"
	"onetoken/internal/preprocess"
	"onetoken/internal/report"
	"onetoken/internal/store"
	"onetoken/internal/verify"
)

// compareFlags 是 compare 命令参数（v0.22 直比模式：参考/待测端点均直传；
// v0.24：--model 必填 + 结果归档 results/<模型>/）。
type compareFlags struct {
	ref           directFlags // --ref-*
	target        directFlags // --target-*
	model         string
	targetModel   string
	k             int
	n             int
	tau           float64
	seed          int64
	concurrency   int
	budgetCalls   int
	reasoning     bool
	saveRef       bool
	refModelID    string
	refVersion    string
	saveResponses bool
	jsonOut       bool
	noReport      bool
	resultsDir    string
}

var compareFlag = &compareFlags{}

var compareCmd = &cobra.Command{
	Use:   "compare",
	Short: "直比两个端点（无需建档）：现场采集参考与待测，输出判定 + HTML 比较报告",
	Long: `compare 对用户直传的参考端点与待测端点分别采样（双路并发），现场构建
参考指纹并直接比对判定。无需先 enroll——参考端点与待测端点一样都是
ProviderConfig（--ref-* / --target-* 直传，密钥走环境变量）。

--model 必填：同时作为两端点 API 请求的模型名与归档文件夹名
（--target-model 可选覆盖待测端请求的模型字符串，缺省 = --model）。

τ 优先级：--tau 直传 > 校准库匹配档 > 内置参考线（direct 0.140 /
reasoning 0.16——未校准中位数基线，输出带「未校准」标注，正式使用前
建议 calibrate）。参考指纹默认不落库（--save-ref + --ref-model-id 可选
落库等价 enroll）。

结果归档（v0.24，默认开启）：判定完成后写入
<--results-dir>/<SanitizeID(--model)>/（默认 ./results/<模型>/）——
  reference.json  参考模板结果（参考指纹 + 端点元数据 + QC flags）
  target.json     待测模型结果（同构，分别命名）
  verdict.json    判定（score/threshold/tau_source/verdict/cells_detail/双方 QC）
  report.html     HTML 比较报告（--no-report 时不生成，三个 JSON 仍写）
固定文件名、原子写覆盖（重测同模型即更新）；测量有效性短路或采集失败
等判定未走出的情况不写归档（避免半截结果误导）。
--save-responses 追加 reference.jsonl/target.jsonl 原始响应取证（同文件夹）。

输出：stdout 简洁判定摘要（行尾附归档目录）；--json 结构化输出
（含 archive_dir 字段）。`,
	Example: `  onetoken compare --model qwen/qwen3-8b \
    --ref-base-url https://api.moonshot.ai --ref-api-key-env MOONSHOT_API_KEY \
    --target-base-url https://openrouter.ai --target-api-key-env OPENROUTER_API_KEY \
    --k 16 --n 15
  onetoken compare --model qwen/qwen3-8b --target-model qwen3-8b-int4 \
    --ref-base-url ... --ref-api-key-env K1 \
    --target-base-url ... --target-api-key-env K2 \
    --results-dir ./results --save-responses --json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		refP, refName, err := resolveProvider(cfg, "", compareFlag.ref)
		if err != nil {
			return fmt.Errorf("参考端点: %w", err)
		}
		tgtP, tgtName, err := resolveProvider(cfg, "", compareFlag.target)
		if err != nil {
			return fmt.Errorf("待测端点: %w", err)
		}
		refClient, err := newClient(refP, cfg.Settings)
		if err != nil {
			return err
		}
		tgtClient, err := newClient(tgtP, cfg.Settings)
		if err != nil {
			return err
		}
		st, err := setupStore()
		if err != nil {
			return err
		}
		b, err := loadBattery()
		if err != nil {
			return err
		}
		s := cfg.Settings

		// --tau 校验（与 audit 同口径：NaN/负值/Inf/>1 静默回落 auto 属笔误）
		if compareFlag.tau != 0 && (compareFlag.tau < 0 || compareFlag.tau > 1 || math.IsNaN(compareFlag.tau) || math.IsInf(compareFlag.tau, 0)) {
			return fmt.Errorf("--tau=%v 非法（需 0=auto 或 [0,1] 内的有限值）", compareFlag.tau)
		}
		if compareFlag.saveRef && compareFlag.refModelID == "" {
			return errors.New("--save-ref 需同时指定 --ref-model-id（参考指纹落库登记名，如 qwen/qwen3-8b）")
		}
		// --model/--target-model/--ref-model-id/--ref-version 控制字符过滤（安全审查：
		// stdout/JSON 输出的 ANSI 终端注入防护；与 store.SanitizeID 的非法字符集一致）
		compareFlag.model = sanitizeCLIString(compareFlag.model)
		compareFlag.targetModel = sanitizeCLIString(compareFlag.targetModel)
		compareFlag.refModelID = sanitizeCLIString(compareFlag.refModelID)
		compareFlag.refVersion = sanitizeCLIString(compareFlag.refVersion)
		compareFlag.resultsDir = sanitizeCLIString(compareFlag.resultsDir) // 派生路径进 stdout/错误信息，同基线过滤（审查 M2.12-安全 M1）
		// --model 必填（v0.24：两端点请求模型名 + 归档文件夹名；MarkFlagRequired 之外
		// 再校验空串——纯控制字符经上方过滤后为空或调用方绕过 flag 层时 fail-closed）
		if compareFlag.model == "" {
			return errors.New("--model 必填（两端点 API 请求模型名 + 归档文件夹名 results/<模型>/）")
		}
		// --target-model 缺省 = --model（v0.24：参考端始终用 --model）
		targetModel := compareFlag.targetModel
		if targetModel == "" {
			targetModel = compareFlag.model
		}

		// --save-ref 版本冲突预检（审查 M1：采集前检查，避免双端 240+ 次 API 调用
		// 浪费后才报冲突；enroll 的冲突语义同款）
		if compareFlag.saveRef {
			version := compareFlag.refVersion
			if version == "" {
				version = "compare-" + time.Now().UTC().Format("20060102T150405")
			}
			existing, err := st.LoadFingerprint(compareFlag.refModelID)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("compare: 读取既有指纹: %w", err)
			}
			if existing != nil && existing.Version == version {
				return fmt.Errorf("compare: 指纹版本冲突（同 %s@%s 已存在）", compareFlag.refModelID, version)
			}
		}

		k := compareFlag.k
		if k <= 0 {
			k = s.AuditK
		}
		n := compareFlag.n
		if n <= 0 {
			n = s.AuditN
		}
		seed := compareFlag.seed
		if seed == 0 {
			seed = time.Now().UnixNano()
		}
		channel := "direct"
		maxTokens := s.OutputTokenCap
		if compareFlag.reasoning {
			channel = "reasoning"
			maxTokens = s.ReasoningMaxTokens
		}
		tauBuiltin := s.BuiltinTauDirect
		if channel == "reasoning" {
			tauBuiltin = s.BuiltinTauReasoning
		}

		cells := pickCells(b.Cells(), k, seed) // 两端点同一 cell 子集（对齐比较）

		// ---- 双路并发采集（各自 provider/预算；内存 store 采集——v0.24：
		// --save-responses 改为归档阶段写模型文件夹 JSONL，不再落 data store） ----
		refStore := store.NewMemoryStore()
		tgtStore := store.NewMemoryStore()
		refID := fmt.Sprintf("compare-%s-r", randHex(8))
		tgtID := fmt.Sprintf("compare-%s-t", randHex(8))
		conc := resolveConcurrency(compareFlag.concurrency, refP)
		if tc := resolveConcurrency(compareFlag.concurrency, tgtP); tc < conc {
			conc = tc // 双路取较小并发（对齐节奏，防单端撑爆限流）
		}

		var (
			wg     sync.WaitGroup
			refRs  []*store.Response
			tgtRs  []*store.Response
			refErr error
			tgtErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			refRs, refErr = collector.RunBattery(runCtx(), refClient, refStore, b, cells, n, 1.0,
				collector.Options{ID: refID, Model: compareFlag.model, Concurrency: conc, MaxTokens: maxTokens,
					Budget: newBudget(compareFlag.budgetCalls), OnProgress: progressToStderr("compare 参考")})
		}()
		go func() {
			defer wg.Done()
			tgtRs, tgtErr = collector.RunBattery(runCtx(), tgtClient, tgtStore, b, cells, n, 1.0,
				collector.Options{ID: tgtID, Model: targetModel, Concurrency: conc, MaxTokens: maxTokens,
					Budget: newBudget(compareFlag.budgetCalls), OnProgress: progressToStderr("compare 待测")})
		}()
		wg.Wait()

		for _, e := range []struct {
			name string
			err  error
		}{{"参考", refErr}, {"待测", tgtErr}} {
			if isAbort(e.err) {
				return fmt.Errorf("compare %s端点: %w", e.name, e.err)
			}
		}
		failedRef := collector.CountTaskFailures(refErr)
		failedTgt := collector.CountTaskFailures(tgtErr)
		// 段级不可达（与 audit/enroll 同口径）：任一端失败率 ≥ 阈值 → 不可达
		if failRatio(failedRef, len(refRs)) >= s.UnreachableFailRatio ||
			failRatio(failedTgt, len(tgtRs)) >= s.UnreachableFailRatio {
			return fmt.Errorf("compare: 端点不可达（参考失败率 %.0f%% / 待测失败率 %.0f%%）",
				failRatio(failedRef, len(refRs))*100, failRatio(failedTgt, len(tgtRs))*100)
		}

		// ---- 派生列回填 + 参考指纹现场构建（与 enroll/verify 同语义） ----
		taskFC := compareTaskResolver(b)
		for _, r := range refRs {
			compareBackfill(r, taskFC)
		}
		for _, r := range tgtRs {
			compareBackfill(r, taskFC)
		}

		refFp, err := fingerprint.Build("ref", "1", "compare", time.Now().UTC(), refRs)
		if err != nil {
			return fmt.Errorf("compare: 参考指纹构建失败: %w", err)
		}
		refFp.Provider = refName
		refFp.Channel = channel
		// 参考端有效 cell 门槛（友好诊断：有效样本不足可能是端点答案空间与探针
		// 任务不匹配/拒绝率高，fail-closed 拒绝比较，不产生误导距离）
		if len(refFp.Cells) < s.KMinCells {
			return fmt.Errorf("compare: 参考端点有效 cell %d < k_min %d（%d 个采样中有效样本不足——端点答案空间与探针任务不匹配或拒绝率过高），比较拒绝",
				len(refFp.Cells), s.KMinCells, len(refRs))
		}

		// 参考端测量有效性（QC flags 展示；测量级 flag → 比较不可信，拒判）
		refScr := detector.Screen(refRs, detector.ScreenOptions{Settings: s, TaskForCell: taskFC})
		if refScr.Flags.ResponseCaching || (refScr.Flags.HiddenReasoning && channel != "reasoning") {
			return fmt.Errorf("compare: 参考端点测量有效性异常（%s），比较不可信",
				strings.Join(refScr.Flags.List(), ","))
		}

		// ---- 判定（τ 优先级：--tau > 校准档 > 内置参考线） ----
		cals, err := st.LoadCalibrations()
		if err != nil {
			return fmt.Errorf("读取校准库失败: %w", err)
		}
		res, err := verify.VerifyAudit(tgtRs, refFp, verify.Options{
			Settings:      s,
			Battery:       b,
			K:             k,
			N:             n,
			Scope:         "global",
			Calibrations:  cals,
			RefChannel:    channel,
			Channel:       channel,
			TargetChannel: tgtName,
			TauOverride:   compareFlag.tau,
			TauBuiltin:    tauBuiltin,
			FailedTasks:   failedTgt,
			TotalTasks:    len(tgtRs),
		})
		if err != nil {
			return err
		}

		// 待测端上游 provider（聚合器透传，解释不稳定）
		upstream := ""
		for _, r := range tgtRs {
			if r.Provider != "" {
				upstream = r.Provider
				break
			}
		}

		// ---- --save-ref：参考指纹落库（等价 enroll；先 models 后指纹可重入） ----
		var savedVersion string
		if compareFlag.saveRef {
			savedVersion, err = compareSaveRef(st, s, refFp, refScr, refName)
			if err != nil {
				return fmt.Errorf("compare: --save-ref 落库失败: %w", err)
			}
		}

		// ---- 输出 ----
		refQCFlags := refScr.Flags.List()
		tgtQCFlags := res.Flags.List()

		// ---- 结果归档（v0.24：默认开启，固定文件名写 <--results-dir>/<SanitizeID(--model)>/，
		// 重测同模型即覆盖更新）。测量有效性短路（TauSource 空，判定未走出）不写归档——
		// 避免半截结果误导；采集失败等错误路径在上方已 return，同样无归档。
		archiveDir := ""
		reportPath := ""
		if res.TauSource != "" {
			archiveDir = filepath.Join(compareFlag.resultsDir, store.SanitizeID(compareFlag.model))
			reportName := ""
			if !compareFlag.noReport {
				reportName = "report.html"
			}
			if err := compareWriteArchive(archiveDir, reportName, s, refP, tgtP, refFp, refScr, res,
				refRs, tgtRs, upstream, refName, tgtName, channel, targetModel, k, n, seed,
				compareFlag.saveResponses); err != nil {
				return err
			}
			if reportName != "" {
				reportPath = filepath.Join(archiveDir, reportName)
			}
		}

		if compareFlag.jsonOut {
			out := map[string]any{
				"verdict":      res.Verdict,
				"score":        res.Score,
				"threshold":    res.Threshold,
				"tau_source":   res.TauSource,
				"cells_used":   res.CellsUsed,
				"channel":      channel,
				"k":            k,
				"n":            n,
				"seed":         seed,
				"model":        compareFlag.model,
				"target_model": targetModel,
				"archive_dir":  archiveDir,
				"ref": map[string]any{
					"endpoint": refP.BaseURL, "provider": refName,
					"flags": refQCFlags, "samples": len(refRs),
				},
				"target": map[string]any{
					"endpoint": tgtP.BaseURL, "provider": tgtName,
					"flags": tgtQCFlags, "samples": len(tgtRs), "upstream": upstream,
				},
				"per_cell": res.CellsDetail,
				"reason":   res.Reason,
			}
			if reportPath != "" {
				out["report"] = reportPath
			}
			if savedVersion != "" {
				out["saved_ref"] = map[string]any{"model_id": compareFlag.refModelID, "version": savedVersion}
			}
			return printJSON(out)
		}
		line := fmt.Sprintf("compare: %s score=%.4f %s cells=%d channel=%s ref=%s target=%s",
			res.Verdict, res.Score, tauDisplay(res), res.CellsUsed, channel, refName, tgtName)
		if res.Reason != "" {
			line += "（" + res.Reason + "）"
		}
		if reportPath != "" {
			line += " 报告: " + reportPath
		}
		if archiveDir != "" {
			line += " 归档: " + archiveDir + string(filepath.Separator)
		}
		if savedVersion != "" {
			line += " 参考已落库: " + compareFlag.refModelID + "@" + savedVersion
		}
		fmt.Fprintln(cmd.OutOrStdout(), line)
		return nil
	},
}

// --- compare 结果归档（v0.24/M2.12） ---

// compareEndpointArchive 是归档的端点结果文件（reference.json / target.json）：
// 现场指纹（cells/t0_cells/channel）+ 端点元数据 + QC flags，顶层 schema_version=1。
type compareEndpointArchive struct {
	SchemaVersion int                       `json:"schema_version"`
	Model         string                    `json:"model"` // API 请求模型名
	BaseURL       string                    `json:"base_url"`
	Provider      string                    `json:"provider"`
	Protocol      string                    `json:"protocol"`
	Channel       string                    `json:"channel"`
	K             int                       `json:"k"`
	N             int                       `json:"n"`
	Seed          int64                     `json:"seed"`
	CollectedAt   string                    `json:"collected_at"`
	Cells         map[string]store.CellDist `json:"cells"`
	T0Cells       map[string]store.CellDist `json:"t0_cells,omitempty"`
	QCFlags       []string                  `json:"qc_flags"`
}

// compareVerdictArchive 是归档的判定结果文件（verdict.json）。
type compareVerdictArchive struct {
	SchemaVersion int                `json:"schema_version"`
	Model         string             `json:"model"`
	TargetModel   string             `json:"target_model"`
	Ref           compareEndpointRef `json:"ref"`
	Target        compareEndpointRef `json:"target"`
	Channel       string             `json:"channel"`
	K             int                `json:"k"`
	N             int                `json:"n"`
	Seed          int64              `json:"seed"`
	Score         float64            `json:"score"`
	Threshold     float64            `json:"threshold"`
	TauSource     string             `json:"tau_source"`
	Verdict       string             `json:"verdict"`
	CellsUsed     int                `json:"cells_used"`
	CellsDetail   map[string]float64 `json:"cells_detail"`
	RefQCFlags    []string           `json:"ref_qc_flags"`
	TargetQCFlags []string           `json:"target_qc_flags"`
	Upstream      string             `json:"upstream,omitempty"`
	Report        string             `json:"report,omitempty"` // 报告文件名（report.html；--no-report 时缺省）
	ComparedAt    string             `json:"compared_at"`      // UTC RFC3339
}

// compareEndpointRef 是 verdict.json 里的端点引用（base_url + provider 名）。
type compareEndpointRef struct {
	BaseURL  string `json:"base_url"`
	Provider string `json:"provider"`
}

// compareWriteArchive 写 compare 结果归档：reference.json / target.json /
// verdict.json → 可选 reference.jsonl/target.jsonl 取证 → 可选 report.html。
// 三个 JSON 走 store.WriteJSONAtomic（tmp+rename 原子覆盖，重测同模型即更新）；
// report.html/JSONL 为可再生产物直接覆盖写（规格取舍：store 仅导出 JSON 原子
// 写包装，设计文档 §7 M2.12 注记）。
func compareWriteArchive(archiveDir, reportName string, s config.Settings, refP, tgtP config.ProviderConfig,
	refFp *store.Fingerprint, refScr *detector.Result, res *verify.Result,
	refRs, tgtRs []*store.Response, upstream, refName, tgtName, channel, targetModel string,
	k, n int, seed int64, saveResponses bool) error {
	// 待测指纹现场构建（与参考同通道同语义；verify 内部的审计指纹不暴露，单独 Build）
	tgtFp, err := fingerprint.Build("target", "1", "compare", time.Now().UTC(), tgtRs)
	if err != nil {
		return fmt.Errorf("compare: 待测指纹构建失败: %w", err)
	}
	tgtFp.Provider = tgtName
	tgtFp.Channel = channel
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return fmt.Errorf("compare: 创建归档目录 %s: %w", archiveDir, err)
	}

	// reference.json / target.json：两端点结果分别命名（v0.24；键名 base_url 与
	// verdict.json 端点引用一致，审查 M2.12-符合性①）
	refArchive := compareEndpointArchive{
		SchemaVersion: 1,
		Model:         compareFlag.model,
		BaseURL:       refP.BaseURL,
		Provider:      refName,
		Protocol:      refP.Protocol,
		Channel:       channel,
		K:             k,
		N:             n,
		Seed:          seed,
		CollectedAt:   refFp.CollectedAt,
		Cells:         refFp.Cells,
		T0Cells:       refFp.T0Cells,
		QCFlags:       refScr.Flags.List(),
	}
	tgtArchive := compareEndpointArchive{
		SchemaVersion: 1,
		Model:         targetModel,
		BaseURL:       tgtP.BaseURL,
		Provider:      tgtName,
		Protocol:      tgtP.Protocol,
		Channel:       channel,
		K:             k,
		N:             n,
		Seed:          seed,
		CollectedAt:   tgtFp.CollectedAt,
		Cells:         tgtFp.Cells,
		T0Cells:       tgtFp.T0Cells,
		QCFlags:       res.Flags.List(),
	}
	verdictArchive := compareVerdictArchive{
		SchemaVersion: 1,
		Model:         compareFlag.model,
		TargetModel:   targetModel,
		Ref:           compareEndpointRef{BaseURL: refP.BaseURL, Provider: refName},
		Target:        compareEndpointRef{BaseURL: tgtP.BaseURL, Provider: tgtName},
		Channel:       channel,
		K:             k,
		N:             n,
		Seed:          seed,
		Score:         res.Score,
		Threshold:     res.Threshold,
		TauSource:     res.TauSource,
		Verdict:       res.Verdict,
		CellsUsed:     res.CellsUsed,
		CellsDetail:   res.CellsDetail,
		RefQCFlags:    refScr.Flags.List(),
		TargetQCFlags: res.Flags.List(),
		Upstream:      upstream,
		Report:        reportName,
		ComparedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	for _, f := range []struct {
		name string
		v    any
	}{
		{"reference.json", refArchive},
		{"target.json", tgtArchive},
		{"verdict.json", verdictArchive},
	} {
		if err := store.WriteJSONAtomic(filepath.Join(archiveDir, f.name), f.v); err != nil {
			return fmt.Errorf("compare: 写归档 %s: %w", f.name, err)
		}
	}

	// --save-responses：两端点原始响应 JSONL 取证（每行一个响应对象，含 raw_sha256；
	// 直接序列化内存 store 采集的响应切片——v0.24 起不再落 data store）
	if saveResponses {
		if err := writeResponsesJSONL(filepath.Join(archiveDir, "reference.jsonl"), refRs); err != nil {
			return fmt.Errorf("compare: 写 reference.jsonl: %w", err)
		}
		if err := writeResponsesJSONL(filepath.Join(archiveDir, "target.jsonl"), tgtRs); err != nil {
			return fmt.Errorf("compare: 写 target.jsonl: %w", err)
		}
	}

	// HTML 比较报告最后写（固定文件名 report.html，随归档覆盖更新；审查 M2.12-L3：
	// 三个 JSON 为权威载体先落盘，失败路径留下的归档仍可用——html 为非原子覆盖写）
	if reportName != "" {
		data := buildCompareData(s, refP, tgtP, refFp, tgtFp, refScr, res, upstream, refName, tgtName, channel)
		html, err := report.CompareReport(data)
		if err != nil {
			return fmt.Errorf("compare: 渲染报告失败: %w", err)
		}
		if err := os.WriteFile(filepath.Join(archiveDir, reportName), []byte(html), 0o644); err != nil {
			return fmt.Errorf("compare: 写报告 %s: %w", filepath.Join(archiveDir, reportName), err)
		}
	}
	return nil
}

// writeResponsesJSONL 将响应切片序列化为 JSONL（每行一个响应对象）。
func writeResponsesJSONL(path string, rs []*store.Response) error {
	var b strings.Builder
	for _, r := range rs {
		line, err := json.Marshal(r)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// compareSaveRef 将现场参考指纹落库（等价 enroll 的入库语义：
// 版本冲突拒绝、有效 cell 门槛、先 models 后指纹、版本链 superseded_by）。
func compareSaveRef(st *store.Store, s config.Settings, fp *store.Fingerprint, scr *detector.Result,
	refName string) (string, error) {
	version := compareFlag.refVersion
	if version == "" {
		version = "compare-" + time.Now().UTC().Format("20060102T150405")
	}
	existing, err := st.LoadFingerprint(compareFlag.refModelID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("读取既有指纹: %w", err)
	}
	var superseded string
	if existing != nil {
		if existing.Version == version {
			return "", fmt.Errorf("指纹版本冲突（同 %s@%s 已存在）", compareFlag.refModelID, version)
		}
		superseded = existing.Version
	}
	validCells := 0
	for _, cd := range fp.Cells {
		if cd.N >= s.MinValidSamples {
			validCells++
		}
	}
	if validCells < s.KMinCells {
		return "", fmt.Errorf("参考指纹有效 cell %d < k_min %d，落库拒绝", validCells, s.KMinCells)
	}
	fp.ModelID = compareFlag.refModelID
	fp.Version = version
	fp.RefSource = "official-api"
	fp.Provider = refName
	fp.SupersededBy = superseded
	fp.QCFlags = scr.Flags.List()
	models, err := st.LoadModels()
	if err != nil {
		return "", fmt.Errorf("读取模型目录: %w", err)
	}
	merged := appendModel(models, store.Model{
		ID: compareFlag.refModelID, RefSource: "official-api", Provider: refName,
		Notes: "saved from compare (ref " + refName + ")",
	})
	if err := st.SaveModels(merged); err != nil {
		return "", fmt.Errorf("保存模型目录: %w", err)
	}
	if err := st.SaveFingerprint(fp); err != nil {
		return "", fmt.Errorf("保存指纹: %w", err)
	}
	return version, nil
}

// buildCompareData 组装比较报告渲染数据。
func buildCompareData(s config.Settings, refP, tgtP config.ProviderConfig, refFp, tgtFp *store.Fingerprint,
	refScr *detector.Result, res *verify.Result, upstream, refName, tgtName, channel string) report.CompareData {
	refQC := endpointQC(refP.BaseURL, refName, refScr.Flags.List(), refFp)
	tgtQC := endpointQC(tgtP.BaseURL, tgtName, res.Flags.List(), tgtFp)
	if upstream != "" {
		tgtQC.Provider = upstream
	}
	// 三参考线 + 生效判定线（判定线 red，参考线灰）
	refLines := []report.RefLine{
		{Label: "同模型分裂半", Value: s.RefLineSameModel, Pct: report.Percent(s.RefLineSameModel)},
		{Label: "噪声底线（DriftBaseline）", Value: s.DriftBaseline, Pct: report.Percent(s.DriftBaseline)},
		{Label: "跨 provider 服务栈", Value: s.RefLineCrossProvider, Pct: report.Percent(s.RefLineCrossProvider)},
		{Label: "生效判定线 τ", Value: res.Threshold, Pct: report.Percent(res.Threshold), Decision: true},
	}
	// 分布对比：共同 cell（ref/target 均有的 cell），top 5 token
	distPairs := make([]report.DistPair, 0, len(res.CellsDetail))
	for _, row := range report.SortCellJSDs(res.CellsDetail) {
		rc, ok1 := refFp.Cells[row.Cell]
		tc, ok2 := tgtFp.Cells[row.Cell]
		if !ok1 || !ok2 {
			continue
		}
		distPairs = append(distPairs, report.DistPair{
			Cell: row.Cell,
			Ref:  report.TopTokens(rc.Dist, 5),
			Tgt:  report.TopTokens(tc.Dist, 5),
		})
	}
	return report.CompareData{
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
		Verdict:        res.Verdict,
		Score:          res.Score,
		ScorePct:       report.Percent(res.Score),
		Threshold:      res.Threshold,
		TauSource:      res.TauSource,
		TauSourceLabel: report.TauSourceLabel(res.TauSource),
		IsBuiltin:      res.TauSource == "builtin",
		CellsUsed:      res.CellsUsed,
		KMinCells:      res.KMinCells,
		Channel:        channel,
		Reason:         res.Reason,
		Ref:            refQC,
		Target:         tgtQC,
		Upstream:       upstream,
		RefLines:       refLines,
		CellJSDs:       report.SortCellJSDs(res.CellsDetail),
		DistPairs:      distPairs,
	}
}

// endpointQC 统计端点测量信息（有效率=各 cell ValidRate 均值，百分比 0..100）。
func endpointQC(endpoint, provider string, flags []string, fp *store.Fingerprint) report.EndpointQC {
	var rateSum, cells, samples int
	for _, cd := range fp.Cells {
		rateSum += int(cd.ValidRate * 100)
		cells++
		samples += cd.N
	}
	rate := 0.0
	if cells > 0 {
		rate = float64(rateSum) / float64(cells) // 0..100
	}
	return report.EndpointQC{Endpoint: endpoint, Provider: provider, Flags: flags,
		ValidRate: rate, ValidCells: cells, TotalCells: len(fp.Cells), Samples: samples}
}

// compareTaskResolver 从 battery 构造 TaskForCell（compare 本地副本；
// verify/enroll 包同构实现，避免跨包依赖重复暴露）。
func compareTaskResolver(b *battery.Battery) func(string) (preprocess.Task, bool) {
	byID := make(map[string]preprocess.Task, len(b.Tasks))
	for _, t := range b.Tasks {
		byID[t.ID] = preprocess.Task{ID: t.ID, AnswerSpace: t.AnswerSpace, SpaceSize: t.SpaceSize}
	}
	return func(cell string) (preprocess.Task, bool) {
		parts := strings.SplitN(cell, ":", 2)
		if len(parts) != 2 {
			return preprocess.Task{}, false
		}
		task, ok := byID[parts[0]]
		return task, ok
	}
}

// compareBackfill 回填派生列（与 enroll.backfill 同语义：归一化/分类输入为
// 提取的回答文本 Text，RawCompletion 含响应唯一 id 会污染分布键）。
func compareBackfill(r *store.Response, taskFC func(string) (preprocess.Task, bool)) {
	if r.Classification == "" || r.Normalized == "" {
		pc := preprocess.NormalizeClassify(r.Text, preprocess.Task{})
		if task, ok := taskFC(r.Cell); ok {
			pc = preprocess.NormalizeClassify(r.Text, task)
		}
		r.Classification = string(pc.Classification)
		r.Normalized = pc.Normalized
	}
}

// appendModel 合并模型登记（modelID 已存在则保留既有条目，与 enroll 同语义）。
func appendModel(models []store.Model, m store.Model) []store.Model {
	for i := range models {
		if models[i].ID == m.ID {
			return models
		}
	}
	return append(models, m)
}

func init() {
	f := compareFlag
	fl := compareCmd.Flags()
	fl.StringVar(&f.ref.baseURL, "ref-base-url", "", "参考端点 base_url（不含 /v1）")
	fl.StringVar(&f.ref.apiKeyEnv, "ref-api-key-env", "", "参考端点密钥环境变量名（密钥只走环境变量）")
	fl.StringVar(&f.ref.protocol, "ref-protocol", "auto", "参考端点协议 auto|responses|chat|anthropic")
	fl.StringVar(&f.ref.headers, "ref-headers", "", "参考端点附加头 k=v,k=v（禁敏感头）")
	fl.StringVar(&f.target.baseURL, "target-base-url", "", "待测端点 base_url（不含 /v1）")
	fl.StringVar(&f.target.apiKeyEnv, "target-api-key-env", "", "待测端点密钥环境变量名（密钥只走环境变量）")
	fl.StringVar(&f.target.protocol, "target-protocol", "auto", "待测端点协议 auto|responses|chat|anthropic")
	fl.StringVar(&f.target.headers, "target-headers", "", "待测端点附加头 k=v,k=v（禁敏感头）")
	fl.StringVar(&f.model, "model", "", "模型名（必填：两端点 API 请求模型名 + 归档文件夹名 results/<模型>/）")
	fl.StringVar(&f.targetModel, "target-model", "", "待测端点请求模型名覆盖（缺省 = --model；参考端始终用 --model）")
	fl.IntVar(&f.k, "k", 0, "比较 cell 数（默认 8；--k 40 全量）")
	fl.IntVar(&f.n, "n", 0, "每 cell 采样数（默认 15；--n 30 更稳）")
	fl.Float64Var(&f.tau, "tau", 0, "判定阈值（>0 直传覆盖校准库与内置线；0=auto）")
	fl.Int64Var(&f.seed, "seed", 0, "cell 选择种子（0=随机；两端点同种子对齐比较）")
	fl.IntVar(&f.concurrency, "concurrency", 0, "采集并发（默认 8，上限 256）")
	fl.IntVar(&f.budgetCalls, "budget-calls", 0, "每端点预算（调用次数上限，0=不限）")
	fl.BoolVar(&f.reasoning, "reasoning", false, "推理端点（系统 2：max_tokens 加大，τ 用 reasoning 内置线 0.16）")
	fl.BoolVar(&f.saveRef, "save-ref", false, "参考指纹落库（等价 enroll；需 --ref-model-id）")
	fl.StringVar(&f.refModelID, "ref-model-id", "", "--save-ref 的登记模型名（如 qwen/qwen3-8b）")
	fl.StringVar(&f.refVersion, "ref-version", "", "--save-ref 的指纹版本（默认 compare-<时间戳>）")
	fl.BoolVar(&f.saveResponses, "save-responses", false, "归档追加两端点响应 JSONL 取证（reference.jsonl/target.jsonl）")
	fl.BoolVar(&f.jsonOut, "json", false, "stdout 输出 JSON")
	fl.BoolVar(&f.noReport, "no-report", false, "归档不生成 report.html（三个 JSON 仍写）")
	fl.StringVar(&f.resultsDir, "results-dir", "results", "结果归档根目录（归档写入 <dir>/<模型>/；相对 cwd）")
	_ = compareCmd.MarkFlagRequired("model")
	_ = compareCmd.MarkFlagRequired("ref-base-url")
	_ = compareCmd.MarkFlagRequired("ref-api-key-env")
	_ = compareCmd.MarkFlagRequired("target-base-url")
	_ = compareCmd.MarkFlagRequired("target-api-key-env")
}

// tauDisplay 生成 stdout 摘要的 τ 展示：测量有效性短路（TauSource 空，未走到
// 判定）时 Threshold=0 无意义，显示「测量无效，未判定」而非 τ=0.0000（审查发现）。
func tauDisplay(res *verify.Result) string {
	if res.TauSource == "" {
		return "（测量无效，未判定）"
	}
	return fmt.Sprintf("τ=%.4f（%s）", res.Threshold, report.TauSourceLabel(res.TauSource))
}

// sanitizeCLIString 过滤控制字符（stdout/JSON 输出终端注入防护；与 store.sanitize
// 的非法字符集一致——NUL/控制字符拒绝，审查 Low 修复）。
func sanitizeCLIString(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\x00' || unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
