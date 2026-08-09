package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// compareBase 构造 compare 命令的两端点直传参数（v0.24：--model 必填 +
// --results-dir 显式指定，e2e 统一传 t.TempDir() 避免污染仓库工作目录）。
func compareBase(refURL, tgtURL, resultsDir string) []string {
	return []string{
		"--ref-base-url", refURL, "--ref-api-key-env", "FAKE_KEY", "--ref-protocol", "chat",
		"--target-base-url", tgtURL, "--target-api-key-env", "FAKE_KEY", "--target-protocol", "chat",
		"--model", "qwen/qwen3-8b", "--results-dir", resultsDir,
	}
}

// resetCompareFlags 重置全局 compare 命令 flag（cobra 不自动重置：
// 未显式传入的 flag 保留上一次值，跨测试污染——如 --save-ref/--tau/--json 残留）。
func resetCompareFlags() { *compareFlag = compareFlags{} }

// modelCapture 包裹 fakeHandler，记录请求体 model 字段（--target-model 覆盖断言用）。
func modelCapture(cap *atomic.Value, inner http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err == nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			var req struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(body, &req) == nil && req.Model != "" {
				cap.Store(req.Model)
			}
		}
		inner(w, r)
	}
}

// readArchiveJSON 读取归档目录下的 JSON 文件为 map。
func readArchiveJSON(t *testing.T, dir, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("归档文件 %s 不存在: %v", name, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("归档文件 %s 非 JSON: %v", name, err)
	}
	return m
}

// assertNoTmpLeftover 断言目录无原子写残留临时文件。
func assertNoTmpLeftover(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("原子写残留临时文件: %s", e.Name())
		}
	}
}

