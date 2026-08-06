package provider

import (
	"errors"
	"testing"
)

func TestBudgetUSD(t *testing.T) {
	b := NewBudget(1.0, 0) // 仅美元维度
	if err := b.Spend(0.4); err != nil {
		t.Fatalf("0.4 应在预算内: %v", err)
	}
	if err := b.Spend(0.6); err != nil {
		t.Fatalf("累计 1.0 应恰好达标: %v", err)
	}
	if err := b.Spend(0.01); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("超限应报 ErrBudgetExceeded，实际 %v", err)
	}
	usd, calls := b.Used()
	if usd < 1.009 || usd > 1.011 || calls != 3 {
		t.Fatalf("用量应保留如实记录，实际 $%.4f × %d 次", usd, calls)
	}
}

func TestBudgetCalls(t *testing.T) {
	b := NewBudget(0, 2) // 仅调用次数维度
	b.Spend(0)
	b.Spend(0)
	if err := b.Spend(0); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("第 3 次调用应超限，实际 %v", err)
	}
}

func TestBudgetUnlimited(t *testing.T) {
	b := NewBudget(0, 0)
	for i := 0; i < 100; i++ {
		if err := b.Spend(0.5); err != nil {
			t.Fatalf("不限预算不应超限: %v", err)
		}
	}
}

func TestBudgetNegativeCost(t *testing.T) {
	b := NewBudget(1.0, 10)
	if err := b.Spend(-5); err != nil {
		t.Fatalf("负成本不应报错（防御性容忍）: %v", err)
	}
}
