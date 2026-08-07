// Package store 是 JSON/JSONL 文件存储层（设计文档 §4）。
//
// 存储形态（用户评审决议 v0.5）：分目录 JSON/JSONL，替代 SQLite。
//   - 按语义分片：指纹按模型、审计/响应按 audit_id 分文件；
//   - 原子写：整文件写 临时文件 + os.Rename（POSIX 原子）；
//   - JSONL 追加：responses 用 O_APPEND 单行追加（O(1)，只增不改，证据链）；
//   - 幂等：重跑审计读回本次响应文件建 cell+sample_idx 内存索引去重；
//   - 证据链：每条响应含 raw_sha256；文件顶层含 schema_version，读取校验。
package store

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// schemaVersion 是当前数据格式版本（所有 JSON 文件顶层字段）。
const schemaVersion = 1

// Verdict 枚举（设计 §4.3 audits.verdict）。
const (
	VerdictPending      = "pending"      // 预检阶段先行建文件
	VerdictPass         = "pass"         // s ≤ τ
	VerdictSuspicious   = "suspicious"   // s > τ
	VerdictInconclusive = "inconclusive" // |s−τ| 在 bootstrap CI 内
	VerdictError        = "error"        // 采集/测量失败
)

// Classification 枚举（设计 §3.2）。
const (
	ClassValid   = "valid"
	ClassInvalid = "invalid"
	ClassRefusal = "refusal"
	ClassEmpty   = "empty"
)

// --- 类型定义（对应设计 §4.3 文件结构） ---

// Model 是模型目录条目。
type Model struct {
	ID              string `json:"id"`
	Vendor          string `json:"vendor,omitempty"`
	Family          string `json:"family,omitempty"`
	ModelType       string `json:"model_type,omitempty"` // open-source | proprietary
	RefSource       string `json:"ref_source,omitempty"` // official-api | none（单通道，v0.15）
	Provider        string `json:"provider,omitempty"`   // 参考来源 provider（同 provider 比对优先，§7.2）
	CatalogSnapshot string `json:"catalog_snapshot,omitempty"`
	Notes           string `json:"notes,omitempty"`
}

// CellDist 是单个 cell 的答案分布（dist 为原始计数，归一化在 fingerprint 层）。
type CellDist struct {
	Dist      map[string]int `json:"dist"`
	N         int            `json:"n"` // 实际有效样本数
	T         float64        `json:"t"`
	ValidRate float64        `json:"valid_rate,omitempty"`
}

// Fingerprint 是模型指纹（含版本链）。
type Fingerprint struct {
	SchemaVersion int                 `json:"schema_version"`
	ModelID       string              `json:"model_id"`
	Version       string              `json:"version"`      // e.g. 2026-07-11v1
	CollectedAt   string              `json:"collected_at"` // UTC Z
	RefSource     string              `json:"ref_source"`
	Provider      string              `json:"provider,omitempty"` // 参考来源 provider（§7.2 同 provider 比对优先）
	Channel       string              `json:"channel,omitempty"`  // 采样通道：direct（非推理，缺省）| reasoning（post-reasoning 回答指纹，v0.19 系统 2）
	Cells         map[string]CellDist `json:"cells"`              // "task:lang" -> 分布
	T0Cells       map[string]CellDist `json:"t0_cells,omitempty"`
	QCFlags       []string            `json:"qc_flags,omitempty"`
	SupersededBy  string              `json:"superseded_by,omitempty"`
}

// Response 是单次采样响应（responses/<audit_id>.jsonl 的一行）。
type Response struct {
	Cell             string          `json:"cell"` // "task:lang"
	SampleIdx        int             `json:"sample_idx"`
	Temperature      float64         `json:"temperature"`
	PromptHash       string          `json:"prompt_hash,omitempty"`
	RawCompletion    string          `json:"raw_completion"`
	RawSHA256        string          `json:"raw_sha256"`
	Text             string          `json:"text,omitempty"` // 提取的回答文本（post-reasoning content；归一化/分类输入，非 RawCompletion）
	Normalized       string          `json:"normalized,omitempty"`
	Classification   string          `json:"classification"` // valid|invalid|refusal|empty
	ReasoningTokens  int             `json:"reasoning_tokens,omitempty"`
	CompletionTokens int             `json:"completion_tokens,omitempty"`
	FinishReason     string          `json:"finish_reason,omitempty"`
	LatencyMS        int64           `json:"latency_ms,omitempty"`
	Provider         string          `json:"provider,omitempty"`
	ReportedModel    string          `json:"reported_model,omitempty"`
	Usage            json.RawMessage `json:"usage,omitempty"` // 协议原始 usage（差异已由 provider 层归一）
	CostUSD          float64         `json:"cost_usd,omitempty"`
	TS               string          `json:"ts"` // UTC Z
}

