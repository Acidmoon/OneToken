// Package detector 实现测量有效性探测与清洗（设计 §5），依赖 preprocess 分类。
//
// 5 类 flag（与设计 §5 逐项对应）：
//   - hidden-reasoning：确定性证据（reasoning_tokens>0、finish_reason 截断
//     ["length"（OpenAI）/ "max_tokens"/"max_output_tokens"（Anthropic/Responses）]）
//     或退化启发式（completion_tokens ≥ CompletionTokenAnomalyMin，仅当协议层
//     无法提供字段时，设计 §5 降级口径）→ 统一标记，o 系 gate 语义（不接受
//     最低档 effort 或 reasoning_tokens>0 → 按论文排除；「接受度探测」归
//     M2.7 能力探测，本层只标记）；
//   - temperature-not-honored：T=0 探针（每 cell ≥T0ProbeN 样本、judged cell
//     数 ≥T0MinJudgedCells）按 cell 判确定性，确定性占比 < T0DeterministicRatio
//     → 端点忽略 T=0。实现为自洽性（同 cell 探针彼此一致）；与参考 T=0 分布
//     的比对为降级取舍（设计 §5 口径注释，由 M2.5 verify 主距离兜底）；
//   - response-caching：T=1.0 cell 内唯一答案数 ≤ CacheUniqueMax 且 n≥CacheMinN
//     （方差崩溃）。closed 空间唯一数达空间规模豁免（抛硬币 unique=2 正常）；
//     偏好类任务（ID 前缀 favorite，模型固有稳定偏好）豁免；open 非偏好任务
//     判定。命中=嫌疑（论文 14/2040 均良性），需按目标生态校准；延迟联合
//     条件默认禁用（CacheLatencyMaxMS=0）——延迟完全受端点控制，启用即
//     可被欺骗（设计已知局限）；
//   - safety-layer-change：审计 refusal 率 vs 参考基线，|差|>RefusalDriftThreshold；
//   - unreachable：采集失败任务占比 ≥ UnreachableFailRatio（失败计数由
//     collector.CountTaskFailures 从聚合错误统计）。
//
// 测量级 flag（hidden-reasoning / response-caching / temperature-not-honored）
// 的测量不进指纹与判定（设计 §5 末段），由调用方（verify/CLI）据 Result.Flags
// 跳过指纹构建；unreachable/safety-layer-change 为端点/审计级标记（无有效测量）。
// 有效 cell 数（ValidCells）与 KMinCells 供 verify 判 inconclusive（有效 cell < k_min）。
package detector

import (
	"math"
	"sort"
	"strings"

	"onetoken/internal/config"
	"onetoken/internal/preprocess"
	"onetoken/internal/store"
)

// Flag 是测量有效性标记（设计 §5 五类）。
type Flag string

const (
	FlagHiddenReasoning       Flag = "hidden-reasoning"
	FlagTemperatureNotHonored Flag = "temperature-not-honored"
	FlagResponseCaching       Flag = "response-caching"
	FlagSafetyLayerChange     Flag = "safety-layer-change"
	FlagUnreachable           Flag = "unreachable"
)

// Flags 是检测结果标记集合。
type Flags struct {
	HiddenReasoning       bool
	TemperatureNotHonored bool
	ResponseCaching       bool
	SafetyLayerChange     bool
	Unreachable           bool
}

// List 返回命中的 flag 列表（设计 §5 顺序）。
func (f Flags) List() []string {
	var out []string
	if f.HiddenReasoning {
		out = append(out, string(FlagHiddenReasoning))
	}
	if f.TemperatureNotHonored {
		out = append(out, string(FlagTemperatureNotHonored))
	}
	if f.ResponseCaching {
		out = append(out, string(FlagResponseCaching))
	}
	if f.SafetyLayerChange {
		out = append(out, string(FlagSafetyLayerChange))
	}
	if f.Unreachable {
		out = append(out, string(FlagUnreachable))
	}
	return out
}

// Any 是否命中任一 flag。
func (f Flags) Any() bool {
	return f.HiddenReasoning || f.TemperatureNotHonored || f.ResponseCaching ||
		f.SafetyLayerChange || f.Unreachable
}

