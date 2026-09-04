package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

func readRPCResponse(body io.Reader, contentType string, requestID int64) (JSONRPCResponse, error) {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return readSSERPCResponse(body, requestID)
	}
	data, err := readHTTPBody(body)
	if err != nil {
		return JSONRPCResponse{}, err
	}
	return decodeRPCResponse(data, requestID)
}

func readSSERPCResponse(body io.Reader, requestID int64) (JSONRPCResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), maxHTTPResponseBytes)
	data := make([]byte, 0, 4096)
	consume := func() (JSONRPCResponse, bool, error) {
		if len(data) == 0 {
			return JSONRPCResponse{}, false, nil
		}
		payload := bytes.TrimSpace(data)
		data = data[:0]
		if bytes.Equal(payload, []byte("[DONE]")) {
			return JSONRPCResponse{}, false, nil
		}
		var probe struct {
			ID     json.RawMessage `json:"id,omitempty"`
			Method string          `json:"method,omitempty"`
		}
		if err := json.Unmarshal(payload, &probe); err != nil || probe.Method != "" || len(probe.ID) == 0 {
			return JSONRPCResponse{}, false, nil
		}
		var id int64
		if json.Unmarshal(probe.ID, &id) != nil || id != requestID {
			return JSONRPCResponse{}, false, nil
		}
		response, err := parseRPCResponse(payload, requestID)
		return response, true, err
	}
	for scanner.Scan() {
		line := bytes.TrimSuffix(scanner.Bytes(), []byte("\r"))
		if len(line) == 0 {
			if response, matched, err := consume(); matched || err != nil {
				return response, err
			}
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		part := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data)+len(part)+1 > maxHTTPResponseBytes {
			return JSONRPCResponse{}, errors.New("MCP SSE 事件超过 16 MiB 限制")
		}
		if len(data) > 0 {
			data = append(data, '\n')
		}
		data = append(data, part...)
	}
	if err := scanner.Err(); err != nil {
		return JSONRPCResponse{}, err
	}
	if response, matched, err := consume(); matched || err != nil {
		return response, err
	}
	return JSONRPCResponse{}, fmt.Errorf("MCP SSE 响应中没有匹配请求 %d 的 JSON-RPC 结果", requestID)
}
