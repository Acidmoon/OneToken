package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHandler 模拟 OpenAI chat 兼容端点：按 prompt 哈希 + 请求计数从答案池
// 返回（T=1.0 方差轮换；T=0 同一 prompt 恒定=确定性）。不同池 = 不同"模型"。
func fakeHandler(pool []string, upstream string) http.HandlerFunc {
	var counter atomic.Int64
	reply := func(w http.ResponseWriter, raw string) {
		if upstream != "" {
			w.Header().Set("X-Openrouter-Via", upstream)
		}
		w.Write([]byte(raw))
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var prompt string
		var temp float64
		var raw string
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			var req struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
				Temperature float64 `json:"temperature"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for _, m := range req.Messages {
				prompt += m.Content
			}
			temp = req.Temperature
			raw = fmt.Sprintf(`{"choices":[{"message":{"content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`, "")
		case strings.HasSuffix(r.URL.Path, "/responses"):
			var req struct {
				Input       string  `json:"input"`
				Temperature float64 `json:"temperature"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			prompt, temp = req.Input, req.Temperature
			raw = `{"output":[{"type":"message","content":[{"type":"output_text","text":""}],"finish_reason":"stop"}],"usage":{"input_tokens":5,"output_tokens":1}}`
		case strings.HasSuffix(r.URL.Path, "/messages"):
			var req struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
				Temperature float64 `json:"temperature"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for _, m := range req.Messages {
				prompt += m.Content
			}
			temp = req.Temperature
			raw = `{"content":[{"type":"text","text":""}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1}}`
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h := fnvHash(prompt)
		idx := h % uint64(len(pool))
		if temp != 0 {
			idx = (h + uint64(counter.Add(1))) % uint64(len(pool)) // 方差轮换
		}
		raw = strings.Replace(raw, `""`, fmt.Sprintf("%q", pool[idx]), 1)
		reply(w, raw)
	}
}

func fnvHash(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// runCLI 执行一条 onetoken 命令并捕获 stdout。
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }() // 审查 L6：panic 也恢复，防污染后续测试
	rootCmd.SetArgs(args)
	rootCmd.SetOut(w)
	rootCmd.SetErr(os.Stderr)
	err = rootCmd.Execute()
	w.Close()
	data, _ := io.ReadAll(r) // ReadAll 防大输出写阻塞死锁
	r.Close()
	return string(data), err
}

// ---- 端到端冒烟：enroll → audit（同模型 pass / impostor suspicious） ----

func TestE2EEnrollAudit(t *testing.T) {
	// 模型 A：数字分布；模型 B：不同数字分布（冒充场景）
	srvA := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, "deepseek"))
	defer srvA.Close()
	srvB := httptest.NewServer(fakeHandler([]string{"13", "24", "66"}, ""))
	defer srvB.Close()

	t.Setenv("ONETOKEN_DATA", t.TempDir())
	t.Setenv("FAKE_KEY", "sk-test")
	base := []string{"--base-url", srvA.URL, "--api-key-env", "FAKE_KEY", "--protocol", "chat"}

	// 1. enroll 模型 A（直传参数）
	args := append([]string{"enroll"}, append(base,
		"--model", "qwen/qwen3-8b", "--version", "v1", "--json")...)
	out, err := runCLI(t, args...)
	if err != nil {
		t.Fatalf("enroll 失败: %v\n%s", err, out)
	}
	var enrolled map[string]any
	if err := json.Unmarshal([]byte(out), &enrolled); err != nil {
		t.Fatalf("enroll 输出非 JSON: %v\n%s", err, out)
	}
	if enrolled["ref_source"] != "official-api" || enrolled["cells"] == nil {
		t.Fatalf("enroll 输出异常: %v", enrolled)
	}

	// 2. audit 同模型端点（模型 A）→ pass（--tau 直传，冒烟无校准库）
	args = append([]string{"audit"}, append(base,
		"--claimed-model", "qwen/qwen3-8b", "--k", "16", "--n", "10",
		"--tau", "0.20", "--seed", "42", "--json")...)
	out, err = runCLI(t, args...)
	if err != nil {
		t.Fatalf("audit 失败: %v\n%s", err, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("audit 输出非 JSON: %v\n%s", err, out)
	}
	t.Logf("audit 输出: %s", out)
	if res["verdict"] != "pass" {
		t.Fatalf("同模型审计应 pass，实际 %v（score=%v）", res["verdict"], res["score"])
	}
	// 同分布 score 应有采样噪声（轮换起点差异），远低于 τ=0.2 即可
	if res["score"].(float64) > 0.05 {
		t.Fatalf("同分布 score 应接近 0，实际 %v", res["score"])
	}

	// 3. impostor：模型 B 端点冒充模型 A → suspicious
	args = append([]string{"audit"},
		"--base-url", srvB.URL, "--api-key-env", "FAKE_KEY", "--protocol", "chat",
		"--claimed-model", "qwen/qwen3-8b", "--k", "16", "--n", "10",
		"--tau", "0.20", "--seed", "42", "--json")
	out, err = runCLI(t, args...)
	if err != nil {
		t.Fatalf("impostor audit 失败: %v\n%s", err, out)
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("impostor audit 输出非 JSON: %v\n%s", err, out)
	}
	if res["verdict"] != "suspicious" {
		t.Fatalf("impostor 审计应 suspicious，实际 %v（score=%v）", res["verdict"], res["score"])
	}
	if res["score"].(float64) < 0.9 {
		t.Fatalf("不相交分布 score 应 ≈1，实际 %v", res["score"])
	}
}

// ---- 冒烟：参考指纹缺失的 audit 应报错（前置条件） ----

func TestE2EAuditWithoutEnroll(t *testing.T) {
	srv := httptest.NewServer(fakeHandler([]string{"42"}, ""))
	defer srv.Close()
	t.Setenv("ONETOKEN_DATA", t.TempDir())
	t.Setenv("FAKE_KEY", "sk-test")

	out, err := runCLI(t, "audit",
		"--base-url", srv.URL, "--api-key-env", "FAKE_KEY", "--protocol", "chat",
		"--claimed-model", "ghost/model", "--k", "8", "--n", "10")
	if err == nil {
		t.Fatalf("未 enroll 的 audit 应报错，实际成功: %s", out)
	}
	if !strings.Contains(err.Error(), "参考指纹不存在") {
		t.Fatalf("错误应提示先 enroll: %v", err)
	}
}

// ---- 冒烟：probe 输出 flags（健康端点无 flag） ----

func TestE2EProbe(t *testing.T) {
	srv := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srv.Close()
	t.Setenv("ONETOKEN_DATA", t.TempDir())
	t.Setenv("FAKE_KEY", "sk-test")

	out, err := runCLI(t, "probe",
		"--base-url", srv.URL, "--api-key-env", "FAKE_KEY", "--protocol", "chat",
		"--model", "qwen/qwen3-8b", "--json")
	if err != nil {
		t.Fatalf("probe 失败: %v\n%s", err, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("probe 输出非 JSON: %v\n%s", err, out)
	}
	flags, ok := res["flags"].([]any)
	if !ok {
		t.Fatalf("flags 字段异常: %v", res)
	}
	if len(flags) != 0 {
		t.Fatalf("健康端点不应有测量 flag，实际 %v", flags)
	}
}

// ---- 启动性能：--help <50ms（设计 §8 性能特征） ----

func TestE2EHelpLatency(t *testing.T) {
	t.Setenv("ONETOKEN_DATA", t.TempDir())
	start := nowNano()
	out, err := runCLI(t, "--help")
	if err != nil {
		t.Fatalf("--help 失败: %v", err)
	}
	elapsedMS := float64(nowNano()-start) / 1e6
	if !strings.Contains(out, "enroll") {
		t.Fatal("--help 应列出子命令")
	}
	if elapsedMS > 50 {
		t.Fatalf("--help 启动耗时 %.1fms > 50ms（设计 §8 性能特征）", elapsedMS)
	}
}

// nowNano 供启动耗时测试使用。
func nowNano() int64 { return time.Now().UnixNano() }

// ---- 审查回归：三协议 enroll、--tau auto 无库拒绝、上游 provider 透传 ----

func TestE2EEnrollProtocols(t *testing.T) {
	// 三协议各跑通一次 enroll（设计 §9.2 ③）
	t.Setenv("ONETOKEN_DATA", t.TempDir())
	t.Setenv("FAKE_KEY", "sk-test")
	pool := []string{"42", "57", "88"}
	for _, proto := range []struct{ name, endpoint string }{
		{"chat", "chat"},
		{"responses", "responses"},
		{"anthropic", "anthropic"},
	} {
		t.Run(proto.name, func(t *testing.T) {
			srv := httptest.NewServer(fakeHandler(pool, ""))
			defer srv.Close()
			model := "proto/" + proto.name
			out, err := runCLI(t, "enroll",
				"--base-url", srv.URL, "--api-key-env", "FAKE_KEY", "--protocol", proto.endpoint,
				"--model", model, "--version", "v1", "--json")
			if err != nil {
				t.Fatalf("%s enroll 失败: %v\n%s", proto.endpoint, err, out)
			}
		})
	}
}

func TestE2EAuditNoCalibration(t *testing.T) {
	// --tau auto（默认）且校准库为空 → ErrNoCalibration 拒绝（设计 §3.4）
	srv := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, ""))
	defer srv.Close()
	t.Setenv("ONETOKEN_DATA", t.TempDir())
	t.Setenv("FAKE_KEY", "sk-test")
	base := []string{"--base-url", srv.URL, "--api-key-env", "FAKE_KEY", "--protocol", "chat"}
	if _, err := runCLI(t, append([]string{"enroll"}, append(base, "--model", "m/a", "--version", "v1")...)...); err != nil {
		t.Fatal(err)
	}
	// k=16+seed 固定：确保 cellsUsed ≥ k_min 后到达校准匹配（与 pass 测试同参数）
	out, err := runCLI(t, append([]string{"audit"}, append(base,
		"--claimed-model", "m/a", "--k", "16", "--n", "10", "--seed", "42", "--tau", "0")...)...)
	t.Logf("err=%v out=%s", err, out)
	if err == nil || !strings.Contains(err.Error(), "无匹配校准档") {
		t.Fatalf("无校准库应拒绝审计（ErrNoCalibration），实际 %v", err)
	}
}

func TestE2EUpstreamProvider(t *testing.T) {
	// 上游 provider 透传：mock 返回 X-Openrouter-Via → audit 输出 upstream 字段
	srv := httptest.NewServer(fakeHandler([]string{"42", "57", "88"}, "deepseek"))
	defer srv.Close()
	t.Setenv("ONETOKEN_DATA", t.TempDir())
	t.Setenv("FAKE_KEY", "sk-test")
	base := []string{"--base-url", srv.URL, "--api-key-env", "FAKE_KEY", "--protocol", "chat"}
	if _, err := runCLI(t, append([]string{"enroll"}, append(base, "--model", "m/up", "--version", "v1")...)...); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, append([]string{"audit"}, append(base,
		"--claimed-model", "m/up", "--k", "16", "--n", "10", "--tau", "0.2", "--seed", "7", "--json")...)...)
	if err != nil {
		t.Fatalf("audit 失败: %v\n%s", err, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res["upstream"] != "deepseek" {
		t.Fatalf("upstream=%v，期望 deepseek（X-Openrouter-Via 透传）", res["upstream"])
	}
}
