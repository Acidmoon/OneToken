// Package battery 定义 40-cell 探针电池：10 任务 × 4 语言（英/俄/中/阿）。
//
// 提示词与语言列表加载自 config/prompts.json（设计文档 §3.1），
// 与程序代码分离。加载时执行结构校验（防缺失/防注入）。
package battery

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Languages 是论文规定的四种提示语言（与 prompts.json 一致）。
var Languages = []string{"en", "ru", "zh", "ar"}

// Task 描述一个探针任务。
type Task struct {
	ID          string            `json:"id"`
	AnswerSpace string            `json:"answer_space"`           // closed | open
	SpaceSize   int               `json:"space_size,omitempty"`   // closed 空间大小
	Prompts     map[string]string `json:"prompts"`                // lang -> 用户提示
}

// Battery 是 40-cell 电池的加载结果。
type Battery struct {
	SchemaVersion int    `json:"schema_version"`
	SystemPrompt  string `json:"system_prompt"`
	Tasks         []Task `json:"tasks"`
}

// CellID 返回 task×lang 的 cell 标识（"task:lang"），
// 与设计文档 §4 responses.cell 列格式一致。
func CellID(taskID, lang string) string { return taskID + ":" + lang }

// Load 从 JSON 文件加载电池并执行结构校验。
func Load(path string) (*Battery, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("battery: 读取 %s: %w", path, err)
	}
	var b Battery
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("battery: 解析 %s: %w", path, err)
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return &b, nil
}

// Validate 校验电池结构完整性（防缺失/防注入）：
//   - schema_version 必须为 1
//   - 任务 id 唯一、非空
//   - 每任务覆盖全部四种语言且提示非空
//   - 提示不得含模板插值占位符（{{ }}、${...}），防止后续提示组装时的注入
//   - closed 空间必须声明空间大小；open 空间不得声明
func (b *Battery) Validate() error {
	if b.SchemaVersion != 1 {
		return fmt.Errorf("battery: schema_version=%d，期望 1", b.SchemaVersion)
	}
	if strings.TrimSpace(b.SystemPrompt) == "" {
		return errors.New("battery: system_prompt 为空")
	}
	seen := make(map[string]bool, len(b.Tasks))
	for i := range b.Tasks {
		t := &b.Tasks[i]
		if strings.TrimSpace(t.ID) == "" {
			return fmt.Errorf("battery: 第 %d 个任务 id 为空", i)
		}
		if seen[t.ID] {
			return fmt.Errorf("battery: 任务 id 重复: %q", t.ID)
		}
		seen[t.ID] = true
		if t.AnswerSpace != "closed" && t.AnswerSpace != "open" {
			return fmt.Errorf("battery: 任务 %s answer_space=%q，期望 closed|open", t.ID, t.AnswerSpace)
		}
		if t.AnswerSpace == "closed" && t.SpaceSize <= 0 {
			return fmt.Errorf("battery: 任务 %s 为 closed 但未声明空间大小", t.ID)
		}
		if t.AnswerSpace == "open" && t.SpaceSize != 0 {
			return fmt.Errorf("battery: 任务 %s 为 open 但声明了空间大小", t.ID)
		}
		for _, lang := range Languages {
			p, ok := t.Prompts[lang]
			if !ok {
				return fmt.Errorf("battery: 任务 %s 缺少语言 %s", t.ID, lang)
			}
			if strings.TrimSpace(p) == "" {
				return fmt.Errorf("battery: 任务 %s 语言 %s 提示为空", t.ID, lang)
			}
			if strings.Contains(p, "{{") || strings.Contains(p, "}}") || strings.Contains(p, "${") {
				return fmt.Errorf("battery: 任务 %s 语言 %s 提示含模板插值占位符（防注入）", t.ID, lang)
			}
		}
	}
	return nil
}

// Cells 返回全部 cell 标识（task×lang，共 40 个）。
func (b *Battery) Cells() []string {
	var cells []string
	for i := range b.Tasks {
		for _, lang := range Languages {
			cells = append(cells, CellID(b.Tasks[i].ID, lang))
		}
	}
	return cells
}

// Prompt 返回指定任务与语言的用户提示。
func (b *Battery) Prompt(taskID, lang string) (string, error) {
	for i := range b.Tasks {
		if b.Tasks[i].ID == taskID {
			if p, ok := b.Tasks[i].Prompts[lang]; ok {
				return p, nil
			}
			return "", fmt.Errorf("battery: 任务 %s 无语言 %s", taskID, lang)
		}
	}
	return "", fmt.Errorf("battery: 未知任务 %s", taskID)
}
