package tasks

import (
	"context"
	"errors"
	"io"
	"testing"
)

// countingEndlessReader 生成长度为 remaining 的 'x' 流（没有换行），并记录
// 实际被读走的字节数。用来证明读取方在超限后立刻收手，而不是把整条流读完。
type countingEndlessReader struct {
	remaining int
	read      int
}

func (r *countingEndlessReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	for i := 0; i < n; i++ {
		p[i] = 'x'
	}
	r.remaining -= n
	r.read += n
	return n, nil
}

// TestReadSSECapsUnterminatedLine 冻结 P1-8 的第四处读取：提交事件流的分帧
// 读取也必须给单行设上限。旧实现用 bufio.ReadString 一路读到 EOF，一条没有
// 换行的 4 MiB 响应会被整条读进内存，最终报成 errSSEIncomplete。
func TestReadSSECapsUnterminatedLine(t *testing.T) {
	r := &countingEndlessReader{remaining: 4 << 20}
	err := readSSE(context.Background(), r, func(SSEEvent) error { return nil })
	if !errors.Is(err, ErrPluginHook) {
		t.Fatalf("err = %v, want ErrPluginHook（行超限必须硬失败）", err)
	}
	if errors.Is(err, errSSEIncomplete) {
		t.Fatalf("行超限不得报成 errSSEIncomplete")
	}
	if r.read > sseLineBytes+64<<10 {
		t.Fatalf("read %d bytes for a %d-byte line cap; reader must stop early",
			r.read, sseLineBytes)
	}
}
