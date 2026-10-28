package sse

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestReadLineBoundaries 冻结上限语义：恰好等于上限成功、超一个字节即
// ErrLineTooLong，且超限时不消费整行（读取量有界）。
func TestReadLineBoundaries(t *testing.T) {
	reader := bufio.NewReaderSize(strings.NewReader("12345678\n"), 4)
	line, err := ReadLine(reader, 9)
	if err != nil || line != "12345678\n" {
		t.Fatalf("恰好等于上限应成功：line=%q err=%v", line, err)
	}

	// 4MB 惰性数据源：超限必须在读取过程中截停，而不是读完再判。
	long := &endlessReader{remaining: 4 << 20}
	line, err = ReadLine(bufio.NewReaderSize(long, 64*1024), 8)
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("超限 err = %v，期望 ErrLineTooLong", err)
	}
	if line != "" {
		t.Fatalf("超限必须返回空串，got %q", line)
	}
	if long.read > 64*1024 {
		t.Fatalf("为 8 字节上限读了 %d 字节：必须在分片内截停", long.read)
	}
}

// TestReadLineEOFWithoutNewline 冻结 EOF 前最后一行（无换行）的语义：
// 返回已读内容与 io.EOF，由调用方决定是否按最后一行处理。
func TestReadLineEOFWithoutNewline(t *testing.T) {
	reader := bufio.NewReaderSize(strings.NewReader("data: tail"), 64)
	line, err := ReadLine(reader, 64)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v，期望 io.EOF", err)
	}
	if line != "data: tail" {
		t.Fatalf("line = %q", line)
	}
}

type endlessReader struct {
	remaining int64
	read      int64
}

func (r *endlessReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = 'x'
	}
	r.remaining -= int64(len(p))
	r.read += int64(len(p))
	return len(p), nil
}
