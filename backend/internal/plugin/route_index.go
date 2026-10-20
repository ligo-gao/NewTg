package plugin

import "strings"

// RouteMatch 是 RouteIndex.Match 的结果。
//
// PathParams 只对带参数段的路由非 nil；精确路由返回 nil（不是空 map），
// 让调用方能用「有无路径参数」区分两类命中，而不必再看 Route.Path。
type RouteMatch struct {
	PluginKey  string
	Route      Route
	PathParams map[string]string
}

// RouteIndex 是插件原生路由的只读索引（T6c-a，冻结裁决 R-03）。
//
// 生命周期与 Snapshot 绑定：buildSnapshot 在注册阶段（registerRoutes）填充，
// 发布后不再修改。所有包外消费者（HTTP 面、后续 T2/T3）都必须复用本索引，
// 不得另建第二套匹配逻辑——否则「注册期冲突检测」与「运行期匹配」会出现
// 两套形状规则，冲突插件可能在运行期仍然可达。
//
// 匹配语义（relay-06 §2.2）：
//   - 方法 + 路径精确匹配优先。精确路由永远胜过参数路由，即使参数路由
//     声明得更早或更「具体」；
//   - 参数段 `{name}` / `:name` 只捕获**单段**，不做贪婪通配；`*name` 在
//     v1 manifest 里没有多段语义，按单段处理（见 parseRouteSegment）；
//   - 歧义消解不在这里做：同形状（`:id` 与 `:task_id`）的冲突已在
//     registerRoutes 阶段整插件拒绝，失败插件不会进入索引。
type RouteIndex struct {
	// exact 的键是「方法 + 空格 + 归一化路径」。精确路由数量通常远大于
	// 参数路由，用 map 让常见路径是一次哈希查找而不是线性扫描。
	exact map[string]RouteMatch
	// params 保存带参数段的路由。不同形状之间仍可能重叠（如
	// `/v1/a/{x}/c` 与 `/v1/{y}/b/c` 都匹配 `/v1/a/b/c`），注册期按
	// 归一化形状去重无法覆盖这种情况，因此 Match 用确定性优先级消解
	// （字面段多者胜，同分按 pattern 字典序），保证同一请求永远命中同一路由。
	params []indexedRoute
}

// indexedRoute 是参数路由的编译形态（段已切分、参数名已提取）。
type indexedRoute struct {
	method    string
	pattern   string
	segments  []routeSegment
	literals  int
	pluginKey string
	route     Route
}

// routeSegment 是路由路径的一段：literal 非空表示字面量，param 非空表示参数。
type routeSegment struct {
	literal string
	param   string
}

func newRouteIndex() RouteIndex {
	return RouteIndex{exact: make(map[string]RouteMatch)}
}

// add 把一条已通过冲突检测的路由加入索引。
//
// 调用方（registerRoutes）保证同一形状只加入一次；这里不重复检测。
func (ix *RouteIndex) add(pluginKey string, route Route) {
	if ix.exact == nil {
		ix.exact = make(map[string]RouteMatch)
	}
	segments, key := splitRoutePath(route.Path)
	parsed := make([]routeSegment, len(segments))
	literals := 0
	hasParam := false
	for i, seg := range segments {
		parsed[i] = parseRouteSegment(seg)
		if parsed[i].param != "" {
			hasParam = true
			continue
		}
		literals++
	}
	entry := RouteMatch{PluginKey: pluginKey, Route: route}
	if !hasParam {
		ix.exact[route.Method+" "+key] = entry
		return
	}
	ix.params = append(ix.params, indexedRoute{
		method:    route.Method,
		pattern:   route.Method + " " + key,
		segments:  parsed,
		literals:  literals,
		pluginKey: pluginKey,
		route:     route,
	})
}

// removePlugin 删除某插件的全部路由。
//
// 注册期回滚用：一个插件只要有一条路由冲突就整插件失败，已经写入索引的
// 其余路由必须一并撤销，否则失败插件仍会出现在 HTTP 面（与 Snapshot.routes
// 的回滚同理）。
func (ix *RouteIndex) removePlugin(pluginKey string) {
	for key, match := range ix.exact {
		if match.PluginKey == pluginKey {
			delete(ix.exact, key)
		}
	}
	kept := ix.params[:0]
	for _, route := range ix.params {
		if route.pluginKey != pluginKey {
			kept = append(kept, route)
		}
	}
	ix.params = kept
}