// Audit 是单次审计结果（audits/<audit_id>.json）。
type Audit struct {
	SchemaVersion         int                `json:"schema_version"`
	ID                    string             `json:"id"`
	Endpoint              string             `json:"endpoint"` // base_url + model string
	ClaimedModel          string             `json:"claimed_model"`
	RefFingerprintVersion string             `json:"ref_fingerprint_version,omitempty"`
	K                     int                `json:"k"`
	N                     int                `json:"n"`
	SelectedCells         []string           `json:"selected_cells"`
	Seed                  int64              `json:"seed"`
	Score                 float64            `json:"score"`
	Threshold             float64            `json:"threshold"`
	ThresholdScope        string             `json:"threshold_scope,omitempty"` // global|family:<x>|size-tier
	Verdict               string             `json:"verdict"`
	CellsDetail           map[string]float64 `json:"cells_detail,omitempty"`
	QCFlags               []string           `json:"qc_flags,omitempty"`
	Provider              string             `json:"provider,omitempty"` // 上游路由（OpenRouter 透传，§9.2 解释不稳定）
	AuditedAt             string             `json:"audited_at"`         // UTC Z
}

// ROCPoint 是 ROC 曲线上的一个点（FPR, TPR）。
type ROCPoint struct {
	FPR float64 `json:"fpr"`
	TPR float64 `json:"tpr"`
}

// Calibration 是一次校准结果（按 (scope,k,n,通道) 分档）。
type Calibration struct {
	Scope         string     `json:"scope"`
	K             int        `json:"k"`
	NPerCell      int        `json:"n"`
	RefChannel    string     `json:"ref_channel,omitempty"`
	TargetChannel string     `json:"target_channel,omitempty"`
	GenuineN      int        `json:"genuine_n,omitempty"`
	ImpostorN     int        `json:"impostor_n,omitempty"`
	AUC           float64    `json:"auc,omitempty"`
	EER           float64    `json:"eer,omitempty"`
	TauFPR1       float64    `json:"tau_fpr1,omitempty"`
	TauFPR1TPR    float64    `json:"tau_fpr1_tpr,omitempty"`
	TPRCI         []float64  `json:"tpr_ci,omitempty"`
	TauFPR5       float64    `json:"tau_fpr5,omitempty"`
	ROC           []ROCPoint `json:"roc,omitempty"`
	CalibratedAt  string     `json:"calibrated_at,omitempty"`
}

// DriftFlag 枚举。
const (
	DriftOK     = "ok"
	DriftRising = "rising"
	DriftStale  = "stale"
)

// DriftEntry 是漂移趋势条目。
type DriftEntry struct {
	ModelID               string    `json:"model_id"`
	RefFingerprintVersion string    `json:"ref_fingerprint_version"`
	AuditID               string    `json:"audit_id,omitempty"`
	Scores                []float64 `json:"scores"`
	Flag                  string    `json:"flag"` // ok|rising|stale
	UpdatedAt             string    `json:"updated_at,omitempty"`
}

// --- 容器（全量小文件） ---

type modelsFile struct {
	SchemaVersion int     `json:"schema_version"`
	Models        []Model `json:"models"`
}

type calibrationsFile struct {
	SchemaVersion int           `json:"schema_version"`
	Calibrations  []Calibration `json:"calibrations"`
}

type driftFile struct {
	SchemaVersion int          `json:"schema_version"`
	Entries       []DriftEntry `json:"entries"`
}

// --- Store ---

// Store 管理 data/ 目录布局（设计 §4.1）。
type Store struct {
	root string
}

// New 创建指向 data/ 根目录的 Store（目录不存在时按需创建）。
func New(root string) *Store { return &Store{root: root} }

// DefaultRoot 返回默认数据目录：ONETOKEN_DATA 覆盖，否则 ~/.onetoken/data。
func DefaultRoot() string {
	if v := os.Getenv("ONETOKEN_DATA"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "data"
	}
	return filepath.Join(home, ".onetoken", "data")
}

// Root 返回数据根目录。
func (s *Store) Root() string { return s.root }

// --- 路径 ---

