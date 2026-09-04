package rag

import (
	"sort"
)

// RRF 倒数排名融合：把多路检索结果按排名融合成一份。
// score = Σ 1/(k + rank_i)，k 通常取 60（经验值，平衡头部与尾部权重）。
//
// 适用于不同检索器分数尺度不一致的场景（向量余弦 vs BM25 分数），
// 融合只看排名不看绝对分数，因此对尺度不敏感。
func RRF(rankings [][]SearchResult, k int) []SearchResult {
	if len(rankings) == 0 {
		return nil
	}
	if k <= 0 {
		k = 60
	}
	type accum struct {
		result SearchResult
		score  float64
	}
	merged := map[string]*accum{}

	for _, ranking := range rankings {
		for rank, hit := range ranking {
			key := chunkKey(hit.Chunk)
			a, ok := merged[key]
			if !ok {
				a = &accum{result: hit, score: 0}
				merged[key] = a
			}
			a.score += 1.0 / float64(k+rank+1)
			// 保留最高原始分作为 tie-breaker
			if hit.Score > a.result.Score {
				a.result.Score = hit.Score
			}
		}
	}

	out := make([]SearchResult, 0, len(merged))
	for _, a := range merged {
		a.result.Score = float32(a.score)
		out = append(out, a.result)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return chunkKey(out[i].Chunk) < chunkKey(out[j].Chunk)
	})
	return out
}

// chunkKey 去重键：同文档同块视为同一命中。
func chunkKey(c Chunk) string {
	return c.DocID + "\x00" + c.Section + "\x00" + itoa(c.Seq)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
