package main

import (
	"encoding/json"
	"fmt"
	"os"

	"onetoken/internal/fingerprint"
	"onetoken/internal/store"
)

func load(path string) *store.Fingerprint {
	var fp store.Fingerprint
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(b, &fp); err != nil {
		panic(err)
	}
	return &fp
}

func main() {
	base := os.Getenv("HOME") + "/.onetoken/data/fingerprints/"
	flash := load(base + "deepseek-v4-flash.json")
	pro := load(base + "deepseek-v4-pro.json")
	d, n := fingerprint.Distance(flash, pro)
	fmt.Printf("flash vs pro: distance=%.6f cells=%d\n", d, n)
	js := fingerprint.CellJSDs(flash, pro)
	total, big := 0.0, 0
	for _, v := range js {
		total += v
		if v > 0.2 {
			big++
		}
	}
	fmt.Printf("cell 均值=%.4f 超 0.2 的 cell=%d/%d\n", total/float64(len(js)), big, len(js))
}
