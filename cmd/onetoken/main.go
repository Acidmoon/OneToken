// Command onetoken 是 LLM 端点真伪检测 CLI。
//
// 基于单 token 行为指纹（arXiv:2607.10252），对"某提供商的某模型端点"
// 做黑盒真伪检测。详见 docs/ 下的工程设计方案与实施计划。
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"onetoken/internal/config"
)

const version = "0.1.0-dev"

var (
	// 全局（根命令持久化）flags
	cfgPath string

	rootCmd = &cobra.Command{
		Use:   "onetoken",
		Short: "LLM 端点真伪检测（单 token 行为指纹）",
		Long: `onetoken 基于单 token 行为指纹（arXiv:2607.10252）对"某提供商的某模型端点"
做黑盒真伪检测：模型替换 / 量化顶替 / 版本回退 / 跨 provider 漂移。

子命令：enroll（建档参考指纹）/ probe（测量有效性预检）/ audit（审计判定）。
密钥一律走环境变量（--api-key-env 或 providers.yaml 的 api_key_env），永不落盘。`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
)

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgPath, "config", "config/providers.yaml", "providers 配置路径（缺省 config/providers.yaml）")
	rootCmd.AddCommand(enrollCmd, probeCmd, auditCmd, compareCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "onetoken: %v\n", err)
		os.Exit(1)
	}
}

// loadConfig 加载 providers 配置并打印绑定校验告警（密钥缺失/疑似误配，不阻断）。
// 配置文件不存在时返回空配置（示例模式：仅直传参数可用）。
func loadConfig() (*config.Config, error) {
	if _, err := os.Stat(cfgPath); err != nil {
		if os.IsNotExist(err) {
			return &config.Config{Settings: config.DefaultSettings()}, nil // 示例模式：仅直传参数可用
		}
		return nil, fmt.Errorf("读取配置 %s: %w", cfgPath, err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintf(os.Stderr, "onetoken: 警告: %s\n", w)
	}
	return cfg, nil
}
