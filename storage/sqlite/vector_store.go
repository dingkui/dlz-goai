package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/dingkui/dlz-goai/rag"
)

var _ rag.VectorStore = (*VectorStore)(nil)

// VectorStore rag.VectorStore 的 SQLite 实现。
// 向量以 little-endian BLOB 存储（rag.EncodeVec/DecodeVec）；
// 检索为全量载入 + 余弦暴力计算——万级块以内可用，
// 更大规模应换专用向量库或加 SQL 侧预过滤。
type VectorStore struct {
	db *sql.DB
}

// NewVectorStore 构造。db 通常来自 Open；多次构造共享同一文件没有问题。
func NewVectorStore(db *sql.DB) *VectorStore { return &VectorStore{db: db} }

// Store 存入文档块与向量（同 docID 全量覆盖，事务保证原子性）。
func (s *VectorStore) Store(ctx context.Context, docID string, chunks []rag.Chunk, vectors [][]float32) error {
	if len(chunks) != len(vectors) {
		return fmt.Errorf("sqlite: chunks(%d) 与 vectors(%d) 数量不一致", len(chunks), len(vectors))
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM rag_chunks WHERE doc_id = ?`, docID); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO rag_chunks
		(doc_id, seq, section, content, start_line, end_line, vector)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, chunk := range chunks {
		if _, err := stmt.ExecContext(ctx,
			docID, chunk.Seq, chunk.Section, chunk.Content,
			chunk.StartLine, chunk.EndLine, rag.EncodeVec(vectors[i])); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Remove 删除文档全部块。
func (s *VectorStore) Remove(ctx context.Context, docID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM rag_chunks WHERE doc_id = ?`, docID)
	return err
}

// Search 全量载入候选、余弦打分、取 topK。
// filter 在 Go 侧执行（接口是任意函数，无法下推到 SQL）。
func (s *VectorStore) Search(ctx context.Context, query []float32, topK int, filter rag.Filter) ([]rag.SearchResult, error) {
	if topK <= 0 {
		topK = 5
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT doc_id, seq, section, content, start_line, end_line, vector FROM rag_chunks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hits []rag.SearchResult
	for rows.Next() {
		var (
			chunk     rag.Chunk
			chunkSeq  int
			startLine int
			endLine   int
			blob      []byte
		)
		if err := rows.Scan(&chunk.DocID, &chunkSeq, &chunk.Section, &chunk.Content,
			&startLine, &endLine, &blob); err != nil {
			return nil, err
		}
		chunk.Seq = chunkSeq
		chunk.StartLine = startLine
		chunk.EndLine = endLine
		if filter != nil && !filter(chunk) {
			continue
		}
		hits = append(hits, rag.SearchResult{
			Chunk: chunk,
			Score: rag.Cosine(query, rag.DecodeVec(blob)),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Chunk.DocID < hits[j].Chunk.DocID ||
			(hits[i].Chunk.DocID == hits[j].Chunk.DocID && hits[i].Chunk.Seq < hits[j].Chunk.Seq)
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}
