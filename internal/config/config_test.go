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
	// 示例模板无密钥环境变量，Load 应成功并给出告警（宽松模式）
	cfg, err := Load(testProvidersPath(t))
	if err != nil {
		t.Fatalf("加载示例失败: %v", err)
	}
	if len(cfg.Providers) != 3 {
		t.Fatalf("provider 数 = %d，期望 3", len(cfg.Providers))
	}
	// base_url 校验：不含 /v1
	for _, p := range cfg.Providers {
		if strings.Contains(p.BaseURL, "/v1") {
			t.Fatalf("base_url 含 /v1: %s", p.BaseURL)
		}
		if p.APIKey() != "" {
			t.Fatalf("示例配置不应注入密钥: %s", p.Name)
		}
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

// TestSecretNeverSerialized：密钥永不进入日志/报告/序列化。
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
	// 3) JSON 序列化不含密钥（apiKey 小写不导出）
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatalf("JSON 泄露密钥: %s", string(b))
	}
	// 4) redact 本身
	if r := redact(secret); strings.Contains(r, "secret") {
		t.Fatalf("redact 泄露密钥: %s", r)
	}
}

func TestValidateProviderRejectsV1(t *testing.T) {
	p := ProviderConfig{Name: "x", BaseURL: "https://api.example.com/v1", APIKeyEnv: "X", Protocol: "chat"}
	if err := validateProvider(&p); err == nil {
		t.Fatal("base_url 含 /v1 应报错")
	}
}

func TestValidateProviderRejectsBadProtocol(t *testing.T) {
	p := ProviderConfig{Name: "x", BaseURL: "https://api.example.com", APIKeyEnv: "X", Protocol: "grpc"}
	if err := validateProvider(&p); err == nil {
		t.Fatal("非法 protocol 应报错")
	}
}

func TestBindCheckWarnsOnMismatch(t *testing.T) {
	// openrouter 密钥配到 anthropic 域名 → 告警
	p := ProviderConfig{
		Name: "mismatch", BaseURL: "https://api.anthropic.com",
		APIKeyEnv: "OPENROUTER_API_KEY", Protocol: "anthropic",
		apiKey: "sk-ant-x",
	}
	warns := BindCheck(p)
	if len(warns) == 0 {
		t.Fatal("厂商不匹配应产生告警")
	}
	// localhost 本地通道：任意密钥命名不告警（除缺失外）
	loc := ProviderConfig{
		Name: "local", BaseURL: "http://localhost:8000",
		APIKeyEnv: "ANYTHING", Protocol: "chat", apiKey: "local-key",
	}
	if w := BindCheck(loc); len(w) != 0 {
		t.Fatalf("本地通道不应告警: %v", w)
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

func TestApplyEnvOverridesDBPath(t *testing.T) {
	t.Setenv("ONETOKEN_DB", "/tmp/test-onetoken.db")
	s := DefaultSettings()
	s.ApplyEnv()
	if s.DBPath != "/tmp/test-onetoken.db" {
		t.Fatalf("ONETOKEN_DB 覆盖失败: %s", s.DBPath)
	}
}
