package rag

import "context"

// VectorStore 向量存储：把文档分块向量存入并按相似度检索。
//
// 实现可以是内存暴力搜索（storage/memory）、SQLite + 余弦、
// 或专用向量库。本接口不规定存储格式，只规定读写契约。
type VectorStore interface {
	// Store 存入一篇文档的块与对应向量。
	// chunks 与 vectors 等长、一一对应；同 DocID 覆盖旧数据。
	Store(ctx context.Context, docID string, chunks []Chunk, vectors [][]float32) error
	// Remove 删除一篇文档的全部块。
	Remove(ctx context.Context, docID string) error
	// Search 按向量检索 topK。filter 为 nil 表示不限制；
	// 返回结果按 Score 降序。
	Search(ctx context.Context, query []float32, topK int, filter Filter) ([]SearchResult, error)
}

// Filter 命中过滤：按 DocID、Section、Metadata 等业务条件裁剪。
// 返回 true 表示纳入候选。为 nil 时不过滤。
type Filter func(Chunk) bool

// FullTextStore 全文检索存储（可选）：BM25 等关键词检索。
// 与 VectorStore 互补，混合检索（Hybrid）时两者结果经 RRF 融合。
// 第一版不强制实现，需要全文检索的应用自行接入。
type FullTextStore interface {
	Index(ctx context.Context, docID string, chunks []Chunk) error
	Remove(ctx context.Context, docID string) error
	Search(ctx context.Context, query string, topK int, filter Filter) ([]SearchResult, error)
}
