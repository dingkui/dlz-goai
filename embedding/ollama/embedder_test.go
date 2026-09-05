package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// /api/embed 批量：embeddings 字段直接返回；请求体带模型名与 input 数组。
func TestEmbedBatch(t *testing.T) {
	var gotModel string
	var gotInput []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/embed") {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, gotInput = body.Model, body.Input
		_, _ = w.Write([]byte(`{"embeddings":[[0.1,0.2],[0.3,0.4]]}`))
	}))
	defer srv.Close()

	e := New(srv.URL, "nomic")
	vecs, err := e.EmbedBatch(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 || vecs[0][0] != 0.1 || vecs[1][1] != 0.4 {
		t.Fatalf("向量不符: %v", vecs)
	}
	if gotModel != "nomic" || len(gotInput) != 2 {
		t.Fatalf("请求体不符: model=%q input=%v", gotModel, gotInput)
	}
}

// 旧版 Ollama 响应：单条 embedding 字段兜底。
func TestEmbedLegacySingleField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"embedding":[0.5,0.6]}`))
	}))
	defer srv.Close()

	e := New(srv.URL, "m")
	vec, err := e.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 2 || vec[0] != 0.5 {
		t.Fatalf("向量不符: %v", vec)
	}
}

// 响应不含任何向量字段时报错而非返回空。
func TestEmbedNoVectors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	e := New(srv.URL, "m")
	if _, err := e.Embed(context.Background(), "hello"); err == nil {
		t.Fatal("空响应应报错")
	}
}
