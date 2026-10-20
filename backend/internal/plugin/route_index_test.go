package plugin

import (
	"context"
	"testing"
)

// TestRouteIndexExactMatch 冻结精确匹配的基本语义：方法 + 完整路径命中，
// 返回值携带插件 key 与 Route 声明，且精确路由没有路径参数。
func TestRouteIndexExactMatch(t *testing.T) {
	var index RouteIndex
	index.add("video", Route{Method: "POST", Path: "/v1/videos", Kind: "submit"})

	match, ok := index.Match("POST", "/v1/videos")
	if !ok {
		t.Fatal("exact route must match")
	}
	if match.PluginKey != "video" || match.Route.Path != "/v1/videos" || match.Route.Kind != "submit" {
		t.Fatalf("unexpected match: %+v", match)
	}
	if match.PathParams != nil {
		t.Fatalf("exact route must not carry path params: %v", match.PathParams)
	}
}

// TestRouteIndexParamMatch：`{name}` 与 `:name` 必须同义（brief 指定前者，
// 既有 manifest 使用后者），且参数只捕获单段。
func TestRouteIndexParamMatch(t *testing.T) {
	var index RouteIndex
	index.add("suno", Route{Method: "GET", Path: "/v1/suno/fetch/{task_id}", Kind: "query"})
	index.add("kling", Route{Method: "POST", Path: "/v1/kling/:action", Kind: "submit"})

	match, ok := index.Match("GET", "/v1/suno/fetch/task-123")
	if !ok {
		t.Fatal("param route with {name} must match")
	}
	if match.PluginKey != "suno" || match.PathParams["task_id"] != "task-123" {
		t.Fatalf("unexpected match: %+v", match)
	}

	match, ok = index.Match("POST", "/v1/kling/text2video")
	if !ok || match.PathParams["action"] != "text2video" {
		t.Fatalf("colon param route must match: %+v ok=%v", match, ok)
	}
}

// TestRouteIndexNoGreedyAndMethodMismatch：参数段不跨段，方法必须一致。
func TestRouteIndexNoGreedyAndMethodMismatch(t *testing.T) {
	var index RouteIndex
	index.add("suno", Route{Method: "GET", Path: "/v1/suno/fetch/:task_id", Kind: "query"})

	if _, ok := index.Match("GET", "/v1/suno/fetch/a/b"); ok {
		t.Fatal("single-segment param must not match multiple segments")
	}
	if _, ok := index.Match("POST", "/v1/suno/fetch/task-1"); ok {
		t.Fatal("method mismatch must not match")
	}
	if _, ok := index.Match("GET", "/v1/suno/fetch/"); ok {
		t.Fatal("empty segment must not be captured as a param")
	}
}

// TestRouteIndexExactBeatsParam 是 brief Step 3 的「精确匹配优先」。
func TestRouteIndexExactBeatsParam(t *testing.T) {
	var index RouteIndex
	// 参数路由先加入：即使顺序在前，也不能抢走精确路由。
	index.add("catch", Route{Method: "POST", Path: "/v1/{action}", Kind: "submit"})
	index.add("video", Route{Method: "POST", Path: "/v1/videos", Kind: "submit"})

	match, ok := index.Match("POST", "/v1/videos")
	if !ok || match.PluginKey != "video" {
		t.Fatalf("exact route must win over param route: %+v ok=%v", match, ok)
	}
	// 非精确路径仍由参数路由兜住。
	match, ok = index.Match("POST", "/v1/images")
	if !ok || match.PluginKey != "catch" || match.PathParams["action"] != "images" {
		t.Fatalf("param route must still match non-exact path: %+v ok=%v", match, ok)
	}
}

// TestRouteIndexTrailingSlashNormalized：两侧尾斜杠归一，避免同一路由因客户端
// 多写一个 `/` 变成 404。
func TestRouteIndexTrailingSlashNormalized(t *testing.T) {
	var index RouteIndex
	index.add("video", Route{Method: "POST", Path: "/v1/videos", Kind: "submit"})

	if _, ok := index.Match("POST", "/v1/videos/"); !ok {
		t.Fatal("trailing slash must be normalized")
	}
}

