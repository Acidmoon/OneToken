package config

// Settings 集中全部阈值/规则/采样参数（设计文档 §3.1、§5、§12）。
// 所有魔法数字集中于此，代码引用配置而非常量。
type Settings struct {
	// 采样与审计默认（设计 §3.1）
	EnrollNT1      int     // T=1.0 参考指纹采样次数 n（论文：30）
	EnrollNT0      int     // T=0 采样次数（论文：3）
	FrontierUSD    float64 // 前沿定价阈值：≥$5 / 1M 输入 token
	FrontierNT1    int     // 前沿模型 T=1.0 采样次数（论文：15）
	AuditK         int     // 审计默认探针 cell 数（8）
	AuditN         int     // 审计默认每 cell 采样数（15）
	OutputTokenCap int     // 输出上限（论文 12→16 最终值；按协议映射 max_tokens/max_output_tokens）
	StoreOutput    bool    // Responses API store 参数（默认 false，不落 OpenAI 平台）

	// 探测器（设计 §5）
	T0ProbeN                  int     // T=0 一致性探针样本数（n≥5，二项换算）
	MinValidSamples           int     // cell 双方 ≥10 有效样本才进入 JSD 平均（论文 Eq.1）
	ValidRateQC               float64 // valid 率 < 0.80 仅作模型级 QC（论文试点 per-model 标准）
	CompletionTokenNormalMax  int     // 正常一词答案 token 数上限（1–6）
	CompletionTokenAnomalyMin int     // 异常端点阈值（40–60）
	KMinCells                 int     // 有效 cell < k_min → inconclusive（占位值 3；设计文档无既定取值，M1 校准后确认）

	// 漂移（设计 §12）
	DriftBaseline float64 // 参考噪声底线（论文：0.140）
	DriftWindow   int     // 趋势窗口（近 N 次）

	// 存储
	// data 目录由 store.DefaultRoot 管理（ONETOKEN_DATA 覆盖，默认 ~/.onetoken/data/），
	// 不在 Settings 中重复定义（v0.5 JSON 存储决议）。
}

// DefaultSettings 返回阈值/采样参数默认值（集中配置的基准）。
func DefaultSettings() Settings {
	return Settings{
		EnrollNT1:      30,
		EnrollNT0:      3,
		FrontierUSD:    5.0,
		FrontierNT1:    15,
		AuditK:         8,
		AuditN:         15,
		OutputTokenCap: 16,
		StoreOutput:    false,

		T0ProbeN:                  5,
		MinValidSamples:           10,
		ValidRateQC:               0.80,
		CompletionTokenNormalMax:  6,
		CompletionTokenAnomalyMin: 40,
		KMinCells:                 3,

		DriftBaseline: 0.140,
		DriftWindow:   5,
	}
}

// ApplyEnv 用环境变量覆盖部分设置（预留扩展；当前无覆盖项，
// data 目录路径由 store.DefaultRoot 的 ONETOKEN_DATA 负责）。
func (s *Settings) ApplyEnv() {}
