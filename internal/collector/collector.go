// Package collector 实现并发采集（设计 §2.1、§2.2、性能设计）：
//
//	worker pool（并发上限可配置，硬上限 256）→ 每个 cell 的 n 次重复并发执行；
//	幂等续采（responses JSONL 已入库样本索引去重，崩溃恢复不重复入库；
//	同 ID 必须串行运行——并发同 ID 属未定义行为）；
//	种子打乱任务顺序（同种子可复现任务集合；执行顺序在并发下不保证——
//	分布/JSD 与顺序无关，故审计结果可复现）；
//	进度经回调输出（stderr 由调用方渲染），collector 自身不写 stdout
//	（进度/结果流分离：stdout 只承载结构化结果）。
//
// 依赖 provider（M2.1/M2.2 传输层：重试/限流/护栏/密钥回显拒收）与
// store（M1.1 幂等键）。
package collector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"onetoken/internal/battery"
	"onetoken/internal/provider"
	"onetoken/internal/store"
)

// defaultConcurrency 是未配置时的 worker 数（设计：per-provider 并发 4–8）。
const defaultConcurrency = 8

// maxConcurrency 是并发硬上限（审查 S-M1：防误配置/恶意配置海量 goroutine OOM）。
const maxConcurrency = 256

// defaultMaxTokens 是输出上限默认值（设计 §3.1：16；按协议映射 max_tokens/max_output_tokens）。
const defaultMaxTokens = 16

// ResponseSink 是采集响应写入端（幂等索引 + 追加）。
// store.Store（磁盘 JSONL，证据链）与 store.MemoryStore（内存，compare 不落库
// 路径，M2.10）均满足；接口定义在消费方（collector），Go 结构类型隐式实现。
type ResponseSink interface {
	// LoadResponsesIndex 返回已完成样本（cell+sample_idx 键）集合。
	LoadResponsesIndex(auditID string) (map[string]bool, error)
	// AppendResponse 追加一条响应（证据链 raw_sha256 必填）。
	AppendResponse(auditID string, r *store.Response) error
}

// Options 是 RunBattery 的采集参数。
type Options struct {
	// ID 是响应 JSONL 的标识（responses/<ID>.jsonl），幂等续采的键：
	// 同 ID 串行重跑时跳过已入库样本（崩溃恢复）。必填。
	// ⚠️ 同 ID 并发运行两次 RunBattery 属未定义行为（启动快照去重，
	// 并发下会重复入库）——调用方必须保证串行。
	ID string
	// Model 是请求的模型名（RequestParams.Model）。必填。
	Model string
	// MaxTokens 输出上限（0 → 默认 16；与 Settings.OutputTokenCap 由 CLI 接线）。
	MaxTokens int
	// Concurrency worker 数（<=0 → 默认 8；>256 → 截断为 256）。
	// CLI 应传 min(Limits.MaxConcurrency, 256)。
	Concurrency int
	// Seed 任务打乱种子。0 也是确定性种子（不再回退时间——调用方负责
	// 生成并持久化实际种子，如 store.Audit.Seed；审计可复现性契约见设计性能设计）。
	Seed int64
	// ReasoningEffort 尽力禁用推理（空 → "minimal"）。
	ReasoningEffort string
	// Budget 审计级预算（provider.Budget）：超限立即中止并返回
	// provider.ErrBudgetExceeded（设计 §10.1-3，记 inconclusive 由调用方处理）。
	Budget *provider.Budget
	// OnProgress 进度回调（done/total）。⚠️ 在内部锁内调用：必须快速返回
	// （仅渲染进度，如 stderr 进度条），不得阻塞、不得重入 RunBattery、
	// 不得 panic。失败/中止任务不计 done，进度可能停在 <total。
	OnProgress func(done, total int)
}

// TaskError 是单个采样任务的失败（Cell/SampleIdx 定位，便于续采与诊断）。
type TaskError struct {
	Cell      string
	SampleIdx int
	Err       error
}

