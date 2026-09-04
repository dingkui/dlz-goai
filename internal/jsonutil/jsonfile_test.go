package jsonutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type fixture struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

// LoadFile 对不存在的文件返回 ErrNotFound 而不是原生 os 错误，
// 调用方据此区分"还没有配置"与"配置损坏"。
func TestLoadFileNotFound(t *testing.T) {
	var v fixture
	err := LoadFile(filepath.Join(t.TempDir(), "missing.json"), &v)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("应返回 ErrNotFound, got %v", err)
	}
}

// SaveFile 原子写入：写完后目录里不应残留临时文件。
func TestSaveFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	if err := SaveFile(path, fixture{Name: "a", N: 1}); err != nil {
		t.Fatal(err)
	}
	var v fixture
	if err := LoadFile(path, &v); err != nil {
		t.Fatal(err)
	}
	if v.Name != "a" || v.N != 1 {
		t.Fatalf("往返数据不符: %+v", v)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("临时文件应被清理: %s", e.Name())
		}
	}
}

// Repair 抢救被截断的 JSON。
func TestRepairTruncated(t *testing.T) {
	var v fixture
	if err := Repair([]byte(`{"name":"a","n":7`), &v); err != nil {
		t.Fatalf("应能修复: %v", err)
	}
	if v.Name != "a" {
		t.Fatalf("修复结果不符: %+v", v)
	}
	var bad fixture
	if err := Repair([]byte(`not json at all`), &bad); err == nil {
		t.Fatal("无法修复的内容应报错")
	}
}
