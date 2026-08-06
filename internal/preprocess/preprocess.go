// Package preprocess 实现归一化与分类（设计文档 §3.2；论文 §IV-B）。
//
// 归一化是确定性的：Unicode NFC → 剥离标点/引号/符号 → 大小写折叠 →
// 数字映射（阿拉伯-印度、波斯-印度、全角、中文数词）→ 任务语义规范化
// （颜色/硬币跨语言词表）。
//
// 分类互斥：valid / invalid（出答案空间或多词）/ refusal（明确拒绝）/ empty。
// 无静默丢弃：所有响应都有分类；有效率统计由调用方基于分类计算，
// refusal 与有效率不进指纹（论文：对安全层变化鲁棒）。
//
// 工程约定（论文 §IV-B 的文本级实现）：
//   - 首 token 提取用 Unicode 空白切分启发式（Go 无模型 tokenizer；一词回答
//     场景下文本级首词与 tokenizer 首 token 一致）；无空格语言（中文）整串
//     即首 token，"答案是42" 这类无空格整句在 open 任务会被记为单 token（局限，
//     设计文档已注明文本级近似）；
//   - random_letter 的闭空间按"单字母字符"判定（论文 closed(26) 基于英文
//     字母表，多语言下放宽以避免破坏有效率）；
//   - emoji 等符号（unicode.IsSymbol）被剥离（与标点同层处理）；
//   - 小数/负号伪影：标点剥离把 "4.5"→"45"（可能落入数字空间）、"-5"→"5"，
//     属剥离语义的固有副作用（论文同样剥离 punctuation）；随机数任务中模型
//     不会持续输出此类异常，对指纹分布统计无实质影响；
//   - favorite_number（open·数值）不做数值过滤，非数值 token 也会进入分布
//     （与 open 任务定义一致）；
//   - refusal 判定为启发式（英文词边界正则 + 中/俄/阿包含匹配），存在少量
//     误报/漏报（如 random_word 答 "refuse" 被判拒绝），属可接受的取舍。
package preprocess

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Classification 是响应分类（设计 §3.2）。
type Classification string

const (
	ClassValid   Classification = "valid"
	ClassInvalid Classification = "invalid"
	ClassRefusal Classification = "refusal"
	ClassEmpty   Classification = "empty"
)

// Task 描述归一化所需的任务语义（与 battery.Task 对齐）。
type Task struct {
	ID          string // 如 "random_number_100"
	AnswerSpace string // closed | open
	SpaceSize   int    // closed 空间大小
}

// Result 是归一化与分类的结果。
type Result struct {
	Normalized     string
	Classification Classification
}

// NormalizeClassify 对原始回答执行确定性归一化与分类。
// 判定顺序：empty → refusal → multi-word（在任务语义词表折叠之前，
// 防 "blue sky" 被折叠为单词放行）→ closed/open 空间判定。
func NormalizeClassify(raw string, task Task) Result {
	n := Normalize(raw)
	t := strings.TrimSpace(n)
	if t == "" {
		return Result{Normalized: t, Classification: ClassEmpty}
	}
	if IsRefusal(t) {
		return Result{Normalized: t, Classification: ClassRefusal}
	}
	// 多词回答判 invalid（论文：invalid = off answer space or multi-word）
	if len(strings.Fields(t)) > 1 {
		return Result{Normalized: FirstToken(t), Classification: ClassInvalid}
	}
	// 任务语义规范化（颜色/硬币跨语言词表；此处必为单 token）
	switch {
	case isColorTask(task.ID):
		t = colorCanonical(t)
	case task.ID == "coin_flip":
		t = coinCanonical(t)
	}
	if task.AnswerSpace == "closed" {
		if inClosedSpace(t, task) {
			return Result{Normalized: t, Classification: ClassValid}
		}
		return Result{Normalized: t, Classification: ClassInvalid}
	}
	return Result{Normalized: t, Classification: ClassValid}
}

// Normalize 执行确定性归一化管线：NFC → 剥离标点/引号/符号 → 小写 → 数字映射。
func Normalize(raw string) string {
	s := norm.NFC.String(raw)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			continue // 标点、引号（属 Punct 类）、emoji 等符号（Symbol 类）一律剥离
		}
		b.WriteRune(r)
	}
	s = strings.ToLower(b.String())
	return mapDigits(s)
}

