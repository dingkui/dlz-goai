package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/rag"
	"github.com/dingkui/dlz-goai/runtime"
	"github.com/dingkui/dlz-goai/storage/sqlite"
	"github.com/dingkui/dlz-goai/tool"
)

// rag VectorStore：存取检索往返 + 覆盖语义
func TestVectorStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rag.db")
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	store := sqlite.NewVectorStore(db)

	_ = store.Store(ctx, "d1", []rag.Chunk{
		{DocID: "d1", Section: "Go", Seq: 0, Content: "Go 并发"},
		{DocID: "d1", Section: "Go", Seq: 1, Content: "Go 内存"},
	}, [][]float32{{1, 0, 0}, {0, 1, 0}})
	_ = store.Store(ctx, "d2", []rag.Chunk{
		{DocID: "d2", Section: "DB", Seq: 0, Content: "数据库"},
	}, [][]float32{{0, 0, 1}})

	// 查询接近 d1 第一块
	hits, err := store.Search(ctx, []float32{1, 0.1, 0}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Chunk.Section != "Go" || hits[0].Chunk.Content != "Go 并发" {
		t.Fatalf("检索结果不符: %+v", hits)
	}

	// 同 docID 覆盖：d1 只剩新块
	_ = store.Store(ctx, "d1", []rag.Chunk{{DocID: "d1", Section: "Go", Seq: 0, Content: "新内容"}}, [][]float32{{1, 0, 0}})
	hits, _ = store.Search(ctx, []float32{1, 0, 0}, 10, nil)
	if len(hits) != 2 { // d1 新块 + d2
		t.Fatalf("覆盖后候选应为 2, got %d", len(hits))
	}

	// Remove
	_ = store.Remove(ctx, "d1")
	hits, _ = store.Search(ctx, []float32{1, 0, 0}, 10, nil)
	if len(hits) != 1 || hits[0].Chunk.DocID != "d2" {
		t.Fatalf("删除后应只剩 d2: %+v", hits)
	}
}

// runtime 三 Store 的持久往返
func TestRuntimeStoresPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rt.db")
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	runs := sqlite.NewRunStore(db)
	events := sqlite.NewEventStore(db)
	checkpoints := sqlite.NewCheckpointStore(db)

	// RunRecord
	record := runtime.RunRecord{
		ID: "r1", Status: runtime.StatusWaitingApproval, CreatedAt: 100, UpdatedAt: 200,
		Error: "", LastStep: 3,
		PendingApproval: &runtime.PendingApproval{CallID: "c1", ToolName: "write", Arguments: map[string]any{"k": "v"}},
	}
	if err := runs.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	got, err := runs.Get(ctx, "r1")
	if err != nil || got.Status != runtime.StatusWaitingApproval || got.LastStep != 3 ||
		got.PendingApproval == nil || got.PendingApproval.ToolName != "write" {
		t.Fatalf("RunRecord 往返不符: %+v err=%v", got, err)
	}
	got.Status = runtime.StatusSucceeded
	got.PendingApproval = nil
	if err := runs.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	pending, _ := runs.List(ctx, runtime.StatusRunning, runtime.StatusWaitingApproval)
	if len(pending) != 0 {
		t.Fatalf("终态不应出现在非终态列表: %+v", pending)
	}
	again, _ := runs.Get(ctx, "r1")
	if again.Status != runtime.StatusSucceeded || again.PendingApproval != nil {
		t.Fatalf("更新后不符: %+v", again)
	}
	if err := runs.Create(ctx, record); err == nil {
		t.Fatal("重复 Create 应报错")
	}

	// Events：批量写入 + 顺序回放
	want := []agent.Event{
		{Type: agent.EventRunStart, RunID: "r1"},
		{Type: agent.EventModelDelta, RunID: "r1", Content: "答"},
		{Type: agent.EventFinal, RunID: "r1", Content: "答"},
	}
	if err := events.Append(ctx, "r1", want...); err != nil {
		t.Fatal(err)
	}
	gotEvents, err := events.List(ctx, "r1")
	if err != nil || len(gotEvents) != 3 {
		t.Fatalf("回放数量不符: %d err=%v", len(gotEvents), err)
	}
	for i, e := range gotEvents {
		if e.Type != want[i].Type || e.Content != want[i].Content {
			t.Fatalf("回放顺序/内容不符: [%d] %+v", i, e)
		}
	}

	// Checkpoint：往返 + 不存在
	cp := runtime.Checkpoint{
		RunID: "r1", Step: 2,
		Messages: []message.Message{{Role: message.RoleUser, Content: "问"}, {Role: message.RoleAssistant, Content: "答"}},
		Options:  &message.Options{System: "sys", MaxTokens: 512},
	}
	if err := checkpoints.Save(ctx, cp); err != nil {
		t.Fatal(err)
	}
	loaded, err := checkpoints.Load(ctx, "r1")
	if err != nil || loaded.Step != 2 || len(loaded.Messages) != 2 ||
		loaded.Options == nil || loaded.Options.MaxTokens != 512 || loaded.Options.System != "sys" {
		t.Fatalf("检查点往返不符: %+v err=%v", loaded, err)
	}
	if _, err := checkpoints.Load(ctx, "missing"); err != runtime.ErrNoCheckpoint {
		t.Fatalf("应返回 ErrNoCheckpoint, got %v", err)
	}
}

// 端到端：runtime.Run 直接跑在 SQLite 存储上，进程内验证持久化闭环
func TestRuntimeOnSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e2e.db")
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rt := runtime.New(runtime.Options{
		Runs:        sqlite.NewRunStore(db),
		Events:      sqlite.NewEventStore(db),
		Checkpoints: sqlite.NewCheckpointStore(db),
	})
	runID := rt.BeginRun()

	echo := tool.NewFunc("echo", "", nil, true,
		func(_ context.Context, args map[string]any) (tool.Result, error) {
			return tool.Text("ok"), nil
		})
	step := 0
	model := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		step++
		if step == 1 {
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "c1", Name: "echo", Arguments: "{}"},
			}})
			return nil
		}
		emit(message.Delta{Content: "done"})
		return nil
	}
	result, err := rt.Run(context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil, agent.Config{RunID: runID, Tools: []tool.Tool{echo}}, model, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "done" {
		t.Fatalf("结果不符: %+v", result)
	}
	rec, _ := rt.Get(context.Background(), runID)
	if rec.Status != runtime.StatusSucceeded || rec.LastStep != 1 {
		t.Fatalf("登记不符: %+v", rec)
	}
	evs, _ := rt.Replay(context.Background(), runID)
	if len(evs) == 0 || evs[len(evs)-1].Type != agent.EventRunDone {
		t.Fatalf("事件回放不符: %d 条", len(evs))
	}
}
