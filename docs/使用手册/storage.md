# 使用手册：storage — 存储实现

`storage` 提供 `rag` 与 `runtime` 接口的具体实现。两个实现、两种取舍：

| 实现 | 适用 | 依赖 |
|---|---|---|
| `storage/memory` | 开发调试、单元测试、不需要跨进程恢复 | 无（纯内存） |
| `storage/sqlite` | 持久化：断点续跑、事件回放、向量检索落库 | modernc.org/sqlite（纯 Go，无 CGO，本库唯一第三方依赖） |

不 import storage/sqlite 就不会引入该依赖——核心包保持零第三方依赖。

## runtime 三 Store

| 接口 | 内存实现 | SQLite 实现 |
|---|---|---|
| `runtime.RunStore` | `memory.NewRunStore()` | `sqlite.NewRunStore(db)` |
| `runtime.EventStore` | `memory.NewEventStore()` | `sqlite.NewEventStore(db)` |
| `runtime.CheckpointStore` | `memory.NewCheckpointStore()` | `sqlite.NewCheckpointStore(db)` |

### 开箱预设：一行装配

```go
db, opts, err := sqlite.OpenRuntime("runs.db")
if err != nil { log.Fatal(err) }
defer db.Close()

rt := runtime.New(opts) // 三个 Store 已装配好
```

`OpenRuntime` 打开（或创建）SQLite 文件、设置 WAL 与 busy_timeout、自动建表，并返回装配好的 `runtime.Options`。**默认的内存实现不跨进程**——需要"重启后 Resume"这一能力时用它。

### 复用宿主应用的主库

不想要单独的文件时，在已有连接上做迁移：

```go
db, err := sql.Open("sqlite", "app.db") // 宿主自己的连接
// ... 应用自己的表 ...
if err := sqlite.Migrate(db); err != nil { log.Fatal(err) } // 只建 dlz-goai 的表，幂等
rt := runtime.New(runtime.Options{
	Runs:        sqlite.NewRunStore(db),
	Events:      sqlite.NewEventStore(db),
	Checkpoints: sqlite.NewCheckpointStore(db),
})
```

WAL 模式 + busy_timeout(5s)：同一文件可被 rag 与 runtime 的 Store 共享，读写并发安全。

## rag VectorStore

```go
// 内存（暴力余弦，开发/测试用）
store := memstore.NewVectorStore()

// SQLite（向量以 LittleEndian float32 BLOB 落库）
store := sqlite.NewVectorStore(db)

// 写入：同 DocID 覆盖；chunks 与 vectors 等长一一对应（sqlite 实现校验并在事务内提交）
err := store.Store(ctx, docID, chunks, vectors)

// 检索：Score 降序；filter 为 nil 不过滤
hits, err := store.Search(ctx, queryVec, topK, func(c rag.Chunk) bool { ... })

err = store.Remove(ctx, docID) // 删除整篇
```

## 选型速查

- **临时运行 / 测试**：`runtime/memory` 全套，进程退出即消失；
- **需要断点续跑**：`sqlite.OpenRuntime(path)`；
- **已有 SQLite 主库**：`sqlite.Open`（新文件）或 `sqlite.Migrate(db)`（复用连接）后按需构造单个 Store；
- **知识库向量检索**：数据量在数万 chunk 内用 `sqlite.NewVectorStore`（暴力余弦）；更大规模实现 `rag.VectorStore` 接专用向量库；
- **多进程共享同一 SQLite 文件**：读写可以（WAL），但 runtime 的恢复模型是单进程的——多实例并发恢复同一 RunID 需外部协调，见 [runtime 手册](使用手册/runtime.md)。

## 实现自定义 Store

三个 runtime 接口的语义要求（实现前必读，契约测试可参考 `storage/sqlite` 的测试）：

- `RunStore.Update` 是**全量覆盖**：调用方先 Get 再改字段；ID 不存在时返回 `runtime.ErrRunNotFound`；
- `EventStore.Append` 是 **append-only**：顺序即到达顺序，`List` 按此顺序返回；运行不存在返回空切片而非错误；
- `CheckpointStore.Save` 同 RunID **覆盖**（只需最新可恢复点）；无检查点返回 `runtime.ErrNoCheckpoint`；
- 落库失败直接返回错误——runtime 是 fail-closed 语义，持久化错误会中止运行。
