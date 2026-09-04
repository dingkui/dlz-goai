// Package ollama 面向本地 Ollama 服务的 Embedder（/api/embed）。
//
// Ollama 的 embed 端点同时支持单条（input string）与批量（input []string），
// 响应字段在新旧版本间有差异（embedding vs embeddings），本实现两者都处理。
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL 本地 Ollama 默认地址。
const DefaultBaseURL = "http://127.0.0.1:11434"

// Embedder 实现 rag.Embedder。
type Embedder struct {
	BaseURL string
	Model   string
	HTTP    *http.Client
}

// New 构造。baseURL 为空用默认。
func New(baseURL, model string) *Embedder {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	return &Embedder{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Model:   model,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
	}
}

func (e *Embedder) client() *http.Client {
	if e.HTTP != nil {
		return e.HTTP
	}
	return http.DefaultClient
}

// Embed 单条。
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := e.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("ollama: 未返回向量")
	}
	return vecs[0], nil
}

// EmbedBatch 批量。Ollama /api/embed 接受 input 数组。
func (e *Embedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, _ := json.Marshal(map[string]any{"model": e.Model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: 连接失败: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ollama: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	// 兼容新旧字段：embeddings（批量）优先，embedding（单条）兜底
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
		Embedding  []float32   `json:"embedding"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("ollama: 解析响应失败: %w", err)
	}
	if len(out.Embeddings) > 0 {
		return out.Embeddings, nil
	}
	if len(out.Embedding) > 0 {
		return [][]float32{out.Embedding}, nil
	}
	return nil, fmt.Errorf("ollama: 响应不含向量")
}
