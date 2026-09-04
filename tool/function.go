package tool

import "context"

// Handler 进程内工具的执行体。
// args 已经过 JSON 反序列化，键对应 Definition.Parameters 里声明的属性。
type Handler func(ctx context.Context, args map[string]any) (Result, error)

// Func 把一个 Go 函数包装成 Tool。
// 适合工具逻辑就在本进程内的场景；跨进程工具走 MCP。
type Func struct {
	Name        string
	Description string
	Parameters  map[string]any
	ReadOnly    bool
	Handler     Handler
}

var _ Tool = Func{}

// NewFunc 构造进程内工具。parameters 为空时自动补一个空对象 schema，
// 因为部分模型服务不接受缺失的 parameters 字段。
func NewFunc(name, description string, parameters map[string]any, readOnly bool, handler Handler) Func {
	if parameters == nil {
		parameters = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return Func{
		Name:        name,
		Description: description,
		Parameters:  parameters,
		ReadOnly:    readOnly,
		Handler:     handler,
	}
}

func (f Func) Definition() Definition {
	return Definition{Name: f.Name, Description: f.Description, Parameters: f.Parameters}
}

func (f Func) IsReadOnly() bool { return f.ReadOnly }

func (f Func) Execute(ctx context.Context, args map[string]any) (Result, error) {
	if f.Handler == nil {
		return Result{}, ErrNoHandler
	}
	return f.Handler(ctx, args)
}
