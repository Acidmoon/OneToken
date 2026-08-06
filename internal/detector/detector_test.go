package detector

import (
	"fmt"
	"testing"

	"onetoken/internal/config"
	"onetoken/internal/preprocess"
	"onetoken/internal/store"
)

func testSettings() config.Settings {
	return config.DefaultSettings()
}

// resp 构造测试响应（分类可预填，或留空走 TaskForCell）。
func resp(cell string, idx int, raw, cls string) *store.Response {
	return &store.Response{
		Cell:             cell,
		SampleIdx:        idx,
		RawCompletion:    raw,
		Classification:   cls,
		CompletionTokens: 1,
		FinishReason:     "stop",
	}
}

func respFull(cell string, idx int, raw string, reason int, finish string, comp int) *store.Response {
	return &store.Response{
		Cell: cell, SampleIdx: idx, RawCompletion: raw,
		ReasoningTokens: reason, FinishReason: finish, CompletionTokens: comp,
	}
}

func screenOpts(s config.Settings) ScreenOptions {
	return ScreenOptions{Settings: s}
}

// ---- hidden-reasoning：确定性证据 + 退化启发式 ----

func TestHiddenReasoningTriggered(t *testing.T) {
	s := testSettings()
	cases := []struct {
		name string
		rs   []*store.Response
	}{
		{"reasoning_tokens>0", []*store.Response{respFull("c", 0, "7", 42, "stop", 1)}},
		{"finish_reason=length", []*store.Response{respFull("c", 0, "7", 0, "length", 16)}},
		{"Anthropic max_tokens 截断", []*store.Response{respFull("c", 0, "7", 0, "max_tokens", 20)}},
		{"Responses max_output_tokens 截断", []*store.Response{respFull("c", 0, "7", 0, "max_output_tokens", 20)}},
		{"退化启发式 tokens≥40", []*store.Response{respFull("c", 0, "7", 0, "stop", 45)}},
		{"多个响应其一命中", []*store.Response{
			respFull("c", 0, "7", 0, "stop", 1),
			respFull("c", 1, "7", 8, "stop", 1),
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Screen(c.rs, screenOpts(s))
			if !res.Flags.HiddenReasoning {
				t.Fatal("应标记 hidden-reasoning")
			}
			if got := res.Flags.List(); len(got) != 1 || got[0] != string(FlagHiddenReasoning) {
				t.Fatalf("List=%v", got)
			}
		})
	}
}

func TestHiddenReasoningNotTriggered(t *testing.T) {
	s := testSettings()
	rs := []*store.Response{
		respFull("c", 0, "7", 0, "stop", 1),
		respFull("c", 1, "8", 0, "stop", 2),
		respFull("c", 2, "9", 0, "stop", 16), // 16 token 属中间带（7–39）不触发（灰区口径，见设计）
	}
	res := Screen(rs, screenOpts(s))
	if res.Flags.HiddenReasoning {
		t.Fatal("正常响应不应标记 hidden-reasoning")
	}
}

// ---- temperature-not-honored：T=0 探针确定性占比 ----

func TestTemperatureNotHonoredTriggered(t *testing.T) {
	s := testSettings() // T0ProbeN=5, T0DeterministicRatio=0.80
	var t0 []*store.Response
	// 5 个探针 cell，每 cell 5 样本；仅 2/5 确定性（40% < 80%）→ 触发
	for c := 0; c < 5; c++ {
		cell := fmt.Sprintf("probe%d", c)
		deterministic := c < 2
		for i := 0; i < s.T0ProbeN; i++ {
			raw := fmt.Sprintf("d%d", c)
			if !deterministic {
				raw = fmt.Sprintf("v%d-%d", c, i)
			}
			t0 = append(t0, resp(cell, i, raw, store.ClassValid))
		}
	}
	res := Screen(nil, ScreenOptions{Settings: s, T0Responses: t0})
	if !res.Flags.TemperatureNotHonored {
		t.Fatal("确定性占比 40% < 80% 应标记 temperature-not-honored")
	}
}

