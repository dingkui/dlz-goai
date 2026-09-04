// Package mcp · client.go — HTTP JSON-RPC 客户端。
//
// 一个 Client 对应一个 MCP server 端点；按需调用 initialize/tools.list/tools.call。
// 鉴权通过自定义 Headers（如 Authorization）传入。
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// HeaderMcpSessionID Streamable HTTP 传输要求的会话标识响应/请求头。
// 服务端在 initialize 响应头返回它，客户端必须在后续请求原样带回，
// 否则 mcp-go 等实现会以 404 "Invalid session ID" 拒绝。
const HeaderMcpSessionID = "Mcp-Session-Id"

// errSessionInvalid 会话已失效（Streamable HTTP 404）的哨兵错误，
// 调用方可用 errors.Is 识别后触发重新握手重试。
var errSessionInvalid = errors.New("mcp session invalid or expired")

// HTTPError 带状态码的 MCP HTTP 错误。
type HTTPError struct {
	StatusCode int
	URL        string
	Body       string
}

func (e *HTTPError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("MCP HTTP %d (%s): %s", e.StatusCode, e.URL, e.Body)
	}
	return fmt.Sprintf("MCP HTTP %d (%s)", e.StatusCode, e.URL)
}

// Client 与单个 MCP server 通信的 HTTP JSON-RPC 客户端。
// 一个 Client 维护一条 Streamable HTTP 会话：initialize 后持有 sessionID，
// 后续 tools/list / tools/call / 通知都必须携带 Mcp-Session-Id 头。
type Client struct {
	URL     string
	Headers map[string]string
	HTTP    *http.Client

	mu        sync.Mutex
	initMu    sync.Mutex
	id        int64
	inited    bool
	info      ServerInfo
	sessionID string
}

// NewClient 构造客户端。timeout=0 表示无总超时（适合长连接的 tools/call）。
func NewClient(url string, headers map[string]string) *Client {
	return &Client{
		URL:     url,
		Headers: cloneStringMap(headers),
		HTTP:    &http.Client{},
	}
}

// nextID 生成单调递增的请求 ID。
func (c *Client) nextID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.id++
	return c.id
}

// call 发送一次 JSON-RPC 请求并把 result 反序列化到 out。
func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	reqBody := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      c.nextID(),
		Method:  method,
		Params:  params,
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	// Streamable HTTP：除 initialize 外的请求必须回传会话头，否则服务端会 404。
	if sid := c.getSessionID(); sid != "" {
		req.Header.Set(HeaderMcpSessionID, sid)
	}
	applyClientHeaders(req, c.Headers)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// initialize 会创建新会话，服务端在响应头返回 Mcp-Session-Id，需记录下来。
	if method == "initialize" {
		if sid := resp.Header.Get(HeaderMcpSessionID); sid != "" {
			c.mu.Lock()
			c.sessionID = sid
			c.mu.Unlock()
		}
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := readHTTPBody(resp.Body)
		herr := &HTTPError{StatusCode: resp.StatusCode, URL: c.URL, Body: string(msg)}
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %v", errSessionInvalid, herr)
		}
		return herr
	}
	rpc, err := readRPCResponse(resp.Body, resp.Header.Get("Content-Type"), reqBody.ID)
	if err != nil {
		return fmt.Errorf("MCP %s: 解析响应失败: %w", method, err)
	}
	if rpc.Error != nil {
		return fmt.Errorf("MCP %s: [%d] %s", method, rpc.Error.Code, rpc.Error.Message)
	}
	if out == nil || rpc.Result == nil {
		return nil
	}
	// result 是任意结构，先 marshal 再 unmarshal 到目标类型
	raw, err := json.Marshal(rpc.Result)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// Initialize 协商协议版本。成功后缓存 server info。
func (c *Client) Initialize(ctx context.Context) (ServerInfo, error) {
	var res InitializeResult
	c.initMu.Lock()
	defer c.initMu.Unlock()
	if c.IsInitialized() {
		return c.ServerInfo(), nil
	}
	err := c.call(ctx, "initialize", InitializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo:      ClientInfo{Name: "ModelBox", Version: "1.0"},
	}, &res)
	if err != nil {
		c.resetSession()
		return ServerInfo{}, err
	}
	if res.ProtocolVersion == "" {
		c.resetSession()
		return ServerInfo{}, errors.New("MCP initialize 响应缺少 protocolVersion")
	}
	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		c.resetSession()
		return ServerInfo{}, err
	}
	c.mu.Lock()
	c.inited = true
	c.info = res.ServerInfo
	c.mu.Unlock()
	return res.ServerInfo, nil
}

// getSessionID 线程安全地读取当前会话 id。
func (c *Client) getSessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

// notify 发送无返回通知。
func (c *Client) notify(ctx context.Context, method string, params any) error {
	reqBody := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		reqBody["params"] = params
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if sid := c.getSessionID(); sid != "" {
		req.Header.Set(HeaderMcpSessionID, sid)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	applyClientHeaders(req, c.Headers)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		return &HTTPError{StatusCode: resp.StatusCode, URL: c.URL}
	}
	return nil
}

// ListTools 拉 server 暴露的工具列表。
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if !c.IsInitialized() {
		if _, err := c.Initialize(ctx); err != nil {
			return nil, err
		}
	}
	var res ListToolsResult
	err := c.retryOnce(ctx, func() error { return c.call(ctx, "tools/list", nil, &res) })
	if err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// CallTool 调用工具。
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (CallToolResult, error) {
	if !c.IsInitialized() {
		if _, err := c.Initialize(ctx); err != nil {
			return CallToolResult{}, err
		}
	}
	var res CallToolResult
	err := c.retryOnce(ctx, func() error { return c.call(ctx, "tools/call", CallToolParams{Name: name, Arguments: args}, &res) })
	if err != nil {
		return CallToolResult{}, err
	}
	return res, nil
}

// IsInitialized 返回是否已完成 initialize 握手。
func (c *Client) IsInitialized() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inited
}

// resetSession 清除会话状态（会话失效/服务重启后调用，触发下一次自动重握手）。
func (c *Client) resetSession() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inited = false
	c.sessionID = ""
	c.info = ServerInfo{}
}

// retryOnce 捕获 errSessionInvalid 时执行一次重新握手并重试 target。
func (c *Client) retryOnce(ctx context.Context, target func() error) error {
	err := target()
	if !errors.Is(err, errSessionInvalid) {
		return err
	}
	// 会话失效（通常 mdk 等主进程重启）：重置后重新 initialize 再试一次
	c.resetSession()
	if _, ie := c.Initialize(ctx); ie != nil {
		return fmt.Errorf("%w (re-init failed: %v)", err, ie)
	}
	if err2 := target(); err2 != nil {
		return err2
	}
	return nil
}

// ServerInfo 返回缓存的 server 信息（未握手时为空）。
func (c *Client) ServerInfo() ServerInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// Close releases idle HTTP connections and clears session state.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.resetSession()
	if c.HTTP != nil {
		c.HTTP.CloseIdleConnections()
	}
	return nil
}