func (e *TaskError) Error() string {
	return fmt.Sprintf("collector: %s[%d] 采样失败: %v", e.Cell, e.SampleIdx, e.Err)
}

func (e *TaskError) Unwrap() error { return e.Err }

// CountTaskFailures 统计聚合错误中的 TaskError 数量（detector unreachable
// 判定的 FailedTasks 生产者；errors.Join 的嵌套链遍历计数）。
func CountTaskFailures(err error) int {
	if err == nil {
		return 0
	}
	seen := make(map[error]bool)
	queue := []error{err}
	n := 0
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == nil || seen[cur] {
			continue
		}
		seen[cur] = true
		// 多错误容器（errors.Join）先展平：errors.As 对 joinError 会递归
		// 命中第一个 TaskError 并吞掉兄弟错误，必须先展开再逐个处理。
		if u, ok := cur.(interface{ Unwrap() []error }); ok {
			queue = append(queue, u.Unwrap()...)
			continue
		}
		var te *TaskError
		if errors.As(cur, &te) {
			n++
			continue
		}
		// 单错误链继续
		if u, ok := cur.(interface{ Unwrap() error }); ok {
			queue = append(queue, u.Unwrap())
		}
	}
	return n
}

// job 是一个采样任务（cell × sample_idx）。
type job struct {
	cell      string
	sampleIdx int
}

