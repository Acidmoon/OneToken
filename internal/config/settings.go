package config

// Settings 集中全部阈值/规则/采样参数（设计文档 §3.1、§5、§12）。
// 所有魔法数字集中于此，代码引用配置而非常量。
type Settings struct {
	// 采样与审计默认（设计 §3.1）
	EnrollNT1          int     // T=1.0 参考指纹采样次数 n（论文：30）
	EnrollNT0          int     // T=0 采样次数（论文：3）
	FrontierUSD        float64 // 前沿定价阈值：≥$5 / 1M 输入 token
	FrontierNT1        int     // 前沿模型 T=1.0 采样次数（论文：15）
	AuditK             int     // 审计默认探针 cell 数（8）
	AuditN             int     // 审计默认每 cell 采样数（15）
	OutputTokenCap     int     // 输出上限（论文 12→16 最终值；按协议映射 max_tokens/max_output_tokens）
	ReasoningMaxTokens int     // 推理通道（系统 2，v0.19）输出上限：思考链 + 最终回答完整输出（DeepSeek 实测 512 足够；按协议映射）
	StoreOutput        bool    // Responses API store 参数（默认 false，不落 OpenAI 平台）

	// 探测器（设计 §5）
	T0ProbeN                  int     // T=0 一致性探针样本数（n≥5，二项换算）
	MinValidSamples           int     // cell 双方 ≥10 有效样本才进入 JSD 平均（论文 Eq.1）
	ValidRateQC               float64 // valid 率 < 0.80 仅作模型级 QC（论文试点 per-model 标准）
	CompletionTokenNormalMax  int     // 正常一词答案 token 数上限（1–6）
	CompletionTokenAnomalyMin int     // 异常端点阈值（40–60）
	KMinCells                 int     // 有效 cell < k_min → inconclusive（占位值 3；设计文档无既定取值，M1 校准后确认）
	T0DeterministicRatio      float64 // T=0 确定性 cell 占比 < 此值 → temperature-not-honored（论文 90.4%/84.5%，保守 0.80，需本地校准）
	T0MinJudgedCells          int     // T=0 判定所需最少 judged cell 数（< 此值不判定并置 T0NotJudged；默认 3，设计探针 3~5 cell）
	CacheUniqueMax            int     // 缓存签名：cell 内唯一答案数 ≤ 此值且 n≥CacheMinN → 方差崩溃嫌疑（默认 2）
	CacheMinN                 int     // 缓存签名：cell 有效样本下限（默认 10）
	CacheLatencyMaxMS         int64   // 缓存签名联合低延迟条件（默认 0=禁用延迟条件，仅方差信号）
	RefusalDriftThreshold     float64 // refusal 率 |审计−基线| 突变阈值 → safety-layer-change（默认 0.15）
	UnreachableFailRatio      float64 // 失败任务占比 ≥ 此值 → unreachable（默认 0.8）

	// 漂移（设计 §12）
	DriftBaseline float64 // 参考噪声底线（论文：0.140）
	DriftWindow   int     // 趋势窗口（近 N 次）

	// 判定（M2.5，设计 §3.4）
	TauInconclusiveBuffer float64 // inconclusive 缓冲：|s−τ| ≤ 此值 → 不确定（τ CI 缺口裁决：
	// 校准未存 τ 自身 CI，用绝对缓冲代替；背景 genuine 中位 0.075 / 跨 provider 0.227，
	// 默认 0.02 保守小缓冲，需按本地校准数据实测调整）

	// 内置参考线（v0.22，compare 直比路径；未校准中位数基线，非 ROC 校准操作点——
	// 误报/漏报率未知，正式使用前建议 calibrate；跨 provider 同模型距离中位 0.227 >
	// 0.140，健康对可能判 suspicious 属服务栈差异）
	BuiltinTauDirect    float64 // direct 通道（M1.6 噪声底线 0.140，§3.4）
	BuiltinTauReasoning float64 // reasoning 通道（v0.20 建议区间 0.15–0.18 取 0.16；M2.9 正式校准后更新）

	// 比较报告参考线（M1.6 实测基线，仅可视化解释用，不参与判定）
	RefLineSameModel     float64 // 同模型分裂半距离中位（论文 0.075）
	RefLineCrossProvider float64 // 跨 provider 同模型距离中位（L8 复现 0.227；服务栈差异参考）

	// 传输层（M2.2，设计 §10.1）：重试矩阵与成本护栏
	MaxRetries       int   // 单请求最大重试次数（不含首次；默认 3）
	RetryBaseDelayMS int   // 指数退避基数（毫秒；默认 500，含 jitter 幅度）
	RetryMaxDelayMS  int   // 退避上限（毫秒；默认 8000）
	MaxResponseBytes int64 // 响应体字节上限（成本护栏①；默认 1 MiB）
	CompletionSlack  int   // completion 长度护栏容差（成本护栏②；默认 16）

	// 存储
	// data 目录由 store.DefaultRoot 管理（ONETOKEN_DATA 覆盖，默认 ~/.onetoken/data/），
	// 不在 Settings 中重复定义（v0.5 JSON 存储决议）。
}

// DefaultSettings 返回阈值/采样参数默认值（集中配置的基准）。
func DefaultSettings() Settings {
	return Settings{
		EnrollNT1:          30,
		EnrollNT0:          3,
		FrontierUSD:        5.0,
		FrontierNT1:        15,
		AuditK:             8,
		AuditN:             15,
		OutputTokenCap:     16,
		ReasoningMaxTokens: 512,
		StoreOutput:        false,

		T0ProbeN:                  5,
		MinValidSamples:           10,
		ValidRateQC:               0.80,
		CompletionTokenNormalMax:  6,
		CompletionTokenAnomalyMin: 40,
		KMinCells:                 3,

		T0DeterministicRatio:  0.80,
		T0MinJudgedCells:      3,
		CacheUniqueMax:        2,
		CacheMinN:             10,
		CacheLatencyMaxMS:     0,
		RefusalDriftThreshold: 0.15,
		UnreachableFailRatio:  0.8,

		DriftBaseline: 0.140,
		DriftWindow:   5,

		TauInconclusiveBuffer: 0.02,

		BuiltinTauDirect:    0.140,
		BuiltinTauReasoning: 0.16,

		RefLineSameModel:     0.075,
		RefLineCrossProvider: 0.227,

		MaxRetries:       3,
		RetryBaseDelayMS: 500,
		RetryMaxDelayMS:  8000,
		MaxResponseBytes: 1 << 20,
		CompletionSlack:  16,
	}
}

// ApplyEnv 用环境变量覆盖部分设置（预留扩展；当前无覆盖项，
// data 目录路径由 store.DefaultRoot 的 ONETOKEN_DATA 负责）。
func (s *Settings) ApplyEnv() {}
