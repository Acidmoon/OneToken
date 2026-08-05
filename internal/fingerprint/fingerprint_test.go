package fingerprint

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"onetoken/internal/store"
)

// 对拍黄金值来源：scipy 1.18.0
// scipy.spatial.distance.jensenshannon(p, q, base=2) 的**平方**
// （scipy 返回 sqrt(JSD)，平方后得到与本文档一致的原始标度 JSD）。
// 生成脚本见设计验收项"与 scipy 平方后对拍（注意 sqrt 差异）"。

// jsdCase 是一个 scipy 对拍用例（key 按词序对齐，缺失 key 概率为 0）。
type jsdCase struct {
	name string
	p, q []float64
	want float64 // scipy jensenshannon(base=2)²
}

func jsdCases() []jsdCase {
	return []jsdCase{
		{"identical", []float64{1, 0}, []float64{1, 0}, 0.0},
		{"half_identical", []float64{0.5, 0.5}, []float64{1, 0}, 0.3112781244591328},
		{"disjoint", []float64{1, 0}, []float64{0, 1}, 1.0},
		{"third", []float64{1.0 / 3, 1.0 / 3, 1.0 / 3}, []float64{0.5, 0.25, 0.25}, 0.020720839623908173},
		{"skew_skew", []float64{0.9, 0.1}, []float64{0.2, 0.8}, 0.39731260974948646},
		{"four_aligned", []float64{0.4, 0.3, 0.2, 0.1}, []float64{0.1, 0.2, 0.3, 0.4}, 0.15356065532898464},
		{"five_sparse", []float64{0.7, 0.1, 0.1, 0.05, 0.05}, []float64{0.2, 0, 0.5, 0.3, 0}, 0.3575585093247074},
		{"six_disjointmix", []float64{0.5, 0.5, 0, 0, 0, 0}, []float64{0, 0, 0.25, 0.25, 0.25, 0.25}, 1.0},
	}
}

// prob 把 key 对齐的词频切片转为概率 map（key 为 "k0","k1",...）。
func prob(vals []float64) map[string]float64 {
	m := make(map[string]float64, len(vals))
	for i, v := range vals {
		m[fmt.Sprintf("k%d", i)] = v
	}
	return m
}

func approx(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}

// --- KL ---

func TestKL(t *testing.T) {
	// 自散度为 0
	if got := KL(prob([]float64{0.3, 0.7}), prob([]float64{0.3, 0.7})); got != 0 {
		t.Errorf("KL(p,p)=%v, want 0", got)
	}
	// 手算：KL({0.5,0.5} ‖ {0.75,0.25}) = 0.5·ln(2/3) + 0.5·ln2 ≈ 0.1438410
	got := KL(prob([]float64{0.5, 0.5}), prob([]float64{0.75, 0.25}))
	want := 0.5*math.Log(2.0/3) + 0.5*math.Log(2.0)
	if !approx(got, want, 1e-15) {
		t.Errorf("KL = %v, want %v", got, want)
	}
	// 非对称：KL(p‖q) ≠ KL(q‖p)
	klpq := KL(prob([]float64{0.9, 0.1}), prob([]float64{0.5, 0.5}))
	klqp := KL(prob([]float64{0.5, 0.5}), prob([]float64{0.9, 0.1}))
	if approx(klpq, klqp, 1e-15) {
		t.Errorf("KL 应非对称: %v vs %v", klpq, klqp)
	}
	// 0·ln0=0：p 侧零概率项跳过，不产生 NaN
	if got := KL(map[string]float64{"a": 0, "b": 1}, map[string]float64{"a": 0, "b": 1}); got != 0 {
		t.Errorf("KL 含零项 = %v, want 0", got)
	}
	// 发散：q[k]=0 且 p[k]>0 → +Inf
	if got := KL(map[string]float64{"a": 1}, map[string]float64{}); !math.IsInf(got, 1) {
		t.Errorf("KL 发散应为 +Inf, got %v", got)
	}
}

