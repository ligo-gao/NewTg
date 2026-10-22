package dto

import (
	"os"
	"strings"
	"testing"
)

// 本文件是契约回归的**逐路径**护栏：生成物漂移检查只能证明「源与生成物一致」，
// 证明不了「源本身没有丢响应」——批量编辑时误删某个 GET 的 404 不会被 drift
// 抓到（源与 bundle 一起错）。这里直接对源文件断言。
//
// 只做最小文本扫描，不引入 YAML 依赖：路径块按缩进切分，GET 操作的响应段
// 以第一个同级操作为界。
func TestAdminSpecKeepsDocumentedNotFoundResponses(t *testing.T) {
	raw, err := os.ReadFile("../../../../api/openapi/paths/admin.yaml")
	if err != nil {
		t.Fatalf("read admin.yaml: %v", err)
	}
	spec := string(raw)

	for _, path := range []string{
		"/api/models/{id}",
		"/api/vendors/{id}",
		"/api/subscription-plans/{id}",
		"/api/announcements/{id}",
	} {
		block, ok := pathBlock(spec, path)
		if !ok {
			t.Fatalf("spec is missing path %s", path)
		}
		getOp, ok := operation(block, "get")
		if !ok {
			t.Fatalf("%s no longer declares a GET operation", path)
		}
		if !strings.Contains(getOp, "'404':") {
			t.Fatalf("%s GET must keep its 404 response (regression guard)", path)
		}
	}
}

// TestRelayErrorResponsesUseOpenAIEnvelope 是 R-W18-13 的错误信封守卫：
// /v1 与 /pg 的 400/401/402/403/404/500/503 必须引用 OpenAI 信封组件
// （RelayError*），不得回落到 /api 的 RFC 7807 组件；problem+json 在 /v1
// 只允许用于 501。`/v1/messages` 的 Claude 形信封归 W18-A，跳过。
func TestRelayErrorResponsesUseOpenAIEnvelope(t *testing.T) {
	raw, err := os.ReadFile("../../../../api/openapi/paths/relay.yaml")
	if err != nil {
		t.Fatalf("read relay.yaml: %v", err)
	}
	spec := string(raw)
	forbidden := []string{
		"#/responses/Error400'",
		"#/responses/Error401'",
		"#/responses/Error402'",
		"#/responses/Error403'",
		"#/responses/Error404'",
		"#/responses/Error500'",
		"#/responses/Error503'",
	}
	for _, path := range relayPathKeys(spec) {
		if !strings.HasPrefix(path, "/v1/") && !strings.HasPrefix(path, "/pg/") {
			continue
		}
		if path == "/v1/messages" {
			continue
		}
		block, ok := pathBlock(spec, path)
		if !ok {
			continue
		}
		for _, method := range []string{"get", "post", "put", "patch", "delete"} {
			op, ok := operation(block, method)
			if !ok {
				continue
			}
			for _, forbiddenRef := range forbidden {
				if strings.Contains(op, forbiddenRef) {
					t.Errorf("%s %s: must use RelayError* for 4xx/5xx, found %s", strings.ToUpper(method), path, forbiddenRef)
				}
			}
			if strings.Contains(op, "'501':") {
				// 501 是「端点形态不支持」的唯一 problem+json 例外；但
				// /v1/responses 是 OpenAI 协议面（W18-C），501 也必须用
				// OpenAI 信封 RelayError501，不能回落到 problem+json。
				want := "#/responses/Error501'"
				if path == "/v1/responses" {
					want = "#/responses/RelayError501'"
				}
				if !strings.Contains(op, want) {
					t.Errorf("%s %s: 501 must reference %s", strings.ToUpper(method), path, want)
				}
			}
		}
	}
}

// relayPathKeys 返回 relay.yaml 中所有 2 空格缩进的路径键（`  /x/y:`）。
func relayPathKeys(spec string) []string {
	var out []string
	for _, line := range strings.Split(spec, "\n") {
		if !strings.HasPrefix(line, "  /") {
			continue
		}
		if len(line)-len(strings.TrimLeft(line, " ")) != 2 {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, ":") {
			out = append(out, strings.TrimSuffix(trimmed, ":"))
		}
	}
	return out
}

