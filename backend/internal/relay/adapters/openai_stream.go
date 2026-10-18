package adapters

import (
	"bufio"
	"encoding/json"
	"io"
	"iter"
	"strings"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/ir"
)

// sseInitialBuffer 是逐行读取的初始缓冲（relay-03 §4.1：初始 64KB）。
// 使用 bufio.Reader.ReadString 而非 Scanner：行会按需增长，不受 Scanner
// 默认 64KB 上限约束（relay-03 §4.1 的 64MB 上限属框架层配置，本适配器
// 自带帧解析时不做截断）。
const sseInitialBuffer = 64 * 1024

// openAIStreamChunk 是 OpenAI SSE 数据块的解码形状。
// 同时覆盖 chat.completion.chunk 的 delta 字段与 include_usage 的末块 usage。
type openAIStreamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content string `json:"content"`
			// DeepSeek 等兼容上游用 reasoning_content；部分上游用 reasoning。
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// StreamEvents 逐帧解析 OpenAI SSE 流（contract.StreamAdapter）。
//
// 帧解析是自包含的（不依赖 internal/relay/stream，后者由并行任务实现）：
//   - 按 SSE 规范先以空行切分事件、再合并同一事件的多个 data: 行
//     （relay-03 §4.2 甲类修正，基线逐行扫描会误解析多行事件）；
//   - 忽略 event:/id:/retry: 与注释行（": ping"）；`[DONE]` 终止迭代；
//   - 每块产出 delta（文本/推理/工具增量）、finish、usage 事件；
//     usage 事件携带归一化后的 contract.Usage（relay-03 §6 row 1）。
//
// 读取错误以 EventError 交付（不 panic）；错误细节不携带 URL/凭据（GC-6）。
// 迭代器一次性消费同一 reader（relay-02 §4.2）。
func (o *OpenAI) StreamEvents(_ *contract.RelayContext, rw io.ReadWriter) iter.Seq[contract.StreamEvent] {
	return func(yield func(contract.StreamEvent) bool) {
		if rw == nil {
			yield(contract.StreamEvent{Kind: contract.EventError})
			return
		}
		reader := bufio.NewReaderSize(rw, sseInitialBuffer)
		var dataLines []string
		finishSent := false
		model := ""

		// dispatch 处理一个已聚合的 SSE 事件。
		// 返回值 keep 表示迭代可继续；stop 表示收到了 [DONE]。
		dispatch := func() (keep, stop bool) {
			if len(dataLines) == 0 {
				return true, false
			}
			payload := strings.Join(dataLines, "\n")
			dataLines = dataLines[:0]

			if strings.TrimSpace(payload) == "[DONE]" {
				// 上游以 [DONE] 正常终止。若此前没有任何 finish_reason 块，
				// 补发一个 finish 事件，保证消费者总能观察到流终结（relay-03 §4.1）。
				if !finishSent {
					finishSent = true
					if !yield(contract.StreamEvent{Kind: contract.EventFinish, Model: model}) {
						return false, true
					}
				}
				return false, true
			}

			var chunk openAIStreamChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				// 单块 JSON 损坏按软错误处理：交出原始帧，流继续
				// （relay-03 §4.3：Error 为软错误，不中断；上限由框架层统计）。
				if !yield(contract.StreamEvent{Kind: contract.EventError, Raw: []byte(payload)}) {
					return false, true
				}
				return true, false
			}
			if chunk.Error != nil {
				// 上游错误事件：只交出原始帧（Raw），不构造可泄露敏感信息的消息串。
				if !yield(contract.StreamEvent{Kind: contract.EventError, Model: model, Raw: []byte(payload)}) {
					return false, true
				}
				return true, false
			}
			if chunk.Model != "" {
				model = chunk.Model
			}

			for _, choice := range chunk.Choices {
				if choice.Delta.Content != "" {
					if !yield(contract.StreamEvent{
						Kind:  contract.EventDelta,
						Delta: choice.Delta.Content,
						Model: model,
						Raw:   []byte(payload),
					}) {
						return false, false
					}
				}
				reasoning := choice.Delta.ReasoningContent
				if reasoning == "" {
					reasoning = choice.Delta.Reasoning
				}
				if reasoning != "" {
					// 推理增量走 StreamEvent.Reasoning 独立通道（契约 §2.4）。
					if !yield(contract.StreamEvent{
						Kind:      contract.EventDelta,
						Reasoning: reasoning,
						Model:     model,
						Raw:       []byte(payload),
					}) {
						return false, false
					}
				}
				for _, call := range choice.Delta.ToolCalls {
					delta := contract.ToolCallDelta{
						Index:             call.Index,
						ID:                call.ID,
						Name:              call.Function.Name,
						ArgumentsFragment: call.Function.Arguments,
					}
					if !yield(contract.StreamEvent{
						Kind:      contract.EventDelta,
						ToolDelta: &delta,
						Model:     model,
						Raw:       []byte(payload),
					}) {
						return false, false
					}
				}
				if choice.FinishReason != "" && !finishSent {
					finishSent = true
					if !yield(contract.StreamEvent{
						Kind:  contract.EventFinish,
						Model: model,
						Raw:   []byte(payload),
					}) {
						return false, false
					}
				}
			}

			// 含 include_usage 时，末块 choices 为空、只带 usage。
			if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
				usage := ir.ParseOpenAIUsage(chunk.Usage)
				if !yield(contract.StreamEvent{
					Kind:  contract.EventUsage,
					Usage: &usage,
					Model: model,
					Raw:   []byte(payload),
				}) {
					return false, false
				}
			}
			return true, false
		}

		for {
			line, readErr := reader.ReadString('\n')
			if len(line) > 0 {
				line = strings.TrimSuffix(line, "\n")
				line = strings.TrimSuffix(line, "\r")
				switch {
				case line == "":
					keep, stop := dispatch()
					if stop || !keep {
						return
					}
				case strings.HasPrefix(line, "data:"):
					dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				default:
					// event:/id:/retry: 字段与注释行（":" 开头）不承载 payload。
				}
			}
			if readErr != nil {
				if readErr == io.EOF {
					// 流未以裸 "\n\n" 结尾时也要处理最后一个事件。
					keep, _ := dispatch()
					if !keep {
						return
					}
					return
				}
				yield(contract.StreamEvent{Kind: contract.EventError})
				return
			}
		}
	}
}
