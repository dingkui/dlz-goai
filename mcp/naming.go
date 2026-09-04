package mcp

import "strings"

// ToolPrefix 给模型暴露的工具名前缀，完整格式为 mcp__{serverID}__{toolName}。
//
// 加前缀不是装饰：多个 MCP 服务端完全可能导出同名工具
// （比如两个服务都有 search），没有命名空间就无法区分。
// 模型看到的是带前缀的名字，调用时原样回传，由本包解析回来源。
const ToolPrefix = "mcp__"

const nameSeparator = "__"

// FullName 拼接模型可见的工具名。
func FullName(serverID, toolName string) string {
	return ToolPrefix + serverID + nameSeparator + toolName
}

// ParseFullName 解析带前缀的工具名，返回服务 ID 与原始工具名。
// 格式不符时 ok 为 false，调用方应拒绝而非猜测。
func ParseFullName(full string) (serverID, toolName string, ok bool) {
	if !strings.HasPrefix(full, ToolPrefix) {
		return "", "", false
	}
	rest := full[len(ToolPrefix):]
	idx := strings.Index(rest, nameSeparator)
	if idx <= 0 || idx == len(rest)-2 {
		return "", "", false
	}
	return rest[:idx], rest[idx+2:], true
}
