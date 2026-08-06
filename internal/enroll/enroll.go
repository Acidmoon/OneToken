// Package enroll 实现参考指纹建档编排（设计 §7 云端 API 单通道、§2.2 enroll 流程）：
//
//	采集（collector，T=1.0 指纹采样 + T=0 变体）→ 测量有效性清洗（detector）
//	→ 指纹构建（fingerprint.Build）→ 入库版本化（SaveFingerprint +
//	models.json 登记；同 modelID+version 冲突拒绝，UNIQUE 语义）。
//
// 参考指纹一律来源云端 API（用户决策 2026-08-06，§7）；指纹标注 RefSource=
// "official-api" 与来源 provider 名。测量级 flag（hidden-reasoning /
// response-caching）或不可达 → 建档失败（参考源本身不可信）；有效 cell
// < k_min → 建档失败（指纹不可用）。
//
// ⚠️ temperature-not-honored 门在 enroll 的默认探针口径下不生效：T0 段按
// 论文配置 EnrollNT0=3 采样（T0Cells 指纹用途），少于探测器 T0ProbeN=5，
// T0 一致性检测归审计侧（M2.7 探针 n≥5）。若调用方调大 EnrollNT0 使每 cell
// ≥ T0ProbeN，该门自动生效。
package enroll

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"onetoken/internal/battery"
	"onetoken/internal/collector"
	"onetoken/internal/config"
	"onetoken/internal/detector"
	"onetoken/internal/fingerprint"
	"onetoken/internal/preprocess"
	"onetoken/internal/provider"
	"onetoken/internal/store"
)

// ErrVersionConflict 同 modelID+version 已建档（UNIQUE(model_id, version) 语义）。
var ErrVersionConflict = errors.New("enroll: 指纹版本冲突（同 model_id+version 已存在）")

// Options 是 Enroll 的输入。
type Options struct {
	Settings config.Settings
	Provider provider.Provider
	Store    *store.Store
	Battery  *battery.Battery

	ModelID   string // 模型标识（如 qwen/qwen3-8b）
	Vendor    string // 厂商（如 zhipu / deepseek / openrouter）
	Family    string // 模型家族（如 qwen）
	ModelType string // open-source | proprietary
	Version   string // 指纹版本（如 2026-08-06v1；同 model_id+version 冲突）
	// ProviderName 参考来源 provider（标注用；RefSource 恒为 "official-api"）。
	ProviderName string

	// Frontier 前沿定价模型（≥$5/1M input，设计 §3.1）：T=1.0 采样减为 FrontierNT1。
	Frontier bool
	// Concurrency worker 数（<=0 → 默认 8）。
	Concurrency int
	// Budget 审计级预算（超限中止）。
	Budget *provider.Budget
	// OnProgress 采集进度回调（T=1.0 与 T=0 两段均触发，done 为各段内计数）。
	OnProgress func(phase string, done, total int)
}

