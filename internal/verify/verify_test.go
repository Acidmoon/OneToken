package verify

import (
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"

	"onetoken/internal/battery"
	"onetoken/internal/config"
	"onetoken/internal/store"
)

func testSettings() config.Settings {
	return config.DefaultSettings() // KMinCells=3, TauInconclusiveBuffer=0.02
}

func testBattery(t *testing.T) *battery.Battery {
	t.Helper()
	b, err := battery.Load(filepath.Join("..", "..", "config", "prompts.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- Judge 三分支（验收：pass/suspicious/inconclusive） ----

func TestJudge(t *testing.T) {
	// 用 2 的幂次（0.5/0.25）保证 τ±buf 浮点精确，规避边界比较问题
	const tau, buf = 0.5, 0.25
	cases := []struct {
		name  string
		score float64
		want  string
	}{
		{"远低于 τ → pass", 0.1, store.VerdictPass},
		{"恰等于 τ−buf → pass", tau - buf, store.VerdictPass},
		{"低于 τ 但在缓冲内 → inconclusive", 0.4, store.VerdictInconclusive},
		{"恰等于 τ → inconclusive", tau, store.VerdictInconclusive},
		{"高于 τ 但在缓冲内 → inconclusive", 0.6, store.VerdictInconclusive},
		{"恰等于 τ+buf → inconclusive（严格 > 才 suspicious）", tau + buf, store.VerdictInconclusive},
		{"略超 τ+buf → suspicious", tau + buf + 0.01, store.VerdictSuspicious},
		{"远高于 τ → suspicious", 0.9, store.VerdictSuspicious},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Judge(c.score, tau, buf); got != c.want {
				t.Fatalf("Judge(%v)=%q，期望 %q", c.score, got, c.want)
			}
		})
	}
}

func TestJudgeZeroBuffer(t *testing.T) {
	// buf=0：退化为 s≤τ pass / s>τ suspicious（无模糊区）
	if got := Judge(0.05, 0.05, 0); got != store.VerdictPass {
		t.Fatalf("buf=0 时 s==τ 应 pass，实际 %q", got)
	}
	if got := Judge(0.051, 0.05, 0); got != store.VerdictSuspicious {
		t.Fatalf("buf=0 时 s>τ 应 suspicious，实际 %q", got)
	}
}

// ---- MatchCalibration：分档键精确匹配 ----

func TestMatchCalibration(t *testing.T) {
	cals := []store.Calibration{
		{Scope: "global", K: 8, NPerCell: 15, RefChannel: "local", TargetChannel: "local", TauFPR1: 0.10},
		{Scope: "global", K: 16, NPerCell: 15, RefChannel: "local", TargetChannel: "official-api", TauFPR1: 0.20},
		{Scope: "family:qwen", K: 8, NPerCell: 15, RefChannel: "local", TargetChannel: "local", TauFPR1: 0.12},
	}
	cases := []struct {
		name     string
		k, n     int
		ref, tgt string
		wantTau  float64
		wantNil  bool
	}{
		{"精确命中", 8, 15, "local", "local", 0.10, false},
		{"通道区分（official）", 16, 15, "local", "official-api", 0.20, false},
		{"k 不匹配", 4, 15, "local", "local", 0, true},
		{"n 不匹配", 8, 30, "local", "local", 0, true},
		{"ref 通道不匹配", 8, 15, "official-api", "local", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MatchCalibration(cals, c.k, c.n, "global", c.ref, c.tgt)
			if c.wantNil {
				if got != nil {
					t.Fatalf("应返回 nil，实际命中 %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("应命中校准档")
			}
			if got.TauFPR1 != c.wantTau {
				t.Fatalf("τ=%v，期望 %v", got.TauFPR1, c.wantTau)
			}
		})
	}
}

// ---- VerifyAudit 完整流程 ----

// auditResp 构造审计响应（预填 valid 分类；Cell 须为真实电池 cell）。
func auditResp(cell string, idx int, raw string) *store.Response {
	return &store.Response{
		Cell: cell, SampleIdx: idx, RawCompletion: raw,
		Normalized:     raw, // Build 要求 Normalized 非空（preprocess 不变量）
		Classification: store.ClassValid, Temperature: 1.0,
		FinishReason: "stop", CompletionTokens: 1,
	}
}

// answers 是 5 个不同答案（每 cell 采样 10 次 = 各 2 次，保证方差不触发缓存签名）。
var answers = []string{"a1", "b2", "c3", "d4", "e5"}

// dist 构造答案均匀分布计数。
func dist(keys []string, n int) map[string]int {
	m := make(map[string]int, len(keys))
	for _, k := range keys {
		m[k] = n / len(keys)
	}
	return m
}

// claimedFp 手工构造参考指纹（3 个 cell × 10 有效样本，均匀方差分布）。
func claimedFp(cells []string) *store.Fingerprint {
	m := make(map[string]store.CellDist, len(cells))
	for _, c := range cells {
		m[c] = store.CellDist{Dist: dist(answers, 10), N: 10, T: 1.0}
	}
	return &store.Fingerprint{ModelID: "claimed", Cells: m}
}

// auditResponses 构造审计响应：每 cell n 个样本，答案按 keys 轮换（方差分布）。
func auditResponses(cells []string, n int, keys []string) []*store.Response {
	var rs []*store.Response
	for _, c := range cells {
		for i := 0; i < n; i++ {
			rs = append(rs, auditResp(c, i, keys[i%len(keys)]))
		}
	}
	return rs
}

func baseOpts(b *battery.Battery) Options {
	return Options{
		Settings:      testSettings(),
		Battery:       b,
		K:             3,
		N:             10,
		Scope:         "global",
		RefChannel:    "local",
		TargetChannel: "local",
		Calibrations: []store.Calibration{
			{Scope: "global", K: 3, NPerCell: 10, RefChannel: "local", TargetChannel: "local", TauFPR1: 0.05},
		},
	}
}

func TestVerifyAuditPass(t *testing.T) {
	b := testBattery(t)
	cells := b.Cells()[:3]
	// 审计与参考同分布（同 keys 轮换）→ 距离 ≈0 → pass（方差避免缓存误报）
	rs := auditResponses(cells, 10, answers)
	res, err := VerifyAudit(rs, claimedFp(cells), baseOpts(b))
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != store.VerdictPass {
		t.Fatalf("verdict=%q，期望 pass（score=%v τ=%v）", res.Verdict, res.Score, res.Threshold)
	}
	if res.CellsUsed != 3 {
		t.Fatalf("CellsUsed=%d，期望 3", res.CellsUsed)
	}
	if res.Calibration == nil {
		t.Fatal("应命中校准档")
	}
}

func TestVerifyAuditSuspicious(t *testing.T) {
	b := testBattery(t)
	cells := b.Cells()[:3]
	// 审计与参考分布不相交（x1..x5 vs a1..e5）→ JSD=1 → suspicious
	rs := auditResponses(cells, 10, []string{"x1", "y2", "z3", "u4", "v5"})
	res, err := VerifyAudit(rs, claimedFp(cells), baseOpts(b))
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != store.VerdictSuspicious {
		t.Fatalf("verdict=%q，期望 suspicious（score=%v）", res.Verdict, res.Score)
	}
}

func TestVerifyAuditNoCalibration(t *testing.T) {
	b := testBattery(t)
	cells := b.Cells()[:3]
	opts := baseOpts(b)
	opts.Calibrations = nil // 无校准库
	_, err := VerifyAudit(auditResponses(cells, 10, answers), claimedFp(cells), opts)
	if !errors.Is(err, ErrNoCalibration) {
		t.Fatalf("应报 ErrNoCalibration，实际 %v", err)
	}
}

func TestVerifyAuditHiddenReasoning(t *testing.T) {
	b := testBattery(t)
	cells := b.Cells()[:3]
	rs := auditResponses(cells, 10, answers)
	rs[0].ReasoningTokens = 99 // 推理痕迹 → 测量级 flag
	res, err := VerifyAudit(rs, claimedFp(cells), baseOpts(b))
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != store.VerdictInconclusive {
		t.Fatalf("hidden-reasoning 应判 inconclusive，实际 %q", res.Verdict)
	}
	if res.Reason == "" {
		t.Fatal("应有判定理由")
	}
}

func TestVerifyAuditKMin(t *testing.T) {
	b := testBattery(t)
	cells := b.Cells()[:3]
	rs := auditResponses(cells, 10, answers)
	// 仅 1 个 cell 达到 10 有效样本（其余改 invalid）→ ValidCells=1 < k_min=3
	for i := 1; i < len(cells); i++ {
		for j := 0; j < 10; j++ {
			rs[i*10+j].Classification = store.ClassInvalid
		}
	}
	res, err := VerifyAudit(rs, claimedFp(cells), baseOpts(b))
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != store.VerdictInconclusive {
		t.Fatalf("有效 cell < k_min 应判 inconclusive，实际 %q", res.Verdict)
	}
}

func TestVerifyAuditUnreachable(t *testing.T) {
	b := testBattery(t)
	cells := b.Cells()[:3]
	opts := baseOpts(b)
	opts.FailedTasks = 10
	opts.TotalTasks = 10 // 全失败 → unreachable
	res, err := VerifyAudit(auditResponses(cells, 10, answers), claimedFp(cells), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != store.VerdictInconclusive {
		t.Fatalf("unreachable 应判 inconclusive，实际 %q", res.Verdict)
	}
	if !res.Flags.Unreachable {
		t.Fatal("应标记 unreachable")
	}
}

func TestVerifyAuditValidation(t *testing.T) {
	b := testBattery(t)
	cells := b.Cells()[:3]
	rs := auditResponses(cells, 10, answers)
	if _, err := VerifyAudit(rs, nil, baseOpts(b)); err == nil {
		t.Fatal("nil claimed 应报错")
	}
	opts := baseOpts(b)
	opts.Battery = nil
	if _, err := VerifyAudit(rs, claimedFp(cells), opts); err == nil {
		t.Fatal("nil battery 应报错")
	}
	opts = baseOpts(b)
	opts.K = 0
	if _, err := VerifyAudit(rs, claimedFp(cells), opts); err == nil {
		t.Fatal("K=0 应报错")
	}
}

// ---- 审查回归：fail-closed、scope、τ 合法性、证据链、副本、并发 ----

func TestVerifyAuditNoCommonCells(t *testing.T) {
	b := testBattery(t)
	// 审计 cell 与 claimed cell 完全不同 → cellsUsed=0 → 不得判 pass（fail-closed）
	auditCells := b.Cells()[4:7] // 与 claimed 前 3 个 cell 不重叠
	claimed := claimedFp(b.Cells()[:3])
	rs := auditResponses(auditCells, 10, answers)
	res, err := VerifyAudit(rs, claimed, baseOpts(b))
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != store.VerdictInconclusive {
		t.Fatalf("无共同 cell 应判 inconclusive（fail-closed），实际 %q score=%v cells=%d",
			res.Verdict, res.Score, res.CellsUsed)
	}
	if res.CellsUsed != 0 {
		t.Fatalf("CellsUsed=%d，期望 0", res.CellsUsed)
	}
}

func TestVerifyAuditInvalidTau(t *testing.T) {
	b := testBattery(t)
	cells := b.Cells()[:3]
	opts := baseOpts(b)
	opts.Calibrations[0].TauFPR1 = 1.5 // 超出 JSD 值域
	_, err := VerifyAudit(auditResponses(cells, 10, answers), claimedFp(cells), opts)
	if err == nil {
		t.Fatal("τ 超值域应报错")
	}
	opts.Calibrations[0].TauFPR1 = -0.1
	if _, err := VerifyAudit(auditResponses(cells, 10, answers), claimedFp(cells), opts); err == nil {
		t.Fatal("τ 为负应报错")
	}
}

func TestVerifyAuditEmptyClaimed(t *testing.T) {
	b := testBattery(t)
	cells := b.Cells()[:3]
	empty := &store.Fingerprint{ModelID: "empty"} // Cells 为空
	_, err := VerifyAudit(auditResponses(cells, 10, answers), empty, baseOpts(b))
	if err == nil {
		t.Fatal("空参考指纹应报错")
	}
}

func TestVerifyAuditCachingShortCircuit(t *testing.T) {
	b := testBattery(t)
	cells := b.Cells()[:3]
	// 全同响应（方差崩溃）→ response-caching → inconclusive（不进指纹）
	rs := auditResponses(cells, 10, []string{"42"})
	res, err := VerifyAudit(rs, claimedFp(cells), baseOpts(b))
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != store.VerdictInconclusive {
		t.Fatalf("response-caching 应判 inconclusive，实际 %q", res.Verdict)
	}
	if !res.Flags.ResponseCaching {
		t.Fatal("应标记 response-caching")
	}
}

func TestVerifyAuditSafetyFlagNotBlocking(t *testing.T) {
	b := testBattery(t)
	cells := b.Cells()[:3]
	// refusal 基线差距大 → safety-layer-change 告警，但不阻断 pass 判定
	opts := baseOpts(b)
	base := 0.9
	opts.RefusalBaseline = &base // 审计 refusal 率 0 vs 基线 0.9 → |0.9| > 0.15
	res, err := VerifyAudit(auditResponses(cells, 10, answers), claimedFp(cells), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Flags.SafetyLayerChange {
		t.Fatal("应标记 safety-layer-change")
	}
	if res.Verdict != store.VerdictPass {
		t.Fatalf("safety-layer-change 不应阻断判定，实际 %q", res.Verdict)
	}
}

func TestVerifyAuditChainHashMismatch(t *testing.T) {
	b := testBattery(t)
	cells := b.Cells()[:3]
	rs := auditResponses(cells, 10, answers)
	rs[0].RawSHA256 = "deadbeef" // 与 RawCompletion 不匹配（伪造）
	_, err := VerifyAudit(rs, claimedFp(cells), baseOpts(b))
	if err == nil {
		t.Fatal("证据链哈希不匹配应报错")
	}
}

func TestVerifyAuditNoInputMutation(t *testing.T) {
	b := testBattery(t)
	cells := b.Cells()[:3]
	rs := auditResponses(cells, 10, answers)
	for _, r := range rs {
		r.Classification = "" // 未分类（需回填）
		r.Normalized = ""
	}
	before := make([]string, len(rs))
	for i, r := range rs {
		before[i] = r.Classification
	}
	if _, err := VerifyAudit(rs, claimedFp(cells), baseOpts(b)); err != nil {
		t.Fatal(err)
	}
	for i, r := range rs {
		if r.Classification != before[i] {
			t.Fatal("VerifyAudit 不应修改调用方切片（副本回填）")
		}
	}
}

func TestVerifyAuditConcurrentSameSlice(t *testing.T) {
	// 并发 VerifyAudit 同一切片：副本回填保证无 data race（-race 验证）
	b := testBattery(t)
	cells := b.Cells()[:3]
	rs := auditResponses(cells, 10, answers)
	claimed := claimedFp(cells)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := VerifyAudit(rs, claimed, baseOpts(b)); err != nil {
				t.Errorf("并发 VerifyAudit 失败: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestVerifyAuditCellsDetail(t *testing.T) {
	b := testBattery(t)
	cells := b.Cells()[:3]
	res, err := VerifyAudit(auditResponses(cells, 10, answers), claimedFp(cells), baseOpts(b))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.CellsDetail) != 3 {
		t.Fatalf("CellsDetail 应有 3 个 cell，实际 %d", len(res.CellsDetail))
	}
}

// ---- M2.10: τ 来源优先级（override > calibration > builtin）与内置线回退 ----

func TestVerifyAuditTauSourcePriority(t *testing.T) {
	b := testBattery(t)
	cells := b.Cells()[:3]
	claimed := claimedFp(cells)
	rs := auditResponses(cells, 10, answers)

	t.Run("无校准档 + TauBuiltin → 内置线回退", func(t *testing.T) {
		opts := baseOpts(b)
		opts.Calibrations = nil
		opts.TauBuiltin = 0.14
		res, err := VerifyAudit(rs, claimed, opts)
		if err != nil {
			t.Fatal(err)
		}
		if res.TauSource != "builtin" {
			t.Fatalf("TauSource=%q，期望 builtin", res.TauSource)
		}
		if res.Threshold != 0.14 {
			t.Fatalf("Threshold=%v，期望 0.14", res.Threshold)
		}
		if res.Calibration != nil {
			t.Fatal("内置线路径不应命中校准档")
		}
		// 同分布 → 距离≈0 < 0.14−0.02 → pass
		if res.Verdict != store.VerdictPass {
			t.Fatalf("verdict=%q，期望 pass（score=%v）", res.Verdict, res.Score)
		}
	})

	t.Run("有校准档 → 校准 τ（优先级高于内置线）", func(t *testing.T) {
		opts := baseOpts(b)
		opts.TauBuiltin = 0.14 // 同时存在时校准档优先
		res, err := VerifyAudit(rs, claimed, opts)
		if err != nil {
			t.Fatal(err)
		}
		if res.TauSource != "calibration" {
			t.Fatalf("TauSource=%q，期望 calibration", res.TauSource)
		}
		if res.Threshold != 0.05 {
			t.Fatalf("Threshold=%v，期望校准档 0.05", res.Threshold)
		}
	})

	t.Run("--tau 直传 → override（最高优先级）", func(t *testing.T) {
		opts := baseOpts(b)
		opts.TauOverride = 0.2
		opts.TauBuiltin = 0.14
		res, err := VerifyAudit(rs, claimed, opts)
		if err != nil {
			t.Fatal(err)
		}
		if res.TauSource != "override" {
			t.Fatalf("TauSource=%q，期望 override", res.TauSource)
		}
		if res.Threshold != 0.2 {
			t.Fatalf("Threshold=%v，期望 0.2", res.Threshold)
		}
	})

	t.Run("TauBuiltin=0 调用方无档仍拒绝（v0.23 起 CLI 均传内置线，API 层保留严格口径）", func(t *testing.T) {
		opts := baseOpts(b)
		opts.Calibrations = nil
		opts.TauBuiltin = 0 // 未启用内置线回退
		_, err := VerifyAudit(rs, claimed, opts)
		if !errors.Is(err, ErrNoCalibration) {
			t.Fatalf("TauBuiltin=0 无档应拒绝 ErrNoCalibration，实际 %v", err)
		}
	})
}

func TestVerifyAuditBuiltinInvalidTau(t *testing.T) {
	b := testBattery(t)
	cells := b.Cells()[:3]
	opts := baseOpts(b)
	opts.Calibrations = nil
	for _, bad := range []float64{math.NaN(), math.Inf(1), -0.1, 1.5} {
		opts.TauBuiltin = bad
		if _, err := VerifyAudit(auditResponses(cells, 10, answers), claimedFp(cells), opts); err == nil {
			t.Fatalf("内置线 %v 非法应报错", bad)
		}
	}
}
