package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// newTestStore 创建临时 data/ 根目录的 Store。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "data"))
}

// testResponse 构造一条带哈希的合法响应。
func testResponse(cell string, idx int) *Response {
	return &Response{
		Cell:           cell,
		SampleIdx:      idx,
		Temperature:    1.0,
		RawCompletion:  "42",
		RawSHA256:      SHA256Hex("42"),
		Classification: ClassValid,
		TS:             "2026-08-05T00:00:00Z",
	}
}

// --- 原子写与目录自动创建 ---

func TestAtomicWriteCreatesDirs(t *testing.T) {
	s := newTestStore(t)
	m := []Model{{ID: "qwen/qwen3-8b", Family: "qwen"}}
	if err := s.SaveModels(m); err != nil {
		t.Fatalf("SaveModels: %v", err)
	}
	// 目录自动创建且无 .tmp-* 残留
	if _, err := os.Stat(s.modelsPath()); err != nil {
		t.Fatalf("models.json 不存在: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(s.modelsPath()))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("原子写残留临时文件: %s", e.Name())
		}
	}
}

// --- WriteJSONAtomic 导出包装（v0.24/M2.12：compare 归档复用原子写） ---

func TestWriteJSONAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results", "qwen__qwen3-8b", "verdict.json")
	v := map[string]any{"schema_version": 1, "verdict": "pass"}
	if err := WriteJSONAtomic(path, v); err != nil {
		t.Fatalf("WriteJSONAtomic: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("写入后读取: %v", err)
	}
	if !strings.Contains(string(data), `"verdict": "pass"`) || !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("内容异常: %q", data)
	}
	// 覆盖写（重测同模型即更新）且无 .tmp-* 残留
	v["verdict"] = "suspicious"
	if err := WriteJSONAtomic(path, v); err != nil {
		t.Fatalf("覆盖写: %v", err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), "suspicious") {
		t.Fatalf("覆盖未生效: %q", data)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("原子写残留临时文件: %s", e.Name())
		}
	}
	// 不可序列化值 → 报错且不产生目标文件
	if err := WriteJSONAtomic(filepath.Join(t.TempDir(), "bad.json"), func() {}); err == nil {
		t.Fatal("不可序列化值应报错")
	}
}

// --- 模型/指纹/审计/校准/漂移 往返 ---

func TestModelRoundTrip(t *testing.T) {
	s := newTestStore(t)
	in := []Model{{ID: "qwen/qwen3-8b", Vendor: "qwen", Family: "qwen", ModelType: "open-source", RefSource: "local"}}
	if err := s.SaveModels(in); err != nil {
		t.Fatal(err)
	}
	out, err := s.LoadModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].ID != "qwen/qwen3-8b" || out[0].RefSource != "local" {
		t.Fatalf("往返不一致: %+v", out)
	}
	// 不存在时返回空列表
	s2 := newTestStore(t)
	empty, err := s2.LoadModels()
	if err != nil || len(empty) != 0 {
		t.Fatalf("空库应返回空列表: %v %v", empty, err)
	}
}

func TestFingerprintRoundTripAndList(t *testing.T) {
	s := newTestStore(t)
	fp := &Fingerprint{
		SchemaVersion: schemaVersion,
		ModelID:       "qwen/qwen3-8b",
		Version:       "2026-07-11v1",
		CollectedAt:   "2026-08-05T00:00:00Z",
		RefSource:     "local",
		Cells: map[string]CellDist{
			"random_number_100:en": {Dist: map[string]int{"42": 12, "57": 6}, N: 18, T: 1.0, ValidRate: 0.99},
		},
		T0Cells: map[string]CellDist{
			"random_number_100:en": {Dist: map[string]int{"42": 3}, N: 3, T: 0.0},
		},
		QCFlags: []string{"ok"},
	}
	if err := s.SaveFingerprint(fp); err != nil {
		t.Fatal(err)
	}
	// 入参不应被修改（SaveFingerprint 复制入参）
	if fp.SchemaVersion != schemaVersion {
		t.Fatalf("SaveFingerprint 修改了入参 SchemaVersion: %d", fp.SchemaVersion)
	}
	got, err := s.LoadFingerprint("qwen/qwen3-8b")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fp, got) {
		t.Fatalf("指纹往返不一致:\n  in: %+v\n  out: %+v", fp, got)
	}
	// ListFingerprints 返回真实 id（含 '/'，sanitize 不可逆）
	ids, err := s.ListFingerprints()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "qwen/qwen3-8b" {
		t.Fatalf("ListFingerprints 应返回真实 id: %v", ids)
	}
	// 不存在时
	if _, err := s.LoadFingerprint("nope/nope"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("应返回 os.ErrNotExist: %v", err)
	}
}

