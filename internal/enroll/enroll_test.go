package enroll

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"onetoken/internal/battery"
	"onetoken/internal/config"
	"onetoken/internal/provider"
	"onetoken/internal/store"
)

// mockProvider 是 provider.Provider 桩：T=1.0 三值轮换（方差，防缓存误报），
// T=0 固定答案；failAll 模拟端点不可达；reasoning 模拟推理痕迹。
type mockProvider struct {
	counter   atomic.Int64
	failAll   bool
	t0Fail    bool // 仅 T=0 段失败（T1 正常）
	reasoning bool
}

var t1Answers = []string{"42", "57", "88"}

func (m *mockProvider) Complete(_ context.Context, rp provider.RequestParams) (*provider.ResponseRecord, error) {
	if m.failAll || (m.t0Fail && rp.Temperature == 0) {
		return nil, errors.New("mock: endpoint unreachable")
	}
	raw := "42"
	if rp.Temperature == 0 {
		raw = "42" // T=0 确定性（探针样本不足不参与判定，无碍）
	} else {
		raw = t1Answers[m.counter.Add(1)%int64(len(t1Answers))]
	}
	body := fmt.Sprintf(`{"choices":[{"message":{"content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`, raw)
	reasoning := 0
	if m.reasoning {
		reasoning = 42
	}
	return &provider.ResponseRecord{
		Protocol: "chat", Text: raw, RawCompletion: body, FinishReason: "stop",
		ReasoningTokens: reasoning, CompletionTokens: 1, PromptTokens: 5,
		TS: time.Now().UTC(),
	}, nil
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

// testSettings 采集参数调小（EnrollNT1=10 满足 MinValidSamples；MinValidSamples
// 调 2 使 3 值答案覆盖的 cell 计数，KMinCells=1 放宽建档门）。
func testSettings() config.Settings {
	s := config.DefaultSettings()
	s.EnrollNT1 = 10
	s.EnrollNT0 = 3
	s.MinValidSamples = 2
	s.KMinCells = 1
	return s
}

func baseOpts(b *battery.Battery, s *store.Store, version string) Options {
	return Options{
		Settings:     testSettings(),
		Provider:     &mockProvider{},
		Store:        s,
		Battery:      b,
		ModelID:      "qwen/qwen3-8b",
		Vendor:       "zhipu",
		Family:       "qwen",
		ModelType:    "open-source",
		Version:      version,
		ProviderName: "zhipu",
	}
}

// ---- 验收：enroll 版本化（UNIQUE(model_id, version)） ----

func TestEnrollVersioned(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)

	fp1, err := Enroll(context.Background(), baseOpts(b, s, "v1"))
	if err != nil {
		t.Fatal(err)
	}
	if fp1.Version != "v1" || fp1.RefSource != "official-api" {
		t.Fatalf("指纹元数据错误: %+v", fp1)
	}
	if len(fp1.Cells) < 3 || len(fp1.T0Cells) < 3 {
		t.Fatalf("指纹应含足够 cell（Cells=%d T0Cells=%d）", len(fp1.Cells), len(fp1.T0Cells))
	}

	// 同 version 重跑 → 版本冲突
	if _, err := Enroll(context.Background(), baseOpts(b, s, "v1")); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("同版本应冲突，实际 %v", err)
	}

	// 新 version → 覆盖成功（版本化演进）
	fp2, err := Enroll(context.Background(), baseOpts(b, s, "v2"))
	if err != nil {
		t.Fatal(err)
	}
	if fp2.Version != "v2" {
		t.Fatalf("v2 应覆盖，实际 %s", fp2.Version)
	}
	loaded, err := s.LoadFingerprint("qwen/qwen3-8b")
	if err != nil || loaded == nil {
		t.Fatalf("指纹应可读: %v", err)
	}
	if loaded.Version != "v2" {
		t.Fatalf("落盘指纹版本=%s，期望 v2", loaded.Version)
	}
}

// ---- 指纹内容与模型登记 ----

