// Package report 生成报告（M3.3 单端点/矩阵报告 + M2.10 compare 比较报告）。
//
// 安全基线：HTML 输出经 html/template **默认上下文感知转义**（模型输出、
// 端点 URL、flags 等动态文本只进文本节点，恶意注入 HTML/JS 无效）；
// 不引入 template.HTML/template.JS（会绕过转义）；条图宽度为模板算术
// 生成的纯数字百分比（非用户输入）。
package report

import (
	"bytes"
	"html/template"
	"sort"
	"strings"
)

// Freq 是单个 token 的频率（分布对比行）。
type Freq struct {
	Token string  // 模型输出（只进文本节点，自动转义）
	Freq  float64 // 0..1
	Pct   float64 // 0..100（条图宽度，纯数字）
}

// EndpointQC 是端点测量信息（报告「两端点 QC」区）。
type EndpointQC struct {
	Endpoint   string   // base_url（文本节点）
	Provider   string   // 上游 provider（X-Openrouter-Via 等）
	Flags      []string // 探测器 flags
	ValidRate  float64  // 有效率百分比（0..100；模板直接 printf）
	ValidCells int
	TotalCells int
	Samples    int
}

// RefLine 是距离图上的参考线（三参考线）或生效判定线。
type RefLine struct {
	Label    string
	Value    float64 // JSD 值（0..1）
	Pct      float64 // 0..100 条图位置（模板算术）
	Decision bool    // 生效判定线（红色实线）vs 参考线（灰色虚线）
}

// CellRow 是逐 cell JSD 行（排序表）。
type CellRow struct {
	Cell string
	JSD  float64
}

// DistPair 是单个 cell 的分布对比（参考 vs 待测 top tokens）。
type DistPair struct {
	Cell string
	Ref  []Freq
	Tgt  []Freq
}

// CompareData 是 compare 比较报告渲染输入。
type CompareData struct {
	GeneratedAt    string
	Verdict        string // pass|suspicious|inconclusive
	Score          float64
	ScorePct       float64 // 距离条图宽度（纯数字）
	Threshold      float64
	TauSource      string // override|calibration|builtin
	TauSourceLabel string // 「--tau 直传」/「校准档」/「内置参考线（未校准）」
	IsBuiltin      bool   // 内置线标注「未校准」风险提示
	CellsUsed      int
	KMinCells      int
	Channel        string
	Reason         string
	Ref            EndpointQC
	Target         EndpointQC
	Upstream       string    // 待测端上游 provider
	RefLines       []RefLine // 三参考线：0.075 同模型分裂半 / 0.140 噪声底线 / 0.227 跨 provider
	CellJSDs       []CellRow // 逐 cell JSD（降序）
	DistPairs      []DistPair
}

// Percent 返回 v（0..1）的条图百分比（0..100，限制 ≤100 防溢出布局）。
func Percent(v float64) float64 {
	if v > 1 {
		return 100
	}
	return v * 100
}

// TopTokens 从分布计数取 top n token 频率（归一化；n<=0 取全部）。
// 供 compare 命令准备 DistPairs（report 包不直接接触 store 类型，保持渲染纯净）。
func TopTokens(dist map[string]int, n int) []Freq {
	total := 0
	for _, c := range dist {
		total += c
	}
	if total == 0 {
		return nil
	}
	type kv struct {
		k string
		c int
	}
	ks := make([]kv, 0, len(dist))
	for k, c := range dist {
		ks = append(ks, kv{k, c})
	}
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].c != ks[j].c {
			return ks[i].c > ks[j].c
		}
		return ks[i].k < ks[j].k
	})
	if n > 0 && len(ks) > n {
		ks = ks[:n]
	}
	out := make([]Freq, 0, len(ks))
	for _, e := range ks {
		f := float64(e.c) / float64(total)
		out = append(out, Freq{Token: e.k, Freq: f, Pct: f * 100})
	}
	return out
}

