package calibrate

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"onetoken/internal/store"
)

// 构造性验收：perfect 分类器 AUC=1、random AUC=0.5（设计 §3.4）。
// 得分约定：越低越像 genuine（s ≤ τ → pass）。

func TestAUCPerfect(t *testing.T) {
	g := []float64{0.1, 0.2, 0.3}
	i := []float64{0.7, 0.8, 0.9}
	if got := AUC(g, i); got != 1.0 {
		t.Errorf("AUC(perfect) = %v, want 1.0", got)
	}
}

func TestAUCRandom(t *testing.T) {
	// 完全同值：tie 规则 → 0.5
	if got := AUC([]float64{0.1, 0.1, 0.1}, []float64{0.1, 0.1, 0.1}); got != 0.5 {
		t.Errorf("AUC(同值) = %v, want 0.5", got)
	}
	// 交错相同集合 → 0.5（浮点次序微差容差）
	if got := AUC([]float64{0.2, 0.4, 0.6}, []float64{0.2, 0.4, 0.6}); math.Abs(got-0.5) > 1e-12 {
		t.Errorf("AUC(交错) = %v, want 0.5", got)
	}
}

func TestAUCSymmetry(t *testing.T) {
	// tie 规则下 AUC(g,i) + AUC(i,g) = 1 恒成立
	pairs := [][2][]float64{
		{{0.1, 0.3, 0.5}, {0.2, 0.4, 0.6}},
		{{0.1, 0.1, 0.2}, {0.1, 0.3, 0.3}},
		{{0.0, 0.5, 1.0}, {0.0, 0.5, 1.0}},
	}
	for _, pr := range pairs {
		a := AUC(pr[0], pr[1])
		b := AUC(pr[1], pr[0])
		if math.Abs(a+b-1) > 1e-12 {
			t.Errorf("AUC 对称性破坏: %v + %v = %v, want 1", a, b, a+b)
		}
	}
}

func TestROCShape(t *testing.T) {
	res := computeROC([]float64{0.1, 0.2}, []float64{0.3, 0.4})
	// 起点 (0,0)，终点 (1,1)，FPR 升序
	if len(res.points) == 0 {
		t.Fatal("ROC 为空")
	}
	first, last := res.points[0], res.points[len(res.points)-1]
	if first.FPR != 0 || first.TPR != 0 {
		t.Errorf("起点 = %+v, want (0,0)", first)
	}
	if last.FPR != 1 || last.TPR != 1 {
		t.Errorf("终点 = %+v, want (1,1)", last)
	}
	prev := -1.0
	for _, p := range res.points {
		if p.FPR < prev {
			t.Errorf("FPR 不单调: %v", p.FPR)
		}
		prev = p.FPR
	}
}

func TestEER(t *testing.T) {
	// perfect 分离 → EER=0
	perf := computeROC([]float64{0, 0}, []float64{1, 1})
	if got := EER(perf); got != 0 {
		t.Errorf("EER(perfect) = %v, want 0", got)
	}
	// 同分布 → EER=0.5
	same := computeROC([]float64{0, 1}, []float64{0, 1})
	if got := EER(same); got != 0.5 {
		t.Errorf("EER(同分布) = %v, want 0.5", got)
	}
	// 空 → 0
	if got := EER(computeROC(nil, nil)); got != 0 {
		t.Errorf("EER(空) = %v, want 0", got)
	}
}

