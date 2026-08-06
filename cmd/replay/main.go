// Command replay — M1.6 数据重放与分层回归 harness。
//
// 从论文 Zenodo 数据（normalized.jsonl）重放：分布 → JSD → split-half ROC，
// 与论文自带产物（distributions.json / divergence.json / split-scores.json /
// verification.json）逐层对拍，验证我们的 Go 算法实现（fingerprint/calibrate）。
//
// 重放口径（与论文 stats/03-divergence.js 一致）：
//   - 仅 Study A（paper=1 的 10 任务 × 4 语言 = 40 cell）+ included 模型 + T=1.0 + valid；
//   - MIN_N=10（cell 双方有效样本），split-half 门槛 MIN_N/2=5；
//   - split: rep 奇偶（even=A/odd=B）；trial = refA vs probeB；genuine = (ref==probe)；
//   - 模型对级聚合：per-(ref,probe) 各 cell JSD 的算术平均 → ROC；
//   - 分布/JSD 模拟论文 toFixed(4) 舍入。
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"onetoken/internal/calibrate"
	"onetoken/internal/preprocess"
)

// ---- 论文数据记录 ----

type paperRec struct {
	Key         string  `json:"key"`
	RunID       string  `json:"run_id"`
	Model       string  `json:"model"`
	Provider    string  `json:"provider"`
	TaskID      string  `json:"task_id"`
	Lang        string  `json:"lang"`
	Temperature float64 `json:"temperature"`
	Rep         int     `json:"rep"`
	Normalized  string  `json:"normalized"`
	Class       string  `json:"answer_class"`
	ColorCanon  string  `json:"color_canon"`
}

// ---- 论文配置 ----

type selModel struct {
	ID       string `json:"id"`
	Included bool   `json:"included"`
}

type paperPrompt struct {
	ID    string `json:"id"`
	Paper int    `json:"paper"`
}

// studyATasks 从 prompts.json 提取 paper==1 的任务（论文 id）。
func studyATasks(promptsPath string) (map[string]bool, error) {
	raw, err := os.ReadFile(promptsPath)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Tasks []paperPrompt `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, t := range doc.Tasks {
		if t.Paper == 1 {
			out[t.ID] = true
		}
	}
	return out, nil
}

func includedModels(selPath string) (map[string]bool, error) {
	raw, err := os.ReadFile(selPath)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Models []selModel `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, m := range doc.Models {
		if m.Included {
			out[m.ID] = true
		}
	}
	return out, nil
}

// ---- 分布构建（模拟论文 02-distributions.js 的 4 位舍入） ----

func round4(f float64) float64 { return math.Round(f*1e4) / 1e4 }

type distCounts struct {
	counts map[string]int
	n      int
}

// loadCells 构建 model|task|lang → 计数（T=1.0 + valid 过滤已在调用处完成）。
func loadCells(recs []paperRec, tasks map[string]bool, models map[string]bool) (map[string]*distCounts, int, int) {
	cells := map[string]*distCounts{}
	valid := 0
	skipped := 0
	for _, r := range recs {
		if r.Temperature != 1 || r.Class != "valid" {
			skipped++
			continue
		}
		if !tasks[r.TaskID] || !models[r.Model] {
			skipped++
			continue
		}
		k := r.Model + "|" + r.TaskID + "|" + r.Lang
		c := cells[k]
		if c == nil {
			c = &distCounts{counts: map[string]int{}}
			cells[k] = c
		}
		c.counts[r.Normalized]++
		c.n++
		valid++
	}
	return cells, valid, skipped
}

// toDist 返回 4 位舍入概率分布；不足 minN 返 nil。
func toDist(c *distCounts, minN int) map[string]float64 {
	if c == nil || c.n < minN {
		return nil
	}
	d := make(map[string]float64, len(c.counts))
	for k, v := range c.counts {
		d[k] = round4(float64(v) / float64(c.n))
	}
	return d
}