func (s *Store) modelsPath() string       { return filepath.Join(s.root, "models.json") }
func (s *Store) calibrationsPath() string { return filepath.Join(s.root, "calibrations.json") }
func (s *Store) driftPath() string        { return filepath.Join(s.root, "drift.json") }
func (s *Store) fingerprintPath(id string) string {
	return filepath.Join(s.root, "fingerprints", sanitize(id)+".json")
}
func (s *Store) auditPath(id string) string {
	return filepath.Join(s.root, "audits", sanitize(id)+".json")
}
func (s *Store) responsesPath(id string) string {
	return filepath.Join(s.root, "responses", sanitize(id)+".jsonl")
}

// sanitize 将任意 id 规整为跨平台安全文件名（阻止路径穿越与 Windows 非法名）。
// 约定：调用方应使用规范小写 id（macOS/Windows 文件系统大小写不敏感，
// 大小写不同但其余相同的 id 会互相覆盖）；含 '/'、'\\'、':' 与 '_' 混合
// 形态的 id 可能碰撞（如 "a:b" 与 "a_b"），规范命名下风险低，不做碰撞检测。
// SanitizeID 将任意 id 规整为跨平台安全文件名（路径穿越与 Windows 非法名防护）。
// 导出供 enroll 构造幂等续采 id 复用同一规范化（消除双规则碰撞面）。
func SanitizeID(id string) string {
	return sanitize(id)
}

func sanitize(id string) string {
	id = strings.ReplaceAll(id, "/", "__")
	id = strings.ReplaceAll(id, "\\", "__")
	id = strings.ReplaceAll(id, ":", "_")
	// 过滤 Windows 非法字符与 NUL
	var b strings.Builder
	b.Grow(len(id))
	for _, r := range id {
		if r == '\x00' || strings.ContainsRune(`*?"<>|`, r) || unicode.IsControl(r) {

			continue // NUL、文件非法字符与控制字符（换行/ANSI 终端注入防护，审查 L5）
		}
		b.WriteRune(r)
	}
	id = b.String()
	// Windows 保留设备名（带扩展名同样非法）：加安全前缀
	base := id
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if isWinReserved(base) {
		id = "_" + id
	}
	if id == "" || id == "." || id == ".." {
		return "_"
	}
	return id
}

// winReserved 是 Windows 保留设备名（大小写不敏感）。
var winReserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true,
	"com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true,
	"lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

func isWinReserved(base string) bool {
	return winReserved[strings.ToLower(base)]
}

// --- 原子写与读取 ---

// atomicWrite 写临时文件 + fsync + rename（同目录，POSIX 原子），自动创建目录。
// 注：os.Rename 在非 Unix 平台（如 Windows）不保证原子；崩溃窗口内可能短暂丢失目标。
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 失败时清理；成功后 rename 已移走
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// 统一权限：CreateTemp 默认 0600；数据文件应为 0644（便于备份/共享 data/ 目录）
	if err := os.Chmod(path, 0o644); err != nil {
		return err
	}
	// 目录 fsync：保证 rename 目录项持久化（POSIX 断电耐久；失败可忽略）
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// writeJSON 将结构体原子写为 JSON（顶层含 schema_version 的由调用方结构体自带）。
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWrite(path, data)
}

// versioned 读取并校验 schema_version：文件不存在时返回 os.ErrNotExist。
func versioned[T any](path string, get func(v *T) int) (*T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("store: 解析 %s: %w", path, err)
	}
	if ver := get(&v); ver != schemaVersion {
		return nil, fmt.Errorf("store: %s schema_version=%d，期望 %d（数据格式不兼容）", path, ver, schemaVersion)
	}
	return &v, nil
}

// --- 模型目录 ---

// SaveModels 原子写 models.json。
func (s *Store) SaveModels(models []Model) error {
	return writeJSON(s.modelsPath(), modelsFile{SchemaVersion: schemaVersion, Models: models})
}

// LoadModels 读取 models.json（不存在时返回空列表）。
func (s *Store) LoadModels() ([]Model, error) {
	f, err := versioned(s.modelsPath(), func(v *modelsFile) int { return v.SchemaVersion })
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return f.Models, nil
}

// --- 指纹 ---

// SaveFingerprint 原子写指纹文件（复制入参，不修改调用方结构体）。
func (s *Store) SaveFingerprint(fp *Fingerprint) error {
	cp := *fp
	cp.SchemaVersion = schemaVersion
	return writeJSON(s.fingerprintPath(cp.ModelID), &cp)
}