func TestThresholdAtFPR(t *testing.T) {
	// g 全在 0.4 以下，i 从 0.6 起：fpr 序列 0, 0.2, 0.4, 0.6, 0.8, 1.0
	g := []float64{0.1, 0.2, 0.3, 0.4}
	i := []float64{0.6, 0.7, 0.8, 0.9, 1.0}
	res := computeROC(g, i)

	// target=0.2：最后一个 fpr ≤ 0.2 的点 = fpr=0.2 → τ=0.6, TPR=1
	if tau, tpr := ThresholdAtFPR(res, 0.2); tau != 0.6 || tpr != 1.0 {
		t.Errorf("τ@20%% = (%v, %v), want (0.6, 1.0)", tau, tpr)
	}
	// target=1.0 → 最宽松 = τ=1.0, TPR=1
	if tau, tpr := ThresholdAtFPR(res, 1.0); tau != 1.0 || tpr != 1.0 {
		t.Errorf("τ@100%% = (%v, %v), want (1.0, 1.0)", tau, tpr)
	}
	// target 低于首个非零 FPR 时，取 FPR=0 的最宽松点（此处 τ=0.4，TPR=1）
	if tau, tpr := ThresholdAtFPR(res, 0.05); tau != 0.4 || tpr != 1.0 {
		t.Errorf("τ@5%% = (%v, %v), want (0.4, 1.0)", tau, tpr)
	}
	// 无任何 fpr ≤ target 的点（起点除外也无一）→ 最严点 (τ=-Inf, TPR=0)：
	// i 最小值与 g 重合 → 首个阈值处 FPR 直接跳变到 1.0
	jump := computeROC([]float64{0.5, 0.6}, []float64{0.5, 0.5, 0.5, 0.5})
	if tau, tpr := ThresholdAtFPR(jump, 0.05); !math.IsInf(tau, -1) || tpr != 0 {
		t.Errorf("τ@5%%（跳变不可达） = (%v, %v), want (-Inf, 0)", tau, tpr)
	}
	// 空 ROC → (0,0)
	if tau, tpr := ThresholdAtFPR(rocResult{}, 0.01); tau != 0 || tpr != 0 {
		t.Errorf("τ(空) = (%v, %v), want (0,0)", tau, tpr)
	}
}

// seq 生成确定性等距序列 [lo, hi]（n 个点，含端点）。
func seq(n int, lo, hi float64) []float64 {
	out := make([]float64, n)
	for k := range out {
		out[k] = lo + (hi-lo)*float64(k)/float64(n-1)
	}
	return out
}

func TestBootstrapTPRCI(t *testing.T) {
	// 案例 A：可分离数据（g∈[0,0.4], i∈[0.6,1.0]），target=0.5 →
	// τ@50% = impostor 中位数 ∈ [0.6,1.0]，TPR 恒为 1.0 → CI=[1,1]
	gA := seq(100, 0, 0.4)
	iA := seq(100, 0.6, 1.0)
	rng := rand.New(rand.NewPCG(0, 42))
	ciA, tprA := BootstrapTPRCI(gA, iA, 0.5, 300, rng)
	if tprA != 1.0 || ciA[0] != 1.0 || ciA[1] != 1.0 {
		t.Errorf("可分离: tpr=%v ci=%v, want 1.0 / [1,1]", tprA, ciA)
	}

	// 案例 B：同支撑重叠（g=i∈[0,1]），真实 TPR@50% ≈ 0.5，CI 应覆盖
	gB := seq(200, 0, 1)
	iB := seq(200, 0, 1)
	rng2 := rand.New(rand.NewPCG(0, 42))
	ciB, tprB := BootstrapTPRCI(gB, iB, 0.5, 500, rng2)
	if ciB[0] > 0.5 || ciB[1] < 0.5 {
		t.Errorf("重叠数据 CI 未覆盖真实 TPR: ci=%v (tpr=%v), want 包含 0.5", ciB, tprB)
	}
	if ciB[0] < 0 || ciB[1] > 1 {
		t.Errorf("CI 越界: %v", ciB)
	}

	// 可复现性：同种子结果一致
	rng3 := rand.New(rand.NewPCG(0, 42))
	ciC, _ := BootstrapTPRCI(gB, iB, 0.5, 500, rng3)
	if ciC != ciB {
		t.Errorf("同种子不可复现: %v vs %v", ciC, ciB)
	}
}

func TestCalibrate(t *testing.T) {
	// 1% 目标可达的大样本：g∈[0,0.4]（100），i∈[0.6,1.0]（100）
	g := seq(100, 0, 0.4)
	i := seq(100, 0.6, 1.0)
	c := Calibrate(g, i, Options{})
	if c.GenuineN != 100 || c.ImpostorN != 100 {
		t.Errorf("样本数 = %d/%d, want 100/100", c.GenuineN, c.ImpostorN)
	}
	if c.AUC != 1.0 {
		t.Errorf("AUC = %v, want 1.0", c.AUC)
	}
	if c.EER != 0 {
		t.Errorf("EER = %v, want 0", c.EER)
	}
	// τ@1% = 最小 impostor ≈ 0.6（τ=0.6 处 fpr=1/100），TPR=1
	if c.TauFPR1 != 0.6 || c.TauFPR1TPR != 1.0 {
		t.Errorf("τ_fpr1 = (%v, %v), want (0.6, 1.0)", c.TauFPR1, c.TauFPR1TPR)
	}
	if len(c.TPRCI) != 2 || c.TPRCI[0] > c.TPRCI[1] {
		t.Errorf("TPRCI 非法: %v", c.TPRCI)
	}
	if len(c.ROC) == 0 || c.ROC[0].FPR != 0 {
		t.Errorf("ROC 非法: %+v", c.ROC)
	}
	if c.CalibratedAt == "" {
		t.Error("CalibratedAt 未设置")
	}
	// 空输入 → nil（无效校准，防"EER=0 看似 perfect"的假校准落盘）
	if empty := Calibrate(nil, nil, Options{}); empty != nil {
		t.Errorf("空输入应返回 nil, got %+v", empty)
	}
}

