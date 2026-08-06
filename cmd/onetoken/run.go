package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"onetoken/internal/battery"
	"onetoken/internal/config"
	"onetoken/internal/provider"
	"onetoken/internal/store"
)

// env 常量：密钥与数据目录（AGENTS.md §4：密钥只走环境变量）。
const (
	envDataDir = "ONETOKEN_DATA"
)

// directFlags 是直传端点参数（--provider 的替代，设计 §8：临时/一次性场景）。
type directFlags struct {
	baseURL   string
	apiKeyEnv string
	protocol  string
	headers   string // k=v,k=v 逗号分隔
}

// resolveProvider 解析端点配置：--provider NAME（providers.yaml）优先，
// 否则用直传参数构造（任意 BaseURL 场景）。返回 ProviderConfig（密钥经
// 环境变量注入）与来源名（provider 名或 base_url）。
func resolveProvider(cfg *config.Config, name string, direct directFlags) (config.ProviderConfig, string, error) {
	if name != "" && (direct.baseURL != "" || direct.apiKeyEnv != "" || direct.protocol != "" || direct.headers != "") {
		return config.ProviderConfig{}, "", errors.New("--provider 与直传参数（--base-url 等）互斥，二选一")
	}
	if name != "" {
		for _, p := range cfg.Providers {
			if p.Name == name {
				return p, p.Name, nil
			}
		}
		return config.ProviderConfig{}, "", fmt.Errorf("providers.yaml 无 provider %q", name)
	}
	if direct.baseURL == "" || direct.apiKeyEnv == "" {
		return config.ProviderConfig{}, "", errors.New("需指定 --provider 或直传 --base-url --api-key-env")
	}
	proto := direct.protocol
	if proto == "" {
		proto = "auto"
	}
	headers := map[string]string{}
	if direct.headers != "" {
		for _, kv := range strings.Split(direct.headers, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
			if !ok || k == "" {
				return config.ProviderConfig{}, "", fmt.Errorf("--headers 格式非法 %q（期望 k=v,k=v）", kv)
			}
			headers[k] = v
		}
	}
	p := config.ProviderConfig{
		Name:      "direct",
		BaseURL:   direct.baseURL,
		APIKeyEnv: direct.apiKeyEnv,
		Protocol:  proto,
		Headers:   headers,
	}
	p.SetAPIKey(os.Getenv(direct.apiKeyEnv))
	// 直传路径复用敏感头黑名单（审查 R-M3）：密钥只走环境变量，敏感头明文
	// 直传一律拒绝（config.validateProvider 仅覆盖 yaml 路径）
	for h := range headers {
		if config.IsSensitiveHeader(h) {
			return config.ProviderConfig{}, "", fmt.Errorf("--headers 禁止含敏感头 %q（密钥必须走 --api-key-env 环境变量）", h)
		}
	}
	// base_url↔密钥绑定校验（审查 M1）：直传路径同样告警（密钥误配外发防护）
	if isLocalhostURL(direct.baseURL) {
		// localhost 豁免（本地 vLLM/Ollama 无密钥）：apiKeyEnv 可空
		if direct.apiKeyEnv != "" {
			p.SetAPIKey(os.Getenv(direct.apiKeyEnv))
		}
		return p, direct.baseURL, nil
	}
	if os.Getenv(direct.apiKeyEnv) == "" {
		return config.ProviderConfig{}, "", fmt.Errorf("环境变量 %s 未设置（密钥只走环境变量）", direct.apiKeyEnv)
	}
	for _, w := range config.BindCheck(p) {
		fmt.Fprintf(os.Stderr, "onetoken: 警告: %s\n", w)
	}
	return p, direct.baseURL, nil
}

// resolveConcurrency 解析采集并发（审查 R-M2）：min(--concurrency||8,
// provider.Limits.MaxConcurrency||8, 256 硬上限)——providers.yaml 的并发配置
// 不再被静默忽略。
func resolveConcurrency(flag int, p config.ProviderConfig) int {
	base := flag
	if base <= 0 {
		base = 8
	}
	if mc := p.Limits.MaxConcurrency; mc > 0 && mc < base {
		base = mc
	}
	if base > 256 {
		base = 256
	}
	return base
}

// isLocalhostURL 判断 base_url 是否本地主机（localhost/127.x/::1/.local；
// 直传路径 apiKeyEnv 豁免用；与 config.BindCheck 的本地豁免语义一致）。
func isLocalhostURL(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if h == "localhost" || h == "::1" || strings.HasSuffix(h, ".local") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// newClient 构造端点客户端（安全传输：SSRF/重定向/限流，M2.2）。
func newClient(p config.ProviderConfig, s config.Settings) (*provider.Client, error) {
	return provider.NewClientWithSettings(p, nil, s)
}

// setupStore 初始化数据目录（ONETOKEN_DATA 或默认），返回 Store。
func setupStore() (*store.Store, error) {
	root := os.Getenv(envDataDir)
	if root == "" {
		root = store.DefaultRoot()
	}
	s := store.New(root)
	if err := os.MkdirAll(s.Root(), 0o755); err != nil {
		return nil, fmt.Errorf("初始化数据目录 %s: %w", s.Root(), err)
	}
	return s, nil
}

// loadBattery 加载 40-cell 探针电池（config/prompts.json，随二进制分发）。
// 路径解析顺序：ONETOKEN_BATTERY 环境变量 → 二进制同目录 config/ → 当前目录
// config/ → 仓库根（开发/测试）。
func loadBattery() (*battery.Battery, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	paths := []string{
		os.Getenv("ONETOKEN_BATTERY"),
		filepath.Join(filepath.Dir(exe), "config", "prompts.json"),
		"config/prompts.json",
		filepath.Join("..", "..", "config", "prompts.json"),
	}
	var lastErr error
	for _, p := range paths {
		if p == "" {
			continue
		}
		b, err := battery.Load(p)
		if err == nil {
			return b, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("加载探针电池失败（config/prompts.json 未找到）: %w", lastErr)
}

// printJSON 输出结构化结果到 stdout（进度走 stderr，结果/进度流分离，设计 §8）。
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// stderr 进度回调（CLI 渲染；进度只走 stderr）。
func progressToStderr(phase string) func(int, int) {
	return func(done, total int) {
		fmt.Fprintf(os.Stderr, "\r%s 采集进度 %d/%d", phase, done, total)
		if done == total {
			fmt.Fprintln(os.Stderr)
		}
	}
}

// randHex 返回 n 字节的 crypto/rand 十六进制串（audit/probe id 随机后缀，
// 防并行毫秒级碰撞，审查 L2）。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// newBudget 构造预算（budgetCalls<=0 返回 nil=不限；成本护栏接线，审查 M3）。
func newBudget(budgetCalls int) *provider.Budget {
	if budgetCalls <= 0 {
		return nil
	}
	return provider.NewBudget(0, budgetCalls)
}

// runCtx 返回采集上下文（无超时；SIGINT 依赖 OS 默认终止——响应已逐条落盘，
// 数据安全，无优雅中止；M4.1 调度接入 signal.NotifyContext 时升级）。
func runCtx() context.Context { return context.Background() }
