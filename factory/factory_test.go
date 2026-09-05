package factory

import (
	"testing"

	"github.com/dingkui/dlz-goai/llm"
	ollamaProvider "github.com/dingkui/dlz-goai/provider/ollama"
	openaiProvider "github.com/dingkui/dlz-goai/provider/openai"
)

func TestNewProviderByKind(t *testing.T) {
	o := NewProvider(llm.Service{Kind: llm.KindOllama, BaseURL: "http://127.0.0.1:11434"})
	if _, ok := o.(*ollamaProvider.Provider); !ok {
		t.Fatalf("ollama kind 应构造 ollama Provider, got %T", o)
	}

	// openai kind 与未知 kind 都按 OpenAI 兼容处理
	for _, kind := range []string{llm.KindOpenAI, "deepseek", ""} {
		p := NewProvider(llm.Service{Kind: kind, BaseURL: "https://api.example.com", APIKey: "k"})
		if _, ok := p.(*openaiProvider.Provider); !ok {
			t.Fatalf("kind %q 应构造 openai Provider, got %T", kind, p)
		}
	}
}
