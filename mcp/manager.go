// Package mcp 是 Model Context Protocol 的客户端实现：连接管理、
// 工具发现、调用路由，以及把 MCP 工具适配成本库统一的 tool.Tool。
//
// Manager 由调用方持有单例。配置变更时调用 Apply(servers) 重建内部状态；
// 启用中的服务会自动拉取 tools/list 并缓存，每 5 分钟刷新一次。
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// ToolWithSource 带来源的工具。ServerID 决定工具名命名空间（见 naming.go）。
type ToolWithSource struct {
	ServerID   string
	ServerName string
	Tool       Tool
}

// FullName 模型看到的工具名，含服务前缀以避免跨服务重名。
func (t ToolWithSource) FullName() string {
	return FullName(t.ServerID, t.Tool.Name)
}

// ReadOnly 判断服务端是否声明该工具无副作用。
// 缺失注解时按"不安全"处理——宁可多问一次，也不要静默执行写操作。
func (t ToolWithSource) ReadOnly() bool {
	readOnly, ok := t.Tool.Annotations["readOnlyHint"].(bool)
	return ok && readOnly
}

// serverState 单个 server 的运行时状态。
type serverState struct {
	server  Server
	client  ServerClient
	tools   []Tool
	err     error
	updated time.Time
}

// Manager 管理所有 MCP 服务的连接与工具缓存。
type Manager struct {
	mu       sync.RWMutex
	states   map[string]*serverState // serverID -> state
	stopCh   chan struct{}
	stopOnce sync.Once
	closed   bool
}

// NewManager 构造并启动后台刷新。
func NewManager() *Manager {
	m := &Manager{
		states: map[string]*serverState{},
		stopCh: make(chan struct{}),
	}
	go m.refreshLoop()
	return m
}

// Apply applies the latest server configuration and retires clients whose
// transport or process configuration changed.
func (m *Manager) Apply(servers []Server) {
	var retired []ServerClient
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	seen := map[string]bool{}
	for _, sv := range servers {
		seen[sv.ID] = true
		st, ok := m.states[sv.ID]
		if !ok {
			st = &serverState{server: sv}
			m.states[sv.ID] = st
		} else if serverChanged(st.server, sv) {
			if st.client != nil {
				retired = append(retired, st.client)
			}
			*st = serverState{server: sv}
		} else {
			if st.server.Enabled && !sv.Enabled && st.client != nil {
				retired = append(retired, st.client)
				st.client = nil
				st.tools = nil
				st.err = nil
			}
			st.server = sv
		}
	}
	for id, st := range m.states {
		if !seen[id] {
			if st.client != nil {
				retired = append(retired, st.client)
			}
			delete(m.states, id)
		}
	}
	m.mu.Unlock()
	closeClients(retired)
	// 异步刷新所有启用的
	for _, sv := range servers {
		if sv.Enabled {
			go m.RefreshOne(sv.ID)
		}
	}
}

// serverChanged compares all process/transport settings while ignoring Enabled.
func serverChanged(a, b Server) bool {
	a.Enabled = false
	b.Enabled = false
	return !reflect.DeepEqual(a, b)
}

// refreshLoop 每 5 分钟刷新所有启用 server 的工具缓存。
func (m *Manager) refreshLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.RefreshAll()
		case <-m.stopCh:
			return
		}
	}
}

