// Package stream 实现上游 SSE 字节流与 contract.StreamEvent 之间的帧级解析与写出。
//
// 【为什么本包只产出 EventRaw、不解析 JSON（R-P2）】
// contract.StreamEvent 的 Delta/Reasoning/ToolDelta/Usage/Model 字段是**协议语义**：
// OpenAI 的 choices[].delta、Claude 的 content_block_delta、Gemini 的 candidates……
// 各自形状不同，且用量提取要遵循 relay-03 §6 的逐协议规格。这些解码属于适配器
// （relay-02 / Task 2）的职责；若在本包内置「猜 JSON 形状」的逻辑，每接一个新协议
// 都要改公共扫描器，正是 relay-03 §4.4 批评的「五种自研解析器各写一套」的反面。
// 因此本包只做协议无关的 SSE 规整：按空行切事件、跨行聚合 data、保序透传
// event/id/retry，产出一个完整帧的原始字节；适配器再对 Raw 做协议级 JSON 解码。
//
// 【与 relay-03 的对应】
//   - §3.1 响应头、§3.3 心跳载荷 → SetSSEHeaders / WriteSSE（EventPing）
//   - §4.1 行扫描与 64KB/64MB 缓冲上限 → ParseSSE
//   - §4.2 事件跨行聚合（甲类修正）→ ParseSSE 的 frame 聚合
package stream

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"newtg/gateway/internal/contract"
)

const (
	// relay-03 §4.1 冻结值：初始 64KB、上限 64MB（可配项 StreamScannerMaxBufferMB
	// 的默认口径）。签名 ParseSSE(r, evCh) 由 R-P2 冻结、不带配置参数，
	// 故上限在本包取设计默认值；需要按配置覆盖时由调用方换用 parseSSE。
	initialScanBufferBytes = 64 << 10
	maxScanBufferBytes     = 64 << 20

	// doneSentinel 是 OpenAI 约定的流终止标记（relay-03 §3.2：结束标记统一为
	// data: [DONE]）。它不是 SSE 规范的一部分，但上游普遍以此收尾。
	doneSentinel = "[DONE]"

	// pingFrame 是 relay-03 §3.3 冻结的 SSE 注释帧载荷（含末尾空行）。
	// 注释帧不会触发客户端事件，只用于穿透中间层保活。
	pingFrame = ": PING\n\n"
)

var (
	// ErrLineTooLong 表示上游单行超过扫描缓冲上限（relay-03 §4.1）。
	// 返回值同时包装 bufio.ErrTooLong，调用方可按任一哨兵归类为扫描错误。
	ErrLineTooLong = errors.New("stream: SSE 单行超过扫描缓冲上限")

	// ErrUnsupportedEventKind 表示调用方把协议语义事件直接交给了 WriteSSE。
	// 流式包不渲染 Delta/Usage 等事件：适配器必须先把它们渲染成 EventRaw 帧，
	// 否则写出层就得知道每个协议的事件格式（违背本包协议无关的边界，见包注释）。
	ErrUnsupportedEventKind = errors.New("stream: WriteSSE 仅接受 EventRaw/EventPing")
)

// ParseSSE 从 r 读取上游 SSE 字节流，把每个完整事件规整为一个
// contract.StreamEvent{Kind: EventRaw, Raw: 归一化帧} 投递到 evCh。
//
// 规整规则（R-P2 + relay-03 §4.2）：
//   - 以空行切分事件；同一事件的多条 data: 行按 SSE 规范以 "\n" 连接成载荷。
//     Raw 中仍逐行保留 data:，因为「单个 data: 字段里塞裸换行」不是合法 SSE 帧，
//     下游按行解析时会丢掉换行后的内容；按规范 join 各行 value 即得到合并载荷。
//   - 保留 event:/id:/retry:，帧内统一按 event→id→retry→data 顺序渲染，
//     保证输出确定性（SSE 规范中字段顺序无语义）。
//   - 忽略注释行（以 ":" 开头）与空 keep-alive；无 data 行的事件块不派发
//     （SSE 规范：data 缓冲为空不触发事件，也避免把 id/retry 元数据当事件解码）。
//   - data: [DONE] 立即终止并返回 nil，不产出该事件，也不再读取其后字节。
//   - EOF 时未以空行收尾的事件块不完整，直接丢弃。
//
// 通道纪律：本函数永不 close(evCh)，所有权归调用方；无 context 参数时
// 对 evCh 的发送是阻塞式的，调用方必须持续排空（R-P2）。
// 返回 nil 表示正常 EOF 或 [DONE]；扫描/读取错误按包装错误返回。
func ParseSSE(r io.Reader, evCh chan<- contract.StreamEvent) error {
	return parseSSE(r, evCh, initialScanBufferBytes, maxScanBufferBytes)
}