func TestTemperatureNotHonoredNotTriggered(t *testing.T) {
	s := testSettings()
	var t0 []*store.Response
	// 5 个探针 cell 全部确定性（100% ≥ 80%）→ 不触发
	for c := 0; c < 5; c++ {
		for i := 0; i < s.T0ProbeN; i++ {
			t0 = append(t0, resp(fmt.Sprintf("probe%d", c), i, fmt.Sprintf("d%d", c), store.ClassValid))
		}
	}
	res := Screen(nil, ScreenOptions{Settings: s, T0Responses: t0})
	if res.Flags.TemperatureNotHonored {
		t.Fatal("全确定性不应标记")
	}
}

func TestTemperatureNotHonoredInsufficientSamples(t *testing.T) {
	s := testSettings()
	// 探针 cell 样本不足（<5）→ 跳过判定（不误报）
	var t0 []*store.Response
	for c := 0; c < 3; c++ {
		for i := 0; i < 2; i++ { // 每 cell 仅 2 样本
			t0 = append(t0, resp(fmt.Sprintf("probe%d", c), i, fmt.Sprintf("v%d-%d", c, i), store.ClassValid))
		}
	}
	res := Screen(nil, ScreenOptions{Settings: s, T0Responses: t0})
	if res.Flags.TemperatureNotHonored {
		t.Fatal("样本不足不应判定")
	}
}

func TestTemperatureNotHonoredNoProbe(t *testing.T) {
	res := Screen(nil, screenOpts(testSettings()))
	if res.Flags.TemperatureNotHonored {
		t.Fatal("无探针不应标记")
	}
}

// ---- response-caching：T=1.0 方差崩溃 ----

func TestResponseCachingTriggered(t *testing.T) {
	s := testSettings() // CacheUniqueMax=2, CacheMinN=10
	var rs []*store.Response
	for i := 0; i < 15; i++ {
		rs = append(rs, resp("c", i, "42", store.ClassValid)) // 全部相同（unique=1）
	}
	res := Screen(rs, screenOpts(s))
	if !res.Flags.ResponseCaching {
		t.Fatal("唯一答案 1 ≤ 2 且 n=15 ≥ 10 应标记 response-caching")
	}
}

func TestResponseCachingNotTriggeredNormalVariance(t *testing.T) {
	s := testSettings()
	var rs []*store.Response
	for i := 0; i < 15; i++ {
		rs = append(rs, resp("c", i, fmt.Sprintf("w%d", i), store.ClassValid)) // unique=15
	}
	res := Screen(rs, screenOpts(s))
	if res.Flags.ResponseCaching {
		t.Fatal("正常方差不应标记")
	}
}

func TestResponseCachingNotTriggeredSmallN(t *testing.T) {
	s := testSettings()
	var rs []*store.Response
	for i := 0; i < 5; i++ { // n=5 < CacheMinN=10
		rs = append(rs, resp("c", i, "42", store.ClassValid))
	}
	res := Screen(rs, screenOpts(s))
	if res.Flags.ResponseCaching {
		t.Fatal("样本不足不应判缓存")
	}
}

func TestResponseCachingNotTriggeredCoinFlip(t *testing.T) {
	// 2 值空间（抛硬币）T=1.0 正常 unique=2，不误报（空间校准豁免）
	s := testSettings()
	taskForCell := func(cell string) (preprocess.Task, bool) {
		if cell != "coin_flip:en" {
			return preprocess.Task{}, false
		}
		return preprocess.Task{ID: "coin_flip", AnswerSpace: "closed", SpaceSize: 2}, true
	}
	var rs []*store.Response
	for i := 0; i < 30; i++ {
		raw := "h"
		if i%2 == 1 {
			raw = "t"
		}
		rs = append(rs, resp("coin_flip:en", i, raw, store.ClassValid))
	}
	res := Screen(rs, ScreenOptions{Settings: s, TaskForCell: taskForCell})
	if res.Flags.ResponseCaching {
		t.Fatal("2 值空间 unique=2 是正常分布，不应误报缓存")
	}
}

