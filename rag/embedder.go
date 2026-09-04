package rag

import "context"

// Embedder 文本转向量的抽象。
//
// 单条与批量分开：批量在支持 batch 的服务端（OpenAI embeddings、
// Ollama /api/embed）能显著降低往返开销；不支持时实现方自行循环。
type Embedder interface {
	// Embed 单条文本转向量。
	Embed(ctx context.Context, text string) ([]float32, error)
	// EmbedBatch 批量转向量，顺序与输入一致。
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}

// Dim 向量的维度。Embedder 实现应保证同一实例输出维度稳定；
// VectorStore 通常按维度建索引，维度漂移会让旧向量失效。
type Dim = int
