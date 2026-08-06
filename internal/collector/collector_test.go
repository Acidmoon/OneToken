package collector

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"onetoken/internal/battery"
	"onetoken/internal/provider"
	"onetoken/internal/store"
)

// mockProvider 是 provider.Provider 的桩：记录调用序列、模拟延迟/失败/ctx 取消。
type mockProvider struct {
	mu    sync.Mutex
	calls []string // 按完成顺序记录的 prompt（任务执行顺序证据）

	delay    time.Duration
	ctxAware bool             // 尊重 ctx（模拟慢请求被取消）
	failOn   map[string]error // prompt → 错误（模拟特定任务失败）

	inFlight atomic.Int64
	peak     atomic.Int64
}

func (m *mockProvider) Complete(ctx context.Context, rp provider.RequestParams) (*provider.ResponseRecord, error) {
	cur := m.inFlight.Add(1)
	defer m.inFlight.Add(-1)
	// 并发峰值记录
	for {
		p := m.peak.Load()
		if cur <= p || m.peak.CompareAndSwap(p, cur) {
			break
		}
	}
	if m.delay > 0 || m.ctxAware {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(m.delay):
		}
	}
	m.mu.Lock()
	m.calls = append(m.calls, rp.Prompt)
	m.mu.Unlock()
	if err, ok := m.failOn[rp.Prompt]; ok {
		return nil, err
	}
	raw := `{"choices":[{"message":{"content":"42"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`
	return &provider.ResponseRecord{
		Protocol: "chat", Text: "42", RawCompletion: raw, FinishReason: "stop",
		CompletionTokens: 1, PromptTokens: 5, TS: time.Now().UTC(),
	}, nil
}

func (m *mockProvider) callsSnapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