// LoadFingerprint 读取指定模型指纹（不存在时返回 os.ErrNotExist）。
func (s *Store) LoadFingerprint(modelID string) (*Fingerprint, error) {
	return versioned(s.fingerprintPath(modelID), func(v *Fingerprint) int { return v.SchemaVersion })
}

// ListFingerprints 返回已建档模型的真实 id 列表。
// 注意：sanitize 不可逆（model_id 含 '/'），必须读文件内部 model_id 字段，不能从文件名还原。
func (s *Store) ListFingerprints() ([]string, error) {
	dir := filepath.Join(s.root, "fingerprints")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		fp, err := versioned(filepath.Join(dir, e.Name()), func(v *Fingerprint) int { return v.SchemaVersion })
		if err != nil {
			return nil, err
		}
		ids = append(ids, fp.ModelID)
	}
	return ids, nil
}

// --- 审计 ---

// SaveAudit 原子写审计文件（含 pending 预检与最终 verdict 两次写入；复制入参）。
func (s *Store) SaveAudit(a *Audit) error {
	cp := *a
	cp.SchemaVersion = schemaVersion
	return writeJSON(s.auditPath(cp.ID), &cp)
}

// LoadAudit 读取指定审计（不存在时返回 os.ErrNotExist）。
func (s *Store) LoadAudit(auditID string) (*Audit, error) {
	return versioned(s.auditPath(auditID), func(v *Audit) int { return v.SchemaVersion })
}

// ListAudits 返回全部审计 id（读文件内部 id 字段，保持与指纹一致的做法）。
func (s *Store) ListAudits() ([]string, error) {
	dir := filepath.Join(s.root, "audits")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		a, err := versioned(filepath.Join(dir, e.Name()), func(v *Audit) int { return v.SchemaVersion })
		if err != nil {
			return nil, err
		}
		ids = append(ids, a.ID)
	}
	return ids, nil
}

// --- 响应（JSONL 追加，只增不改） ---

// AppendResponse 向 responses/<audit_id>.jsonl 追加一行（O(1)，不重写文件）。
func (s *Store) AppendResponse(auditID string, r *Response) error {
	if r.RawSHA256 == "" {
		return errors.New("store: 响应缺少 raw_sha256（证据链要求）")
	}
	path := s.responsesPath(auditID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

// LoadResponses 读取指定审计的全部响应（按行解析）。
func (s *Store) LoadResponses(auditID string) ([]*Response, error) {
	path := s.responsesPath(auditID)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []*Response
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // 行上限 4MB
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Response
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("store: 解析 %s 第 %d 行: %w", path, lineNo, err)
		}
		out = append(out, &r)
	}
	return out, sc.Err()
}

// ResponseKey 返回幂等去重键（cell + sample_idx）。
func ResponseKey(cell string, sampleIdx int) string {
	return cell + "\x00" + strconv.Itoa(sampleIdx)
}

// LoadResponsesIndex 读取本次审计已完成样本的幂等索引（cell+sample_idx）。
func (s *Store) LoadResponsesIndex(auditID string) (map[string]bool, error) {
	rs, err := s.LoadResponses(auditID)
	if err != nil {
		return nil, err
	}
	idx := make(map[string]bool, len(rs))
	for _, r := range rs {
		idx[ResponseKey(r.Cell, r.SampleIdx)] = true
	}
	return idx, nil
}

// --- 校准 ---

// SaveCalibrations 原子写 calibrations.json。
func (s *Store) SaveCalibrations(cs []Calibration) error {
	return writeJSON(s.calibrationsPath(), calibrationsFile{SchemaVersion: schemaVersion, Calibrations: cs})
}

// LoadCalibrations 读取校准（不存在时返回空列表）。
func (s *Store) LoadCalibrations() ([]Calibration, error) {
	f, err := versioned(s.calibrationsPath(), func(v *calibrationsFile) int { return v.SchemaVersion })
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return f.Calibrations, nil
}

// --- 漂移 ---

// SaveDrift 原子写 drift.json。
func (s *Store) SaveDrift(entries []DriftEntry) error {
	return writeJSON(s.driftPath(), driftFile{SchemaVersion: schemaVersion, Entries: entries})
}

// LoadDrift 读取漂移趋势（不存在时返回空列表）。
func (s *Store) LoadDrift() ([]DriftEntry, error) {
	f, err := versioned(s.driftPath(), func(v *driftFile) int { return v.SchemaVersion })
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return f.Entries, nil
}

// --- 工具 ---

// SHA256Hex 计算证据链哈希。
func SHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
