package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"onetoken/internal/config"
)

// ---- URL 构造形态矩阵（验收单测） ----

func TestEndpointURL(t *testing.T) {
	cases := []struct {
		name    string
		base    string
		proto   Protocol
		want    string
		wantErr bool
	}{
		{"根路径", "https://api.openai.com", ProtocolResponses, "https://api.openai.com/v1/responses", false},
		{"尾斜杠", "https://api.openai.com/", ProtocolChat, "https://api.openai.com/v1/chat/completions", false},
		{"误配含 /v1", "https://api.openai.com/v1", ProtocolResponses, "https://api.openai.com/v1/responses", false},
		{"误配含 /v1/", "https://api.openai.com/v1/", ProtocolChat, "https://api.openai.com/v1/chat/completions", false},
		{"本地 vLLM", "http://localhost:8000", ProtocolChat, "http://localhost:8000/v1/chat/completions", false},
		{"本地含 /v1", "http://localhost:8000/v1", ProtocolAnthropic, "http://localhost:8000/v1/messages", false},
		{"任意 BaseURL 子路径", "https://gateway.example.com/proxy", ProtocolResponses, "https://gateway.example.com/proxy/v1/responses", false},
		{"/v10 不误伤", "https://api.example.com/v10", ProtocolChat, "https://api.example.com/v10/v1/chat/completions", false},
		{"/v1something 不误伤", "https://api.example.com/v1something", ProtocolChat, "https://api.example.com/v1something/v1/chat/completions", false},
		{"域名含 v1", "https://v1.example.com", ProtocolChat, "https://v1.example.com/v1/chat/completions", false},
		{"query 拒绝", "https://api.example.com?x=1", ProtocolChat, "", true},
		{"空 base", "", ProtocolResponses, "", true},
		{"未知协议", "https://api.openai.com", Protocol("weird"), "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := EndpointURL(c.base, c.proto)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望错误，实际 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if got != c.want {
				t.Fatalf("URL=%q，期望 %q", got, c.want)
			}
			if strings.Count(got, "/v1/") != 1 {
				t.Fatalf("防双 /v1 失败: %q", got)
			}
		})
	}
}

// ---- 请求体构建 ----

func mustJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("JSON 解析失败: %v (raw=%s)", err, raw)
	}
	return m
}

func TestBuildRequestBodyResponses(t *testing.T) {
	rp := RequestParams{Model: "gpt-5", Prompt: "p", SystemPrompt: "sys", Temperature: 1.0, MaxTokens: 16, ReasoningEffort: "minimal"}
	body, err := BuildRequestBody(ProtocolResponses, rp)
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, string(body))
	if m["model"] != "gpt-5" || m["input"] != "p" || m["instructions"] != "sys" {
		t.Fatalf("responses 字段错: %v", m)
	}
	if m["max_output_tokens"] != float64(16) || m["store"] != false {
		t.Fatalf("responses max_output_tokens/store 错: %v", m)
	}
	r, ok := m["reasoning"].(map[string]any)
	if !ok || r["effort"] != "minimal" {
		t.Fatalf("responses reasoning 应为 {effort:minimal}: %v", m["reasoning"])
	}
	if _, has := m["messages"]; has {
		t.Fatal("responses 不应有 messages 字段")
	}
}

func TestBuildRequestBodyChat(t *testing.T) {
	rp := RequestParams{Model: "gpt-4o", Prompt: "p", SystemPrompt: "sys", Temperature: 0, MaxTokens: 16, ReasoningEffort: "low"}
	body, err := BuildRequestBody(ProtocolChat, rp)
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, string(body))
	msgs := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("chat messages 应为 [system,user]，实际 %v", msgs)
	}
	if m["reasoning_effort"] != "low" { // 顶层字符串
		t.Fatalf("chat reasoning_effort 应为顶层字符串: %v", m["reasoning_effort"])
	}
	if m["max_tokens"] != float64(16) {
		t.Fatalf("chat max_tokens 应为 16: %v", m["max_tokens"])
	}
	if _, has := m["reasoning"]; has {
		t.Fatal("chat 不应有嵌套 reasoning 对象")
	}
	if _, has := m["max_output_tokens"]; has {
		t.Fatal("chat 应使用 max_tokens")
	}
}

func TestBuildRequestBodyAnthropic(t *testing.T) {
	rp := RequestParams{Model: "claude", Prompt: "p", SystemPrompt: "sys", Temperature: 1.0, MaxTokens: 16}
	body, err := BuildRequestBody(ProtocolAnthropic, rp)
	if err != nil {
		t.Fatal(err)
	}
	m := mustJSON(t, string(body))
	if m["system"] != "sys" { // anthropic system 顶层
		t.Fatalf("anthropic system 应为顶层: %v", m)
	}
	if m["max_tokens"] != float64(16) {
		t.Fatalf("anthropic max_tokens 必填: %v", m)
	}
	th, ok := m["thinking"].(map[string]any)
	if !ok || th["type"] != "disabled" {
		t.Fatalf("anthropic thinking 应 {type:disabled}: %v", m["thinking"])
	}
}

