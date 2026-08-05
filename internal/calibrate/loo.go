package calibrate

import (
	"math"

	"onetoken/internal/fingerprint"
	"onetoken/internal/store"
)

// LOO1NN 留一法最近邻家族分类（复现论文家族分类；设计 §2.1 投产路径 v1.2）。
//
// 对每个指纹 i，以其余指纹为库，取 JSD 距离最近者（fingerprint.Distance，B′
// 过滤语义同判定路径）的家族作为预测；等距时取库中序号最小者（确定性）。
// 返回 modelID → 预测家族。仅产出线索（§3.5），不构成判定。
//
// 防御性规则：nil 指纹跳过；与候选**无共同可比较 cell**（Distance 参与数 n==0）
// 时跳过——(0,0) 是"不可比"而非"距离 0"，不得当作最近邻（否则家族静默错配）。
func LOO1NN(fps []*store.Fingerprint, familyOf func(modelID string) string) map[string]string {
	out := make(map[string]string, len(fps))
	for i, a := range fps {
		if a == nil {
			continue
		}
		bestDist := math.Inf(1)
		best := -1
		for j, b := range fps {
			if j == i || b == nil {
				continue
			}
			d, n := fingerprint.Distance(a, b)
			if n == 0 {
				continue // 无共同可比较 cell（B′ 空），不可作为最近邻候选
			}
			if d < bestDist || best == -1 {
				bestDist = d
				best = j
			}
		}
		if best >= 0 {
			out[a.ModelID] = familyOf(fps[best].ModelID)
		}
	}
	return out
}
