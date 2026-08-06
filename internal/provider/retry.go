// 重试矩阵（设计 §10.1-4）：429 尊重 Retry-After（含 Anthropic 毫秒变体）、
// 4xx 校验错误不重试、5xx 指数退避 + jitter、最大重试次数由调用方（Client）
// 配置、总 deadline 由 ctx 控制。
package provider

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

// ParseRetryAfter 解析限流/退避指示头：
//   - `retry-after-ms`（Anthropic，毫秒）优先；
//   - `Retry-After`（RFC 7231：秒数或 HTTP-date）。
//
// 无头或解析失败返回 (0, false)，调用方回落指数退避。
// 溢出保护（审查 L1）：超大头值（恶意/损坏）clamp 到接近 MaxInt64，
// 防止相乘环绕为负导致退避失效。
func ParseRetryAfter(h http.Header) (time.Duration, bool) {
	const maxMS = int64((1<<63 - 1) / int64(time.Millisecond))
	const maxSec = int64((1<<63 - 1) / int64(time.Second))
	if v := h.Get("retry-after-ms"); v != "" {
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil && ms >= 0 {
			if ms > maxMS {
				ms = maxMS
			}
			return time.Duration(ms) * time.Millisecond, true
		}
	}
	if v := h.Get("Retry-After"); v != "" {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil && sec >= 0 {
			if sec > maxSec {
				sec = maxSec
			}
			return time.Duration(sec) * time.Second, true
		}
		// HTTP-date 形式：按服务器时间计算剩余等待。
		if t, err := http.ParseTime(v); err == nil {
			d := time.Until(t)
			if d < 0 {
				d = 0
			}
			return d, true
		}
	}
	return 0, false
}

// isRetryableStatus 判定状态码是否走重试矩阵（429 / 5xx）。
func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// isRetryableErr 判定错误是否可重试。
//   - HTTPError：仅 429/5xx 可重试（4xx 为校验错误，不重试）；
//   - 语义错误（密钥/请求体/护栏/SSRF/预算）与 ctx 取消：终止；
//   - 其余（网络瞬态：连接重置、超时、DNS 临时失败）：可重试。
func isRetryableErr(err error) bool {
	if err == nil {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return isRetryableStatus(he.StatusCode)
	}
	switch {
	case errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, ErrRetryExhausted),
		errors.Is(err, ErrResponseTooLarge),
		errors.Is(err, ErrCompletionTooLong),
		errors.Is(err, ErrBadResponse),
		errors.Is(err, ErrBudgetExceeded),
		errors.Is(err, ErrSSRFBlocked),
		errors.Is(err, ErrKeyRejected),
		errors.Is(err, ErrBadRequest),
		errors.Is(err, ErrEmptyAPIKey),
		errors.Is(err, ErrUnknownProtocol):
		return false
	default:
		return true // 网络瞬态：重试
	}
}

// 超时重试口径（审查 R-L3）：http.Client.Timeout 触发的单请求超时经 url.Error
// 包裹 context.DeadlineExceeded，与调用方 ctx deadline 同类型——统一判终止
// （保守策略：不自动重试超时，避免慢端点重复计费）；慢端点的恢复由上层
// 在审计级重试/下次会话处理。

// retryDelay 计算第 attempt 次失败后的等待时长：
//   - 服务器显式 Retry-After → 尊重（上限 max，防恶意超长等待）；
//   - 否则指数退避 + jitter：base·2^attempt + [0, base)，封顶 max。
//
// attempt 从 0 起（首次失败）。
func retryDelay(attempt int, err error, base, max time.Duration) time.Duration {
	if max <= 0 {
		max = base
	}
	var he *HTTPError
	if errors.As(err, &he) && he.HasRetryAfter {
		d := he.RetryAfter
		if d < 0 {
			d = 0
		}
		if d > max {
			d = max
		}
		return d
	}
	// 指数：base·2^attempt，封顶 max；jitter：[0, base) 均匀（防惊群）。
	// 整体封顶（审查 L2）：jitter 叠加后不得超过 max（注释/实现/测试一致）。
	shift := attempt
	if shift > 5 { // 2^5=32 倍基数后交给封顶，防溢出
		shift = 5
	}
	exp := base << shift
	if exp > max {
		exp = max
	}
	d := exp + time.Duration(rand.Float64()*float64(base))
	if d > max {
		d = max
	}
	return d
}
