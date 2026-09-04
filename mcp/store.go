package mcp

import (
	"errors"
	"sync"

	"github.com/dingkui/dlz-goai/internal/jsonutil"
)

// Server 一个 MCP 服务配置。stdio 用 Command/Args/Env，HTTP 用 URL/Headers。
type Server struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Transport string            `json:"transport,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Enabled   bool              `json:"enabled"`
}

// TransportType 返回传输类型。老配置只有 URL 没有 Transport 字段，
// 缺省按 HTTP 处理以保持向后兼容。
func (s Server) TransportType() string {
	if s.Transport == "" {
		return TransportHTTP
	}
	return s.Transport
}

// Store MCP 服务配置仓库，持久化到构造时给定的 JSON 文件。
// 路径由调用方注入，库不假设任何应用目录。
type Store struct {
	path string

	mu      sync.Mutex
	Servers []Server
}

// NewStore 绑定配置文件路径。
func NewStore(path string) *Store {
	return &Store{path: path}
}

// Path 返回绑定的配置文件路径。
func (s *Store) Path() string { return s.path }

// Load 从磁盘加载。文件不存在时返回空配置而非错误。
func (s *Store) Load() error {
	var snapshot struct {
		Servers []Server
	}
	err := jsonutil.LoadFile(s.path, &snapshot)
	if err != nil && !errors.Is(err, jsonutil.ErrNotFound) {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Servers = snapshot.Servers
	return nil
}

// Save 持久化到磁盘。
func (s *Store) Save() error {
	s.mu.Lock()
	snapshot := struct {
		Servers []Server
	}{Servers: s.Servers}
	s.mu.Unlock()
	return jsonutil.SaveFile(s.path, snapshot)
}

// Get 按 ID 查找。
func (s *Store) Get(id string) (Server, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sv := range s.Servers {
		if sv.ID == id {
			return sv, true
		}
	}
	return Server{}, false
}

// EnabledList 返回启用中的服务。
func (s *Store) EnabledList() []Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Server, 0, len(s.Servers))
	for _, sv := range s.Servers {
		if sv.Enabled {
			out = append(out, sv)
		}
	}
	return out
}

// Replace 原子替换服务列表。
func (s *Store) Replace(servers []Server) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Servers = append([]Server(nil), servers...)
}
