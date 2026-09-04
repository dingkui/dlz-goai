package rag

import "context"

// Retriever 检索器：把查询文本变成命中结果。
// 实现可以是纯向量、纯全文、或混合；调用方不关心来源。
type Retriever interface {
	Retrieve(ctx context.Context, query string, opts RetrieveOptions) ([]SearchResult, error)
}

// RetrieveOptions 检索参数。零值时 TopK 用默认（实现决定）。
type RetrieveOptions struct {
	TopK   int
	Filter Filter
}

// Reranker 重排器：对初步检索结果二次排序。
// 典型实现用 LLM 对 query-doc 相关性打分（cross-encoder 风格）。
type Reranker interface {
	Rerank(ctx context.Context, query string, results []SearchResult) ([]SearchResult, error)
}
