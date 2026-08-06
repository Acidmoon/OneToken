// 传输层（M2.2）：Complete 编排——协议锁定/惰性协商 → 限流预算 →
// 单请求（护栏/SSRF/重试矩阵），实现设计 §2.1 的 Provider.Complete 语义。
package provider

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"onetoken/internal/config"
)

// maxErrBody 错误响应体保留字节数（诊断用，防日志/错误信息膨胀）。
const maxErrBody = 512

// Provider 是端点调用抽象（设计 §2.1）：经统一调用层对任意端点做一次完整
// 请求，含协议协商、限流预算、重试矩阵、成本护栏与 SSRF 防护。
// Client 是其具体实现。
type Provider interface {
	Complete(ctx context.Context, rp RequestParams) (*ResponseRecord, error)
}

// Client 是单个端点的调用客户端（协议已解析/协商 + 传输层）。
type Client struct {
	cfg  config.ProviderConfig
	http *http.Client
	rate *RateLimiter

	// mu 保护 protocol（Negotiate 写、Complete/Protocol 读）；协商仅发生一次，
	// 锁竞争仅存在于 auto 协议首次调用窗口（并发竞态审查 H1）。
	mu       sync.Mutex
	protocol Protocol

	// 传输参数（M2.2，来自 Settings）
	maxRetries   int
	retryBase    time.Duration
	retryMax     time.Duration
	maxRespBytes int64
	compSlack    int
}

// NewClient 创建端点客户端（传输参数取默认 Settings；自定义见 NewClientWithSettings）。
func NewClient(cfg config.ProviderConfig, httpClient *http.Client) (*Client, error) {
	return NewClientWithSettings(cfg, httpClient, config.DefaultSettings())
}

// NewClientWithSettings 创建端点客户端，传输参数由调用方 Settings 覆盖。
//
// httpClient 为 nil 时自建安全传输：SSRF 校验 DialContext（解析→校验→拨号，
// ssrf.go）、禁重定向、单请求超时（Limits.TimeoutSec）。调用方注入自定义
// httpClient 时自行负责传输安全（测试常用 httptest.Server.Client()）。
func NewClientWithSettings(cfg config.ProviderConfig, httpClient *http.Client, s config.Settings) (*Client, error) {
	p, err := ParseProtocol(cfg.Protocol)
	if err != nil {
		return nil, err
	}
	if err := validateBaseURL(cfg.BaseURL); err != nil {
		return nil, err
	}
	allow, err := parseAllowList(cfg.SSRFAllow)
	if err != nil {
		return nil, err
	}
	if httpClient == nil {
		timeout := 60 * time.Second
		if cfg.Limits.TimeoutSec > 0 {
			timeout = time.Duration(cfg.Limits.TimeoutSec) * time.Second
		}
		tr := &http.Transport{
			DialContext: secureDialContext(allow, 30*time.Second),
			// ProxyFromEnvironment 保持默认（用户显式信任的系统代理；局限见 ssrf.go）
		}
		httpClient = &http.Client{
			Transport: tr,
			Timeout:   timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse // 禁用重定向（§10/§6.4 安全基线）
			},
		}
	}
	base := time.Duration(s.RetryBaseDelayMS) * time.Millisecond
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	rmax := time.Duration(s.RetryMaxDelayMS) * time.Millisecond
	if rmax < base {
		rmax = base
	}
	// 护栏溢出保护（审查 L5）：maxRespBytes+1 不得溢出
	mb := maxInt64(s.MaxResponseBytes, 1<<20)
	if mb >= math.MaxInt64 {
		mb = math.MaxInt64 - 1
	}
	return &Client{
		cfg:          cfg,
		protocol:     p,
		http:         httpClient,
		rate:         NewRateLimiter(cfg.Limits.RPM, cfg.Limits.RPD),
		maxRetries:   maxInt(s.MaxRetries, 0),
		retryBase:    base,
		retryMax:     rmax,
		maxRespBytes: mb,
		compSlack:    maxInt(s.CompletionSlack, 0),
	}, nil
}

