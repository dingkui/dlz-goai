package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/tool"
)

// 并行模式下结果仍按调用顺序写回（tool 消息与调用一一对应），且确实并发执行。
func TestRunParallelToolExecution(t *testing.T) {
	var concurrent atomic.Int32
	var peak atomic.Int32
	slowTool := func(name string) tool.Tool {
		return tool.NewFunc(name, "", nil, true,
			func(ctx context.Context, args map[string]any) (tool.Result, error) {
				now := concurrent.Add(1)
				for {
					old := peak.Load()
					if now <= old || peak.CompareAndSwap(old, now) {
						break
					}
				}
				time.Sleep(50 * time.Millisecond)
				concurrent.Add(-1)
				return tool.Text(name + "-done"), nil
			})
	}
	tools := []tool.Tool{slowTool("t1"), slowTool("t2"), slowTool("t3")}

	step := 0
	model := func(ctx context.Context, messages []message.Message, options *message.Options, emit func(message.Delta)) error {
		step++
		if step > 1 {
			emit(message.Delta{Content: "全部完成"})
			return nil
		}
		emit(message.Delta{ToolCalls: []tool.CallDelta{
			{Index: 0, ID: "c1", Name: "t1", Arguments: "{}"},
			{Index: 1, ID: "c2", Name: "t2", Arguments: "{}"},
			{Index: 2, ID: "c3", Name: "t3", Arguments: "{}"},
		}})
		return nil
	}

	start := time.Now()
	result, err := New().Run(
		context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil,
		Config{Tools: tools, ToolExecution: ToolParallel},
		model, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	// 结果顺序必须与调用顺序一致
	if result.Messages[2].ToolCallID != "c1" || !contains(result.Messages[2].Content, "t1-done") {
		t.Fatalf("第 1 条回执顺序错乱: %+v", result.Messages[2])
	}
	if result.Messages[3].ToolCallID != "c2" || !contains(result.Messages[3].Content, "t2-done") {
		t.Fatalf("第 2 条回执顺序错乱: %+v", result.Messages[3])
	}
	if result.Messages[4].ToolCallID != "c3" || !contains(result.Messages[4].Content, "t3-done") {
		t.Fatalf("第 3 条回执顺序错乱: %+v", result.Messages[4])
	}
	// 三个 50ms 任务串行需 150ms+，并行应显著更快，且确实出现过并发
	if peak.Load() < 2 {
		t.Fatalf("未观察到并行执行, peak=%d", peak.Load())
	}
	if elapsed > 140*time.Millisecond {
		t.Fatalf("并行执行应快于串行, elapsed=%v", elapsed)
	}
}

// 串行（默认零值）行为回归：顺序执行，顺序写回。
func TestRunSequentialByDefault(t *testing.T) {
	var order []string
	var mu sync.Mutex
	tools := []tool.Tool{
		tool.NewFunc("t1", "", nil, true, func(context.Context, map[string]any) (tool.Result, error) {
			mu.Lock(); order = append(order, "t1"); mu.Unlock()
			return tool.Text("1"), nil
		}),
		tool.NewFunc("t2", "", nil, true, func(context.Context, map[string]any) (tool.Result, error) {
			mu.Lock(); order = append(order, "t2"); mu.Unlock()
			return tool.Text("2"), nil
		}),
	}
	step := 0
	model := func(ctx context.Context, messages []message.Message, options *message.Options, emit func(message.Delta)) error {
		step++
		if step > 1 {
			emit(message.Delta{Content: "done"})
			return nil
		}
		emit(message.Delta{ToolCalls: []tool.CallDelta{
			{Index: 0, ID: "c1", Name: "t2", Arguments: "{}"},
			{Index: 1, ID: "c2", Name: "t1", Arguments: "{}"},
		}})
		return nil
	}
	result, err := New().Run(context.Background(),
		[]message.Message{{Role: message.RoleUser, Content: "问"}},
		nil, Config{Tools: tools}, model, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "t2" || order[1] != "t1" {
		t.Fatalf("串行应按调用顺序执行: %v", order)
	}
	if result.Messages[2].ToolCallID != "c1" || result.Messages[3].ToolCallID != "c2" {
		t.Fatalf("回执顺序错乱: %+v", result.Messages)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 ||
		indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