func TestResponseCachingCoinFlipAllSameTriggered(t *testing.T) {
	// 抛硬币 n=30 全同（unique=1）：2^-29 概率，确系方差崩溃
	s := testSettings()
	taskForCell := func(cell string) (preprocess.Task, bool) {
		return preprocess.Task{ID: "coin_flip", AnswerSpace: "closed", SpaceSize: 2}, true
	}
	var rs []*store.Response
	for i := 0; i < 30; i++ {
		rs = append(rs, resp("coin_flip:en", i, "h", store.ClassValid))
	}
	res := Screen(rs, ScreenOptions{Settings: s, TaskForCell: taskForCell})
	if !res.Flags.ResponseCaching {
		t.Fatal("全同（unique=1）应标记方差崩溃")
	}
}

func TestResponseCachingPreferenceTaskExempt(t *testing.T) {
	// 偏好类任务（favorite_number）：模型固有稳定偏好，固定答案不误报（审查 R-M2）
	s := testSettings()
	taskForCell := func(cell string) (preprocess.Task, bool) {
		return preprocess.Task{ID: "favorite_number", AnswerSpace: "open"}, true
	}
	var rs []*store.Response
	for i := 0; i < 15; i++ {
		rs = append(rs, resp("favorite_number:en", i, "7", store.ClassValid))
	}
	res := Screen(rs, ScreenOptions{Settings: s, TaskForCell: taskForCell})
	if res.Flags.ResponseCaching {
		t.Fatal("偏好任务稳定输出不应误报缓存")
	}
}

func TestResponseCachingOpenSpaceTriggered(t *testing.T) {
	// open 非偏好任务（random_word）：固定输出即方差崩溃
	s := testSettings()
	taskForCell := func(cell string) (preprocess.Task, bool) {
		return preprocess.Task{ID: "random_word", AnswerSpace: "open"}, true
	}
	var rs []*store.Response
	for i := 0; i < 15; i++ {
		rs = append(rs, resp("random_word:en", i, "banana", store.ClassValid))
	}
	res := Screen(rs, ScreenOptions{Settings: s, TaskForCell: taskForCell})
	if !res.Flags.ResponseCaching {
		t.Fatal("open 非偏好任务全同应标记")
	}
}

// ---- safety-layer-change：refusal 率突变 ----

func refusalResp(cell string, idx int, refusal bool) *store.Response {
	cls := store.ClassValid
	if refusal {
		cls = store.ClassRefusal
	}
	return resp(cell, idx, "x", cls)
}

func TestSafetyLayerChangeTriggered(t *testing.T) {
	s := testSettings() // RefusalDriftThreshold=0.15
	var rs []*store.Response
	for i := 0; i < 10; i++ {
		rs = append(rs, refusalResp("c", i, i%2 == 0)) // refusal 率 50%
	}
	res := Screen(rs, ScreenOptions{Settings: s, RefusalBaseline: fl(0.05)})
	if !res.Flags.SafetyLayerChange {
		t.Fatal("|0.5-0.05| > 0.15 应标记 safety-layer-change")
	}
}

func TestSafetyLayerChangeNotTriggered(t *testing.T) {
	s := testSettings()
	var rs []*store.Response
	for i := 0; i < 10; i++ {
		rs = append(rs, refusalResp("c", i, false))
	}
	res := Screen(rs, ScreenOptions{Settings: s, RefusalBaseline: fl(0.05)})
	if res.Flags.SafetyLayerChange {
		t.Fatal("|0-0.05| ≤ 0.15 不应标记")
	}
}

