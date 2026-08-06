package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"onetoken/internal/config"
)

// testSettings 传输参数测试配置：极小退避，让重试测试快速完成。
func testSettings() config.Settings {
	s := config.DefaultSettings()
	s.MaxRetries = 3
	s.RetryBaseDelayMS = 1
	s.RetryMaxDelayMS = 5
	return s
}

func chatOKResponse(content string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`, content)
}

// newTestClient 创建 chat 协议 client（自建安全传输或注入 client），返回端点函数。
func newTestClient(t *testing.T, srv *httptest.Server, httpClient *http.Client, s config.Settings) *Client {
	t.Helper()
	cfg := testConfig(srv.URL, "chat")
	c, err := NewClientWithSettings(cfg, httpClient, s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func reqParams(model string) RequestParams {
	return RequestParams{Model: model, Prompt: "random number", SystemPrompt: "one word", Temperature: 1.0, MaxTokens: 16}
}

func TestCompleteOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(chatOKResponse("7")))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, nil, testSettings())

	rec, err := c.Complete(context.Background(), reqParams("m"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Text != "7" {
		t.Fatalf("Text=%q，期望 \"7\"", rec.Text)
	}
	if rec.FinishReason != "stop" {
		t.Fatalf("FinishReason=%q", rec.FinishReason)
	}
	if rec.LatencyMS < 0 {
		t.Fatal("LatencyMS 非法")
	}
}

func TestCompleteAutoNegotiates(t *testing.T) {
	// auto 协议 + 首次 Complete 惰性协商（responses 端点 200 → 锁定）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/responses") {
			w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"7"}]}],"usage":{"input_tokens":10,"output_tokens":1}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL, "auto")
	c, err := NewClientWithSettings(cfg, nil, testSettings())
	if err != nil {
		t.Fatal(err)
	}
	rec, err := c.Complete(context.Background(), reqParams("m"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Protocol != "responses" || c.Protocol() != ProtocolResponses {
		t.Fatalf("应锁定 responses，实际 %s", c.Protocol())
	}
}

// ---- 重试矩阵 ----

func TestCompleteRetry429ThenOK(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "0") // 尊重退避：0s 立即重试
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(chatOKResponse("7")))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, nil, testSettings())

	if _, err := c.Complete(context.Background(), reqParams("m")); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 {
		t.Fatalf("应重试 1 次共 2 请求，实际 %d", n.Load())
	}
}

func TestCompleteRetry429RetryAfterMS(t *testing.T) {
	// Anthropic 毫秒变体
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("retry-after-ms", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(chatOKResponse("7")))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, nil, testSettings())

	if _, err := c.Complete(context.Background(), reqParams("m")); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 {
		t.Fatalf("应重试 1 次，实际 %d", n.Load())
	}
}

func TestCompleteRetry5xxThenOK(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(chatOKResponse("7")))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, nil, testSettings())

	if _, err := c.Complete(context.Background(), reqParams("m")); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 3 {
		t.Fatalf("应重试 2 次共 3 请求，实际 %d", n.Load())
	}
}

func TestCompleteNoRetry4xx(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, nil, testSettings())

	_, err := c.Complete(context.Background(), reqParams("m"))
	if err == nil {
		t.Fatal("404 应报错")
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != 404 {
		t.Fatalf("应返回 HTTPError{404}，实际 %v", err)
	}
	if n.Load() != 1 {
		t.Fatalf("4xx 不应重试，实际请求 %d 次", n.Load())
	}
}

func TestCompleteKeyRejectedNoRetry(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, nil, testSettings())

	_, err := c.Complete(context.Background(), reqParams("m"))
	if !errors.Is(err, ErrKeyRejected) {
		t.Fatalf("401 应报 ErrKeyRejected，实际 %v", err)
	}
	if n.Load() != 1 {
		t.Fatalf("401 不应重试，实际请求 %d 次", n.Load())
	}
}

func TestCompleteBadRequestAbort(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, nil, testSettings())

	_, err := c.Complete(context.Background(), reqParams("m"))
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("400 应报 ErrBadRequest，实际 %v", err)
	}
	if n.Load() != 1 {
		t.Fatalf("400 不应重试，实际请求 %d 次", n.Load())
	}
}

func TestCompleteRetryExhausted(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	s := testSettings()
	s.MaxRetries = 2
	c := newTestClient(t, srv, nil, s)

	_, err := c.Complete(context.Background(), reqParams("m"))
	if !errors.Is(err, ErrRetryExhausted) {
		t.Fatalf("应报 ErrRetryExhausted，实际 %v", err)
	}
	if n.Load() != 3 {
		t.Fatalf("maxRetries=2 应共 3 请求，实际 %d", n.Load())
	}
}

// ---- 成本护栏 ----

func TestCompleteResponseTooLarge(t *testing.T) {
	big := strings.Repeat("a", 2<<20) // 2 MiB > 默认 1 MiB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(big))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, nil, testSettings())

	_, err := c.Complete(context.Background(), reqParams("m"))
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("应报 ErrResponseTooLarge，实际 %v", err)
	}
}

func TestCompleteCompletionTooLong(t *testing.T) {
	// 端点忽略 max_tokens：completion_tokens=100 > 16+16（slack 默认 16）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":100}}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, nil, testSettings())

	_, err := c.Complete(context.Background(), reqParams("m"))
	if !errors.Is(err, ErrCompletionTooLong) {
		t.Fatalf("应报 ErrCompletionTooLong，实际 %v", err)
	}
}

// ---- 限流集成 ----

func TestCompleteRateLimited(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Write([]byte(chatOKResponse("7")))
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL, "chat")
	cfg.Limits.RPM = 1 // 每分钟 1 个
	c, err := NewClientWithSettings(cfg, nil, testSettings())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.Complete(context.Background(), reqParams("m")); err != nil {
		t.Fatal(err)
	}
	// 第 2 次：桶空需等 60s，ctx 30ms 超时 → 限流等待失败
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Complete(ctx, reqParams("m")); err == nil {
		t.Fatal("限流下应失败（ctx 超时）")
	}
	if n.Load() != 1 {
		t.Fatalf("第 2 次不应发出请求，实际 %d", n.Load())
	}
}

// ---- 日志脱敏（密钥永不进错误） ----

func TestCompleteSecretNotLeaked(t *testing.T) {
	const secret = "sk-secret-xyz-123456"
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"401", http.StatusUnauthorized, `{"error":"bad key"}`},
		{"400", http.StatusBadRequest, `{"error":"bad request"}`},
		{"500", http.StatusInternalServerError, `{"error":"boom"}`},
		{"429", http.StatusTooManyRequests, `{"error":"slow down"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			cfg := testConfig(srv.URL, "chat")
			cfg.SetAPIKey(secret)
			c, err := NewClientWithSettings(cfg, nil, testSettings())
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Complete(context.Background(), reqParams("m"))
			if err == nil {
				t.Fatal("应报错")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("错误消息泄露密钥: %v", err)
			}
			if strings.Contains(fmt.Sprintf("%+v", err), secret) {
				t.Fatalf("%+v 泄露密钥: %+v", secret, err)
			}
		})
	}
}