// ---- HTTP 请求头 ----

func TestBuildHTTPRequestHeaders(t *testing.T) {
	// responses/chat: Bearer
	req, err := BuildHTTPRequest("https://api.openai.com", ProtocolChat,
		RequestParams{Model: "m", Prompt: "p", MaxTokens: 16}, "sk-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer sk-test" {
		t.Fatalf("chat 认证头错: %q", req.Header.Get("Authorization"))
	}
	// anthropic: x-api-key + version
	req2, err := BuildHTTPRequest("https://api.anthropic.com", ProtocolAnthropic,
		RequestParams{Model: "m", Prompt: "p", MaxTokens: 16}, "sk-ant-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if req2.Header.Get("x-api-key") != "sk-ant-test" || req2.Header.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("anthropic 认证头错: x-api-key=%q version=%q",
			req2.Header.Get("x-api-key"), req2.Header.Get("anthropic-version"))
	}
	if req2.Header.Get("Authorization") != "" {
		t.Fatal("anthropic 不应有 Authorization 头")
	}
	// 附加头
	req3, err := BuildHTTPRequest("https://openrouter.ai", ProtocolChat,
		RequestParams{Model: "m", Prompt: "p", MaxTokens: 16}, "sk-test",
		map[string]string{"HTTP-Referer": "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if req3.Header.Get("HTTP-Referer") != "https://example.com" {
		t.Fatalf("附加头丢失: %q", req3.Header.Get("HTTP-Referer"))
	}
	// 空密钥：非 localhost 拒绝
	if _, err := BuildHTTPRequest("https://api.openai.com", ProtocolChat,
		RequestParams{Model: "m", Prompt: "p", MaxTokens: 16}, "", nil); err == nil {
		t.Fatal("非 localhost 空密钥应报错")
	}
	// 空密钥：localhost 豁免（本地 vLLM 无认证）
	req4, err := BuildHTTPRequest("http://localhost:8000", ProtocolChat,
		RequestParams{Model: "m", Prompt: "p", MaxTokens: 16}, "", nil)
	if err != nil {
		t.Fatalf("localhost 空密钥应豁免: %v", err)
	}
	if req4.Header.Get("Authorization") != "" {
		t.Fatal("localhost 空密钥不应设 Authorization")
	}
}

// ---- 响应解析（三协议） ----

const responsesBody = `{
  "output": [
    {"type": "reasoning", "content": [{"type": "reasoning_text", "text": "hidden"}]},
    {"type": "message", "role": "assistant",
     "content": [{"type": "output_text", "text": "42"}],
     "finish_reason": "stop"}
  ],
  "usage": {"input_tokens": 12, "output_tokens": 5,
            "output_tokens_details": {"reasoning_tokens": 3}},
  "model": "gpt-5"
}`

func TestParseResponses(t *testing.T) {
	rec, err := ParseResponse(ProtocolResponses, responsesBody)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Text != "42" {
		t.Fatalf("Text=%q，期望 42（reasoning item 应被过滤）", rec.Text)
	}
	if rec.FinishReason != "stop" {
		t.Fatalf("FinishReason=%q", rec.FinishReason)
	}
	if rec.ReasoningTokens != 3 {
		t.Fatalf("ReasoningTokens=%d，期望 3（output_tokens_details）", rec.ReasoningTokens)
	}
	if rec.PromptTokens != 12 || rec.CompletionTokens != 5 {
		t.Fatalf("usage 错: %d/%d", rec.PromptTokens, rec.CompletionTokens)
	}
	if rec.ReportedModel != "gpt-5" {
		t.Fatalf("ReportedModel=%q", rec.ReportedModel)
	}
}

const chatBody = `{
  "choices": [{"message": {"content": "blue"}, "finish_reason": "length"}],
  "usage": {"prompt_tokens": 20, "completion_tokens": 16,
            "completion_tokens_details": {"reasoning_tokens": 10}},
  "model": "gpt-4o"
}`

func TestParseChat(t *testing.T) {
	rec, err := ParseResponse(ProtocolChat, chatBody)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Text != "blue" || rec.FinishReason != "length" {
		t.Fatalf("chat 提取错: %q/%q", rec.Text, rec.FinishReason)
	}
	if rec.ReasoningTokens != 10 {
		t.Fatalf("ReasoningTokens=%d，期望 10（completion_tokens_details）", rec.ReasoningTokens)
	}
	if rec.PromptTokens != 20 || rec.CompletionTokens != 16 {
		t.Fatalf("usage 错: %d/%d", rec.PromptTokens, rec.CompletionTokens)
	}
}

const anthropicBody = `{
  "content": [{"type": "text", "text": "hello"}, {"type": "text", "text": " world"}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 30, "output_tokens": 6,
            "cache_creation_input_tokens": 5, "cache_read_input_tokens": 7},
  "model": "claude-sonnet"
}`

func TestParseAnthropic(t *testing.T) {
	rec, err := ParseResponse(ProtocolAnthropic, anthropicBody)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Text != "hello world" {
		t.Fatalf("Text=%q（多 text 块应拼接）", rec.Text)
	}
	if rec.FinishReason != "end_turn" {
		t.Fatalf("FinishReason=%q", rec.FinishReason)
	}
	if rec.ReasoningTokens != 0 {
		t.Fatalf("anthropic 无 reasoning_tokens，应为 0: %d", rec.ReasoningTokens)
	}
	if rec.CachedTokens != 7 {
		t.Fatalf("CachedTokens=%d，期望 7（cache_read）", rec.CachedTokens)
	}
}

const responsesCachedBody = `{"output": [{"type": "message", "content": [{"type": "output_text", "text": "a"}]}],
  "usage": {"input_tokens": 12, "output_tokens": 2,
            "input_tokens_details": {"cached_tokens": 8}}}`
const chatCachedBody = `{"choices": [{"message": {"content": "b"}}],
  "usage": {"prompt_tokens": 20, "completion_tokens": 2,
            "prompt_tokens_details": {"cached_tokens": 9}}}`

func TestParseCachedTokens(t *testing.T) {
	rec, err := ParseResponse(ProtocolResponses, responsesCachedBody)
	if err != nil {
		t.Fatal(err)
	}
	if rec.CachedTokens != 8 {
		t.Fatalf("responses CachedTokens=%d，期望 8（input_tokens_details）", rec.CachedTokens)
	}
	rec2, err := ParseResponse(ProtocolChat, chatCachedBody)
	if err != nil {
		t.Fatal(err)
	}
	if rec2.CachedTokens != 9 {
		t.Fatalf("chat CachedTokens=%d，期望 9（prompt_tokens_details）", rec2.CachedTokens)
	}
}

func TestParseInvalid(t *testing.T) {
	if _, err := ParseResponse(ProtocolChat, "not json"); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	if _, err := ParseResponse(Protocol("weird"), "{}"); err == nil {
		t.Fatal("未知协议应报错")
	}
}

// ---- auto 协商（mock 服务器，§6.3 四步） ----

// 测试用：注入密钥（httptest 服务器不校验，但 BuildHTTPRequest 要求非空）。
func testConfig(base string, protocol string) config.ProviderConfig {
	p := config.ProviderConfig{
		Name:     "test",
		BaseURL:  base,
		Protocol: protocol,
	}
	p.SetAPIKey("sk-test")
	return p
}

// 测试用：注入密钥（ProviderConfig.apiKey 不导出，通过 NewClient 无法注入——
// 这里用 httptest 服务器不校验密钥，协商只看状态码）。
func TestNegotiateExplicit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c, err := NewClient(testConfig(srv.URL, "chat"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Negotiate(context.Background(), "m"); err != nil {
		t.Fatalf("显式 chat 不应协商: %v", err)
	}
	if c.Protocol() != ProtocolChat {
		t.Fatalf("协议应为 chat，实际 %s", c.Protocol())
	}
}

func TestNegotiateAuto200(t *testing.T) {
	// responses 端点 200 → 锁定 responses
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/responses") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c, err := NewClient(testConfig(srv.URL, "auto"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Negotiate(context.Background(), "m"); err != nil {
		t.Fatalf("协商失败: %v", err)
	}
	if c.Protocol() != ProtocolResponses {
		t.Fatalf("应锁定 responses，实际 %s", c.Protocol())
	}
}

func TestNegotiateAuto404ThenChat(t *testing.T) {
	// responses 404 → 换 chat（200）
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		if strings.Contains(r.URL.Path, "/responses") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c, _ := NewClient(testConfig(srv.URL, "auto"), nil)
	if err := c.Negotiate(context.Background(), "m"); err != nil {
		t.Fatalf("协商失败: %v", err)
	}
	if c.Protocol() != ProtocolChat {
		t.Fatalf("应锁定 chat，实际 %s", c.Protocol())
	}
	if len(hits) != 2 {
		t.Fatalf("应探测 2 个端点，实际 %d: %v", len(hits), hits)
	}
}

func TestNegotiateKeyRejected(t *testing.T) {
	// 401 → ErrKeyRejected 且不继续探测
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c, _ := NewClient(testConfig(srv.URL, "auto"), nil)
	err := c.Negotiate(context.Background(), "m")
	if err == nil || !strings.Contains(err.Error(), "密钥被拒绝") {
		t.Fatalf("应返回 ErrKeyRejected: %v", err)
	}
	if hits != 1 {
		t.Fatalf("401 后不应继续探测，实际 %d 次", hits)
	}
}

func TestNegotiateBadRequestAbort(t *testing.T) {
	// 400 → ErrBadRequest 中止（不换协议）
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	c, _ := NewClient(testConfig(srv.URL, "auto"), nil)
	err := c.Negotiate(context.Background(), "m")
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("应返回 400 中止: %v", err)
	}
	if hits != 1 {
		t.Fatalf("400 后不应换协议，实际 %d 次", hits)
	}
}

func TestNegotiateAllFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c, _ := NewClient(testConfig(srv.URL, "auto"), nil)
	err := c.Negotiate(context.Background(), "m")
	if err == nil || !strings.Contains(err.Error(), "protocol-undetermined") {
		t.Fatalf("三协议失败应 protocol-undetermined: %v", err)
	}
}

func TestCandidateProtocols(t *testing.T) {
	cases := map[string][]Protocol{
		"https://api.openai.com":    {ProtocolResponses, ProtocolChat},
		"https://api.anthropic.com": {ProtocolAnthropic},
		"https://openrouter.ai":     {ProtocolResponses, ProtocolChat, ProtocolAnthropic},
		"http://localhost:8000":     {ProtocolResponses, ProtocolChat, ProtocolAnthropic},
	}
	for base, want := range cases {
		got := candidateProtocols(base)
		if len(got) != len(want) {
			t.Fatalf("%s 候选=%v，期望 %v", base, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s 候选=%v，期望 %v", base, got, want)
			}
		}
	}
}

// ---- base_url scheme 校验 ----

func TestNegotiateServerError(t *testing.T) {
	// 503：端点存在但暂不可用 → 中止（不换协议，避免锁错）
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c, _ := NewClient(testConfig(srv.URL, "auto"), nil)
	err := c.Negotiate(context.Background(), "m")
	if err == nil || !strings.Contains(err.Error(), "暂不可用或限流") {
		t.Fatalf("5xx 应中止: %v", err)
	}
	if hits != 1 {
		t.Fatalf("5xx 后不应换协议，实际 %d 次", hits)
	}
}