func TestCompareSameModelPass(t *testing.T) {
	resetCompareFlags()
	// 同池 = 同模型：距离 ≈0 → pass（无校准库 → 内置线 0.140）
	srvA := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srvA.Close()
	srvB := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srvB.Close()

	dataDir := t.TempDir()
	resultsDir := t.TempDir()
	t.Setenv("ONETOKEN_DATA", dataDir)
	t.Setenv("FAKE_KEY", "sk-test")
	args := append([]string{"compare"}, append(compareBase(srvA.URL, srvB.URL, resultsDir),
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
	// 默认不落库：data 目录无指纹/模型登记（--save-ref 未传；--save-responses v0.24
	// 起写归档文件夹，同样不落 data store）
	if _, err := os.Stat(filepath.Join(dataDir, "fingerprints")); !os.IsNotExist(err) {
		t.Fatalf("默认不应落指纹目录（err=%v）", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "models.json")); !os.IsNotExist(err) {
		t.Fatalf("默认不应写 models.json（err=%v）", err)
	}
	// JSON schema 关键字段（v0.24 增 archive_dir）
	for _, k := range []string{"verdict", "score", "threshold", "tau_source", "cells_used", "channel", "ref", "target", "per_cell", "archive_dir"} {
		if _, ok := res[k]; !ok {
			t.Fatalf("JSON 缺字段 %q", k)
		}
	}
	// 归档目录 = <results-dir>/<SanitizeID(--model)>/，四文件齐全
	archiveDir := filepath.Join(resultsDir, "qwen__qwen3-8b")
	if res["archive_dir"] != archiveDir {
		t.Fatalf("archive_dir=%v，期望 %s", res["archive_dir"], archiveDir)
	}
	for _, name := range []string{"reference.json", "target.json", "verdict.json", "report.html"} {
		if _, err := os.Stat(filepath.Join(archiveDir, name)); err != nil {
			t.Fatalf("归档缺文件 %s: %v", name, err)
		}
	}
	assertNoTmpLeftover(t, archiveDir)

	// verdict.json schema：判定要素 + τ 来源 + 逐 cell 明细 + 时间戳
	vj := readArchiveJSON(t, archiveDir, "verdict.json")
	if vj["schema_version"].(float64) != 1 {
		t.Fatalf("verdict.json schema_version=%v，期望 1", vj["schema_version"])
	}
	for _, k := range []string{"model", "target_model", "score", "threshold", "tau_source",
		"verdict", "cells_used", "cells_detail", "ref_qc_flags", "target_qc_flags", "compared_at"} {
		if _, ok := vj[k]; !ok {
			t.Fatalf("verdict.json 缺字段 %q", k)
		}
	}
	if vj["tau_source"] != "builtin" || vj["verdict"] != "pass" {
		t.Fatalf("verdict.json 判定异常: tau_source=%v verdict=%v", vj["tau_source"], vj["verdict"])
	}
	if vj["model"] != "qwen/qwen3-8b" || vj["target_model"] != "qwen/qwen3-8b" {
		t.Fatalf("verdict.json 模型名异常: %v / %v", vj["model"], vj["target_model"])
	}
	if cd, ok := vj["cells_detail"].(map[string]any); !ok || len(cd) == 0 {
		t.Fatalf("verdict.json cells_detail 应为非空明细: %v", vj["cells_detail"])
	}
	if vj["report"] != "report.html" {
		t.Fatalf("verdict.json report=%v，期望 report.html", vj["report"])
	}

	// reference.json / target.json 分别命名且内容对应各自端点
	ref := readArchiveJSON(t, archiveDir, "reference.json")
	tgt := readArchiveJSON(t, archiveDir, "target.json")
	if ref["base_url"] != srvA.URL {
		t.Fatalf("reference.json base_url=%v，期望参考端 %s", ref["base_url"], srvA.URL)
	}
	if tgt["base_url"] != srvB.URL {
		t.Fatalf("target.json base_url=%v，期望待测端 %s", tgt["base_url"], srvB.URL)
	}
	for name, m := range map[string]map[string]any{"reference.json": ref, "target.json": tgt} {
		if m["schema_version"].(float64) != 1 {
			t.Fatalf("%s schema_version=%v，期望 1", name, m["schema_version"])
		}
		if m["model"] != "qwen/qwen3-8b" {
			t.Fatalf("%s model=%v，期望 qwen/qwen3-8b", name, m["model"])
		}
		for _, k := range []string{"provider", "protocol", "channel", "k", "n", "seed", "collected_at", "cells", "qc_flags"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("%s 缺字段 %q", name, k)
			}
		}
		if cells, ok := m["cells"].(map[string]any); !ok || len(cells) == 0 {
			t.Fatalf("%s cells 应为非空指纹", name)
		}
	}

	// report.html 默认生成并含判定；JSON 报告路径指向归档内
	rp, _ := res["report"].(string)
	if rp != filepath.Join(archiveDir, "report.html") {
		t.Fatalf("report 路径=%v，期望归档内 report.html", rp)
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

	resultsDir := t.TempDir()
	t.Setenv("ONETOKEN_DATA", t.TempDir())
	t.Setenv("FAKE_KEY", "sk-test")
	args := append([]string{"compare"}, append(compareBase(srvA.URL, srvB.URL, resultsDir),
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
	// --no-report：无 report.html，其余三个 JSON 仍写（v0.24）
	archiveDir := filepath.Join(resultsDir, "qwen__qwen3-8b")
	for _, name := range []string{"reference.json", "target.json", "verdict.json"} {
		if _, err := os.Stat(filepath.Join(archiveDir, name)); err != nil {
			t.Fatalf("--no-report 归档缺文件 %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(archiveDir, "report.html")); !os.IsNotExist(err) {
		t.Fatalf("--no-report 不应生成 report.html（err=%v）", err)
	}
	vj := readArchiveJSON(t, archiveDir, "verdict.json")
	if _, ok := vj["report"]; ok {
		t.Fatal("--no-report 时 verdict.json 不应含 report 字段")
	}
	if vj["verdict"] != "suspicious" {
		t.Fatalf("verdict.json verdict=%v，期望 suspicious", vj["verdict"])
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
	args := append([]string{"compare"}, append(compareBase(srvA.URL, srvB.URL, t.TempDir()),
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
	resultsDir := t.TempDir()
	t.Setenv("ONETOKEN_DATA", dataDir)
	t.Setenv("FAKE_KEY", "sk-test")
	args := append([]string{"compare"}, append(compareBase(srvA.URL, srvB.URL, resultsDir),
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
	// --save-ref 不影响归档（v0.24：归档默认开启）
	if _, err := os.Stat(filepath.Join(resultsDir, "qwen__qwen3-8b", "verdict.json")); err != nil {
		t.Fatalf("--save-ref 时归档仍应写 verdict.json: %v", err)
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
	args := append([]string{"compare"}, append(compareBase(srvA.URL, srvB.URL, t.TempDir()),
		"--save-ref", "--k", "8", "--n", "5")...) // 缺 --ref-model-id
	out, err := runCLI(t, args...)
	if err == nil || !strings.Contains(err.Error(), "--ref-model-id") {
		t.Fatalf("--save-ref 缺 --ref-model-id 应报错，实际 err=%v\n%s", err, out)
	}
}

// TestCompareRequiresModel：--model 必填（v0.24：请求模型名 + 归档文件夹名）。
func TestCompareRequiresModel(t *testing.T) {
	resetCompareFlags()
	srvA := httptest.NewServer(fakeHandler([]string{"42"}, ""))
	defer srvA.Close()
	srvB := httptest.NewServer(fakeHandler([]string{"42"}, ""))
	defer srvB.Close()
	t.Setenv("ONETOKEN_DATA", t.TempDir())
	t.Setenv("FAKE_KEY", "sk-test")
	out, err := runCLI(t, "compare",
		"--ref-base-url", srvA.URL, "--ref-api-key-env", "FAKE_KEY", "--ref-protocol", "chat",
		"--target-base-url", srvB.URL, "--target-api-key-env", "FAKE_KEY", "--target-protocol", "chat",
		"--results-dir", t.TempDir(), "--k", "8", "--n", "5") // 缺 --model
	if err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("缺 --model 应报错，实际 err=%v\n%s", err, out)
	}
}

// TestCompareTargetModelOverride：--target-model 覆盖待测端请求模型名，
// 参考端始终用 --model（v0.24 修复占位符 "ref"/"target" 真实端点缺口）。
func TestCompareTargetModelOverride(t *testing.T) {
	resetCompareFlags()
	var refModel, tgtModel atomic.Value
	srvA := httptest.NewServer(modelCapture(&refModel, fakeHandler([]string{"42", "57", "88"}, "")))
	defer srvA.Close()
	srvB := httptest.NewServer(modelCapture(&tgtModel, fakeHandler([]string{"42", "57", "88"}, "")))
	defer srvB.Close()

	resultsDir := t.TempDir()
	t.Setenv("ONETOKEN_DATA", t.TempDir())
	t.Setenv("FAKE_KEY", "sk-test")
	args := append([]string{"compare"}, append(compareBase(srvA.URL, srvB.URL, resultsDir),
		"--target-model", "qwen3-8b-int4", "--k", "16", "--n", "10", "--seed", "42",
		"--json", "--no-report")...)
	out, err := runCLI(t, args...)
	if err != nil {
		t.Fatalf("compare 失败: %v\n%s", err, out)
	}
	if got, _ := refModel.Load().(string); got != "qwen/qwen3-8b" {
		t.Fatalf("参考端请求模型名=%q，期望 --model 值 qwen/qwen3-8b", got)
	}
	if got, _ := tgtModel.Load().(string); got != "qwen3-8b-int4" {
		t.Fatalf("待测端请求模型名=%q，期望 --target-model 覆盖值 qwen3-8b-int4", got)
	}
	// 归档留痕：verdict.json / target.json 的 target_model 为覆盖值
	archiveDir := filepath.Join(resultsDir, "qwen__qwen3-8b")
	vj := readArchiveJSON(t, archiveDir, "verdict.json")
	if vj["target_model"] != "qwen3-8b-int4" {
		t.Fatalf("verdict.json target_model=%v，期望 qwen3-8b-int4", vj["target_model"])
	}
	tgt := readArchiveJSON(t, archiveDir, "target.json")
	if tgt["model"] != "qwen3-8b-int4" {
		t.Fatalf("target.json model=%v，期望 qwen3-8b-int4", tgt["model"])
	}
	ref := readArchiveJSON(t, archiveDir, "reference.json")
	if ref["model"] != "qwen/qwen3-8b" {
		t.Fatalf("reference.json model=%v，期望 qwen/qwen3-8b", ref["model"])
	}
}

// TestCompareArchiveOverwrite：重测同模型 → 固定文件名原子覆盖（两次均不报错，
// 文件更新且无 .tmp-* 残留）。
func TestCompareArchiveOverwrite(t *testing.T) {
	resetCompareFlags()
	srvA := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srvA.Close()
	srvB := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srvB.Close()

	resultsDir := t.TempDir()
	t.Setenv("ONETOKEN_DATA", t.TempDir())
	t.Setenv("FAKE_KEY", "sk-test")
	args := append([]string{"compare"}, append(compareBase(srvA.URL, srvB.URL, resultsDir),
		"--k", "16", "--n", "10", "--seed", "42", "--json")...)
	out, err := runCLI(t, args...)
	if err != nil {
		t.Fatalf("第一次 compare 失败: %v\n%s", err, out)
	}
	archiveDir := filepath.Join(resultsDir, "qwen__qwen3-8b")
	first := readArchiveJSON(t, archiveDir, "verdict.json")
	firstVerdictInfo, err := os.Stat(filepath.Join(archiveDir, "verdict.json"))
	if err != nil {
		t.Fatal(err)
	}
	// 删除一个归档文件：第二次运行若不重写归档则该文件缺失（防"第二次根本没写
	// 但断言全过"的假断言，审查 M2.12-正确性中）
	if err := os.Remove(filepath.Join(archiveDir, "target.json")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // 保证 ModTime 可区分（粗粒度文件系统）

	out, err = runCLI(t, args...)
	if err != nil {
		t.Fatalf("重测同模型应覆盖更新而非报错: %v\n%s", err, out)
	}
	second := readArchiveJSON(t, archiveDir, "verdict.json")
	if second["verdict"] != first["verdict"] {
		t.Fatalf("同参数重测判定应一致: %v → %v", first["verdict"], second["verdict"])
	}
	// 第二次确实发生了写盘：被删文件重建 + verdict.json ModTime 更新
	if _, err := os.Stat(filepath.Join(archiveDir, "target.json")); err != nil {
		t.Fatalf("重测应重建被删的 target.json: %v", err)
	}
	secondVerdictInfo, err := os.Stat(filepath.Join(archiveDir, "verdict.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !secondVerdictInfo.ModTime().After(firstVerdictInfo.ModTime()) {
		t.Fatalf("重测应原子覆盖 verdict.json（ModTime 未更新：%v → %v）",
			firstVerdictInfo.ModTime(), secondVerdictInfo.ModTime())
	}
	assertNoTmpLeftover(t, archiveDir)
	// 覆盖写后仍是四个固定文件（不多不少）
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("归档目录应为 4 个固定文件，实际 %v", names)
	}
}

// TestCompareModelPathTraversal：--model 含路径分隔符/父目录引用时，归档落点
// 必须经 SanitizeID 收敛在 resultsDir 内（防 compare 层改动绕过清洗，审查 M2.12-安全 L4）。
func TestCompareModelPathTraversal(t *testing.T) {
	resetCompareFlags()
	srvA := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srvA.Close()
	srvB := httptest.NewServer(fakeHandler([]string{"13", "24", "66"}, ""))
	defer srvB.Close()

	resultsDir := t.TempDir()
	t.Setenv("ONETOKEN_DATA", t.TempDir())
	t.Setenv("FAKE_KEY", "sk-test")
	args := []string{"compare",
		"--ref-base-url", srvA.URL, "--ref-api-key-env", "FAKE_KEY", "--ref-protocol", "chat",
		"--target-base-url", srvB.URL, "--target-api-key-env", "FAKE_KEY", "--target-protocol", "chat",
		"--model", "../evil", "--results-dir", resultsDir,
		"--k", "16", "--n", "10", "--seed", "42", "--json", "--no-report"}
	out, err := runCLI(t, args...)
	if err != nil {
		t.Fatalf("恶意模型名 compare 失败: %v\n%s", err, out)
	}
	// 归档落点：resultsDir 直下一层（SanitizeID("../evil") = "..__evil"，/ 已转义，
	// 无路径分隔符残留 → 无目录逃逸）
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("resultsDir 下应只有一个模型文件夹，实际 %v", names)
	}
	dirName := entries[0].Name()
	if dirName != "..__evil" {
		t.Fatalf("模型文件夹名应为 SanitizeID 后的 ..__evil，实际 %q", dirName)
	}
	if strings.ContainsRune(dirName, '/') || strings.ContainsRune(dirName, '\\') {
		t.Fatalf("模型文件夹名含路径分隔符（目录逃逸）: %q", dirName)
	}
	if _, err := os.Stat(filepath.Join(resultsDir, dirName, "verdict.json")); err != nil {
		t.Fatalf("归档未落在清洗后的目录内: %v", err)
	}
}

// TestCompareSaveResponses：--save-responses 写 reference.jsonl/target.jsonl
// 到模型文件夹（v0.24 起不再落 data store responses/）。
func TestCompareSaveResponses(t *testing.T) {
	resetCompareFlags()
	srvA := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srvA.Close()
	srvB := httptest.NewServer(fakeHandler([]string{"13", "24", "66"}, ""))
	defer srvB.Close()

	dataDir := t.TempDir()
	resultsDir := t.TempDir()
	t.Setenv("ONETOKEN_DATA", dataDir)
	t.Setenv("FAKE_KEY", "sk-test")
	args := append([]string{"compare"}, append(compareBase(srvA.URL, srvB.URL, resultsDir),
		"--k", "16", "--n", "10", "--seed", "42", "--save-responses", "--json", "--no-report")...)
	out, err := runCLI(t, args...)
	if err != nil {
		t.Fatalf("compare --save-responses 失败: %v\n%s", err, out)
	}
	archiveDir := filepath.Join(resultsDir, "qwen__qwen3-8b")
	for _, name := range []string{"reference.jsonl", "target.jsonl"} {
		data, err := os.ReadFile(filepath.Join(archiveDir, name))
		if err != nil {
			t.Fatalf("归档缺取证文件 %s: %v", name, err)
		}
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if len(lines) == 0 || lines[0] == "" {
			t.Fatalf("%s 应非空", name)
		}
		for i, line := range lines {
			var r map[string]any
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				t.Fatalf("%s 第 %d 行非 JSON: %v", name, i+1, err)
			}
			if r["raw_sha256"] == nil || r["raw_sha256"] == "" {
				t.Fatalf("%s 第 %d 行缺 raw_sha256（证据链）", name, i+1)
			}
		}
	}
	// 行为变更：不再写 data store 的 responses/ 目录
	if _, err := os.Stat(filepath.Join(dataDir, "responses")); !os.IsNotExist(err) {
		t.Fatalf("--save-responses 不应再落 data store responses/（err=%v）", err)
	}
}

// TestCompareErrorNoArchive：测量有效性门未过（参考端全空回答 → 有效 cell < k_min
// 报错）等判定未走出的路径不写归档（避免半截结果误导）。
func TestCompareErrorNoArchive(t *testing.T) {
	resetCompareFlags()
	srvA := httptest.NewServer(fakeHandler([]string{""}, "")) // 全空回答 → 参考端无有效 cell
	defer srvA.Close()
	srvB := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srvB.Close()

	resultsDir := t.TempDir()
	t.Setenv("ONETOKEN_DATA", t.TempDir())
	t.Setenv("FAKE_KEY", "sk-test")
	args := append([]string{"compare"}, append(compareBase(srvA.URL, srvB.URL, resultsDir),
		"--k", "8", "--n", "10", "--seed", "42", "--json", "--no-report")...)
	out, err := runCLI(t, args...)
	if err == nil {
		t.Fatalf("参考端无有效 cell 应报错，实际成功: %s", out)
	}
	if _, statErr := os.Stat(filepath.Join(resultsDir, "qwen__qwen3-8b")); !os.IsNotExist(statErr) {
		t.Fatalf("错误路径不应写归档目录（err=%v）", statErr)
	}
}

func TestCompareStdoutSummaryNoTable(t *testing.T) {
	resetCompareFlags()
	srvA := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srvA.Close()
	srvB := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srvB.Close()
	resultsDir := t.TempDir()
	t.Setenv("ONETOKEN_DATA", t.TempDir())
	t.Setenv("FAKE_KEY", "sk-test")
	args := append([]string{"compare"}, append(compareBase(srvA.URL, srvB.URL, resultsDir),
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
	// v0.24：摘要行尾附归档目录
	wantArchive := "归档: " + filepath.Join(resultsDir, "qwen__qwen3-8b")
	if !strings.Contains(out, wantArchive) {
		t.Fatalf("摘要应含归档目录 %q: %q", wantArchive, out)
	}
	if strings.Contains(out, "<table") || strings.Contains(out, "|") {
		t.Fatalf("stdout 不应输出表格: %q", out)
	}
}