// TestRouteIndexMoreLiteralsWins：字面段多者优先，且必须能区分于字典序
// tiebreak——这里的「字面多」路由 pattern 字典序反而更大（`/v1/{}/b/c` >
// `/v1/a/{}/{}`），删掉 literals 比较后本用例会失败。
func TestRouteIndexMoreLiteralsWins(t *testing.T) {
	var index RouteIndex
	index.add("generic", Route{Method: "GET", Path: "/v1/a/{b}/{c}", Kind: "query"})
	index.add("literal", Route{Method: "GET", Path: "/v1/{a}/b/c", Kind: "query"})

	for i := 0; i < 10; i++ {
		match, ok := index.Match("GET", "/v1/a/b/c")
		if !ok || match.PluginKey != "literal" {
			t.Fatalf("more literal segments must win over lexicographic order: %+v ok=%v", match, ok)
		}
	}
}

// TestRouteIndexOverlapTieBreakDeterministic 固化同分情形的 tiebreak：
// 两个字面段相同时按 pattern 字典序取小，保证同一请求恒定命中同一条路由
// （（`/v1/{a}/b/c` 与 `/v1/a/{b}/c` 都是 2 个字面段）。
func TestRouteIndexOverlapTieBreakDeterministic(t *testing.T) {
	var index RouteIndex
	index.add("later", Route{Method: "GET", Path: "/v1/{a}/b/c", Kind: "query"})
	index.add("earlier", Route{Method: "GET", Path: "/v1/a/{b}/c", Kind: "query"})

	for i := 0; i < 10; i++ {
		match, ok := index.Match("GET", "/v1/a/b/c")
		if !ok || match.PluginKey != "earlier" {
			t.Fatalf("tie must be broken by pattern order: %+v ok=%v", match, ok)
		}
	}
}

// TestRouteIndexZeroValue：未 Reload / 无插件的快照返回空索引，Match 不 panic。
func TestRouteIndexZeroValue(t *testing.T) {
	var index RouteIndex
	if _, ok := index.Match("POST", "/v1/videos"); ok {
		t.Fatal("zero index must not match")
	}
	if _, ok := index.Match("", ""); ok {
		t.Fatal("empty method must not match")
	}
}

// TestSnapshotRoutesOnlyContainsRegisteredPlugins 证明索引与注册结果同源：
// 冲突失败插件的**全部**路由都不得留在可匹配索引里（包括冲突之前已写入的
// 那一条），否则失败的插件在运行期仍局部可达。
func TestSnapshotRoutesOnlyContainsRegisteredPlugins(t *testing.T) {
	first := fixture{Key: "sample", Routes: routeJS("POST", "/v1/:id/submit", "submit", "buildSubmitRequest", "parseSubmitResponse")}.Source()
	// sample2 的第一条路由形状唯一（会被写入索引），第二条才冲突：
	// 回滚必须把第一条也删掉。
	second := fixture{Key: "sample2", Routes: `[
		{ method: "POST", path: "/v1/sample2/run", kind: "submit", decode: "buildSubmitRequest", render: "parseSubmitResponse" },
		{ method: "POST", path: "/v1/:task_id/submit", kind: "submit", decode: "buildSubmitRequest", render: "parseSubmitResponse" }
	]`}.Source()
	m := MustNewManager(map[string]string{"sample": first, "sample2": second}, nil, Options{MasterEnabled: true})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if entry := m.Snapshot().Lookup("sample2"); entry.Status != StatusCompileFailed {
		t.Fatalf("conflict plugin must fail as a whole: %+v", entry)
	}

	index := m.Snapshot().Routes()
	match, ok := index.Match("POST", "/v1/abc/submit")
	if !ok || match.PluginKey != "sample" {
		t.Fatalf("registered plugin route must be matchable: %+v ok=%v", match, ok)
	}
	if match.PathParams["id"] != "abc" {
		t.Fatalf("param capture = %v, want id=abc", match.PathParams)
	}
	if _, leaked := index.Match("POST", "/v1/sample2/run"); leaked {
		t.Fatal("rolled-back plugin route must not remain in the index")
	}
	if _, ok := m.Routes().Match("POST", "/v1/abc/submit"); !ok {
		t.Fatal("Manager.Routes must mirror the current snapshot")
	}
}