func TestSafetyLayerChangeNoBaseline(t *testing.T) {
	s := testSettings()
	var rs []*store.Response
	for i := 0; i < 10; i++ {
		rs = append(rs, refusalResp("c", i, i%2 == 0))
	}
	res := Screen(rs, ScreenOptions{Settings: s}) // 无基线（缺省 nil）
	if res.Flags.SafetyLayerChange {
		t.Fatal("无基线不应标记")
	}
	if res.RefusalRate < 0 {
		t.Fatal("RefusalRate 应被计算")
	}
}

// ---- unreachable：失败率 ----

func TestUnreachableTriggered(t *testing.T) {
	s := testSettings() // UnreachableFailRatio=0.8
	res := Screen(nil, ScreenOptions{Settings: s, FailedTasks: 10, TotalTasks: 10})
	if !res.Flags.Unreachable {
		t.Fatal("全失败应标记 unreachable")
	}
	res = Screen(nil, ScreenOptions{Settings: s, FailedTasks: 9, TotalTasks: 10})
	if !res.Flags.Unreachable {
		t.Fatal("失败率 90% ≥ 80% 应标记")
	}
}

func TestUnreachableNotTriggered(t *testing.T) {
	s := testSettings()
	res := Screen(nil, ScreenOptions{Settings: s, FailedTasks: 1, TotalTasks: 10})
	if res.Flags.Unreachable {
		t.Fatal("失败率 10% < 80% 不应标记")
	}
	res = Screen(nil, ScreenOptions{Settings: s, FailedTasks: 0, TotalTasks: 0})
	if res.Flags.Unreachable {
		t.Fatal("无失败统计不应标记")
	}
}

// ---- 有效 cell 计数（verify 判 inconclusive 的信号） ----

func TestValidCellsCount(t *testing.T) {
	s := testSettings() // MinValidSamples=10, KMinCells=3
	valid10 := func() []*store.Response {
		var out []*store.Response
		for i := 0; i < 10; i++ {
			out = append(out, resp("a", i, "7", store.ClassValid))
		}
		return out
	}
	valid5 := func() []*store.Response {
		var out []*store.Response
		for i := 0; i < 5; i++ {
			out = append(out, resp("b", i, "8", store.ClassValid))
		}
		return out
	}
	rs := append(valid10(), valid5()...)
	res := Screen(rs, screenOpts(s))
	if res.ValidCells != 1 {
		t.Fatalf("ValidCells=%d，期望 1（cell b 仅 5 有效）", res.ValidCells)
	}
	if res.KMinCells != s.KMinCells {
		t.Fatalf("KMinCells=%d", res.KMinCells)
	}
}

// ---- 模型级有效率 ----

func TestValidRate(t *testing.T) {
	s := testSettings()
	var rs []*store.Response
	for i := 0; i < 8; i++ {
		rs = append(rs, resp("c", i, fmt.Sprintf("v%d", i), store.ClassValid))
	}
	for i := 0; i < 2; i++ {
		rs = append(rs, resp("c", 10+i, "no", store.ClassRefusal))
	}
	res := Screen(rs, screenOpts(s))
	if res.ValidRate < 0.79 || res.ValidRate > 0.81 {
		t.Fatalf("ValidRate=%v，期望 0.8", res.ValidRate)
	}
	if res.RefusalRate < 0.19 || res.RefusalRate > 0.21 {
		t.Fatalf("RefusalRate=%v，期望 0.2", res.RefusalRate)
	}
}

// ---- 未分类响应 + TaskForCell（preprocess 现算） ----

func TestTaskForCellClassification(t *testing.T) {
	s := testSettings()
	// cell 语义：random_number_1_100 closed 空间 100；答案 "50" valid、"banana" invalid
	taskForCell := func(cell string) (preprocess.Task, bool) {
		if cell != "n:en" {
			return preprocess.Task{}, false
		}
		return preprocess.Task{ID: "random_number_1_100", AnswerSpace: "closed", SpaceSize: 100}, true
	}
	rs := []*store.Response{
		{Cell: "n:en", SampleIdx: 0, RawCompletion: "50"}, // 未分类
		{Cell: "n:en", SampleIdx: 1, RawCompletion: "banana"},
	}
	res := Screen(rs, ScreenOptions{Settings: s, TaskForCell: taskForCell})
	cs := res.CellStats[0]
	if cs.Valid != 1 || cs.Invalid != 1 {
		t.Fatalf("分类错误: %+v", cs)
	}
	if res.ValidRate < 0.49 || res.ValidRate > 0.51 {
		t.Fatalf("ValidRate=%v，期望 0.5", res.ValidRate)
	}
}

