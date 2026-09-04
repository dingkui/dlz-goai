// Package sqlite 提供 rag 与 runtime 接口的 SQLite 持久实现。
//
// 本包是 dlz-goai 唯一引入第三方依赖的模块（modernc.org/sqlite，纯 Go 无 CGO）；
// 不 import 本包时，其余模块仍保持零第三方依赖。
//
// Open 用 WAL 模式与 busy_timeout 打开数据库并自动建表，
// 同一文件可同时被 rag 与 runtime 的 Store 共享。
package sqlite

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// schema 全部建表语句。向后兼容原则：只加表加列，不改已有列。
var schema = []string{
	`CREATE TABLE IF NOT EXISTS rag_chunks (
		doc_id     TEXT    NOT NULL,
		seq        INTEGER NOT NULL,
		section    TEXT    NOT NULL DEFAULT '',
		content    TEXT    NOT NULL DEFAULT '',
		start_line INTEGER NOT NULL DEFAULT 0,
		end_line   INTEGER NOT NULL DEFAULT 0,
		vector     BLOB    NOT NULL,
		PRIMARY KEY (doc_id, seq)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_rag_chunks_doc ON rag_chunks(doc_id)`,

	`CREATE TABLE IF NOT EXISTS rt_runs (
		id               TEXT PRIMARY KEY,
		status           TEXT    NOT NULL,
		created_at       INTEGER NOT NULL DEFAULT 0,
		updated_at       INTEGER NOT NULL DEFAULT 0,
		error            TEXT    NOT NULL DEFAULT '',
		pending_approval TEXT    NOT NULL DEFAULT '',
		last_step        INTEGER NOT NULL DEFAULT 0,
		model            TEXT    NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_rt_runs_status ON rt_runs(status)`,

	`CREATE TABLE IF NOT EXISTS rt_events (
		rowid_seq INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id    TEXT NOT NULL,
		payload   TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_rt_events_run ON rt_events(run_id)`,

	`CREATE TABLE IF NOT EXISTS rt_checkpoints (
		run_id     TEXT PRIMARY KEY,
		step       INTEGER NOT NULL DEFAULT 0,
		messages   TEXT    NOT NULL DEFAULT '[]',
		citations  TEXT    NOT NULL DEFAULT '[]',
		options    TEXT    NOT NULL DEFAULT '{}',
		created_at INTEGER NOT NULL DEFAULT 0
	)`,
}

// Open 打开（或创建）SQLite 数据库，设置 WAL 与 busy_timeout 并执行迁移。
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlite: 打开失败: %w", err)
	}
	// WAL：读写并发；busy_timeout：多连接写冲突时等待而非立即报错
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("sqlite: pragma 失败: %w", err)
		}
	}
	if err := Migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Migrate 在调用方已有的 SQLite 连接上创建 RAG/Runtime 表。
// 宿主应用可借此复用自己的主库，而无需额外打开数据库文件。
func Migrate(db *sql.DB) error {
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("sqlite: 迁移失败: %w", err)
		}
	}
	return nil
}
