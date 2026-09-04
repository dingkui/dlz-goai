package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
)

const maxStdioMessageBytes = 16 * 1024 * 1024

type stdioReply struct {
	result json.RawMessage
	rpcErr *RPCError
	err    error
}

type stdioEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// StdioClient manages one long-lived MCP child process. Messages on stdout are
// newline-delimited JSON-RPC. stderr is consumed separately and never treated
// as protocol data.
type StdioClient struct {
	Command string
	Args    []string
	Env     map[string]string

	stateMu sync.Mutex
	writeMu sync.Mutex
	initMu  sync.Mutex
	id      int64
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	pending map[int64]chan stdioReply
	inited  bool
	info    ServerInfo
	stderr  string
}

func NewStdioClient(command string, args []string, env map[string]string) *StdioClient {
	return &StdioClient{
		Command: strings.TrimSpace(command),
		Args:    append([]string(nil), args...),
		Env:     cloneStringMap(env),
		pending: map[int64]chan stdioReply{},
	}
}

func (c *StdioClient) ensureStarted() error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.cmd != nil {
		return nil
	}
	if c.Command == "" {
		return errors.New("MCP stdio 启动命令为空")
	}
	cmd := exec.Command(c.Command, c.Args...)
	cmd.Env = mergedEnv(c.Env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("创建 MCP stdin 失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("创建 MCP stdout 失败: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("创建 MCP stderr 失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 MCP 命令失败: %w", err)
	}
	c.cmd = cmd
	c.stdin = stdin
	c.stderr = ""
	go c.consumeStderr(cmd, stderr)
	go c.readLoop(cmd, stdout)
	return nil
}

func (c *StdioClient) readLoop(cmd *exec.Cmd, stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), maxStdioMessageBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) > 0 {
			c.handleLine(append([]byte(nil), line...))
		}
	}
	scanErr := scanner.Err()
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if scanErr != nil {
		waitErr = fmt.Errorf("读取 MCP stdout 失败: %w", scanErr)
	}
	if waitErr == nil {
		waitErr = errors.New("MCP stdio 进程已退出")
	}
	c.finishProcess(cmd, waitErr)
}

func (c *StdioClient) consumeStderr(cmd *exec.Cmd, stderr io.Reader) {
	buffer := make([]byte, 4096)
	for {
		count, err := stderr.Read(buffer)
		if count > 0 {
			text := strings.TrimSpace(string(buffer[:count]))
			if len(text) > 2048 {
				text = text[len(text)-2048:]
			}
			if text != "" {
				c.stateMu.Lock()
				if c.cmd == cmd {
					c.stderr = text
				}
				c.stateMu.Unlock()
			}
		}
		if err != nil {
			return
		}
	}
}

func (c *StdioClient) handleLine(line []byte) {
	var envelope stdioEnvelope
	if json.Unmarshal(line, &envelope) != nil || envelope.JSONRPC != "2.0" {
		return
	}
	if envelope.Method != "" {
		if len(envelope.ID) > 0 && string(envelope.ID) != "null" {
			_ = c.writeJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      envelope.ID,
				"error":   map[string]any{"code": -32601, "message": "Client method not supported"},
			})
		}
		return
	}
	var id int64
	if len(envelope.ID) == 0 || json.Unmarshal(envelope.ID, &id) != nil {
		return
	}
	c.stateMu.Lock()
	waiter := c.pending[id]
	delete(c.pending, id)
	c.stateMu.Unlock()
	if waiter != nil {
		waiter <- stdioReply{result: envelope.Result, rpcErr: envelope.Error}
	}
}

func (c *StdioClient) call(ctx context.Context, method string, params any, out any) error {
	if err := c.ensureStarted(); err != nil {
		return err
	}
	c.stateMu.Lock()
	if c.cmd == nil {
		c.stateMu.Unlock()
		return errors.New("MCP stdio 进程未运行")
	}
	c.id++
	id := c.id
	waiter := make(chan stdioReply, 1)
	c.pending[id] = waiter
	c.stateMu.Unlock()

	request := JSONRPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	if err := c.writeJSON(request); err != nil {
		c.removePending(id)
		return err
	}
	select {
	case <-ctx.Done():
		c.removePending(id)
		return ctx.Err()
	case reply := <-waiter:
		if reply.err != nil {
			return reply.err
		}
		if reply.rpcErr != nil {
			return fmt.Errorf("MCP %s: [%d] %s", method, reply.rpcErr.Code, reply.rpcErr.Message)
		}
		if out == nil || len(reply.result) == 0 || bytes.Equal(reply.result, []byte("null")) {
			return nil
		}
		if err := json.Unmarshal(reply.result, out); err != nil {
			return fmt.Errorf("MCP %s: 解析响应失败: %w", method, err)
		}
		return nil
	}
}