// ---- 空输入 ----

func TestScreenEmpty(t *testing.T) {
	s := testSettings()
	res := Screen(nil, screenOpts(s))
	if res.Flags.Any() {
		t.Fatalf("空输入不应有 flag: %v", res.Flags.List())
	}
	if res.ValidRate != -1 || res.RefusalRate != -1 {
		t.Fatal("无分类数据应返回 -1")
	}
}

// ---- CellStats：unique 与平均延迟 ----

func TestCellStatsUniqueLatency(t *testing.T) {
	s := testSettings()
	rs := []*store.Response{
		{Cell: "c", SampleIdx: 0, RawCompletion: "a", Classification: store.ClassValid, LatencyMS: 100},
		{Cell: "c", SampleIdx: 1, RawCompletion: "b", Classification: store.ClassValid, LatencyMS: 200},
		{Cell: "c", SampleIdx: 2, RawCompletion: "a", Classification: store.ClassValid, LatencyMS: 300},
	}
	res := Screen(rs, screenOpts(s))
	cs := res.CellStats[0]
	if cs.UniqueRaw != 2 {
		t.Fatalf("UniqueRaw=%d，期望 2", cs.UniqueRaw)
	}
	if cs.AvgLatencyMS < 199 || cs.AvgLatencyMS > 201 {
		t.Fatalf("AvgLatencyMS=%v，期望 200", cs.AvgLatencyMS)
	}
	if cs.Total != 3 || cs.Valid != 3 {
		t.Fatalf("统计错误: %+v", cs)
	}
}

// ---- 审查回归：T0 判定门槛/兜底/归一化、Unknown、ValidRateLow、缓存延迟条件 ----

func TestT0JudgedThreshold(t *testing.T) {
	s := testSettings() // T0MinJudgedCells=3
	// 仅 2 个 cell 达到样本门槛（judged=2 < 3）→ 不触发，但诊断可见
	var t0 []*store.Response
	for c := 0; c < 2; c++ {
		cell := fmt.Sprintf("p%d", c)
		for i := 0; i < s.T0ProbeN; i++ {
			t0 = append(t0, resp(cell, i, fmt.Sprintf("d%d", c), store.ClassValid))
		}
	}
	res := Screen(nil, ScreenOptions{Settings: s, T0Responses: t0})
	if res.Flags.TemperatureNotHonored {
		t.Fatal("judged=2 < 3 不应触发")
	}
	if res.T0Judged != 2 || res.T0DetRatio < 0.99 {
		t.Fatalf("诊断字段异常: judged=%d ratio=%v", res.T0Judged, res.T0DetRatio)
	}
}

func TestT0RatioInvalidConfig(t *testing.T) {
	// ratio>1 配置：兜底禁用（不恒触发）
	s := testSettings()
	s.T0DeterministicRatio = 1.5
	var t0 []*store.Response
	for c := 0; c < 5; c++ {
		for i := 0; i < s.T0ProbeN; i++ {
			t0 = append(t0, resp(fmt.Sprintf("p%d", c), i, fmt.Sprintf("v%d-%d", c, i), store.ClassValid))
		}
	}
	res := Screen(nil, ScreenOptions{Settings: s, T0Responses: t0})
	if res.Flags.TemperatureNotHonored {
		t.Fatal("ratio>1 配置应禁用检测（不恒触发）")
	}
}

