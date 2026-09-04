// Package integration 提供跨包的端到端测试：全部依赖由 httptest 模拟，
// 不访问真实网络，可在 CI 中稳定运行。
package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/mcp"
	"github.com/dingkui/dlz-goai/provider/ollama"
	"github.com/dingkui/dlz-goai/provider/openai"
	"github.com/dingkui/dlz-goai/tool"
)

// --- OpenAI 兼容服务端模拟 ---

func fakeOpenAI(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body map[string]any)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		handler(w, r, body)
	}))
}

// 回归：OpenAI 兼容流式的文本分片、finish_reason、请求体里的 tools。
func TestOpenAIChatStreamAndTools(t *testing.T) {
	var gotTools any
	srv := fakeOpenAI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		gotTools = body["tools"]
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	})
	defer srv.Close()

	provider := openai.New(srv.URL, "sk-test")
	temperature := 0.7
	var deltas []message.Delta
	err := provider.ChatStream(context.Background(), "gpt-test",
		[]message.Message{{Role: message.RoleUser, Content: "hi"}},
		&message.Options{Temperature: &temperature},
		func(d message.Delta) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if deltas[0].Content != "你" || deltas[1].Content != "好" || deltas[2].FinishReason != "stop" {
		t.Fatalf("流式分片不符: %+v", deltas)
	}
	if gotTools != nil {
		t.Fatalf("未传工具时请求体不应包含 tools: %v", gotTools)
	}

	// 带 tools 的请求
	tools := []tool.Tool{tool.NewFunc("t1", "测试工具", nil, true,
		func(context.Context, map[string]any) (tool.Result, error) { return tool.Text("ok"), nil })}
	opts := &message.Options{Tools: []tool.Definition{tools[0].Definition()}}
	err = provider.ChatStream(context.Background(), "gpt-test",
		[]message.Message{{Role: message.RoleUser, Content: "hi"}}, opts, func(message.Delta) {})
	if err != nil {
		t.Fatal(err)
	}
	if gotTools == nil {
		t.Fatal("传了工具但请求体没有 tools 字段")
	}
}

// 回归：Ollama 原生接口的 NDJSON 流与用量统计。
func TestOllamaChatStreamWithUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/chat") {
			w.Write([]byte(`{"message":{"content":"答"},"done":false}` + "\n"))
			w.Write([]byte(`{"message":{"content":""},"done":true,"prompt_eval_count":9,"eval_count":21,"eval_duration":1500000,"total_duration":800000000}` + "\n"))
			return
		}
		// /api/tags
		w.Write([]byte(`{"models":[{"name":"qwen3:8b"}]}`))
	}))
	defer srv.Close()

	provider := ollama.New(srv.URL)
	var deltas []message.Delta
	err := provider.ChatStream(context.Background(), "qwen3:8b",
		[]message.Message{{Role: message.RoleUser, Content: "hi"}}, nil,
		func(d message.Delta) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if deltas[0].Content != "答" {
		t.Fatalf("文本不符: %+v", deltas[0])
	}
	last := deltas[len(deltas)-1]
	if last.PromptTokens != 9 || last.EvalTokens != 21 || last.EvalMs != 1 {
		t.Fatalf("用量不符: %+v", last)
	}
	models, err := provider.ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0] != "qwen3:8b" {
		t.Fatalf("模型列表不符: %v err=%v", models, err)
	}
}

// --- MCP 服务端模拟（Streamable HTTP + 会话语义）---

type fakeMCPServer struct {
	mu     sync.Mutex
	active string
	inits  int
}

func (s *fakeMCPServer) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	reply := func(result map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
	if req.Method == "initialize" {
		s.mu.Lock()
		s.inits++
		s.active = "sess-" + strconv.Itoa(s.inits)
		sid := s.active
		s.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", sid)
		reply(map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "fake", "version": "1"},
		})
		return
	}
	s.mu.Lock()
	valid := r.Header.Get("Mcp-Session-Id") == s.active
	s.mu.Unlock()
	if !valid {
		http.Error(w, "Invalid session ID", http.StatusNotFound)
		return
	}
	switch req.Method {
	case "tools/list":
		reply(map[string]any{"tools": []map[string]any{
			{"name": "echo", "description": "回声", "inputSchema": map[string]any{
				"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}},
			}, "annotations": map[string]any{"readOnlyHint": true}},
		}})
	default: // tools/call
		reply(map[string]any{"content": []map[string]any{{"type": "text", "text": "echo-ok"}}})
	}
}

// 全链路：MCP 服务端 → Manager → tool.Tool → agent.Runner → 模型拿到真实工具结果。
func TestAgentWithMCPEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc((&fakeMCPServer{}).handle))
	defer srv.Close()

	manager := mcp.NewManager()
	defer manager.Close()
	manager.Apply([]mcp.Server{{
		ID: "fake", Name: "Fake", Enabled: true, Transport: mcp.TransportHTTP, URL: srv.URL,
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	manager.EnsureReady(ctx)

	tools := manager.ToolsOf([]string{"fake"})
	if len(tools) != 1 || tools[0].Definition().Name != "mcp__fake__echo" {
		t.Fatalf("工具发现不符: %+v", tools)
	}
	// 只读注解应透传
	if ro, ok := tools[0].(tool.ReadOnlyTool); !ok || !ro.IsReadOnly() {
		t.Fatal("readOnlyHint 注解未透传")
	}

	step := 0
	model := func(ctx context.Context, messages []message.Message, options *message.Options, emit func(message.Delta)) error {
		step++
		if step == 1 {
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "c1", Name: "mcp__fake__echo", Arguments: `{"text":"hi"}`},
			}})
			return nil
		}
		emit(message.Delta{Content: "最终回答"})
		return nil
	}
	result, err := agent.New().Run(ctx,
		[]message.Message{{Role: message.RoleUser, Content: "调用回声"}},
		nil, agent.Config{Tools: tools}, model, nil)
	if err != nil {
		t.Fatal(err)
	}
	toolMsg := result.Messages[2]
	if toolMsg.Role != message.RoleTool || toolMsg.Content != "echo-ok" {
		t.Fatalf("MCP 工具结果不符: %+v", toolMsg)
	}
	if result.Content != "最终回答" {
		t.Fatalf("最终回答不符: %+v", result)
	}
}
