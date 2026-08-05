package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testProvidersPath 定位仓库 config/providers.yaml.example。
func testProvidersPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "config", "providers.yaml.example"))
	if err != nil {
		t.Fatalf("定位 providers.yaml.example: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("文件不存在: %s", p)
	}
	return p
}

func TestLoadExample(t *testing.T) {
	// 清空可能存在于真实环境中的密钥变量，保证测试确定性
	for _, env := range []string{"OPENROUTER_API_KEY", "ANTHROPIC_API_KEY", "LOCAL_API_KEY"} {
		t.Setenv(env, "")
	}
	cfg, err := Load(testProvidersPath(t))
	if err != nil {
		t.Fatalf("加载示例失败: %v", err)
	}
	if len(cfg.Providers) != 3 {
		t.Fatalf("provider 数 = %d，期望 3", len(cfg.Providers))
	}
	// base_url 校验：不含 /v1 路径段
	for _, p := range cfg.Providers {
		if strings.Contains(p.BaseURL, "/v1") {
			t.Fatalf("base_url 含 /v1: %s", p.BaseURL)
		}
		if p.APIKey() != "" {
			t.Fatalf("示例配置不应注入密钥: %s", p.Name)
		}
	}
	// 绑定校验已接入 Load：三个 provider 密钥均缺失 → Warnings 有告警
	if len(cfg.Warnings) < 3 {
		t.Fatalf("Warnings 应含至少 3 条密钥缺失告警，实际 %d: %v", len(cfg.Warnings), cfg.Warnings)
	}
	// 默认设置断言（采样参数默认值）
	s := cfg.Settings
	if s.EnrollNT1 != 30 || s.EnrollNT0 != 3 || s.FrontierNT1 != 15 {
		t.Fatalf("enroll 采样默认错误: %+v", s)
	}
	if s.AuditK != 8 || s.AuditN != 15 {
		t.Fatalf("audit 采样默认错误: %+v", s)
	}
	if s.OutputTokenCap != 16 {
		t.Fatalf("output cap 默认错误: %d", s.OutputTokenCap)
	}
	if s.DriftBaseline != 0.140 {
		t.Fatalf("漂移底线默认错误: %v", s.DriftBaseline)
	}
	if s.MinValidSamples != 10 {
		t.Fatalf("有效样本门槛默认错误: %d", s.MinValidSamples)
	}
	if s.T0ProbeN < 5 {
		t.Fatalf("T0 探针样本数应 ≥5: %d", s.T0ProbeN)
	}
}

// TestSecretNeverSerialized：密钥永不进入日志/报告/序列化（含 %#v）。
func TestSecretNeverSerialized(t *testing.T) {
	const secret = "sk-super-secret-value-123456"
	p := ProviderConfig{
		Name:      "test",
		BaseURL:   "https://api.example.com",
		APIKeyEnv: "TEST_API_KEY",
		Protocol:  "chat",
		apiKey:    secret,
	}
	// 1) String() 脱敏
	if s := p.String(); strings.Contains(s, secret) {
		t.Fatalf("String() 泄露密钥: %s", s)
	}
	// 2) fmt %+v 走 Stringer
	if s := fmt.Sprintf("%+v", p); strings.Contains(s, secret) {
		t.Fatalf("fmt 泄露密钥: %s", s)
	}
	// 3) %#v（Go 语法表示）经 GoString 拦截
	if s := fmt.Sprintf("%#v", p); strings.Contains(s, secret) {
		t.Fatalf("%%#v 泄露密钥: %s", s)
	}
	// 4) JSON 序列化不含密钥（apiKey 小写不导出）
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatalf("JSON 泄露密钥: %s", string(b))
	}
	// 5) redact 边界：空/短密钥不泄露明文
	if r := redact(""); r != "<empty>" {
		t.Fatalf("redact 空密钥: %s", r)
	}
	if r := redact("ab"); !strings.Contains(r, "redacted") {
		t.Fatalf("redact 短密钥应脱敏: %s", r)
	}
}

func TestValidateProviderRejectsV1(t *testing.T) {
	p := ProviderConfig{Name: "x", BaseURL: "https://api.example.com/v1", APIKeyEnv: "X", Protocol: "chat"}
	if err := validateProvider(&p); err == nil {
		t.Fatal("base_url 含 /v1 路径段应报错")
	}
}

// /v10、/v1beta 等非 /v1 路径段不应被误伤（只查路径段，不查主机名/query）。
func TestValidateProviderAcceptsV10Path(t *testing.T) {
	p := ProviderConfig{Name: "x", BaseURL: "https://api.example.com/v10", APIKeyEnv: "X", Protocol: "chat"}
	if err := validateProvider(&p); err != nil {
		t.Fatalf("/v10 不应误伤: %v", err)
	}
	p2 := ProviderConfig{Name: "y", BaseURL: "https://v1.example.com", APIKeyEnv: "Y", Protocol: "chat"}
	if err := validateProvider(&p2); err != nil {
		t.Fatalf("主机名 v1.example.com 不应误伤: %v", err)
	}
}

