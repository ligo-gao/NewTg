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

// TestRouteIndexOverlapDeterministic：不同形状的参数路由可以同时匹配一个路径
// （注册期按形状去重覆盖不到）。Match 必须给出稳定答案：字面段多者优先。
func TestRouteIndexOverlapDeterministic(t *testing.T) {
	var index RouteIndex
	index.add("generic", Route{Method: "GET", Path: "/v1/{a}/b/c", Kind: "query"})
	index.add("literal", Route{Method: "GET", Path: "/v1/a/{b}/c", Kind: "query"})

	for i := 0; i < 10; i++ {
		match, ok := index.Match("GET", "/v1/a/b/c")
		if !ok || match.PluginKey != "literal" {
			t.Fatalf("more literal segments must win deterministically: %+v ok=%v", match, ok)
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
