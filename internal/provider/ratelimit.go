// per-provider 限流预算（设计 §10.1-4、§6.1 limits）：
// RPM（每分钟）匀速令牌桶 + RPD（每日）UTC 日窗口，0 = 不限。
// 并发上限 MaxConcurrency 由 M2.3 collector 的 worker pool 使用（Limits 已配置）。
//
// 时钟可注入（now/sleep），便于确定性单测。
package provider

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrRateLimited 限流等待被 ctx 中断/超时。
var ErrRateLimited = errors.New("provider: 限流等待超时")

// RateLimiter 是 per-provider 请求预算（RPM/RPD，并发安全）。
type RateLimiter struct {
	mu sync.Mutex

	rpm float64 // tokens/分钟；0 = 不限
	rpd float64 // tokens/日；0 = 不限

	rpmTok float64   // RPM 桶当前余额（cap = rpm）
	rpmRef time.Time // RPM 桶上次补充时刻
	rpdTok float64   // RPD 桶当前余额（cap = rpd）
	rpdRef time.Time // RPD 桶上次刷新时刻（UTC 日切换点）

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// NewRateLimiter 创建限流器（rpm/rpd <= 0 表示不限）。
func NewRateLimiter(rpm, rpd int) *RateLimiter {
	now := time.Now().UTC()
	l := &RateLimiter{
		rpm:    float64(rpm),
		rpd:    float64(rpd),
		rpmTok: float64(rpm),
		rpmRef: now,
		rpdTok: float64(rpd),
		rpdRef: now,
		now:    func() time.Time { return time.Now().UTC() },
		sleep:  sleepCtx,
	}
	return l
}

// sleepCtx 等待 d 或 ctx 取消（默认实现）。
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Wait 阻塞至获得一个请求额度（或 ctx 取消/超时）。
func (l *RateLimiter) Wait(ctx context.Context) error {
	if l.rpm <= 0 && l.rpd <= 0 {
		return nil
	}
	for {
		l.mu.Lock()
		wait := l.acquireLocked()
		l.mu.Unlock()
		if wait <= 0 {
			return nil
		}
		if err := l.sleep(ctx, wait); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrRateLimited
		}
	}
}

// acquireLocked 在持锁下补充余额并尝试消耗；返回需等待时长（<=0 表示已消耗）。
func (l *RateLimiter) acquireLocked() time.Duration {
	now := l.now()
	// RPM：按流逝时间匀速补充，cap 到 rpm（突发上限 = rpm，防瞬时打满）。
	if l.rpm > 0 {
		if el := now.Sub(l.rpmRef); el > 0 {
			l.rpmTok += l.rpm * el.Minutes()
			if l.rpmTok > l.rpm {
				l.rpmTok = l.rpm
			}
			l.rpmRef = now
		}
	}
	// RPD：UTC 日切换即重置。
	if l.rpd > 0 {
		if !sameUTCDay(now, l.rpdRef) {
			l.rpdTok = l.rpd
			l.rpdRef = now
		}
	}

	var wait time.Duration
	if l.rpm > 0 && l.rpmTok < 1 {
		wait = time.Duration((1 - l.rpmTok) / l.rpm * 60 * float64(time.Second))
	}
	if l.rpd > 0 && l.rpdTok < 1 {
		if rpdWait := nextUTCDayStart(now).Sub(now); rpdWait > wait {
			wait = rpdWait
		}
	}
	if wait > 0 {
		return wait
	}
	if l.rpm > 0 {
		l.rpmTok -= 1
	}
	if l.rpd > 0 {
		l.rpdTok -= 1
	}
	return 0
}

// sameUTCDay 判断两个时刻是否同一 UTC 日。
func sameUTCDay(a, b time.Time) bool {
	ay, am, ad := a.UTC().Date()
	by, bm, bd := b.UTC().Date()
	return ay == by && am == bm && ad == bd
}

// nextUTCDayStart 返回下一 UTC 日零点。
func nextUTCDayStart(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}