func TestAuditRoundTripAndList(t *testing.T) {
	s := newTestStore(t)
	a := &Audit{
		SchemaVersion:         schemaVersion,
		ID:                    "audit-001",
		Endpoint:              "https://openrouter.ai openai/gpt-5.1",
		ClaimedModel:          "openai/gpt-5.1",
		RefFingerprintVersion: "2026-07-11v1",
		K:                     8, N: 15,
		SelectedCells: []string{"random_number_100:en"},
		Seed:          12345,
		Score:         0.18, Threshold: 0.15, ThresholdScope: "global",
		Verdict:     VerdictPass,
		CellsDetail: map[string]float64{"random_number_100:en": 0.12},
		AuditedAt:   "2026-08-05T00:00:00Z",
	}
	if err := s.SaveAudit(a); err != nil {
		t.Fatal(err)
	}
	if a.SchemaVersion != schemaVersion {
		t.Fatalf("SaveAudit 修改了入参 SchemaVersion: %d", a.SchemaVersion)
	}
	got, err := s.LoadAudit("audit-001")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, got) {
		t.Fatalf("审计往返不一致:\n  in: %+v\n  out: %+v", a, got)
	}
	ids, err := s.ListAudits()
	if err != nil || len(ids) != 1 || ids[0] != "audit-001" {
		t.Fatalf("ListAudits: %v %v", ids, err)
	}
}

func TestCalibrationsAndDriftRoundTrip(t *testing.T) {
	s := newTestStore(t)
	cs := []Calibration{{
		Scope: "global", K: 8, NPerCell: 15, RefChannel: "local", TargetChannel: "aggregator",
		GenuineN: 100, ImpostorN: 1000, AUC: 0.971, EER: 0.073,
		TauFPR1: 0.30, TauFPR1TPR: 0.65, TPRCI: []float64{0.55, 0.75}, TauFPR5: 0.25,
		ROC:          []ROCPoint{{FPR: 0.01, TPR: 0.65}, {FPR: 0.05, TPR: 0.90}},
		CalibratedAt: "2026-08-05T00:00:00Z",
	}}
	if err := s.SaveCalibrations(cs); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadCalibrations()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cs, got) {
		t.Fatalf("校准往返不一致:\n  in: %+v\n  out: %+v", cs, got)
	}
	d := []DriftEntry{{ModelID: "qwen/qwen3-8b", RefFingerprintVersion: "v1", AuditID: "a1", Scores: []float64{0.10, 0.12}, Flag: DriftOK, UpdatedAt: "2026-08-05T00:00:00Z"}}
	if err := s.SaveDrift(d); err != nil {
		t.Fatal(err)
	}
	gd, err := s.LoadDrift()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d, gd) {
		t.Fatalf("漂移往返不一致:\n  in: %+v\n  out: %+v", d, gd)
	}
}

// --- JSONL 追加 / 读回 / 幂等索引 ---

func TestResponsesAppendAndLoad(t *testing.T) {
	s := newTestStore(t)
	rs := []*Response{
		testResponse("random_number_100:en", 0),
		testResponse("random_number_100:en", 1),
		testResponse("random_number_10:zh", 0),
	}
	for _, r := range rs {
		if err := s.AppendResponse("audit-001", r); err != nil {
			t.Fatalf("AppendResponse: %v", err)
		}
	}
	// 追加不重写：再追加一条，读回 4 条
	if err := s.AppendResponse("audit-001", testResponse("random_number_10:zh", 1)); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadResponses("audit-001")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("读回 %d 条，期望 4", len(got))
	}
	if got[2].Cell != "random_number_10:zh" || got[2].SampleIdx != 0 {
		t.Fatalf("顺序/内容错误: %+v", got[2])
	}
	// 幂等索引
	idx, err := s.LoadResponsesIndex("audit-001")
	if err != nil {
		t.Fatal(err)
	}
	if len(idx) != 4 || !idx[ResponseKey("random_number_100:en", 0)] || !idx[ResponseKey("random_number_10:zh", 1)] {
		t.Fatalf("幂等索引错误: %v", idx)
	}
	// 不存在时返回空
	s2 := newTestStore(t)
	empty, err := s2.LoadResponses("nope")
	if err != nil || len(empty) != 0 {
		t.Fatalf("空响应应返回空: %v %v", empty, err)
	}
}

