// Command onetoken 是 LLM 端点真伪检测 CLI。
//
// 基于单 token 行为指纹（arXiv:2607.10252），对"某提供商的某模型端点"
// 做黑盒真伪检测。详见 docs/ 下的工程设计方案与实施计划。
package main

import "fmt"

const version = "0.1.0-dev"

func main() {
	fmt.Println("onetoken", version)
}