func TestCalibrateNonFiniteFiltered(t *testing.T) {
	g := append(seq(100, 0, 0.4), math.NaN()) // 混入 NaN
	i := seq(100, 0.6, 1.0)
	c := Calibrate(g, i, Options{})
	if c == nil {
		t.Fatal("过滤后仍应有有效校准")
	}
	if c.GenuineN != 100 || c.ImpostorN != 100 {
		t.Errorf("非有限应被过滤: GenuineN=%d, want 100", c.GenuineN)
	}
	if c.AUC != 1.0 {
		t.Errorf("AUC = %v, want 1.0（NaN 不得污染）", c.AUC)
	}
	// 全部非有限 → 过滤后为空 → nil
	if c2 := Calibrate([]float64{math.NaN(), math.Inf(1)}, seq(10, 0, 1), Options{}); c2 != nil {
		t.Errorf("全部非有限应返回 nil, got %+v", c2)
	}
}

func TestCalibrateUnreachableTargetNil(t *testing.T) {
	// 操作点不可达（i 全同值 → FPR 跳变，τ=−∞）：档位无效 → nil，不得落盘 -Inf
	g := []float64{0.5, 0.6}
	i := []float64{0.5, 0.5, 0.5, 0.5}
	if c := Calibrate(g, i, Options{}); c != nil {
		t.Errorf("操作点不可达应返回 nil, got %+v", c)
	}
}

func TestOptionsDefaultsSanitize(t *testing.T) {
	// NaN/越界 target → 默认；Seed=0 与未设置等价（用合理 NResamples 避免长跑）
	g := seq(200, 0, 0.4)
	i := seq(200, 0.6, 1.0)
	a := Calibrate(g, i, Options{NResamples: 50})
	b := Calibrate(g, i, Options{FPRTarget: math.NaN(), FPRTarget2: 2.0, NResamples: 50, Seed: 0})
	if a == nil || b == nil {
		t.Fatal("两种 Options 都应有有效校准")
	}
	if a.TauFPR1 != b.TauFPR1 {
		t.Errorf("NaN target 未回落默认: %v vs %v", a.TauFPR1, b.TauFPR1)
	}
	// clamp 逻辑单独断言（不实际跑 10 万次 bootstrap）
	if o := (Options{NResamples: 999999999}).withDefaults(); o.NResamples != maxResamples {
		t.Errorf("NResamples 未封顶: %d, want %d", o.NResamples, maxResamples)
	}
	if o := (Options{FPRTarget: math.NaN()}).withDefaults(); o.FPRTarget != defaultFPRTarget {
		t.Errorf("NaN target 未回落默认: %v", o.FPRTarget)
	}
}

// TestSklearnGoldenAUC 落库 sklearn.metrics.roc_auc_score 对拍黄金值
// （得分约定一致：低=genuine，高=impostor）。
func TestSklearnGoldenAUC(t *testing.T) {
	// sklearn: roc_auc_score([0]*5+[1]*6, [0.1,0.3,0.5,0.2,0.4,0.6,0.8,0.7,0.9,0.65,0.75]) = 1.0
	if got := AUC([]float64{0.1, 0.3, 0.5, 0.2, 0.4}, []float64{0.6, 0.8, 0.7, 0.9, 0.65, 0.75}); got != 1.0 {
		t.Errorf("AUC = %v, want 1.0 (sklearn)", got)
	}
	// sklearn: tie 案例 = 0.6666666666666667（= 2/3）
	got := AUC([]float64{0.5, 0.5, 0.2}, []float64{0.5, 0.7, 0.3})
	if math.Abs(got-2.0/3) > 1e-12 {
		t.Errorf("AUC = %v, want 2/3 (sklearn 0.6666666666666667)", got)
	}
}

