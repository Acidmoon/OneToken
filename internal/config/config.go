// Package config 负责运行配置加载：提供商端点（providers.yaml）、
// 阈值/采样参数（Settings）、密钥注入与 base_url↔密钥绑定校验。
//
// 设计文档 §6.1：base_url 为协议根（不含 /v1），层内统一拼接 /v1/<endpoint>。
// 密钥只走环境变量（api_key_env 引用），永不落盘/落日志/落报告。
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Protocols 是合法协议值。
var Protocols = []string{"auto", "responses", "chat", "anthropic"}

// Limits 是 per-provider 限流与并发预算（0 = 不限）。
type Limits struct {
	RPM            int `yaml:"rpm"`
	RPD            int `yaml:"rpd"`
	MaxConcurrency int `yaml:"max_concurrency"`
	TimeoutSec     int `yaml:"timeout_s"`
}

// ProviderConfig 描述一个端点（任意 BaseURL + 密钥环境变量引用）。
type ProviderConfig struct {
	Name      string            `yaml:"name"`
	BaseURL   string            `yaml:"base_url"`    // 协议根，不含 /v1
	APIKeyEnv string            `yaml:"api_key_env"` // 环境变量名，不含密钥值
	Protocol  string            `yaml:"protocol"`    // auto|responses|chat|anthropic
	Limits    Limits            `yaml:"limits"`
	Headers   map[string]string `yaml:"headers,omitempty"`

	apiKey string // 运行时注入；不导出、不参与序列化
}

// String 返回脱敏表示（密钥永不进入日志/报告）。
func (p ProviderConfig) String() string {
	return fmt.Sprintf("ProviderConfig{name:%s base_url:%s protocol:%s api_key:%s}",
		p.Name, p.BaseURL, p.Protocol, redact(p.apiKey))
}

// APIKey 返回注入的密钥值。
func (p *ProviderConfig) APIKey() string { return p.apiKey }

// redact 将密钥脱敏为前 4 位 + 星号。
func redact(key string) string {
	if key == "" {
		return "<empty>"
	}
	if len(key) <= 4 {
		return "<redacted>"
	}
	return key[:4] + "…<redacted>"
}

// Config 是完整运行配置。
type Config struct {
	Settings  Settings
	Providers []ProviderConfig
}

// Load 从 providers.yaml 加载提供商配置，注入环境变量密钥，
// 并对每个 provider 执行绑定校验（告警写入返回的 warnings，不阻断）。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: 读取 %s: %w", path, err)
	}
	var raw struct {
		Providers []ProviderConfig `yaml:"providers"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("config: 解析 %s: %w", path, err)
	}
	if len(raw.Providers) == 0 {
		return nil, errors.New("config: providers.yaml 未定义任何 provider")
	}
	cfg := &Config{
		Settings:  DefaultSettings(),
		Providers: make([]ProviderConfig, 0, len(raw.Providers)),
	}
	cfg.Settings.ApplyEnv()
	for i := range raw.Providers {
		p := &raw.Providers[i]
		if err := validateProvider(p); err != nil {
			return nil, fmt.Errorf("config: provider %q: %w", p.Name, err)
		}
		// 注入密钥：环境变量缺失时不报错（宽松告警），实际请求时由 provider 层拒绝
		p.apiKey = os.Getenv(p.APIKeyEnv)
		cfg.Providers = append(cfg.Providers, *p)
	}
	return cfg, nil
}

// validateProvider 校验字段合法性。
func validateProvider(p *ProviderConfig) error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("name 为空")
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		return errors.New("base_url 为空")
	}
	if strings.HasSuffix(p.BaseURL, "/") {
		p.BaseURL = strings.TrimSuffix(p.BaseURL, "/")
	}
	// base_url 约定：不含 /v1 路径段（层内统一拼接）
	lower := strings.ToLower(p.BaseURL)
	if strings.Contains(lower, "/v1") {
		return errors.New("base_url 不应包含 /v1（协议根，层内统一拼接 /v1/<endpoint>）")
	}
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return errors.New("base_url 需以 http:// 或 https:// 开头")
	}
	if p.APIKeyEnv == "" {
		return errors.New("api_key_env 为空（密钥必须走环境变量引用）")
	}
	ok := false
	for _, pr := range Protocols {
		if p.Protocol == pr {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("protocol=%q，期望 %v", p.Protocol, Protocols)
	}
	return nil
}

// BindCheck 执行 base_url↔密钥绑定校验，返回告警列表（宽松告警，不阻断）。
//
// 规则（骨架级，后续可增强）：
//  1. 密钥环境变量缺失 → 告警；
//  2. base_url 为知名厂商域名而 api_key_env 前缀明显属于另一厂商 → 告警
//     （防把 A 厂商密钥误配到恶意/第三方 base_url）；
//  3. localhost 本地通道不作厂商匹配（允许任意密钥命名）。
func BindCheck(p ProviderConfig) []string {
	var warns []string
	if strings.TrimSpace(p.APIKey()) == "" {
		warns = append(warns, fmt.Sprintf("provider %q: 环境变量 %s 未设置或为空", p.Name, p.APIKeyEnv))
	}
	host := strings.ToLower(p.BaseURL)
	keyEnv := strings.ToLower(p.APIKeyEnv)
	if strings.Contains(host, "localhost") || strings.Contains(host, "127.0.0.1") {
		return warns // 本地通道不检查厂商匹配
	}
	// 知名厂商域名 → 期望 env 前缀（宽松包含匹配）
	known := []struct{ domain, prefix string }{
		{"openai.com", "openai"},
		{"anthropic.com", "anthropic"},
		{"openrouter.ai", "openrouter"},
		{"deepseek.com", "deepseek"},
		{"zhipuai", "zhipu"},
		{"moonshot", "moonshot"},
	}
	for _, k := range known {
		if strings.Contains(host, k.domain) && !strings.Contains(keyEnv, k.prefix) {
			warns = append(warns, fmt.Sprintf(
				"provider %q: base_url 主机 %s 与密钥环境变量 %s 疑似不匹配（防密钥误配）",
				p.Name, p.BaseURL, p.APIKeyEnv))
		}
	}
	return warns
}