// RunBattery 对 cells 列表的每个 cell 采样 n 次（温度 T），经 provider 并发执行，
// 响应入库 responses/<ID>.jsonl（只增不改，证据链），返回本次新增的响应。
//
// 语义：
//   - 幂等：启动时 LoadResponsesIndex(ID) 跳过已完成 (cell, sample_idx)；
//     崩溃/中断后同 ID 串行重跑只补缺失样本，不重复入库；
//   - 种子打乱：按 seed 混洗 cell 顺序后展开任务，限流亏空（某 cell 重试/等待）
//     均匀扩散到整个电池，而非集中在相邻 cell；同种子任务集合可复现；
//   - 失败容忍：单任务失败（重试耗尽/密钥回显拒收等）记 TaskError 继续其他任务；
//     ctx 取消或预算超限/致命错误（磁盘写失败）立即中止（部分结果已入库，可续采）；
//   - 错误返回优先级：外部 ctx 取消 > 中止（预算超限/致命错误，含根因）> 任务错误聚合。
func RunBattery(ctx context.Context, p provider.Provider, s ResponseSink, b *battery.Battery,
	cells []string, n int, T float64, opts Options) ([]*store.Response, error) {

	if p == nil || s == nil || b == nil {
		return nil, errors.New("collector: provider/store/battery 均为必填")
	}
	if n <= 0 {
		return nil, fmt.Errorf("collector: n=%d 必须 >0", n)
	}
	if len(cells) == 0 {
		return nil, errors.New("collector: cells 为空")
	}
	if math.IsNaN(T) || math.IsInf(T, 0) {
		return nil, fmt.Errorf("collector: T=%v 非法（NaN/Inf 会污染指纹）", T)
	}
	if opts.ID == "" {
		return nil, errors.New("collector: Options.ID 必填（幂等续采键）")
	}
	if opts.Model == "" {
		return nil, errors.New("collector: Options.Model 必填")
	}
	// cells 白名单校验（审查 S-L3/L5）：cell 必须来自电池（挡注入/超长/控制字符），
	// 且不得重复（单次运行内重复键会污染证据链）。
	validCells := make(map[string]bool, 40)
	for _, c := range b.Cells() {
		validCells[c] = true
	}
	seenCells := make(map[string]bool, len(cells))
	for _, c := range cells {
		if !validCells[c] {
			return nil, fmt.Errorf("collector: 非法 cell %q（不在电池内）", c)
		}
		if seenCells[c] {
			return nil, fmt.Errorf("collector: cell 重复 %q", c)
		}
		seenCells[c] = true
	}

	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	conc := opts.Concurrency
	if conc <= 0 {
		conc = defaultConcurrency
	}
	if conc > maxConcurrency {
		conc = maxConcurrency // 硬上限（审查 S-M1）
	}
	effort := opts.ReasoningEffort
	if effort == "" {
		effort = "minimal" // 设计 §3.1：尽力禁用推理（o 系拒绝时由能力探测/探测器处理）
	}
	seed := opts.Seed // 0 为确定性种子，不回退时间（可复现性由调用方持久化 seed）

	// 幂等索引：跳过已完成样本（崩溃续采核心）。
	doneIdx, err := s.LoadResponsesIndex(opts.ID)
	if err != nil {
		return nil, fmt.Errorf("collector: 读取幂等索引: %w", err)
	}

	// 种子打乱 cell 顺序（Fisher-Yates，可复现）。
	shuffled := shuffleCells(cells, seed)

	// 生成任务（跳过已完成）。
	var tasks []job
	for _, cell := range shuffled {
		for idx := 0; idx < n; idx++ {
			if doneIdx[store.ResponseKey(cell, idx)] {
				continue
			}
			tasks = append(tasks, job{cell: cell, sampleIdx: idx})
		}
	}
	if len(tasks) == 0 {
		return nil, nil // 全部已完成（续采幂等返回）
	}

	// 预填充任务队列后关闭：worker 消费；中止时 worker 自查标志退出。
	jobs := make(chan job, len(tasks))
	for _, j := range tasks {
		jobs <- j
	}
	close(jobs)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	total := len(tasks)
	var (
		mu        sync.Mutex
		results   []*store.Response
		taskErrs  []error
		completed atomic.Int64
		aborted   atomic.Bool  // 预算超限或致命错误中止
		abortErr  atomic.Value // 首个中止原因（error；幂等保存）
	)

	// recordResult 在锁内登记结果并触发进度回调（defer 解锁：回调 panic 不锁死）。
	recordResult := func(rec *store.Response) {
		mu.Lock()
		defer mu.Unlock()
		results = append(results, rec)
		done := int(completed.Add(1))
		if opts.OnProgress != nil {
			opts.OnProgress(done, total) // ⚠️ 锁内调用：回调必须快速返回（见 Options 注释）
		}
	}

	// abort 幂等记录首个中止原因并取消剩余任务。
	abort := func(err error) {
		if _, loaded := abortErr.Load().(error); loaded {
			return
		}
		abortErr.Store(err)
		aborted.Store(true)
		cancel()
	}

	run := func(j job) {
		params, err := paramsFor(b, j.cell, opts.Model, T, maxTokens, effort)
		if err != nil {
			mu.Lock()
			taskErrs = append(taskErrs, &TaskError{Cell: j.cell, SampleIdx: j.sampleIdx, Err: err})
			mu.Unlock()
			return
		}
		resp, err := p.Complete(ctx, params)
		if err != nil {
			// 中止相关错误不记任务错误（预算/ctx/致命由主路径返回）
			if ctx.Err() != nil || aborted.Load() {
				return
			}
			mu.Lock()
			taskErrs = append(taskErrs, &TaskError{Cell: j.cell, SampleIdx: j.sampleIdx, Err: err})
			mu.Unlock()
			return
		}
		rec := toResponse(j, T, resp)
		if err := s.AppendResponse(opts.ID, rec); err != nil {
			// 磁盘写失败：致命（证据链中断），中止整个采集并保留根因
			mu.Lock()
			taskErrs = append(taskErrs, &TaskError{Cell: j.cell, SampleIdx: j.sampleIdx,
				Err: fmt.Errorf("入库失败: %w", err)})
			mu.Unlock()
			abort(errors.New("collector: 入库失败（证据链中断）"))
			return
		}
		if opts.Budget != nil {
			// 预算在响应之后记账：超限的那次调用成本已花、结果如实保留（与库一致），
			// 其后任务中止。预算是软上限，超支量级 ≤ 并发度（审查 M1）。
			if err := opts.Budget.Spend(resp.CostUSD); err != nil {
				recordResult(rec)
				abort(err)
				return
			}
		}
		recordResult(rec)
	}

	var wg sync.WaitGroup
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if aborted.Load() || ctx.Err() != nil {
					return // 中止：丢弃剩余任务
				}
				func() {
					// worker recover（审查 M5）：Provider/回调 panic 转 TaskError 并中止，
					// 防止 goroutine panic 杀死整个进程（已入库数据不受损）。
					defer func() {
						if r := recover(); r != nil {
							mu.Lock()
							taskErrs = append(taskErrs, &TaskError{Cell: j.cell, SampleIdx: j.sampleIdx,
								Err: fmt.Errorf("panic: %v", r)})
							mu.Unlock()
							abort(errors.New("collector: worker panic"))
						}
					}()
					run(j)
				}()
			}
		}()
	}
	wg.Wait()

	// 错误返回优先级（审查 M2/H1）：
	// 1. 外部 ctx 取消（aborted 为假时才返回——预算/致命中止时内部 cancel 也会
	//    置派生 ctx.Err，需用 aborted 区分，避免中止错误被误判为取消）；
	// 2. 中止（预算超限/致命错误）：中止原因 + 已收集任务错误一并返回（保留根因，
	//    不返回泛化「采集中止」吞掉诊断信息）；
	// 3. 任务错误聚合。
	if err := ctx.Err(); err != nil && !aborted.Load() {
		return results, err
	}
	if aborted.Load() {
		if be, ok := abortErr.Load().(error); ok && be != nil {
			return results, errors.Join(append([]error{be}, taskErrs...)...)
		}
		if len(taskErrs) > 0 {
			return results, errors.Join(taskErrs...)
		}
		return results, errors.New("collector: 采集中止")
	}
	if len(taskErrs) > 0 {
		return results, errors.Join(taskErrs...)
	}
	return results, nil
}

