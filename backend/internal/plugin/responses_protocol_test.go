package plugin

// R-W18-07 的绑定层验收：protocols.openai_responses 是宿主端点的声明面，
// supports 必须真正收窄能力；routes[] 仍然只服务插件自有 URL。

import (
	"context"
	"strings"
	"testing"
)

// responsesProtocolFixture 生成一个在 routes[] 之外仅靠 protocols 声明
// Responses 绑定的插件。renderFinal 是校验器要求的最小渲染钩子。
func responsesProtocolFixture(key, supports string) fixture {
	return fixture{
		Key:       key,
		Protocols: `{ openai_responses: { supports: ` + supports + ` } }`,
		Exports: map[string]string{
			"decodeRequest": `function (ctx) { return ctx.body; }`,
			"renderFinal":   `function () { return { output: [] }; }`,
		},
	}
}

func newProtocolTestManager(t *testing.T, sources map[string]string) *Manager {
	t.Helper()
	manager := MustNewManager(sources, nil, Options{MasterEnabled: true})
	if err := manager.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	return manager
}

// TestProtocolsDeriveResponsesCreateBinding 是 R-W18-07 的核心：只声明
// protocols（routes[] 为空）即可接管 POST /v1/responses，且不需要重复 URL。
func TestProtocolsDeriveResponsesCreateBinding(t *testing.T) {
	source := responsesProtocolFixture("sample", `["create", "retrieve"]`).Source()
	manager := newProtocolTestManager(t, map[string]string{"sample": source})

	entry := manager.Snapshot().Lookup("sample")
	if entry.Status != StatusRegistered || entry.Plugin == nil {
		t.Fatalf("protocols-only plugin must register: %+v", entry)
	}
	parsed := entry.Plugin.Manifest().Responses
	if parsed == nil {
		t.Fatal("manifest.Responses must be parsed from protocols.openai_responses")
	}
	if !parsed.SupportsOperation(ResponsesOpCreate) || !parsed.SupportsOperation(ResponsesOpRetrieve) {
		t.Fatalf("supports = %v, want create+retrieve", parsed.Supports)
	}

	match, ok := manager.Snapshot().Routes().Match("POST", "/v1/responses")
	if !ok {
		t.Fatal("protocols.openai_responses.supports=create 必须派生 POST /v1/responses")
	}
	if match.PluginKey != "sample" || match.Route.Kind != RouteKindResponses {
		t.Fatalf("derived route = %+v, want plugin sample kind=%s", match, RouteKindResponses)
	}
	// routes[] 只用于插件自有 URL：协议派生不得额外伪造插件路由。
	if _, ok := manager.Snapshot().Routes().Match("GET", "/v1/responses/resp_x"); ok {
		t.Fatal("retrieve 绑定由 supports 判定，不应登记参数路由（避免跨插件冲突）")
	}
}

// TestProtocolsSupportsNarrowing 证明 supports 不是装饰：只声明 retrieve 的
// 插件不得抢 create；只声明 create 的插件也不会获得 retrieve 语义（由
// handlers 的 supports 判定测试覆盖）。
func TestProtocolsSupportsNarrowing(t *testing.T) {
	retrieveOnly := responsesProtocolFixture("sample", `["retrieve"]`).Source()
	manager := newProtocolTestManager(t, map[string]string{"sample": retrieveOnly})
	if entry := manager.Snapshot().Lookup("sample"); entry.Status != StatusRegistered {
		t.Fatalf("retrieve-only plugin must register: %+v", entry)
	}
	if _, ok := manager.Snapshot().Routes().Match("POST", "/v1/responses"); ok {
		t.Fatal("retrieve-only plugin 不得接管 create")
	}

	createOnly := responsesProtocolFixture("sample", `["create"]`).Source()
	manager2 := newProtocolTestManager(t, map[string]string{"sample": createOnly})
	if _, ok := manager2.Snapshot().Routes().Match("POST", "/v1/responses"); !ok {
		t.Fatal("create-only plugin 必须接管 create")
	}
}

// TestProtocolsRejectsInvalidSupports 覆盖加载期 fail-closed：未知/缺失/空
// supports 都不可被当成隐式推断。
func TestProtocolsRejectsInvalidSupports(t *testing.T) {
	cases := []struct {
		name      string
		protocols string
		rule      string
	}{
		{"missing_supports", `{ openai_responses: {} }`, "protocol_shape"},
		{"bare_string", `{ openai_responses: "create" }`, "protocol_shape"},
		{"not_array", `{ openai_responses: { supports: "create" } }`, "field_type"},
		{"empty", `{ openai_responses: { supports: [] } }`, "protocol_supports"},
		{"unknown_op", `{ openai_responses: { supports: ["cancel"] } }`, "protocol_supports"},
		{"duplicate_op", `{ openai_responses: { supports: ["create", "create"] } }`, "protocol_supports"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fix := responsesProtocolFixture("sample", `["create"]`)
			fix.Protocols = tc.protocols
			manager, err := NewManager(map[string]string{"sample": fix.Source()}, nil, Options{MasterEnabled: true})
			if err != nil {
				// factory 源编译失败由 NewManager 直接返回结构化 Issues。
				if !strings.Contains(err.Error(), tc.rule) {
					t.Fatalf("error = %v, want rule %s", err, tc.rule)
				}
				return
			}
			if err := manager.Reload(context.Background()); err != nil {
				if !strings.Contains(err.Error(), tc.rule) {
					t.Fatalf("reload error = %v, want rule %s", err, tc.rule)
				}
				return
			}
			entry := manager.Snapshot().Lookup("sample")
			if entry.Status != StatusCompileFailed || !entry.Errors.hasRule(tc.rule) {
				t.Fatalf("invalid supports must be compile_failed with %s: %+v", tc.rule, entry)
			}
		})
	}
}

