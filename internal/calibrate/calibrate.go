// Package calibrate 实现阈值校准算法（设计文档 §3.4）：分裂半 genuine/impostor
// 试验构造、ROC/AUC/EER、操作点阈值（τ_fpr1/τ_fpr5）与 bootstrap CI、
// LOO 1-NN 谱系分类（复现用途，投产路径 v1.2）。全部 Go 自写。
//
// 得分约定（与 §3.3 判定一致）：**得分越低越像 genuine**（s ≤ τ → pass）。
// 因此 ROC 上 τ 大 → 宽松（FPR/TPR 高），τ 小 → 严格；AUC = P(impostor > genuine)
// （曼-惠特尼 U 统计，tie 取 0.5），perfect 分类 AUC=1，随机分类 AUC=0.5。
//
// 校准输出写入 store.Calibration（§4.3 calibrations.json 分档结构）；分档键
// （Scope/K/NPerCell/通道）由调用方填充，本包只计算数值。
package calibrate

import (
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"onetoken/internal/store"
)

// Options 是校准参数（缺省值见 withDefaults）。
type Options struct {
	FPRTarget  float64 // 主操作点 τ_fpr1（默认 0.01，设计 §3.4 误报优先）
	FPRTarget2 float64 // 辅操作点 τ_fpr5（默认 0.05）
	NResamples int     // bootstrap 重采样次数（默认 1000，上限 maxResamples）
	Seed       int64   // bootstrap RNG 种子（默认 defaultSeed；值类型无法区分"未设置"与显式 0，二者等价）
}

const (
	defaultFPRTarget  = 0.01
	defaultFPRTarget2 = 0.05
	defaultResamples  = 1000
	maxResamples      = 100000 // 资源上限：tprs 预分配 8B×n，防配置错误 OOM
	defaultSeed       = 20260805
)

func (o Options) withDefaults() Options {
	if math.IsNaN(o.FPRTarget) || math.IsInf(o.FPRTarget, 0) || o.FPRTarget <= 0 || o.FPRTarget > 1 {
		o.FPRTarget = defaultFPRTarget
	}
	if math.IsNaN(o.FPRTarget2) || math.IsInf(o.FPRTarget2, 0) || o.FPRTarget2 <= 0 || o.FPRTarget2 > 1 {
		o.FPRTarget2 = defaultFPRTarget2
	}
	if o.NResamples <= 0 {
		o.NResamples = defaultResamples
	}
	if o.NResamples > maxResamples {
		o.NResamples = maxResamples
	}
	if o.Seed == 0 {
		o.Seed = defaultSeed
	}
	return o
}

// rocResult 是 ROC 计算内部结果：阈值与点一一对应（同下标）。
type rocResult struct {
	thresholds []float64
	points     []store.ROCPoint
}

// computeROC 对得分阈值扫描生成 ROC 曲线（FPR 升序，含 (0,0) 起点与 (1,1) 终点）。
// 阈值 τ 语义：s ≤ τ 判 genuine。points[i].FPR/TPR = 阈值 thresholds[i] 处的值。
func computeROC(genuine, impostor []float64) rocResult {
	g := append([]float64(nil), genuine...)
	i := append([]float64(nil), impostor...)
	sort.Float64s(g)
	sort.Float64s(i)
	ng, ni := len(g), len(i)
	if ng == 0 || ni == 0 {
		return rocResult{}
	}
	// 唯一阈值（合并升序去重）
	merged := make([]float64, 0, ng+ni)
	merged = append(merged, g...)
	merged = append(merged, i...)
	sort.Float64s(merged)
	uniq := make([]float64, 0, len(merged))
	var last float64
	for k, v := range merged {
		if k == 0 || v != last {
			uniq = append(uniq, v)
			last = v
		}
	}
	res := rocResult{
		thresholds: make([]float64, 0, len(uniq)+1),
		points:     make([]store.ROCPoint, 0, len(uniq)+1),
	}
	// 起点：τ=-Inf（任何得分 > -Inf → FPR=TPR=0）
	res.thresholds = append(res.thresholds, math.Inf(-1))
	res.points = append(res.points, store.ROCPoint{})
	gi, ii := 0, 0
	for _, tau := range uniq {
		for gi < ng && g[gi] <= tau {
			gi++
		}
		for ii < ni && i[ii] <= tau {
			ii++
		}
		res.thresholds = append(res.thresholds, tau)
		res.points = append(res.points, store.ROCPoint{
			FPR: float64(ii) / float64(ni),
			TPR: float64(gi) / float64(ng),
		})
	}
	return res
}