// Match 按方法 + 路径查找原生路由。
//
// 精确匹配先于参数匹配是冻结语义（brief Step 3）：`/v1/videos` 这样的精确
// 声明不会被 `/{id}` 形状抢走，插件作者因此可以同时声明固定端点和参数端点。
// 未命中返回 false；调用方负责 404 语义，本方法绝不返回错误或兜底路由。
func (ix RouteIndex) Match(method, path string) (RouteMatch, bool) {
	if len(ix.exact) == 0 && len(ix.params) == 0 {
		return RouteMatch{}, false
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		return RouteMatch{}, false
	}
	segments, key := splitRoutePath(path)
	if match, ok := ix.exact[method+" "+key]; ok {
		return match, true
	}

	var best *indexedRoute
	var bestParams map[string]string
	for i := range ix.params {
		candidate := &ix.params[i]
		if candidate.method != method || len(candidate.segments) != len(segments) {
			continue
		}
		params, ok := matchRouteSegments(candidate.segments, segments)
		if !ok {
			continue
		}
		if best == nil || moreSpecific(candidate, best) {
			best = candidate
			bestParams = params
		}
	}
	if best == nil {
		return RouteMatch{}, false
	}
	return RouteMatch{PluginKey: best.pluginKey, Route: best.route, PathParams: bestParams}, true
}

// moreSpecific 定义重叠参数路由的确定性优先级。
//
// 字面段越多越具体（`/v1/a/{x}/c` 胜过 `/v1/{y}/b/c`）；同分时按 pattern
// 字典序取小，避免依赖 map/切片插入顺序带来的「有时 A 有时 B」。注册期
// 同形状冲突已拒绝，这里只处理不同形状的重叠，不存在「应该报冲突」的情况。
func moreSpecific(candidate, current *indexedRoute) bool {
	if candidate.literals != current.literals {
		return candidate.literals > current.literals
	}
	return candidate.pattern < current.pattern
}

// matchRouteSegments 逐段比对；参数段捕获单段，空段不匹配参数。
func matchRouteSegments(pattern []routeSegment, actual []string) (map[string]string, bool) {
	var params map[string]string
	for i, segment := range pattern {
		if segment.param == "" {
			if segment.literal != actual[i] {
				return nil, false
			}
			continue
		}
		// `/a//b` 的空段不是有效参数值：允许它会被当成一个「有值但为空」
		// 的路径参数，把路由绕进后续层的空值处理。
		if actual[i] == "" {
			return nil, false
		}
		if params == nil {
			params = make(map[string]string, 2)
		}
		// 同名参数重复（`/:id/x/:id`）语义未定义：按不匹配处理，
		// 而不是让后一段静默覆盖前一段。
		if _, dup := params[segment.param]; dup {
			return nil, false
		}
		params[segment.param] = actual[i]
	}
	return params, true
}

// splitRoutePath 把路径切成段并返回归一化键。
//
// 首尾斜杠一律忽略（`/v1/videos/` 与 `/v1/videos` 同一路由），内部空段保留
// （与 registry.routeShape 的口径一致：`/a//b` 不是 `/a/b`）。
func splitRoutePath(path string) ([]string, string) {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil, "/"
	}
	return strings.Split(trimmed, "/"), "/" + trimmed
}

// parseRouteSegment 识别三种参数写法。
//
// `{name}` 是 brief 指定的单段参数语法，`:name` 是仓库既有 manifest/测试
// 使用的写法（registry_test 的 `/v1/:id/submit`），两者必须同义，否则同一份
// 插件源在「注册期形状」与「运行期匹配」之间会分叉。`*name` 在 v1 没有
// 多段语义，按单段处理：至少让字面段仍然可路由，而不是注册成功后永远不可达。
func parseRouteSegment(segment string) routeSegment {
	switch {
	case len(segment) > 2 && strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}"):
		return routeSegment{param: segment[1 : len(segment)-1]}
	case len(segment) > 1 && strings.HasPrefix(segment, ":"):
		return routeSegment{param: segment[1:]}
	case len(segment) > 1 && strings.HasPrefix(segment, "*"):
		return routeSegment{param: segment[1:]}
	}
	return routeSegment{literal: segment}
}
