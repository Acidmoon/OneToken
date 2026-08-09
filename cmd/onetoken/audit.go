package main

import (
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/spf13/cobra"

	"onetoken/internal/collector"
	"onetoken/internal/store"
	"onetoken/internal/verify"
)

// auditFlags 是 audit 命令参数。
type auditFlags struct {
	provider    string
	direct      directFlags
	claimed     string // claimed-model（参考指纹）
	k           int
	n           int
	tau         float64 // 直传阈值（>0 时跳过校准库匹配，冒烟/临时用）
	seed        int64
	concurrency int
	budgetCalls int  // 审计预算（调用次数上限，0=不限；成本护栏 M3）
	reasoning   bool // 目标端点为推理模型（系统 2：max_tokens=ReasoningMaxTokens）
	jsonOut     bool
}

var auditFlag = &auditFlags{}

var auditCmd = &cobra.Command{
	Use:   "audit",
	Short: "审计端点真伪（指纹距离 vs τ，判定 pass/suspicious/inconclusive）",
	Long: `audit 对目标端点做 k cell × n 次采样，与 claimed-model 的参考指纹比对，
输出距离与判定（设计 §3.4：pass ≤ τ−buf / suspicious > τ+buf / inconclusive）。

前置条件：claimed-model 已 enroll（参考指纹存在）。--tau auto（默认）按
(k,n,通道) 从校准库精确匹配；无匹配档拒绝审计（需先 calibrate 或 --tau 直传）。
--tau <float> 直传阈值（冒烟/临时，不查库）。

端点选择与 enroll 相同（--provider 或直传参数）。密钥只走环境变量。`,
	Example: `  onetoken audit --provider openrouter --claimed-model qwen/qwen3-8b --k 8 --n 15
  onetoken audit --base-url https://openrouter.ai --api-key-env OPENROUTER_API_KEY \
    --claimed-model qwen/qwen3-8b --k 8 --n 15 --tau 0.10`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		p, srcName, err := resolveProvider(cfg, auditFlag.provider, auditFlag.direct)
		if err != nil {
			return err
		}
		client, err := newClient(p, cfg.Settings)
		if err != nil {
			return err
		}
		st, err := setupStore()
		if err != nil {
			return err
		}
		b, err := loadBattery()
		if err != nil {
			return err
		}
		s := cfg.Settings

		// --tau 校验（审查 L3）：NaN/负值/Inf 会静默回落 auto（用户笔误被忽略）
		if auditFlag.tau != 0 && (auditFlag.tau < 0 || auditFlag.tau > 1 || math.IsNaN(auditFlag.tau) || math.IsInf(auditFlag.tau, 0)) {
			return fmt.Errorf("--tau=%v 非法（需 0=auto 或 [0,1] 内的有限值）", auditFlag.tau)
		}

		// 前置条件：参考指纹
		claimed, err := st.LoadFingerprint(auditFlag.claimed)
		if err != nil || claimed == nil {
			return fmt.Errorf("参考指纹不存在（先 enroll %q）: %w", auditFlag.claimed, err)
		}
		k := auditFlag.k
		if k <= 0 {
			k = s.AuditK
		}
		n := auditFlag.n
		if n <= 0 {
			n = s.AuditN
		}
		seed := auditFlag.seed
		if seed == 0 {
			seed = time.Now().UnixNano()
		}
		conc := resolveConcurrency(auditFlag.concurrency, p)

		// k 个 cell 子集（seeded RNG，种子持久化到 Audit.Seed，可复现）
		allCells := b.Cells()
		selected := pickCells(allCells, k, seed)

		// T=1.0 审计采样：幂等续采 id = audit-<timestamp>-<rand>（随机后缀防并行
		// 毫秒级碰撞共享响应文件/覆盖审计记录，审查 R-M6）
		auditID := fmt.Sprintf("audit-%s-%s", time.Now().UTC().Format("20060102T150405.000Z"), randHex(8))
		maxTokens := s.OutputTokenCap
		if auditFlag.reasoning {
			maxTokens = s.ReasoningMaxTokens // 推理通道（v0.19 系统 2）
		}
		cc := collector.Options{
			ID: auditID, Model: auditFlag.claimed, Concurrency: conc,
			MaxTokens: maxTokens, OnProgress: progressToStderr("audit"),
			Budget: newBudget(auditFlag.budgetCalls),
		}
		rs, err1 := collector.RunBattery(runCtx(), client, st, b, selected, n, 1.0, cc)
		if isAbort(err1) {
			return err1
		}
		failed1 := collector.CountTaskFailures(err1)

		// T=0 探针：前 3 个选中 cell × T0ProbeN（测量有效性预检，设计 §5）
		probeCells := selected[:minInt(len(selected), 3)]
		cc.ID = auditID + "-t0"
		cc.OnProgress = progressToStderr("audit T=0")
		cc.Budget = newBudget(auditFlag.budgetCalls)
		t0, err0 := collector.RunBattery(runCtx(), client, st, b, probeCells, s.T0ProbeN, 0.0, cc)
		if isAbort(err0) {
			return err0
		}
		failed0 := collector.CountTaskFailures(err0)
		// 段级失败率（审查 R-M1）：T0 段全败不被 T1 成功稀释——命中即不可达，
		// 不再进入判定（verify 的全局比率会掩盖单段不可用）
		if failRatio(failed1, len(rs)) >= s.UnreachableFailRatio ||
			failRatio(failed0, len(t0)) >= s.UnreachableFailRatio {
			return fmt.Errorf("audit: 端点不可达（T1 失败率 %.0f%% / T0 失败率 %.0f%%）",
				failRatio(failed1, len(rs))*100, failRatio(failed0, len(t0))*100)
		}

		// 校准库（--tau auto 用）
		cals, err := st.LoadCalibrations()
		if err != nil {
			return fmt.Errorf("读取校准库失败: %w", err)
		}
		scope := "global"
		vopts := verify.Options{
			Settings:      s,
			Battery:       b,
			K:             k,
			N:             n,
			Scope:         scope,
			Calibrations:  cals,
			RefChannel:    claimedChannel(claimed), // 参考通道（direct|reasoning，v0.19 同通道比对）
			Channel:       claimedChannel(claimed), // 短路语义：推理通道思考链正常
			TargetChannel: srcName,                 // 被审计端点 provider（校准分档键，审查 R-7）
			T0Responses:   t0,
			TauOverride:   auditFlag.tau, // >0 直传阈值（冒烟/临时），0=auto 查库
		}
		res, err := verify.VerifyAudit(rs, claimed, vopts)
		if err != nil {
			return err
		}

		// 上游 provider（聚合器透传，§9.2 解释不稳定）：取响应记录首个非空
		upstream := ""
		for _, r := range rs {
			if r.Provider != "" {
				upstream = r.Provider
				break
			}
		}
		// 落盘审计记录（§4.3 audits/<id>.json）
		rec := &store.Audit{
			SchemaVersion:         1,
			ID:                    auditID,
			Endpoint:              p.BaseURL,
			ClaimedModel:          auditFlag.claimed,
			RefFingerprintVersion: claimed.Version,
			K:                     k,
			N:                     n,
			SelectedCells:         selected,
			Seed:                  seed,
			Score:                 res.Score,
			Threshold:             res.Threshold,
			ThresholdScope:        scope,
			Verdict:               res.Verdict,
			CellsDetail:           res.CellsDetail,
			QCFlags:               res.Flags.List(),
			Provider:              upstream,
			AuditedAt:             time.Now().UTC().Format(time.RFC3339),
		}
		if err := st.SaveAudit(rec); err != nil {
			return fmt.Errorf("保存审计记录失败: %w", err)
		}

		out := map[string]any{
			"audit_id":   auditID,
			"claimed":    auditFlag.claimed,
			"endpoint":   p.BaseURL,
			"verdict":    res.Verdict,
			"score":      res.Score,
			"threshold":  res.Threshold,
			"cells_used": res.CellsUsed,
			"flags":      flagsOrEmpty(res.Flags),
			"reason":     res.Reason,
			"upstream":   upstream,
			"k":          k,
			"n":          n,
			"seed":       seed,
		}
		if auditFlag.jsonOut {
			return printJSON(out)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "audit %s: %s score=%.4f τ=%.4f（cells=%d flags=%v）\n",
			auditFlag.claimed, res.Verdict, res.Score, res.Threshold, res.CellsUsed, out["flags"])
		return nil
	},
}

