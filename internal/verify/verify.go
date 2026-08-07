// Package verify 实现审计判定（设计 §2.1、§3.4）：
//   - 指纹距离（复用 fingerprint.Distance，M1.3）；
//   - τ 匹配：按 (k, n, scope, ref_channel, target_channel) 精确匹配校准库，
//     无匹配档 → 拒绝审计（设计 §3.4：强制全电池校准或拒绝，实现选拒绝）；
//   - 三分支判定：pass（s ≤ τ−buf）/ suspicious（s > τ+buf）/
//     inconclusive（|s−τ| ≤ buf，τ CI 缺口裁决：绝对缓冲 TauInconclusiveBuffer，
//     需本地校准）；
//   - 测量有效性联动（M2.4 detector）：测量级 flag（hidden-reasoning /
//     response-caching / temperature-not-honored）或 unreachable → 不进指纹，
//     判 inconclusive；safety-layer-change 为告警项不阻断判定；
//     有效共同 cell（cellsUsed）< k_min → inconclusive（fail-closed，
//     防无共同 cell 时 Distance 返回 (0,0) 被误判 pass）。
//
// 安全基线：响应证据链哈希校验（RawSHA256，写侧强制的读侧补全）、
// 校准档 τ 合法性校验（有限且 ∈ [0,1]）、入参副本回填（不修改调用方切片）。
//
// inconclusive 处置口径（设计 §5）：为「待复核/触发重复审计」，不得视为通过；
// 连续 N 次 inconclusive 的 fail-closed 升级由 M2.7 CLI 工作流实现。
//
// 设计 §2.1 的 `Verify(ctx, target Provider, claimed Fingerprint, k, n)` 为
// 端到端形态（含采集），由 M2.7 CLI 组装；本包交付判定核心（Judge /
// MatchCalibration）与完整流程（VerifyAudit，输入已入库响应）。
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"onetoken/internal/battery"
	"onetoken/internal/config"
	"onetoken/internal/detector"
	"onetoken/internal/fingerprint"
	"onetoken/internal/preprocess"
	"onetoken/internal/store"
)

// Options 是 VerifyAudit 的输入。
type Options struct {
	Settings config.Settings
	Battery  *battery.Battery // 分类与 cell 语义（TaskForCell 来源）
	// K/N/Scope 审计采样参数（设计 §3.4 分档键 (k_cells, n_per_cell) × scope × 通道）：
	// K 为审计请求的 cell 子集大小、N 为每 cell 采样数、Scope 为校准分档范围
	// （global / family:<x> / size-tier；空串只匹配 Scope 为空的档，严格防误配）。
	K     int
	N     int
	Scope string
	// Channel 参考指纹采样通道（direct|reasoning，v0.19）：推理通道下
	// hidden-reasoning（思考链）属正常不短路，以 post-reasoning 回答有效性
	// （ValidCells/cellsUsed）为准；空=direct。
	Channel string
	// Calibrations 校准库（store.LoadCalibrations）；匹配失败 → 拒绝审计。
	Calibrations []store.Calibration
	// RefChannel/TargetChannel 校准分档通道（参考指纹来源通道 / 审计目标通道）。
	RefChannel    string
	TargetChannel string
	// RefusalBaseline 参考指纹 refusal 率（safety-layer-change 基线；nil=跳过）。
	RefusalBaseline *float64
	// TauOverride 直传阈值（>0 时跳过校准库匹配，直接以该值判定；冒烟/临时场景，
	// 如试点无校准库。正常审计保持 0=auto 查库）。须有限且 ∈ [0,1]。
	TauOverride float64
	// T0Responses 可选 T=0 探针响应（temperature-not-honored 检测）。
	T0Responses []*store.Response
	// FailedTasks/TotalTasks 采集失败统计（unreachable）。
	FailedTasks int
	TotalTasks  int
}

// Result 是判定结果。
type Result struct {
	Verdict     string // pass | suspicious | inconclusive（store.Verdict*）
	Score       float64
	Threshold   float64
	CellsUsed   int // B′ 共同有效 cell 数（k_min 判定基准，设计 §2.1）
	KMinCells   int
	Flags       detector.Flags
	Reason      string
	Calibration *store.Calibration
	CellsDetail map[string]float64 // 逐 cell JSD（§4.3 Audit.CellsDetail 用）
}

// ErrNoCalibration 校准库无匹配档（设计 §3.4：拒绝审计，强制全电池校准）。
var ErrNoCalibration = errors.New("verify: 无匹配校准档（(k,n,scope,通道) 精确匹配失败），需强制全电池校准或先校准")

