package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/dingkui/dlz-goai/tool"
)

// NewTool 把 Retriever 包装成 tool.Tool，让 agent 能调用知识库检索。
//
// 工具参数：{ "query": string, "top_k": int (可选) }。
// 返回命中块的 JSON（content + 来源 + 分数），并在 Result.Citations
// 里附带引用——agent 会自动聚合到最终回答。
func NewTool(name, description string, retriever Retriever, defaultTopK int) tool.Tool {
	if defaultTopK <= 0 {
		defaultTopK = 5
	}
	parameters := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":  map[string]any{"type": "string", "description": "检索查询"},
			"top_k":  map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "default": defaultTopK},
		},
		"required": []string{"query"},
	}
	return tool.NewFunc(name, description, parameters, true,
		func(ctx context.Context, args map[string]any) (tool.Result, error) {
			query, _ := args["query"].(string)
			if query == "" {
				return tool.Error("query 不能为空"), nil
			}
			topK := defaultTopK
			if raw, ok := args["top_k"]; ok {
				switch v := raw.(type) {
				case float64:
					topK = int(v)
				case int:
					topK = v
				case string:
					if n, err := strconv.Atoi(v); err == nil {
						topK = n
					}
				}
			}
			hits, err := retriever.Retrieve(ctx, query, RetrieveOptions{TopK: topK})
			if err != nil {
				return tool.Result{}, err
			}
			payload := map[string]any{
				"query":   query,
				"results": hits,
				"citation_instruction": "在最终回答中用 Markdown 链接引用实际使用的来源。",
			}
			encoded, err := json.MarshalIndent(payload, "", "  ")
			if err != nil {
				return tool.Result{}, err
			}
			citations := make([]tool.Citation, 0, len(hits))
			for _, hit := range hits {
				if hit.Citation != nil {
					citations = append(citations, *hit.Citation)
				}
			}
			return tool.Result{Content: string(encoded), Citations: citations}, nil
		})
}

// FormatHits 把命中结果格式化为模型易读的纯文本（无 JSON 噪声）。
// 当不想用 NewTool 的默认 JSON 输出时，可用它自定义工具。
func FormatHits(query string, hits []SearchResult) string {
	if len(hits) == 0 {
		return fmt.Sprintf("query=%q 无命中", query)
	}
	out := fmt.Sprintf("query=%q, 命中 %d 条：\n", query, len(hits))
	for i, hit := range hits {
		out += fmt.Sprintf("[%d] score=%.3f %s\n%s\n\n", i+1, hit.Score, hit.Chunk.Section, hit.Chunk.Content)
	}
	return out
}