// --- JSD：scipy 对拍（M1.3 核心验收项） ---

func TestJSDScipyGolden(t *testing.T) {
	for _, c := range jsdCases() {
		got := JSD(prob(c.p), prob(c.q))
		if !approx(got, c.want, 1e-12) {
			t.Errorf("JSD(%s) = %.17g, want scipy² %.17g (diff %g)", c.name, got, c.want, got-c.want)
		}
	}
}

func TestJSDSymmetryAndBounds(t *testing.T) {
	// 对称性 + 有界 [0,1]：多组随机分布
	pairs := [][][]float64{
		{{0.9, 0.1}, {0.2, 0.8}},
		{{0.4, 0.3, 0.2, 0.1}, {0.1, 0.2, 0.3, 0.4}},
		{{1.0 / 3, 1.0 / 3, 1.0 / 3}, {0.1, 0.1, 0.8}},
		{{0.6, 0.4, 0}, {0, 0.3, 0.7}},
	}
	for _, pr := range pairs {
		dp, dq := prob(pr[0]), prob(pr[1])
		v := JSD(dp, dq)
		r := JSD(dq, dp)
		if !approx(v, r, 1e-15) {
			t.Errorf("JSD 不对称: %v vs %v", v, r)
		}
		if v < 0 || v > 1 {
			t.Errorf("JSD 越界 [0,1]: %v", v)
		}
	}
}

func TestJSDPartialDisjoint(t *testing.T) {
	// 部分不相交（0·ln0 分支 + 无平滑）：手算 = 0.5
	// p={a:0.5,b:0.5}, q={a:0.5,c:0.5} → m={a:0.5,b:0.25,c:0.25}
	// KL(p‖m)=0.5·ln2, KL(q‖m)=0.5·ln2 → JSD=ln2/(2·ln2)=0.5
	p := map[string]float64{"a": 0.5, "b": 0.5}
	q := map[string]float64{"a": 0.5, "c": 0.5}
	if got := JSD(p, q); !approx(got, 0.5, 1e-15) {
		t.Errorf("JSD(partial disjoint) = %v, want 0.5", got)
	}
	// 空分布是退化输入（正常路径由 cellJSDs 的 len(dist)==0 过滤）：
	// p 侧零项全跳过，剩 KL(q‖q/2)/(2·ln2) = 0.5；此处仅验证不 panic、行为确定。
	if got := JSD(map[string]float64{}, map[string]float64{"a": 1}); !approx(got, 0.5, 1e-15) {
		t.Errorf("JSD(空, p) = %v, want 0.5（退化输入快照）", got)
	}
}

// --- Normalize ---

func TestNormalize(t *testing.T) {
	got := Normalize(map[string]int{"a": 3, "b": 1})
	want := map[string]float64{"a": 0.75, "b": 0.25}
	for k, v := range want {
		if !approx(got[k], v, 1e-15) {
			t.Errorf("Normalize[%s] = %v, want %v", k, got[k], v)
		}
	}
	var sum float64
	for _, v := range got {
		sum += v
	}
	if !approx(sum, 1.0, 1e-15) {
		t.Errorf("Normalize 总和 = %v, want 1", sum)
	}
	if got := Normalize(nil); len(got) != 0 {
		t.Errorf("Normalize(nil) = %v, want 空 map", got)
	}
}

// --- Build（分布估计） ---

func resp(cell, norm, class string, t float64, idx int) *store.Response {
	return &store.Response{
		Cell:           cell,
		SampleIdx:      idx,
		Temperature:    t,
		Normalized:     norm,
		Classification: class,
		RawSHA256:      store.SHA256Hex(norm),
		TS:             "2026-08-05T00:00:00Z",
	}
}

