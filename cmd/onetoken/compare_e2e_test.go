package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// compareBase 构造 compare 命令的两端点直传参数。
func compareBase(refURL, tgtURL string) []string {
	return []string{
		"--ref-base-url", refURL, "--ref-api-key-env", "FAKE_KEY", "--ref-protocol", "chat",
		"--target-base-url", tgtURL, "--target-api-key-env", "FAKE_KEY", "--target-protocol", "chat",
	}
}

// resetCompareFlags 重置全局 compare 命令 flag（cobra 不自动重置：
// 未显式传入的 flag 保留上一次值，跨测试污染——如 --save-ref/--tau/--json 残留）。
func resetCompareFlags() { *compareFlag = compareFlags{} }

func TestCompareSameModelPass(t *testing.T) {
	resetCompareFlags()
	// 同池 = 同模型：距离 ≈0 → pass（无校准库 → 内置线 0.140）
	srvA := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srvA.Close()
	srvB := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srvB.Close()

	dataDir := t.TempDir()
	t.Setenv("ONETOKEN_DATA", dataDir)
	t.Setenv("FAKE_KEY", "sk-test")
	args := append([]string{"compare"}, append(compareBase(srvA.URL, srvB.URL),
		"--k", "16", "--n", "10", "--seed", "42", "--json")...)
	out, err := runCLI(t, args...)
	if err != nil {
		t.Fatalf("compare 失败: %v\n%s", err, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("输出非 JSON: %v\n%s", err, out)
	}
	if res["verdict"] != "pass" {
		t.Fatalf("同模型应 pass，实际 %q（score=%v）", res["verdict"], res["score"])
	}
	if res["tau_source"] != "builtin" {
		t.Fatalf("无校准库应 builtin，实际 %v", res["tau_source"])
	}
	// 默认不落库：data 目录无指纹/模型登记（--save-* 未传）
	if _, err := os.Stat(filepath.Join(dataDir, "fingerprints")); !os.IsNotExist(err) {
		t.Fatalf("默认不应落指纹目录（err=%v）", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "models.json")); !os.IsNotExist(err) {
		t.Fatalf("默认不应写 models.json（err=%v）", err)
	}
	// JSON schema 关键字段
	for _, k := range []string{"verdict", "score", "threshold", "tau_source", "cells_used", "channel", "ref", "target", "per_cell"} {
		if _, ok := res[k]; !ok {
			t.Fatalf("JSON 缺字段 %q", k)
		}
	}
	// HTML 报告默认生成并记录路径
	rp, _ := res["report"].(string)
	if rp == "" {
		t.Fatal("默认应生成 HTML 报告（report 路径缺失）")
	}
	if _, err := os.Stat(rp); err != nil {
		t.Fatalf("报告文件不存在: %v", err)
	}
	html, err := os.ReadFile(rp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), "pass") {
		t.Fatal("报告应含判定")
	}
}

