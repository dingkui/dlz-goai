package mcp

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const (
	maxServerIDLength = 64
	maxConfigItems    = 128
	maxConfigValue    = 8192
)

var (
	serverIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	headerRE   = regexp.MustCompile(`^[!#$%&'*+.^_\x60|~0-9A-Za-z-]+$`)
	envNameRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// NewServerClient creates a transport client without invoking a shell.
func NewServerClient(server Server) (ServerClient, error) {
	if err := ValidateServer(server); err != nil {
		return nil, err
	}
	if server.TransportType() == TransportStdio {
		return NewStdioClient(server.Command, server.Args, server.Env), nil
	}
	return NewClient(server.URL, server.Headers), nil
}

// ValidateServer validates persisted or one-off MCP server configuration.
func ValidateServer(server Server) error {
	if server.ID != "" {
		if len(server.ID) > maxServerIDLength || !serverIDRE.MatchString(server.ID) || strings.Contains(server.ID, "__") {
			return errors.New("MCP 服务 id 仅允许 1-64 位字母、数字、下划线或连字符，且不能包含连续双下划线")
		}
	}
	if len(strings.TrimSpace(server.Name)) > 128 {
		return errors.New("MCP 服务名称不能超过 128 个字符")
	}
	switch server.TransportType() {
	case TransportHTTP:
		return validateHTTPConfig(server)
	case TransportStdio:
		return validateStdioConfig(server)
	default:
		return fmt.Errorf("不支持的 MCP 传输方式: %s", server.Transport)
	}
}

func validateHTTPConfig(server Server) error {
	rawURL := strings.TrimSpace(server.URL)
	if rawURL == "" {
		return fmt.Errorf("MCP 服务「%s」缺少 URL", server.Name)
	}
	if len(rawURL) > 4096 {
		return errors.New("MCP server URL 过长")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("MCP server URL 必须是有效的 HTTP 或 HTTPS 地址")
	}
	if parsed.User != nil {
		return errors.New("MCP server URL 不允许包含用户名或密码，请使用请求头鉴权")
	}
	if len(server.Headers) > 64 {
		return errors.New("MCP 自定义请求头不能超过 64 项")
	}
	for name, value := range server.Headers {
		if !headerRE.MatchString(name) || len(value) > maxConfigValue || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("MCP 请求头无效: %s", name)
		}
		if isReservedHeader(name) {
			return fmt.Errorf("MCP 请求头 %s 由客户端管理，不能自定义", name)
		}
	}
	return nil
}

func validateStdioConfig(server Server) error {
	command := strings.TrimSpace(server.Command)
	if command == "" {
		return fmt.Errorf("MCP 服务「%s」缺少启动命令", server.Name)
	}
	if len(command) > 4096 || strings.ContainsRune(command, '\x00') {
		return errors.New("MCP 启动命令无效")
	}
	if len(server.Args) > maxConfigItems || len(server.Env) > maxConfigItems {
		return errors.New("MCP 命令参数或环境变量数量过多")
	}
	for _, arg := range server.Args {
		if len(arg) > maxConfigValue || strings.ContainsRune(arg, '\x00') {
			return errors.New("MCP 命令参数无效或过长")
		}
	}
	for name, value := range server.Env {
		if !envNameRE.MatchString(name) || len(value) > maxConfigValue || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("MCP 环境变量无效: %s", name)
		}
	}
	return nil
}

func isReservedHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Accept", "Connection", "Content-Length", "Content-Type", "Host", HeaderMcpSessionID, "Transfer-Encoding":
		return true
	default:
		return false
	}
}
