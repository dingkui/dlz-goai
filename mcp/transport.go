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
			return errors.New("MCP server id must be 1-64 chars of letters, digits, underscore or hyphen, without consecutive underscores")
		}
	}
	if len(strings.TrimSpace(server.Name)) > 128 {
		return errors.New("MCP server name exceeds 128 characters")
	}
	switch server.TransportType() {
	case TransportHTTP:
		return validateHTTPConfig(server)
	case TransportStdio:
		return validateStdioConfig(server)
	default:
		return fmt.Errorf("unsupported MCP transport: %s", server.Transport)
	}
}

func validateHTTPConfig(server Server) error {
	rawURL := strings.TrimSpace(server.URL)
	if rawURL == "" {
		return fmt.Errorf("MCP server %q is missing a URL", server.Name)
	}
	if len(rawURL) > 4096 {
		return errors.New("MCP server URL too long")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("MCP server URL must be a valid HTTP or HTTPS address")
	}
	if parsed.User != nil {
		return errors.New("MCP server URL must not embed credentials; use headers for auth")
	}
	if len(server.Headers) > 64 {
		return errors.New("MCP custom headers exceed 64 entries")
	}
	for name, value := range server.Headers {
		if !headerRE.MatchString(name) || len(value) > maxConfigValue || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("MCP invalid header name: %s", name)
		}
		if isReservedHeader(name) {
			return fmt.Errorf("MCP header %s is client-managed and cannot be overridden", name)
		}
	}
	return nil
}

func validateStdioConfig(server Server) error {
	command := strings.TrimSpace(server.Command)
	if command == "" {
		return fmt.Errorf("MCP server %q is missing a launch command", server.Name)
	}
	if len(command) > 4096 || strings.ContainsRune(command, '\x00') {
		return errors.New("MCP invalid launch command")
	}
	if len(server.Args) > maxConfigItems || len(server.Env) > maxConfigItems {
		return errors.New("too many MCP command args or env vars")
	}
	for _, arg := range server.Args {
		if len(arg) > maxConfigValue || strings.ContainsRune(arg, '\x00') {
			return errors.New("MCP command argument invalid or too long")
		}
	}
	for name, value := range server.Env {
		if !envNameRE.MatchString(name) || len(value) > maxConfigValue || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("MCP invalid env var name: %s", name)
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
