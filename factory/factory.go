// Package factory 按服务配置构造内置的 llm.Provider 实现。
//
// 本包是可选的：只需要 llm 接口 + 自定义 Provider 的应用不必引入它，
// 这也是 llm 包不直接 import provider 的原因——核心接口不被内置实现牵连。
package factory

import (
	"github.com/dingkui/dlz-goai/llm"
	"github.com/dingkui/dlz-goai/provider/ollama"
	"github.com/dingkui/dlz-goai/provider/openai"
)

// NewProvider 按配置构造对应的 Provider 实现。
// Kind 为 ollama 时走原生接口（能拿到 token 用量与耗时），其余按 OpenAI 兼容处理。
func NewProvider(svc llm.Service) llm.Provider {
	if svc.Kind == llm.KindOllama {
		return ollama.New(svc.BaseURL)
	}
	return openai.New(svc.BaseURL, svc.APIKey)
}