// Enroll 执行参考指纹建档（云端 API 单通道）。返回构建的指纹。
//
// 响应落盘：T=1.0 与 T=0 分段独立续采（幂等键 ref-<model>-<version>[/-t0]），
// 中断后同参数重跑可续采；指纹保存为最后一步（保存前中断不产生版本占用）。
// 错误语义：预算/ctx 中止立即返回；任务级失败（TaskError 聚合）容忍，
// 由测量有效性门（unreachable/有效 cell）兜底。
func Enroll(ctx context.Context, opts Options) (*store.Fingerprint, error) {
	if opts.Provider == nil || opts.Store == nil || opts.Battery == nil {
		return nil, errors.New("enroll: provider/store/battery 均为必填")
	}
	if opts.ModelID == "" || opts.Version == "" || opts.ProviderName == "" {
		return nil, errors.New("enroll: ModelID/Version/ProviderName 必填")
	}
	s := opts.Settings

	// 0. 版本唯一性检查（UNIQUE(model_id, version)）；首次建档（无文件）视为无冲突。
	// existingVersion 用于覆盖时的 superseded_by 留痕（版本链，审查 R-M1）。
	existing, err := opts.Store.LoadFingerprint(opts.ModelID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("enroll: 读取既有指纹: %w", err)
	}
	var superseded string
	if existing != nil {
		if existing.Version == opts.Version {
			return nil, fmt.Errorf("%w: model=%s version=%s", ErrVersionConflict, opts.ModelID, opts.Version)
		}
		superseded = existing.Version
	}

	// 1. 采集：T=1.0 指纹采样（EnrollNT1；前沿 FrontierNT1）+ T=0 变体（EnrollNT0）。
	n1 := s.EnrollNT1
	if opts.Frontier {
		n1 = s.FrontierNT1
	}
	if n1 < s.MinValidSamples {
		return nil, fmt.Errorf("enroll: 采样数 n1=%d < MinValidSamples=%d（有效 cell 门槛永远无法满足）", n1, s.MinValidSamples)
	}
	if s.KMinCells < 1 {
		return nil, errors.New("enroll: Settings.KMinCells 非法（<1）")
	}
	cells := opts.Battery.Cells()
	conc := opts.Concurrency
	if conc <= 0 {
		conc = 8
	}
	refID := refResponsesID(opts.ModelID, opts.Version)
	progress := func(phase string) func(int, int) {
		if opts.OnProgress == nil {
			return nil
		}
		return func(done, total int) { opts.OnProgress(phase, done, total) }
	}
	cc := collector.Options{
		ID: refID, Model: opts.ModelID, Concurrency: conc,
		MaxTokens: s.OutputTokenCap, // 接线 Settings.OutputTokenCap（审查 R-L3）
		Budget:    opts.Budget, OnProgress: progress("t1"),
	}

	r1, err1 := collector.RunBattery(ctx, opts.Provider, opts.Store, opts.Battery, cells,
		n1, 1.0, cc)
	// 中止类错误（预算/ctx）立即返回，跳过第二段（审查 R-L2）；
	// 任务级失败（TaskError 聚合）容忍，由测量有效性门兜底
	// （errors.Is 对 errors.Join 递归有效）。
	if errors.Is(err1, provider.ErrBudgetExceeded) ||
		errors.Is(err1, context.Canceled) || errors.Is(err1, context.DeadlineExceeded) {
		return nil, err1
	}
	cc.ID = refID + "-t0"
	cc.OnProgress = progress("t0")
	r0, err0 := collector.RunBattery(ctx, opts.Provider, opts.Store, opts.Battery, cells,
		s.EnrollNT0, 0.0, cc)
	if errors.Is(err0, provider.ErrBudgetExceeded) ||
		errors.Is(err0, context.Canceled) || errors.Is(err0, context.DeadlineExceeded) {
		return nil, err0
	}
	failed1 := collector.CountTaskFailures(err1)
	failed0 := collector.CountTaskFailures(err0)

	// 2. 测量有效性清洗（参考源必须可信：测量级 flag → 建档失败）
	rs1, err := opts.Store.LoadResponses(refID)
	if err != nil {
		return nil, fmt.Errorf("enroll: 读取参考响应失败: %w", err)
	}
	rs0, err := opts.Store.LoadResponses(refID + "-t0")
	if err != nil {
		return nil, fmt.Errorf("enroll: 读取 T=0 参考响应失败: %w", err)
	}
	// 段级失败率（审查 R-H1）：T1/T0 分段独立判定——T0 段全败不被 T1 成功稀释
	// （全局比率会掩盖单段不可用）。Screen 的全局 unreachable 不适用，自行判定。
	ratio1, ratio0 := failRatio(failed1, len(r1)), failRatio(failed0, len(r0))
	scr := detector.Screen(rs1, detector.ScreenOptions{
		Settings:    s,
		T0Responses: rs0,
		TaskForCell: taskResolver(opts.Battery),
	})
	switch {
	case ratio1 >= s.UnreachableFailRatio || ratio0 >= s.UnreachableFailRatio:
		return nil, fmt.Errorf("enroll: 参考源不可达（T1 失败率 %.0f%% / T0 失败率 %.0f%%）",
			ratio1*100, ratio0*100)
	case scr.Flags.HiddenReasoning || scr.Flags.ResponseCaching || scr.Flags.TemperatureNotHonored:
		return nil, fmt.Errorf("enroll: 参考源测量有效性异常（%s），建档拒绝",
			strings.Join(scr.Flags.List(), ","))
	case scr.ValidCells < s.KMinCells:
		return nil, fmt.Errorf("enroll: 有效 cell %d < k_min %d，指纹不可用", scr.ValidCells, s.KMinCells)
	}

	// 3. 指纹构建（T=1.0 与 T=0 响应一并传入，Build 按温度分组）
	all := append(rs1, rs0...)
	fp, err := fingerprint.Build(opts.ModelID, opts.Version, "official-api", time.Now().UTC(), all)
	if err != nil {
		return nil, fmt.Errorf("enroll: 指纹构建失败: %w", err)
	}
	if len(fp.Cells) < s.KMinCells {
		return nil, fmt.Errorf("enroll: 指纹有效 cell %d < k_min %d", len(fp.Cells), s.KMinCells)
	}

	// 4. 入库（审查 R-M2：先 models 后指纹——models 失败时指纹未落盘，重跑可重入；
	// 指纹失败时 models 有登记无指纹，重跑不撞版本冲突）。
	fp.Provider = opts.ProviderName
	fp.SupersededBy = superseded // 版本链留痕（M4.2 扩展 per-version 布局的前置）
	models, err := opts.Store.LoadModels()
	if err != nil {
		return nil, fmt.Errorf("enroll: 读取模型目录失败: %w", err)
	}
	merged := appendModel(models, store.Model{
		ID: opts.ModelID, Vendor: opts.Vendor, Family: opts.Family,
		ModelType: opts.ModelType, RefSource: "official-api",
		Provider: opts.ProviderName,
		Notes:    "ref via provider " + opts.ProviderName,
	})
	if err := opts.Store.SaveModels(merged); err != nil {
		return nil, fmt.Errorf("enroll: 保存模型目录失败: %w", err)
	}
	if err := opts.Store.SaveFingerprint(fp); err != nil {
		return nil, fmt.Errorf("enroll: 保存指纹失败: %w", err)
	}
	return fp, nil
}

