package rag

import (
	"math"
	"testing"
)

func TestEncodeDecodeVecRoundTrip(t *testing.T) {
	v := []float32{0.1, -0.2, 0.3, 1.0, -0.999}
	got := DecodeVec(EncodeVec(v))
	if len(got) != len(v) {
		t.Fatalf("长度不符: %d", len(got))
	}
	for i := range v {
		if math.Abs(float64(got[i]-v[i])) > 1e-6 {
			t.Fatalf("第 %d 个值不符: %v vs %v", i, got[i], v[i])
		}
	}
}

func TestCosineBasic(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float32
	}{
		{"相同", []float32{1, 0, 0}, []float32{1, 0, 0}, 1},
		{"正交", []float32{1, 0, 0}, []float32{0, 1, 0}, 0},
		{"反向", []float32{1, 0}, []float32{-1, 0}, -1},
		{"维度不同", []float32{1, 0}, []float32{1, 0, 0}, 0},
		{"零向量", []float32{0, 0}, []float32{1, 1}, 0},
	}
	for _, c := range cases {
		got := Cosine(c.a, c.b)
		if math.Abs(float64(got-c.want)) > 1e-5 {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
