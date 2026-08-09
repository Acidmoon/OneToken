package main

import (
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

// compareFlags 是 compare 命令参数（v0.22 直比模式：参考/待测端点均直传）。
type compareFlags struct {
	ref           directFlags // --ref-*
	target        directFlags // --target-*
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
	reportDir     string
}

var compareFlag = &compareFlags{}

var compareCmd = &cobra.Command{
	Use:   "compare",
	Short: "直比两个端点（无需建档）：现场采集参考与待测，输出判定 + HTML 比较报告",
	Long: `compare 对用户直传的参考端点与待测端点分别采样（双路并发），现场构建
参考指纹并直接比对判定。无需先 enroll——参考端点与待测端点一样都是
ProviderConfig（--ref-* / --target-* 直传，密钥走环境变量）。

τ 优先级：--tau 直传 > 校准库匹配档 > 内置参考线（direct 0.140 /
reasoning 0.16——未校准中位数基线，输出带「未校准」标注，正式使用前
建议 calibrate）。参考指纹默认不落库（--save-ref + --ref-model-id 可选
落库等价 enroll；--save-responses 落两端点响应 JSONL 取证）。

输出：stdout 简洁判定摘要；默认生成 HTML 比较报告（距离 vs 三参考线
0.075/0.140/0.227 + 判定线、逐 cell JSD、分布对比、两端点 QC）；
--no-report 关闭报告、--report-dir 指定目录；--json 结构化输出。`,
	Example: `  onetoken compare \
    --ref-base-url https://api.moonshot.ai --ref-api-key-env MOONSHOT_API_KEY \
    --target-base-url https://openrouter.ai --target-api-key-env OPENROUTER_API_KEY \
    --k 16 --n 15
  onetoken compare --ref-base-url ... --ref-api-key-env K1 \
    --target-base-url ... --target-api-key-env K2 --json --no-report`,
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
		// --ref-model-id/--ref-version 控制字符过滤（安全审查：stdout/JSON 输出
		// 的 ANSI 终端注入防护；与 store.SanitizeID 的非法字符集一致）
		compareFlag.refModelID = sanitizeCLIString(compareFlag.refModelID)
		compareFlag.refVersion = sanitizeCLIString(compareFlag.refVersion)

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

		// ---- 双路并发采集（各自 provider/预算/存储；默认内存 store 不落库） ----
		var refStore collector.ResponseSink = store.NewMemoryStore()
		var tgtStore collector.ResponseSink = store.NewMemoryStore()
		if compareFlag.saveResponses {
			refStore = st // 取证：响应 JSONL 落盘（responses/<id>.jsonl）
			tgtStore = st
		}
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
				collector.Options{ID: refID, Model: "ref", Concurrency: conc, MaxTokens: maxTokens,
					Budget: newBudget(compareFlag.budgetCalls), OnProgress: progressToStderr("compare 参考")})
		}()
		go func() {
			defer wg.Done()
			tgtRs, tgtErr = collector.RunBattery(runCtx(), tgtClient, tgtStore, b, cells, n, 1.0,
				collector.Options{ID: tgtID, Model: "target", Concurrency: conc, MaxTokens: maxTokens,
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

		// ---- HTML 比较报告 ----
		reportPath := ""
		if !compareFlag.noReport {
			dir := compareFlag.reportDir
			if dir == "" {
				dir = filepath.Join(st.Root(), "reports")
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("创建报告目录 %s: %w", dir, err)
			}
			tgtFp, err := fingerprint.Build("target", "1", "compare", time.Now().UTC(), tgtRs)
			if err != nil {
				return fmt.Errorf("compare: 待测指纹构建失败（报告用）: %w", err)
			}
			data := buildCompareData(s, refP, tgtP, refFp, tgtFp, refScr, res, upstream, refName, tgtName, channel)
			html, err := report.CompareReport(data)
			if err != nil {
				return fmt.Errorf("compare: 渲染报告失败: %w", err)
			}
			reportPath = filepath.Join(dir, fmt.Sprintf("compare-%s-%s.html",
				time.Now().UTC().Format("20060102T150405"), randHex(4))) // 随机后缀防同秒覆盖（审查 Note）
			if err := os.WriteFile(reportPath, []byte(html), 0o644); err != nil {
				return fmt.Errorf("compare: 写报告 %s: %w", reportPath, err)
			}
		}

		// ---- 输出 ----
		refQCFlags := refScr.Flags.List()
		tgtQCFlags := res.Flags.List()
		if compareFlag.jsonOut {
			out := map[string]any{
				"verdict":    res.Verdict,
				"score":      res.Score,
				"threshold":  res.Threshold,
				"tau_source": res.TauSource,
				"cells_used": res.CellsUsed,
				"channel":    channel,
				"k":          k,
				"n":          n,
				"seed":       seed,
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
		if savedVersion != "" {
			line += " 参考已落库: " + compareFlag.refModelID + "@" + savedVersion
		}
		fmt.Fprintln(cmd.OutOrStdout(), line)
		return nil
	},
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
	fl.BoolVar(&f.saveResponses, "save-responses", false, "两端点响应 JSONL 落盘取证（默认内存不落库）")
	fl.BoolVar(&f.jsonOut, "json", false, "stdout 输出 JSON")
	fl.BoolVar(&f.noReport, "no-report", false, "不生成 HTML 比较报告")
	fl.StringVar(&f.reportDir, "report-dir", "", "报告输出目录（默认 <data>/reports/）")
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
