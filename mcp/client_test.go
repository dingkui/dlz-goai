package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// fakeStreamableServer 复现 mcp-go 的 Streamable HTTP 会话语义（经 streamable_http.go 确认）：
//   - initialize 建立会话并在响应头返回 Mcp-Session-Id；200
//   - 其余请求（tools/list / tools/call / 通知）若未携带有效会话头 → 404 "Invalid session ID"
//   - 通过 expire() 模拟服务端重启，使旧会话立即失效
type fakeStreamableServer struct {
	mu         sync.Mutex
	active     string
	inits      int
	lastAttach string
}

func (s *fakeStreamableServer) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)

	writeJSON := func(payload map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}

	if req.Method == "initialize" {
		s.mu.Lock()
		s.inits++
		sid := "sess-" + strconv.Itoa(s.inits)
		s.active = sid
		s.mu.Unlock()
		w.Header().Set(HeaderMcpSessionID, sid)
		writeJSON(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "fake", "version": "1.0"},
			},
		})
		return
	}

	sid := r.Header.Get(HeaderMcpSessionID)
	s.mu.Lock()
	valid := sid != "" && sid == s.active
	s.lastAttach = sid
	s.mu.Unlock()
	if !valid {
		http.Error(w, "Invalid session ID", http.StatusNotFound)
		return
	}

	if req.Method == "tools/list" {
		writeJSON(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{"tools": []map[string]any{
				{"name": "t1", "description": "x", "inputSchema": map[string]any{}},
			}},
		})
		return
	}
	writeJSON(map[string]any{
		"jsonrpc": "2.0", "id": req.ID,
		"result": map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}},
	})
}

func (s *fakeStreamableServer) expire() {
	s.mu.Lock()
	s.active = ""
	s.mu.Unlock()
}

func (s *fakeStreamableServer) attach() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAttach
}

func (s *fakeStreamableServer) countInits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inits
}

// 回归：Streamable HTTP 客户端必须携带 Mcp-Session-Id，
// 否则服务端对 tools/list 返回 404（此前修复点）。
func TestClientStreamableCarriesSession(t *testing.T) {
	srv := &fakeStreamableServer{}
	hs := httptest.NewServer(http.HandlerFunc(srv.handle))
	defer hs.Close()

	c := NewClient(hs.URL, nil)
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "t1" {
		t.Fatalf("工具列表异常: %+v", tools)
	}
	if attach := srv.attach(); attach == "" {
		t.Fatal("后续请求未携带 Mcp-Session-Id 头")
	}
}

// 回归：会话失效（服务端重启）后，客户端应自动重新握手并成功恢复。
func TestClientSessionExpiredRetry(t *testing.T) {
	srv := &fakeStreamableServer{}
	hs := httptest.NewServer(http.HandlerFunc(srv.handle))
	defer hs.Close()

	c := NewClient(hs.URL, nil)
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("首次 ListTools: %v", err)
	}
	initsBefore := srv.countInits()

	srv.expire() // 模拟 mdk 等服务端重启，旧会话失效

	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("会话失效后应自动重连成功, got %v", err)
	}
	if got := srv.countInits(); got <= initsBefore {
		t.Fatalf("应重新执行 initialize 以建立新会话, inits=%d", got)
	}
}

// 回归：tools/call 同样携带会话头并正确返回文本。
func TestClientCallToolCarriesSession(t *testing.T) {
	srv := &fakeStreamableServer{}
	hs := httptest.NewServer(http.HandlerFunc(srv.handle))
	defer hs.Close()

	c := NewClient(hs.URL, nil)
	res, err := c.CallTool(context.Background(), "t1", map[string]any{"q": "x"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.Text() != "ok" {
		t.Fatalf("CallTool 文本异常: %q", res.Text())
	}
	if attach := srv.attach(); attach == "" {
		t.Fatal("tools/call 请求未携带 Mcp-Session-Id 头")
	}
}