// mapDigits 将阿拉伯-印度（٠-٩ / ۰-۹）与全角（０-９）数字映射为 ASCII 数字。
// 中文数字由 ChineseToDigits 单独处理（数词组合需整体解析，不能逐字符映射）。
func mapDigits(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= '\u0660' && r <= '\u0669': // 阿拉伯-印度
			b.WriteRune(rune('0' + (r - '\u0660')))
		case r >= '\u06F0' && r <= '\u06F9': // 波斯-印度
			b.WriteRune(rune('0' + (r - '\u06F0')))
		case r >= '\uFF10' && r <= '\uFF19': // 全角
			b.WriteRune(rune('0' + (r - '\uFF10')))
		default:
			b.WriteRune(r)
		}
	}
	s = b.String()
	return ChineseToDigits(s)
}

// chineseDigits 是中文数字字符 → 数值。
var chineseDigits = map[rune]int64{
	'零': 0, '〇': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4,
	'五': 5, '六': 6, '七': 7, '八': 8, '九': 9,
}

// chineseUnits 是中文单位（十百千）。
var chineseUnits = map[rune]int64{'十': 10, '百': 100, '千': 1000}

// ChineseToDigits 将中文数字串映射为拉丁数字串。
//   - 纯数字串（一二三）：按字符级映射 → "123"；
//   - 数词组合（四十二、一百零五）：按数词文法解析 → "42"、"105"；
//   - 无法整体解析（含非中文数字字符）：返回原串。
func ChineseToDigits(s string) string {
	if allChineseDigitRunes(s) {
		var b strings.Builder
		for _, r := range s {
			if d, ok := chineseDigits[r]; ok {
				b.WriteString(strconv.FormatInt(d, 10))
			}
		}
		return b.String()
	}
	if v, ok := parseChineseNumber(s); ok {
		return strconv.FormatInt(v, 10)
	}
	return s
}

// allChineseDigitRunes 判断字符串是否全为中文数字字符。
func allChineseDigitRunes(s string) bool {
	for _, r := range s {
		if _, ok := chineseDigits[r]; !ok {
			return false
		}
	}
	return s != ""
}

// parseChineseNumber 按数词文法解析中文数字，支持亿级分段组合
// （"十二"→12、"二十"→20、"一百零五"→105、"十万"→100000、
//
//	"一万亿"→1e12、"一亿五千万"→1.5e8）。
//
// 已知边界（口语省略形，非标准文法，结果按位解析）："二百五"→205（口语 250）、
// "一千五"→1005（口语 1500）、"两万五"→20005（口语 25000）。
func parseChineseNumber(s string) (int64, bool) {
	var total, section, number int64
	valid := false
	for _, r := range s {
		if d, ok := chineseDigits[r]; ok {
			number = d
			valid = true
			continue
		}
		if u, ok := chineseUnits[r]; ok {
			if number == 0 && section == 0 {
				number = 1 // "十" → 10
			}
			section += number * u
			number = 0
			valid = true
			continue
		}
		switch r {
		case '万':
			section = (section + number) * 10000
			total += section
			section, number = 0, 0
			valid = true
		case '亿':
			// 亿 = 当前累计（含万段）整体 × 1e8；亿后仍可接万段（一亿五千万）
			total = (total + section + number) * 100000000
			section, number = 0, 0
			valid = true
		default:
			return 0, false // 含非中文数字字符
		}
	}
	total += section + number
	return total, valid
}

// FirstToken 取第一个非空白 token（Unicode 空白切分启发式）。
// 无空格语言（中文等）整串即首 token。
func FirstToken(s string) string {
	for _, f := range strings.Fields(s) {
		return f
	}
	return ""
}

