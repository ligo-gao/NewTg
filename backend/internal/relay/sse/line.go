// Package sse 提供中继侧所有 SSE 解析器共用的有界行读取。
//
// 原先三处解析器（OpenAI、Anthropic/Gemini 共用帧、多模态透传）都调用
// bufio.Reader.ReadString('\n')，把整行读进内存之后才检查累计字节数：单条
// 不带换行的流可以让内存按上游长度增长，网关 OOM（P1-8）。上限必须在读取
// 过程中生效，所以集中在这里，避免三处各自漂移。
package sse

import (
	"bufio"
	"errors"
)

// ErrLineTooLong 表示一行超过了调用方给出的字节预算。调用方必须按协议级
// 硬错误处理（终止流、不伪造 [DONE]），不能截断后继续解析。
var ErrLineTooLong = errors.New("sse: line exceeds the byte limit")

// ReadLine 读取一行（含行尾 '\n'），行内容累计超过 limit 时立即返回
// ErrLineTooLong，绝不把超限的行整条读进内存。
//
// limit 只数行内容，不数行尾 '\n'：调用方通常把预算用到刚好（例如
// `maxEventBytes - consumed`），若把结束符算进去，预算归零时连事件之间那个
// 空行都会被判超限。CRLF 的 '\r' 计为内容，与调用方读取后 trim '\r' 再
// `len+1` 的记法一致。返回的字符串只在非 ErrLineTooLong 的错误下有意义
// （例如 EOF 前的最后一行）；超限时返回空串。
func ReadLine(reader *bufio.Reader, limit int) (string, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		content := len(line) + len(fragment)
		if n := len(fragment); n > 0 && fragment[n-1] == '\n' {
			content--
		}
		if content > limit {
			return "", ErrLineTooLong
		}
		line = append(line, fragment...)
		if err == nil {
			return string(line), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return string(line), err
		}
	}
}
