package stream

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
)

// mustParseSSE 用容量足够的有缓冲通道跑完一次解析并收集全部事件。
// 测试侧负责 close（stream 包永不关闭调用方传入的通道，见 ParseSSE 注释）。
func mustParseSSE(t *testing.T, input string) []contract.StreamEvent {
	t.Helper()
	ch := make(chan contract.StreamEvent, 64)
	if err := ParseSSE(strings.NewReader(input), ch); err != nil {
		t.Fatalf("ParseSSE() error = %v, want nil", err)
	}
	close(ch)
	var events []contract.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

// rawFrames 断言全部事件都是 EventRaw 并取出帧字节。
func rawFrames(t *testing.T, events []contract.StreamEvent) []string {
	t.Helper()
	frames := make([]string, 0, len(events))
	for i, ev := range events {
		if ev.Kind != contract.EventRaw {
			t.Fatalf("events[%d].Kind = %v, want contract.EventRaw", i, ev.Kind)
		}
		if ev.Delta != "" || ev.Reasoning != "" || ev.Usage != nil || ev.ToolDelta != nil || ev.Model != "" {
			t.Fatalf("events[%d] 只应携带 Raw，实得 %+v", i, ev)
		}
		frames = append(frames, string(ev.Raw))
	}
	return frames
}

// relay-03 §4.2：同一事件的多个 data: 行按 SSE 规范以 "\n" 连接成一个载荷。
// 归一化帧保留逐行 data: 落线（在单个 data: 字段里塞裸换行不是合法 SSE 帧），
// 下游按规范 join 各行 value 即得到合并后的载荷。
func TestParseSSE_MultiLineDataMergedIntoOneEvent(t *testing.T) {
	input := "data: {\"choices\":[{\"delta\":\n" +
		"data: {\"content\":\"hi\"}}]}\n" +
		"\n"
	want := "data: {\"choices\":[{\"delta\":\ndata: {\"content\":\"hi\"}}]}\n\n"

	events := mustParseSSE(t, input)
	frames := rawFrames(t, events)
	if len(frames) != 1 {
		t.Fatalf("事件数 = %d, want 1（多行 data 必须聚合成一个事件）", len(frames))
	}
	if frames[0] != want {
		t.Fatalf("Raw = %q, want %q", frames[0], want)
	}

	// 按 SSE 规范 join data 行 value，验证「合并载荷」语义。
	merged := mergeFrameData(t, frames[0])
	if merged != "{\"choices\":[{\"delta\":\n{\"content\":\"hi\"}}]}" {
		t.Fatalf("合并载荷 = %q", merged)
	}
}

// mergeFrameData 是测试侧的极简 SSE data 行提取器（只为验证合并语义）。
func mergeFrameData(t *testing.T, frame string) string {
	t.Helper()
	var values []string
	for _, line := range strings.Split(frame, "\n") {
		if v, ok := strings.CutPrefix(line, "data: "); ok {
			values = append(values, v)
		}
	}
	return strings.Join(values, "\n")
}

func TestParseSSE_DoneTerminatesStream(t *testing.T) {
	input := "data: {\"n\":1}\n" +
		"\n" +
		"data: [DONE]\n" +
		"\n" +
		"data: {\"n\":2}\n" +
		"\n"

	events := mustParseSSE(t, input)
	frames := rawFrames(t, events)
	if len(frames) != 1 {
		t.Fatalf("事件数 = %d, want 1（[DONE] 之后的帧不得被读取）", len(frames))
	}
	if frames[0] != "data: {\"n\":1}\n\n" {
		t.Fatalf("Raw = %q", frames[0])
	}
}

func TestParseSSE_DoneFirstEmitsNothing(t *testing.T) {
	events := mustParseSSE(t, "data: [DONE]\n\n")
	if len(events) != 0 {
		t.Fatalf("事件数 = %d, want 0（[DONE] 只终止、不产出事件）", len(events))
	}
}

func TestParseSSE_CommentsAndKeepAlivesIgnored(t *testing.T) {
	input := ": PING\n" + // 心跳注释帧（relay-03 §3.3）
		"\n" + // 空 keep-alive
		": keep-alive\n" +
		"\n" +
		"data: x\n" +
		"\n" +
		":\n" // 纯注释行

	events := mustParseSSE(t, input)
	frames := rawFrames(t, events)
	if len(frames) != 1 {
		t.Fatalf("事件数 = %d, want 1（注释与空行不得产出事件）", len(frames))
	}
	if frames[0] != "data: x\n\n" {
		t.Fatalf("Raw = %q", frames[0])
	}
}

func TestParseSSE_CommentInsideEventDoesNotBreakAggregation(t *testing.T) {
	input := "data: a\n: 中间注释\ndata: b\n\n"
	frames := rawFrames(t, mustParseSSE(t, input))
	if len(frames) != 1 || frames[0] != "data: a\ndata: b\n\n" {
		t.Fatalf("frames = %q, want 一个含两行 data 的帧", frames)
	}
}

// R-P2：event/id/retry 必须保留；帧内字段统一按 event→id→retry→data 归一，
// 保证确定性（SSE 规范中字段顺序无语义）。
func TestParseSSE_PreservesEventIDRetry(t *testing.T) {
	input := "retry: 1500\nid: abc-1\nevent: message_delta\ndata: {}\n\n"
	want := "event: message_delta\nid: abc-1\nretry: 1500\ndata: {}\n\n"

	frames := rawFrames(t, mustParseSSE(t, input))
	if len(frames) != 1 {
		t.Fatalf("事件数 = %d, want 1", len(frames))
	}
	if frames[0] != want {
		t.Fatalf("Raw = %q, want %q", frames[0], want)
	}
}

func TestParseSSE_CRLFNormalized(t *testing.T) {
	input := "event: ping\r\ndata: x\r\n\r\n"
	frames := rawFrames(t, mustParseSSE(t, input))
	if len(frames) != 1 || frames[0] != "event: ping\ndata: x\n\n" {
		t.Fatalf("frames = %q, want CRLF 归一为 LF", frames)
	}
}

func TestParseSSE_DataWithoutSpaceAfterColon(t *testing.T) {
	frames := rawFrames(t, mustParseSSE(t, "data:{\"a\":1}\n\n"))
	if len(frames) != 1 || frames[0] != "data: {\"a\":1}\n\n" {
		t.Fatalf("frames = %q", frames)
	}
}

// chunkReader 每次只喂固定字节数，用于制造「一行被 chunk 边界切开」。
type chunkReader struct {
	data []byte
	size int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := r.size
	if n > len(r.data) {
		n = len(r.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestParseSSE_ChunkBoundarySplittingLine(t *testing.T) {
	long := strings.Repeat("x", 100<<10) // 超过 64KB 初始缓冲，强制 Scanner 扩容
	input := "event: message\nid: 9\ndata: " + long + "\n\n"
	want := "event: message\nid: 9\ndata: " + long + "\n\n"

	ch := make(chan contract.StreamEvent, 4)
	if err := ParseSSE(&chunkReader{data: []byte(input), size: 3}, ch); err != nil {
		t.Fatalf("ParseSSE() error = %v", err)
	}
	close(ch)
	var frames []string
	for ev := range ch {
		frames = append(frames, string(ev.Raw))
	}
	if len(frames) != 1 || frames[0] != want {
		t.Fatalf("chunk 边界解析结果错误：事件数 = %d", len(frames))
	}
}

// EOF 时未以空行收尾的事件块不完整，必须丢弃（R-P2）。
func TestParseSSE_IncompleteTrailingEventDiscarded(t *testing.T) {
	frames := rawFrames(t, mustParseSSE(t, "data: a\n\ndata: b\n"))
	if len(frames) != 1 || frames[0] != "data: a\n\n" {
		t.Fatalf("frames = %q, want 只保留以空行收尾的事件", frames)
	}

	if events := mustParseSSE(t, "event: x\ndata: y"); len(events) != 0 {
		t.Fatalf("事件数 = %d, want 0（无空行收尾）", len(events))
	}
}

func TestParseSSE_EmptyInput(t *testing.T) {
	if events := mustParseSSE(t, ""); len(events) != 0 {
		t.Fatalf("事件数 = %d, want 0", len(events))
	}
}

// 事件块内没有 data 行时不派发（SSE 规范：data 缓冲为空则不触发事件），
// 避免把只有 id/retry 的元数据当成可解析事件投给适配器。
func TestParseSSE_FieldsWithoutDataNotDispatched(t *testing.T) {
	if events := mustParseSSE(t, "id: 7\nretry: 100\n\n"); len(events) != 0 {
		t.Fatalf("事件数 = %d, want 0", len(events))
	}
}

func TestParseSSE_LineTooLongReturnsWrappingError(t *testing.T) {
	// 用可注入的缓冲参数直接打小上限，避免测试真分配 64MB。
	ch := make(chan contract.StreamEvent, 1)
	err := parseSSE(strings.NewReader("data: "+strings.Repeat("x", 200)+"\n\n"), ch, 16, 32)
	if err == nil {
		t.Fatal("ParseSSE() error = nil, want 超限错误")
	}
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("error = %v, want errors.Is(ErrLineTooLong)", err)
	}
	// relay-03 §4.1：超限必须可按 bufio.ErrTooLong 归类为扫描错误。
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("error = %v, want errors.Is(bufio.ErrTooLong)", err)
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func TestParseSSE_ReaderErrorWrapped(t *testing.T) {
	err := ParseSSE(errorReader{err: errors.New("boom")}, make(chan contract.StreamEvent, 1))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("ParseSSE() error = %v, want 包装底层读错误", err)
	}
}

// stream 包只借用通道，不拥有通道：解析结束后调用方仍可安全 close。
func TestParseSSE_DoesNotCloseChannel(t *testing.T) {
	ch := make(chan contract.StreamEvent, 4)
	if err := ParseSSE(strings.NewReader("data: a\n\n"), ch); err != nil {
		t.Fatalf("ParseSSE() error = %v", err)
	}
	close(ch) // 若 ParseSSE 自行 close，这里会 panic
	for range ch {
	}
}

// 无缓冲通道也必须逐事件送达（R-P2：无 ctx，阻塞式投递，不丢事件）。
func TestParseSSE_BlocksUntilReceiverDrains(t *testing.T) {
	ch := make(chan contract.StreamEvent)
	done := make(chan error, 1)
	go func() { done <- ParseSSE(strings.NewReader("data: a\n\ndata: b\n\n"), ch) }()

	var got []string
	for i := 0; i < 2; i++ {
		select {
		case ev := <-ch:
			got = append(got, string(ev.Raw))
		case <-time.After(time.Second):
			t.Fatal("等待事件超时：ParseSSE 未阻塞式投递")
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("ParseSSE() error = %v", err)
	}
	if len(got) != 2 || got[0] != "data: a\n\n" || got[1] != "data: b\n\n" {
		t.Fatalf("got = %q", got)
	}
}

// WriteSSE 对 EventRaw 逐字节透传（保留 event/id），对 EventPing 写固定注释帧。
func TestWriteSSE_RawPassthroughAndPing(t *testing.T) {
	rec := httptest.NewRecorder()
	ch := make(chan contract.StreamEvent, 2)
	ch <- contract.StreamEvent{Kind: contract.EventRaw, Raw: []byte("event: message\nid: 7\ndata: {}\n\n")}
	ch <- contract.StreamEvent{Kind: contract.EventPing}
	close(ch)

	if err := WriteSSE(rec, ch); err != nil {
		t.Fatalf("WriteSSE() error = %v", err)
	}
	want := "event: message\nid: 7\ndata: {}\n\n: PING\n\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if !rec.Flushed {
		t.Fatal("WriteSSE 未 Flush：客户端无法逐事件收到数据")
	}
}

// Delta/Usage 等事件由适配器渲染成 EventRaw 后再写出；stream 包不猜协议。
func TestWriteSSE_UnsupportedKindReturnsError(t *testing.T) {
	rec := httptest.NewRecorder()
	ch := make(chan contract.StreamEvent, 1)
	ch <- contract.StreamEvent{Kind: contract.EventDelta, Delta: "hi"}
	close(ch)

	err := WriteSSE(rec, ch)
	if !errors.Is(err, ErrUnsupportedEventKind) {
		t.Fatalf("WriteSSE() error = %v, want errors.Is(ErrUnsupportedEventKind)", err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want 空（不支持的事件不得写出半帧）", rec.Body.String())
	}
}

type failingResponseWriter struct{ header http.Header }

func (w *failingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *failingResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("write boom")
}

func (w *failingResponseWriter) WriteHeader(int) {}

func TestWriteSSE_WriteErrorWrapped(t *testing.T) {
	ch := make(chan contract.StreamEvent, 1)
	ch <- contract.StreamEvent{Kind: contract.EventPing}
	close(ch)

	err := WriteSSE(&failingResponseWriter{}, ch)
	if err == nil || !strings.Contains(err.Error(), "write boom") {
		t.Fatalf("WriteSSE() error = %v, want 包装写错误", err)
	}
}

// relay-03 §3.1：四个头必须精确设置；Transfer-Encoding 交给 net/http 自动处理。
func TestSetSSEHeaders(t *testing.T) {
	h := http.Header{}
	SetSSEHeaders(h)

	want := map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"Connection":        "keep-alive",
		"X-Accel-Buffering": "no",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if got := h.Get("Transfer-Encoding"); got != "" {
		t.Errorf("Transfer-Encoding = %q, want 空（不得手工设置，Go 自动分块）", got)
	}
}