// RefreshAll 同步刷新所有启用 server。
func (m *Manager) RefreshAll() {
	m.mu.RLock()
	ids := make([]string, 0, len(m.states))
	for id, st := range m.states {
		if st.server.Enabled {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range ids {
		m.RefreshOne(id)
	}
}

// EnsureReady waits for the first tool discovery of enabled servers. It is
// used before building an automatic agent tool set so startup races do not
// silently reduce a conversation to built-in web tools only.
func (m *Manager) EnsureReady(ctx context.Context) {
	m.mu.RLock()
	ids := make([]string, 0, len(m.states))
	for id, st := range m.states {
		if st.server.Enabled && st.updated.IsZero() {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	if len(ids) == 0 {
		return
	}

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Add(1)
			go func(serverID string) {
				defer wg.Done()
				m.RefreshOne(serverID)
			}(id)
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// RefreshOne 刷新指定 server 的工具缓存。
func (m *Manager) RefreshOne(serverID string) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	st, ok := m.states[serverID]
	if !ok || !st.server.Enabled {
		m.mu.Unlock()
		return
	}
	if st.client == nil {
		client, err := NewServerClient(st.server)
		if err != nil {
			st.err = err
			st.updated = time.Now()
			name, id := st.server.Name, st.server.ID
			m.mu.Unlock()
			log.Printf("[mcp] refresh %s (%s) 失败: %v", name, id, err)
			return
		}
		st.client = client
	}
	client := st.client
	name, id := st.server.Name, st.server.ID
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tools, err := client.ListTools(ctx)
	m.mu.Lock()
	current, exists := m.states[serverID]
	if exists && current == st && current.client == client && current.server.Enabled {
		current.tools = tools
		current.err = err
		current.updated = time.Now()
	}
	m.mu.Unlock()
	if err != nil {
		log.Printf("[mcp] refresh %s (%s) 失败: %v", name, id, err)
	}
}

// AllTools 返回所有启用 server 的工具合并列表（带 server 来源）。
func (m *Manager) AllTools() []ToolWithSource {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ToolWithSource, 0, 8)
	for _, st := range m.states {
		if !st.server.Enabled || st.err != nil {
			continue
		}
		for _, t := range st.tools {
			out = append(out, ToolWithSource{
				ServerID:   st.server.ID,
				ServerName: st.server.Name,
				Tool:       t,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullName() < out[j].FullName() })
	return out
}

// ToolsFor 返回指定 MCP 服务上允许暴露给智能体的工具。
// 智能体必须显式选择至少一个服务和至少一个工具。
func (m *Manager) ToolsFor(serverIDs, allowList []string) []ToolWithSource {
	servers := stringSet(serverIDs)
	if len(servers) == 0 {
		return nil
	}
	allowed := stringSet(allowList)
	if len(allowed) == 0 {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ToolWithSource, 0, 8)
	for _, st := range m.states {
		if _, ok := servers[st.server.ID]; !ok || !st.server.Enabled || st.err != nil {
			continue
		}
		for _, tool := range st.tools {
			candidate := ToolWithSource{ServerID: st.server.ID, ServerName: st.server.Name, Tool: tool}
			if len(allowed) > 0 {
				if _, ok := allowed[candidate.FullName()]; !ok {
					continue
				}
			}
			out = append(out, candidate)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullName() < out[j].FullName() })
	return out
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result[value] = struct{}{}
		}
	}
	return result
}

// CallTool 调用工具。fullToolName 必须是 AllTools 返回的 FullName。
func (m *Manager) CallTool(ctx context.Context, fullToolName string, args map[string]any) (CallToolResult, error) {
	serverID, toolName, ok := ParseFullName(fullToolName)
	if !ok {
		return CallToolResult{}, fmt.Errorf("invalid tool name: %s", fullToolName)
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return CallToolResult{}, errors.New("MCP manager is closed")
	}
	st, ok := m.states[serverID]
	if !ok || !st.server.Enabled {
		m.mu.Unlock()
		return CallToolResult{}, fmt.Errorf("MCP server %s is unavailable", serverID)
	}
	found := false
	for _, tool := range st.tools {
		if tool.Name == toolName {
			found = true
			break
		}
	}
	if !found {
		m.mu.Unlock()
		return CallToolResult{}, fmt.Errorf("MCP tool %s is unavailable", fullToolName)
	}
	if st.client == nil {
		client, err := NewServerClient(st.server)
		if err != nil {
			m.mu.Unlock()
			return CallToolResult{}, err
		}
		st.client = client
	}
	client := st.client
	m.mu.Unlock()
	return client.CallTool(ctx, toolName, args)
}

// ServerStatus 单个 server 的状态摘要（供前端展示）。
type ServerStatus struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Transport string `json:"transport"`
	Enabled   bool   `json:"enabled"`
	ToolCount int    `json:"toolCount"`
	Err       string `json:"err,omitempty"`
	Updated   int64  `json:"updated,omitempty"`
}

// Statuses 返回所有 server 的状态摘要。
func (m *Manager) Statuses() []ServerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ServerStatus, 0, len(m.states))
	for _, st := range m.states {
		s := ServerStatus{
			ID:        st.server.ID,
			Name:      st.server.Name,
			URL:       st.server.URL,
			Transport: st.server.TransportType(),
			Enabled:   st.server.Enabled,
			ToolCount: len(st.tools),
		}
		if st.err != nil {
			s.Err = st.err.Error()
		}
		if !st.updated.IsZero() {
			s.Updated = st.updated.Unix()
		}
		out = append(out, s)
	}
	return out
}

// TestConnection 测试与指定配置（不一定已保存）的连通性，返回 server info 与工具数。
func (m *Manager) TestConnection(ctx context.Context, server Server) (ServerInfo, int, error) {
	client, err := NewServerClient(server)
	if err != nil {
		return ServerInfo{}, 0, err
	}
	defer client.Close()
	info, err := client.Initialize(ctx)
	if err != nil {
		return ServerInfo{}, 0, err
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		return info, 0, err
	}
	return info, len(tools), nil
}

// Close stops background refreshes and all long-lived MCP clients.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		close(m.stopCh)
		m.mu.Lock()
		m.closed = true
		clients := make([]ServerClient, 0, len(m.states))
		for _, state := range m.states {
			if state.client != nil {
				clients = append(clients, state.client)
				state.client = nil
			}
		}
		m.mu.Unlock()
		closeClients(clients)
	})
}

func closeClients(clients []ServerClient) {
	for _, client := range clients {
		_ = client.Close()
	}
}