// paramsFor 将 cell（"task:lang"）解析为 RequestParams（提示词来自电池，防注入已由 battery 校验）。
func paramsFor(b *battery.Battery, cell, model string, T float64, maxTokens int, effort string) (provider.RequestParams, error) {
	parts := strings.Split(cell, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return provider.RequestParams{}, fmt.Errorf("collector: 非法 cell %q（期望 task:lang）", cell)
	}
	prompt, err := b.Prompt(parts[0], parts[1])
	if err != nil {
		return provider.RequestParams{}, err
	}
	return provider.RequestParams{
		Model:           model,
		Prompt:          prompt,
		SystemPrompt:    b.SystemPrompt,
		Temperature:     T,
		MaxTokens:       maxTokens,
		ReasoningEffort: effort,
		// Store: false（Responses API store 参数，默认不落 OpenAI 平台）
	}, nil
}

// toResponse 将协议统一响应收敛为 store.Response（证据链：raw_sha256 必填）。
func toResponse(j job, T float64, r *provider.ResponseRecord) *store.Response {
	return &store.Response{
		Cell:             j.cell,
		SampleIdx:        j.sampleIdx,
		Temperature:      T,
		RawCompletion:    r.RawCompletion,
		RawSHA256:        sha256Hex(r.RawCompletion),
		Text:             r.Text,
		ReasoningTokens:  r.ReasoningTokens,
		CompletionTokens: r.CompletionTokens,
		FinishReason:     r.FinishReason,
		LatencyMS:        r.LatencyMS,
		Provider:         r.Provider,
		ReportedModel:    r.ReportedModel,
		CostUSD:          r.CostUSD,
		TS:               r.TS.UTC().Format(time.RFC3339),
	}
}

// sha256Hex 计算 raw 的 SHA-256 十六进制摘要（证据链哈希）。
func sha256Hex(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// shuffleCells 按种子打乱 cell 顺序（副本，不改原切片）。
func shuffleCells(cells []string, seed int64) []string {
	out := append([]string(nil), cells...)
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}
