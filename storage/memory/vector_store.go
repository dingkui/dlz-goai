// Package memory 提供 rag 接口的内存实现。
// 适合开发调试与中小规模知识库；数据随进程退出而消失，
// 持久化需求请实现自己的 VectorStore 或等待 storage/sqlite。
package memory

import (
	"context"
	"sort"
	"sync"

	"github.com/dingkui/dlz-goai/rag"
)

var _ rag.VectorStore = (*VectorStore)(nil)

type entry struct {
	docID  string
	chunk  rag.Chunk
	vector []float32
}

// VectorStore 内存向量存储：暴力余弦搜索。
// 简单但不依赖任何外部服务，适合中小规模（万级块以内）与测试。
type VectorStore struct {
	mu      sync.RWMutex
	entries []entry
}

// NewVectorStore 构造空存储。
func NewVectorStore() *VectorStore { return &VectorStore{} }

// Store 存入文档块与向量。同 docID 覆盖旧数据。
func (s *VectorStore) Store(_ context.Context, docID string, chunks []rag.Chunk, vectors [][]float32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 删除旧块
	kept := s.entries[:0]
	for _, e := range s.entries {
		if e.docID != docID {
			kept = append(kept, e)
		}
	}
	s.entries = kept
	// 追加新块
	for i := range chunks {
		if i >= len(vectors) {
			break
		}
		s.entries = append(s.entries, entry{
			docID:  docID,
			chunk:  chunks[i],
			vector: append([]float32(nil), vectors[i]...),
		})
	}
	return nil
}

// Remove 删除文档的全部块。
func (s *VectorStore) Remove(_ context.Context, docID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.entries[:0]
	for _, e := range s.entries {
		if e.docID != docID {
			kept = append(kept, e)
		}
	}
	s.entries = kept
	return nil
}

// Search 按余弦相似度检索 topK。
func (s *VectorStore) Search(_ context.Context, query []float32, topK int, filter rag.Filter) ([]rag.SearchResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if topK <= 0 {
		topK = 5
	}
	var hits []rag.SearchResult
	for _, e := range s.entries {
		if filter != nil && !filter(e.chunk) {
			continue
		}
		hits = append(hits, rag.SearchResult{Chunk: e.chunk, Score: rag.Cosine(query, e.vector)})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Chunk.Seq < hits[j].Chunk.Seq
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}

// Size 返回当前块数（测试与监控用）。
func (s *VectorStore) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}
