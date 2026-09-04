// Package mcp — Model Context Protocol 客户端与服务管理。
//
// MCP（Model Context Protocol，Anthropic 开源）基于 JSON-RPC 2.0，核心流程：
// initialize → tools/list → tools/call。传输方式有 stdio 与 Streamable HTTP 两种；
// 本包支持 stdio 与 Streamable HTTP 传输。
package mcp

import "context"

const (
	TransportHTTP  = "http"
	TransportStdio = "stdio"
)

// ServerClient 是 Manager 使用的 MCP 传输抽象。
type ServerClient interface {
	Initialize(context.Context) (ServerInfo, error)
	ListTools(context.Context) ([]Tool, error)
	CallTool(context.Context, string, map[string]any) (CallToolResult, error)
	Close() error
}

// ProtocolVersion 协商的协议版本（兼容主流 MCP server）。
const ProtocolVersion = "2024-11-05"

// JSONRPCRequest JSON-RPC 2.0 请求体。
type JSONRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// JSONRPCResponse JSON-RPC 2.0 响应体。
type JSONRPCResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      int64     `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

// RPCError JSON-RPC 错误对象。
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

// InitializeParams initialize 请求参数。
type InitializeParams struct {
	ProtocolVersion string     `json:"protocolVersion"`
	Capabilities    any        `json:"capabilities"`
	ClientInfo      ClientInfo `json:"clientInfo"`
}

// ClientInfo 客户端标识。
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResult initialize 响应结果。
type InitializeResult struct {
	ProtocolVersion string     `json:"protocolVersion"`
	Capabilities    any        `json:"capabilities"`
	ServerInfo      ServerInfo `json:"serverInfo"`
}

// ServerInfo server 端标识。
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ListToolsResult tools/list 响应结果。
type ListToolsResult struct {
	Tools []Tool `json:"tools"`
}

// Tool 一个 MCP 工具定义。
type Tool struct {
	Name         string         `json:"name"`
	Title        string         `json:"title,omitempty"`
	Description  string         `json:"description,omitempty"`
	InputSchema  map[string]any `json:"inputSchema,omitempty"`
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	Annotations  map[string]any `json:"annotations,omitempty"`
}

// CallToolParams tools/call 请求参数。
type CallToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// CallToolResult tools/call 响应结果。
type CallToolResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// ContentBlock 工具调用返回的内容块。
type ContentBlock struct {
	Type string `json:"type"` // text / image / resource
	Text string `json:"text,omitempty"`
}

// Text 提取所有 text 类型块的拼接文本（用于注入到对话上下文）。
func (r *CallToolResult) Text() string {
	if r == nil || len(r.Content) == 0 {
		return ""
	}
	var buf []byte
	for i, c := range r.Content {
		if c.Type == "text" && c.Text != "" {
			if i > 0 {
				buf = append(buf, '\n')
			}
			buf = append(buf, c.Text...)
		}
	}
	return string(buf)
}
