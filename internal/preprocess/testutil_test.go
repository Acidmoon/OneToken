package preprocess

import (
	"os"
	"path/filepath"

	"onetoken/internal/battery"
)

// loadRealBattery 加载仓库 config/prompts.json（不存在时返回错误，测试跳过）。
func loadRealBattery() (*battery.Battery, error) {
	p, err := filepath.Abs(filepath.Join("..", "..", "config", "prompts.json"))
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(p); err != nil {
		return nil, err
	}
	return battery.Load(p)
}

func findTask(b *battery.Battery, id string) *battery.Task {
	for i := range b.Tasks {
		if b.Tasks[i].ID == id {
			return &b.Tasks[i]
		}
	}
	return nil
}

func taskSpace(b *battery.Battery, id string) string {
	if t := findTask(b, id); t != nil {
		return t.AnswerSpace
	}
	return "open"
}

func taskSize(b *battery.Battery, id string) int {
	if t := findTask(b, id); t != nil {
		return t.SpaceSize
	}
	return 0
}

// sampleAnswer 为每种任务 × 语言提供合理的样例回答（冒烟测试用）。
func sampleAnswer(taskID, lang string) string {
	samples := map[string]map[string]string{
		"random_number_100": {"en": "42", "ru": "42", "zh": "四十二", "ar": "٤٢"},
		"random_number_10":  {"en": "7", "ru": "7", "zh": "七", "ar": "٧"},
		"favorite_number":   {"en": "42", "ru": "7", "zh": "八", "ar": "٨"},
		"random_letter":     {"en": "k", "ru": "к", "zh": "a", "ar": "أ"},
		"random_word":       {"en": "cat", "ru": "слово", "zh": "词语", "ar": "كلمة"},
		"random_color":      {"en": "blue", "ru": "синий", "zh": "蓝色", "ar": "أزرق"},
		"favorite_color":    {"en": "red", "ru": "красный", "zh": "红色", "ar": "أحمر"},
		"random_animal":     {"en": "cat", "ru": "кот", "zh": "猫", "ar": "قطة"},
		"random_city":       {"en": "paris", "ru": "париж", "zh": "巴黎", "ar": "باريس"},
		"coin_flip":         {"en": "heads", "ru": "орёл", "zh": "正面", "ar": "صورة"},
	}
	if m, ok := samples[taskID]; ok {
		if v, ok := m[lang]; ok {
			return v
		}
	}
	return "42"
}