// TestSnapshotRoutesMasterDisabled：系统总开关关闭时不注册任何插件，
// 索引必须为空（否则被禁用的插件仍可路由）。
func TestSnapshotRoutesMasterDisabled(t *testing.T) {
	m := newManager(t, nil, Options{MasterEnabled: false})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := m.Snapshot().Routes().Match("POST", "/v1/anything"); ok {
		t.Fatal("disabled master switch must not expose plugin routes")
	}
}

// TestRegistryBraceParamShapeConflict 覆盖复核 Important-1：`{id}` 与 `:task_id`
// 混写/同写必须按同一形状冲突，后到插件整插件失败且不进索引。
func TestRegistryBraceParamShapeConflict(t *testing.T) {
	cases := []struct {
		name   string
		first  string
		second string
	}{
		{"brace_vs_colon", "/v1/a/{id}", "/v1/a/:task_id"},
		{"colon_vs_brace", "/v1/a/:id", "/v1/a/{task_id}"},
		{"brace_vs_brace", "/v1/a/{id}", "/v1/a/{task_id}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := fixture{Key: "sample", Routes: routeJS("POST", tc.first, "submit", "buildSubmitRequest", "parseSubmitResponse")}.Source()
			second := fixture{Key: "sample2", Routes: routeJS("POST", tc.second, "submit", "buildSubmitRequest", "parseSubmitResponse")}.Source()
			m := MustNewManager(map[string]string{"sample": first, "sample2": second}, nil, Options{MasterEnabled: true})
			if err := m.Reload(context.Background()); err != nil {
				t.Fatalf("reload: %v", err)
			}
			entry := m.Snapshot().Lookup("sample2")
			if entry.Status != StatusCompileFailed || !entry.Errors.hasRule("route_conflict") {
				t.Fatalf("形状冲突必须让后到插件 compile_failed: %+v", entry)
			}
			// 先注册者仍可达；后注册者的路由不得进入索引（同形状时无法区分，
			// 因此只断言先到者命中）。
			match, ok := m.Snapshot().Routes().Match("POST", "/v1/a/x")
			if !ok || match.PluginKey != "sample" {
				t.Fatalf("first plugin must keep the route: %+v ok=%v", match, ok)
			}
		})
	}
}

// TestBuiltinRouteShapesCoverResponsesFamily 冻结 R-10 的维护契约（T2/T3）：
// `/v1/responses` 族的三个内建形状都必须在保留集里，否则插件可以声明一条
// 内建 ServeMux 会永远抢走的路由（注册成功但不可达）。
//
// 例外集只对「处理器先查 RouteIndex」的形状开放：T2 的创建与 T3 的产物检索
// 都先 Match 插件索引；compact 是 fail-closed 的独立计费口径，不查索引，
// 必须继续按内建保留拒绝（否则插件声明会被静默吞掉）。
func TestBuiltinRouteShapesCoverResponsesFamily(t *testing.T) {
	for _, shape := range []string{
		"POST /v1/responses",
		"GET /v1/responses/{}",
		"POST /v1/responses/compact",
	} {
		if _, ok := builtinRouteShapes[shape]; !ok {
			t.Fatalf("%s 必须登记在 builtinRouteShapes（R-10 维护契约）", shape)
		}
	}
	for _, shape := range []string{"POST /v1/responses", "GET /v1/responses/{}"} {
		if _, ok := pluginDispatchedBuiltinShapes[shape]; !ok {
			t.Fatalf("%s 的处理器先查 RouteIndex，必须对插件开放", shape)
		}
	}
	if _, ok := pluginDispatchedBuiltinShapes["POST /v1/responses/compact"]; ok {
		t.Fatal("POST /v1/responses/compact 不查 RouteIndex，不得对插件开放")
	}
}