// pickCells 用种子从电池 cell 中随机选 k 个（Fisher-Yates 前缀；种子可复现）。
func pickCells(all []string, k int, seed int64) []string {
	if k >= len(all) {
		return append([]string(nil), all...)
	}
	rng := rand.New(rand.NewSource(seed))
	out := append([]string(nil), all...)
	for i := len(out) - 1; i > 0; i-- {
		j := rng.Intn(i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return out[:k]
}

// minInt 返回较小值。
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func init() {
	f := auditFlag
	fl := auditCmd.Flags()
	fl.StringVar(&f.provider, "provider", "", "providers.yaml 中的端点名（与直传参数二选一）")
	fl.StringVar(&f.direct.baseURL, "base-url", "", "端点 base_url（不含 /v1）")
	fl.StringVar(&f.direct.apiKeyEnv, "api-key-env", "", "密钥环境变量名")
	fl.StringVar(&f.direct.protocol, "protocol", "auto", "协议 auto|responses|chat|anthropic")
	fl.StringVar(&f.direct.headers, "headers", "", "附加头 k=v,k=v（禁敏感头）")
	fl.StringVar(&f.claimed, "claimed-model", "", "声称模型（须已 enroll 有参考指纹）")
	fl.IntVar(&f.k, "k", 0, "审计探针 cell 数（默认 8）")
	fl.IntVar(&f.n, "n", 0, "每 cell 采样数（默认 15）")
	fl.Float64Var(&f.tau, "tau", 0, "判定阈值（>0 直传覆盖校准库；0=auto 查库）")
	fl.Int64Var(&f.seed, "seed", 0, "cell 选择种子（0=随机，落盘可复现）")
	fl.IntVar(&f.concurrency, "concurrency", 0, "采集并发")
	fl.IntVar(&f.budgetCalls, "budget-calls", 0, "审计预算（调用次数上限，0=不限；成本护栏）")
	fl.BoolVar(&f.reasoning, "reasoning", false, "目标端点为推理模型（系统 2：max_tokens 加大采 post-reasoning 回答）")
	fl.BoolVar(&f.jsonOut, "json", false, "stdout 输出 JSON")
	_ = auditCmd.MarkFlagRequired("claimed-model")
}