// 幂等重跑：追加同 key 响应，索引正确识别已完成样本。
func TestAppendIdempotentRerun(t *testing.T) {
	s := newTestStore(t)
	if err := s.AppendResponse("a1", testResponse("coin_flip:en", 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendResponse("a1", testResponse("coin_flip:en", 0)); err != nil {
		t.Fatal(err) // 重复追加由调用方用索引跳过；store 本身不拒绝（JSONL 是 append-only 日志）
	}
	idx, err := s.LoadResponsesIndex("a1")
	if err != nil {
		t.Fatal(err)
	}
	// 调用方用索引判定：该 key 已完成，不应再次采集
	if !idx[ResponseKey("coin_flip:en", 0)] {
		t.Fatal("已完成样本应出现在索引中")
	}
	if len(idx) != 1 {
		t.Fatalf("同 key 两次追加后索引应去重为 1: %v", idx)
	}
}

// --- 证据链 ---

func TestSHA256Hex(t *testing.T) {
	if SHA256Hex("42") != SHA256Hex("42") {
		t.Fatal("哈希应确定")
	}
	if SHA256Hex("42") == SHA256Hex("43") {
		t.Fatal("不同内容哈希应不同")
	}
	if len(SHA256Hex("42")) != 64 {
		t.Fatalf("sha256 hex 长度应为 64: %s", SHA256Hex("42"))
	}
}

func TestAppendResponseRequiresHash(t *testing.T) {
	s := newTestStore(t)
	r := &Response{Cell: "x:en", SampleIdx: 0, RawCompletion: "42", Classification: ClassValid}
	if err := s.AppendResponse("a1", r); err == nil {
		t.Fatal("缺少 raw_sha256 应被拒绝（证据链要求）")
	}
}

// --- schema_version 校验（覆盖全部整文件读取路径） ---

func TestSchemaVersionRejected(t *testing.T) {
	cases := []struct {
		name string
		path string
		json string
		load func(*Store) error
	}{
		{"models", "models.json", `{"schema_version":2,"models":[]}`,
			func(s *Store) error { _, e := s.LoadModels(); return e }},
		{"fingerprint", "fingerprints/qwen__qwen3-8b.json", `{"schema_version":2,"model_id":"qwen/qwen3-8b"}`,
			func(s *Store) error { _, e := s.LoadFingerprint("qwen/qwen3-8b"); return e }},
		{"audit", "audits/a1.json", `{"schema_version":2,"id":"a1"}`,
			func(s *Store) error { _, e := s.LoadAudit("a1"); return e }},
		{"calibrations", "calibrations.json", `{"schema_version":2,"calibrations":[]}`,
			func(s *Store) error { _, e := s.LoadCalibrations(); return e }},
		{"drift", "drift.json", `{"schema_version":2,"entries":[]}`,
			func(s *Store) error { _, e := s.LoadDrift(); return e }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestStore(t)
			p := filepath.Join(s.root, c.path)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(c.json), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := c.load(s); err == nil {
				t.Fatalf("%s schema_version=2 应拒绝加载", c.name)
			}
		})
	}
}

// --- sanitize 路径穿越与跨平台文件名防护 ---

func TestSanitizeBlocksTraversal(t *testing.T) {
	// 关键：结果不得含路径分隔符，也不得是 ".."/"." 本身（目录穿越的充要条件）
	for _, evil := range []string{"../../etc/passwd", `..\..\etc`, ".", ".."} {
		if got := sanitize(evil); strings.ContainsAny(got, `/\`) || got == ".." || got == "." {
			t.Fatalf("sanitize 未阻止路径穿越: %q -> %q", evil, got)
		}
	}
	if got := sanitize("qwen/qwen3-8b"); got != "qwen__qwen3-8b" {
		t.Fatalf("sanitize 规整错误: %q", got)
	}
	if got := sanitize(""); got != "_" {
		t.Fatalf("空 id 应归一为 _: %q", got)
	}
}

func TestSanitizeWindowsReservedAndIllegal(t *testing.T) {
	// Windows 保留设备名（大小写不敏感）与非法字符、NUL 必须被处理
	for _, name := range []string{"CON", "nul", "Com1", "LPT9", "AUX"} {
		if got := sanitize(name); got == name {
			t.Fatalf("Windows 保留名应被处理: %q", name)
		}
	}
	for _, evil := range []string{"a*b?c", "a\x00b", `a<b>c`} {
		if got := sanitize(evil); strings.ContainsAny(got, `*?"<>|`) || strings.Contains(got, "\x00") {
			t.Fatalf("sanitize 未过滤非法字符: %q -> %q", evil, got)
		}
	}
}

// --- 并发追加（O_APPEND 单次 write 原子性） ---

func TestConcurrentAppendResponses(t *testing.T) {
	s := newTestStore(t)
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r := testResponse(fmt.Sprintf("cell_%d:en", idx%4), idx)
			if err := s.AppendResponse("a1", r); err != nil {
				t.Errorf("AppendResponse: %v", err)
			}
		}(i)
	}
	wg.Wait()
	got, err := s.LoadResponses("a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("并发追加后读回 %d 条，期望 %d（行应完整不撕裂）", len(got), n)
	}
}