// ---- ctx 取消：重试等待中尊重 ctx ----

func TestCompleteCtxCancelDuringRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	// 退避上限放大到 50ms：单次 sleep 超过 ctx 30ms，验证 ctx 先于重试耗尽生效
	s := testSettings()
	s.RetryMaxDelayMS = 50
	c := newTestClient(t, srv, nil, s)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := c.Complete(ctx, reqParams("m"))
	if err == nil {
		t.Fatal("应报错")
	}
	if errors.Is(err, ErrRetryExhausted) {
		t.Fatalf("ctx 取消应短路，不应重试耗尽: %v", err)
	}
}

// ---- 审查回归（正确性 M1/M2、安全性 H1、L1） ----

func TestCompleteNoRetry3xx(t *testing.T) {
	// 3xx（重定向已禁用）属确定性错误：不重试、不误报解析失败
	for _, code := range []int{301, 302, 307, 308} {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			http.Redirect(w, r, "/elsewhere", code)
		}))
		c := newTestClient(t, srv, nil, testSettings())
		_, err := c.Complete(context.Background(), reqParams("m"))
		if err == nil {
			t.Fatalf("%d 应报错", code)
		}
		var he *HTTPError
		if !errors.As(err, &he) || he.StatusCode != code {
			t.Fatalf("%d 应返回 HTTPError{%d}，实际 %v", code, code, err)
		}
		if n.Load() != 1 {
			t.Fatalf("%d 不应重试，实际请求 %d 次", code, n.Load())
		}
		srv.Close()
	}
}