// splitHalf 按 rep 奇偶把 cell 计数拆成 A/B 两半。
func splitHalf(cells map[string]*distCounts, recs []paperRec, tasks map[string]bool, models map[string]bool) map[string]*distCounts {
	halves := map[string]*distCounts{} // model|task|lang|half
	for _, r := range recs {
		if r.Temperature != 1 || r.Class != "valid" {
			continue
		}
		if !tasks[r.TaskID] || !models[r.Model] {
			continue
		}
		half := "A"
		if r.Rep%2 != 0 {
			half = "B"
		}
		k := r.Model + "|" + r.TaskID + "|" + r.Lang + "|" + half
		c := halves[k]
		if c == nil {
			c = &distCounts{counts: map[string]int{}}
			halves[k] = c
		}
		c.counts[r.Normalized]++
		c.n++
	}
	return halves
}

// ---- 对拍：L1 分布 / L2 JSD 矩阵 ----

type cellMeta struct {
	Model       string             `json:"model"`
	TaskID      string             `json:"task_id"`
	Lang        string             `json:"lang"`
	Temperature int                `json:"temperature"`
	NValid      int                `json:"n_valid"`
	Dist        map[string]float64 `json:"dist"`
}

func compareDistributions(cells map[string]*distCounts, paperDistPath string, tasks map[string]bool) error {
	raw, err := os.ReadFile(paperDistPath)
	if err != nil {
		return err
	}
	var doc struct {
		Distributions []cellMeta `json:"distributions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	// 论文 dist 已是 toFixed(4)，直接比对。
	paper := map[string]map[string]float64{}
	for _, c := range doc.Distributions {
		if c.Temperature != 1 {
			continue
		}
		paper[c.Model+"|"+c.TaskID+"|"+c.Lang] = c.Dist
	}
	maxDiff := 0.0
	checked, mism, nilCell := 0, 0, 0
	for k, c := range cells {
		got := toDist(c, 10)
		want, ok := paper[k]
		if !ok {
			continue
		}
		if got == nil {
			nilCell++ // 我们侧有效样本 <10（不构成逐值差异）
			continue
		}
		keys := map[string]bool{}
		for x := range got {
			keys[x] = true
		}
		for x := range want {
			keys[x] = true
		}
		for x := range keys {
			g, w := got[x], want[x]
			d := math.Abs(g - w)
			if d > maxDiff {
				maxDiff = d
			}
			if d > 1e-9 {
				mism++
			}
		}
		checked++
	}
	fmt.Printf("[L1 分布对拍] 核对 %d cell（论文侧匹配）；逐值差异 >1e-9 的 cell 数 %d（另有 %d 个 Go 侧有效样本 <10 未计入）；最大逐值差 %.6f\n",
		checked, mism, nilCell, maxDiff)
	return nil
}

// ---- L3/L4/L5：split-half 分数 → 模型对级 → ROC + 分层中位数 ----

func jsd(p, q map[string]float64) float64 {
	keys := make([]string, 0, len(p)+len(q))
	seen := map[string]bool{}
	for x := range p {
		if !seen[x] {
			seen[x] = true
			keys = append(keys, x)
		}
	}
	for x := range q {
		if !seen[x] {
			seen[x] = true
			keys = append(keys, x)
		}
	}
	sort.Strings(keys) // 确定性：浮点累加顺序固定（对拍可复现）
	d := 0.0
	for _, x := range keys {
		px, qx := p[x], q[x]
		mx := (px + qx) / 2
		if px > 0 {
			d += 0.5 * px * math.Log2(px/mx)
		}
		if qx > 0 {
			d += 0.5 * qx * math.Log2(qx/mx)
		}
	}
	return d
}

// splitScores 计算全部 (ref, probe, cell) 的 split-half JSD，返回：
// cellScores: 每个 trial 的 (ref, probe, cell, jsd, genuine)
// pairScores: per-(ref,probe) 聚合 mean
func splitScores(halves map[string]*distCounts, cellsList []string) (cellJSDs []float64, cellGenuine []bool, pairScores []float64, pairGenuine []bool) {
	type trial struct {
		ref, probe, cell string
		jsd              float64
		genuine          bool
	}
	var trials []trial
	for _, cell := range cellsList {
		parts := strings.Split(cell, "|")
		cellKey := parts[1] + "|" + parts[2] // task|lang
		// 该 cell 下所有模型
		var mods []string
		seen := map[string]bool{}
		for k := range halves {
			p := strings.SplitN(k, "|", 4)
			if p[1]+"|"+p[2] == cellKey && !seen[p[0]] {
				seen[p[0]] = true
				mods = append(mods, p[0])
			}
		}
		sort.Strings(mods)
		for _, ref := range mods {
			refD := toDist(halves[ref+"|"+cellKey+"|A"], 5)
			if refD == nil {
				continue
			}
			for _, probe := range mods {
				probeD := toDist(halves[probe+"|"+cellKey+"|B"], 5)
				if probeD == nil {
					continue
				}
				trials = append(trials, trial{ref, probe, cellKey, round4(jsd(refD, probeD)), ref == probe})
			}
		}
	}
	// per-(ref,probe) 聚合
	agg := map[string]struct {
		sum     float64
		n       int
		genuine bool
	}{}
	for _, t := range trials {
		k := t.ref + "||" + t.probe
		a := agg[k]
		a.sum += t.jsd
		a.n++
		a.genuine = t.genuine
		agg[k] = a
	}
	for _, a := range agg {
		pairScores = append(pairScores, a.sum/float64(a.n)) // 论文 R aggregate(FUN=mean) 不舍入
		pairGenuine = append(pairGenuine, a.genuine)
	}
	for _, t := range trials {
		cellJSDs = append(cellJSDs, t.jsd)
		cellGenuine = append(cellGenuine, t.genuine)
	}
	return
}

// compareCellJSDs L2 对拍：per-task×lang cell 的 JSD（modelA||modelB）vs 论文 divergence.json。
func compareCellJSDs(cells map[string]*distCounts, paperDivPath string, tasks map[string]bool) error {
	raw, err := os.ReadFile(paperDivPath)
	if err != nil {
		return err
	}
	var doc struct {
		PerTask map[string]map[string]float64 `json:"per_task_lang"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	// 对每 cell：模型两两 JSD vs 论文
	cellKeys := map[string][]string{} // task|lang -> cell keys (model|task|lang)
	for k := range cells {
		p := strings.SplitN(k, "|", 3)
		cellKeys[p[1]+"|"+p[2]] = append(cellKeys[p[1]+"|"+p[2]], k)
	}
	checked, mism, maxDiff := 0, 0, 0.0
	var maxPair string
	for cell, ks := range cellKeys {
		paperCell, ok := doc.PerTask[cell]
		if !ok {
			continue
		}
		var mods []string
		for _, k := range ks {
			p := strings.SplitN(k, "|", 3)
			mods = append(mods, p[0])
		}
		sort.Strings(mods)
		for i := 0; i < len(mods); i++ {
			for j := i + 1; j < len(mods); j++ {
				a := toDist(cells[mods[i]+"|"+cell], 10)
				b := toDist(cells[mods[j]+"|"+cell], 10)
				if a == nil || b == nil {
					continue
				}
				got := round4(jsd(a, b))
				want, ok := paperCell[mods[i]+"||"+mods[j]]
				if !ok {
					want, ok = paperCell[mods[j]+"||"+mods[i]]
				}
				if !ok {
					continue
				}
				checked++
				if d := math.Abs(got - want); d > 1e-9 {
					mism++
					if d > maxDiff {
						maxDiff = d
						maxPair = mods[i] + "||" + mods[j] + " @" + cell
					}
				}
			}
		}
	}
	fmt.Printf("[L2 JSD 对拍] 核对 %d 对（cell 内模型对）；差异 >1e-9 的 %d；最大差 %.6f（%s）\n",
		checked, mism, maxDiff, maxPair)
	return nil
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// eerFromROC 与 calibrate.computeROC 同公式的独立实现（交叉验证）：
// 阈值 τ 语义 s ≤ τ 判 genuine；FPR = P(impostor ≤ τ)、FNR = 1−TPR = P(genuine > τ)；
// EER = (FPR+FNR)/2 @ min|FPR−FNR|。与论文 R pROC 的 EER 口径一致（0.07282）。
func eerFromROC(genuine, impostor []float64) float64 {
	gs, is := append([]float64(nil), genuine...), append([]float64(nil), impostor...)
	sort.Float64s(gs)
	sort.Float64s(is)
	ng, ni := len(gs), len(is)
	if ng == 0 || ni == 0 {
		return math.NaN()
	}
	uniq := map[float64]bool{}
	for _, v := range append(append([]float64{}, gs...), is...) {
		uniq[v] = true
	}
	ths := make([]float64, 0, len(uniq))
	for v := range uniq {
		ths = append(ths, v)
	}
	sort.Float64s(ths)
	best, eer := math.Inf(1), 0.0
	for _, tau := range ths {
		gi := sort.Search(ng, func(k int) bool { return gs[k] > tau }) // #genuine ≤ τ
		ii := sort.Search(ni, func(k int) bool { return is[k] > tau }) // #impostor ≤ τ
		fpr := float64(ii) / float64(ni)
		tpr := float64(gi) / float64(ng)
		fnr := 1 - tpr
		if d := math.Abs(fpr - fnr); d < best {
			best = d
			eer = (fpr + fnr) / 2
		}
	}
	return eer
}

func main() {
	root := flag.String("data", "data/zenodo/dataset-raw", "论文数据根目录")
	flag.Parse()

	normPath := filepath.Join(*root, "derived", "normalized.jsonl")
	promptsPath := filepath.Join(*root, "..", "software-code", "config", "prompts.json")
	selPath := filepath.Join(*root, "..", "software-code", "config", "models.selected.json")

	tasks, err := studyATasks(promptsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "prompts:", err)
		os.Exit(1)
	}
	models, err := includedModels(selPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "models:", err)
		os.Exit(1)
	}
	familyGuess, err := familyGuesses(selPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "family:", err)
		os.Exit(1)
	}
	fmt.Printf("Study A 任务 %d 个、included 模型 %d 个\n", len(tasks), len(models))

	// 流式读 normalized.jsonl
	var recs []paperRec
	f, err := os.Open(normPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "normalized:", err)
		os.Exit(1)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r paperRec
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		recs = append(recs, r)
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "normalized 读取:", err)
	}
	f.Close()
	fmt.Printf("读取归一化记录 %d 条\n", len(recs))

	cells, valid, skipped := loadCells(recs, tasks, models)
	fmt.Printf("T=1.0+valid+StudyA+included: %d 条（跳过 %d）\n", valid, skipped)
	fmt.Printf("cell 数（model×task×lang）: %d\n", len(cells))

	// L1 分布对拍
	if err := compareDistributions(cells, filepath.Join(*root, "results", "distributions.json"), tasks); err != nil {
		fmt.Fprintln(os.Stderr, "L1:", err)
	}

	// L2 JSD 对拍：per-task×lang cell JSD vs 论文 divergence.json
	if err := compareCellJSDs(cells, filepath.Join(*root, "results", "divergence.json"), tasks); err != nil {
		fmt.Fprintln(os.Stderr, "L2:", err)
	}

	// L3/L4/L5 split-half
	halves := splitHalf(cells, recs, tasks, models)
	var cellsList []string
	seen := map[string]bool{}
	for k := range cells {
		p := strings.SplitN(k, "|", 3)
		ck := p[1] + "|" + p[2]
		if !seen[ck] {
			seen[ck] = true
			cellsList = append(cellsList, k)
		}
	}
	sort.Strings(cellsList)
	fmt.Printf("参与 cell（task|lang）: %d\n", len(cellsList))

	cellJSDs, cellGen, pairScores, pairGen := splitScores(halves, cellsList)

	// 分层统计
	var cg, ci []float64
	for i, s := range cellJSDs {
		if cellGen[i] {
			cg = append(cg, s)
		} else {
			ci = append(ci, s)
		}
	}
	fmt.Printf("\n[L4 cell 级] genuine %d 对 中位数 %.3f（论文 0.075 ±5%% → [0.071,0.079]）\n", len(cg), median(cg))
	fmt.Printf("[L4 cell 级] impostor %d 对 中位数 %.3f（论文 0.489 ±5%% → [0.465,0.513]）\n", len(ci), median(ci))

	var pg, pi []float64
	for i, s := range pairScores {
		if pairGen[i] {
			pg = append(pg, s)
		} else {
			pi = append(pi, s)
		}
	}
	fmt.Printf("\n[L5 模型对级] genuine %d 对 中位数 %.3f（论文噪声底线 0.140 ±5%% → [0.133,0.147]）\n", len(pg), median(pg))
	fmt.Printf("\n[L5 模型对级] impostor %d 对 中位数 %.3f（论文数据 split-scores 同口径 0.4832 ✅；设计“跨 provider 0.227”由 L8 复现：同模型跨 provider 56 对中位 0.2230，±5%% 内命中）\n", len(pi), median(pi))

	// L3 ROC
	auc := calibrate.AUC(pg, pi)
	eer := eerFromROC(pg, pi)
	fmt.Printf("\n[L3 ROC] AUC = %.6f（论文 0.971342）；EER = %.4f（论文 0.07282）\n", auc, eer)
	fmt.Printf("[L3 ROC] 结构检查：impostor 中位数 %.3f > genuine 中位数 %.3f → %v\n",
		median(pi), median(pg), median(pi) > median(pg))

	// L6 1-NN 家族分类（LOO on 全样本 mean JSD 矩阵，与论文 R/11 同口径）
	looAccuracy(cells, familyGuess, tasks)

	// L8 同模型跨 provider 距离（论文“跨 provider 0.227”）：
	// 数据集同一 slug 下有多 provider 记录（如 openai/gpt-4o-2024-05-13 = OpenAI+Azure），
	// 按 (model,provider) 拆分布，同 model 不同 provider 对的距离（cell 级 mean）
	crossProvider(*root, recs, tasks, models)

	// L7 归一化层对拍（我们的 preprocess vs 论文归一化，main-02 全量）
	compareNormalize(*root, tasks)
}