// CellStats 是单个 cell 的统计。
type CellStats struct {
	Cell         string
	TaskID       string  // 任务语义 ID（经 TaskForCell；空=未知）
	Total        int     // 响应总数
	Valid        int     // 已分类：valid
	Invalid      int     // 已分类：invalid
	Refusal      int     // 已分类：refusal
	Empty        int     // 已分类：empty
	Unknown      int     // 无法分类（TaskForCell 未命中且分类未预填）
	UniqueRaw    int     // RawCompletion 去重唯一数（缓存签名信号）
	SpaceSize    int     // closed 空间大小（经 TaskForCell 获取；0=open/未知，缓存签名豁免用）
	AvgLatencyMS float64 // 平均延迟（LatencyMS>0 的响应）
}

// Result 是 Screen 的输出。
type Result struct {
	Flags        Flags
	ValidCells   int     // valid ≥ MinValidSamples 的 cell 数（verify 判 inconclusive）
	KMinCells    int     // 有效 cell < 此值 → inconclusive（M2.5 verify 使用）
	ValidRate    float64 // 模型级有效率（已分类基数）；无分类数据为 -1
	RefusalRate  float64 // 模型级 refusal 率；无分类数据为 -1
	ValidRateLow bool    // ValidRate < ValidRateQC（模型级 QC 指标，设计 §5「仅作 QC 不弃用」）
	// T=0 判定诊断（temperature-not-honored 校准用）
	T0Judged         int     // 参与判定的探针 cell 数（样本 ≥T0ProbeN）
	T0DetRatio       float64 // 确定性 cell 占比（0..1；T0Judged==0 时为 -1）
	T0NotJudged      bool    // 探针 cell 数不足 T0MinJudgedCells，未判定（调用方应检查并考虑重探）
	UnknownResponses int     // 无法分类的响应数（TaskForCell 未命中；>0 时 ValidRate/RefusalRate 口径不完整）
	CellStats        []CellStats
}

// ScreenOptions 是 Screen 的可选输入。
type ScreenOptions struct {
	Settings config.Settings
	// T0Responses：T=0 一致性探针响应（temperature-not-honored 检测；按 cell 判确定性）。
	T0Responses []*store.Response
	// RefusalBaseline：参考指纹 refusal 率基线；nil 表示无基线（跳过 safety-layer-change）。
	// 指针语义：零值 nil 即无基线，必须显式传入才启用（防误用 0 基线导致误报）。
	RefusalBaseline *float64
	// FailedTasks/TotalTasks：采集失败统计（unreachable；TotalTasks<=0 跳过）。
	// FailedTasks 可由 collector.CountTaskFailures 从 RunBattery 聚合错误统计。
	FailedTasks int
	TotalTasks  int
	// TaskForCell：cell → 归一化任务语义（preprocess 分类与缓存签名空间豁免）；
	// nil 时未分类响应计为 Unknown（不静默丢弃，见 Result.UnknownResponses）。
	TaskForCell func(cell string) (preprocess.Task, bool)
}

