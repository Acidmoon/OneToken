package provider

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		set  func(h http.Header)
		want time.Duration
		ok   bool
	}{
		{"Retry-After 秒", func(h http.Header) { h.Set("Retry-After", "30") }, 30 * time.Second, true},
		{"Retry-After 0", func(h http.Header) { h.Set("Retry-After", "0") }, 0, true},
		{"retry-after-ms 毫秒", func(h http.Header) { h.Set("retry-after-ms", "1500") }, 1500 * time.Millisecond, true},
		{"retry-after-ms 优先于秒", func(h http.Header) { h.Set("Retry-After", "60"); h.Set("retry-after-ms", "250") }, 250 * time.Millisecond, true},
		{"HTTP-date", func(h http.Header) { h.Set("Retry-After", time.Now().Add(5*time.Second).UTC().Format(http.TimeFormat)) }, 5 * time.Second, true},
		{"无头", func(h http.Header) {}, 0, false},
		{"非法数字", func(h http.Header) { h.Set("Retry-After", "abc") }, 0, false},
		{"负数秒", func(h http.Header) { h.Set("Retry-After", "-5") }, 0, false},
		{"负毫秒", func(h http.Header) { h.Set("retry-after-ms", "-1") }, 0, false},
		{"非法毫秒", func(h http.Header) { h.Set("retry-after-ms", "x") }, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			c.set(h)
			got, ok := ParseRetryAfter(h)
			if ok != c.ok {
				t.Fatalf("ok=%v，期望 %v", ok, c.ok)
			}
			if ok {
				// HTTP-date 允许 ±2s 时钟容差
				diff := got - c.want
				if diff < -2*time.Second || diff > 2*time.Second {
					t.Fatalf("等待 %v，期望约 %v", got, c.want)
				}
			}
		})
	}
}

func TestIsRetryableStatus(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{429, true}, {500, true}, {502, true}, {503, true}, {504, true},
		{200, false}, {400, false}, {401, false}, {403, false}, {404, false}, {405, false},
	}
	for _, c := range cases {
		if got := isRetryableStatus(c.code); got != c.want {
			t.Errorf("isRetryableStatus(%d)=%v，期望 %v", c.code, got, c.want)
		}
	}
}

func TestIsRetryableErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"429 可重试", &HTTPError{StatusCode: 429}, true},
		{"500 可重试", &HTTPError{StatusCode: 500}, true},
		{"4xx 不重试", &HTTPError{StatusCode: 404}, false},
		{"密钥拒绝不重试", ErrKeyRejected, false},
		{"请求体错误不重试", ErrBadRequest, false},
		{"响应体护栏不重试", ErrResponseTooLarge, false},
		{"completion 护栏不重试", ErrCompletionTooLong, false},
		{"SSRF 不重试", ErrSSRFBlocked, false},
		{"预算不重试", ErrBudgetExceeded, false},
		{"空密钥不重试", ErrEmptyAPIKey, false},
		{"未知协议不重试", ErrUnknownProtocol, false},
		{"ctx 取消不重试", context.Canceled, false},
		{"ctx 超时不重试", context.DeadlineExceeded, false},
		{"url.Error 包 ctx 取消不重试", &url.Error{Err: context.Canceled}, false},
		{"网络瞬态可重试", errors.New("net/http: connection reset"), true},
	}
	for _, c := range cases {
		if got := isRetryableErr(c.err); got != c.want {
			t.Errorf("%s: got %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestRetryDelay(t *testing.T) {
	base, max := time.Second, 8*time.Second

	t.Run("Retry-After 优先", func(t *testing.T) {
		err := &HTTPError{StatusCode: 429, RetryAfter: 2 * time.Second, HasRetryAfter: true}
		if got := retryDelay(0, err, base, max); got != 2*time.Second {
			t.Fatalf("期望尊重 2s，实际 %v", got)
		}
	})
	t.Run("Retry-After 封顶", func(t *testing.T) {
		err := &HTTPError{StatusCode: 429, RetryAfter: time.Hour, HasRetryAfter: true}
		if got := retryDelay(0, err, base, max); got != max {
			t.Fatalf("期望封顶 %v，实际 %v", max, got)
		}
	})
	t.Run("指数退避", func(t *testing.T) {
		d0 := retryDelay(0, nil, base, max)
		if d0 < base || d0 >= base+base {
			t.Fatalf("attempt0 期望 [%v,%v)，实际 %v", base, base+base, d0)
		}
		d1 := retryDelay(1, nil, base, max)
		if d1 < 2*base || d1 >= 2*base+base {
			t.Fatalf("attempt1 期望 [%v,%v)，实际 %v", 2*base, 2*base+base, d1)
		}
	})
	t.Run("封顶", func(t *testing.T) {
		for i := 0; i < 200; i++ {
			if got := retryDelay(10, nil, base, max); got > max {
				t.Fatalf("整体封顶后不得超过 %v，实际 %v", max, got)
			}
		}
	})
	t.Run("jitter 确定性范围", func(t *testing.T) {
		for i := 0; i < 100; i++ {
			d := retryDelay(2, nil, base, max)
			if d < 4*base || d > 4*base+base {
				t.Fatalf("越界: %v", d)
			}
		}
	})
}