// TestRegistryAllowsPluginDispatchedResponsesRoute 冻结 P-01 与 R-10 的交集：
// `POST /v1/responses` 虽然在保留集里，但它的内建处理器先查 RouteIndex，
// 插件声明必须注册成功并可匹配——否则「插件优先、通用回退」的插件分支
// 永远无法上线（整插件 route_conflict 失败）。
func TestRegistryAllowsPluginDispatchedResponsesRoute(t *testing.T) {
	source := fixture{Key: "video", Routes: routeJS("POST", "/v1/responses", "submit", "buildSubmitRequest", "parseSubmitResponse")}.Source()
	m := MustNewManager(map[string]string{"video": source}, nil, Options{MasterEnabled: true})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	entry := m.Snapshot().Lookup("video")
	if entry.Status != StatusRegistered {
		t.Fatalf("plugin-first 的 /v1/responses 必须注册成功: %+v", entry)
	}
	match, ok := m.Snapshot().Routes().Match("POST", "/v1/responses")
	if !ok || match.PluginKey != "video" || match.Route.Kind != "submit" {
		t.Fatalf("plugin route must be matchable: %+v ok=%v", match, ok)
	}

	// 例外只解除「内建冲突」这一层：两个插件声明同一形状仍然按插件间冲突
	// 处理（后到者整插件失败），否则运行期归谁执行就变成未定义。
	// key 按字典序注册（"video" 先于 "zzlate"），先到者保留路由。
	second := fixture{Key: "zzlate", Routes: routeJS("POST", "/v1/responses", "submit", "buildSubmitRequest", "parseSubmitResponse")}.Source()
	m2 := MustNewManager(map[string]string{"video": source, "zzlate": second}, nil, Options{MasterEnabled: true})
	if err := m2.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if entry := m2.Snapshot().Lookup("zzlate"); entry.Status != StatusCompileFailed || !entry.Errors.hasRule("route_conflict") {
		t.Fatalf("duplicate /v1/responses declarations must conflict: %+v", entry)
	}
	if match, ok := m2.Snapshot().Routes().Match("POST", "/v1/responses"); !ok || match.PluginKey != "video" {
		t.Fatalf("first plugin must keep the route: %+v ok=%v", match, ok)
	}
}

// TestRegistryRejectsBuiltinReservedFamilyRoutes 覆盖 R-11/R-12/R-14：内建
// 通配族 `/v1/models`、`/v1beta/models` 按「族根 + 其下任意深度、任意方法」
// 保留——族内任何插件路由都必须在注册期整插件失败。
//
// 两种「注册成功但不可达」的形态都要挡住：
//   - 族下精确声明（`/v1/models/gpt-4o`、`/v1/models/x/y`）：ServeMux 的
//     `POST /v1/models/{path...}` 永远先命中；
//   - 族根（`/v1/models`，任意方法）：注册 `{path...}` 模式后 ServeMux 会对
//     族根发 307 重定向到 `/v1/models/`，随后由多模态 embeddings 处理器接管。
func TestRegistryRejectsBuiltinReservedFamilyRoutes(t *testing.T) {
	cases := []struct {
		method string
		path   string
	}{
		{"POST", "/v1/models"},
		{"GET", "/v1/models"},
		{"POST", "/v1/models/"},
		{"POST", "/v1/models/gpt-4o"},
		{"POST", "/v1/models/x/y"},
		{"GET", "/v1/models/gpt-4o"},
	}
	// `/v1beta/models` 族已在 R-09 被加载期拒绝（原生路由必须以 /v1/ 开头），
	// 构造不出能走到族判定的插件源；builtinReservedRouteFamilies 里的该条目
	// 是纵深防御（若将来 R-09 放开，族保留仍然生效），无独立用例。
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			// 同一插件里再放一条本可注册的路由：族冲突必须让整插件失败，
			// 已写入索引的那一条要一并回滚（否则失败插件仍局部可达）。
			source := fixture{Key: "sample", Routes: `[
				{ method: "POST", path: "/v1/sample/unique", kind: "submit", decode: "buildSubmitRequest", render: "parseSubmitResponse" },
				{ method: "` + tc.method + `", path: "` + tc.path + `", kind: "submit", decode: "buildSubmitRequest", render: "parseSubmitResponse" }
			]`}.Source()
			m := MustNewManager(map[string]string{"sample": source}, nil, Options{MasterEnabled: true})
			if err := m.Reload(context.Background()); err != nil {
				t.Fatalf("reload: %v", err)
			}
			entry := m.Snapshot().Lookup("sample")
			if entry.Status != StatusCompileFailed || !entry.Errors.hasRule("route_conflict") {
				t.Fatalf("保留族下的路由必须整插件 compile_failed: %+v", entry)
			}
			if _, ok := m.Snapshot().Routes().Match(tc.method, tc.path); ok {
				t.Fatal("family-conflicting route must not enter the index")
			}
			if _, leaked := m.Snapshot().Routes().Match("POST", "/v1/sample/unique"); leaked {
				t.Fatal("failed plugin's other routes must be rolled back")
			}
		})
	}
}