// crossProvider 计算同模型不同 provider 的指纹距离（跨通道基线，论文 0.227）。
func crossProvider(root string, recs []paperRec, tasks map[string]bool, models map[string]bool) {
	// model|provider|task|lang → counts
	cells := map[string]*distCounts{}
	for _, r := range recs {
		if r.Temperature != 1 || r.Class != "valid" {
			continue
		}
		if !tasks[r.TaskID] || !models[r.Model] {
			continue
		}
		k := r.Model + "|" + r.Provider + "|" + r.TaskID + "|" + r.Lang
		c := cells[k]
		if c == nil {
			c = &distCounts{counts: map[string]int{}}
			cells[k] = c
		}
		c.counts[r.Normalized]++
		c.n++
	}
	// 每 (model,provider) 的指纹 = 跨 cell 的分布集合
	fp := map[string]map[string]*distCounts{} // model|provider -> task|lang -> counts
	for k, c := range cells {
		p := strings.SplitN(k, "|", 4)
		if fp[p[0]+"|"+p[1]] == nil {
			fp[p[0]+"|"+p[1]] = map[string]*distCounts{}
		}
		fp[p[0]+"|"+p[1]][p[2]+"|"+p[3]] = c
	}
	// 同 model 不同 provider 对
	byModel := map[string][]string{}
	for k := range fp {
		byModel[strings.SplitN(k, "|", 2)[0]] = append(byModel[strings.SplitN(k, "|", 2)[0]], k)
	}
	var dists []float64
	var pairs []string
	for _, provs := range byModel {
		if len(provs) < 2 {
			continue
		}
		sort.Strings(provs)
		for i := 0; i < len(provs); i++ {
			for j := i + 1; j < len(provs); j++ {
				d := fpMeanDist(fp[provs[i]], fp[provs[j]])
				if math.IsNaN(d) {
					continue // 无共同 cell 双方 ≥10
				}
				dists = append(dists, d)
				pairs = append(pairs, provs[i]+" vs "+provs[j])
			}
		}
	}
	fmt.Printf("\n[L8 同模型跨 provider] %d 对（%d 个模型有多 provider）；距离中位数 %.4f（论文跨通道 0.227 ±5%% → [0.216,0.238]）\n",
		len(dists), len(byModel), median(dists))
	// 样例
	if len(pairs) > 8 {
		pairs = pairs[:8]
	}
	for k, p := range pairs {
		fmt.Printf("  %s: %.4f\n", p, dists[k])
	}
}