func TestEnrollFingerprintContent(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	opts := baseOpts(b, s, "v1")
	opts.OnProgress = func(phase string, done, total int) {
		// 进度回调被调用（两段）
		if done > total {
			t.Fatalf("进度越界: %d/%d", done, total)
		}
	}
	fp, err := Enroll(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	// RefSource/来源标注
	if fp.RefSource != "official-api" {
		t.Fatalf("RefSource=%q，期望 official-api", fp.RefSource)
	}
	// T0 变体入指纹
	if len(fp.T0Cells) == 0 {
		t.Fatal("T0Cells 应为空（EnrollNT0>0）")
	}
	// models.json 登记
	models, err := s.LoadModels()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range models {
		if m.ID == "qwen/qwen3-8b" {
			found = true
			if m.RefSource != "official-api" || m.Family != "qwen" {
				t.Fatalf("模型登记错误: %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("模型未登记")
	}
	// 幂等：再 Enroll 同模型不同版本 → models 不重复登记
	if _, err := Enroll(context.Background(), baseOpts(b, s, "v2")); err != nil {
		t.Fatal(err)
	}
	models, _ = s.LoadModels()
	count := 0
	for _, m := range models {
		if m.ID == "qwen/qwen3-8b" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("模型应唯一登记，实际 %d 条", count)
	}
}

// ---- 测量有效性门 ----

func TestEnrollMeasurementFlag(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	opts := baseOpts(b, s, "v1")
	// 推理痕迹：mock 返回 ReasoningTokens>0 → hidden-reasoning → 建档拒绝
	opts.Provider = &mockProvider{reasoning: true}
	if _, err := Enroll(context.Background(), opts); err == nil {
		t.Fatal("参考源含推理痕迹应拒绝建档")
	}
}

func TestEnrollUnreachable(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	opts := baseOpts(b, s, "v1")
	opts.Provider = &mockProvider{failAll: true}
	if _, err := Enroll(context.Background(), opts); err == nil {
		t.Fatal("参考源不可达应拒绝建档")
	}
}

// ---- 参数校验 ----

func TestEnrollValidation(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	opts := baseOpts(b, s, "v1")
	opts.ModelID = ""
	if _, err := Enroll(context.Background(), opts); err == nil {
		t.Fatal("ModelID 空应报错")
	}
	opts = baseOpts(b, s, "v1")
	opts.ProviderName = ""
	if _, err := Enroll(context.Background(), opts); err == nil {
		t.Fatal("ProviderName 空应报错")
	}
	opts = baseOpts(b, s, "v1")
	opts.Battery = nil
	if _, err := Enroll(context.Background(), opts); err == nil {
		t.Fatal("Battery nil 应报错")
	}
}

// ---- 审查回归：SupersededBy 版本链、T0 段失败、参数联动校验 ----

func TestEnrollSupersededChain(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	if _, err := Enroll(context.Background(), baseOpts(b, s, "v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := Enroll(context.Background(), baseOpts(b, s, "v2")); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadFingerprint("qwen/qwen3-8b")
	if err != nil || loaded == nil {
		t.Fatalf("指纹应可读: %v", err)
	}
	if loaded.SupersededBy != "v1" {
		t.Fatalf("SupersededBy=%q，期望 v1（版本链留痕）", loaded.SupersededBy)
	}
	if loaded.Provider != "zhipu" {
		t.Fatalf("指纹 Provider=%q，期望 zhipu（结构化标注）", loaded.Provider)
	}
}

func TestEnrollT0SegmentFailure(t *testing.T) {
	// T=0 段全失败 → unreachable 必须触发（不静默建档空 T0Cells）
	b := testBattery(t)
	s := testStore(t)
	opts := baseOpts(b, s, "v1")
	opts.Provider = &mockProvider{t0Fail: true}
	if _, err := Enroll(context.Background(), opts); err == nil {
		t.Fatal("T=0 段全失败应拒绝建档（unreachable）")
	}
}

func TestEnrollSampleParamGuard(t *testing.T) {
	b := testBattery(t)
	s := testStore(t)
	opts := baseOpts(b, s, "v1")
	opts.Settings.EnrollNT1 = 1 // < MinValidSamples=2 → 恒失败无根因
	if _, err := Enroll(context.Background(), opts); err == nil {
		t.Fatal("n1 < MinValidSamples 应报参数错误")
	}
	opts = baseOpts(b, s, "v1")
	opts.Settings.KMinCells = 0
	if _, err := Enroll(context.Background(), opts); err == nil {
		t.Fatal("KMinCells<1 应报参数错误")
	}
}

func TestEnrollBudgetAbortSkipsT0(t *testing.T) {
	// 预算超限（T=1.0 段）→ 立即返回，不执行 T=0 段
	b := testBattery(t)
	s := testStore(t)
	opts := baseOpts(b, s, "v1")
	opts.Budget = provider.NewBudget(0, 3) // 3 次调用后超限
	mp := &mockProvider{}
	opts.Provider = mp
	_, err := Enroll(context.Background(), opts)
	if !errors.Is(err, provider.ErrBudgetExceeded) {
		t.Fatalf("预算超限应返回 ErrBudgetExceeded，实际 %v", err)
	}
	// T=0 段不应启动：T1 段并发（8）在途调用约 3+8=11，若 T0 段执行将 +120
	if n := mp.counter.Load(); n > 30 {
		t.Fatalf("预算中止后不应继续大量调用（T=0 段未跳过？），实际 %d 次", n)
	}
	// T=0 段响应文件不应存在（未执行）
	if rs, _ := s.LoadResponses(refResponsesID("qwen/qwen3-8b", "v1") + "-t0"); len(rs) != 0 {
		t.Fatal("T=0 段不应有响应落盘")
	}
}
