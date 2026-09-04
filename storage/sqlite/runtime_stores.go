package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dingkui/dlz-goai/agent"
	"github.com/dingkui/dlz-goai/runtime"
)

// --- RunStore ---

var _ runtime.RunStore = (*RunStore)(nil)

// RunStore runtime.RunStore 的 SQLite 实现。
type RunStore struct {
	db *sql.DB
}

// NewRunStore 构造。
func NewRunStore(db *sql.DB) *RunStore { return &RunStore{db: db} }

// Create 登记一次运行。ID 已存在时报错（防止重复发起）。
func (s *RunStore) Create(ctx context.Context, record runtime.RunRecord) error {
	pending, err := marshalPending(record.PendingApproval)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO rt_runs (id, status, created_at, updated_at, error, pending_approval, last_step, model)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, string(record.Status), record.CreatedAt, record.UpdatedAt,
		record.Error, pending, record.LastStep, record.Model)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("sqlite: 运行 %s 已存在", record.ID)
	}
	return nil
}

// Update 全量更新登记。
func (s *RunStore) Update(ctx context.Context, record runtime.RunRecord) error {
	pending, err := marshalPending(record.PendingApproval)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE rt_runs SET status = ?, updated_at = ?, error = ?, pending_approval = ?, last_step = ?, model = ?
		 WHERE id = ?`,
		string(record.Status), record.UpdatedAt, record.Error, pending, record.LastStep, record.Model, record.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return runtime.ErrRunNotFound
	}
	return nil
}

// Get 按 ID 查找。
func (s *RunStore) Get(ctx context.Context, runID string) (runtime.RunRecord, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, status, created_at, updated_at, error, pending_approval, last_step, model
		 FROM rt_runs WHERE id = ?`, runID)
	return scanRun(row)
}

// List 按状态过滤；不传状态返回全部，按 updated_at 降序。
func (s *RunStore) List(ctx context.Context, statuses ...runtime.Status) ([]runtime.RunRecord, error) {
	query := `SELECT id, status, created_at, updated_at, error, pending_approval, last_step, model FROM rt_runs`
	var args []any
	if len(statuses) > 0 {
		query += ` WHERE status IN (`
		for i, status := range statuses {
			if i > 0 {
				query += ","
			}
			query += "?"
			args = append(args, string(status))
		}
		query += `)`
	}
	query += ` ORDER BY updated_at DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []runtime.RunRecord
	for rows.Next() {
		record, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanRun(row rowScanner) (runtime.RunRecord, error) {
	var record runtime.RunRecord
	var status, pending string
	if err := row.Scan(&record.ID, &status, &record.CreatedAt, &record.UpdatedAt,
		&record.Error, &pending, &record.LastStep, &record.Model); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runtime.RunRecord{}, runtime.ErrRunNotFound
		}
		return runtime.RunRecord{}, err
	}
	record.Status = runtime.Status(status)
	if pending != "" {
		var pa runtime.PendingApproval
		if err := json.Unmarshal([]byte(pending), &pa); err == nil {
			record.PendingApproval = &pa
		}
	}
	return record, nil
}

func marshalPending(pa *runtime.PendingApproval) (string, error) {
	if pa == nil {
		return "", nil
	}
	b, err := json.Marshal(pa)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// --- EventStore ---

var _ runtime.EventStore = (*EventStore)(nil)

// EventStore runtime.EventStore 的 SQLite 实现：事件逐行 JSON 存储，
// List 按写入顺序（自增 rowid）回放。
type EventStore struct {
	db *sql.DB
}

// NewEventStore 构造。
func NewEventStore(db *sql.DB) *EventStore { return &EventStore{db: db} }

// Append 追加事件（单事务批量写入）。
func (s *EventStore) Append(ctx context.Context, runID string, events ...agent.Event) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO rt_events (run_id, payload) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range events {
		payload, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, runID, string(payload)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// List 回放运行的全部事件（按写入顺序）。
func (s *EventStore) List(ctx context.Context, runID string) ([]agent.Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT payload FROM rt_events WHERE run_id = ? ORDER BY rowid_seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []agent.Event
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var e agent.Event
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- CheckpointStore ---

var _ runtime.CheckpointStore = (*CheckpointStore)(nil)

// CheckpointStore runtime.CheckpointStore 的 SQLite 实现：
// 同一 run_id 覆盖保存，只保留最新可恢复点。
type CheckpointStore struct {
	db *sql.DB
}

// NewCheckpointStore 构造。
func NewCheckpointStore(db *sql.DB) *CheckpointStore { return &CheckpointStore{db: db} }

// Save 保存（覆盖）检查点。
func (s *CheckpointStore) Save(ctx context.Context, cp runtime.Checkpoint) error {
	messages, err := json.Marshal(cp.Messages)
	if err != nil {
		return err
	}
	citations, err := json.Marshal(cp.Citations)
	if err != nil {
		return err
	}
	options, err := json.Marshal(cp.Options)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO rt_checkpoints (run_id, step, messages, citations, options, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(run_id) DO UPDATE SET
		   step = excluded.step, messages = excluded.messages,
		   citations = excluded.citations, options = excluded.options,
		   created_at = excluded.created_at`,
		cp.RunID, cp.Step, string(messages), string(citations), string(options), cp.CreatedAt)
	return err
}

// Load 读取最新检查点。不存在时返回 runtime.ErrNoCheckpoint。
func (s *CheckpointStore) Load(ctx context.Context, runID string) (runtime.Checkpoint, error) {
	var (
		cp             runtime.Checkpoint
		step           int
		messages       string
		citations      string
		options        string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT run_id, step, messages, citations, options, created_at FROM rt_checkpoints WHERE run_id = ?`,
		runID).Scan(&cp.RunID, &step, &messages, &citations, &options, &cp.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runtime.Checkpoint{}, runtime.ErrNoCheckpoint
		}
		return runtime.Checkpoint{}, err
	}
	cp.Step = step
	if err := json.Unmarshal([]byte(messages), &cp.Messages); err != nil {
		return runtime.Checkpoint{}, err
	}
	if err := json.Unmarshal([]byte(citations), &cp.Citations); err != nil {
		return runtime.Checkpoint{}, err
	}
	if err := json.Unmarshal([]byte(options), &cp.Options); err != nil {
		return runtime.Checkpoint{}, err
	}
	return cp, nil
}