func testBattery(t *testing.T) *battery.Battery {
	t.Helper()
	b, err := battery.Load(filepath.Join("..", "..", "config", "prompts.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	return store.New(t.TempDir())
}

func baseOpts(id string) Options {
	return Options{ID: id, Model: "test-model"}
}

// ---- 基本采集与入库 ----

func TestRunBatteryBasic(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	cells := b.Cells()[:2] // 2 cell
	mp := &mockProvider{}

	results, err := RunBattery(context.Background(), mp, s, b, cells, 3, 1.0, baseOpts("basic"))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 6 {
		t.Fatalf("结果数=%d，期望 6（2 cell × 3）", len(results))
	}
	rs, err := s.LoadResponses("basic")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 6 {
		t.Fatalf("入库数=%d，期望 6", len(rs))
	}
	// 证据链：raw_sha256 必填；无重复幂等键；样本索引覆盖 0..n-1
	seen := map[string]bool{}
	for _, r := range rs {
		if r.RawSHA256 == "" {
			t.Fatalf("响应缺少 raw_sha256: %+v", r)
		}
		k := store.ResponseKey(r.Cell, r.SampleIdx)
		if seen[k] {
			t.Fatalf("重复幂等键 %q", k)
		}
		seen[k] = true
		if r.Temperature != 1.0 || r.RawCompletion == "" || r.TS == "" {
			t.Fatalf("字段缺失: %+v", r)
		}
	}
	if len(seen) != 6 {
		t.Fatalf("幂等键覆盖=%d，期望 6", len(seen))
	}
}

// ---- 崩溃续采不重复入库（验收项：模拟中断） ----

func TestRunBatteryIdempotentResume(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	cells := b.Cells()[:1] // 1 cell
	const total = 5

	// 第一次：ctx 在完成 2 个后被外部取消（模拟崩溃/中断）。
	// 串行 worker：done==2 后第 3 个任务必然被取消，结果稳定。
	ctx, cancel := context.WithCancel(context.Background())
	mp := &mockProvider{delay: 5 * time.Millisecond, ctxAware: true}
	_, err := RunBattery(ctx, mp, s, b, cells, total, 1.0, Options{
		ID: "resume", Model: "m", Concurrency: 1,
		OnProgress: func(done, _ int) {
			if done == 2 {
				cancel() // 中断
			}
		},
	})
	if err == nil {
		t.Fatal("中断应返回错误")
	}
	first, err := s.LoadResponses("resume")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("第一次应恰完成 2 条（done==2 中断），实际 %d", len(first))
	}

	// 第二次：同 ID 续采（无中断）→ 只补缺失样本，不重复入库
	results, err := RunBattery(context.Background(), mp, s, b, cells, total, 1.0, baseOpts("resume"))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != total-len(first) {
		t.Fatalf("续采应只补 %d 条，实际 %d", total-len(first), len(results))
	}
	rs, err := s.LoadResponses("resume")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != total {
		t.Fatalf("总入库=%d，期望 %d（无重复）", len(rs), total)
	}
	// 幂等键无重复
	seen := map[string]bool{}
	for _, r := range rs {
		k := store.ResponseKey(r.Cell, r.SampleIdx)
		if seen[k] {
			t.Fatalf("续采后出现重复键 %q", k)
		}
		seen[k] = true
	}
	// 样本索引完整覆盖 0..n-1
	for i := 0; i < total; i++ {
		if !seen[store.ResponseKey(cells[0], i)] {
			t.Fatalf("缺样本 %d", i)
		}
	}
}

// ---- 种子可复现 + 打乱生效 ----

func TestRunBatterySeedDeterminism(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	cells := b.Cells() // 全部 40

	run := func(id string, seed int64) *mockProvider {
		mp := &mockProvider{}
		if _, err := RunBattery(context.Background(), mp, s, b, cells, 1, 1.0, Options{
			ID: id, Model: "m", Seed: seed, Concurrency: 1,
		}); err != nil {
			t.Fatal(err)
		}
		return mp
	}

	seqA1 := run("seed-a1", 42).callsSnapshot()
	seqA2 := run("seed-a2", 42).callsSnapshot()
	if len(seqA1) != 40 || len(seqA2) != 40 {
		t.Fatalf("应采集 40 条，实际 %d/%d", len(seqA1), len(seqA2))
	}
	for i := range seqA1 {
		if seqA1[i] != seqA2[i] {
			t.Fatalf("同种子执行顺序不一致（第 %d 项）", i)
		}
	}
	seqB := run("seed-b", 43).callsSnapshot()
	diff := 0
	for i := range seqA1 {
		if seqA1[i] != seqB[i] {
			diff++
		}
	}
	if diff == 0 {
		t.Fatal("不同种子应产生不同打乱顺序")
	}
}

// ---- 进度回调 + 进度/结果流分离（验收项） ----

func TestRunBatteryProgressAndNoStdout(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)

	// 结果流分离：collector 不得写 stdout（进度走回调，结果由返回值承载）
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()

	var mu sync.Mutex
	var progress []int
	_, err = RunBattery(context.Background(), &mockProvider{}, s, b, b.Cells()[:2], 3, 1.0, Options{
		ID: "stream", Model: "m", Concurrency: 2,
		OnProgress: func(done, total int) {
			mu.Lock()
			progress = append(progress, done)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = old
	w.Close()
	buf := make([]byte, 1024)
	n, _ := r.Read(buf)
	r.Close()
	if n > 0 {
		t.Fatalf("collector 不应写 stdout，实际 %q", buf[:n])
	}

	// 进度回调：done 单调递增且最终 == total（6）
	mu.Lock()
	defer mu.Unlock()
	if len(progress) != 6 {
		t.Fatalf("进度回调次数=%d，期望 6", len(progress))
	}
	for i := 1; i < len(progress); i++ {
		if progress[i] <= progress[i-1] {
			t.Fatalf("进度应单调递增: %v", progress)
		}
	}
	if progress[len(progress)-1] != 6 {
		t.Fatalf("最终进度=%d，期望 6", progress[len(progress)-1])
	}
}

// ---- 并发上限 ----

func TestRunBatteryConcurrencyCap(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	mp := &mockProvider{delay: 10 * time.Millisecond}

	_, err := RunBattery(context.Background(), mp, s, b, b.Cells()[:3], 4, 1.0, Options{
		ID: "conc", Model: "m", Concurrency: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	peak := mp.peak.Load()
	if peak > 3 {
		t.Fatalf("并发峰值=%d 超过上限 3", peak)
	}
	if peak < 2 {
		t.Fatalf("并发峰值=%d 过低（应真实并发）", peak)
	}
}

// ---- 预算超限中止 ----

func TestRunBatteryBudgetExceeded(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	budget := provider.NewBudget(0, 2) // 最多 2 次调用

	results, err := RunBattery(context.Background(), &mockProvider{}, s, b, b.Cells()[:1], 4, 1.0, Options{
		ID: "budget", Model: "m", Concurrency: 1, Budget: budget,
	})
	if !errors.Is(err, provider.ErrBudgetExceeded) {
		t.Fatalf("应报 ErrBudgetExceeded，实际 %v", err)
	}
	// 超限的那次调用成本已花、响应已入库（如实保留），其后任务中止
	if len(results) != 3 {
		t.Fatalf("前 3 次调用应完成并保留，实际 %d", len(results))
	}
	rs, _ := s.LoadResponses("budget")
	if len(rs) != 3 {
		t.Fatalf("入库应 3 条，实际 %d", len(rs))
	}
}

// ---- 任务失败容忍（其余任务继续） ----

func TestRunBatteryErrorCollect(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	cells := b.Cells()[:2]
	badPrompt, _ := b.Prompt(cells[0][:len(cells[0])-3], "en")
	mp := &mockProvider{failOn: map[string]error{badPrompt: errors.New("boom")}}

	results, err := RunBattery(context.Background(), mp, s, b, cells, 2, 1.0, baseOpts("errc"))
	if err == nil {
		t.Fatal("部分失败应返回聚合错误")
	}
	if len(results) != 2 {
		t.Fatalf("失败 cell 的 2 任务应跳过，成功 cell 的 2 任务应完成，实际 %d", len(results))
	}
	var te *TaskError
	if !errors.As(err, &te) {
		t.Fatalf("应可定位 TaskError，实际 %v", err)
	}
	if te.Cell != cells[0] {
		t.Fatalf("TaskError.Cell=%q，期望 %q", te.Cell, cells[0])
	}
}

// ---- ctx 预取消 / 参数校验 ----

func TestRunBatteryCtxCancel(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := RunBattery(ctx, &mockProvider{}, s, b, b.Cells()[:1], 3, 1.0, baseOpts("ctx"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("预取消应返回 context.Canceled，实际 %v", err)
	}
}

func TestRunBatteryValidation(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	mp := &mockProvider{}
	cells := b.Cells()[:1]

	cases := []struct {
		name string
		opts Options
		n    int
	}{
		{"n=0", baseOpts("v1"), 0},
		{"ID 空", Options{Model: "m"}, 1},
		{"Model 空", Options{ID: "v2"}, 1},
	}
	for _, c := range cases {
		if _, err := RunBattery(context.Background(), mp, s, b, cells, c.n, 1.0, c.opts); err == nil {
			t.Errorf("%s: 应报参数错误", c.name)
		}
	}
	if _, err := RunBattery(context.Background(), mp, s, b, nil, 1, 1.0, baseOpts("v3")); err == nil {
		t.Error("空 cells 应报错")
	}
}

// ---- 非法 cell 格式 ----

func TestRunBatteryInvalidCell(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	_, err := RunBattery(context.Background(), &mockProvider{}, s, b, []string{"no-colon"}, 1, 1.0, baseOpts("badcell"))
	if err == nil {
		t.Fatal("非法 cell 应报错")
	}
	if !strings.Contains(err.Error(), "no-colon") {
		t.Fatalf("错误应含 cell 原文: %v", err)
	}
}

// ---- 审查回归：参数校验 / 并发 clamp / 根因保留 / panic 恢复 / seed 确定性 ----

func TestRunBatteryNaNInfRejected(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	for _, T := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := RunBattery(context.Background(), &mockProvider{}, s, b, b.Cells()[:1], 1, T, baseOpts("nan")); err == nil {
			t.Errorf("T=%v 应报参数错误", T)
		}
	}
}

func TestRunBatteryCellDupRejected(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	c := b.Cells()[0]
	_, err := RunBattery(context.Background(), &mockProvider{}, s, b, []string{c, c}, 1, 1.0, baseOpts("dup"))
	if err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复 cell 应报错，实际 %v", err)
	}
}

func TestRunBatteryConcurrencyClamp(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	mp := &mockProvider{}
	// 超大并发不 OOM（clamp 到 256），峰值不超过硬上限
	_, err := RunBattery(context.Background(), mp, s, b, b.Cells()[:10], 1, 1.0, Options{
		ID: "clamp", Model: "m", Concurrency: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if peak := mp.peak.Load(); peak > maxConcurrency {
		t.Fatalf("并发峰值=%d 超过硬上限 %d", peak, maxConcurrency)
	}
}

func TestRunBatterySeedZeroDeterministic(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	run := func(id string) []string {
		mp := &mockProvider{}
		if _, err := RunBattery(context.Background(), mp, s, b, b.Cells()[:5], 1, 1.0, Options{
			ID: id, Model: "m", Seed: 0, Concurrency: 1,
		}); err != nil {
			t.Fatal(err)
		}
		return mp.callsSnapshot()
	}
	a1, a2 := run("seed0-a"), run("seed0-b")
	if len(a1) != 5 || len(a2) != 5 {
		t.Fatalf("采集数异常: %d/%d", len(a1), len(a2))
	}
	for i := range a1 {
		if a1[i] != a2[i] {
			t.Fatalf("seed=0 应确定性（两次顺序不一致，第 %d 项）", i)
		}
	}
}

// panicProvider 模拟 Provider 实现 panic（worker recover 应兜住）。
type panicProvider struct{}

func (*panicProvider) Complete(context.Context, provider.RequestParams) (*provider.ResponseRecord, error) {
	panic("boom")
}

func TestRunBatteryWorkerPanicRecover(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	_, err := RunBattery(context.Background(), &panicProvider{}, s, b, b.Cells()[:1], 2, 1.0, baseOpts("panic"))
	// 进程未崩（本测试继续运行即证明 recover 生效）；错误应含 panic 详情
	if err == nil {
		t.Fatal("panic 应转为错误")
	}
	if !strings.Contains(err.Error(), "panic: boom") {
		t.Fatalf("错误应含 panic 根因: %v", err)
	}
}

func TestRunBatteryAppendFailurePreservesRootCause(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 不受目录权限限制")
	}
	dir := t.TempDir()
	s := store.New(dir)
	b := testBattery(t)
	cells := b.Cells()[:1]

	// 第一次成功：创建 responses 目录并入库 2 条
	if _, err := RunBattery(context.Background(), &mockProvider{}, s, b, cells, 2, 1.0, baseOpts("ro")); err != nil {
		t.Fatal(err)
	}
	// 响应文件改只读 → 续采新样本时 AppendResponse 打开失败（目录写权限不影响已存在文件）
	respFile := filepath.Join(dir, "responses", "ro.jsonl")
	if err := os.Chmod(respFile, 0o444); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(respFile, 0o644)

	_, err := RunBattery(context.Background(), &mockProvider{}, s, b, cells, 4, 1.0, baseOpts("ro"))
	if err == nil {
		t.Fatal("入库失败应报错")
	}
	if !strings.Contains(err.Error(), "入库失败") {
		t.Fatalf("根因应保留（入库失败），实际 %v", err)
	}
}

// ---- 全部已完成时幂等返回 ----

func TestRunBatteryAllDone(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	cells := b.Cells()[:1]
	if _, err := RunBattery(context.Background(), &mockProvider{}, s, b, cells, 2, 1.0, baseOpts("done")); err != nil {
		t.Fatal(err)
	}
	mp := &mockProvider{}
	results, err := RunBattery(context.Background(), mp, s, b, cells, 2, 1.0, baseOpts("done"))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("全部完成后应返回空结果，实际 %d", len(results))
	}
	if len(mp.callsSnapshot()) != 0 {
		t.Fatal("全部完成后不应再发请求")
	}
}

// ---- 审查回归：CountTaskFailures（unreachable 判定生产者） ----

func TestCountTaskFailures(t *testing.T) {
	e1 := &TaskError{Cell: "a", SampleIdx: 0, Err: errors.New("x")}
	e2 := &TaskError{Cell: "b", SampleIdx: 1, Err: errors.New("y")}
	plain := errors.New("plain")

	if n := CountTaskFailures(nil); n != 0 {
		t.Fatalf("nil 应为 0，实际 %d", n)
	}
	if n := CountTaskFailures(plain); n != 0 {
		t.Fatalf("普通错误应为 0，实际 %d", n)
	}
	if n := CountTaskFailures(e1); n != 1 {
		t.Fatalf("单 TaskError 应为 1，实际 %d", n)
	}
	// errors.Join 展平计数（含嵌套与普通错误混入；e1 两次出现去重）
	joined := errors.Join(e1, e2, plain, errors.Join(e1, errors.New("nested")))
	if n := CountTaskFailures(joined); n != 2 {
		t.Fatalf("Join 聚合应计 2 个唯一 TaskError（e1 去重），实际 %d", n)
	}
	// fmt.Errorf 包装的 TaskError 也应识别
	wrapped := fmt.Errorf("outer: %w", e1)
	if n := CountTaskFailures(wrapped); n != 1 {
		t.Fatalf("包装 TaskError 应计 1，实际 %d", n)
	}
}