func TestValidateProviderRejectsUserinfo(t *testing.T) {
	p := ProviderConfig{Name: "x", BaseURL: "https://user@attacker.com", APIKeyEnv: "X", Protocol: "chat"}
	if err := validateProvider(&p); err == nil {
		t.Fatal("userinfo 应报错")
	}
}

func TestValidateProviderRejectsQueryFragment(t *testing.T) {
	p := ProviderConfig{Name: "x", BaseURL: "https://api.example.com?next=/v1/x", APIKeyEnv: "X", Protocol: "chat"}
	if err := validateProvider(&p); err == nil {
		t.Fatal("query 应报错")
	}
}

func TestValidateProviderRejectsBadProtocol(t *testing.T) {
	p := ProviderConfig{Name: "x", BaseURL: "https://api.example.com", APIKeyEnv: "X", Protocol: "grpc"}
	if err := validateProvider(&p); err == nil {
		t.Fatal("非法 protocol 应报错")
	}
}

func TestValidateProviderRejectsSensitiveHeader(t *testing.T) {
	p := ProviderConfig{Name: "x", BaseURL: "https://api.example.com", APIKeyEnv: "X", Protocol: "chat",
		Headers: map[string]string{"Authorization": "Bearer sk-inline"}}
	if err := validateProvider(&p); err == nil {
		t.Fatal("敏感头应报错（防密钥明文落配置）")
	}
}

func TestValidateProviderFillsLimitDefaults(t *testing.T) {
	p := ProviderConfig{Name: "x", BaseURL: "https://api.example.com", APIKeyEnv: "X", Protocol: "chat"}
	if err := validateProvider(&p); err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if p.Limits.MaxConcurrency != 8 || p.Limits.TimeoutSec != 60 {
		t.Fatalf("limits 默认值错误: %+v", p.Limits)
	}
}

// TestYAMLUnknownFieldRejected：严格解析，未知字段（如直觉写法的 api_key）显式报错。
func TestYAMLUnknownFieldRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.yaml")
	content := "providers:\n  - name: x\n    base_url: https://api.example.com\n    api_key_env: X\n    protocol: chat\n    api_key: sk-inline-leak\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("未知字段 api_key 应报错（严格解析）")
	}
}

func TestBindCheckWarnsOnMismatch(t *testing.T) {
	// openrouter 密钥配到 anthropic 域名 → 告警
	p := ProviderConfig{
		Name: "mismatch", BaseURL: "https://api.anthropic.com",
		APIKeyEnv: "OPENROUTER_API_KEY", Protocol: "anthropic",
		apiKey: "sk-ant-x",
	}
	if w := BindCheck(p); len(w) == 0 {
		t.Fatal("厂商不匹配应产生告警")
	}
	// 同厂商不告警（防误报）
	ok := ProviderConfig{
		Name: "same", BaseURL: "https://api.anthropic.com",
		APIKeyEnv: "ANTHROPIC_API_KEY", Protocol: "anthropic",
		apiKey: "sk-ant-x",
	}
	if w := BindCheck(ok); len(w) != 0 {
		t.Fatalf("同厂商不应告警: %v", w)
	}
	// 密钥缺失 → 告警
	empty := ProviderConfig{
		Name: "no-key", BaseURL: "https://api.openai.com",
		APIKeyEnv: "MISSING_KEY", Protocol: "auto",
	}
	if w := BindCheck(empty); len(w) == 0 {
		t.Fatal("密钥缺失应告警")
	}
}

// 本地通道变体：127/8、::1、.local 均视为本地，不作厂商匹配。
func TestBindCheckLocalVariants(t *testing.T) {
	for _, u := range []string{
		"http://localhost:8000", "http://127.0.0.2:8000",
		"http://[::1]:8000", "http://box.local:8000",
	} {
		p := ProviderConfig{Name: "local", BaseURL: u, APIKeyEnv: "ANYTHING", Protocol: "chat", apiKey: "k"}
		if w := BindCheck(p); len(w) != 0 {
			t.Fatalf("%s 应视为本地通道: %v", u, w)
		}
	}
	// 域名混淆：localhost.evil.com 不是本地通道 → 应有告警
	p := ProviderConfig{Name: "x", BaseURL: "https://localhost.evil.com", APIKeyEnv: "OPENAI_API_KEY", Protocol: "auto", apiKey: "sk-x"}
	if w := BindCheck(p); len(w) == 0 {
		t.Fatal("localhost.evil.com 不应被视为本地通道")
	}
}

// 非知名厂商域 + 密钥 → 宽松告警（防"攻击者域名 + 真实厂商密钥环境变量名"组合绕过）。
func TestBindCheckUnknownDomainWarns(t *testing.T) {
	p := ProviderConfig{Name: "x", BaseURL: "https://evil.example", APIKeyEnv: "OPENAI_API_KEY", Protocol: "auto", apiKey: "sk-x"}
	if w := BindCheck(p); len(w) == 0 {
		t.Fatal("非知名域 + 密钥应给出宽松告警")
	}
}