// Screen 对一批响应执行测量有效性探测（设计 §5）。
//
// 响应可来自主采集（T=1.0）或探针（T=0，经 T0Responses 单独传入）。
// 分类：响应已有 Classification 则直接使用；否则经 TaskForCell + preprocess 现算
// （确定性、无状态）；TaskForCell 未命中且分类为空 → 计 Unknown（可观测，不静默）。
func Screen(responses []*store.Response, opts ScreenOptions) *Result {
	res := &Result{KMinCells: opts.Settings.KMinCells, ValidRate: -1, RefusalRate: -1, T0DetRatio: -1}

	// 按 cell 分组并逐 cell 统计
	byCell := groupByCell(responses)
	for _, cell := range sortedCells(byCell) {
		cs := classifyCell(cell, byCell[cell], opts)
		res.CellStats = append(res.CellStats, cs)

		if !res.Flags.HiddenReasoning {
			res.Flags.HiddenReasoning = hasHiddenReasoning(byCell[cell], opts.Settings)
		}
		if !res.Flags.ResponseCaching {
			res.Flags.ResponseCaching = isCachingSuspicious(cs, opts.Settings)
		}
		res.UnknownResponses += cs.Unknown
		if cs.Valid >= opts.Settings.MinValidSamples {
			res.ValidCells++
		}
	}

	// 模型级有效/refusal 率（已分类基数；Unknown 不计入，避免静默失真）
	totalClassified, totalValid, totalRefusal := 0, 0, 0
	for _, cs := range res.CellStats {
		totalClassified += cs.Valid + cs.Invalid + cs.Refusal + cs.Empty
		totalValid += cs.Valid
		totalRefusal += cs.Refusal
	}
	if totalClassified > 0 {
		res.ValidRate = float64(totalValid) / float64(totalClassified)
		res.RefusalRate = float64(totalRefusal) / float64(totalClassified)
		res.ValidRateLow = res.ValidRate < opts.Settings.ValidRateQC // 模型级 QC（设计 §5）
	}

	// temperature-not-honored：T=0 探针确定性占比（设计 §5：n≥T0ProbeN 二项口径）
	res.T0Judged, res.T0DetRatio, res.Flags.TemperatureNotHonored = checkT0Determinism(opts)

	// safety-layer-change：refusal 率突变（无基线/无分类数据则跳过）
	if opts.RefusalBaseline != nil && res.RefusalRate >= 0 {
		if math.Abs(res.RefusalRate-*opts.RefusalBaseline) > opts.Settings.RefusalDriftThreshold {
			res.Flags.SafetyLayerChange = true
		}
	}

	// unreachable：重试统计持续失败（设计 §5：持续失败 → unreachable）
	if opts.TotalTasks > 0 && float64(opts.FailedTasks)/float64(opts.TotalTasks) >= opts.Settings.UnreachableFailRatio {
		res.Flags.Unreachable = true
	}
	return res
}

// hasHiddenReasoning 判定确定性证据或退化启发式（设计 §5 推理痕迹行）。
func hasHiddenReasoning(rs []*store.Response, s config.Settings) bool {
	for _, r := range rs {
		// 截断信号跨协议口径：OpenAI "length"；Anthropic "max_tokens"；
		// Responses 新版可能 "max_output_tokens"（审查 R-H1）。
		if r.ReasoningTokens > 0 || r.FinishReason == "length" ||
			r.FinishReason == "max_tokens" || r.FinishReason == "max_output_tokens" ||
			r.CompletionTokens >= s.CompletionTokenAnomalyMin {
			return true
		}
	}
	return false
}

// isCachingSuspicious 判定 cell 的 T=1.0 方差崩溃（response-caching）。
// 触发：唯一答案数 ≤ CacheUniqueMax 且 n ≥ CacheMinN；
// 豁免：closed 空间唯一数达到空间规模（抛硬币 unique=2 正常）；偏好类任务
// （ID 前缀 favorite，模型固有稳定偏好——favorite_number/color 等，审查 R-M2）；
// open 未知空间：判（随机词/动物固定输出即崩溃）。
func isCachingSuspicious(cs CellStats, s config.Settings) bool {
	if cs.UniqueRaw > s.CacheUniqueMax || cs.Total < s.CacheMinN {
		return false
	}
	// closed 空间：唯一数达空间规模属正常分布
	if cs.SpaceSize > 0 && cs.UniqueRaw >= cs.SpaceSize {
		return false
	}
	// 偏好类任务：固有稳定偏好，非缓存信号
	if strings.HasPrefix(cs.TaskID, "favorite") {
		return false
	}
	// 联合低延迟（可选，默认禁用）：命中=嫌疑（论文 14/2040 均良性，需本地校准）。
	// 延迟完全受端点控制，启用即被欺骗——设计已知局限，默认仅方差信号。
	if s.CacheLatencyMaxMS > 0 && cs.AvgLatencyMS > float64(s.CacheLatencyMaxMS) {
		return false
	}
	return true
}