// SortCellJSDs 返回按 JSD 降序排序的逐 cell 行（compare 命令准备数据用）。
func SortCellJSDs(m map[string]float64) []CellRow {
	out := make([]CellRow, 0, len(m))
	for c, v := range m {
		out = append(out, CellRow{Cell: c, JSD: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JSD > out[j].JSD })
	return out
}

const compareTmpl = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>onetoken compare 报告</title>
<style>
  body { font-family: system-ui, -apple-system, "Segoe UI", sans-serif; margin: 2rem auto; max-width: 960px; padding: 0 1rem; color: #1a1a1a; }
  h1 { font-size: 1.4rem; } h2 { font-size: 1.1rem; margin-top: 2rem; border-bottom: 1px solid #ddd; padding-bottom: .3rem; }
  .verdict { display: inline-block; padding: .2rem .8rem; border-radius: 4px; font-weight: 700; color: #fff; }
  .verdict.pass { background: #2e8b57; } .verdict.suspicious { background: #c0392b; } .verdict.inconclusive { background: #b8860b; }
  table { border-collapse: collapse; width: 100%; margin: .5rem 0; }
  th, td { border: 1px solid #ccc; padding: .3rem .5rem; text-align: left; font-size: .9rem; }
  th { background: #f5f5f5; }
  .barwrap { position: relative; height: 24px; background: #eee; border-radius: 3px; margin: .5rem 0; }
  .bar { position: absolute; left: 0; top: 0; height: 100%; background: #4a90d9; border-radius: 3px; }
  .refline { position: absolute; top: -4px; bottom: -4px; width: 2px; background: #999; }
  .refline.decision { background: #c0392b; width: 3px; }
  .refcell { margin-left: .4rem; font-size: .8rem; color: #555; }
  .dist td { vertical-align: top; }
  .freqbar { display: inline-block; height: 10px; background: #4a90d9; vertical-align: middle; border-radius: 2px; }
  .freqbar.tgt { background: #e67e22; }
  .warn { background: #fff8e1; border: 1px solid #f0d060; padding: .6rem .9rem; border-radius: 4px; margin: .8rem 0; }
  code { background: #f0f0f0; padding: 0 .2rem; border-radius: 3px; font-size: .85em; }
</style>
</head>
<body>
<h1>端点直比报告（compare）</h1>
<p>生成时间：{{.GeneratedAt}}　通道：{{.Channel}}</p>

<h2>判定</h2>
<div class="verdict {{.Verdict}}">{{.Verdict}}</div>
<ul>
  <li>距离（JSD）：<strong>{{printf "%.4f" .Score}}</strong>（共同有效 cell {{.CellsUsed}}，k_min {{.KMinCells}}）</li>
  <li>判定阈值 τ：{{printf "%.4f" .Threshold}}（{{.TauSourceLabel}}）</li>
  {{if .Reason}}<li>说明：{{.Reason}}</li>{{end}}
</ul>
{{if .IsBuiltin}}
<div class="warn">⚠️ <strong>内置参考线（未校准）</strong>：τ={{printf "%.4f" .Threshold}} 为 M1.6 实测距离基线（中位数口径），<strong>不是</strong>校准出的 FPR≈1% 操作点，误报/漏报率未知；跨 provider 同模型距离中位 0.227 高于 direct 内置线 0.140，健康端点可能判 suspicious（服务栈差异而非替换证据）。正式使用前建议执行 calibrate 校准。</div>
{{end}}

<h2>距离与参考线</h2>
<div class="barwrap">
  <div class="bar" style="width: {{.ScorePct}}%"></div>
  {{range .RefLines}}<div class="refline{{if .Decision}} decision{{end}}" style="left: {{.Pct}}%" title="{{.Label}} {{printf "%.3f" .Value}}"></div>{{end}}
</div>
<div class="refcell">
  {{range .RefLines}}<span style="margin-right:1rem">{{if .Decision}}▍判定线{{else}}参考线{{end}} {{.Label}}（{{printf "%.3f" .Value}}）</span>{{end}}
</div>

<h2>端点测量信息（QC）</h2>
<table>
  <tr><th></th><th>参考端点</th><th>待测端点</th></tr>
  <tr><td>端点</td><td>{{.Ref.Endpoint}}</td><td>{{.Target.Endpoint}}</td></tr>
  <tr><td>上游 provider</td><td>{{.Ref.Provider}}</td><td>{{.Target.Provider}}</td></tr>
  <tr><td>有效率</td><td>{{printf "%.1f%%" .Ref.ValidRate}}</td><td>{{printf "%.1f%%" .Target.ValidRate}}</td></tr>
  <tr><td>有效 cell</td><td>{{.Ref.ValidCells}}/{{.Ref.TotalCells}}</td><td>{{.Target.ValidCells}}/{{.Target.TotalCells}}</td></tr>
  <tr><td>采样数</td><td>{{.Ref.Samples}}</td><td>{{.Target.Samples}}</td></tr>
  <tr><td>QC flags</td><td>{{range .Ref.Flags}}<code>{{.}}</code> {{end}}</td><td>{{range .Target.Flags}}<code>{{.}}</code> {{end}}</td></tr>
  {{if .Upstream}}<tr><td>待测上游透传</td><td colspan="2">{{.Upstream}}</td></tr>{{end}}
</table>

<h2>逐 cell JSD（降序）</h2>
<table>
  <tr><th>cell</th><th>JSD</th></tr>
  {{range .CellJSDs}}<tr><td>{{.Cell}}</td><td>{{printf "%.4f" .JSD}}</td></tr>{{end}}
</table>

<h2>分布对比（top tokens）</h2>
{{range .DistPairs}}
<h3>{{.Cell}}</h3>
<table class="dist">
  <tr><th>参考端点</th><th>待测端点</th></tr>
  <tr><td>
    {{if .Ref}}{{range .Ref}}<div><span class="freqbar" style="width: {{.Pct}}px"></span> <code>{{.Token}}</code> {{printf "%.1f%%" .Freq}}</div>{{end}}{{else}}<em>无有效样本</em>{{end}}
  </td><td>
    {{if .Tgt}}{{range .Tgt}}<div><span class="freqbar tgt" style="width: {{.Pct}}px"></span> <code>{{.Token}}</code> {{printf "%.1f%%" .Freq}}</div>{{end}}{{else}}<em>无有效样本</em>{{end}}
  </td></tr>
</table>
{{end}}
</body>
</html>`

// CompareReport 渲染比较报告 HTML（模板默认转义；返回完整 HTML 文档）。
func CompareReport(d CompareData) (string, error) {
	tmpl, err := template.New("compare").Parse(compareTmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, d); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// TauSourceLabel 生成 τ 来源标签（内置线带「未校准」警示）。
func TauSourceLabel(src string) string {
	switch src {
	case "override":
		return "--tau 直传（用户指定）"
	case "calibration":
		return "校准档（(k,n,scope,通道) 匹配）"
	case "builtin":
		return "内置参考线（未校准）"
	default:
		return strings.ToUpper(src)
	}
}
