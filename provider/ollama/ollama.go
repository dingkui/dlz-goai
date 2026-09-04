// Package ollama 面向本地 Ollama 服务（/api/chat、/api/tags）。
//
// Ollama 虽然也提供 OpenAI 兼容端点，但原生接口能拿到生成耗时与 token 用量，
// 且工具参数格式不同（对象而非字符串），因此单独实现。
package ollama

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/dingkui/dlz-goai/internal/wire"
	"github.com/dingkui/dlz-goai/message"
)

// DefaultBaseURL 本地 Ollama 默认监听地址。
const DefaultBaseURL = "http://127.0.0.1:11434"

// Provider 实现 llm.Provider 接口，HTTP 流程全部复用 wire.Client。
// 接口一致性由 llm 包内的编译期断言保证，本包因此不依赖 llm。
type Provider struct {
	*wire.Client
}

// New 构造 Ollama Provider。baseURL 为空时用 DefaultBaseURL。
// Ollama 本地服务通常不需要 API Key，故不提供 key 参数。
func New(baseURL string) *Provider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	return &Provider{Client: wire.NewClient(baseURL, "", codec{})}
}

type codec struct{}

func (codec) Ollama() bool { return true }

func (codec) ChatURLs(baseURL string) []string {
	return []string{strings.TrimRight(baseURL, "/") + "/api/chat"}
}

func (codec) ModelsURL(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/api/tags"
}

func (codec) EncodeMessages(messages []message.Message) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		item := map[string]any{"role": m.Role}
		// Ollama：图片走消息级 images 数组，文本仍是字符串
		item["content"] = m.Content
		if len(m.Images) > 0 {
			images := make([]string, len(m.Images))
			for i, image := range m.Images {
				images[i] = ollamaImagePayload(image)
			}
			item["images"] = images
		}
		wire.ApplyMessageToolFields(item, m, true)
		out = append(out, item)
	}
	return out
}

// ollamaImagePayload 把浏览器常用的 Data URI 转成 Ollama 原生 API
// 所需的纯 base64；已经是纯 base64（或其他调用方自定义值）时保持不变。
func ollamaImagePayload(image string) string {
	comma := strings.IndexByte(image, ',')
	if comma < 0 {
		return image
	}
	header := strings.ToLower(strings.TrimSpace(image[:comma]))
	if strings.HasPrefix(header, "data:") && strings.HasSuffix(header, ";base64") {
		return image[comma+1:]
	}
	return image
}

func (codec) ApplyOptions(body map[string]any, opts *message.Options) {
	options := map[string]any{}
	if opts.Temperature != nil {
		options["temperature"] = *opts.Temperature
	}
	if opts.NumCtx != nil {
		options["num_ctx"] = *opts.NumCtx
	}
	if opts.MaxTokens > 0 {
		options["num_predict"] = opts.MaxTokens
	}
	if len(options) > 0 {
		body["options"] = options
	}
}

func (codec) DecodeModels(r io.Reader) ([]string, error) {
	var decoded struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(r).Decode(&decoded); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(decoded.Models))
	for _, model := range decoded.Models {
		out = append(out, model.Name)
	}
	return out, nil
}
