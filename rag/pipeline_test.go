package rag_test

import (
	"context"
	"strings"
	"testing"

	"github.com/dingkui/dlz-goai/rag"
	"github.com/dingkui/dlz-goai/storage/memory"
)

// stubEmbedder 返回与输入文本绑定的确定性向量，便于测试控制相似度。
type stubEmbedder struct{}

type stubFullTextStore struct {
	hits []rag.SearchResult
}

func (s stubFullTextStore) Index(context.Context, string, []rag.Chunk) error { return nil }
func (s stubFullTextStore) Remove(context.Context, string) error             { return nil }
func (s stubFullTextStore) Search(_ context.Context, _ string, topK int, filter rag.Filter) ([]rag.SearchResult, error) {
	var hits []rag.SearchResult
	for _, hit := range s.hits {
		if filter == nil || filter(hit.Chunk) {
			hits = append(hits, hit)
		}
		if len(hits) == topK {
			break
		}
	}
	return hits, nil
}

func (stubEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	return textVec(text), nil
}

func (stubEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i] = textVec(text)
	}
	return out, nil
}

// textVec 把文本映射到 3 维空间，相同文本完全相同、相近文本部分重叠。
func textVec(text string) []float32 {
	v := []float32{0, 0, 0}
	for _, r := range text {
		switch {
		case strings.ContainsRune("Go编程语言并发", r):
			v[0]++
		case strings.ContainsRune("数据库查询SQL", r):
			v[1]++
		case strings.ContainsRune("文档知识检索", r):
			v[2]++
		}
	}
	return v
}

func TestPipelineEndToEnd(t *testing.T) {
	store := memory.NewVectorStore()
	ctx := context.Background()

	// 建库：三块不同主题
	docs := []struct {
		docID, section, content string
		vec                     []float32
	}{
		{"d1", "Go", "Go 编程语言与并发", []float32{5, 0, 0}},
		{"d2", "DB", "数据库查询与 SQL", []float32{0, 4, 0}},
		{"d3", "RAG", "文档知识与检索", []float32{0, 0, 4}},
	}
	for _, d := range docs {
		_ = store.Store(ctx, d.docID, []rag.Chunk{{DocID: d.docID, Section: d.section, Content: d.content}}, [][]float32{d.vec})
	}

	pipe := rag.Pipeline{Embedder: stubEmbedder{}, Store: store, DefaultTopK: 3}

	// 查询 Go 相关
	hits, err := pipe.Retrieve(ctx, "Go 并发编程", rag.RetrieveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("应有命中")
	}
	if hits[0].Chunk.Section != "Go" {
		t.Fatalf("最相关应为 Go 块, got %+v", hits[0])
	}
}

func TestPipelineFullTextOnlyDoesNotRequireEmbedder(t *testing.T) {
	want := rag.SearchResult{Chunk: rag.Chunk{DocID: "d1", Content: "全文检索命中"}, Score: 1}
	pipe := rag.Pipeline{FullText: stubFullTextStore{hits: []rag.SearchResult{want}}}
	hits, err := pipe.Retrieve(context.Background(), "检索", rag.RetrieveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Chunk.DocID != want.Chunk.DocID {
		t.Fatalf("纯 FTS Pipeline 返回不符: %+v", hits)
	}
}

func TestPipelineFilter(t *testing.T) {
	store := memory.NewVectorStore()
	ctx := context.Background()
	_ = store.Store(ctx, "d1", []rag.Chunk{{DocID: "d1", Section: "A", Content: "Go", Seq: 0}}, [][]float32{{1, 0, 0}})
	_ = store.Store(ctx, "d2", []rag.Chunk{{DocID: "d2", Section: "B", Content: "Go", Seq: 0}}, [][]float32{{1, 0, 0}})

	pipe := rag.Pipeline{Embedder: stubEmbedder{}, Store: store}
	// 只在 d1 范围检索
	hits, err := pipe.Retrieve(ctx, "Go", rag.RetrieveOptions{
		Filter: func(c rag.Chunk) bool { return c.DocID == "d1" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Chunk.DocID != "d1" {
		t.Fatalf("过滤应只返回 d1: %+v", hits)
	}
}

func TestRRFFusion(t *testing.T) {
	// 两路检索，d1 在第一路排第1、第二路排第2；d2 相反
	rank1 := []rag.SearchResult{
		{Chunk: rag.Chunk{DocID: "d1", Seq: 0}, Score: 0.9},
		{Chunk: rag.Chunk{DocID: "d2", Seq: 0}, Score: 0.5},
	}
	rank2 := []rag.SearchResult{
		{Chunk: rag.Chunk{DocID: "d2", Seq: 0}, Score: 0.8},
		{Chunk: rag.Chunk{DocID: "d1", Seq: 0}, Score: 0.4},
	}
	merged := rag.RRF([][]rag.SearchResult{rank1, rank2}, 60)
	if len(merged) != 2 {
		t.Fatalf("融合后应 2 条, got %d", len(merged))
	}
	// d1: 1/61 + 1/62, d2: 1/62 + 1/61，两者相等，靠 tie-break
	// 改成不等排名验证排序：
	rank1[0].Chunk.DocID = "d1"
	rank2[0].Chunk.DocID = "d1" // d1 在两路都第一
	merged = rag.RRF([][]rag.SearchResult{rank1, rank2}, 60)
	if merged[0].Chunk.DocID != "d1" {
		t.Fatalf("双路第一的应排首位, got %+v", merged[0])
	}
	if merged[1].Chunk.DocID != "d2" {
		t.Fatalf("第二应为 d2: %+v", merged[1])
	}
	if merged[0].Score <= merged[1].Score {
		t.Fatalf("分数应降序: %v vs %v", merged[0].Score, merged[1].Score)
	}
}

func TestNewToolWrapsRetriever(t *testing.T) {
	store := memory.NewVectorStore()
	ctx := context.Background()
	_ = store.Store(ctx, "d1", []rag.Chunk{{DocID: "d1", Section: "Go", Content: "Go 并发", Seq: 0}}, [][]float32{{5, 0, 0}})

	pipe := rag.Pipeline{Embedder: stubEmbedder{}, Store: store}
	tool := rag.NewTool("kb_search", "搜索知识库", pipe, 3)
	def := tool.Definition()
	if def.Name != "kb_search" {
		t.Fatalf("工具名不符: %s", def.Name)
	}
	// 执行工具
	result, err := tool.Execute(ctx, map[string]any{"query": "Go 并发"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "Go 并发") {
		t.Fatalf("结果应含命中内容: %s", result.Content)
	}
}