// Judge 判定三分支（纯函数，设计 §3.4）：
//
//	s ≤ τ−buf → pass；s > τ+buf → suspicious；其余（|s−τ| ≤ buf）→ inconclusive。
//
// 边界：s == τ−buf 属 pass（≤）；s == τ+buf 属 inconclusive（suspicious 严格 >）；
// s == τ 属 inconclusive。score 为 NaN/Inf 时比较恒 false → inconclusive
// （安全方向）。buf<0 无意义（区间反转），由调用方校验（VerifyAudit 已拦）。
func Judge(score, tau, buf float64) string {
	switch {
	case score <= tau-buf:
		return store.VerdictPass
	case score > tau+buf:
		return store.VerdictSuspicious
	default:
		return store.VerdictInconclusive
	}
}

// MatchCalibration 按 (k, n, scope, ref_channel, target_channel) 精确匹配校准档
// （设计 §3.4 分档维度）。Scope 精确匹配（空串只命中空 Scope 档），防
// global/family 同键档顺序依赖误配。未命中返回 nil。
func MatchCalibration(cals []store.Calibration, k, n int, scope, refCh, tgtCh string) *store.Calibration {
	for i := range cals {
		c := &cals[i]
		if c.K == k && c.NPerCell == n && c.Scope == scope &&
			c.RefChannel == refCh && c.TargetChannel == tgtCh {
			return c
		}
	}
	return nil
}

