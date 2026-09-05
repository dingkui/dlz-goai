package sqlite

import (
	"database/sql"

	"github.com/dingkui/dlz-goai/runtime"
)

// OpenRuntime 打开（或创建）一个 SQLite 文件并装配 Runtime 的全部三个
// Store（运行登记、事件、检查点）——开箱即用的持久化恢复预设。
//
// 返回的 Options 可直接传给 runtime.New；db 由调用方持有并在进程退出前
// Close。三个 Store 共享同一连接（WAL 模式），不额外维护文件。
//
// 注意：默认的 runtime/memory 实现不支持跨进程恢复；需要"重启进程后
// Resume"这一核心能力时，用本函数（或自行 Open + Migrate 复用主库）。
func OpenRuntime(path string) (*sql.DB, runtime.Options, error) {
	db, err := Open(path)
	if err != nil {
		return nil, runtime.Options{}, err
	}
	return db, runtime.Options{
		Runs:        NewRunStore(db),
		Events:      NewEventStore(db),
		Checkpoints: NewCheckpointStore(db),
	}, nil
}
