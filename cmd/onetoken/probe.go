package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"onetoken/internal/battery"
	"onetoken/internal/collector"
	"onetoken/internal/detector"
	"onetoken/internal/preprocess"
	"onetoken/internal/provider"
	"onetoken/internal/store"
)

// probeFlags 是 probe 命令参数。
type probeFlags struct {
	provider    string
	direct      directFlags
	model       string
	concurrency int
	budgetCalls int
	jsonOut     bool
}

var probeFlag = &probeFlags{}

var probeCmd = &cobra.Command{
	Use:   "probe",
	Short: "测量有效性预检（设计 §5：T=0 一致性 / 推理痕迹 / 缓存签名 / 有效率）",
	Long: `probe 对端点做测量有效性预检：3 个探针 cell × T=0 采样（T0ProbeN≥5）
+ T=1.0 样本（≥CacheMinN），输出探测器 flag（hidden-reasoning / temperature-not-honored /
response-caching / unreachable；safety-layer-change 需参考 refusal 基线，审计侧判定）
与有效率，不建指纹不判定。

端点选择与 enroll 相同（--provider 或直传参数）。`,
	Example: `  onetoken probe --provider openrouter --model openai/gpt-5.1`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		p, _, err := resolveProvider(cfg, probeFlag.provider, probeFlag.direct)
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

		cells := b.Cells()[:3]
		// 每次 probe 用新鲜 id（时间戳）：防幂等续采 skip 导致重复预检返回空结果（审查 R-H2）
		id := "probe-" + store.SanitizeID(probeFlag.model) + "-" + time.Now().UTC().Format("20060102T150405.000Z")
		conc := resolveConcurrency(probeFlag.concurrency, p)
		cc := collector.Options{
			ID: id + "-t0", Model: probeFlag.model, Concurrency: conc,
			MaxTokens: s.OutputTokenCap, OnProgress: progressToStderr("probe T=0"),
			Budget: newBudget(probeFlag.budgetCalls),
		}
		// T=0 探针：每 cell T0ProbeN 样本（设计 §5：n≥5 二项口径）
		t0, err := collector.RunBattery(runCtx(), client, st, b, cells, s.T0ProbeN, 0.0, cc)
		if isAbort(err) {
			return err
		}
		failed0 := collector.CountTaskFailures(err)
		// T=1.0 样本：每 cell CacheMinN（≥10）——方差/缓存签名信号（审查 R-H1：
		// 3 样本 < CacheMinN 使 response-caching 结构性不可触发）
		cc.ID = id + "-t1"
		cc.OnProgress = progressToStderr("probe T=1.0")
		n1 := s.CacheMinN
		if n1 < 3 {
			n1 = 3
		}
		rs, err1 := collector.RunBattery(runCtx(), client, st, b, cells, n1, 1.0, cc)
		if isAbort(err1) {
			return err1
		}
		failed1 := collector.CountTaskFailures(err1)

		// 段级失败率（与 enroll 一致，审查 R-M1）：T0 段全败不被 T1 成功稀释
		ratio1, ratio0 := failRatio(failed1, len(rs)), failRatio(failed0, len(t0))
		scr := detector.Screen(rs, detector.ScreenOptions{
			Settings:    s,
			T0Responses: t0,
			TaskForCell: taskForCellOf(b),
		})
		if ratio1 >= s.UnreachableFailRatio || ratio0 >= s.UnreachableFailRatio {
			scr.Flags.Unreachable = true
		}
		out := map[string]any{
			"flags":             flagsOrEmpty(scr.Flags),
			"valid_rate":        scr.ValidRate,
			"valid_cells":       scr.ValidCells,
			"t0_judged":         scr.T0Judged,
			"t0_det_ratio":      scr.T0DetRatio,
			"t0_not_judged":     scr.T0NotJudged,
			"unknown_responses": scr.UnknownResponses,
		}
		if probeFlag.jsonOut {
			return printJSON(out)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "probe %s: flags=%v valid_rate=%.2f\n", probeFlag.model, out["flags"], scr.ValidRate)
		return nil
	},
}

// flagsOrEmpty 返回 flag 列表（空时给空数组，避免 JSON null）。
func flagsOrEmpty(f detector.Flags) []string {
	if l := f.List(); l != nil {
		return l
	}
	return []string{}
}

// isAbort 判断中止类错误（预算/ctx 取消；probe 与 audit 共用）。
// 与 enroll 包一致：errors.Is 对 errors.Join 递归有效。
func isAbort(err error) bool {
	return errors.Is(err, provider.ErrBudgetExceeded) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// failRatio 计算失败率（分母为 0 时返回 0）。
func failRatio(failed, succeeded int) float64 {
	if failed+succeeded == 0 {
		return 0
	}
	return float64(failed) / float64(failed+succeeded)
}

// taskForCellOf 从 battery 构造 TaskForCell（cell "task:lang" → preprocess.Task；
// probe/audit 共用，避免逐命令重复实现）。
func taskForCellOf(b *battery.Battery) func(string) (preprocess.Task, bool) {
	byID := make(map[string]preprocess.Task, len(b.Tasks))
	for _, t := range b.Tasks {
		byID[t.ID] = preprocess.Task{ID: t.ID, AnswerSpace: t.AnswerSpace, SpaceSize: t.SpaceSize}
	}
	return func(cell string) (preprocess.Task, bool) {
		taskID, _, ok := splitCell(cell)
		if !ok {
			return preprocess.Task{}, false
		}
		task, ok := byID[taskID]
		return task, ok
	}
}

// splitCell 解析 cell "task:lang"。
func splitCell(cell string) (taskID, lang string, ok bool) {
	for i := 0; i < len(cell); i++ {
		if cell[i] == ':' {
			return cell[:i], cell[i+1:], true
		}
	}
	return "", "", false
}

func init() {
	f := probeFlag
	fl := probeCmd.Flags()
	fl.StringVar(&f.provider, "provider", "", "providers.yaml 中的端点名（与直传参数二选一）")
	fl.StringVar(&f.direct.baseURL, "base-url", "", "端点 base_url（不含 /v1）")
	fl.StringVar(&f.direct.apiKeyEnv, "api-key-env", "", "密钥环境变量名")
	fl.StringVar(&f.direct.protocol, "protocol", "auto", "协议 auto|responses|chat|anthropic")
	fl.StringVar(&f.direct.headers, "headers", "", "附加头 k=v,k=v（禁敏感头）")
	fl.StringVar(&f.model, "model", "", "模型标识")
	fl.IntVar(&f.concurrency, "concurrency", 0, "采集并发")
	fl.IntVar(&f.budgetCalls, "budget-calls", 0, "预检预算（调用次数上限，0=不限）")
	fl.BoolVar(&f.jsonOut, "json", false, "stdout 输出 JSON")
	_ = probeCmd.MarkFlagRequired("model")
}