func TestCompleteBadResponseNoRetry(t *testing.T) {
	// 200 + 非 JSON（WAF/网关错误页）：确定性错误不重试（防请求放大）
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>Forbidden by WAF</html>"))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, nil, testSettings())

	_, err := c.Complete(context.Background(), reqParams("m"))
	if !errors.Is(err, ErrBadResponse) {
		t.Fatalf("应报 ErrBadResponse，实际 %v", err)
	}
	if n.Load() != 1 {
		t.Fatalf("解析失败不应重试，实际请求 %d 次", n.Load())
	}
}

func TestCompleteSecretEchoRedacted(t *testing.T) {
	// 恶意/误配端点把 Authorization 回显进错误体：错误消息必须擦洗（审查 H1）
	const secret = "sk-echo-secret-789"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		echo := "bad auth: " + r.Header.Get("Authorization")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(echo))
	}))
	defer srv.Close()
	cfg := testConfig(srv.URL, "chat")
	cfg.SetAPIKey(secret)
	c, err := NewClientWithSettings(cfg, nil, testSettings())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Complete(context.Background(), reqParams("m"))
	if err == nil {
		t.Fatal("应报错")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("密钥回显未擦洗: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("应含擦洗标记: %v", err)
	}
}

func TestCompleteConcurrentAutoNegotiate(t *testing.T) {
	// 并发 auto 协商竞态回归（审查 H1）：-race 下必须干净。
	// 协商仅一次，其余 goroutine 等待锁定结果。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(chatOKResponse("7")))
	}))
	defer srv.Close()
	cfg := testConfig(srv.URL, "auto")
	c, err := NewClientWithSettings(cfg, nil, testSettings())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, err := c.Complete(context.Background(), reqParams("m"))
			if err != nil || rec == nil {
				t.Errorf("并发 Complete 失败: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestRetryAfterOverflowClamped(t *testing.T) {
	// 超大头值不得环绕为负（审查 L1）
	h := http.Header{}
	h.Set("Retry-After", "9223372036854775807")
	d, ok := ParseRetryAfter(h)
	if !ok || d <= 0 {
		t.Fatalf("超大头值应 clamp 到正值，实际 %v ok=%v", d, ok)
	}
	h2 := http.Header{}
	h2.Set("retry-after-ms", "9223372036854775807")
	d2, ok2 := ParseRetryAfter(h2)
	if !ok2 || d2 <= 0 {
		t.Fatalf("超大头值（ms）应 clamp 到正值，实际 %v ok=%v", d2, ok2)
	}
}

func TestBuildHTTPRequestProtectedHeaders(t *testing.T) {
	// extraHeaders 不得覆盖认证/内容类型/协议版本头（审查 S-L5）
	req, err := BuildHTTPRequest("https://api.anthropic.com", ProtocolAnthropic,
		RequestParams{Model: "m", Prompt: "p", MaxTokens: 16},
		"sk-anthropic", map[string]string{
			"x-api-key":         "evil",
			"Content-Type":      "text/plain",
			"anthropic-version": "1900-01-01",
			"X-Custom":          "ok",
		})
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("x-api-key"); got != "sk-anthropic" {
		t.Fatalf("认证头被附加头覆盖: %q", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type 被覆盖: %q", got)
	}
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version 被覆盖: %q", got)
	}
	if got := req.Header.Get("X-Custom"); got != "ok" {
		t.Fatalf("非敏感附加头应生效: %q", got)
	}
}
