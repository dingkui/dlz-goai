package rag

import (
	"context"
	"fmt"
)

// Pipeline 一条完整的向量检索管线：query → embed → search → rerank。
// 使用向量 Store 时 Embedder 必填；纯全文检索不需要 Embedder；
// Reranker 可选；Store 与 FullTextStore 至少一个。
type Pipeline struct {
	Embedder Embedder
	Store    VectorStore
	FullText FullTextStore // 可选：开启混合检索时使用
	Reranker Reranker      // 可选
	// DefaultTopK RetrieveOptions.TopK 为 0 时的默认值。
	DefaultTopK int
}

var _ Retriever = Pipeline{}

// Retrieve 实现 Retriever。
func (p Pipeline) Retrieve(ctx context.Context, query string, opts RetrieveOptions) ([]SearchResult, error) {
	if p.Store == nil && p.FullText == nil {
		return nil, fmt.Errorf("rag: pipeline 未配置任何 Store")
	}
	if p.Store != nil && p.Embedder == nil {
		return nil, fmt.Errorf("rag: 向量检索未配置 Embedder")
	}
	topK := opts.TopK
	if topK <= 0 {
		topK = p.DefaultTopK
	}
	if topK <= 0 {
		topK = 5
	}
	filter := opts.Filter

	var rankings [][]SearchResult
	if p.Store != nil {
		vec, err := p.Embedder.Embed(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("rag: 查询向量化失败: %w", err)
		}
		hits, err := p.Store.Search(ctx, vec, topK, filter)
		if err != nil {
			return nil, fmt.Errorf("rag: 向量检索失败: %w", err)
		}
		rankings = append(rankings, hits)
	}
	if p.FullText != nil {
		hits, err := p.FullText.Search(ctx, query, topK, filter)
		if err != nil {
			return nil, fmt.Errorf("rag: 全文检索失败: %w", err)
		}
		rankings = append(rankings, hits)
	}

	var merged []SearchResult
	if len(rankings) == 1 {
		merged = rankings[0]
	} else {
		merged = RRF(rankings, 60)
		if len(merged) > topK {
			merged = merged[:topK]
		}
	}

	if p.Reranker != nil && len(merged) > 1 {
		reranked, err := p.Reranker.Rerank(ctx, query, merged)
		if err != nil {
			return nil, fmt.Errorf("rag: 重排失败: %w", err)
		}
		merged = reranked
	}
	return merged, nil
}
