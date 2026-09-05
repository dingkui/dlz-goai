package tool

import "errors"

var (
	// ErrNoHandler 工具没有绑定执行体。
	ErrNoHandler = errors.New("tool: handler is nil")
	// ErrNotFound 请求的工具未注册。
	ErrNotFound = errors.New("tool: not found")
	// ErrDuplicate 同名工具重复注册。
	ErrDuplicate = errors.New("tool: duplicate name")
	// ErrInvalidArguments 模型给出的参数不是合法 JSON。
	ErrInvalidArguments = errors.New("tool: arguments are not valid JSON")
	// ErrDenied 工具被当前策略禁止。
	ErrDenied = errors.New("tool: denied by policy")
)