// --- DefaultRoot ---

func TestDefaultRoot(t *testing.T) {
	t.Setenv("ONETOKEN_DATA", "/tmp/onetoken-test-data")
	if got := DefaultRoot(); got != "/tmp/onetoken-test-data" {
		t.Fatalf("ONETOKEN_DATA 覆盖失败: %s", got)
	}
	t.Setenv("ONETOKEN_DATA", "")
	home, _ := os.UserHomeDir()
	if got := DefaultRoot(); got != filepath.Join(home, ".onetoken", "data") {
		t.Fatalf("默认根错误: %s", got)
	}
}

// --- 导入导出往返（拷贝 data/ 目录即导出，解包即恢复） ---

func TestExportImportRoundTrip(t *testing.T) {
	src := newTestStore(t)
	if err := src.SaveModels([]Model{{ID: "m1", Family: "qwen"}}); err != nil {
		t.Fatal(err)
	}
	fp := &Fingerprint{SchemaVersion: schemaVersion, ModelID: "qwen/qwen3-8b", Version: "v1", CollectedAt: "t", RefSource: "local",
		Cells: map[string]CellDist{"c:en": {Dist: map[string]int{"1": 1}, N: 1, T: 1.0}}}
	if err := src.SaveFingerprint(fp); err != nil {
		t.Fatal(err)
	}
	if err := src.AppendResponse("a1", testResponse("c:en", 0)); err != nil {
		t.Fatal(err)
	}
	a := &Audit{SchemaVersion: schemaVersion, ID: "a1", ClaimedModel: "m1", Verdict: VerdictPass, AuditedAt: "t"}
	if err := src.SaveAudit(a); err != nil {
		t.Fatal(err)
	}
	if err := src.SaveCalibrations([]Calibration{{Scope: "global", K: 8, NPerCell: 15}}); err != nil {
		t.Fatal(err)
	}
	// 导出 = 复制整个目录
	dstRoot := filepath.Join(t.TempDir(), "imported")
	if err := copyDir(src.Root(), dstRoot); err != nil {
		t.Fatal(err)
	}
	dst := New(dstRoot)
	ms, err := dst.LoadModels()
	if err != nil || len(ms) != 1 || ms[0].ID != "m1" {
		t.Fatalf("导入模型: %v %v", ms, err)
	}
	fp2, err := dst.LoadFingerprint("qwen/qwen3-8b")
	if err != nil || !reflect.DeepEqual(fp, fp2) {
		t.Fatalf("导入指纹值不一致: %v", err)
	}
	rs, err := dst.LoadResponses("a1")
	if err != nil || len(rs) != 1 {
		t.Fatalf("导入响应: %v %v", rs, err)
	}
	a2, err := dst.LoadAudit("a1")
	if err != nil || !reflect.DeepEqual(a, a2) {
		t.Fatalf("导入审计值不一致: %v", err)
	}
	cs, err := dst.LoadCalibrations()
	if err != nil || len(cs) != 1 || cs[0].Scope != "global" {
		t.Fatalf("导入校准: %v %v", cs, err)
	}
	// 损坏源数据导入应报错（反向用例）：改坏指纹文件
	if err := os.WriteFile(filepath.Join(dstRoot, "fingerprints", "qwen__qwen3-8b.json"), []byte(`{broken`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.ListFingerprints(); err == nil {
		t.Fatal("损坏指纹文件应导致列表报错")
	}
}

// copyDir 递归复制目录（测试辅助）。
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
