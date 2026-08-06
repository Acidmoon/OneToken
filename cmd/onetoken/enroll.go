package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"onetoken/internal/enroll"
)

// enrollFlags 是 enroll 命令参数。
type enrollFlags struct {
	provider    string
	direct      directFlags
	model       string
	version     string
	vendor      string
	family      string
	modelType   string
	frontier    bool
	concurrency int
	budgetCalls int
	jsonOut     bool
}

var enrollFlag = &enrollFlags{}

var enrollCmd = &cobra.Command{
	Use:   "enroll",
	Short: "为模型建立参考指纹（云端 API 参考通道，设计 §7）",
	Long: `enroll 采集 40-cell 探针电池（T=1.0 指纹采样 + T=0 变体）并构建参考指纹，
版本化入库（UNIQUE(model_id, version)）。

端点选择：--provider <providers.yaml 中的名字>，或直传
--base-url <url> --api-key-env <ENV> [--protocol auto|responses|chat|anthropic]。

密钥一律走环境变量（--api-key-env 引用的环境变量），永不落盘/落日志。`,
	Example: `  onetoken enroll --provider zhipu --model zhipu/glm-4.5 --version 2026-08-06v1
  onetoken enroll --base-url https://dashscope.aliyuncs.com --api-key-env DASHSCOPE_API_KEY \
    --protocol auto --model qwen/qwen3-8b --version v1 --vendor dashscope --family qwen

  # 参考端点由用户自定（示例用厂商官方 API，可另选信任端点，工具不作规定）`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		p, srcName, err := resolveProvider(cfg, enrollFlag.provider, enrollFlag.direct)
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
		progress := progressToStderr("enroll T=1.0")
		progress0 := progressToStderr("enroll T=0")
		fp, err := enroll.Enroll(runCtx(), enroll.Options{
			Budget:       newBudget(enrollFlag.budgetCalls),
			Settings:     cfg.Settings,
			Provider:     client,
			Store:        st,
			Battery:      b,
			ModelID:      enrollFlag.model,
			Vendor:       enrollFlag.vendor,
			Family:       enrollFlag.family,
			ModelType:    enrollFlag.modelType,
			Version:      enrollFlag.version,
			ProviderName: srcName,
			Frontier:     enrollFlag.frontier,
			Concurrency:  enrollFlag.concurrency,
			OnProgress: func(phase string, done, total int) {
				if phase == "t0" {
					progress0(done, total)
				} else {
					progress(done, total)
				}
			},
		})
		if err != nil {
			return err
		}
		if enrollFlag.jsonOut {
			return printJSON(map[string]any{
				"model_id":      fp.ModelID,
				"version":       fp.Version,
				"ref_source":    fp.RefSource,
				"provider":      fp.Provider,
				"cells":         len(fp.Cells),
				"t0_cells":      len(fp.T0Cells),
				"collected_at":  fp.CollectedAt,
				"superseded_by": fp.SupersededBy,
			})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "已建档 %s @%s（%d cell，%s）\n", fp.ModelID, fp.Version, len(fp.Cells), fp.Provider)
		return nil
	},
}

func init() {
	f := enrollFlag
	fl := enrollCmd.Flags()
	fl.StringVar(&f.provider, "provider", "", "providers.yaml 中的端点名（与直传参数二选一）")
	fl.StringVar(&f.direct.baseURL, "base-url", "", "端点 base_url（不含 /v1，设计 §6.1）")
	fl.StringVar(&f.direct.apiKeyEnv, "api-key-env", "", "密钥环境变量名（密钥只走环境变量）")
	fl.StringVar(&f.direct.protocol, "protocol", "auto", "协议 auto|responses|chat|anthropic")
	fl.StringVar(&f.direct.headers, "headers", "", "附加头 k=v,k=v（禁敏感头）")
	fl.StringVar(&f.model, "model", "", "模型标识（如 qwen/qwen3-8b）")
	fl.StringVar(&f.version, "version", "", "指纹版本（如 2026-08-06v1；同 model+version 冲突）")
	fl.StringVar(&f.vendor, "vendor", "", "厂商（如 zhipu）")
	fl.StringVar(&f.family, "family", "", "模型家族（如 qwen）")
	fl.StringVar(&f.modelType, "model-type", "open-source", "open-source|proprietary")
	fl.BoolVar(&f.frontier, "frontier", false, "前沿定价模型（≥$5/1M input，采样减半）")
	fl.IntVar(&f.concurrency, "concurrency", 0, "采集并发（默认 8，上限 256）")
	fl.IntVar(&f.budgetCalls, "budget-calls", 0, "建档预算（调用次数上限，0=不限；成本护栏）")
	fl.BoolVar(&f.jsonOut, "json", false, "stdout 输出 JSON")
	_ = enrollCmd.MarkFlagRequired("model")
	_ = enrollCmd.MarkFlagRequired("version")
}