// parseSSE 是 ParseSSE 的可注入缓冲版本，仅为测试打小上限而存在。
func parseSSE(r io.Reader, evCh chan<- contract.StreamEvent, initialBuf, maxBuf int) error {
	sc := bufio.NewScanner(r)
	// Scanner.Buffer 的语义：初始缓冲 initialBuf，token（行）最大 maxBuf。
	// 注意最大值取 max(maxBuf, cap(buf))，因此 initialBuf <= maxBuf 时必须成立。
	if initialBuf <= 0 || maxBuf < initialBuf {
		return fmt.Errorf("stream: 非法扫描缓冲配置 initial=%d max=%d", initialBuf, maxBuf)
	}
	sc.Buffer(make([]byte, initialBuf), maxBuf)

	var frame sseFrame
	for sc.Scan() {
		line := sc.Text()

		if line == "" {
			// 空行 = 事件边界。只有累计到 data 行才派发（SSE 规范）。
			if frame.hasData {
				evCh <- contract.StreamEvent{Kind: contract.EventRaw, Raw: frame.bytes()}
			}
			frame.reset()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // SSE 注释 / 心跳，忽略
		}

		name, value, _ := strings.Cut(line, ":")
		// SSE 规范：冒号后紧跟的**一个**空格属于分隔符，不属于值。
		value = strings.TrimPrefix(value, " ")

		switch name {
		case "data":
			if strings.TrimSpace(value) == doneSentinel {
				// [DONE] 是终止哨兵而非事件：不派发，立即停止读取。
				// 若同一事件块此前还累积了半截 data，视为上游格式异常一并丢弃。
				return nil
			}
			frame.data = append(frame.data, value)
			frame.hasData = true
		case "event":
			frame.event, frame.hasEvent = value, true
		case "id":
			frame.id, frame.hasID = value, true
		case "retry":
			frame.retry, frame.hasRetry = value, true
		}
		// 其余字段按 SSE 规范忽略（本系统只透传上述四类）。
	}

	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			// 双 %w：调用方可用 ErrLineTooLong 或 bufio.ErrTooLong 归类
			// （relay-03 §4.1 要求超限可按 bufio.ErrTooLong 视为扫描错误）。
			return fmt.Errorf("%w: %w", ErrLineTooLong, err)
		}
		return fmt.Errorf("stream: 扫描 SSE 流失败: %w", err)
	}
	// EOF：frame 中若还有未以空行收尾的字段，按「不完整事件」丢弃。
	return nil
}

// sseFrame 聚合一个 SSE 事件块内被空行分隔的字段。
type sseFrame struct {
	event string
	id    string
	retry string
	data  []string

	hasEvent bool
	hasID    bool
	hasRetry bool
	hasData  bool
}

func (f *sseFrame) reset() { *f = sseFrame{} }

// bytes 渲染归一化帧：event→id→retry→逐行 data，最后补一个空行。
// 逐行保留 data: 而非合并进单个 data: 字段的原因见 ParseSSE 注释。
func (f *sseFrame) bytes() []byte {
	var b bytes.Buffer
	if f.hasEvent {
		b.WriteString("event: ")
		b.WriteString(f.event)
		b.WriteByte('\n')
	}
	if f.hasID {
		b.WriteString("id: ")
		b.WriteString(f.id)
		b.WriteByte('\n')
	}
	if f.hasRetry {
		b.WriteString("retry: ")
		b.WriteString(f.retry)
		b.WriteByte('\n')
	}
	for _, d := range f.data {
		b.WriteString("data: ")
		b.WriteString(d)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.Bytes()
}

// SetSSEHeaders 设置 relay-03 §3.1 冻结的 SSE 响应头。
//
// 必须在 WriteHeader/首次 Write 之前调用；http.Header 本身幂等，重复调用无害。
// Transfer-Encoding 不在此设置：Go 的 net/http 会对流式响应自动使用 chunked，
// 手工设置会与 HTTP/2 冲突并在服务端触发告警（relay-03 §3.1 的 chunked 行属于
// 传输层行为，不是应用层要写的头）。
// Cache-Control 固定 no-cache（允许存储但每次重验证），不得改成 no-store——
// no-store 是认证/日志等 JSON 敏感响应的口径（security-09 §2.4），
// 用在 SSE 上会破坏事件流的增量语义（relay-03 §3.1 注）。
func SetSSEHeaders(h http.Header) {
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

// WriteSSE 消费 evCh 并把事件逐帧写给客户端，直到通道关闭。
//
// 透传语义（R-P2）：
//   - EventRaw：原样写出 Raw 字节（归一化已由 ParseSSE 或适配器完成），
//     因此上游的 event:/id:/retry: 与 data 内容逐字节到达客户端。
//   - EventPing：写出 relay-03 §3.3 冻结的 ": PING\n\n" 注释帧。
//   - 其他 Kind：返回 ErrUnsupportedEventKind。适配器必须先把 Delta/Usage 等
//     协议事件渲染成完整 SSE 帧再以 EventRaw 投递——否则本包就得理解每个协议
//     的帧格式，破坏协议无关边界（见包注释）。
//
// 每帧写出后若 ResponseWriter 实现 http.Flusher 则立即 Flush：不刷新时
// net/http 的缓冲会让事件攒批到达，破坏流式的逐事件语义（relay-03 §3.4 的
// 单写者约束由调用方用 writeMutex 保证，本函数只做顺序写出）。
func WriteSSE(w http.ResponseWriter, evCh <-chan contract.StreamEvent) error {
	flusher, _ := w.(http.Flusher)

	for ev := range evCh {
		var frame []byte
		switch ev.Kind {
		case contract.EventRaw:
			frame = ev.Raw
		case contract.EventPing:
			frame = []byte(pingFrame)
		default:
			return fmt.Errorf("%w: kind=%d", ErrUnsupportedEventKind, ev.Kind)
		}

		n, err := w.Write(frame)
		if err != nil {
			return fmt.Errorf("stream: 写出 SSE 帧失败: %w", err)
		}
		if n != len(frame) {
			return fmt.Errorf("stream: 写出 SSE 帧不完整（%d/%d 字节）: %w", n, len(frame), io.ErrShortWrite)
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	return nil
}
