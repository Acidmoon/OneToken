// Package provider 实现统一提供商调用层（设计 §6）：任意 BaseURL + API Key 端点，
// 三协议适配（OpenAI Responses / chat completions 兼容 / Anthropic messages）。
//
// M2.1 范围：URL 构造（防双 /v1）、请求体构建、响应解析收敛为 ResponseRecord、
// auto 协商四步。传输层（重试/限流/SSRF/DNS rebinding）在 M2.2。
package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"onetoken/internal/config"
)

// Protocol 是三协议枚举。
type Protocol string

const (
	ProtocolAuto      Protocol = "auto"
	ProtocolResponses Protocol = "responses"
	ProtocolChat      Protocol = "chat"
	ProtocolAnthropic Protocol = "anthropic"
)

// 协商/解析错误（M2.1 语义见 §6.3）。
var (
	ErrUnknownProtocol = errors.New("provider: 未知协议")
	ErrKeyRejected     = errors.New("provider: 密钥被拒绝（401/403，不降级协议）")
	ErrBadRequest      = errors.New("provider: 请求体/参数被 400 拒绝（不换协议，中止）")
	ErrUndetermined    = errors.New("provider: 三协议探测均失败（protocol-undetermined）")
	ErrEmptyAPIKey     = errors.New("provider: 密钥为空")
)

// endpoints 协议 → API 端点路径段（base_url 不含 /v1，层内统一拼 /v1/<endpoint>）。
var endpoints = map[Protocol]string{
	ProtocolResponses: "responses",
	ProtocolChat:      "chat/completions",
	ProtocolAnthropic: "messages",
}

// ResponseRecord 是三协议收敛的统一响应结构（设计 §6.4）。
type ResponseRecord struct {
	Protocol         string // responses | chat | anthropic
	Text             string // 提取后的回答文本
	RawCompletion    string // 原样保留（证据链 + raw_hash）
	FinishReason     string // stop | length | ...
	ReasoningTokens  int    // responses 的 output_tokens_details / chat 的 completion_tokens_details
	CompletionTokens int
	PromptTokens     int
	CachedTokens     int // anthropic cache_read（缓存签名探测用）
	LatencyMS        int64
	Provider         string // 上游路由（OpenRouter 等透传）
	ReportedModel    string
	CostUSD          float64
	TS               time.Time // UTC
}

// RequestParams 是协议无关的请求参数（M2.1 最小集）。
type RequestParams struct {
	Model           string  // 模型标识
	Prompt          string  // 用户提示（cell 提示词）
	SystemPrompt    string  // 系统提示（responses→instructions、chat→system message、anthropic→顶层 system）
	Temperature     float64 // 采样温度
	MaxTokens       int     // 输出上限（协议映射：max_output_tokens / max_tokens / max_tokens）
	ReasoningEffort string  // responses 的 reasoning.effort / chat 顶层 reasoning_effort；空则不设
	Store           bool    // responses 的 store（默认 false）
}

// ParseProtocol 解析协议字符串；空串按 auto。
func ParseProtocol(s string) (Protocol, error) {
	p := Protocol(strings.ToLower(strings.TrimSpace(s)))
	if p == "" {
		return ProtocolAuto, nil
	}
	switch p {
	case ProtocolAuto, ProtocolResponses, ProtocolChat, ProtocolAnthropic:
		return p, nil
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownProtocol, s)
}

// EndpointURL 拼接 base_url + /v1/<endpoint>，防双 /v1。
//
// 规范化：去尾斜杠 → 去尾部 "/v1" 段 → 拼 /v1/<endpoint>。
// 形态矩阵（验收单测）：尾斜杠、含 /v1、含 /v1/、localhost、根路径。
func EndpointURL(baseURL string, p Protocol) (string, error) {
	ep, ok := endpoints[p]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownProtocol, p)
	}
	// 防畸形（query/fragment/userinfo 会吞掉路径段；审查 F2）
	if err := validateBaseURL(baseURL); err != nil {
		return "", err
	}
	base := strings.TrimRight(baseURL, "/")
	// 防双 /v1：仅当尾路径段恰为 v1 时去除（任意 BaseURL 语义：用户可能误配含 /v1）
	base = strings.TrimSuffix(base, "/v1")
	base = strings.TrimRight(base, "/")
	if base == "" {
		return "", fmt.Errorf("provider: base_url 为空")
	}
	return base + "/v1/" + ep, nil
}

