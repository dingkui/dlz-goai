package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ClientInfo：默认标识为 dlz-goai（不再泄漏任何宿主应用名），
// 宿主可通过 Client.Info / StdioClient.Info 覆盖。
func TestClientInfoIdentity(t *testing.T) {
	if info := DefaultClientInfo(); info.Name != "dlz-goai" || info.Version == "" {
		t.Fatalf("默认标识不符: %+v", info)
	}

	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			seen, _ = req.Params["clientInfo"].(map[string]any)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]any{"name": "fake", "version": "1"},
			},
		})
	}))
	defer srv.Close()

	// 默认：上报 dlz-goai
	c := NewClient(srv.URL, nil)
	if _, err := c.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if name, _ := seen["name"].(string); name != "dlz-goai" {
		t.Fatalf("默认应上报 dlz-goai, got %v", seen["name"])
	}

	// 覆盖：宿主产品名透传
	c2 := NewClient(srv.URL, nil)
	c2.Info = ClientInfo{Name: "MyApp", Version: "2.0"}
	if _, err := c2.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if name, _ := seen["name"].(string); name != "MyApp" {
		t.Fatalf("覆盖应生效, got %v", seen["name"])
	}
	if v, _ := seen["version"].(string); v != "2.0" {
		t.Fatalf("版本应透传, got %v", seen["version"])
	}
}