// AUC 用曼-惠特尼 U 统计计算 ROC 下面积：
// AUC = P(impostor > genuine) + 0.5·P(impostor == genuine)。
// 前置条件：两分布均非空；perfect 分离 → 1，完全混叠 → 0.5，tie 规则下
// AUC(g,i) + AUC(i,g) = 1 恒成立。
func AUC(genuine, impostor []float64) float64 {
	if len(genuine) == 0 || len(impostor) == 0 {
		return 0
	}
	is := append([]float64(nil), impostor...)
	sort.Float64s(is)
	var sum float64
	for _, g := range genuine {
		lower := sort.SearchFloat64s(is, g)                                  // #i < g
		upper := sort.Search(len(is), func(k int) bool { return is[k] > g }) // #i ≤ g
		greater := len(is) - upper                                           // #i > g
		equal := upper - lower                                               // #i == g
		sum += (float64(greater) + 0.5*float64(equal)) / float64(len(is))
	}
	return sum / float64(len(genuine))
}

// EER 返回等错误率：ROC 上 |FPR − (1−TPR)| 最小处的 (FPR+FNR)/2。
func EER(res rocResult) float64 {
	best := math.Inf(1)
	var eer float64
	for _, p := range res.points {
		if d := math.Abs(p.FPR + p.TPR - 1); d < best {
			best = d
			eer = (p.FPR + (1 - p.TPR)) / 2
		}
	}
	return eer
}

// ThresholdAtFPR 返回 FPR ≤ target 的**最宽松**阈值（= FPR 最大的满足点）及该处 TPR。
// 语义：τ 是误报率不超 target 的前提下最宽的操作点（误报优先，设计 §3.4）。
// 若 target 低于最小非零 FPR（数据分辨率不足，如小样本），返回最严点 (τ=-Inf, TPR=0)，
// 调用方应据此告警"校准数据不足以支撑该操作点"。空 ROC 返回 (0,0)。
func ThresholdAtFPR(res rocResult, target float64) (threshold, tpr float64) {
	if len(res.points) == 0 {
		return 0, 0
	}
	best := 0 // 起点 (0,0) 恒满足 fpr ≤ target（target ≥ 0）
	for i, p := range res.points {
		if p.FPR <= target {
			best = i
		} else {
			break // FPR 升序，此后均超限
		}
	}
	return res.thresholds[best], res.points[best].TPR
}

// BootstrapTPRCI 对 τ@target 处的 TPR 做 bootstrap 百分位 CI（2.5%–97.5%）。
// 每次重采样对 genuine/impostor 有放回抽等量样本后重算阈值与 TPR。
// 返回 (CI, 原始数据点估计 TPR)。RNG 由调用方注入（可复现）。
//
// 注：O(B·(n_g+n_i)·log(n))，M1.6 重放（107 万 impostor）若慢改为分位数法
// （预排序 + 重采样计数），语义不变。
func BootstrapTPRCI(genuine, impostor []float64, target float64, nResamples int, rng *rand.Rand) (ci [2]float64, tpr float64) {
	if len(genuine) == 0 || len(impostor) == 0 || nResamples <= 0 {
		return [2]float64{}, 0
	}
	_, tpr = ThresholdAtFPR(computeROC(genuine, impostor), target)
	tprs := make([]float64, 0, nResamples)
	for b := 0; b < nResamples; b++ {
		g := sampleWR(genuine, rng)
		i := sampleWR(impostor, rng)
		_, t := ThresholdAtFPR(computeROC(g, i), target)
		tprs = append(tprs, t)
	}
	sort.Float64s(tprs)
	lo := tprs[pctIdx(len(tprs), 0.025)]
	hi := tprs[pctIdx(len(tprs), 0.975)]
	return [2]float64{lo, hi}, tpr
}

