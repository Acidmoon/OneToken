package battery

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testPromptsPath 定位仓库 config/prompts.json。
func testPromptsPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "config", "prompts.json"))
	if err != nil {
		t.Fatalf("定位 prompts.json: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("prompts.json 不存在: %s", p)
	}
	return p
}

// validBattery 构造一个合法的 10 任务×4 语言电池（供负向测试修改单一维度）。
func validBattery() *Battery {
	langs := map[string]string{"en": "a", "ru": "a", "zh": "a", "ar": "a"}
	var tasks []Task
	for i := 0; i < taskCount; i++ {
		tasks = append(tasks, Task{ID: fmt.Sprintf("task_%d", i), AnswerSpace: "closed", SpaceSize: 2, Prompts: langs})
	}
	return &Battery{
		SchemaVersion: 1,
		SystemPrompt:  "Answer with a single word only.",
		Languages:     []string{"en", "ru", "zh", "ar"},
		Tasks:         tasks,
	}
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

func TestValidateRejectsTaskCount(t *testing.T) {
	b := validBattery()
	b.Tasks = b.Tasks[:9] // 9 个任务
	if err := b.Validate(); err == nil {
		t.Fatal("任务数≠10 应报错")
	}
}

func TestValidateRejectsDupID(t *testing.T) {
	b := validBattery()
	b.Tasks[1].ID = b.Tasks[0].ID
	if err := b.Validate(); err == nil {
		t.Fatal("重复任务 id 应报错")
	} else if !strings.Contains(err.Error(), "重复") {
		t.Fatalf("错误信息不符: %v", err)
	}
}

func TestValidateRejectsColonInID(t *testing.T) {
	b := validBattery()
	b.Tasks[0].ID = "a:b"
	if err := b.Validate(); err == nil {
		t.Fatal("任务 id 含 ':' 应报错")
	}
}

func TestValidateRejectsMissingLang(t *testing.T) {
	b := validBattery()
	delete(b.Tasks[0].Prompts, "ar")
	if err := b.Validate(); err == nil {
		t.Fatal("缺语言应报错")
	}
}

func TestValidateRejectsLangDrift(t *testing.T) {
	b := validBattery()
	b.Languages = []string{"en", "ru", "zh"} // 缺 ar
	if err := b.Validate(); err == nil {
		t.Fatal("语言声明与内置常量不一致应报错")
	}
}

func TestValidateRejectsInterpolation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Battery)
		kw   string
	}{
		{"模板占位符", func(b *Battery) { b.Tasks[0].Prompts["en"] = "say {{model}}" }, "占位符"},
		{"裸美元变量", func(b *Battery) { b.Tasks[0].Prompts["en"] = "say $model" }, "变量"},
		{"fmt 占位符", func(b *Battery) { b.Tasks[0].Prompts["en"] = "say %s of %d" }, "格式化"},
		{"system_prompt 模板", func(b *Battery) { b.SystemPrompt = "Answer {{model}} now." }, "占位符"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := validBattery()
			c.mut(b)
			if err := b.Validate(); err == nil {
				t.Fatal("含占位符应报错（防注入）")
			} else if !strings.Contains(err.Error(), c.kw) {
				t.Fatalf("错误信息不符: %v", err)
			}
		})
	}
}

func TestValidateRejectsClosedWithoutSize(t *testing.T) {
	b := validBattery()
	b.Tasks[0].SpaceSize = 0
	if err := b.Validate(); err == nil {
		t.Fatal("closed 无空间大小应报错")
	}
}