func TestNegotiateForbidden(t *testing.T) {
	// 403 与 401 同语义（密钥拒绝，不降级）
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c, _ := NewClient(testConfig(srv.URL, "auto"), nil)
	err := c.Negotiate(context.Background(), "m")
	if err == nil || !strings.Contains(err.Error(), "密钥被拒绝") {
		t.Fatalf("403 应视为密钥拒绝: %v", err)
	}
	if hits != 1 {
		t.Fatalf("403 后不应继续探测，实际 %d 次", hits)
	}
}

// 审查 F1：重定向禁用（x-api-key 不随重定向外泄）
func TestClientNoRedirect(t *testing.T) {
	var leaked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			w.Header().Set("Location", "/v1/chat/completions")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		leaked = r.Header.Get("x-api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfg := testConfig(srv.URL, "anthropic")
	c, err := NewClient(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := BuildHTTPRequest(cfg.BaseURL, ProtocolAnthropic,
		RequestParams{Model: "m", Prompt: "p", MaxTokens: 16}, "sk-ant-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("应保持 307（不跟随），实际 %d", resp.StatusCode)
	}
	if leaked != "" {
		t.Fatal("重定向目标不应收到 x-api-key")
	}
}

func TestValidateBaseURL(t *testing.T) {
	if err := validateBaseURL("https://api.openai.com"); err != nil {
		t.Fatalf("https 应通过: %v", err)
	}
	if err := validateBaseURL("http://localhost:8000"); err != nil {
		t.Fatalf("localhost http 应通过: %v", err)
	}
	if err := validateBaseURL("http://127.0.0.1:8000"); err != nil {
		t.Fatalf("127.0.0.1 http 应通过: %v", err)
	}
	for _, bad := range []string{"http://api.openai.com", "ftp://x.com", "", "https://"} {
		if err := validateBaseURL(bad); err == nil {
			t.Fatalf("%q 应被拒绝", bad)
		}
	}
	// userinfo/query/fragment（审查 F2）
	for _, bad := range []string{
		"https://api.openai.com@evil.com", "https://api.openai.com?x=1", "https://api.openai.com#frag",
	} {
		if err := validateBaseURL(bad); err == nil {
			t.Fatalf("%q 应被拒绝（userinfo/query/fragment）", bad)
		}
	}
	// EndpointURL 也拒绝畸形（CLI 直传路径防御）
	if _, err := EndpointURL("https://api.example.com?x=1", ProtocolChat); err == nil {
		t.Fatal("EndpointURL 应拒绝 query")
	}
}
