package store

import (
	"errors"
	"sync"
)

// MemoryStore 是内存响应存储（compare 直比「默认不落库」路径，M2.10）。
//
// 实现 collector.ResponseSink 的幂等索引 + 追加语义，无磁盘 IO：
//   - AppendResponse 与 store.Store 同语义（证据链 raw_sha256 必填）；
//   - LoadResponsesIndex 返回已完成样本（cell+sample_idx）集合（续采去重键）；
//   - Responses 取回全部响应（compare 构建现场指纹用）。
//
// 并发安全（互斥锁保护）。compare 双路（参考/待测）各用独立实例。
type MemoryStore struct {
	mu    sync.Mutex
	items map[string][]*Response
}

// NewMemoryStore 创建空内存响应存储。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{items: make(map[string][]*Response)}
}

// AppendResponse 追加一条响应到内存（与 Store.AppendResponse 的证据链校验一致）。
func (m *MemoryStore) AppendResponse(auditID string, r *Response) error {
	if r == nil {
		return errors.New("store: 响应为 nil")
	}
	if r.RawSHA256 == "" {
		return errors.New("store: 响应缺少 raw_sha256（证据链要求）")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[auditID] = append(m.items[auditID], r)
	return nil
}

// LoadResponsesIndex 返回指定 id 已完成样本的幂等索引（cell+sample_idx）。
func (m *MemoryStore) LoadResponsesIndex(auditID string) (map[string]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := make(map[string]bool, len(m.items[auditID]))
	for _, r := range m.items[auditID] {
		idx[ResponseKey(r.Cell, r.SampleIdx)] = true
	}
	return idx, nil
}

// Responses 返回指定 id 的全部响应（副本；compare 构建现场指纹/取证用）。
func (m *MemoryStore) Responses(auditID string) ([]*Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Response, len(m.items[auditID]))
	copy(out, m.items[auditID])
	return out, nil
}