func TestSplitHalves(t *testing.T) {
	rs := make([]*store.Response, 0, 11)
	for idx := 0; idx < 10; idx++ {
		rs = append(rs, &store.Response{Cell: "x:en", SampleIdx: idx})
	}
	rs = append(rs, nil) // nil 跳过
	even, odd := SplitHalves(rs)
	if len(even) != 5 || len(odd) != 5 {
		t.Fatalf("even/odd = %d/%d, want 5/5", len(even), len(odd))
	}
	for _, r := range even {
		if r.SampleIdx%2 != 0 {
			t.Errorf("even 含奇数 idx %d", r.SampleIdx)
		}
	}
	for _, r := range odd {
		if r.SampleIdx%2 == 0 {
			t.Errorf("odd 含偶数 idx %d", r.SampleIdx)
		}
	}
}

// --- LOO 1-NN（复现用途） ---

func cellDist1(n int, key string, count int) store.CellDist {
	return store.CellDist{Dist: map[string]int{key: count}, N: n}
}

func TestLOO1NN(t *testing.T) {
	fps := []*store.Fingerprint{
		{ModelID: "qwen/qwen3-8b", Cells: map[string]store.CellDist{"x:en": cellDist1(30, "a", 30)}},
		{ModelID: "qwen/qwen3-32b", Cells: map[string]store.CellDist{"x:en": cellDist1(30, "a", 30)}},
		{ModelID: "llama/llama3-8b", Cells: map[string]store.CellDist{"x:en": cellDist1(30, "b", 30)}},
		{ModelID: "llama/llama3-70b", Cells: map[string]store.CellDist{"x:en": cellDist1(30, "b", 30)}},
	}
	familyOf := func(id string) string {
		parts := strings.SplitN(id, "/", 2)
		if len(parts) < 2 {
			return id
		}
		return parts[0]
	}
	pred := LOO1NN(fps, familyOf)
	want := map[string]string{
		"qwen/qwen3-8b":    "qwen",
		"qwen/qwen3-32b":   "qwen",
		"llama/llama3-8b":  "llama",
		"llama/llama3-70b": "llama",
	}
	for id, f := range want {
		if pred[id] != f {
			t.Errorf("LOO 预测 %s = %q, want %q", id, pred[id], f)
		}
	}
}

// TestLOO1NNSkipsNonComparable：无共同 cell 的 (0,0) 距离不得被当作最近邻
// （审查 High 级发现：原实现把"不可比"当"距离 0"，导致家族静默错配）。
func TestLOO1NNSkipsNonComparable(t *testing.T) {
	fps := []*store.Fingerprint{
		{ModelID: "a/a1", Cells: map[string]store.CellDist{"x:en": cellDist1(30, "a", 30)}},
		{ModelID: "a/a2", Cells: map[string]store.CellDist{"x:en": cellDist1(30, "a", 30)}},
		{ModelID: "c/c1", Cells: map[string]store.CellDist{"x:en": cellDist1(30, "b", 30)}},
		{ModelID: "c/c2", Cells: map[string]store.CellDist{"x:en": cellDist1(30, "b", 30)}},
		// b 族只持有 y:zh cell：与 a/c 族无共同可比较 cell（Distance 参与数=0）
		{ModelID: "b/b1", Cells: map[string]store.CellDist{"y:zh": cellDist1(30, "b", 30)}},
		{ModelID: "b/b2", Cells: map[string]store.CellDist{"y:zh": cellDist1(30, "b", 30)}},
	}
	familyOf := func(id string) string {
		return strings.SplitN(id, "/", 2)[0]
	}
	pred := LOO1NN(fps, familyOf)
	want := map[string]string{
		"a/a1": "a", "a/a2": "a",
		"c/c1": "c", "c/c2": "c",
		"b/b1": "b", "b/b2": "b", // 不得因 (0,0) 伪距离落入 a/c 族
	}
	for id, f := range want {
		if pred[id] != f {
			t.Errorf("LOO 预测 %s = %q, want %q（无共同 cell 必须跳过）", id, pred[id], f)
		}
	}
}

func TestLOO1NNNilSafe(t *testing.T) {
	fps := []*store.Fingerprint{
		{ModelID: "a/a1", Cells: map[string]store.CellDist{"x:en": cellDist1(30, "a", 30)}},
		nil,
		{ModelID: "a/a2", Cells: map[string]store.CellDist{"x:en": cellDist1(30, "a", 30)}},
	}
	familyOf := func(id string) string {
		return strings.SplitN(id, "/", 2)[0]
	}
	pred := LOO1NN(fps, familyOf) // 不得 panic
	if pred["a/a1"] != "a" || pred["a/a2"] != "a" {
		t.Errorf("nil 指纹应被跳过: %v", pred)
	}
}
