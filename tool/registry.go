package tool

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Policy 决定一次工具调用是否需要人工确认。
type Policy string

const (
	// PolicyAuto 直接执行，无需确认。
	PolicyAuto Policy = "auto"
	// PolicyConfirm 执行前先征求确认。
	PolicyConfirm Policy = "confirm"
	// PolicyDeny 禁止调用，模型会得到一条错误回执。
	PolicyDeny Policy = "deny"
)

// Tool 是 agent 眼中唯一的工具抽象。
//
// MCP 工具、进程内原生工具、HTTP 远程工具都实现这个接口，
// 因此 agent 不需要知道工具来自哪里——这是 agent 不依赖 mcp 的前提。
type Tool interface {
	// Definition 返回给模型看的工具描述。
	Definition() Definition
	// Execute 执行工具。返回的 error 表示执行失败，
	// Result.IsError 表示业务失败但要把信息交回模型。
	Execute(ctx context.Context, args map[string]any) (Result, error)
}

// ReadOnlyTool 由无副作用的工具可选实现。
// 实现且返回 true 时，默认策略为直接执行；未实现的工具默认需要确认。
// 这样即使调用方忘记配置策略，危险操作也不会被静默放行。
type ReadOnlyTool interface {
	IsReadOnly() bool
}

// Sourced 有出处的工具可选实现，用于在事件与审批请求中标注工具来源
// （例如 MCP 服务 ID 与显示名）。未实现时来源字段为空。
type Sourced interface {
	SourceID() string
	SourceName() string
}

// Registry 工具注册表：集中管理工具、策略与暴露给模型的定义列表。
type Registry struct {
	mu       sync.RWMutex
	tools    map[string]Tool
	policies map[string]Policy
}

// NewRegistry 构造空注册表。
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}, policies: map[string]Policy{}}
}

// Register 注册工具。同名工具会被拒绝，避免静默覆盖——
// 名字冲突几乎总是配置错误。
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return fmt.Errorf("tool: 工具为 nil")
	}
	name := t.Definition().Name
	if name == "" {
		return fmt.Errorf("tool: 工具名为空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("tool: 工具 %s 已注册", name)
	}
	r.tools[name] = t
	return nil
}

// MustRegister 注册工具，失败直接 panic。适用于初始化阶段的工具装配。
func (r *Registry) MustRegister(t Tool) {
	if err := r.Register(t); err != nil {
		panic(err)
	}
}

// SetPolicy 为工具设置策略。
func (r *Registry) SetPolicy(name string, policy Policy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.policies[name] = policy
}

// Get 按名字取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// List 返回全部工具，按名字排序以保证暴露顺序稳定。
// 顺序稳定很重要：否则同样的输入可能产生不同的提示词。
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Definition().Name < out[j].Definition().Name
	})
	return out
}

// Select 返回名字命中白名单的工具。白名单为空表示不选任何工具——
// 与"不做限制"区分开，避免误把空过滤器当成全量。
func (r *Registry) Select(names []string) []Tool {
	if len(names) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		allowed[name] = struct{}{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(names))
	for name, t := range r.tools {
		if _, ok := allowed[name]; ok {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Definition().Name < out[j].Definition().Name
	})
	return out
}

// Definitions 返回选中工具的定义列表，直接用于构造模型请求。
func (r *Registry) Definitions(tools []Tool) []Definition {
	out := make([]Definition, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Definition())
	}
	return out
}

// PolicyFor 判定某个工具的生效策略：
// 显式配置的策略优先；未配置时按工具是否声明只读决定。
func (r *Registry) PolicyFor(name string) Policy {
	r.mu.RLock()
	policy, ok := r.policies[name]
	t := r.tools[name]
	r.mu.RUnlock()
	if ok && (policy == PolicyAuto || policy == PolicyConfirm || policy == PolicyDeny) {
		return policy
	}
	if ro, ok := t.(ReadOnlyTool); ok && ro.IsReadOnly() {
		return PolicyAuto
	}
	return PolicyConfirm
}
