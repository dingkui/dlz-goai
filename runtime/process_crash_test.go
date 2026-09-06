package runtime_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/message"
	rt "github.com/dingkui/dlz-goai/runtime"
	"github.com/dingkui/dlz-goai/storage/sqlite"
	"github.com/dingkui/dlz-goai/tool"
)

type crashCheckpoint struct {
	rt.CheckpointStore
	enabled bool
}

func (s crashCheckpoint) Save(ctx context.Context, cp rt.Checkpoint) error {
	if s.enabled && cp.Step > 0 {
		os.Exit(73)
	}
	return s.CheckpointStore.Save(ctx, cp)
}

type crashRunStore struct {
	rt.RunStore
	enabled bool
}

func (s crashRunStore) Update(ctx context.Context, r rt.RunRecord) error {
	if s.enabled && r.Status == rt.StatusSucceeded {
		os.Exit(73)
	}
	return s.RunStore.Update(ctx, r)
}

type crashNoRetry struct{ tool.Tool }

func (crashNoRetry) RetryPolicy() tool.RetryPolicy { return tool.RetryPolicyNoRetry }

// Both execution and recovery run in separate OS processes. os.Exit deliberately
// bypasses every defer, including SQLite Close and Runtime's terminal bookkeeping.
func TestHardProcessCrashRecovery(t *testing.T) {
	if os.Getenv("DLZ_CRASH_HELPER") == "1" {
		crashProcessHelper(t)
		return
	}
	for _, boundary := range []string{"approval", "side_effect", "checkpoint", "terminal_commit"} {
		t.Run(boundary, func(t *testing.T) {
			dir := t.TempDir()
			for _, phase := range []string{"crash", "resume"} {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHardProcessCrashRecovery$", "-test.count=1")
				cmd.Env = append(os.Environ(), "DLZ_CRASH_HELPER=1", "DLZ_CRASH_DIR="+dir, "DLZ_CRASH_BOUNDARY="+boundary, "DLZ_CRASH_PHASE="+phase)
				output, err := cmd.CombinedOutput()
				cancel()
				if phase == "crash" {
					if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 73 {
						t.Fatalf("helper did not crash at boundary: %v\n%s", err, output)
					}
				} else if err != nil {
					t.Fatalf("recovery process: %v\n%s", err, output)
				}
			}
			data, err := os.ReadFile(filepath.Join(dir, "effects"))
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "effect\n" {
				t.Fatalf("side effect count changed: %q", data)
			}
		})
	}
}

func crashProcessHelper(t *testing.T) {
	dir, boundary := os.Getenv("DLZ_CRASH_DIR"), os.Getenv("DLZ_CRASH_BOUNDARY")
	crashing := os.Getenv("DLZ_CRASH_PHASE") == "crash"
	db, err := sqlite.Open(filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runtime := rt.New(rt.Options{Runs: crashRunStore{sqlite.NewRunStore(db), crashing && boundary == "terminal_commit"}, Events: sqlite.NewEventStore(db), Checkpoints: crashCheckpoint{sqlite.NewCheckpointStore(db), crashing && boundary == "checkpoint"}})
	effect := crashNoRetry{tool.NewFunc("effect", "effect", nil, false, func(context.Context, map[string]any) (tool.Result, error) {
		f, err := os.OpenFile(filepath.Join(dir, "effects"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return tool.Result{}, err
		}
		_, err = f.WriteString("effect\n")
		if err == nil {
			err = f.Sync()
		}
		f.Close()
		if err != nil {
			return tool.Result{}, err
		}
		if crashing && boundary == "side_effect" {
			os.Exit(73)
		}
		return tool.Text("recorded"), nil
	})}
	model := func(_ context.Context, msgs []message.Message, _ *message.Options, emit func(message.Delta)) error {
		if msgs[len(msgs)-1].Role == message.RoleTool {
			if !crashing && boundary == "side_effect" && !strings.Contains(msgs[len(msgs)-1].Content, "NoRetry") {
				t.Errorf("uncertain side effect was not blocked: %+v", msgs)
			}
			emit(message.Delta{Content: "done"})
			return nil
		}
		emit(message.Delta{ToolCalls: []tool.CallDelta{{Index: 0, ID: "effect-call", Name: "effect", Arguments: "{}"}}})
		return nil
	}
	cfg := agent.Config{RunID: "crash-run", Tools: []tool.Tool{effect}}
	emit := func(e agent.Event) {
		if e.Type == agent.EventApprovalRequired && e.Approved == nil {
			if crashing && boundary == "approval" {
				os.Exit(73)
			}
			if err := runtime.Approve(e.RunID, e.CallID, true); err != nil {
				t.Error(err)
			}
		}
	}
	if crashing {
		_, err = runtime.Run(context.Background(), []message.Message{{Role: message.RoleUser, Content: "do it"}}, nil, cfg, model, emit)
		t.Fatalf("expected hard exit, got %v", err)
	}
	if n, err := runtime.Recover(context.Background()); err != nil || n != 1 {
		t.Fatalf("recover: %d %v", n, err)
	}
	result, err := runtime.Resume(context.Background(), cfg.RunID, cfg, model, emit)
	if err != nil || result.Content != "done" {
		t.Fatalf("resume: %+v %v", result, err)
	}
}