// Complete 执行一次带传输层语义的请求（设计 §2.1 Provider.Complete 的 M2.2 形态）：
//  1. 协议锁定：protocol=auto 时惰性协商（幂等，失败即中止）；
//  2. 限流预算：RPM/RPD 令牌桶（Wait 可被 ctx 取消）；
//  3. 单请求重试：429/5xx 指数退避 + jitter（尊重 Retry-After），4xx 不重试，
//     网络瞬态可重试；总 deadline 由 ctx 控制；
//  4. 成本护栏：响应体字节上限、completion 长度上限（端点忽略 max_tokens）。
//
// 密钥经 BuildHTTPRequest 注入请求头，错误链永不携带密钥值（日志脱敏）。
func (c *Client) Complete(ctx context.Context, rp RequestParams) (*ResponseRecord, error) {
	// 协议锁定（锁内快读）：auto 时惰性协商（幂等；Negotiate 内部持锁，
	// 并发调用仅一次真正协商，其余等待锁定结果）。
	c.mu.Lock()
	proto := c.protocol
	c.mu.Unlock()
	if proto == ProtocolAuto {
		if err := c.Negotiate(ctx, rp.Model); err != nil {
			return nil, err
		}
		c.mu.Lock()
		proto = c.protocol
		c.mu.Unlock()
	}
	if err := c.rate.Wait(ctx); err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		rec, err := c.doOnce(ctx, proto, rp)
		if err == nil {
			return rec, nil
		}
		if !isRetryableErr(err) {
			return nil, err
		}
		lastErr = err
		if attempt >= c.maxRetries {
			return nil, fmt.Errorf("%w（第 %d 次尝试后）: %v", ErrRetryExhausted, attempt+1, lastErr)
		}
		if err := sleepCtx(ctx, retryDelay(attempt, err, c.retryBase, c.retryMax)); err != nil {
			return nil, err
		}
		// 每次重试同样受限流预算约束（429 本身就是预算信号）。
		if err := c.rate.Wait(ctx); err != nil {
			return nil, err
		}
	}
}

// doOnce 执行单次 HTTP 请求（不重试）：构建 → 发送 → 限量读体 → 状态分类 → 解析。
// proto 由调用方传入（Complete 已锁定），避免在热点路径加锁。
func (c *Client) doOnce(ctx context.Context, proto Protocol, rp RequestParams) (*ResponseRecord, error) {
	req, err := BuildHTTPRequest(c.cfg.BaseURL, proto, rp, c.cfg.APIKey(), c.cfg.Headers)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err // 网络瞬态：由 Complete 重试矩阵决定
	}
	defer resp.Body.Close()

	// 成本护栏①：响应体字节上限（限量读，超限即弃）。
	limited := io.LimitReader(resp.Body, c.maxRespBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > c.maxRespBytes {
		return nil, ErrResponseTooLarge
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		ra, ok := ParseRetryAfter(resp.Header)
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: c.redactBody(data), RetryAfter: ra, HasRetryAfter: ok}
	case resp.StatusCode >= 500:
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: c.redactBody(data)}
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// 重定向已禁用（CheckRedirect → ErrUseLastResponse）：3xx 属确定性错误，
		// 不重试（避免重放请求放大；审查 M1/M2 同根）。
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: c.redactBody(data)}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// 密钥错误：不重试（避免把 A 厂商密钥误配到恶意端点后反复重试放大请求）
		return nil, fmt.Errorf("%w: %d %s", ErrKeyRejected, resp.StatusCode, c.redactBody(data))
	case resp.StatusCode == http.StatusBadRequest:
		// 请求体/参数错误：不换协议、不重试（换协议会掩盖真实 bug，设计 §6.3）
		return nil, fmt.Errorf("%w: %d %s", ErrBadRequest, resp.StatusCode, c.redactBody(data))
	case resp.StatusCode >= 400:
		// 其余 4xx（404/405 等）：校验类错误，不重试
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: c.redactBody(data)}
	}

	rec, err := ParseResponse(proto, string(data))
	if err != nil {
		// 200 但非 JSON/结构不符（WAF 拦截页、网关错误页等）：确定性错误不重试，
		// 避免解析失败被当网络瞬态放大请求（审查 M2/L4）。
		return nil, fmt.Errorf("%w: %v", ErrBadResponse, err)
	}
	rec.LatencyMS = time.Since(start).Milliseconds()

	// 成本护栏②：completion 长度上限（端点忽略 max_tokens 的确定性信号；
	// 上层识别 ErrCompletionTooLong 后标记 hidden-reasoning 并中止 cell，M2.4）。
	// 溢出保护（审查 L6）：MaxTokens 极端值下跳过加法直接比较。
	if rp.MaxTokens > 0 {
		if rp.MaxTokens <= math.MaxInt-c.compSlack && rec.CompletionTokens > rp.MaxTokens+c.compSlack {
			return nil, fmt.Errorf("%w: completion_tokens=%d 超过 %d+%d",
				ErrCompletionTooLong, rec.CompletionTokens, rp.MaxTokens, c.compSlack)
		}
	}
	return rec, nil
}

// redactBody 限量截断响应体并擦洗密钥回显（审查 H1）：恶意/误配端点可能
// 在错误体里回显 Authorization 值，任何错误路径（含重试耗尽包装）都经此清洗。
func (c *Client) redactBody(b []byte) string {
	s := truncate(b, maxErrBody)
	if key := c.cfg.APIKey(); key != "" {
		s = strings.ReplaceAll(s, key, "[REDACTED]")
	}
	return s
}

// ---- 小工具 ----

// truncate 截断字节到 n，并回退到完整 UTF-8 rune 边界（审查 L4：
// 避免切断多字节字符产生非法 UTF-8 串）。
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	s := b[:n]
	for len(s) > 0 && !utf8.Valid(s) {
		s = s[:len(s)-1]
	}
	return string(s) + "…"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