func TestBuildGroupsAndFilters(t *testing.T) {
	rs := []*store.Response{
		// cell x:en，T=1.0 —— 3 valid + 1 refusal + 1 empty
		resp("x:en", "42", store.ClassValid, 1.0, 0),
		resp("x:en", "42", store.ClassValid, 1.0, 1),
		resp("x:en", "57", store.ClassValid, 1.0, 2),
		resp("x:en", "i cannot answer", store.ClassRefusal, 1.0, 3),
		resp("x:en", "", store.ClassEmpty, 1.0, 4),
		// cell x:en，T=0 —— 应进 T0Cells
		resp("x:en", "42", store.ClassValid, 0.0, 5),
		// cell y:en，T=1.0 —— invalid 不进指纹
		resp("y:en", "42 43", store.ClassInvalid, 1.0, 6),
		nil, // 空指针跳过
	}
	at := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	fp, err := Build("qwen/qwen3-8b", "2026-08-05v1", "local", at, rs)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if fp.ModelID != "qwen/qwen3-8b" || fp.Version != "2026-08-05v1" || fp.RefSource != "local" {
		t.Errorf("元数据不符: %+v", fp)
	}
	if fp.CollectedAt != "2026-08-05T00:00:00Z" {
		t.Errorf("CollectedAt = %q, want UTC Z", fp.CollectedAt)
	}
	// T=1.0 分组：只收 valid
	x := fp.Cells["x:en"]
	if x.N != 3 {
		t.Errorf("x:en N = %d, want 3", x.N)
	}
	if x.Dist["42"] != 2 || x.Dist["57"] != 1 || len(x.Dist) != 2 {
		t.Errorf("x:en dist = %v, want {42:2, 57:1}", x.Dist)
	}
	if !approx(x.ValidRate, 0.6, 1e-15) {
		t.Errorf("x:en valid_rate = %v, want 0.6 (3/5)", x.ValidRate)
	}
	if x.T != 1.0 {
		t.Errorf("x:en T = %v, want 1.0", x.T)
	}
	// invalid 全被滤除 → y:en 无 dist 条目（N=0）
	y := fp.Cells["y:en"]
	if y.N != 0 || len(y.Dist) != 0 {
		t.Errorf("y:en 不应有有效样本: N=%d dist=%v", y.N, y.Dist)
	}
	// T=0 分离
	x0 := fp.T0Cells["x:en"]
	if x0.N != 1 || x0.Dist["42"] != 1 || x0.T != 0 {
		t.Errorf("x:en T0 = %+v, want N=1 {42:1} T=0", x0)
	}
	if _, ok := fp.Cells["x:en"]; !ok {
		t.Error("T=1.0 的 x:en 应存在于 Cells")
	}
	if len(fp.T0Cells) != 1 {
		t.Errorf("T0Cells 数量 = %d, want 1", len(fp.T0Cells))
	}
}

