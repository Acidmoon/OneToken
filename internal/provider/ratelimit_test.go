package provider

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeClock 注入 RateLimiter 时钟：sleep 直接推进假时钟，记录每次等待时长。
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	sleeps []time.Duration
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) sleep(_ context.Context, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
	f.sleeps = append(f.sleeps, d)
	return nil
}

func newFakeLimiter(rpm, rpd int) (*RateLimiter, *fakeClock) {
	l := NewRateLimiter(rpm, rpd)
	fc := &fakeClock{t: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)}
	l.now = fc.now
	l.sleep = fc.sleep
	return l, fc
}

func waitOK(t *testing.T, l *RateLimiter) {
	t.Helper()
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait 意外失败: %v", err)
	}
}

func TestRateLimiterUnlimited(t *testing.T) {
	l, fc := newFakeLimiter(0, 0)
	for i := 0; i < 5; i++ {
		waitOK(t, l)
	}
	if len(fc.sleeps) != 0 {
		t.Fatalf("不限流不应等待，实际 sleep %v 次", len(fc.sleeps))
	}
}

func TestRateLimiterRPM(t *testing.T) {
	l, fc := newFakeLimiter(2, 0)

	// 前 2 次立即通过（突发上限 = rpm）
	waitOK(t, l)
	waitOK(t, l)
	if len(fc.sleeps) != 0 {
		t.Fatalf("前 2 次不应等待")
	}

	// 第 3 次：桶空，等待 (1-0)/2*60s = 30s 补充后消耗
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait 失败: %v", err)
	}
	if len(fc.sleeps) != 1 || fc.sleeps[0] != 30*time.Second {
		t.Fatalf("期望等待 30s，实际 %v", fc.sleeps)
	}

	// 第 4 次：上一轮已消耗补充 token，桶空再等 30s（匀速语义）
	waitOK(t, l)
	if len(fc.sleeps) != 2 || fc.sleeps[1] != 30*time.Second {
		t.Fatalf("期望再次等待 30s，实际 %v", fc.sleeps)
	}
}

func TestRateLimiterRPMBurstCap(t *testing.T) {
	// 长时间不调用后，桶余额不超上限（防积压后突发打满）
	l, _ := newFakeLimiter(10, 0)
	for i := 0; i < 10; i++ {
		waitOK(t, l)
	}
	l.mu.Lock()
	if l.rpmTok != 0 {
		t.Fatalf("桶应耗尽，实际 %v", l.rpmTok)
	}
	l.mu.Unlock()
}

func TestRateLimiterRPD(t *testing.T) {
	l, fc := newFakeLimiter(0, 2)

	waitOK(t, l)
	waitOK(t, l)
	if len(fc.sleeps) != 0 {
		t.Fatalf("前 2 次不应等待")
	}

	// 第 3 次：RPD 桶空，等待至次日 UTC 0 点（12:00 → 12h）
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait 失败: %v", err)
	}
	if len(fc.sleeps) != 1 || fc.sleeps[0] != 12*time.Hour {
		t.Fatalf("期望等待 12h，实际 %v", fc.sleeps)
	}

	// 跨日重置后可用
	waitOK(t, l)
	if len(fc.sleeps) != 1 {
		t.Fatalf("跨日重置后不应再等待")
	}
}

func TestRateLimiterRPDPartialDay(t *testing.T) {
	// 构造在 UTC 日末：次日零点重置后配额恢复
	l := NewRateLimiter(0, 5)
	fc := &fakeClock{t: time.Date(2026, 8, 6, 23, 59, 0, 0, time.UTC)}
	l.now = fc.now
	l.sleep = fc.sleep

	for i := 0; i < 5; i++ {
		waitOK(t, l)
	}
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait 失败: %v", err)
	}
	if len(fc.sleeps) != 1 || fc.sleeps[0] != time.Minute {
		t.Fatalf("期望等待 1min 到次日零点，实际 %v", fc.sleeps)
	}
	// 跨日后：重置为 5，立即通过
	waitOK(t, l)
	if len(fc.sleeps) != 1 {
		t.Fatalf("跨日重置后不应再等待")
	}
}

func TestRateLimiterCtxCancel(t *testing.T) {
	// 真实时钟：ctx 超时先于 60s 退避到达，验证 Wait 尊重 ctx。
	l := NewRateLimiter(1, 0)
	waitOK(t, l) // 耗尽

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx); err == nil {
		t.Fatal("桶空 + ctx 超时应返回错误")
	}
}