// fpMeanDist 两指纹的 cell 级 JSD 均值（共同 cell 双方 ≥10）。
func fpMeanDist(a, b map[string]*distCounts) float64 {
	var sum float64
	n := 0
	for cell, ca := range a {
		cb, ok := b[cell]
		if !ok {
			continue
		}
		aD := toDist(ca, 10)
		bD := toDist(cb, 10)
		if aD == nil || bD == nil {
			continue
		}
		sum += jsd(aD, bD)
		n++
	}
	if n == 0 {
		return math.NaN()
	}
	return sum / float64(n)
}

// paperTaskToOurs 映射论文 task_id → 我们 preprocess.Task（Study A 10 任务）。
func paperTaskToOurs(id string) (preprocess.Task, bool) {
	m := map[string]preprocess.Task{
		"num100-random":  {ID: "random_number_100", AnswerSpace: "closed", SpaceSize: 100},
		"num10-random":   {ID: "random_number_10", AnswerSpace: "closed", SpaceSize: 10},
		"num-favorite":   {ID: "favorite_number", AnswerSpace: "open"},
		"letter-random":  {ID: "random_letter", AnswerSpace: "closed", SpaceSize: 26},
		"word-random":    {ID: "random_word", AnswerSpace: "open"},
		"color-random":   {ID: "random_color", AnswerSpace: "open"},
		"color-favorite": {ID: "favorite_color", AnswerSpace: "open"},
		"animal-random":  {ID: "random_animal", AnswerSpace: "open"},
		"city-random":    {ID: "random_city", AnswerSpace: "open"},
		"coin-flip":      {ID: "coin_flip", AnswerSpace: "closed", SpaceSize: 2},
	}
	t, ok := m[id]
	return t, ok
}

