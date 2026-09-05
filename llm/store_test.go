package llm

import (
	"path/filepath"
	"testing"
)

func TestStoreMissingFileIsEmpty(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "nope.json"))
	if err := s.Load(); err != nil {
		t.Fatalf("文件不存在应容忍: %v", err)
	}
	if services, def := s.Snapshot(); len(services) != 0 || def != "" {
		t.Fatalf("应为空配置: %v %q", services, def)
	}
}

func TestStoreSaveLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llm.json")
	s := NewStore(path)
	s.Replace([]Service{
		{ID: "local", Kind: KindOllama, BaseURL: "http://127.0.0.1:11434", Models: []string{"qwen3"}, Enabled: true},
		{ID: "cloud", Kind: KindOpenAI, BaseURL: "https://api.deepseek.com", Models: []string{"a", "b"}, Enabled: false},
	}, "local")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	loaded := NewStore(path)
	if err := loaded.Load(); err != nil {
		t.Fatal(err)
	}
	services, def := loaded.Snapshot()
	if def != "local" || len(services) != 2 {
		t.Fatalf("往返不符: def=%q services=%+v", def, services)
	}
	if got, ok := loaded.Get("cloud"); !ok || got.Kind != KindOpenAI || len(got.Models) != 2 {
		t.Fatalf("Get 不符: %+v ok=%v", got, ok)
	}
	if _, ok := loaded.Get("missing"); ok {
		t.Fatal("不存在的 ID 应返回 false")
	}
}

// EnabledList 过滤未启用项；AllModels 跨服务去重并含默认模型。
func TestStoreListAndModels(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "x.json"))
	s.Replace([]Service{
		{ID: "a", Kind: KindOllama, Models: []string{"m1", "m2"}, DefaultModel: "m1", Enabled: true},
		{ID: "b", Kind: KindOpenAI, Models: []string{"m2", "m3"}, Enabled: true},
		{ID: "c", Kind: KindOpenAI, Models: []string{"m4"}, Enabled: false},
	}, "a")

	if enabled := s.EnabledList(); len(enabled) != 2 {
		t.Fatalf("EnabledList 应只含启用的 2 个: %+v", enabled)
	}
	models := s.AllModels()
	want := []string{"m1", "m2", "m3"}
	if len(models) != len(want) {
		t.Fatalf("AllModels 去重后不符: %v", models)
	}
	for i := range want {
		if models[i] != want[i] {
			t.Fatalf("AllModels 顺序/去重不符: %v", models)
		}
	}
}

// Replace 应深拷贝：修改入参切片不影响仓库内部状态。
func TestStoreReplaceClones(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "x.json"))
	input := []Service{{ID: "a", Models: []string{"m1"}}}
	s.Replace(input, "a")
	input[0].Models[0] = "mutated"
	if got, _ := s.Get("a"); got.Models[0] != "m1" {
		t.Fatalf("Replace 应隔离入参: %v", got.Models)
	}
}
