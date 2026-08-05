// Package fingerprint 实现经验分布估计与基 2 JSD 距离（设计文档 §3.3；论文 Eq.1）。
//
// 指纹 F(M) = {(t,ℓ): p̂_{t,ℓ}}：每个 cell 的有效样本经验分布（dist 存原始计数，
// 归一化在本层完成，与 §4.3 文件结构一致）+ T=0 变体（t0_cells）。
//
// 距离（论文 Eq.1）：
//
//	D(Mₐ, M_b) = (1/|B′|) Σ₍ₜ,ℓ₎∈B′ JSD(p̂ᵃₜ,ℓ ‖ p̂ᵇₜ,ℓ)
//
// B′ = 双方均 ≥10 有效样本的 cell 交集（MinCellSamples）。
//
// JSD 基 2 = (KL(p‖m) + KL(q‖m)) / (2·ln2)，m=(p+q)/2；KL 取自然对数、整体除以 ln2；
// 采用 0·ln0=0 约定、无任何平滑——m 的支撑是 p∪q 的并集，JSD 从不出现未定义项；
// 加性平滑会系统性改变每个值，与论文"支持不相交也可用"的约定冲突。
//
// 单位标度（sqrt vs 原始）：本实现返回**原始标度**（不取 sqrt）。scipy 的
// jensenshannon(p, q, base=2) 返回的是 sqrt(JSD)，对拍时需平方；最终常数标度
// 以 M1 前置门（M1.5，Zenodo 软件归档）pin 的论文实现语义为准。
package fingerprint

import (
	"errors"
	"fmt"
	"math"
	"time"

	"onetoken/internal/store"
)

const (
	// MinCellSamples 是论文 Eq.1 门槛：cell 双方均 ≥10 有效样本才入 JSD 平均。
	MinCellSamples = 10
	// MinCellSamplesT0 是 T=0 变体门槛：T=0 每 cell 采样 n=3，无法达到 ≥10，
	// 放宽为 ≥1（T=0 用于确定性比对，见设计 §5 探测器 temperature-not-honored）。
	MinCellSamplesT0 = 1
	// MaxNormalizedLen 是 Build 对归一化串的长度防御上限（字节）：合法样本为
	// 一词回答（≤16 token，正常数十字节），超限视为异常数据不入分布。
	MaxNormalizedLen = 1024
)

// ErrTemperature 是 Build 遇到非法/混合温度时返回的错误（不静默处理）。
var ErrTemperature = errors.New("fingerprint: 非法或混合温度")

// KL 计算 KL 散度 D_KL(p‖q)：自然对数，采用 0·ln0=0 约定（p 侧为零的项直接跳过）。
// 若存在 p[k]>0 而 q[k]==0，返回 +Inf（数学上发散）；JSD 内部的 m 支撑为 p∪q
// 的并集，不会触发此分支。
// 前置条件：p、q 为有限值、非负的概率分布（输入未归一化时结果无意义）；
// NaN/±Inf/负值输入原样传播（本包不做函数内校验，防线在 cellJSDs）。
func KL(p, q map[string]float64) float64 {
	var sum float64
	for k, pv := range p {
		if pv == 0 {
			continue // 0·ln0=0
		}
		sum += pv * math.Log(pv/q[k])
	}
	return sum
}

// JSD 计算基 2 Jensen–Shannon 散度（原始标度，不取 sqrt）：
// (KL(p‖m) + KL(q‖m)) / (2·ln2)，m = (p+q)/2。
// 对称、有界 [0,1]；p==q 时 0，支持不相交时 1.0。无平滑。
// 前置条件：p、q 为有限值、非负、归一化（总和 1）的概率分布。
//   - 空分布是**退化输入**：JSD(∅,∅)=0、JSD(∅,q)=0.5（p 侧零项全跳过，剩
//     KL(q‖q/2)/(2·ln2)），与 scipy 对等输入的行为不一致；调用方必须保证非空
//     （cellJSDs 已对空/全零计数分布过滤）。
//   - 非归一化输入不保证有界（如 p={a:2}, q={b:1} → 1.5）；
//   - NaN/±Inf/负值输入原样传播（距离防线在 cellJSDs）。
func JSD(p, q map[string]float64) float64 {
	m := make(map[string]float64, len(p)+len(q))
	for k, v := range p {
		m[k] = v * 0.5
	}
	for k, v := range q {
		m[k] += v * 0.5
	}
	return (KL(p, m) + KL(q, m)) / (2 * math.Ln2)
}