// sampleWR 有放回重采样（等长）。
func sampleWR(xs []float64, rng *rand.Rand) []float64 {
	out := make([]float64, len(xs))
	for k := range out {
		out[k] = xs[rng.IntN(len(xs))]
	}
	return out
}

// pctIdx 返回百分位索引（clamp 到 [0, n-1]）。
func pctIdx(n int, p float64) int {
	idx := int(float64(n) * p)
	if idx >= n {
		idx = n - 1
	}
	if idx < 0 {
		idx = 0
	}
	return idx
}

// Calibrate 从 genuine/impostor 距离分布计算完整校准指标并填入 store.Calibration。
// 分档键（Scope/K/NPerCell/RefChannel/TargetChannel）与 CalibratedAt 由调用方
// 填充/覆盖；本函数只计算数值字段。
//
// 防御性语义（设计 v0.7 留痕）：
//   - 输入含非有限得分（NaN/±Inf）时**过滤后重算**（GenuineN/ImpostorN 记录
//     过滤后有效数）；全部被过滤视为空输入；
//   - 空输入（或过滤后为空）→ 返回 **nil**（无效校准，调用方跳过/回退全局档，
//     防"EER=0 看似 perfect、τ=0 看似可用"的假校准落盘）；
//   - 操作点阈值非有限（数据分辨率不足，τ=−∞）→ 返回 **nil**：非有限阈值无法
//     JSON 序列化（SaveCalibrations 必然失败），且 −∞ 会被判定层误读为全拒。
func Calibrate(genuine, impostor []float64, opts Options) *store.Calibration {
	o := opts.withDefaults()
	g, i := filterFinite(genuine), filterFinite(impostor)
	if len(g) == 0 || len(i) == 0 {
		return nil
	}
	res := computeROC(g, i)
	tau1, tpr1 := ThresholdAtFPR(res, o.FPRTarget)
	tau5, _ := ThresholdAtFPR(res, o.FPRTarget2)
	if !isFinite(tau1) || !isFinite(tau5) {
		return nil // 操作点不可达，该档位无效（调用方回退全局档并标注）
	}
	rng := rand.New(rand.NewPCG(0, uint64(o.Seed)))
	ci, _ := BootstrapTPRCI(g, i, o.FPRTarget, o.NResamples, rng)
	return &store.Calibration{
		GenuineN:     len(g),
		ImpostorN:    len(i),
		AUC:          AUC(g, i),
		EER:          EER(res),
		TauFPR1:      tau1,
		TauFPR1TPR:   tpr1,
		TPRCI:        []float64{ci[0], ci[1]},
		TauFPR5:      tau5,
		ROC:          res.points,
		CalibratedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

// filterFinite 过滤 NaN/±Inf（得分应为 JSD ≥0 的非有限安全值，异常数据防御）。
func filterFinite(xs []float64) []float64 {
	out := make([]float64, 0, len(xs))
	for _, x := range xs {
		if isFinite(x) {
			out = append(out, x)
		}
	}
	return out
}

// isFinite 判断非 NaN 且非 ±Inf（标准库无 math.IsFinite）。
func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// SplitHalves 按 sample_idx 奇偶把响应切为两半（论文"重复奇偶切分"）。
// 同一采集的两半指纹距离 → genuine 分布；跨模型半指纹距离 → impostor 分布
// （设计 §3.4）。sample_idx 不连续（幂等去重后）不影响奇偶归属。
func SplitHalves(responses []*store.Response) (even, odd []*store.Response) {
	for _, r := range responses {
		if r == nil {
			continue
		}
		if r.SampleIdx%2 == 0 {
			even = append(even, r)
		} else {
			odd = append(odd, r)
		}
	}
	return even, odd
}
