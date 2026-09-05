package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 响应 data 乱序返回时按 index 还原输入顺序；鉴权头携带。
func TestEmbedBatchSortsByIndex(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		// index=1 在前、index=0 在后
		_, _ = w.Write([]byte(`{"data":[
			{"index":1,"embedding":[0.3,0.4]},
			{"index":0,"embedding":[0.1,0.2]}
		]}`))
	}))
	defer srv.Close()

	e := New(srv.URL, "sk-test", "text-embedding-3")
	vecs, err := e.EmbedBatch(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 || vecs[0][0] != 0.1 || vecs[1][0] != 0.3 {
		t.Fatalf("应按 index 排序还原: %v", vecs)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("鉴权头不符: %q", gotAuth)
	}
}

// Embed：单条取首向量；无向量时报错。
func TestEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.7]}]}`))
	}))
	defer srv.Close()

	e := New(srv.URL, "", "m")
	vec, err := e.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 1 || vec[0] != 0.7 {
		t.Fatalf("向量不符: %v", vec)
	}
}

func TestEmbedHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "quota exceeded", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	e := New(srv.URL, "", "m")
	if _, err := e.Embed(context.Background(), "hello"); err == nil {
		t.Fatal("HTTP 429 应报错")
	}
}
