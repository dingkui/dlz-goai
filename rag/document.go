// Package rag 提供检索增强生成的基础抽象：文档分块、向量化、
// 向量与全文检索、重排、检索管线，以及把检索器包装成 agent 工具。
//
// 本包只定义接口与通用算法；具体 Embedder 实现在 embedding/*，
// 存储实现在 storage/*。rag 可独立使用（构建知识库检索），
// 也可经 rag.NewTool 包成 tool.Tool 接入 agent。
//
// 依赖方向：rag → tool（Citation 复用）；不依赖 agent、llm、mcp。
package rag

import "github.com/dingkui/dlz-goai/tool"

// Citation 是 tool.Citation 的别名，引用定义在 tool 包以维持依赖单向。
type Citation = tool.Citation

// Document 一篇待索引的原始文档。
// ID 在同一 VectorStore 内唯一；Source 是人类可读来源（文件路径、URL 等）。
type Document struct {
	ID       string         `json:"id"`
	Source   string         `json:"source,omitempty"`
	Content  string         `json:"content"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Chunk 一篇文档切分后的一块，是索引与检索单元。
// 字段与知识库场景对齐：Section 标题、Seq 块序、起止行号。
type Chunk struct {
	DocID     string `json:"docId"`
	Section   string `json:"section,omitempty"`
	Seq       int    `json:"seq"`
	Content   string `json:"content"`
	StartLine int    `json:"startLine,omitempty"`
	EndLine   int    `json:"endLine,omitempty"`
}

// SearchResult 一次检索的命中。
// Citation 把命中转成引用（知识库场景用 DocID/RelPath），由调用方按需填充。
type SearchResult struct {
	Chunk    Chunk     `json:"chunk"`
	Score    float32   `json:"score"`
	Citation *Citation `json:"citation,omitempty"`
}
