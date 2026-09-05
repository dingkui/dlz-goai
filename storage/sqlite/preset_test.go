package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/runtime"
	"github.com/dingkui/dlz-goai/storage/sqlite"
	"github.com/dingkui/dlz-goai/tool"
)

// OpenRuntime 开箱预设：一次打开即获得可跨进程恢复的 Runtime。
// 用两个独立 Runtime 实例共享同一 SQLite 文件模拟进程重启。
func TestOpenRuntimePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.db")

	// 第一个"进程"：跑一次带工具的两步运行
	db1, opts, err := sqlite.OpenRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	rt1 := runtime.New(opts)
	runID := rt1.BeginRun()

	step := 0
	model := func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
		step++
		if step == 1 {
			emit(message.Delta{ToolCalls: []tool.CallDelta{
				{Index: 0, ID: "c1", Name: "echo", Arguments: `{"q":"hi"}`},
			}})
			return nil
		}
		emit(message.Delta{Content: "完成"})
		return nil
	}
	echo := tool.NewFunc("echo", "回声", nil, true,
		func(_ context.Context, args map[string]any) (tool.Result, error) {
			return tool.Text("echo:" + args["q"].(string)), nil
		})
	if _, err := rt1.Run(context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil, agent.Config{RunID: runID, Tools: []tool.Tool{echo}}, model, nil); err != nil {
		t.Fatal(err)
	}
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}

	// 第二个"进程"：重新打开同一文件，运行登记与事件仍在
	db2, opts2, err := sqlite.OpenRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	rt2 := runtime.New(opts2)

	rec, err := rt2.Get(context.Background(), runID)
	if err != nil {
		t.Fatalf("重启后应能查到运行: %v", err)
	}
	if rec.Status != runtime.StatusSucceeded {
		t.Fatalf("重启后状态应为 succeeded，得到 %s", rec.Status)
	}
	events, err := rt2.Replay(context.Background(), runID)
	if err != nil || len(events) == 0 {
		t.Fatalf("重启后应能回放事件: %v len=%d", err, len(events))
	}
}