// BuildRequestBody 按协议构建请求 JSON 体（设计 §6.2 适配矩阵）。
func BuildRequestBody(p Protocol, rp RequestParams) ([]byte, error) {
	switch p {
	case ProtocolResponses:
		body := map[string]any{
			"model":             rp.Model,
			"input":             rp.Prompt,
			"temperature":       rp.Temperature,
			"max_output_tokens": rp.MaxTokens,
			"store":             rp.Store,
		}
		if rp.SystemPrompt != "" {
			body["instructions"] = rp.SystemPrompt
		}
		if rp.ReasoningEffort != "" {
			body["reasoning"] = map[string]any{"effort": rp.ReasoningEffort}
		}
		return json.Marshal(body)

	case ProtocolChat:
		msgs := []map[string]any{}
		if rp.SystemPrompt != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": rp.SystemPrompt})
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": rp.Prompt})
		body := map[string]any{
			"model":       rp.Model,
			"messages":    msgs,
			"temperature": rp.Temperature,
			"max_tokens":  rp.MaxTokens,
		}
		if rp.ReasoningEffort != "" {
			// 顶层字符串（非嵌套对象）；o 系/gpt-5 部分端点忽略，能力探测降级（M2.1 只构建）
			body["reasoning_effort"] = rp.ReasoningEffort
		}
		return json.Marshal(body)

	case ProtocolAnthropic:
		body := map[string]any{
			"model":       rp.Model,
			"max_tokens":  rp.MaxTokens,
			"temperature": rp.Temperature,
			"thinking":    map[string]any{"type": "disabled"},
			"messages":    []map[string]any{{"role": "user", "content": rp.Prompt}},
		}
		if rp.SystemPrompt != "" {
			body["system"] = rp.SystemPrompt // anthropic：system 为顶层参数
		}
		return json.Marshal(body)
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownProtocol, p)
}