// TestRelayRateLimitContractUsesRelayEnvelope 是 W7/M5-c 的限流契约守卫。
//
// 为什么直接扫源文件：drift 检查只能证明「源与生成物一致」，不能证明「源本身
// 没有把中继 429 误写成 /api 的 problem+json」（源和 bundle 一起错时漂移检查
// 依然 PASS）。M5 的 429 由网关自己产生，形状必须与 writeRelayError 一致：
// {error:{message,type,code}} + Retry-After/X-Request-ID，不能回落到 RFC 7807。
//
// 同时反向守住 /api/*：管理面/公开面的 429 继续引用 Error429（problem+json），
// 两组组件不得互相污染。
func TestRelayRateLimitContractUsesRelayEnvelope(t *testing.T) {
	relayRaw, err := os.ReadFile("../../../../api/openapi/paths/relay.yaml")
	if err != nil {
		t.Fatalf("read relay.yaml: %v", err)
	}
	relaySpec := string(relayRaw)

	// 每一处会真正走到中继提交/准入接缝并返回 429 的 POST 都必须声明 relay
	// 组件：chat/messages 走 BuildRelayContext 限流；tasks/videos/responses
	// 由 handleSubmit 同一接缝限流；/pg 会话路径同样计入 B10/B12。
	for _, path := range []string{
		"/v1/chat/completions",
		"/pg/chat/completions",
		"/v1/tasks/{key}",
		"/v1/videos",
		"/v1/responses",
	} {
		block, ok := pathBlock(relaySpec, path)
		if !ok {
			t.Fatalf("spec is missing path %s", path)
		}
		postOp, ok := operation(block, "post")
		if !ok {
			t.Fatalf("%s no longer declares a POST operation", path)
		}
		if !strings.Contains(postOp, "$ref: '../components/responses.yaml#/responses/RelayError429'") {
			t.Fatalf("%s POST 429 must reference the relay envelope component RelayError429", path)
		}
		if strings.Contains(postOp, "#/responses/Error429'") {
			t.Fatalf("%s POST 429 must not fall back to the /api problem+json Error429", path)
		}
	}

	// /v1/messages 是 Anthropic 原生协议面（W18-A / R-W18-04）：429 必须与其它
	// 错误一样走 Claude 形信封，而不是 OpenAI 的 RelayError429。两条断言分开写，
	// 避免「把 messages 混进 RelayError429 列表」掩盖协议面分裂。
	messages, ok := pathBlock(relaySpec, "/v1/messages")
	if !ok {
		t.Fatal("spec is missing path /v1/messages")
	}
	messagesPost, ok := operation(messages, "post")
	if !ok {
		t.Fatal("/v1/messages no longer declares a POST operation")
	}
	if !strings.Contains(messagesPost, "$ref: '../components/responses.yaml#/responses/ClaudeError429'") {
		t.Fatal("/v1/messages POST 429 must reference the Claude envelope component ClaudeError429")
	}
	if strings.Contains(messagesPost, "#/responses/RelayError429'") {
		t.Fatal("/v1/messages POST 429 must not use the OpenAI RelayError429 envelope")
	}

	// /v1/batches 顶层**不声明** 429：每个子行各自计数，子行 429 被记为
	// request_counts.failed，父批次仍返回 200；声明 429 会误导客户端。
	batches, ok := pathBlock(relaySpec, "/v1/batches")
	if !ok {
		t.Fatal("spec is missing path /v1/batches")
	}
	batchesPost, ok := operation(batches, "post")
	if !ok {
		t.Fatal("/v1/batches no longer declares a POST operation")
	}
	if strings.Contains(batchesPost, "'429':") {
		t.Fatal("/v1/batches must not declare 429 (child limits surface as failed rows, not a batch-level 429)")
	}

	responsesRaw, err := os.ReadFile("../../../../api/openapi/components/responses.yaml")
	if err != nil {
		t.Fatalf("read responses.yaml: %v", err)
	}
	responsesSpec := string(responsesRaw)

	relay429, ok := componentBlock(responsesSpec, "RelayError429")
	if !ok {
		t.Fatal("responses.yaml is missing RelayError429")
	}
	// Retry-After 必须是整数秒；X-Request-ID 与响应体 message 里的 requestId 同源。
	for _, want := range []string{
		"Retry-After:",
		"X-Request-ID:",
		"type: integer",
		"application/json",
		"../schemas/relay.yaml#/schemas/RelayError",
	} {
		if !strings.Contains(relay429, want) {
			t.Fatalf("RelayError429 must declare %q", want)
		}
	}
	if strings.Contains(relay429, "application/problem+json") {
		t.Fatal("RelayError429 must not advertise problem+json")
	}

	schemaRaw, err := os.ReadFile("../../../../api/openapi/schemas/relay.yaml")
	if err != nil {
		t.Fatalf("read schemas/relay.yaml: %v", err)
	}
	relayError, ok := componentBlock(string(schemaRaw), "RelayError")
	if !ok {
		t.Fatal("schemas/relay.yaml is missing RelayError")
	}
	// 属性级断言（不是裸 `type:`）：`error` 对象自身也有 `type: object`，
	// 只匹配字符串会让「删掉 error.type」的变异继续变绿。
	errorBlock, ok := childBlock(relayError, "      error:\n", 6)
	if !ok {
		t.Fatal("RelayError must declare an error object")
	}
	if !strings.Contains(errorBlock, "required: [message, type, code]") {
		t.Fatal("RelayError.error must require message, type and code")
	}
	errorProps, ok := childBlock(errorBlock, "        properties:\n", 8)
	if !ok {
		t.Fatal("RelayError.error must declare properties")
	}
	normalized := "\n" + errorProps
	for _, field := range []string{"message", "type", "code"} {
		if !strings.Contains(normalized, "\n          "+field+":\n") {
			t.Fatalf("RelayError envelope must declare error.%s", field)
		}
	}
	if !strings.Contains(normalized, "enum: [new_api_error]") {
		t.Fatal("RelayError.error.type must freeze the literal new_api_error")
	}

	// 反向守卫：/api/* 的 429 仍是 problem+json 组件，未被中继组件替换。
	userRaw, err := os.ReadFile("../../../../api/openapi/paths/user.yaml")
	if err != nil {
		t.Fatalf("read user.yaml: %v", err)
	}
	account, ok := pathBlock(string(userRaw), "/api/account")
	if !ok {
		t.Fatal("spec is missing path /api/account")
	}
	if !strings.Contains(account, "$ref: '../components/responses.yaml#/responses/Error429'") {
		t.Fatal("/api/account 429 must keep the problem+json Error429 component")
	}
	if strings.Contains(account, "RelayError429") {
		t.Fatal("/api/account must not reference the relay envelope component")
	}
	error429, ok := componentBlock(responsesSpec, "Error429")
	if !ok {
		t.Fatal("responses.yaml is missing Error429")
	}
	if !strings.Contains(error429, "application/problem+json") {
		t.Fatal("Error429 must remain problem+json for /api/*")
	}
}

