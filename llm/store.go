package llm

import (
	"errors"
	"sync"

	"github.com/dingkui/dlz-goai/internal/jsonutil"
)

// Store 模型服务配置仓库，持久化到构造时给定的 JSON 文件。
//
// 本包不假设任何应用目录——路径由调用方注入，
// 这样同一进程内多个应用可以各自持有互不影响的配置。
type Store struct {
	path string

	mu       sync.RWMutex
	Services []Service
	Default  string
}

// NewStore 绑定配置文件路径。文件可以不存在，Load 时会容忍。
func NewStore(path string) *Store {
	return &Store{path: path}
}

// Path 返回绑定的配置文件路径。
func (s *Store) Path() string { return s.path }

// Load 从磁盘加载。文件不存在时返回空配置而非错误。
func (s *Store) Load() error {
	var snapshot struct {
		Services []Service
		Default  string
	}
	err := jsonutil.LoadFile(s.path, &snapshot)
	if err != nil && !errors.Is(err, jsonutil.ErrNotFound) {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Services = snapshot.Services
	s.Default = snapshot.Default
	return nil
}

// Save 持久化到磁盘。
func (s *Store) Save() error {
	s.mu.RLock()
	snapshot := struct {
		Services []Service
		Default  string
	}{
		Services: cloneServices(s.Services),
		Default:  s.Default,
	}
	s.mu.RUnlock()
	return jsonutil.SaveFile(s.path, snapshot)
}

// Get 按 ID 返回服务。
func (s *Store) Get(id string) (Service, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id != "" {
		for _, svc := range s.Services {
			if svc.ID == id {
				return svc, true
			}
		}
	}
	return Service{}, false
}

// EnabledList 返回启用中的服务。
func (s *Store) EnabledList() []Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Service, 0, len(s.Services))
	for _, svc := range s.Services {
		if svc.Enabled {
			out = append(out, svc)
		}
	}
	return out
}

// Snapshot 返回可安全交给调用方的完整配置副本。
func (s *Store) Snapshot() ([]Service, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneServices(s.Services), s.Default
}

// Replace 原子替换服务列表和默认服务。
func (s *Store) Replace(services []Service, defaultID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Services = cloneServices(services)
	s.Default = defaultID
}

// AllModels 返回启用服务声明的全部模型名，按出现顺序去重。
func (s *Store) AllModels() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	seen := map[string]bool{}
	appendIf := func(list []string) {
		for _, m := range list {
			if m != "" && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	for _, svc := range s.Services {
		if !svc.Enabled {
			continue
		}
		appendIf(svc.Models)
		if svc.DefaultModel != "" && !seen[svc.DefaultModel] {
			seen[svc.DefaultModel] = true
			out = append(out, svc.DefaultModel)
		}
	}
	return out
}

func cloneServices(services []Service) []Service {
	out := make([]Service, len(services))
	copy(out, services)
	for i := range out {
		out[i].Models = append([]string(nil), services[i].Models...)
	}
	return out
}