func TestCompareImpostorSuspicious(t *testing.T) {
	resetCompareFlags()
	// 不同池 = 冒充：距离高 → suspicious
	srvA := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srvA.Close()
	srvB := httptest.NewServer(fakeHandler([]string{"13", "24", "66"}, ""))
	defer srvB.Close()

	t.Setenv("ONETOKEN_DATA", t.TempDir())
	t.Setenv("FAKE_KEY", "sk-test")
	args := append([]string{"compare"}, append(compareBase(srvA.URL, srvB.URL),
		"--k", "16", "--n", "10", "--seed", "42", "--json", "--no-report")...)
	out, err := runCLI(t, args...)
	if err != nil {
		t.Fatalf("compare 失败: %v\n%s", err, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("输出非 JSON: %v\n%s", err, out)
	}
	if res["verdict"] != "suspicious" {
		t.Fatalf("冒充应 suspicious，实际 %q（score=%v τ=%v）", res["verdict"], res["score"], res["threshold"])
	}
	if _, ok := res["report"]; ok {
		t.Fatal("--no-report 不应有报告路径")
	}
}

func TestCompareTauPriority(t *testing.T) {
	resetCompareFlags()
	srvA := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srvA.Close()
	srvB := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srvB.Close()

	t.Setenv("ONETOKEN_DATA", t.TempDir())
	t.Setenv("FAKE_KEY", "sk-test")

	// --tau 直传 → override（最高优先级）
	args := append([]string{"compare"}, append(compareBase(srvA.URL, srvB.URL),
		"--k", "8", "--n", "10", "--tau", "0.30", "--json", "--no-report")...)
	out, err := runCLI(t, args...)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	_ = json.Unmarshal([]byte(out), &res)
	if res["tau_source"] != "override" || res["threshold"].(float64) != 0.30 {
		t.Fatalf("--tau 应 override，实际 %v", res["tau_source"])
	}
}

func TestCompareSaveRef(t *testing.T) {
	resetCompareFlags()
	srvA := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srvA.Close()
	srvB := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srvB.Close()

	dataDir := t.TempDir()
	t.Setenv("ONETOKEN_DATA", dataDir)
	t.Setenv("FAKE_KEY", "sk-test")
	args := append([]string{"compare"}, append(compareBase(srvA.URL, srvB.URL),
		"--k", "16", "--n", "10", "--seed", "7",
		"--save-ref", "--ref-model-id", "qwen/qwen3-8b", "--ref-version", "cmp1",
		"--json", "--no-report")...)
	out, err := runCLI(t, args...)
	if err != nil {
		t.Fatalf("compare --save-ref 失败: %v\n%s", err, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	saved, _ := res["saved_ref"].(map[string]any)
	if saved == nil || saved["model_id"] != "qwen/qwen3-8b" || saved["version"] != "cmp1" {
		t.Fatalf("saved_ref 异常: %v", res["saved_ref"])
	}
	// 指纹落库
	fp, err := os.ReadFile(filepath.Join(dataDir, "fingerprints", "qwen__qwen3-8b.json"))
	if err != nil {
		t.Fatalf("指纹未落库: %v", err)
	}
	if !strings.Contains(string(fp), "cmp1") {
		t.Fatal("指纹版本不符")
	}
	models, err := os.ReadFile(filepath.Join(dataDir, "models.json"))
	if err != nil {
		t.Fatalf("models.json 未写: %v", err)
	}
	if !strings.Contains(string(models), "qwen/qwen3-8b") {
		t.Fatal("models 登记缺失")
	}
	// 同版本再存 → 版本冲突拒绝
	out, err = runCLI(t, args...)
	if err == nil || !strings.Contains(err.Error(), "版本冲突") {
		t.Fatalf("同版本重复落库应报版本冲突，实际 err=%v\n%s", err, out)
	}
}

func TestCompareRequiresSaveRefModelID(t *testing.T) {
	resetCompareFlags()
	srvA := httptest.NewServer(fakeHandler([]string{"42"}, ""))
	defer srvA.Close()
	srvB := httptest.NewServer(fakeHandler([]string{"42"}, ""))
	defer srvB.Close()
	t.Setenv("ONETOKEN_DATA", t.TempDir())
	t.Setenv("FAKE_KEY", "sk-test")
	args := append([]string{"compare"}, append(compareBase(srvA.URL, srvB.URL),
		"--save-ref", "--k", "8", "--n", "5")...) // 缺 --ref-model-id
	out, err := runCLI(t, args...)
	if err == nil || !strings.Contains(err.Error(), "--ref-model-id") {
		t.Fatalf("--save-ref 缺 --ref-model-id 应报错，实际 err=%v\n%s", err, out)
	}
}

func TestCompareStdoutSummaryNoTable(t *testing.T) {
	resetCompareFlags()
	srvA := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srvA.Close()
	srvB := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srvB.Close()
	t.Setenv("ONETOKEN_DATA", t.TempDir())
	t.Setenv("FAKE_KEY", "sk-test")
	args := append([]string{"compare"}, append(compareBase(srvA.URL, srvB.URL),
		"--k", "8", "--n", "10", "--no-report")...)
	out, err := runCLI(t, args...)
	if err != nil {
		t.Fatalf("compare 失败: %v\n%s", err, out)
	}
	// stdout 应为简洁摘要：含 verdict/score/τ（内置参考线）/通道/两端点，无表格
	if !strings.HasPrefix(out, "compare: ") {
		t.Fatalf("摘要格式错误: %q", out)
	}
	if !strings.Contains(out, "内置参考线") || !strings.Contains(out, "direct") {
		t.Fatalf("摘要应含 τ 来源（内置参考线）与通道: %q", out)
	}
	if strings.Contains(out, "<table") || strings.Contains(out, "|") {
		t.Fatalf("stdout 不应输出表格: %q", out)
	}
}