// Normalize 将原始计数分布归一化为概率分布（总和 1）。空分布返回空 map
// （调用方应避免对空分布调用 JSD）。
func Normalize(dist map[string]int) map[string]float64 {
	total := 0
	for _, v := range dist {
		total += v
	}
	if total == 0 {
		return map[string]float64{}
	}
	out := make(map[string]float64, len(dist))
	for k, v := range dist {
		out[k] = float64(v) / float64(total)
	}
	return out
}

// Build 从一次采集的响应列表构建指纹：按 cell × 温度分组，仅 valid 分类进入
// 经验分布（refusal/有效率不进指纹，设计 §3.2）；T=0 响应进入 T0Cells，
// 其余（T>0，通常 1.0）进入 Cells。每 cell 记录实际有效样本数 N（指纹门槛依据）
// 与有效率 valid_rate（cell 内全部响应，含无效/拒绝/空，供 detector QC）。
//
// 防御性校验（不静默，违反即返回错误）：
//   - 温度必须有限且 ≥0（NaN/±Inf/负 → error）；
//   - 同一 cell 只允许一种正温度（设计仅定义 T=0 与单一 T>0 建指纹；
//     混入 1.0 与 0.5 等 → error，防分布静默混合）。
//
// 防御性跳过（异常数据不入分布，计数仍计入 valid_rate 分母）：
//   - valid 但 Normalized 为空（preprocess 不变量保证 valid ⇒ 非空，防外部篡改）；
//   - Normalized 超过 MaxNormalizedLen（一词回答远低于该上限）。
//
// responses 为空或全为 nil 时返回指纹骨架（空 map）与 nil 错误（由上层 detector 判 QC）。
func Build(modelID, version, refSource string, collectedAt time.Time, responses []*store.Response) (*store.Fingerprint, error) {
	type agg struct {
		dist  map[string]int
		n     int // 有效样本数
		total int // 全部响应数
		t     float64
	}
	cells := map[string]*agg{}
	t0 := map[string]*agg{}
	for _, r := range responses {
		if r == nil {
			continue
		}
		if !isFinite(r.Temperature) || r.Temperature < 0 {
			return nil, fmt.Errorf("%w: cell %q 温度 %v", ErrTemperature, r.Cell, r.Temperature)
		}
		var a *agg
		if r.Temperature == 0 {
			a = t0[r.Cell]
			if a == nil {
				a = &agg{dist: map[string]int{}}
				t0[r.Cell] = a
			}
		} else {
			a = cells[r.Cell]
			if a == nil {
				a = &agg{dist: map[string]int{}, t: r.Temperature}
				cells[r.Cell] = a
			} else if a.t != r.Temperature {
				return nil, fmt.Errorf("%w: cell %q 混入温度 %v 与 %v", ErrTemperature, r.Cell, a.t, r.Temperature)
			}
		}
		a.total++
		if r.Classification != store.ClassValid {
			continue // invalid/refusal/empty 不进指纹（无静默丢弃：上层仍有分类统计）
		}
		if r.Normalized == "" {
			continue // 防御：valid 必非空（preprocess 不变量）；空串不可作分布键
		}
		if len(r.Normalized) > MaxNormalizedLen {
			continue // 防御：超长归一化串不入分布（异常数据）
		}
		a.dist[r.Normalized]++
		a.n++
	}

	fp := &store.Fingerprint{
		SchemaVersion: 1, // 与 store.schemaVersion 一致（SaveFingerprint 亦强制覆盖）
		ModelID:       modelID,
		Version:       version,
		CollectedAt:   collectedAt.UTC().Format(time.RFC3339), // UTC Z（§4.2 约定）
		RefSource:     refSource,
		Cells:         map[string]store.CellDist{},
		T0Cells:       map[string]store.CellDist{},
	}
	for cell, a := range cells {
		fp.Cells[cell] = store.CellDist{
			Dist:      a.dist,
			N:         a.n,
			T:         a.t,
			ValidRate: rate(a.n, a.total),
		}
	}
	for cell, a := range t0 {
		fp.T0Cells[cell] = store.CellDist{
			Dist:      a.dist,
			N:         a.n,
			T:         0,
			ValidRate: rate(a.n, a.total),
		}
	}
	return fp, nil
}

