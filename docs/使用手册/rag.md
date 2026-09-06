# 使用手册：rag — 检索增强

`rag` 定义检索增强的基础抽象：文档分块、向量化、向量/全文检索、重排、检索管线，以及把检索器包装成 agent 工具。只依赖 `tool`（复用 Citation）——可以独立构建知识库检索，也可以包成工具接入 agent。

## 概念链

```
Document（原始文档）
  → Splitter 切分 → []Chunk（索引与检索单元：DocID/Section/Seq/Content/起止行）
  → Embedder 向量化 → [][]float32
  → VectorStore 存储
查询：query → Embedder → Store.Search →（可选 Reranker 重排）→ []SearchResult
```

## 建库：切分 + 向量化 + 存储

```go
// 1. 切分：按 Markdown 标题分段，超长段按字符切（不切断多字节字符）
splitter := rag.TextSplitter{ChunkSize: 900} // 0 用默认 900 rune
chunks := splitter.Split(doc.Content)
for i := range chunks {
	chunks[i].DocID = doc.ID
}

// 2. 向量化（双厂商实现）
embedder := ollamaembed.New("http://127.0.0.1:11434", "nomic-embed-text")
// embedder := openaiembed.New("https://api.openai.com/v1", "sk-...", "text-embedding-3-small")
texts := make([]string, len(chunks))
for i, c := range chunks {
	texts[i] = c.Content
}
vecs, err := embedder.EmbedBatch(ctx, texts) // 顺序与输入一一对应

// 3. 存储（同 DocID 全量覆盖）
store := memstore.NewVectorStore() // 开发测试用，暴力余弦
err = store.Store(ctx, doc.ID, chunks, vecs)
```

删除整篇文档：`store.Remove(ctx, docID)`。

## 检索：Pipeline

```go
pipeline := rag.Pipeline{
	Embedder:    embedder,          // 必填（配置了向量通道时）
	Store:       store,             // 向量通道
	// FullText: ftStore,          // 可选全文通道（混合检索）
	// Reranker: reranker,         // 可选重排
	DefaultTopK: 5,
}
hits, err := pipeline.Retrieve(ctx, "如何配置鉴权", rag.RetrieveOptions{
	TopK:   8,
	Filter: func(c rag.Chunk) bool { return c.DocID != "excluded" }, // 可选过滤
})
```

- 双通道（Store + FullText）结果经 **RRF 融合**（k=60，双通道命中的文档天然靠前），单通道直接返回；
- `Reranker` 存在且命中数 > 1 时对融合结果二次排序；**注意：重排失败会返回错误**（fail-fast），对可用性敏感的场景请自行包一层降级；
- `FullTextStore` 接口已定义但**暂无内置实现**——需要全文检索的应用可接 SQLite FTS5 等自行实现。

## 接入 agent：rag.NewTool

把检索器包成只读工具，模型即可自主检索：

```go
kbTool := rag.NewTool("kb_search", "搜索产品知识库", pipeline, 5)

result, _ := agent.New().Run(ctx, msgs, nil,
	agent.Config{Tools: []tool.Tool{kbTool}}, callModel, nil)
// 最终回答的 result.Citations 自动携带命中文档引用（来自 SearchResult.Citation）
```

工具参数 `{"query": string, "top_k": int?}`，返回命中块的 JSON（content + 来源 + 分数）。偏好纯文本输出时用 `rag.FormatHits(query, hits)` 自定义工具体。

`SearchResult.Citation` 由调用方在建库或检索后填充（`DocID`/`RelPath`/`Section` 等），填充后引用链路全自动：工具结果 → agent 汇总 → 最终回答。

## 通用算法（零依赖复用）

```go
score := rag.Cosine(a, b)              // 余弦相似度（float64 累积防溢出）
blob := rag.EncodeVec(vec)             // 向量 → 二进制（LittleEndian float32，落库用）
vec := rag.DecodeVec(blob)             // 二进制 → 向量
```

## 自定义扩展点

| 接口 | 语义 | 内置实现 |
|---|---|---|
| `Splitter` | 切分策略（`Split(text) []Chunk`） | `TextSplitter`（Markdown 标题） |
| `Embedder` | 文本 → 向量（`Embed`/`EmbedBatch`） | `embedding/ollama`、`embedding/openai` |
| `VectorStore` | 存储 + 相似度检索（同 DocID 覆盖、Score 降序） | `storage/memory`、`storage/sqlite` |
| `FullTextStore` | 关键词检索通道 | 暂无（可接 SQLite FTS5） |
| `Retriever` | 查询 → 命中（Pipeline 实现了它） | 自定义混合策略 |
| `Reranker` | 二次排序 | 暂无（应用按需求实现） |

已知边界：向量检索为暴力余弦，无 ANN 索引——性能取决于向量维度、条数和并发量，需按实际数据测试；必要时接专用向量库实现 VectorStore。切换 Embedder 模型时注意维度匹配（旧向量需重建）。