// VerifyAudit 对一次审计的响应执行完整判定：
// 证据链校验 → 入参副本 → 分类回填 → detector.Screen → 测量有效性门
// （测量级 flag/unreachable/有效 cell < k_min → inconclusive）→ 指纹构建
// （仅 valid）→ 距离 → 校准档匹配（含 τ 合法性）→ Judge。
//
// 入参副作用：VerifyAudit 在内部副本上回填派生列，不修改调用方切片
// （并发安全；审查 S-M1）。响应需属于同一 audit；T=0 探针走 T0Responses，
// 不得混入主 responses（正温混合会让 Build 报 ErrTemperature）。
func VerifyAudit(responses []*store.Response, claimed *store.Fingerprint, opts Options) (*Result, error) {
	if claimed == nil {
		return nil, errors.New("verify: claimed 参考指纹为 nil")
	}
	if claimed.Cells == nil || len(claimed.Cells) == 0 {
		return nil, errors.New("verify: claimed 参考指纹为空（无 cell 分布）")
	}
	if opts.Battery == nil {
		return nil, errors.New("verify: Options.Battery 必填（cell 语义）")
	}
	if opts.Settings.KMinCells <= 0 {
		return nil, errors.New("verify: Settings.KMinCells 非法")
	}
	if opts.K <= 0 || opts.N <= 0 {
		return nil, errors.New("verify: Options.K/N 必填（校准分档键）")
	}
	if opts.Settings.TauInconclusiveBuffer < 0 {
		return nil, errors.New("verify: TauInconclusiveBuffer 非法（<0）")
	}

	// 0. 证据链哈希校验（写侧强制、读侧补全；审查 S-M3）：被篡改的响应
	// 不得进入判定（RawCompletion 与 RawSHA256 不匹配即报错）。
	rs := cloneResponses(responses) // 入参副本：回填不污染调用方（审查 S-M1）
	for _, r := range rs {
		if r == nil {
			continue
		}
		if r.RawSHA256 != "" && sha256Hex(r.RawCompletion) != r.RawSHA256 {
			return nil, fmt.Errorf("verify: 证据链哈希不匹配（cell %q 样本 %d 疑似被篡改）", r.Cell, r.SampleIdx)
		}
	}

	// 1. 分类回填（派生列一次性写入语义，设计 §2.2；副本上执行）
	taskForCell := taskResolver(opts.Battery)
	res := &Result{KMinCells: opts.Settings.KMinCells}
	for _, r := range rs {
		if r.Classification == "" || r.Normalized == "" {
			pc := preprocess.NormalizeClassify(r.Text, preprocess.Task{})
			if task, ok := taskForCell(r.Cell); ok {
				pc = preprocess.NormalizeClassify(r.Text, task)
			}
			r.Classification = string(pc.Classification)
			r.Normalized = pc.Normalized
		}
	}

	// 2. 测量有效性探测
	scr := detector.Screen(rs, detector.ScreenOptions{
		Settings:        opts.Settings,
		T0Responses:     opts.T0Responses,
		RefusalBaseline: opts.RefusalBaseline,
		FailedTasks:     opts.FailedTasks,
		TotalTasks:      opts.TotalTasks,
		TaskForCell:     taskForCell,
	})
	res.Flags = scr.Flags

	// 3. 测量有效性门（设计 §5：被标记测量不进指纹与判定）。
	// 注意：不可达/测量级 flag 短路先于校准档匹配（L4 注记：短路时
	// Calibration 为 nil、Threshold 为 0，调用方按 inconclusive 语义处理）。
	switch {
	case scr.Flags.Unreachable:
		res.Reason = "端点不可达（unreachable）"
		res.Verdict = store.VerdictInconclusive
		return res, nil
	case scr.Flags.HiddenReasoning && opts.Channel != "reasoning":
		// 非推理通道的推理痕迹 → 排除（论文方法论）；推理通道（v0.19）思考链
		// 属正常，不短路（post-reasoning 回答有效性由后续 k_min 门兜底）。
		res.Reason = "测量有效性 flag: " + joinFlags(scr.Flags)
		res.Verdict = store.VerdictInconclusive
		return res, nil
	case scr.Flags.ResponseCaching || (scr.Flags.TemperatureNotHonored && opts.Channel != "reasoning"):
		// response-caching 两通道均拦截；temperature-not-honored 仅非推理通道
		// 拦截——推理通道思考链在 T=0 下仍有随机性（DeepSeek 实测），
		// 该探针不适用（v0.19 适配）。
		res.Reason = "测量有效性 flag: " + joinFlags(scr.Flags)
		res.Verdict = store.VerdictInconclusive
		return res, nil
	}

	// 4. 指纹构建 + 距离（audit 指纹：仅 valid 样本入指纹，fingerprint.Build 语义；
	// version/refSource 为审计占位，不持久化）
	auditFp, err := fingerprint.Build("audit", "1", "audit", time.Now().UTC(), rs)
	if err != nil {
		return nil, fmt.Errorf("verify: 审计指纹构建失败: %w", err)
	}
	score, cellsUsed := fingerprint.Distance(auditFp, claimed)
	res.Score = score
	res.CellsUsed = cellsUsed
	res.CellsDetail = fingerprint.CellJSDs(auditFp, claimed)

	// 5. 有效共同 cell 门槛（k_min 双口径裁决 M2.5：以 B′ 参与数 cellsUsed 为准，
	// 与设计 §2.1「参与数供上层按有效 cell < k_min 判 inconclusive」一致；
	// ValidCells 为审计侧计数仅作报告。fail-closed：无共同 cell（0）不得判 pass）。
	if cellsUsed < opts.Settings.KMinCells {
		res.Reason = fmt.Sprintf("参与 cell %d < k_min %d", cellsUsed, opts.Settings.KMinCells)
		res.Verdict = store.VerdictInconclusive
		return res, nil
	}

	// 6. 判定（设计 §3.4）
	if opts.TauOverride > 0 {
		// 直传阈值模式（冒烟/临时）：跳过校准库匹配，τ 合法性校验同档。
		if !finite01(opts.TauOverride) {
			return nil, fmt.Errorf("verify: --tau=%v 非法（JSD 值域 [0,1]）", opts.TauOverride)
		}
		res.Threshold = opts.TauOverride
		res.Verdict = Judge(score, opts.TauOverride, opts.Settings.TauInconclusiveBuffer)
		return res, nil
	}
	// 校准档匹配（设计 §3.4：按请求 k/n/scope/通道精确匹配，无档拒绝审计）
	cal := MatchCalibration(opts.Calibrations, opts.K, opts.N, opts.Scope,
		opts.RefChannel, opts.TargetChannel)
	if cal == nil {
		return nil, fmt.Errorf("%w: (k=%d, n=%d, scope=%q, ref=%q, tgt=%q)",
			ErrNoCalibration, opts.K, opts.N, opts.Scope, opts.RefChannel, opts.TargetChannel)
	}
	// τ 合法性校验（审查 R-H2）：JSD 值域 [0,1]，篡改/损坏的校准档不得静默
	// 改变全部审计结论。
	if !finite01(cal.TauFPR1) {
		return nil, fmt.Errorf("verify: 校准档 τ=%v 非法（JSD 值域 [0,1]）", cal.TauFPR1)
	}
	res.Calibration = cal
	res.Threshold = cal.TauFPR1
	res.Verdict = Judge(score, cal.TauFPR1, opts.Settings.TauInconclusiveBuffer)
	return res, nil
}

// finite01 校验数值有限且 ∈ [0,1]。
func finite01(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1
}

// taskResolver 从 battery 构造 TaskForCell（cell "task:lang" → preprocess.Task）。
func taskResolver(b *battery.Battery) func(string) (preprocess.Task, bool) {
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

// cloneResponses 深拷贝响应切片（结构体逐元素复制；回填派生列不影响调用方）。
func cloneResponses(rs []*store.Response) []*store.Response {
	out := make([]*store.Response, len(rs))
	for i, r := range rs {
		if r == nil {
			continue
		}
		cp := *r
		out[i] = &cp
	}
	return out
}

// sha256Hex 计算 raw 的 SHA-256 十六进制摘要（证据链哈希比对）。
func sha256Hex(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// joinFlags 拼接 flag 列表（判定理由用）。
func joinFlags(f detector.Flags) string {
	out := f.List()
	if len(out) == 0 {
		return "(无)"
	}
	return out[0] + " 等"
}