// TestProtocolsRoutesStillWork 证明 routes[] 的插件自有 URL 与协议派生可以
// 共存；同一插件在 routes[] 里重复声明宿主端点被当成冗余而不是冲突。
func TestProtocolsRoutesStillWork(t *testing.T) {
	fix := responsesProtocolFixture("sample", `["create", "retrieve"]`)
	fix.Routes = `[
		{ method: "POST", path: "/v1/responses", kind: "submit", decode: "decodeRequest", render: "renderFinal" },
		{ method: "POST", path: "/v1/sample/submit", kind: "submit", decode: "decodeRequest", render: "renderFinal" }
	]`
	manager := newProtocolTestManager(t, map[string]string{"sample": fix.Source()})
	if entry := manager.Snapshot().Lookup("sample"); entry.Status != StatusRegistered {
		t.Fatalf("protocol + routes[] must coexist: %+v", entry)
	}
	// 宿主端点由协议派生接管（Kind=responses），不是 routes[] 的 submit。
	if match, ok := manager.Snapshot().Routes().Match("POST", "/v1/responses"); !ok || match.Route.Kind != RouteKindResponses {
		t.Fatalf("host endpoint must be protocol-derived: %+v ok=%v", match, ok)
	}
	if match, ok := manager.Snapshot().Routes().Match("POST", "/v1/sample/submit"); !ok || match.Route.Kind != "submit" {
		t.Fatalf("plugin-owned route must still register: %+v ok=%v", match, ok)
	}
}

// TestRoutesCannotBypassSupports 是 M1（W18-C 复核）：声明了协议后，routes[]
// 不能通过显式声明宿主端点来绕过 supports 的能力收窄。双向都拒绝；与
// supports 一致的显式声明保持可用。
func TestRoutesCannotBypassSupports(t *testing.T) {
	const getRoute = `[ { method: "GET", path: "/v1/responses/:response_id", kind: "query", taskIdParam: "response_id", render: "parseTaskResult" } ]`
	const postRoute = `[ { method: "POST", path: "/v1/responses", kind: "submit", decode: "decodeRequest", render: "renderFinal" } ]`

	cases := []struct {
		name       string
		supports   string
		routes     string
		wantStatus PluginStatus
		wantRule   string
	}{
		{"create_only_cannot_own_retrieve", `["create"]`, getRoute, StatusCompileFailed, "protocol_capability_mismatch"},
		{"retrieve_only_cannot_own_create", `["retrieve"]`, postRoute, StatusCompileFailed, "protocol_capability_mismatch"},
		{"create_redundant_post_is_ok", `["create"]`, postRoute, StatusRegistered, ""},
		{"retrieve_explicit_get_is_ok", `["retrieve"]`, getRoute, StatusRegistered, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fix := responsesProtocolFixture("sample", tc.supports)
			fix.Routes = tc.routes
			manager, err := NewManager(map[string]string{"sample": fix.Source()}, nil, Options{MasterEnabled: true})
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			if err := manager.Reload(context.Background()); err != nil {
				t.Fatalf("Reload: %v", err)
			}
			entry := manager.Snapshot().Lookup("sample")
			if entry.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s: %+v", entry.Status, tc.wantStatus, entry)
			}
			if tc.wantRule != "" && !entry.Errors.hasRule(tc.wantRule) {
				t.Fatalf("errors = %v, want rule %s", entry.Errors, tc.wantRule)
			}
		})
	}
}

// TestProtocolsCreateBindingConflictAcrossPlugins：协议声明面同样受注册期
// 冲突语义约束——两个插件都接管 create 时，后到者整插件失败，运行期归属
// 不得依赖注册顺序之外的不确定因素。
func TestProtocolsCreateBindingConflictAcrossPlugins(t *testing.T) {
	manager := newProtocolTestManager(t, map[string]string{
		"sample": responsesProtocolFixture("sample", `["create"]`).Source(),
		"zzlate": responsesProtocolFixture("zzlate", `["create"]`).Source(),
	})
	if entry := manager.Snapshot().Lookup("zzlate"); entry.Status != StatusCompileFailed || !entry.Errors.hasRule("route_conflict") {
		t.Fatalf("second create binding must conflict: %+v", entry)
	}
	if match, ok := manager.Snapshot().Routes().Match("POST", "/v1/responses"); !ok || match.PluginKey != "sample" {
		t.Fatalf("first plugin must keep the binding: %+v ok=%v", match, ok)
	}
}
