package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const maxHTTPResponseBytes = 16 * 1024 * 1024

func readHTTPBody(body io.Reader) ([]byte, error) {
	limited := io.LimitReader(body, maxHTTPResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > maxHTTPResponseBytes {
		return nil, errors.New("MCP 响应超过 16 MiB 限制")
	}
	return data, nil
}

func decodeRPCResponse(body []byte, requestID int64) (JSONRPCResponse, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return JSONRPCResponse{}, errors.New("MCP 返回空响应")
	}
	if trimmed[0] == '{' {
		return parseRPCResponse(trimmed, requestID)
	}

	normalized := bytes.ReplaceAll(trimmed, []byte("\r\n"), []byte("\n"))
	for _, block := range bytes.Split(normalized, []byte("\n\n")) {
		var payload []byte
		for _, line := range bytes.Split(block, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(data, []byte("[DONE]")) {
				continue
			}
			payload = append(payload, data...)
		}
		if len(payload) == 0 {
			continue
		}
		response, err := parseRPCResponse(payload, requestID)
		if err == nil {
			return response, nil
		}
	}
	return JSONRPCResponse{}, fmt.Errorf("MCP 响应中没有匹配请求 %d 的 JSON-RPC 结果", requestID)
}

func parseRPCResponse(payload []byte, requestID int64) (JSONRPCResponse, error) {
	var response JSONRPCResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return JSONRPCResponse{}, err
	}
	if response.JSONRPC != "2.0" {
		return JSONRPCResponse{}, errors.New("MCP 响应缺少 jsonrpc 2.0 标识")
	}
	if response.ID != requestID {
		return JSONRPCResponse{}, fmt.Errorf("MCP 响应 id 不匹配: got %d, want %d", response.ID, requestID)
	}
	return response, nil
}

func applyClientHeaders(req *http.Request, headers map[string]string) {
	for key, value := range headers {
		if !isReservedHeader(key) {
			req.Header.Set(key, value)
		}
	}
}