// componentBlock 返回公共组件文件中 `  <name>:` 起、到下一个同级键为止的文本。
// 与 pathBlock 的区别只是键的缩进层级（组件在 responses:/schemas: 下缩进两格）。
func componentBlock(spec, name string) (string, bool) {
	head := "\n  " + name + ":\n"
	start := strings.Index(spec, head)
	if start < 0 {
		return "", false
	}
	rest := spec[start+len(head):]
	for i := 0; i < len(rest); {
		j := strings.Index(rest[i:], "\n  ")
		if j < 0 {
			break
		}
		j += i
		// 下一个同级键：换行 + 两空格 + 非空白字符（继续缩进表示仍是本组件）。
		if j+3 < len(rest) && rest[j+3] != ' ' && rest[j+3] != '\n' && rest[j+3] != '#' {
			return rest[:j], true
		}
		i = j + 1
	}
	return rest, true
}

// childBlock 返回以 head 开头、到下一个缩进 <= parentIndent 的非空行为止的
// 子块文本。用于把「error 对象内部」与它自己的 type: object 区分开——属性级
// 断言必须落在正确的缩进层级，否则删掉 error.type 仍会被 schema 级 type 蒙混。
func childBlock(spec, head string, parentIndent int) (string, bool) {
	start := strings.Index(spec, head)
	if start < 0 {
		return "", false
	}
	rest := spec[start+len(head):]
	var out []string
	for _, line := range strings.Split(rest, "\n") {
		if strings.TrimSpace(line) != "" {
			indent := len(line) - len(strings.TrimLeft(line, " "))
			if indent <= parentIndent {
				break
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n"), true
}

// pathBlock 返回 `  <path>:` 起、到下一个同级路径（两空格缩进的 `/`）为止的文本。
func pathBlock(spec, path string) (string, bool) {
	key := "  " + path + ":\n"
	start := strings.Index(spec, key)
	if start < 0 {
		return "", false
	}
	rest := spec[start+len(key):]
	if next := strings.Index(rest, "\n  /"); next >= 0 {
		return rest[:next], true
	}
	return rest, true
}

// operation 返回路径块内某个操作的文本（到下一个四空格缩进的方法名为止）。
func operation(block, method string) (string, bool) {
	head := "    " + method + ":\n"
	start := strings.Index(block, head)
	if start < 0 {
		return "", false
	}
	rest := block[start+len(head):]
	for _, next := range []string{"\n    get:", "\n    put:", "\n    post:", "\n    patch:", "\n    delete:"} {
		if idx := strings.Index(rest, next); idx >= 0 {
			return rest[:idx], true
		}
	}
	return rest, true
}
