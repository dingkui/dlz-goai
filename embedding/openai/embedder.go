// Package openai 面向 OpenAI 兼容 embeddings 端点（/v1/embeddings）。
//
// 适用于 OpenAI 自身、通义、智谱、本地 vLLM 等任何兼容服务。
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Embedder 实现 rag.Embedder。
type Embedder struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client
}

// New 构造。baseURL 形如 https://api.openai.com/v1。
func New(baseURL, apiKey, model string) *Embedder {
	return &Embedder{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
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
		return nil, fmt.Errorf("openai: 未返回向量")
	}
	return vecs[0], nil
}

// EmbedBatch 批量。OpenAI 要求 input 是字符串数组；
// 响应 data 按 index 对齐，本实现按 index 排序还原输入顺序。
func (e *Embedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, _ := json.Marshal(map[string]any{"model": e.Model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	resp, err := e.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai: 连接失败: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("openai: 解析响应失败: %w", err)
	}
	// 按 index 排序还原输入顺序（服务端可能乱序返回）
	sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].Index < out.Data[j].Index })
	vecs := make([][]float32, 0, len(out.Data))
	for _, item := range out.Data {
		vecs = append(vecs, item.Embedding)
	}
	return vecs, nil
}