func TestT0NormalizedComparison(t *testing.T) {
	// 格式变体（尾标点/空白）归一化后一致 → 判确定性（不误报）
	s := testSettings()
	taskForCell := func(cell string) (preprocess.Task, bool) {
		return preprocess.Task{ID: "random_number_1_100", AnswerSpace: "closed", SpaceSize: 100}, true
	}
	var t0 []*store.Response
	for c := 0; c < 5; c++ {
		cell := fmt.Sprintf("p%d", c)
		for i := 0; i < s.T0ProbeN; i++ {
			raw := fmt.Sprintf("42.") // 尾标点变体
			if c == 4 {
				raw = "42" // 不同格式但同一语义
			}
			t0 = append(t0, resp(cell, i, raw, ""))
		}
	}
	res := Screen(nil, ScreenOptions{Settings: s, T0Responses: t0, TaskForCell: taskForCell})
	if res.Flags.TemperatureNotHonored {
		t.Fatal("归一化后一致的格式变体应判确定性（不触发）")
	}
}

func TestUnknownClassification(t *testing.T) {
	// TaskForCell 未命中 → Unknown 计数可见（不静默跳过）；ValidRate=-1
	s := testSettings()
	rs := []*store.Response{
		{Cell: "unknown_cell", SampleIdx: 0, RawCompletion: "x"}, // 无分类、TaskForCell 未命中
	}
	taskForCell := func(cell string) (preprocess.Task, bool) {
		return preprocess.Task{}, false // 永不命中
	}
	res := Screen(rs, ScreenOptions{Settings: s, TaskForCell: taskForCell})
	if res.UnknownResponses != 1 {
		t.Fatalf("UnknownResponses=%d，期望 1", res.UnknownResponses)
	}
	if res.ValidRate != -1 || res.RefusalRate != -1 {
		t.Fatal("无分类数据应返回 -1")
	}
	if cs := res.CellStats[0]; cs.Unknown != 1 {
		t.Fatalf("CellStats.Unknown=%d", cs.Unknown)
	}
}

func TestValidRateLowQC(t *testing.T) {
	s := testSettings() // ValidRateQC=0.80
	var rs []*store.Response
	for i := 0; i < 5; i++ { // 5 valid / 10 分类 = 50% < 80%
		rs = append(rs, resp("c", i, fmt.Sprintf("v%d", i), store.ClassValid))
	}
	for i := 0; i < 5; i++ {
		rs = append(rs, resp("c", 10+i, "no", store.ClassRefusal))
	}
	res := Screen(rs, screenOpts(s))
	if !res.ValidRateLow {
		t.Fatalf("ValidRate=%v < 0.80 应置 ValidRateLow", res.ValidRate)
	}
	if res.Flags.Any() {
		t.Fatal("QC 信号不应触发任何 flag（仅模型级 QC）")
	}
}

func TestResponseCachingLatencyJoint(t *testing.T) {
	// 延迟联合条件启用：高延迟豁免（方差崩溃但延迟正常 → 不触发）
	s := testSettings()
	s.CacheLatencyMaxMS = 100
	taskForCell := func(cell string) (preprocess.Task, bool) {
		return preprocess.Task{ID: "random_number_1_100", AnswerSpace: "closed", SpaceSize: 100}, true
	}
	var rs []*store.Response
	for i := 0; i < 15; i++ {
		r := resp("c", i, "42", store.ClassValid)
		r.LatencyMS = 500 // 高延迟
		rs = append(rs, r)
	}
	res := Screen(rs, ScreenOptions{Settings: s, TaskForCell: taskForCell})
	if res.Flags.ResponseCaching {
		t.Fatal("高延迟应豁免（联合条件生效）")
	}
	// 低延迟 → 触发
	for i := 0; i < 15; i++ {
		rs[i].LatencyMS = 30
	}
	res = Screen(rs, ScreenOptions{Settings: s, TaskForCell: taskForCell})
	if !res.Flags.ResponseCaching {
		t.Fatal("低延迟 + 方差崩溃应触发")
	}
}

// fl 构造 *float64（safety-layer-change 基线指针测试用）。
func fl(v float64) *float64 { return &v }