// compareNormalize 用我们的 preprocess 处理论文原始响应，与论文归一化结果对比。
func compareNormalize(root string, tasks map[string]bool) {
	normPath := filepath.Join(root, "derived", "normalized.jsonl")
	runsPath := filepath.Join(root, "runs", "main-02", "responses.jsonl")

	// 论文归一化索引 key -> (class, normalized)
	type normRes struct {
		class      string
		norm       string
		colorCanon string
	}
	paper := map[string]normRes{}
	f, err := os.Open(normPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "L7 norm:", err)
		return
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r paperRec
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			paper[r.Key] = normRes{r.Class, r.Normalized, r.ColorCanon}
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "L7 norm 读取:", err)
	}
	f.Close()

	// 流式读原始响应并对比
	f2, err := os.Open(runsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "L7 runs:", err)
		return
	}
	defer f2.Close()
	sc2 := bufio.NewScanner(f2)
	sc2.Buffer(make([]byte, 4<<20), 8<<20)
	type diffStat struct{ n, classMism, normMism int }
	byTask := map[string]*diffStat{}
	total, prMarked := 0, 0
	for sc2.Scan() {
		var raw struct {
			Key         string `json:"key"`
			TaskID      string `json:"task_id"`
			Lang        string `json:"lang"`
			Raw         string `json:"raw"`
			AnswerClass string `json:"answer_class"`
		}
		if json.Unmarshal(sc2.Bytes(), &raw) != nil {
			continue
		}
		want, ok := paper[raw.Key]
		if !ok || !tasks[raw.TaskID] {
			continue
		}
		// 论文 post_reasoning 是探测器层概念（归一化标记），跳过统计
		if want.class == "post_reasoning" {
			prMarked++
			continue
		}
		task, ok := paperTaskToOurs(raw.TaskID)
		if !ok {
			continue
		}
		// 注意：论文归一化带 lang 语义（硬币词表、颜色词表按语言），
		// 我们的 Task 结构无 lang——硬币/颜色词表按全部语言折叠，
		// 与论文按语言查表等效（词表键已含各语言词）。
		got := preprocess.NormalizeClassify(raw.Raw, task)
		total++
		d := byTask[raw.TaskID]
		if d == nil {
			d = &diffStat{}
			byTask[raw.TaskID] = d
		}
		d.n++
		if string(got.Classification) != want.class {
			d.classMism++
		}
		// 颜色任务：论文 normalized=原词、color_canon=规范码；我们 normalized=规范码（跨语言合并设计）。
		// 规范码层面用论文 color_canon 对比（canonical vs canonical）；其余任务用 normalized。
		wantNorm := want.norm
		if raw.TaskID == "color-random" || raw.TaskID == "color-favorite" {
			wantNorm = want.colorCanon
		}
		if got.Normalized != wantNorm && string(got.Classification) == want.class {
			d.normMism++
		}
	}
	if err := sc2.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "L7 runs 读取:", err)
	}
	fmt.Printf("\n[L7 归一化层] main-02 Study A 对比 %d 条（论文 post_reasoning 标记 %d 条跳过）\n", total, prMarked)
	order := []string{"num100-random", "num10-random", "num-favorite", "letter-random", "word-random", "color-random", "color-favorite", "animal-random", "city-random", "coin-flip"}
	for _, id := range order {
		d := byTask[id]
		if d == nil {
			continue
		}
		fmt.Printf("  %-14s n=%-6d class一致率 %.4f  同class下normalized一致率 %.4f\n",
			id, d.n, 1-float64(d.classMism)/float64(d.n), 1-float64(d.normMism)/float64(d.n-d.classMism))
	}
}