func TestBuildEmpty(t *testing.T) {
	fp, err := Build("m", "v1", "local", time.Now(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if fp.Cells == nil || fp.T0Cells == nil {
		t.Fatal("空采集也应返回非 nil 的 map")
	}
	if len(fp.Cells) != 0 || len(fp.T0Cells) != 0 {
		t.Errorf("空采集应有 0 cell: %d/%d", len(fp.Cells), len(fp.T0Cells))
	}
}

// --- Build 防御性校验（审查修复） ---

func TestBuildRejectsBadTemperature(t *testing.T) {
	cases := []struct {
		name string
		t    float64
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
		{"负温度", -0.5},
	}
	for _, c := range cases {
		rs := []*store.Response{resp("x:en", "42", store.ClassValid, c.t, 0)}
		if _, err := Build("m", "v1", "local", time.Now(), rs); err == nil {
			t.Errorf("温度 %s: 应返回错误", c.name)
		}
	}
}

func TestBuildRejectsMixedTemperature(t *testing.T) {
	rs := []*store.Response{
		resp("x:en", "42", store.ClassValid, 1.0, 0),
		resp("x:en", "42", store.ClassValid, 0.5, 1), // 同 cell 第二正温度
	}
	if _, err := Build("m", "v1", "local", time.Now(), rs); err == nil {
		t.Error("同 cell 混合正温度应返回错误（防分布静默混合）")
	}
}

func TestBuildSkipsDegenerateValid(t *testing.T) {
	// valid 但空 Normalized：preprocess 不变量保证正常路径不可达，防御外部篡改
	empty := resp("x:en", "", store.ClassValid, 1.0, 0)
	// 超长 Normalized：正常一词回答远低于 MaxNormalizedLen
	long := resp("x:en", strings.Repeat("a", MaxNormalizedLen+1), store.ClassValid, 1.0, 1)
	ok := resp("x:en", "42", store.ClassValid, 1.0, 2)

	fp, err := Build("m", "v1", "local", time.Now(), []*store.Response{empty, long, ok})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	x := fp.Cells["x:en"]
	if x.N != 1 || x.Dist["42"] != 1 || len(x.Dist) != 1 {
		t.Errorf("退化 valid 不应入分布: N=%d dist=%v", x.N, x.Dist)
	}
	// 但它们计入 valid_rate 分母（total=3）
	if !approx(x.ValidRate, 1.0/3, 1e-15) {
		t.Errorf("valid_rate = %v, want 1/3", x.ValidRate)
	}
}

func TestDistanceAllZeroCounts(t *testing.T) {
	// 全零计数 dist（len>0 但总和 0，仅手改 JSON 可达）：Normalize 得空分布，
	// 与空 dist 同等过滤，不得以 JSD(∅,∅)=0 冒充"完全一致"
	a := fpWith(map[string]store.CellDist{"x:en": {Dist: map[string]int{"a": 0, "b": 0}, N: 30}}, nil)
	b := fpWith(map[string]store.CellDist{"x:en": cellDist(30, 30)}, nil)
	if got, n := Distance(a, b); got != 0 || n != 0 {
		t.Errorf("全零计数 dist 应被过滤: got (%v,%d), want (0,0)", got, n)
	}
}

func TestJSDDegenerateInputs(t *testing.T) {
	// 退化输入快照（正常路径被 cellJSDs 过滤，此处固化语义防回归）：
	// JSD(∅,∅)=0、JSD(∅,q)=0.5（与 scipy 对等输入行为不一致，属 Go 约定自洽产物）
	if got := JSD(map[string]float64{}, map[string]float64{}); got != 0 {
		t.Errorf("JSD(∅,∅) = %v, want 0", got)
	}
	// NaN 输入原样传播（函数内不校验，防线在 cellJSDs）
	if got := JSD(map[string]float64{"a": math.NaN()}, map[string]float64{"a": 1}); !math.IsNaN(got) {
		t.Errorf("JSD(NaN,…) 应传播 NaN, got %v", got)
	}
	// 非归一化输入不保证有界（前置条件文档化）
	if got := JSD(map[string]float64{"a": 2}, map[string]float64{"b": 1}); got != 1.5 {
		t.Errorf("JSD(未归一化) = %v, want 1.5（前置条件外行为快照）", got)
	}
}

// --- Distance（Eq.1 距离，cell 双方 ≥10 过滤） ---

func cellDist(n int, counts ...int) store.CellDist {
	labels := []string{"a", "b", "c", "d", "e", "f", "g"}
	dist := map[string]int{}
	for i, c := range counts {
		dist[labels[i]] = c
	}
	return store.CellDist{Dist: dist, N: n}
}

func fpWith(cells, t0 map[string]store.CellDist) *store.Fingerprint {
	return &store.Fingerprint{ModelID: "m", Version: "v1", Cells: cells, T0Cells: t0}
}

func TestDistanceFiltersCells(t *testing.T) {
	a := fpWith(map[string]store.CellDist{
		"x:en": cellDist(30, 15, 15), // 双方 N≥10 → 计入，JSD=0.3112781244591328
		"y:en": cellDist(20, 20),     // 对方 N=6 <10 → 剔除
		"z:en": cellDist(30, 30),     // 单方有 → 剔除
	}, nil)
	b := fpWith(map[string]store.CellDist{
		"x:en": cellDist(30, 30, 0),
		"y:en": cellDist(6, 2, 2, 2), // N=6 <10
	}, nil)

	got, n := Distance(a, b)
	if n != 1 {
		t.Fatalf("参与 cell 数 = %d, want 1", n)
	}
	if !approx(got, 0.3112781244591328, 1e-12) {
		t.Errorf("Distance = %v, want scipy² 0.3112781244591328", got)
	}
}

func TestDistanceMean(t *testing.T) {
	// 两个计入 cell：0.3112781244591328 与 1.0（disjoint）→ 均值 0.6556390622295664
	a := fpWith(map[string]store.CellDist{
		"x:en": cellDist(30, 15, 15),
		"y:en": cellDist(30, 30),
	}, nil)
	b := fpWith(map[string]store.CellDist{
		"x:en": cellDist(30, 30),
		"y:en": cellDist(30, 0, 30),
	}, nil)

	got, n := Distance(a, b)
	want := (0.3112781244591328 + 1.0) / 2
	if n != 2 {
		t.Fatalf("参与 cell 数 = %d, want 2", n)
	}
	if !approx(got, want, 1e-12) {
		t.Errorf("Distance = %v, want %v", got, want)
	}
}

func TestDistanceNoComparableCells(t *testing.T) {
	// 无共同 cell → (0, 0)，调用方判 inconclusive
	a := fpWith(map[string]store.CellDist{"x:en": cellDist(30, 30)}, nil)
	b := fpWith(map[string]store.CellDist{"y:en": cellDist(30, 30)}, nil)
	if got, n := Distance(a, b); got != 0 || n != 0 {
		t.Errorf("无共同 cell: got (%v,%d), want (0,0)", got, n)
	}
	// 共同 cell 但双方均无有效样本（dist 空）
	a2 := fpWith(map[string]store.CellDist{"x:en": {N: 30}}, nil)
	b2 := fpWith(map[string]store.CellDist{"x:en": {N: 30}}, nil)
	if got, n := Distance(a2, b2); got != 0 || n != 0 {
		t.Errorf("空 dist: got (%v,%d), want (0,0)", got, n)
	}
	// nil map 安全
	if got, n := Distance(&store.Fingerprint{}, &store.Fingerprint{}); got != 0 || n != 0 {
		t.Errorf("nil map: got (%v,%d), want (0,0)", got, n)
	}
}

// --- T=0 变体 ---

func TestDistanceT0(t *testing.T) {
	a := fpWith(nil, map[string]store.CellDist{
		"x:en": cellDist(3, 3), // T=0 n=3，门槛 ≥1
	})
	b := fpWith(nil, map[string]store.CellDist{
		"x:en": cellDist(3, 0, 3), // 与 A 不相交 → JSD=1
	})
	got, n := DistanceT0(a, b)
	if n != 1 {
		t.Fatalf("参与 cell 数 = %d, want 1", n)
	}
	if !approx(got, 1.0, 1e-12) {
		t.Errorf("DistanceT0 = %v, want 1.0", got)
	}
	// 一致 → 0
	b2 := fpWith(nil, map[string]store.CellDist{"x:en": cellDist(3, 3)})
	if got, _ := DistanceT0(a, b2); got != 0 {
		t.Errorf("DistanceT0 一致 = %v, want 0", got)
	}
	// T=0 与 T=1.0 互不干扰
	if _, n := Distance(a, b); n != 0 {
		t.Errorf("T0 分布不应计入 Cells 距离: n=%d, want 0", n)
	}
}
