package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/tool"
)

// sse 把若干 JSON 载荷编码为 OpenAI 风格的 SSE 流。
func sse(t *testing.T, payloads ...map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, p := range payloads {
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString("data: " + string(raw) + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func writeSSE(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(body))
}

// ChatStream：SSE 内容增量与结束原因正确回传；请求携带模型名与鉴权头。
func TestChatStreamSSE(t *testing.T) {
	var gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		writeSSE(w, sse(t,
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "你"}}}},
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "好"}}}},
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}},
		))
	}))
	defer srv.Close()

	p := New(srv.URL+"/v1", "sk-test")
	var contents []string
	var finish string
	err := p.ChatStream(context.Background(), "gpt-4o-mini",
		[]message.Message{{Role: message.RoleUser, Content: "hi"}}, nil,
		func(d message.Delta) {
			if d.Content != "" {
				contents = append(contents, d.Content)
			}
			if d.FinishReason != "" {
				finish = d.FinishReason
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(contents, "") != "你好" {
		t.Fatalf("增量内容不符: %v", contents)
	}
	if finish != "stop" {
		t.Fatalf("finish_reason 不符: %q", finish)
	}
	if gotModel != "gpt-4o-mini" {
		t.Fatalf("模型名不符: %q", gotModel)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("鉴权头不符: %q", gotAuth)
	}
}

// 端点候选回退：baseURL 未带 /v1 时先试 /v1/chat/completions（404），
// 再回退到根路径 /chat/completions。
func TestChatStreamURLFallback(t *testing.T) {
	var hitPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPaths = append(hitPaths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeSSE(w, sse(t, map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"content": "ok"}, "finish_reason": "stop",
		}}}))
	}))
	defer srv.Close()

	p := New(srv.URL, "")
	err := p.ChatStream(context.Background(), "m",
		[]message.Message{{Role: message.RoleUser, Content: "hi"}}, nil,
		func(message.Delta) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(hitPaths) != 2 {
		t.Fatalf("应依次尝试两个候选端点, got %v", hitPaths)
	}
}

// 流式工具调用分片：wire 层按原样透传 CallDelta 片段（聚合由 agent 层完成），
// 此处验证分片的 Index/ID/name/arguments 字段无丢失。
func TestChatStreamToolCallDelta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, sse(t,
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "id": "c1", "type": "function",
					"function": map[string]any{"name": "echo", "arguments": `{"q"`},
			}}}}}},
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "function": map[string]any{"arguments": `:"hi"}`}},
			}}}}},
			map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"}}},
		))
	}))
	defer srv.Close()

	p := New(srv.URL+"/v1", "")
	var fragments []tool.CallDelta
	err := p.ChatStream(context.Background(), "m",
		[]message.Message{{Role: message.RoleUser, Content: "hi"}}, nil,
		func(d message.Delta) {
			fragments = append(fragments, d.ToolCalls...)
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(fragments) != 2 {
		t.Fatalf("应透传 2 个分片: %+v", fragments)
	}
	first, second := fragments[0], fragments[1]
	if first.Index != 0 || first.ID != "c1" || first.Name != "echo" || first.Arguments != `{"q"` {
		t.Fatalf("首片字段不符: %+v", first)
	}
	if second.Index != 0 || second.ID != "" || second.Arguments != `:"hi"}` {
		t.Fatalf("续片字段不符: %+v", second)
	}
}

// ListModels：/models 端点解析 data[].id。
func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/models") {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"}]}`))
	}))
	defer srv.Close()

	p := New(srv.URL+"/v1", "key")
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != "a" || models[1] != "b" {
		t.Fatalf("模型列表不符: %v", models)
	}
}

// Ping：任何内容增量即视为可用。
func TestPing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, sse(t, map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"content": "pong"},
		}}}))
	}))
	defer srv.Close()

	if err := New(srv.URL+"/v1", "").Ping(context.Background()); err != nil {
		t.Fatalf("Ping 应成功: %v", err)
	}
}