// BuildHTTPRequest 构造完整 HTTP 请求（URL + 认证头 + 附加头 + 请求体）。
func BuildHTTPRequest(baseURL string, p Protocol, rp RequestParams, apiKey string, extraHeaders map[string]string) (*http.Request, error) {
	u, err := EndpointURL(baseURL, p)
	if err != nil {
		return nil, err
	}
	body, err := BuildRequestBody(p, rp)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	hasKey := apiKey != ""
	switch p {
	case ProtocolAnthropic:
		if !hasKey && !isLocalHost(req.URL.Hostname()) {
			return nil, ErrEmptyAPIKey
		}
		if hasKey {
			req.Header.Set("x-api-key", apiKey)
		}
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		if !hasKey && !isLocalHost(req.URL.Hostname()) {
			return nil, ErrEmptyAPIKey
		}
		if hasKey {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}
	for k, v := range extraHeaders {
		// 受保护/认证头禁止被附加头覆盖（审查 S-M3/L5）：
		// 认证头只走 api_key_env（config 已拒同名头，此处双保险防注入路径），
		// Content-Type/Host 由请求语义决定，anthropic-version 由协议层固定。
		switch strings.ToLower(k) {
		case "authorization", "proxy-authorization", "x-api-key", "api-key",
			"apikey", "cookie", "content-type", "host", "anthropic-version":
			continue
		}
		req.Header.Set(k, v)
	}
	return req, nil
}

// ---- 响应解析 ----

// rawResponse 是三协议响应的宽松结构（字段按需提取，容忍额外字段）。
type rawResponse struct {
	// Responses
	Output []struct {
		Type         string `json:"type"`
		FinishReason string `json:"finish_reason"`
		Content      []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	// Chat
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	// Anthropic
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	// 统一 usage 结构（三协议字段并集，缺失为 0）
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		InputTokens      int `json:"input_tokens"`
		OutputTokens     int `json:"output_tokens"`
		InputDetails     struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		PromptDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		OutputDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
		CompletionDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
		CacheCreation int `json:"cache_creation_input_tokens"`
		CacheRead     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	Model string `json:"model"`
}

// ParseResponse 解析三协议响应体为 ResponseRecord。
func ParseResponse(p Protocol, raw string) (*ResponseRecord, error) {
	var r rawResponse
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("provider: 响应解析失败: %w", err)
	}
	rec := &ResponseRecord{
		Protocol:      string(p),
		RawCompletion: raw,
		ReportedModel: r.Model,
	}
	now := time.Now().UTC()
	rec.TS = now
	switch p {
	case ProtocolResponses:
		// output[] 过滤 type=="message"，content[] 过滤 type=="output_text"
		var parts []string
		for _, item := range r.Output {
			if item.Type != "message" {
				continue // reasoning/tool 等 item 跳过
			}
			if rec.FinishReason == "" {
				rec.FinishReason = item.FinishReason
			}
			for _, c := range item.Content {
				if c.Type == "output_text" {
					parts = append(parts, c.Text)
				}
			}
		}
		rec.Text = strings.Join(parts, "")
		rec.PromptTokens = r.Usage.InputTokens
		rec.CompletionTokens = r.Usage.OutputTokens
		rec.ReasoningTokens = r.Usage.OutputDetails.ReasoningTokens
		rec.CachedTokens = r.Usage.InputDetails.CachedTokens // input_tokens_details.cached_tokens（§6.2）
	case ProtocolChat:
		if len(r.Choices) > 0 {
			rec.Text = r.Choices[0].Message.Content
			rec.FinishReason = r.Choices[0].FinishReason
		}
		rec.PromptTokens = r.Usage.PromptTokens
		rec.CompletionTokens = r.Usage.CompletionTokens
		rec.ReasoningTokens = r.Usage.CompletionDetails.ReasoningTokens
		rec.CachedTokens = r.Usage.PromptDetails.CachedTokens // chat 用 prompt_tokens_details（§6.2）
	case ProtocolAnthropic:
		var parts []string
		for _, c := range r.Content {
			if c.Type == "text" {
				parts = append(parts, c.Text)
			}
		}
		rec.Text = strings.Join(parts, "")
		rec.FinishReason = r.StopReason
		rec.PromptTokens = r.Usage.InputTokens
		rec.CompletionTokens = r.Usage.OutputTokens
		rec.CachedTokens = r.Usage.CacheRead // 缓存命中（签名探测用）
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownProtocol, p)
	}
	return rec, nil
}

// ---- Client ----
//
// Client 结构与 NewClient/NewClientWithSettings 在 transport.go（M2.2 传输层）。
// 本文件保留 Client 的只读视图方法（Protocol/Config/Endpoint）与协商入口。

// Protocol 返回当前协议（协商后为锁定值）。并发安全（审查 H1）。
func (c *Client) Protocol() Protocol {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.protocol
}

// Config 返回端点配置（脱敏后的只读视图由调用方自行处理）。
func (c *Client) Config() config.ProviderConfig { return c.cfg }

// Endpoint 返回当前协议端点 URL。
func (c *Client) Endpoint() (string, error) { return EndpointURL(c.cfg.BaseURL, c.protocol) }

// BaseURL 规范化校验（scheme/路径语义；SSRF IP 拦截在 M2.2）。
// 覆盖所有协议路径（NewClient 即校验，不依赖 Negotiate 的 auto 分支）。
func validateBaseURL(base string) error {
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("provider: base_url 解析失败: %w", err)
	}
	if u.User != nil {
		return fmt.Errorf("provider: base_url 不得含 userinfo（防主机混淆）")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("provider: base_url 不得含 query/fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLocalHost(u.Hostname())) {
		return fmt.Errorf("provider: scheme 必须为 https（localhost 例外 http）: %s", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("provider: base_url 缺少主机")
	}
	return nil
}

// isLocalHost 判断主机名是否为 localhost / 127.x / ::1（本地通道豁免）。
func isLocalHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