// checkT0Determinism 统计 T=0 探针的确定性 cell 占比。
// 返回 (judged, ratio, flagged)：judged=参与判定的 cell 数（样本 ≥T0ProbeN）；
// ratio=确定性占比（judged==0 为 -1）；flagged=占比 < T0DeterministicRatio。
// 判定门槛：judged ≥ T0MinJudgedCells（防单 cell 误判）；样本不足的 cell 跳过；
// 全部不足时置 T0NotJudged（调用方可感知，不静默失效，审查 S-M5）。
func checkT0Determinism(opts ScreenOptions) (int, float64, bool) {
	s := opts.Settings
	if len(opts.T0Responses) == 0 || s.T0ProbeN <= 0 || s.T0MinJudgedCells <= 0 {
		return 0, -1, false
	}
	// 配置兜底（审查 R-M4）：ratio 非法（<=0 或 >1）视为禁用（不恒触发/不静默禁用）
	if s.T0DeterministicRatio <= 0 || s.T0DeterministicRatio > 1 {
		return 0, -1, false
	}
	byCell := groupByCell(opts.T0Responses)
	det, judged := 0, 0
	for _, cell := range sortedCells(byCell) {
		rs := byCell[cell]
		if len(rs) < s.T0ProbeN {
			continue
		}
		judged++
		if t0Deterministic(rs, opts.TaskForCell, cell) {
			det++
		}
	}
	if judged == 0 {
		return 0, -1, false
	}
	ratio := float64(det) / float64(judged)
	if judged < s.T0MinJudgedCells {
		return judged, ratio, false // 判定不足：不触发（T0NotJudged 由调用方经 T0Judged 感知）
	}
	return judged, ratio, ratio < s.T0DeterministicRatio
}

// t0Deterministic 判定同一 cell 的 T=0 响应是否一致（T=0 应确定性）。
// 比较口径：TaskForCell 可用时按归一化结果比较（容错空白/标点变体，审查 R-L6）；
// 否则回退 raw 精确比较。
func t0Deterministic(rs []*store.Response, taskForCell func(string) (preprocess.Task, bool), cell string) bool {
	if len(rs) == 0 {
		return false
	}
	first := normalizeT0(rs[0], taskForCell, cell)
	for _, r := range rs[1:] {
		if normalizeT0(r, taskForCell, cell) != first {
			return false
		}
	}
	return true
}

// normalizeT0 取 T=0 响应的比较值（归一化或 raw）。
func normalizeT0(r *store.Response, taskForCell func(string) (preprocess.Task, bool), cell string) string {
	if taskForCell != nil {
		if _, ok := taskForCell(cell); ok {
			return preprocess.Normalize(r.RawCompletion)
		}
	}
	return r.RawCompletion
}

// classifyCell 统计单 cell：分类计数、唯一答案数、空间大小、平均延迟。
// 分类已预填则直接使用；否则经 TaskForCell 现算；未命中且未预填 → Unknown。
func classifyCell(cell string, rs []*store.Response, opts ScreenOptions) CellStats {
	cs := CellStats{Cell: cell, Total: len(rs)}
	uniq := make(map[string]bool, len(rs))
	var latencySum int64
	var latencyN int
	for _, r := range rs {
		if r.RawCompletion != "" {
			uniq[r.RawCompletion] = true
		}
		if r.LatencyMS > 0 {
			latencySum += r.LatencyMS
			latencyN++
		}
		cls := r.Classification
		if opts.TaskForCell != nil {
			if task, ok := opts.TaskForCell(cell); ok {
				if cs.TaskID == "" {
					cs.TaskID = task.ID
				}
				if cls == "" {
					pc := preprocess.NormalizeClassify(r.RawCompletion, task)
					cls = string(pc.Classification)
				}
				// 空间大小与分类解耦：已分类响应也需 SpaceSize（缓存签名豁免用）
				if task.AnswerSpace == "closed" && task.SpaceSize > 0 {
					cs.SpaceSize = task.SpaceSize
				}
			}
		}
		switch cls {
		case store.ClassValid:
			cs.Valid++
		case store.ClassInvalid:
			cs.Invalid++
		case store.ClassRefusal:
			cs.Refusal++
		case store.ClassEmpty:
			cs.Empty++
		default:
			cs.Unknown++ // 未分类（TaskForCell 未命中且未预填）：可观测，不静默
		}
	}
	cs.UniqueRaw = len(uniq)
	if latencyN > 0 {
		cs.AvgLatencyMS = float64(latencySum) / float64(latencyN)
	}
	return cs
}

// groupByCell 按 cell 分组响应。
func groupByCell(rs []*store.Response) map[string][]*store.Response {
	out := make(map[string][]*store.Response, len(rs))
	for _, r := range rs {
		out[r.Cell] = append(out[r.Cell], r)
	}
	return out
}

// sortedCells 返回排序后的 cell 键（确定性输出顺序）。
func sortedCells(m map[string][]*store.Response) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
