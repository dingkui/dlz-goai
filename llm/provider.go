// Package llm 定义与厂商无关的模型能力接口。
//
// 本包只依赖 message 与 tool，不 import 任何具体实现——
// 内置实现的按需构造在可选的 factory 包，自定义实现只需实现 Provider 接口。
package llm

import (
	"context"

	"github.com/dingkui/dlz-goai/message"
)

// Provider 统一模型能力接口（与后端协议无关）。
type Provider interface {
	// ChatStream 流式对话。delta 为空且 Done 表示结束，Error 表示中断。
	// 模型名由调用方指定；实现不得自行猜测。
	ChatStream(ctx context.Context, model string, messages []message.Message, opts *message.Options, cb func(message.Delta)) error
	// ListModels 拉取服务可用模型名。
	ListModels(ctx context.Context) ([]string, error)
	// Ping 探测连通性。
	Ping(ctx context.Context) error
}

// 服务类型。openai 适用于任何 OpenAI 兼容接口，ollama 为本地 Ollama 原生接口。
const (
	KindOpenAI = "openai"
	KindOllama = "ollama"
)

// Service 一个模型服务配置（多服务多模型）。
type Service struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	BaseURL      string   `json:"baseURL"`
	APIKey       string   `json:"apiKey,omitempty"`
	Models       []string `json:"models"`
	DefaultModel string   `json:"defaultModel"`
	Enabled      bool     `json:"enabled"`
}
