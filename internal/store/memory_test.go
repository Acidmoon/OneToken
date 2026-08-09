package store

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
)

// resp 构造带证据链哈希的测试响应。
func memResp(cell string, idx int, raw string) *Response {
	return &Response{Cell: cell, SampleIdx: idx, RawCompletion: raw, RawSHA256: sha256HexForTest(raw)}
}

func TestMemoryStoreAppendAndIndex(t *testing.T) {
	m := NewMemoryStore()
	r1 := memResp("a:en", 0, "x1")
	if err := m.AppendResponse("id1", r1); err != nil {
		t.Fatal(err)
	}
	if err := m.AppendResponse("id1", memResp("a:en", 1, "x2")); err != nil {
		t.Fatal(err)
	}
	if err := m.AppendResponse("id2", memResp("b:en", 0, "y1")); err != nil {
		t.Fatal(err)
	}

	idx, err := m.LoadResponsesIndex("id1")
	if err != nil {
		t.Fatal(err)
	}
	if !idx[ResponseKey("a:en", 0)] || !idx[ResponseKey("a:en", 1)] {
		t.Fatalf("id1 索引缺样本: %v", idx)
	}
	if idx[ResponseKey("b:en", 0)] {
		t.Fatal("id2 样本不应出现在 id1 索引")
	}

	rs, err := m.Responses("id1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Cell != "a:en" || rs[1].SampleIdx != 1 {
		t.Fatalf("Responses 读回异常: %+v", rs)
	}
}

func TestMemoryStoreRequiresChainHash(t *testing.T) {
	m := NewMemoryStore()
	if err := m.AppendResponse("id", &Response{Cell: "a:en", SampleIdx: 0}); err == nil {
		t.Fatal("缺 raw_sha256 应拒绝（证据链要求，与 Store 一致）")
	}
	if err := m.AppendResponse("id", nil); err == nil {
		t.Fatal("nil 响应应拒绝")
	}
}

func TestMemoryStoreConcurrentAppend(t *testing.T) {
	m := NewMemoryStore()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = m.AppendResponse("conc", memResp("c:en", g*50+j, "r"))
			}
		}(i)
	}
	wg.Wait()
	idx, err := m.LoadResponsesIndex("conc")
	if err != nil {
		t.Fatal(err)
	}
	if len(idx) != 400 {
		t.Fatalf("并发追加后索引应有 400 样本，实际 %d", len(idx))
	}
}

// sha256HexForTest 是测试用证据链哈希（避免与 Store 实现耦合测试内部）。
func sha256HexForTest(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