// TestRegistryRejectsBuiltinRouteConflict 覆盖复核 Important-2 / R-10：与内建
// /v1 路由同形状的声明必须在注册期整插件失败（否则 ServeMux 更具体模式优先，
// 插件路由静默不可达）。
func TestRegistryRejectsBuiltinRouteConflict(t *testing.T) {
	cases := []struct {
		method string
		path   string
	}{
		{"POST", "/v1/chat/completions"},
		{"GET", "/v1/models"},
		{"POST", "/v1/tasks/:key"},
		{"GET", "/v1/tasks/{key}/artifacts"},
		{"POST", "/v1/images/generations"},
		{"POST", "/v1/engines/:model/embeddings"},
		{"GET", "/v1/realtime"},
		// T2 / R-10：Responses 族中**没有**插件优先分发面的形状。
		// （`POST /v1/responses` 与 `GET /v1/responses/{id}` 例外：处理器先查
		// RouteIndex，见 TestRegistryAllowsPluginDispatchedResponsesRoute 与
		// TestBuiltinRouteShapesCoverResponsesFamily。）
		{"POST", "/v1/responses/compact"},
		// T4 / R-10：Videos 与 Batches 是纯内建形状（处理器不查 RouteIndex）。
		{"POST", "/v1/videos"},
		{"GET", "/v1/videos/{video_id}"},
		{"GET", "/v1/videos/{video_id}/content"},
		{"POST", "/v1/batches"},
		{"GET", "/v1/batches/{batch_id}"},
		{"POST", "/v1/batches/{batch_id}/cancel"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			source := fixture{Key: "sample", Routes: routeJS(tc.method, tc.path, "submit", "buildSubmitRequest", "parseSubmitResponse")}.Source()
			m := MustNewManager(map[string]string{"sample": source}, nil, Options{MasterEnabled: true})
			if err := m.Reload(context.Background()); err != nil {
				t.Fatalf("reload: %v", err)
			}
			entry := m.Snapshot().Lookup("sample")
			if entry.Status != StatusCompileFailed || !entry.Errors.hasRule("route_conflict") {
				t.Fatalf("内建路由冲突必须 compile_failed: %+v", entry)
			}
			if _, ok := m.Snapshot().Routes().Match(tc.method, tc.path); ok {
				t.Fatal("builtin-conflicting route must not enter the index")
			}
		})
	}
}

// TestCompileRejectsNonV1RoutePrefix 覆盖复核 R-09：非 /v1/ 前缀在加载期以
// 结构化 issue 拒绝（运行时没有任何 mux 会匹配它）。
func TestCompileRejectsNonV1RoutePrefix(t *testing.T) {
	for _, path := range []string{"/kling/v1/videos", "/suno/submit/:action", "/v1"} {
		issues := compileIssues(t, fixture{
			Key:    "sample",
			Routes: routeJS("POST", path, "submit", "buildSubmitRequest", "parseSubmitResponse"),
		}.Source(), CompileOptions{})
		if !issues.hasRule("route_prefix_unsupported") {
			t.Fatalf("path %q: want route_prefix_unsupported, got %v", path, issues)
		}
	}
	// 合法前缀不得误报。
	if _, err := Compile(context.Background(), fixture{
		Key:    "sample",
		Routes: routeJS("POST", "/v1/videos", "submit", "buildSubmitRequest", "parseSubmitResponse"),
	}.Source(), CompileOptions{}); err != nil {
		t.Fatalf("/v1/ 前缀不应被拒绝: %v", err)
	}
}
