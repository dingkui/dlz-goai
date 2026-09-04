package tool

import "errors"

var (
	// ErrNoHandler 工具没有绑定执行体。
	ErrNoHandler = errors.New("tool: 工具没有执行体")
	// ErrNotFound 请求的工具未注册。
	ErrNotFound = errors.New("tool: 工具不存在")
	// ErrDuplicate 同名工具重复注册。
	ErrDuplicate = errors.New("tool: 工具名重复")
	// ErrInvalidArguments 模型给出的参数不是合法 JSON。
	ErrInvalidArguments = errors.New("tool: 参数不是合法 JSON")
	// ErrDenied 工具被当前策略禁止。
	ErrDenied = errors.New("tool: 工具被当前策略禁止")
)
