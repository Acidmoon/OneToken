package battery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testPromptsPath 定位仓库 config/prompts.json。
func testPromptsPath(t *testing.T) string {
	t.Helper()
	// 从包目录向上找仓库根：internal/battery -> 仓库根
	p, err := filepath.Abs(filepath.Join("..", "..", "config", "prompts.json"))
	if err != nil {
		t.Fatalf("定位 prompts.json: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("prompts.json 不存在: %s", p)
	}
	return p
}

func TestLoadRealBattery(t *testing.T) {
	b, err := Load(testPromptsPath(t))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	cells := b.Cells()
	if len(cells) != 40 {
		t.Fatalf("cell 数 = %d，期望 40", len(cells))
	}
	// cell 标识唯一且为 task:lang 格式
	seen := map[string]bool{}
	for _, c := range cells {
		if seen[c] {
			t.Fatalf("cell 重复: %s", c)
		}
		seen[c] = true
		parts := strings.Split(c, ":")
		if len(parts) != 2 {
			t.Fatalf("cell 格式错误: %s", c)
		}
	}
	// 每个任务每语言提示非空（Validate 已保证，此处抽查）
	if _, err := b.Prompt("random_number_100", "zh"); err != nil {
		t.Fatalf("zh 提示: %v", err)
	}
	if b.SystemPrompt == "" {
		t.Fatal("system_prompt 为空")
	}
}

func TestValidateRejectsDupID(t *testing.T) {
	b := &Battery{
		SchemaVersion: 1,
		SystemPrompt:  "Answer with a single word only.",
		Tasks: []Task{
			{ID: "x", AnswerSpace: "closed", SpaceSize: 2, Prompts: map[string]string{
				"en": "a", "ru": "a", "zh": "a", "ar": "a"}},
			{ID: "x", AnswerSpace: "closed", SpaceSize: 2, Prompts: map[string]string{
				"en": "b", "ru": "b", "zh": "b", "ar": "b"}},
		},
	}
	if err := b.Validate(); err == nil {
		t.Fatal("重复任务 id 应报错")
	} else if !strings.Contains(err.Error(), "重复") {
		t.Fatalf("错误信息不符: %v", err)
	}
}

func TestValidateRejectsMissingLang(t *testing.T) {
	b := &Battery{
		SchemaVersion: 1,
		SystemPrompt:  "Answer with a single word only.",
		Tasks: []Task{
			{ID: "x", AnswerSpace: "closed", SpaceSize: 2, Prompts: map[string]string{
				"en": "a", "ru": "a", "zh": "a"}}, // 缺 ar
		},
	}
	if err := b.Validate(); err == nil {
		t.Fatal("缺语言应报错")
	}
}

func TestValidateRejectsInterpolation(t *testing.T) {
	b := &Battery{
		SchemaVersion: 1,
		SystemPrompt:  "Answer with a single word only.",
		Tasks: []Task{
			{ID: "x", AnswerSpace: "closed", SpaceSize: 2, Prompts: map[string]string{
				"en": "a", "ru": "a", "zh": "a", "ar": "say {{model}}"}},
		},
	}
	if err := b.Validate(); err == nil {
		t.Fatal("含插值占位符应报错（防注入）")
	} else if !strings.Contains(err.Error(), "插值") {
		t.Fatalf("错误信息不符: %v", err)
	}
}

func TestValidateRejectsClosedWithoutSize(t *testing.T) {
	b := &Battery{
		SchemaVersion: 1,
		SystemPrompt:  "Answer with a single word only.",
		Tasks: []Task{
			{ID: "x", AnswerSpace: "closed", Prompts: map[string]string{
				"en": "a", "ru": "a", "zh": "a", "ar": "a"}},
		},
	}
	if err := b.Validate(); err == nil {
		t.Fatal("closed 无空间大小应报错")
	}
}