// IsRefusal 判定是否为明确拒绝回答。
// 英文用词边界正则（防 -cant/-refus 后缀普通词误报，如 vacant/lubricant）；
// 中文/俄/阿为多字词，包含匹配误伤风险低。归一化已剥离标点（i'm→im），
// 故模式同时覆盖有/无撇号形态。
func IsRefusal(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	for _, re := range refusalPatterns {
		if re.MatchString(lower) {
			return true
		}
	}
	for _, p := range refusalContains {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// refusalPatterns 是英文拒绝模式（词边界正则）。
var refusalPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bsorry\b`),
	regexp.MustCompile(`\bcannot\b`),
	regexp.MustCompile(`\bcan't\b`),
	regexp.MustCompile(`\bcant\b`),
	regexp.MustCompile(`\bunable\b`),
	regexp.MustCompile(`\brefus\w*\b`), // refuse/refusing/refused/refusal
	regexp.MustCompile(`\bas an ai\b`),
	regexp.MustCompile(`\bnot able\b`),
	regexp.MustCompile(`\bdon't know\b`),
	regexp.MustCompile(`\bdont know\b`),
}

// refusalContains 是中文/俄/阿拒绝模式（包含匹配；多字词误伤风险低）。
var refusalContains = []string{
	"抱歉", "对不起", "不好意思", "无法", "不能", "拒绝", "不知道", "我不会",
	"извини", "не могу", "отказ", "как ии", "не знаю",
	"آسف", "عذرا", "لا أستطيع", "لا يمكن", "كذكاء اصطناعي",
	"لااستطيع", "لا استطيع", "لايمكن", "لا أعرف",
}

// inClosedSpace 判定归一化后的首 token 是否落在闭空间内。
func inClosedSpace(s string, task Task) bool {
	switch {
	case strings.HasPrefix(task.ID, "random_number_"):
		v, err := strconv.Atoi(s)
		return err == nil && v >= 1 && v <= task.SpaceSize
	case task.ID == "random_letter":
		if s == "" || utf8.RuneCountInString(s) != 1 {
			return false
		}
		r, _ := utf8.DecodeRuneInString(s)
		return unicode.IsLetter(r)
	case task.ID == "coin_flip":
		return s == "h" || s == "t" // 论文 canonical 码（M1.5 pin）
	}
	return false
}

// isColorTask 判断任务是否为颜色任务（random/favorite color）。
func isColorTask(id string) bool {
	return id == "random_color" || id == "favorite_color"
}

// colorLexicon 是跨语言颜色词表：各语言颜色词 → 规范码（英文小写）。
// 用于将四种语言的同一颜色映射到同一规范码，使跨语言分布可比（论文 §IV-B）。
// 高频色（violet/magenta 等）在论文 canonical 中为独立码（M1.5 pin 后不再归并基础色）；
// 词表外颜色保留原 token（既定行为，M1.6 重放时可按真实分布补充）。
// colorLexicon 是跨语言颜色词表：各语言颜色词 → 论文 canonical 码（22 码：red/blue/green/yellow/
// orange/purple/violet/pink/black/white/gray/brown/cyan/turquoise/magenta/indigo/teal/gold/silver/
// azure/crimson/emerald）。来源：论文软件归档 stats/color-lexicon.json（M1.5 pin，4 语言合并），
// 另保留本项目扩展键（超集）。词表外的颜色保留原 token（既定行为）。
// 注意：canonical 码集为论文定义，本项目扩展键（beige/navy 等）非论文 canonical，
// 跨语言可比性以论文 22 码为准。
var colorLexicon = map[string]string{
	"azure":       "azure",     // azure
	"голубой":     "azure",     // azure
	"سماوي":       "azure",     // azure
	"天蓝色":         "azure",     // azure
	"beige":       "beige",     // beige
	"бежевый":     "beige",     // beige
	"بيج":         "beige",     // beige
	"米色":          "beige",     // beige
	"black":       "black",     // black
	"черный":      "black",     // black
	"чёрный":      "black",     // black
	"أسود":        "black",     // black
	"اسود":        "black",     // black
	"黑":           "black",     // black
	"黑色":          "black",     // black
	"blue":        "blue",      // blue
	"navy":        "blue",      // blue
	"синий":       "blue",      // blue
	"أزرق":        "blue",      // blue
	"ازرق":        "blue",      // blue
	"蓝":           "blue",      // blue
	"蓝色":          "blue",      // blue
	"brown":       "brown",     // brown
	"maroon":      "brown",     // brown
	"бордовый":    "brown",     // brown
	"коричневый":  "brown",     // brown
	"بني":         "brown",     // brown
	"عنابي":       "brown",     // brown
	"栗色":          "brown",     // brown
	"棕":           "brown",     // brown
	"棕色":          "brown",     // brown
	"褐色":          "brown",     // brown
	"crimson":     "crimson",   // crimson
	"багровый":    "crimson",   // crimson
	"قرمزي":       "crimson",   // crimson
	"深红":          "crimson",   // crimson
	"cyan":        "cyan",      // cyan
	"تركوازي":     "cyan",      // cyan
	"绿松石":         "cyan",      // cyan
	"青":           "cyan",      // cyan
	"青色":          "cyan",      // cyan
	"emerald":     "emerald",   // emerald
	"изумрудный":  "emerald",   // emerald
	"زمردي":       "emerald",   // emerald
	"翡翠绿":         "emerald",   // emerald
	"gold":        "gold",      // gold
	"золотой":     "gold",      // gold
	"ذهبي":        "gold",      // gold
	"金":           "gold",      // gold
	"金色":          "gold",      // gold
	"gray":        "gray",      // gray
	"grey":        "gray",      // gray
	"серый":       "gray",      // gray
	"رمادي":       "gray",      // gray
	"灰":           "gray",      // gray
	"灰色":          "gray",      // gray
	"green":       "green",     // green
	"lime":        "green",     // green
	"olive":       "green",     // green
	"зеленый":     "green",     // green
	"зелёный":     "green",     // green
	"салатовый":   "green",     // green
	"أخضر":        "green",     // green
	"اخضر":        "green",     // green
	"橄榄绿":         "green",     // green
	"祖母绿":         "green",     // green
	"绿":           "green",     // green
	"绿色":          "green",     // green
	"青柠":          "green",     // green
	"indigo":      "indigo",    // indigo
	"индиго":      "indigo",    // indigo
	"نيلي":        "indigo",    // indigo
	"靛蓝":          "indigo",    // indigo
	"magenta":     "magenta",   // magenta
	"пурпурный":   "magenta",   // magenta
	"品红":          "magenta",   // magenta
	"тёмно-синий": "navy",      // navy
	"тёмносиний":  "navy",      // navy
	"كحلي":        "navy",      // navy
	"藏青":          "navy",      // navy
	"amber":       "orange",    // orange
	"coral":       "orange",    // orange
	"orange":      "orange",    // orange
	"оранжевый":   "orange",    // orange
	"янтарный":    "orange",    // orange
	"برتقالي":     "orange",    // orange
	"橘色":          "orange",    // orange
	"橙":           "orange",    // orange
	"橙色":          "orange",    // orange
	"琥珀":          "orange",    // orange
	"pink":        "pink",      // pink
	"розовый":     "pink",      // pink
	"زهري":        "pink",      // pink
	"وردي":        "pink",      // pink
	"粉":           "pink",      // pink
	"粉红色":         "pink",      // pink
	"粉色":          "pink",      // pink
	"lavender":    "purple",    // purple
	"purple":      "purple",    // purple
	"фиолетовый":  "purple",    // purple
	"بنفسجي":      "purple",    // purple
	"紫":           "purple",    // purple
	"紫罗兰":         "purple",    // purple
	"紫色":          "purple",    // purple
	"red":         "red",       // red
	"scarlet":     "red",       // red
	"красный":     "red",       // red
	"أحمر":        "red",       // red
	"احمر":        "red",       // red
	"红":           "red",       // red
	"红色":          "red",       // red
	"silver":      "silver",    // silver
	"серебряный":  "silver",    // silver
	"فضي":         "silver",    // silver
	"银":           "silver",    // silver
	"银色":          "silver",    // silver
	"teal":        "teal",      // teal
	"turquoise":   "turquoise", // turquoise
	"бирюзовый":   "turquoise", // turquoise
	"فيروزي":      "turquoise", // turquoise
	"绿松石色":        "turquoise", // turquoise
	"violet":      "violet",    // violet
	"лиловый":     "violet",    // violet
	"أرجواني":     "violet",    // violet
	"white":       "white",     // white
	"белый":       "white",     // white
	"أبيض":        "white",     // white
	"ابيض":        "white",     // white
	"白":           "white",     // white
	"白色":          "white",     // white
	"yellow":      "yellow",    // yellow
	"желтый":      "yellow",    // yellow
	"жёлтый":      "yellow",    // yellow
	"أصفر":        "yellow",    // yellow
	"اصفر":        "yellow",    // yellow
	"黄":           "yellow",    // yellow
	"黄色":          "yellow",    // yellow
}

// colorCanonical 将颜色回答映射为规范码（整串优先，再试首 token）。
func colorCanonical(s string) string {
	if c, ok := colorLexicon[s]; ok {
		return c
	}
	if f := FirstToken(s); f != "" {
		if c, ok := colorLexicon[f]; ok {
			return c
		}
	}
	return s
}

// coinLexicon 是硬币结果跨语言词表 → 规范码。
// coinLexicon 是硬币结果跨语言词表 → 论文 canonical 码 h/t（论文 01-normalize.js COIN 表：
// en heads/tails→h/t、ru орёл/орел/решка、zh 正面/正/反面/反、ar صورة/كتابة；M1.5 pin）。
var coinLexicon = map[string]string{
	"head": "h", "heads": "h", "tail": "t", "tails": "t",
	"正面": "h", "正": "h", "反面": "t", "反": "t",
	"орел": "h", "орёл": "h", "решка": "t",
	"صورة": "h", "كتابة": "t",
}

// coinCanonical 将硬币回答映射为论文 canonical 码 h/t（M1.5 pin：论文 01-normalize.js COIN 表）。
func coinCanonical(s string) string {
	if c, ok := coinLexicon[s]; ok {
		return c
	}
	if f := FirstToken(s); f != "" {
		if c, ok := coinLexicon[f]; ok {
			return c
		}
	}
	return s
}
