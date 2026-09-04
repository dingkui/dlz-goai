package rag

import (
	"encoding/binary"
	"math"
)

// EncodeVec 把 float32 向量编码为 little-endian 字节序列。
// 用于向量持久化（存储为 BLOB），解码用 DecodeVec 还原。
// 字节长度 = len(v) * 4。
func EncodeVec(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// DecodeVec 是 EncodeVec 的逆操作。长度不是 4 的倍数时截断尾部。
func DecodeVec(b []byte) []float32 {
	n := len(b) / 4
	v := make([]float32, n)
	for i := 0; i < n; i++ {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// Cosine 余弦相似度。维度不一致或零向量时返回 0
// （而非 NaN 或报错——检索场景下"无相似性"比"错误"更合理）。
//
// 用 float64 累积避免大量 float32 相加的精度损失，
// 这是 mdk 实战踩过的坑。
func Cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}