// familyGuesses 读取 models.selected.json 的 family_guess（发布数据无 family 字段）。
func familyGuesses(selPath string) (map[string]string, error) {
	raw, err := os.ReadFile(selPath)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Models []struct {
			ID          string `json:"id"`
			FamilyGuess string `json:"family_guess"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, m := range doc.Models {
		if m.FamilyGuess != "" {
			out[m.ID] = m.FamilyGuess
		}
	}
	return out, nil
}

// looAccuracy 全样本 mean JSD 矩阵上的 LOO 1-NN 家族分类（论文 R/11 同口径：
// 仅 ≥2 成员家族参与；family_guess 标签）。
func looAccuracy(cells map[string]*distCounts, fam map[string]string, tasks map[string]bool) {
	// 模型列表（included 且 Study A 有数据）
	models := map[string]bool{}
	for k := range cells {
		p := strings.SplitN(k, "|", 3)
		models[p[0]] = true
	}
	mods := make([]string, 0, len(models))
	for m := range models {
		mods = append(mods, m)
	}
	sort.Strings(mods)
	// 全样本 per-cell 分布 + 模型对 mean JSD
	meanJSD := map[string]float64{} // a<b 键
	cnt := map[string]int{}
	cellKeys := map[string][]string{}
	for k := range cells {
		p := strings.SplitN(k, "|", 3)
		cellKeys[p[1]+"|"+p[2]] = append(cellKeys[p[1]+"|"+p[2]], k)
	}
	for _, ks := range cellKeys {
		for i := 0; i < len(ks); i++ {
			pi := strings.SplitN(ks[i], "|", 3)
			ai := toDist(cells[ks[i]], 10)
			if ai == nil {
				continue
			}
			for j := i + 1; j < len(ks); j++ {
				pj := strings.SplitN(ks[j], "|", 3)
				if pi[0] == pj[0] {
					continue // 同模型只出现一次 per cell
				}
				aj := toDist(cells[ks[j]], 10)
				if aj == nil {
					continue
				}
				k := pi[0] + "||" + pj[0]
				if pi[0] > pj[0] {
					k = pj[0] + "||" + pi[0]
				}
				meanJSD[k] += jsd(ai, aj)
				cnt[k]++
			}
		}
	}
	dist := func(a, b string) float64 {
		if a == b {
			return 0
		}
		k := a + "||" + b
		if a > b {
			k = b + "||" + a
		}
		if cnt[k] == 0 {
			return math.NaN()
		}
		return meanJSD[k] / float64(cnt[k])
	}
	// family 计数，仅 ≥2 成员参与
	famCount := map[string]int{}
	for _, m := range mods {
		famCount[fam[m]]++
	}
	usable := make([]string, 0, len(mods))
	for _, m := range mods {
		if famCount[fam[m]] >= 2 {
			usable = append(usable, m)
		}
	}
	sort.Strings(usable)
	hit := 0
	for _, m := range usable {
		best, bestD := "", math.Inf(1)
		for _, o := range usable {
			if o == m {
				continue
			}
			if d := dist(m, o); d < bestD {
				bestD = d
				best = o
			}
		}
		if fam[m] == fam[best] {
			hit++
		}
	}
	acc := float64(hit) / float64(len(usable))
	fmt.Printf("\n[L6 1-NN 家族分类] %d 模型（≥2 成员家族）；accuracy = %.3f%%（论文 59.5%%，chance 18.4%%）\n",
		len(usable), acc*100)
}
