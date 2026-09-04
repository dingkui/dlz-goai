package rag

import (
	"strings"
	"testing"
)

func TestTextSplitterByHeading(t *testing.T) {
	doc := "# 标题一\n内容A\n\n## 标题二\n内容B\n"
	chunks := TextSplitter{}.Split(doc)
	if len(chunks) != 2 {
		t.Fatalf("应切成 2 块（按标题）, got %d: %+v", len(chunks), chunks)
	}
	if chunks[0].Section != "标题一" || !strings.Contains(chunks[0].Content, "内容A") {
		t.Fatalf("第一块不符: %+v", chunks[0])
	}
	if chunks[1].Section != "标题二" || !strings.Contains(chunks[1].Content, "内容B") {
		t.Fatalf("第二块不符: %+v", chunks[1])
	}
	// 每段内独立编号，各只有一块所以 Seq 都为 0
	if chunks[0].Seq != 0 || chunks[1].Seq != 0 {
		t.Fatalf("块序不符: %+v", chunks)
	}
}

func TestTextSplitterLongBody(t *testing.T) {
	// 无标题的长正文，应按 ChunkSize 切多块
	body := strings.Repeat("中", 2000)
	chunks := TextSplitter{ChunkSize: 500}.Split(body)
	if len(chunks) < 3 {
		t.Fatalf("长正文应切多块, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.Section != "正文" {
			t.Fatalf("第 %d 块 Section 应为正文: %+v", i, c)
		}
		if c.Seq != i {
			t.Fatalf("第 %d 块 Seq 不符: %d", i, c.Seq)
		}
	}
	// 拼起来应覆盖全部内容（可能多空格但字符不丢）
	var total int
	for _, c := range chunks {
		total += len([]rune(c.Content))
	}
	if total < 1900 {
		t.Fatalf("切分后字符总量过少: %d", total)
	}
}

func TestTextSplitterEmpty(t *testing.T) {
	if got := (TextSplitter{}).Split(""); got != nil {
		t.Fatalf("空文本应返回 nil, got %+v", got)
	}
	if got := (TextSplitter{}).Split("   \n\n  "); got != nil {
		t.Fatalf("纯空白应返回 nil, got %+v", got)
	}
}

func TestTextSplitterPreface(t *testing.T) {
	// 标题前的前言应单独成块
	doc := "前言内容\n# 标题\n正文"
	chunks := TextSplitter{}.Split(doc)
	if len(chunks) != 2 {
		t.Fatalf("应切成前言+标题 2 块, got %d: %+v", len(chunks), chunks)
	}
	if chunks[0].Section != "前言" || !strings.Contains(chunks[0].Content, "前言内容") {
		t.Fatalf("前言块不符: %+v", chunks[0])
	}
}
