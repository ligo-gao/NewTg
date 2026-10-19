package multimodal

import (
	"bufio"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/adapters"
)

// ============================================================================
// 实时端点骨架（relay-04 §0.1、relay-07 G-06）
//
// P0 只实现 DialUpstream：校验渠道、完成 WebSocket 握手并返回连接。
// 双向桥接/事件迭代属进阶工作（relay-07 §3），不在 M4 范围；因此 dispatcher
// 在成功建连后以 501 明确告知「桥接未实现」，绝不假装已转发。
// ============================================================================

const (
	realtimePath        = "/v1/realtime"
	realtimeHandshake   = 10 * time.Second
	realtimeDialTimeout = 10 * time.Second
)

var _ contract.RealtimeAdapter = (*endpointAdapter)(nil)

// DialUpstream 实现 contract.RealtimeAdapter（P0 骨架）。
//
// 协议：按 RFC 6455 发送 HTTP Upgrade 握手，校验 101 响应后返回底层连接。
// 不做帧解析——帧级读写留给进阶的 StreamEvents/双向桥接。
//
// GC-6：错误只含固定文案或状态码，绝不回显上游 URL/凭据。
func (a *endpointAdapter) DialUpstream(rc *contract.RelayContext) (contract.UpstreamConn, error) {
	if rc == nil {
		return nil, classified(contract.ErrChannelConfig, "multimodal: nil relay context")
	}
	base := strings.TrimSpace(rc.APIBase)
	if base == "" {
		base = a.defaultBase
	}
	if base == "" {
		return nil, classified(contract.ErrChannelConfig, "multimodal: channel requires a base URL")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return nil, classified(contract.ErrChannelConfig, "multimodal: invalid API base URL")
	}
	// http(s) → ws(s)；显式 ws/wss 直接使用。
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "http":
		scheme = "ws"
	case "https":
		scheme = "wss"
	case "ws", "wss":
	default:
		return nil, classified(contract.ErrChannelConfig, "multimodal: unsupported realtime scheme")
	}
	host := parsed.Host
	if parsed.Port() == "" {
		if scheme == "wss" {
			host = net.JoinHostPort(parsed.Hostname(), "443")
		} else {
			host = net.JoinHostPort(parsed.Hostname(), "80")
		}
	}

	dialer := &net.Dialer{Timeout: realtimeDialTimeout}
	var conn net.Conn
	if scheme == "wss" {
		conn, err = tls.DialWithDialer(dialer, "tcp", host, &tls.Config{ServerName: parsed.Hostname(), MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.Dial("tcp", host)
	}
	if err != nil {
		return nil, classified(contract.ErrRetryable, "multimodal: realtime upstream dial failed")
	}
	// 握手阶段的失败必须关闭连接，避免泄漏 socket。
	fail := func(err error) (contract.UpstreamConn, error) {
		_ = conn.Close()
		return nil, err
	}

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return fail(classified(contract.ErrChannelConfig, "multimodal: websocket key generation failed"))
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	path := parsed.Path
	path = strings.TrimRight(path, "/") + realtimePath
	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}

	_ = conn.SetDeadline(time.Now().Add(realtimeHandshake))
	var request strings.Builder
	fmt.Fprintf(&request, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&request, "Host: %s\r\n", parsed.Host)
	request.WriteString("Upgrade: websocket\r\n")
	request.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&request, "Sec-WebSocket-Key: %s\r\n", key)
	request.WriteString("Sec-WebSocket-Version: 13\r\n")
	if rc.Credential != "" {
		fmt.Fprintf(&request, "Authorization: Bearer %s\r\n", rc.Credential)
	}
	request.WriteString("\r\n")
	if _, err := conn.Write([]byte(request.String())); err != nil {
		return fail(classified(contract.ErrRetryable, "multimodal: realtime handshake write failed"))
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	if err != nil {
		return fail(classified(contract.ErrRetryable, "multimodal: realtime handshake read failed"))
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		class := a.ClassifyError(resp.StatusCode, nil)
		return fail(&adapters.Error{Class: class, Err: fmt.Errorf("multimodal: realtime upstream returned status %d", resp.StatusCode)})
	}
	// 握手成功后清除 deadline：长连接不应被握手超时切断。
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