// failRatio 计算失败率（分母为 0 时返回 0，避免除零）。
func failRatio(failed, succeeded int) float64 {
	if failed+succeeded == 0 {
		return 0
	}
	return float64(failed) / float64(failed+succeeded)
}

// refResponsesID 构造响应 JSONL 的幂等续采 id（ref-<model>-<version>[/-t0]）。
// 复用 store.SanitizeID：与响应文件路径的规范化一致，消除双规则碰撞面
// （审查 S-M1：此前 "/"→"_" 与 store "/"→"__" 不一致，导致 "a/b" 与 "a_b" 同 id）。
func refResponsesID(modelID, version string) string {
	return "ref-" + store.SanitizeID(modelID) + "-" + version
}

// appendModel 合并模型登记（modelID 已存在则保留既有条目，防止覆盖元数据）。
func appendModel(models []store.Model, m store.Model) []store.Model {
	for i := range models {
		if models[i].ID == m.ID {
			return models
		}
	}
	return append(models, m)
}

// taskResolver 从 battery 构造 TaskForCell（与 verify 包同构；避免跨包依赖重复暴露）。
func taskResolver(b *battery.Battery) func(string) (preprocess.Task, bool) {
	byID := make(map[string]preprocess.Task, len(b.Tasks))
	for _, t := range b.Tasks {
		byID[t.ID] = preprocess.Task{ID: t.ID, AnswerSpace: t.AnswerSpace, SpaceSize: t.SpaceSize}
	}
	return func(cell string) (preprocess.Task, bool) {
		parts := strings.SplitN(cell, ":", 2)
		if len(parts) != 2 {
			return preprocess.Task{}, false
		}
		task, ok := byID[parts[0]]
		return task, ok
	}
}