func (c *StdioClient) notify(ctx context.Context, method string, params any) error {
	if err := c.ensureStarted(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	payload := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		payload["params"] = params
	}
	return c.writeJSON(payload)
}

func (c *StdioClient) writeJSON(payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.stateMu.Lock()
	stdin := c.stdin
	running := c.cmd != nil
	c.stateMu.Unlock()
	if !running || stdin == nil {
		return errors.New("MCP stdio 进程未运行")
	}
	if _, err := stdin.Write(encoded); err != nil {
		return fmt.Errorf("写入 MCP stdin 失败: %w", err)
	}
	return nil
}

func (c *StdioClient) Initialize(ctx context.Context) (ServerInfo, error) {
	c.initMu.Lock()
	defer c.initMu.Unlock()
	if c.IsInitialized() {
		return c.ServerInfo(), nil
	}
	var result InitializeResult
	if err := c.call(ctx, "initialize", InitializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo:      ClientInfo{Name: "ModelBox", Version: "1.0"},
	}, &result); err != nil {
		_ = c.Close()
		return ServerInfo{}, err
	}
	if result.ProtocolVersion == "" {
		_ = c.Close()
		return ServerInfo{}, errors.New("MCP initialize 响应缺少 protocolVersion")
	}
	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		_ = c.Close()
		return ServerInfo{}, err
	}
	c.stateMu.Lock()
	c.inited = true
	c.info = result.ServerInfo
	c.stateMu.Unlock()
	return result.ServerInfo, nil
}

func (c *StdioClient) ListTools(ctx context.Context) ([]Tool, error) {
	if !c.IsInitialized() {
		if _, err := c.Initialize(ctx); err != nil {
			return nil, err
		}
	}
	var result ListToolsResult
	if err := c.call(ctx, "tools/list", nil, &result); err != nil {
		return nil, err
	}
	return result.Tools, nil
}

func (c *StdioClient) CallTool(ctx context.Context, name string, args map[string]any) (CallToolResult, error) {
	if !c.IsInitialized() {
		if _, err := c.Initialize(ctx); err != nil {
			return CallToolResult{}, err
		}
	}
	var result CallToolResult
	if err := c.call(ctx, "tools/call", CallToolParams{Name: name, Arguments: args}, &result); err != nil {
		return CallToolResult{}, err
	}
	return result, nil
}

func (c *StdioClient) IsInitialized() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.inited && c.cmd != nil
}

func (c *StdioClient) ServerInfo() ServerInfo {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.info
}

func (c *StdioClient) Close() error {
	c.stateMu.Lock()
	cmd := c.cmd
	stdin := c.stdin
	c.cmd = nil
	c.stdin = nil
	c.inited = false
	c.info = ServerInfo{}
	waiters := c.takePendingLocked()
	c.stateMu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	for _, waiter := range waiters {
		waiter <- stdioReply{err: errors.New("MCP stdio 客户端已关闭")}
	}
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	err := cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (c *StdioClient) finishProcess(cmd *exec.Cmd, processErr error) {
	c.stateMu.Lock()
	if c.cmd != cmd {
		c.stateMu.Unlock()
		return
	}
	if c.stderr != "" {
		processErr = fmt.Errorf("%w: %s", processErr, c.stderr)
	}
	c.cmd = nil
	c.stdin = nil
	c.inited = false
	c.info = ServerInfo{}
	waiters := c.takePendingLocked()
	c.stateMu.Unlock()
	for _, waiter := range waiters {
		waiter <- stdioReply{err: processErr}
	}
}

func (c *StdioClient) removePending(id int64) {
	c.stateMu.Lock()
	delete(c.pending, id)
	c.stateMu.Unlock()
}

func (c *StdioClient) takePendingLocked() []chan stdioReply {
	waiters := make([]chan stdioReply, 0, len(c.pending))
	for id, waiter := range c.pending {
		waiters = append(waiters, waiter)
		delete(c.pending, id)
	}
	return waiters
}

func mergedEnv(overrides map[string]string) []string {
	values := make(map[string]string)
	for _, item := range os.Environ() {
		name, value, ok := strings.Cut(item, "=")
		if ok {
			values[name] = value
		}
	}
	for name, value := range overrides {
		values[name] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
