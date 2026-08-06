// 成本护栏与传输层错误定义（设计 §10.1-3、§2.1）。
//
// 传输级护栏：响应体字节上限（防端点忽略 max_tokens 疯狂输出导致
// 带宽/内存/账单爆炸）、completion 长度上限（端点忽略 max_tokens 的
// 确定性信号，>阈值即报错，上层识别后标记 hidden-reasoning 并中止 cell）。
//
// 审计级预算：Budget 供采集/审计循环记账（CostUSD 来自 ResponseRecord），
// 超限即中止并记 inconclusive（M2.3 collector 使用）。
package provider

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// 传输层错误（M2.2）。上层按 errors.Is 分类：重试 or 终止。
var (
	// ErrRetryExhausted 重试耗尽（含最后一次错误详情）。
	ErrRetryExhausted = errors.New("provider: 重试耗尽")
	// ErrResponseTooLarge 响应体字节超限（成本护栏①）。
	ErrResponseTooLarge = errors.New("provider: 响应体超限（成本护栏）")
	// ErrCompletionTooLong completion 长度超限（成本护栏②，端点忽略 max_tokens）。
	ErrCompletionTooLong = errors.New("provider: completion 长度超限（成本护栏）")
	// ErrBudgetExceeded 审计预算超限（记 inconclusive 的触发条件）。
	ErrBudgetExceeded = errors.New("provider: 审计预算超限")
	// ErrBadResponse 响应体解析失败（200 但非 JSON / 结构不符），确定性错误不重试。
	ErrBadResponse = errors.New("provider: 响应解析失败（确定性错误，不重试）")
)

// HTTPError 携带 HTTP 状态码与限量响应体片段的可重试错误。
// 仅 429 / 5xx 走重试矩阵；4xx（除 401/403/400 已收敛为语义错误）不重试。
type HTTPError struct {
	StatusCode int
	Body       string // 脱敏截断（≤512B）的错误体片段；响应体不含请求密钥
	// RetryAfter 尊重服务器退避（429 的 Retry-After 秒 / retry-after-ms 毫秒）。
	RetryAfter    time.Duration
	HasRetryAfter bool
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("provider: HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("provider: HTTP %d: %s", e.StatusCode, e.Body)
}

// ---- 审计预算 ----

// Budget 是审计级成本/调用预算（设计 §10.1-3：单次审计总 token/总成本预算，
// 超预算立即中止并记 inconclusive）。由调用方（collector）持有并在每次
// Complete 后记账；非零字段才生效（0 = 不限）。
type Budget struct {
	mu        sync.Mutex
	maxUSD    float64
	maxCalls  int
	usedUSD   float64
	usedCalls int
}

// NewBudget 创建预算（maxUSD<=0 或 maxCalls<=0 表示对应维度不限）。
func NewBudget(maxUSD float64, maxCalls int) *Budget {
	return &Budget{maxUSD: maxUSD, maxCalls: maxCalls}
}

// Spend 记账一次响应成本；超限返回 ErrBudgetExceeded（记录仍保留，
// 供上层如实报告用量）。
func (b *Budget) Spend(costUSD float64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.usedCalls++
	b.usedUSD += costUSD
	if (b.maxCalls > 0 && b.usedCalls > b.maxCalls) ||
		(b.maxUSD > 0 && b.usedUSD > b.maxUSD) {
		return fmt.Errorf("%w: 已用 %d 次 / $%.4f，上限 %d 次 / $%.4f",
			ErrBudgetExceeded, b.usedCalls, b.usedUSD, b.maxCalls, b.maxUSD)
	}
	return nil
}

// Used 返回当前用量（美元，调用次数）。
func (b *Budget) Used() (float64, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.usedUSD, b.usedCalls
}