func rate(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total)
}

// isFinite 判断浮点值非 NaN 且非 ±Inf（标准库无 math.IsFinite）。
func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// cellJSDs 计算两组 cell 分布的逐 cell JSD（minSamples 门槛参数化）。
// B′ 过滤规则（论文 Eq.1）：cell 同时存在、双方有效样本 ≥ minSamples、
// 双方均有实际样本（len 过滤 + Normalize 后非空，拦截全零计数等退化分布）、
// JSD 结果有限（防御篡改数据，理论不可达）。
func cellJSDs(cellsA, cellsB map[string]store.CellDist, minSamples int) map[string]float64 {
	out := make(map[string]float64)
	for cell, da := range cellsA {
		db, ok := cellsB[cell]
		if !ok {
			continue // 单方有的 cell 不可比
		}
		if da.N < minSamples || db.N < minSamples {
			continue // 有效样本不足门槛
		}
		pa, pb := Normalize(da.Dist), Normalize(db.Dist)
		if len(pa) == 0 || len(pb) == 0 {
			continue // 无有效样本（含全零计数 dist）不可比
		}
		v := JSD(pa, pb)
		if !isFinite(v) {
			continue // 防御：非有限 JSD 不进距离均值
		}
		out[cell] = v
	}
	return out
}

// CellJSDs 返回两个指纹的逐 cell JSD 明细（B′ 过滤后），供报告与判定
// （Audit.CellsDetail）使用。
func CellJSDs(a, b *store.Fingerprint) map[string]float64 {
	return cellJSDs(a.Cells, b.Cells, MinCellSamples)
}

// Distance 计算论文 Eq.1 距离：B′ 上逐 cell JSD 的算术平均。
// 返回 (距离, 参与 cell 数)——参与数供上层按有效 cell < k_min 判 inconclusive
// （设计 §5）；无共同可比较 cell 时返回 (0, 0)。
func Distance(a, b *store.Fingerprint) (float64, int) {
	return mean(cellJSDs(a.Cells, b.Cells, MinCellSamples))
}

// CellJSDsT0 返回 T=0 变体的逐 cell JSD（门槛 MinCellSamplesT0=1）。
func CellJSDsT0(a, b *store.Fingerprint) map[string]float64 {
	return cellJSDs(a.T0Cells, b.T0Cells, MinCellSamplesT0)
}

// DistanceT0 返回 T=0 变体距离（均值与参与 cell 数），供 T=0 一致性比对
// （设计 §5 temperature-not-honored 探测的分布侧工具）。
func DistanceT0(a, b *store.Fingerprint) (float64, int) {
	return mean(cellJSDs(a.T0Cells, b.T0Cells, MinCellSamplesT0))
}

func mean(js map[string]float64) (float64, int) {
	if len(js) == 0 {
		return 0, 0
	}
	var sum float64
	for _, v := range js {
		sum += v
	}
	return sum / float64(len(js)), len(js)
}
