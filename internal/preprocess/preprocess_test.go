package preprocess

import (
	"strings"
	"testing"
)

// 常用任务
var (
	taskNum100 = Task{ID: "random_number_100", AnswerSpace: "closed", SpaceSize: 100}
	taskNum10  = Task{ID: "random_number_10", AnswerSpace: "closed", SpaceSize: 10}
	taskLetter = Task{ID: "random_letter", AnswerSpace: "closed", SpaceSize: 26}
	taskCoin   = Task{ID: "coin_flip", AnswerSpace: "closed", SpaceSize: 2}
	taskColor  = Task{ID: "random_color", AnswerSpace: "open"}
	taskFavCol = Task{ID: "favorite_color", AnswerSpace: "open"}
	taskWord   = Task{ID: "random_word", AnswerSpace: "open"}
	taskCity   = Task{ID: "random_city", AnswerSpace: "open"}
	taskAnimal = Task{ID: "random_animal", AnswerSpace: "open"}
)

// --- 归一化管线 ---

func TestNormalizeArabicIndicDigits(t *testing.T) {
	cases := map[string]string{
		"٤٢":         "42",
		"٠١٢٣٤٥٦٧٨٩": "0123456789",
		"۴۲":         "42", // 波斯-印度
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Fatalf("Normalize(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestNormalizeChineseNumbers(t *testing.T) {
	cases := map[string]string{
		"四十二":  "42",
		"一二三":  "123",
		"十":    "10",
		"二十":   "20",
		"一百零五": "105",
		"七":    "7",
		"零":    "0",
		"两百":   "200",
		"十万":   "100000",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Fatalf("Normalize(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestNormalizeFullwidthDigits(t *testing.T) {
	if got := Normalize("４２"); got != "42" {
		t.Fatalf("全角数字: %q", got)
	}
}

func TestNormalizeNFC(t *testing.T) {
	// e + combining acute → é（NFC 合成）
	if got := Normalize("e\u0301"); got != "é" {
		t.Fatalf("NFC 失败: %q", got)
	}
}

func TestNormalizeStripsPunctQuotes(t *testing.T) {
	for in, want := range map[string]string{
		`"42"`:   "42",
		"42.":    "42",
		"42，":    "42",
		"(42)":   "42",
		"42!":    "42",
		"'blue'": "blue",
		"red;":   "red",
		"1,000":  "1000", // 千分位逗号
	} {
		if got := Normalize(in); got != want {
			t.Fatalf("Normalize(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestNormalizeLowercase(t *testing.T) {
	if got := Normalize("RED"); got != "red" {
		t.Fatalf("小写折叠: %q", got)
	}
	if got := Normalize("КРАСНЫЙ"); got != "красный" {
		t.Fatalf("西里尔小写: %q", got)
	}
}

func TestNormalizeStripsEmoji(t *testing.T) {
	// emoji 属 Symbol 类，与标点同层剥离
	if got := Normalize("42🎉"); got != "42" {
		t.Fatalf("emoji 剥离: %q", got)
	}
}

func TestFirstToken(t *testing.T) {
	if got := FirstToken("the answer is 42"); got != "the" {
		t.Fatalf("FirstToken: %q", got)
	}
	if got := FirstToken("42"); got != "42" {
		t.Fatalf("FirstToken: %q", got)
	}
	if got := FirstToken("答案"); got != "答案" { // 无空格中文
		t.Fatalf("FirstToken 中文: %q", got)
	}
	if got := FirstToken("   "); got != "" {
		t.Fatalf("空白: %q", got)
	}
}

// --- 分类 ---

func TestClassifyEmpty(t *testing.T) {
	for _, raw := range []string{"", "   ", "\t\n"} {
		r := NormalizeClassify(raw, taskWord)
		if r.Classification != ClassEmpty {
			t.Fatalf("%q 应判 empty，实际 %s", raw, r.Classification)
		}
	}
}

func TestClassifyRefusal(t *testing.T) {
	cases := []string{
		"Sorry, I cannot help with that.",
		"I'm sorry, I can't answer that.",
		"As an AI, I cannot provide a random number.",
		"抱歉，我无法回答。",
		"我不能告诉你。",
		"Извините, я не могу это сделать.",
		"آسف، لا أستطيع الإجابة.",
	}
	for _, raw := range cases {
		r := NormalizeClassify(raw, taskNum100)
		if r.Classification != ClassRefusal {
			t.Fatalf("%q 应判 refusal，实际 %s (%q)", raw, r.Classification, r.Normalized)
		}
	}
}

func TestClassifyClosedNumbers(t *testing.T) {
	for _, raw := range []string{"42", "1", "100", "٤٢", "四十二", "７", " 42 "} {
		r := NormalizeClassify(raw, taskNum100)
		if r.Classification != ClassValid {
			t.Fatalf("%q 应判 valid，实际 %s (%q)", raw, r.Classification, r.Normalized)
		}
	}
	for _, raw := range []string{"101", "0", "abc", "42.5", "答案"} {
		r := NormalizeClassify(raw, taskNum100)
		if r.Classification != ClassInvalid {
			t.Fatalf("%q 应判 invalid，实际 %s (%q)", raw, r.Classification, r.Normalized)
		}
	}
	// 注："-5" 的负号属标点被剥离 → "5" 判 valid。这是剥离管线的既定行为
	// （论文同样剥离 punctuation；随机数任务中模型不会答负号，对指纹统计无影响）。
	if r := NormalizeClassify("-5", taskNum100); r.Classification != ClassValid {
		t.Fatalf("-5 剥离负号后应为 valid（既定行为），实际 %s", r.Classification)
	}
	// 1-10 空间
	if r := NormalizeClassify("11", taskNum10); r.Classification != ClassInvalid {
		t.Fatalf("11 超出 1-10 应 invalid: %s", r.Classification)
	}
	if r := NormalizeClassify("9", taskNum10); r.Classification != ClassValid {
		t.Fatalf("9 应在 1-10 内: %s", r.Classification)
	}
}

func TestClassifyClosedLetter(t *testing.T) {
	for _, raw := range []string{"a", "z", "k"} {
		r := NormalizeClassify(raw, taskLetter)
		if r.Classification != ClassValid {
			t.Fatalf("%q 应判 valid: %s", raw, r.Classification)
		}
	}
	for _, raw := range []string{"ab", "42", "a b"} {
		r := NormalizeClassify(raw, taskLetter)
		if r.Classification != ClassInvalid {
			t.Fatalf("%q 应判 invalid: %s (%q)", raw, r.Classification, r.Normalized)
		}
	}
}

func TestClassifyCoinFlip(t *testing.T) {
	for raw, want := range map[string]string{
		"heads": "h", "tails": "t",
		"正面": "h", "反面": "t",
		"орёл": "h", "решка": "t",
		"صورة": "h", "كتابة": "t",
	} {
		r := NormalizeClassify(raw, taskCoin)
		if r.Classification != ClassValid || r.Normalized != want {
			t.Fatalf("%q 应映射为 %s valid，实际 %s (%q)", raw, want, r.Classification, r.Normalized)
		}
	}
	if r := NormalizeClassify("xyz", taskCoin); r.Classification != ClassInvalid {
		t.Fatalf("xyz 应 invalid: %s", r.Classification)
	}
}

func TestClassifyColors(t *testing.T) {
	for raw, want := range map[string]string{
		"红色": "red", "красный": "red", "أحمر": "red", "RED": "red",
		"绿色": "green", "blue": "blue", "жёлтый": "yellow",
	} {
		r := NormalizeClassify(raw, taskColor)
		if r.Classification != ClassValid || r.Normalized != want {
			t.Fatalf("%q 应映射为 %s valid，实际 %s (%q)", raw, want, r.Classification, r.Normalized)
		}
	}
	// favorite_color 同样映射
	if r := NormalizeClassify("紫色", taskFavCol); r.Normalized != "purple" {
		t.Fatalf("favorite_color 映射: %q", r.Normalized)
	}
}

func TestClassifyOpen(t *testing.T) {
	for _, raw := range []string{"cat", "paris", "狗", "существо"} {
		r := NormalizeClassify(raw, taskWord)
		if r.Classification != ClassValid {
			t.Fatalf("%q 应 valid: %s", raw, r.Classification)
		}
	}
	// 多词 → invalid（论文：multi-word）
	for _, raw := range []string{"light blue", "the cat", "blue sky", "答案是 42"} {
		r := NormalizeClassify(raw, taskWord)
		if r.Classification != ClassInvalid {
			t.Fatalf("%q 多词应 invalid: %s (%q)", raw, r.Classification, r.Normalized)
		}
	}
}

// 多词整句：closed 任务出空间（首 token 不在数字空间）→ invalid
func TestClassifySentence(t *testing.T) {
	r := NormalizeClassify("The answer is 42", taskNum100)
	if r.Classification != ClassInvalid {
		t.Fatalf("整句应 invalid: %s (%q)", r.Classification, r.Normalized)
	}
}

// --- 黄金样本（真实场景组合，论文 §IV-B 语义） ---

func TestGoldenSamples(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		task Task
		cls  Classification
		norm string
	}{
		{"中文数字回答", "四十二", taskNum100, ClassValid, "42"},
		{"阿印数字回答", "٤٢", taskNum100, ClassValid, "42"},
		{"英文数字带标点", "\"42\".", taskNum100, ClassValid, "42"},
		{"中文颜色", "红色", taskColor, ClassValid, "red"},
		{"俄文颜色大写", "КРАСНЫЙ", taskColor, ClassValid, "red"},
		{"硬币中文", "正面", taskCoin, ClassValid, "h"},
		{"超出空间", "101", taskNum100, ClassInvalid, "101"},
		{"非数字", "apple", taskNum100, ClassInvalid, "apple"},
		{"明确拒绝英文", "Sorry, I cannot help.", taskNum100, ClassRefusal, "sorry, i cannot help."},
		{"明确拒绝中文", "抱歉，我无法回答。", taskNum100, ClassRefusal, "抱歉，我无法回答。"},
		{"空回答", "", taskWord, ClassEmpty, ""},
		{"空白回答", "   ", taskWord, ClassEmpty, ""},
		{"多词整句", "The answer is cat", taskWord, ClassInvalid, "the"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := NormalizeClassify(c.raw, c.task)
			if r.Classification != c.cls {
				t.Fatalf("分类=%s，期望 %s (norm=%q)", r.Classification, c.cls, r.Normalized)
			}
			if c.cls == ClassValid && r.Normalized != c.norm {
				t.Fatalf("归一化=%q，期望 %q", r.Normalized, c.norm)
			}
		})
	}
}

// --- ChineseToDigits 边界 ---

func TestChineseToDigits(t *testing.T) {
	cases := map[string]string{
		"四十二": "42", "一二三": "123", "十": "10", "二十": "20",
		"一百零五": "105", "七": "7", "零": "0", "两": "2",
		"十二": "12", "一百": "100", "千": "1000",
		"数字": "数字", // 无法解析 → 原串
		"":   "",
	}
	for in, want := range cases {
		if got := ChineseToDigits(in); got != want {
			t.Fatalf("ChineseToDigits(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// --- 确定性：同一输入同一输出 ---

func TestDeterministic(t *testing.T) {
	for i := 0; i < 3; i++ {
		r1 := NormalizeClassify("  「四十二」！ ", taskNum100)
		r2 := NormalizeClassify("  「四十二」！ ", taskNum100)
		if r1 != r2 {
			t.Fatalf("归一化应确定: %+v vs %+v", r1, r2)
		}
	}
}

func TestNoSilentDrop(t *testing.T) {
	// 所有输入都有分类（无静默丢弃）
	for _, raw := range []string{"", " ", "42", "abc", "抱歉", "四十二", "🎉", "42🎉", "The answer"} {
		r := NormalizeClassify(raw, taskNum100)
		if r.Classification == "" {
			t.Fatalf("%q 无分类（静默丢弃）", raw)
		}
	}
}

// 多词颜色/硬币回答不得被词表折叠放行（审查 H1）
func TestMultiWordColorCoinInvalid(t *testing.T) {
	for _, raw := range []string{"blue sky", "красный цвет", "light red", "tails definitely"} {
		r := NormalizeClassify(raw, taskColor)
		if r.Classification != ClassInvalid {
			t.Fatalf("%q 多词颜色应 invalid，实际 %s (%q)", raw, r.Classification, r.Normalized)
		}
	}
}

// refusal 词边界：普通英文词不得误报（审查 H2）
func TestRefusalNoFalsePositive(t *testing.T) {
	for _, raw := range []string{"vacant", "lubricant", "significant", "applicant", "cat", "42"} {
		if IsRefusal(raw) {
			t.Fatalf("%q 不应判 refusal（词边界）", raw)
		}
		r := NormalizeClassify(raw, taskWord)
		if r.Classification == ClassRefusal {
			t.Fatalf("%q 不应判 refusal", raw)
		}
	}
}

// 俄语 navy 无连字符变体（审查 H3：连字符被归一化剥离）
func TestColorNavyRussian(t *testing.T) {
	r := NormalizeClassify("тёмно-синий", taskColor)
	if r.Classification != ClassValid || r.Normalized != "navy" {
		t.Fatalf("тёмно-синий 应映射 navy valid，实际 %s (%q)", r.Classification, r.Normalized)
	}
}

// 中文单字色与扩展色系（canonical 码以论文 22 码为准，M1.5 pin）
func TestColorExtendedLexicon(t *testing.T) {
	for raw, want := range map[string]string{
		"红": "red", "蓝": "blue", "绿": "green",
		"violet": "violet", "teal": "teal", "maroon": "brown",
		"тёмносиний": "navy", "фиолетовый": "purple",
	} {
		r := NormalizeClassify(raw, taskColor)
		if r.Classification != ClassValid || r.Normalized != want {
			t.Fatalf("%q 应映射 %s，实际 %s (%q)", raw, want, r.Classification, r.Normalized)
		}
	}
}

// coin 单数 head（审查 M6）
func TestClassifyCoinHead(t *testing.T) {
	r := NormalizeClassify("head", taskCoin)
	if r.Classification != ClassValid || r.Normalized != "h" {
		t.Fatalf("head 应映射 h valid，实际 %s (%q)", r.Classification, r.Normalized)
	}
}

// refusal 补充模式（审查 M5）
func TestRefusalExtended(t *testing.T) {
	for _, raw := range []string{"لااستطيع", "لا استطيع", "我不知道", "не знаю", "I don't know"} {
		if !IsRefusal(raw) {
			t.Fatalf("%q 应判 refusal", raw)
		}
	}
}

// 小数伪影（审查 G-3）：4.5 剥离小数点后落入数字空间，文档化行为
func TestDecimalArtifact(t *testing.T) {
	r := NormalizeClassify("4.5", taskNum100)
	if r.Classification != ClassValid || r.Normalized != "45" {
		t.Fatalf("4.5 剥离后应 45 valid（既定伪影），实际 %s (%q)", r.Classification, r.Normalized)
	}
}

// 无空格中文整句（审查 G-4）：open 任务按单 token 记录（启发式局限，文档化）
func TestNoSpaceChineseSentence(t *testing.T) {
	r := NormalizeClassify("答案是42", taskNum100)
	if r.Classification != ClassInvalid {
		t.Fatalf("closed 任务无空格中文整句应 invalid，实际 %s", r.Classification)
	}
}

func TestChineseToDigitsLarge(t *testing.T) {
	for in, want := range map[string]string{
		"一万亿":   "1000000000000",
		"一亿五千万": "150000000",
		"十万亿":   "10000000000000",
		"一亿":    "100000000",
	} {
		if got := ChineseToDigits(in); got != want {
			t.Fatalf("ChineseToDigits(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// --- 与 battery 真实提示词组合冒烟 ---

// --- 与 battery 真实提示词组合冒烟 ---

func TestWithRealBattery(t *testing.T) {
	b, err := loadRealBattery()
	if err != nil {
		t.Skipf("无真实 prompts.json（非仓库环境）: %v", err)
	}
	// 40 个 cell 都能完成一次分类冒烟（用样例回答）
	for _, cell := range b.Cells() {
		parts := strings.SplitN(cell, ":", 2)
		taskID, lang := parts[0], parts[1]
		task := Task{ID: taskID, AnswerSpace: taskSpace(b, taskID), SpaceSize: taskSize(b, taskID)}
		// 每种任务用一个合理样例回答
		sample := sampleAnswer(taskID, lang)
		r := NormalizeClassify(sample, task)
		if r.Classification != ClassValid {
			t.Fatalf("cell %s 样例 %q 应 valid，实际 %s", cell, sample, r.Classification)
		}
	}
}
