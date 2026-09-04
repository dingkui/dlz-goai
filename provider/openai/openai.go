// Package openai 面向任何 OpenAI 兼容接口：OpenAI 自身、DeepSeek、通义、
// 月之暗面、本地 vLLM / llama.cpp / Ollama 的 /v1 端点等。
//
// 只要服务端接受 POST {base}/v1/chat/completions 的 SSE 流，本包即可工作。
package openai

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/dingkui/dlz-goai/internal/wire"
	"github.com/dingkui/dlz-goai/message"
)

// Provider 实现 llm.Provider 接口，HTTP 流程全部复用 wire.Client。
// 接口一致性由 llm 包内的编译期断言保证，本包因此不依赖 llm。
type Provider struct {
	*wire.Client
}

// New 构造 OpenAI 兼容服务的 Provider。
// baseURL 常见写法：https://api.openai.com/v1、https://api.deepseek.com。
// apiKey 为空时不发送 Authorization 头。
func New(baseURL, apiKey string) *Provider {
	return &Provider{Client: wire.NewClient(baseURL, apiKey, codec{})}
}

type codec struct{}

func (codec) Ollama() bool { return false }

func (codec) ChatURLs(baseURL string) []string {
	base := strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return []string{base}
	}
	if strings.HasSuffix(base, "/v1") {
		return []string{base + "/chat/completions"}
	}
	// 未带版本前缀时两种都试，兼容把端点直接暴露在根路径的服务
	return []string{base + "/v1/chat/completions", base + "/chat/completions"}
}

func (codec) ModelsURL(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/models"
}

func (codec) EncodeMessages(messages []message.Message) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		item := map[string]any{"role": m.Role}
		if len(m.Images) > 0 {
			// 多模态：content 是块数组，图文混排
			content := make([]map[string]any, 0, len(m.Images)+1)
			if m.Content != "" {
				content = append(content, map[string]any{"type": "text", "text": m.Content})
			}
			for _, img := range m.Images {
				content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": img}})
			}
			if len(content) == 0 {
				content = []map[string]any{{"type": "text", "text": " "}}
			}
			item["content"] = content
		} else {
			item["content"] = m.Content
		}
		wire.ApplyMessageToolFields(item, m, false)
		out = append(out, item)
	}
	return out
}

func (codec) ApplyOptions(body map[string]any, opts *message.Options) {
	if opts.Temperature != nil {
		body["temperature"] = *opts.Temperature
	}
	if opts.MaxTokens > 0 {
		body["max_tokens"] = opts.MaxTokens
	}
}

func (codec) DecodeModels(r io.Reader) ([]string, error) {
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r).Decode(&decoded); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(decoded.Data))
	for _, model := range decoded.Data {
		out = append(out, model.ID)
	}
	return out, nil
}
