package report

import (
	"strings"
	"testing"
)

// testData 构造一份典型比较报告数据（builtin τ 路径，覆盖未校准警示）。
func testData() CompareData {
	return CompareData{
		GeneratedAt:    "2026-08-09T00:00:00Z",
		Verdict:        "suspicious",
		Score:          0.32,
		ScorePct:       32,
		Threshold:      0.14,
		TauSource:      "builtin",
		TauSourceLabel: "内置参考线（未校准）",
		IsBuiltin:      true,
		CellsUsed:      30,
		KMinCells:      3,
		Channel:        "direct",
		Reason:         "",
		Ref:            EndpointQC{Endpoint: "https://ref.example", Provider: "dashscope", ValidRate: 95, ValidCells: 30, TotalCells: 30, Samples: 300},
		Target:         EndpointQC{Endpoint: "https://tgt.example", Provider: "openrouter", ValidRate: 90, ValidCells: 29, TotalCells: 30, Samples: 300},
		Upstream:       "deepseek",
		RefLines: []RefLine{
			{Label: "同模型分裂半", Value: 0.075, Pct: 7.5},
			{Label: "噪声底线", Value: 0.140, Pct: 14},
			{Label: "跨 provider 服务栈", Value: 0.227, Pct: 22.7},
			{Label: "生效判定线 τ", Value: 0.14, Pct: 14, Decision: true},
		},
		CellJSDs: []CellRow{{Cell: "sum:en", JSD: 0.52}, {Cell: "color:zh", JSD: 0.11}},
		DistPairs: []DistPair{
			{Cell: "sum:en",
				Ref: []Freq{{Token: "42", Freq: 0.5, Pct: 50}, {Token: "57", Freq: 0.5, Pct: 50}},
				Tgt: []Freq{{Token: "13", Freq: 0.5, Pct: 50}}},
		},
	}
}

func TestCompareReportRenders(t *testing.T) {
	html, err := CompareReport(testData())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<!DOCTYPE html>", "suspicious", "32", "0.1400", "内置参考线（未校准）",
		"未校准", "0.227", "sum:en", "42", "13", "deepseek", "95.0%",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("报告缺少 %q", want)
		}
	}
	// 生成时间/通道
	if !strings.Contains(html, "2026-08-09T00:00:00Z") || !strings.Contains(html, "direct") {
		t.Error("报告缺少元信息")
	}
}

// TestCompareReportXSS：恶意模型输出/端点必须被转义（html/template 默认转义，
// 模型输出只进文本节点，防注入）。审查验收：XSS 注入用例。
func TestCompareReportXSS(t *testing.T) {
	evil := `<script>alert(1)</script><img src=x onerror=alert(2)>`
	d := testData()
	d.Target.Endpoint = "https://tgt.example/" + evil
	d.Ref.Flags = []string{`"><script>bad()</script>`}
	d.DistPairs[0].Tgt[0].Token = evil
	d.CellJSDs[0].Cell = `x"><svg/onload=alert(3)>`
	html, err := CompareReport(d)
	if err != nil {
		t.Fatal(err)
	}
	// 恶意原始串必须被转义（&lt;script&gt;），不得原样出现在 HTML 中
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Fatal("XSS: 恶意 endpoint 未转义")
	}
	if strings.Contains(html, "<img src=x onerror=alert(2)>") {
		t.Fatal("XSS: 恶意 endpoint 未转义 img")
	}
	if strings.Contains(html, `"><script>bad()</script>`) {
		t.Fatal("XSS: 恶意 flag 未转义")
	}
	if strings.Contains(html, "<svg/onload=alert(3)>") {
		t.Fatal("XSS: 恶意 cell 名未转义")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Error("转义后的实体应存在")
	}
}

func TestPercentClamp(t *testing.T) {
	if got := Percent(0.5); got != 50 {
		t.Fatalf("Percent(0.5)=%v，期望 50", got)
	}
	if got := Percent(1.5); got != 100 {
		t.Fatalf("Percent(1.5) 应钳位到 100，实际 %v", got)
	}
}

func TestTopTokensSortAndNormalize(t *testing.T) {
	freqs := TopTokens(map[string]int{"b": 30, "a": 10, "c": 60}, 2)
	if len(freqs) != 2 {
		t.Fatalf("应取 top2，实际 %d", len(freqs))
	}
	if freqs[0].Token != "c" || freqs[1].Token != "b" {
		t.Fatalf("top tokens 排序错误: %+v", freqs)
	}
	if freqs[0].Freq != 0.6 || freqs[1].Freq != 0.3 {
		t.Fatalf("频率归一化错误: %+v", freqs)
	}
	if len(TopTokens(map[string]int{}, 5)) != 0 {
		t.Fatal("空分布应返回 nil")
	}
}

func TestSortCellJSDs(t *testing.T) {
	rows := SortCellJSDs(map[string]float64{"a": 0.1, "b": 0.9, "c": 0.5})
	if len(rows) != 3 || rows[0].Cell != "b" || rows[2].Cell != "a" {
		t.Fatalf("JSD 降序排序错误: %+v", rows)
	}
}

func TestTauSourceLabel(t *testing.T) {
	if got := TauSourceLabel("builtin"); !strings.Contains(got, "未校准") {
		t.Fatalf("builtin 标签应含未校准: %q", got)
	}
	if got := TauSourceLabel("override"); !strings.Contains(got, "--tau") {
		t.Fatalf("override 标签应含 --tau: %q", got)
	}
	if got := TauSourceLabel("calibration"); !strings.Contains(got, "校准档") {
		t.Fatalf("calibration 标签应含校准档: %q", got)
	}
}
