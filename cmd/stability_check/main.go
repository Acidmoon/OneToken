package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"onetoken/internal/battery"
	"onetoken/internal/fingerprint"
	"onetoken/internal/preprocess"
	"onetoken/internal/store"
)

func loadResponses(path string) []*store.Response {
	rs, err := loadJSONL(path)
	if err != nil {
		panic(err)
	}
	return rs
}

func loadJSONL(path string) ([]*store.Response, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return decodeJSONL(f)
}

func main() {
	home, _ := os.UserHomeDir()
	respDir := filepath.Join(home, ".onetoken", "data", "responses")
	b, err := battery.Load(filepath.Join("config", "prompts.json"))
	if err != nil {
		panic(err)
	}
	// TaskForCell
	byID := map[string]preprocess.Task{}
	for _, t := range b.Tasks {
		byID[t.ID] = preprocess.Task{ID: t.ID, AnswerSpace: t.AnswerSpace, SpaceSize: t.SpaceSize}
	}
	taskFC := func(cell string) (preprocess.Task, bool) {
		tid, _, ok := cutCell(cell)
		if !ok {
			return preprocess.Task{}, false
		}
		t, ok := byID[tid]
		return t, ok
	}
	// 从响应重建指纹（Text 管线：回填派生列后 Build）
	build := func(ver string) *store.Fingerprint {
		rs := loadResponses(filepath.Join(respDir, "ref-deepseek-v4-flash-"+ver+".jsonl"))
		rs = append(rs, loadResponses(filepath.Join(respDir, "ref-deepseek-v4-flash-"+ver+"-t0.jsonl"))...)
		for _, r := range rs {
			if r.Classification == "" || r.Normalized == "" {
				pc := preprocess.NormalizeClassify(r.Text, preprocess.Task{})
				if t, ok := taskFC(r.Cell); ok {
					pc = preprocess.NormalizeClassify(r.Text, t)
				}
				r.Classification = string(pc.Classification)
				r.Normalized = pc.Normalized
			}
		}
		fp, err := fingerprint.Build("flash-"+ver, ver, "official-api", time.Now().UTC(), rs)
		if err != nil {
			panic(err)
		}
		return fp
	}
	fp3 := build("2026-08-06v3")
	fp4 := build("2026-08-06v4")
	pro := loadResponses(filepath.Join(respDir, "ref-deepseek-v4-pro-2026-08-06v1.jsonl"))
	pro = append(pro, loadResponses(filepath.Join(respDir, "ref-deepseek-v4-pro-2026-08-06v1-t0.jsonl"))...)
	for _, r := range pro {
		if r.Classification == "" || r.Normalized == "" {
			pc := preprocess.NormalizeClassify(r.Text, preprocess.Task{})
			if t, ok := taskFC(r.Cell); ok {
				pc = preprocess.NormalizeClassify(r.Text, t)
			}
			r.Classification = string(pc.Classification)
			r.Normalized = pc.Normalized
		}
	}
	fpPro, err := fingerprint.Build("pro", "v1", "official-api", time.Now().UTC(), pro)
	if err != nil {
		panic(err)
	}
	report := func(name string, a, b *store.Fingerprint) {
		d, n := fingerprint.Distance(a, b)
		js := fingerprint.CellJSDs(a, b)
		vals := make([]float64, 0, len(js))
		over := 0
		for _, v := range js {
			vals = append(vals, v)
			if v > 0.2 {
				over++
			}
		}
		sort.Float64s(vals)
		med := 0.0
		if len(vals) > 0 {
			med = vals[len(vals)/2]
		}
		fmt.Printf("%-28s 距离=%.4f cells=%d cell中位=%.4f cell>0.2: %d/%d\n", name, d, n, med, over, len(vals))
	}
	fmt.Println("=== 推理通道稳定性实验（post-reasoning 回答分布）===")
	report("同模型 flash v3 vs v4", fp3, fp4)
	report("跨模型 flash v4 vs pro", fp4, fpPro)
	report("跨模型 flash v3 vs pro", fp3, fpPro)
	// 分裂半 genuine 基线
	a3, b3 := splitHalf(fp3)
	report("flash v3 分裂半（genuine 基线）", a3, b3)
	a4, b4 := splitHalf(fp4)
	report("flash v4 分裂半（genuine 基线）", a4, b4)
}

func cutCell(cell string) (string, string, bool) {
	for i := 0; i < len(cell); i++ {
		if cell[i] == ':' {
			return cell[:i], cell[i+1:], true
		}
	}
	return "", "", false
}
