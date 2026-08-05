// Command onetoken 是 LLM 端点真伪检测 CLI。
//
// 基于单 token 行为指纹（arXiv:2607.10252），对"某提供商的某模型端点"
// 做黑盒真伪检测。详见 docs/ 下的工程设计方案与实施计划。
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"onetoken/internal/config"
)

const version = "0.1.0-dev"

func main() {
	fmt.Println("onetoken", version)

	// 启动时加载配置并打印绑定校验告警（密钥缺失/疑似误配），不阻断。
	// config/providers.yaml 为本地真实配置（.gitignore 忽略）；仓库默认只有 .example 模板。
	path := filepath.Join("config", "providers.yaml")
	if _, err := os.Stat(path); err != nil {
		return // 无本地配置：静默跳过（示例模式）
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "onetoken: 配置加载失败: %v\n", err)
		os.Exit(1)
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintf(os.Stderr, "onetoken: 警告: %s\n", w)
	}
}
