package rag

import (
	"regexp"
	"strings"
)

// Splitter 文档分块器。
// 分块策略直接影响检索质量：太小丢上下文、太大稀释相关性、
// 跨标题切断破坏语义。本接口把策略交给实现，调用方按文档类型选择。
type Splitter interface {
	Split(text string) []Chunk
}

// TextSplitter 按 Markdown 标题分段、超长段按字符再切的通用分块器。
// 吸收自 mdk 的 SplitChunks：先按标题切大段，每段超过 ChunkSize 时
// 再按字符（rune）切，避免切断多字节字符。
type TextSplitter struct {
	// ChunkSize 单块最大字符数（rune）。0 用默认 900。
	ChunkSize int
	// Heading 标题正则；nil 用 Markdown # 默认。
	Heading *regexp.Regexp
}

// DefaultChunkSize 知识库场景的常用块大小（约 600-1200 字符）。
const DefaultChunkSize = 900

var defaultHeading = regexp.MustCompile(`(?m)^(\#{1,6})\s+(.+?)\s*$`)

// Split 实现 Splitter。
func (s TextSplitter) Split(text string) []Chunk {
	size := s.ChunkSize
	if size <= 0 {
		size = DefaultChunkSize
	}
	heading := s.Heading
	if heading == nil {
		heading = defaultHeading
	}
	lines := strings.Split(text, "\n")

	type section struct {
		title string
		start int
	}
	var sections []section
	for i, ln := range lines {
		if m := heading.FindStringSubmatch(ln); m != nil {
			sections = append(sections, section{strings.TrimSpace(m[2]), i})
		}
	}

	if len(sections) == 0 {
		body := strings.TrimSpace(text)
		if body == "" {
			return nil
		}
		return chunksFromBody(body, "正文", size, len(lines))
	}

	var out []Chunk
	// 标题前的前言
	if sections[0].start > 0 {
		if body := strings.TrimSpace(strings.Join(lines[:sections[0].start], "\n")); body != "" {
			out = append(out, chunksFromBody(body, "前言", size, sections[0].start)...)
		}
	}
	// 各标题段
	for i, sec := range sections {
		end := len(lines)
		if i+1 < len(sections) {
			end = sections[i+1].start
		}
		body := strings.TrimSpace(strings.Join(lines[sec.start:end], "\n"))
		if body == "" {
			continue
		}
		out = append(out, chunksFromBody(body, sec.title, size, end-1)...)
	}
	return out
}

// chunksFromBody 把一段正文按 ChunkSize 切成多块，统一标注 Section。
func chunksFromBody(body, section string, size, endLine int) []Chunk {
	parts := splitLongBody(body, size)
	out := make([]Chunk, 0, len(parts))
	for i, part := range parts {
		out = append(out, Chunk{
			Section: section, Seq: i, Content: part,
			StartLine: 0, EndLine: endLine,
		})
	}
	return out
}

// splitLongBody 按字符（rune）切，不切断多字节字符；
// 优先在段落/句子边界切，找不到再硬切。
func splitLongBody(body string, size int) []string {
	if size <= 0 {
		size = DefaultChunkSize
	}
	runes := []rune(body)
	if len(runes) <= size {
		return []string{body}
	}
	var parts []string
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		// 尽量在换行处切，避免硬切句子中间
		if end < len(runes) {
			if j := lastIndexRune(runes[i:end], '\n'); j >= size/2 {
				end = i + j + 1
			}
		}
		parts = append(parts, strings.TrimSpace(string(runes[i:end])))
	}
	return parts
}

func lastIndexRune(runes []rune, target rune) int {
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == target {
			return i
		}
	}
	return -1
}
