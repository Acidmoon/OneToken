// Package config 负责运行配置加载：提供商端点（providers.yaml）、
// 阈值/采样参数（Settings）、密钥注入与 base_url↔密钥绑定校验。
//
// 设计文档 §6.1：base_url 为协议根（不含 /v1），层内统一拼接 /v1/<endpoint>。
// 密钥只走环境变量（api_key_env 引用），永不落盘/落日志/落报告。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
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

// GoString 拦截 %#v（Go 语法表示）：fmt 对 %#v 优先调用 GoStringer，
// 防止反射打印未导出的 apiKey 字段。
func (p ProviderConfig) GoString() string { return p.String() }

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
// Warnings 保存启动时绑定校验告警（密钥缺失/疑似误配），由 CLI 打印到 stderr。
type Config struct {
	Settings  Settings
	Providers []ProviderConfig
	Warnings  []string
}

// Load 从 providers.yaml 加载提供商配置，注入环境变量密钥，
// 并对每个 provider 执行绑定校验（告警写入 cfg.Warnings，不阻断，
// 由调用方打印到 stderr）。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: 读取 %s: %w", path, err)
	}
	var raw struct {
		Providers []ProviderConfig `yaml:"providers"`
	}
	// 严格解析：未知字段（如直觉写法的 api_key、拼写错误的 limits 字段）显式报错，
	// 防静默吞掉限流/成本护栏配置。
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
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
		cfg.Warnings = append(cfg.Warnings, BindCheck(*p)...)
		cfg.Providers = append(cfg.Providers, *p)
	}
	return cfg, nil
}

// validateProvider 校验字段合法性。
func validateProvider(p *ProviderConfig) error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("name 为空")
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil {
		return fmt.Errorf("base_url 无法解析: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("base_url 需以 http:// 或 https:// 开头")
	}
	if u.User != nil {
		return errors.New("base_url 不得包含 userinfo（https://user@host 形式，主机将被解析为别的主机）")
	}
	if u.Fragment != "" || u.RawQuery != "" {
		return errors.New("base_url 不得包含 query 或 fragment（应为协议根）")
	}
	if u.Host == "" {
		return errors.New("base_url 缺少主机名")
	}
	// base_url 约定：协议根，路径段不得为 /v1（层内统一拼接，防双 /v1）。
	// 只检查路径段（HasPrefix "/v1/" 或等于 "/v1"），不查主机名/query，避免误伤。
	if u.Path == "/v1" || strings.HasPrefix(u.Path, "/v1/") {
		return errors.New("base_url 不应包含 /v1 路径段（协议根，层内统一拼接 /v1/<endpoint>）")
	}
	p.BaseURL = strings.TrimRight(p.BaseURL, "/")
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
	// Headers 禁止敏感头名：防止绕过 api_key_env 约定，密钥明文落入配置文件
	for h := range p.Headers {
		lower := strings.ToLower(h)
		if lower == "authorization" || lower == "proxy-authorization" ||
			lower == "x-api-key" || lower == "cookie" {
			return fmt.Errorf("headers 禁止含敏感头 %q（密钥必须走 api_key_env 环境变量）", h)
		}
	}
	// Limits 默认值：未配置时套 8 并发 / 60s 超时（rpm/rpd 0=不限 保留语义）
	if p.Limits.MaxConcurrency == 0 {
		p.Limits.MaxConcurrency = 8
	}
	if p.Limits.TimeoutSec == 0 {
		p.Limits.TimeoutSec = 60
	}
	return nil
}

// BindCheck 执行 base_url↔密钥绑定校验，返回告警列表（宽松告警，不阻断）。
//
// 规则（骨架级，M2.2 传输层可升级为 fail-closed）：
//  1. 密钥环境变量缺失 → 告警；
//  2. base_url 为知名厂商域（精确主机或 .域名 子域）而 api_key_env 前缀明显
//     属于另一厂商 → 告警（防把 A 厂商密钥误配到恶意/第三方 base_url）；
//  3. 非本地通道且非任何知名厂商域 → 对任意非空密钥给出宽松告警（提示确认
//     端点可信，防"攻击者域名 + 真实厂商密钥环境变量名"的组合绕过）；
//  4. 本地通道（localhost / 127/8 / ::1 / .local）不作厂商匹配。
func BindCheck(p ProviderConfig) []string {
	var warns []string
	if strings.TrimSpace(p.APIKey()) == "" {
		warns = append(warns, fmt.Sprintf("provider %q: 环境变量 %s 未设置或为空", p.Name, p.APIKeyEnv))
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil {
		warns = append(warns, fmt.Sprintf("provider %q: base_url 无法解析: %v", p.Name, err))
		return warns
	}
	host := strings.ToLower(u.Hostname())
	if isLocalHost(host) {
		return warns // 本地通道不检查厂商匹配
	}
	keyEnv := strings.ToLower(p.APIKeyEnv)
	matchedKnown := false
	// 知名厂商域名 → 期望 env 前缀。精确主机或子域（.domain 结尾），拒绝裸子串，防域名混淆。
	known := []struct{ domain, prefix string }{
		{"openai.com", "openai"},
		{"anthropic.com", "anthropic"},
		{"openrouter.ai", "openrouter"},
		{"deepseek.com", "deepseek"},
		{"zhipuai.com", "zhipu"},
		{"moonshot.cn", "moonshot"},
	}
	for _, k := range known {
		if host == k.domain || strings.HasSuffix(host, "."+k.domain) {
			matchedKnown = true
			if !strings.Contains(keyEnv, k.prefix) {
				warns = append(warns, fmt.Sprintf(
					"provider %q: base_url 主机 %s 与密钥环境变量 %s 疑似不匹配（防密钥误配）",
					p.Name, p.BaseURL, p.APIKeyEnv))
			}
			break
		}
	}
	// 非知名厂商域：密钥存在时给出宽松告警（提示确认端点可信）
	if !matchedKnown && strings.TrimSpace(p.APIKey()) != "" {
		warns = append(warns, fmt.Sprintf(
			"provider %q: base_url %s 非已知厂商域，请确认端点可信（密钥 %s 将发往该主机）",
			p.Name, p.BaseURL, p.APIKeyEnv))
	}
	return warns
}

// isLocalHost 判定主机是否为本地通道（localhost / 127/8 / ::1 / .local）。
func isLocalHost(host string) bool {
	if host == "localhost" || host == "::1" || strings.HasSuffix(host, ".local") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
