// 插件原生路由的分发面（T6c-a）。
//
// 路由索引由 plugin.Snapshot 提供（注册期已解决冲突），本文件只负责把
// 「命中了哪条路由」翻译成任务数据面的既有入口：提交复用 handleSubmit、
// 查询复用 handleGet。刻意不在 HTTP 层复制提交/查询逻辑——鉴权、插件准入、
// 渠道选择、预扣、退款与落库必须只有一份实现，否则原生入口与 /v1/tasks
// 入口会在计费语义上悄悄分叉（P-01：计费只发生一次）。
package httpapi

import (
	"log/slog"
	"net/http"
	"strings"

	"newtg/gateway/internal/plugin"
)

// NativeRoutes 实现 NativeRouteDispatcher：返回当前插件快照的只读索引。
//
// 每次请求都取最新快照而不是缓存：插件 30s 轮询会发布新 generation，缓存
// 会让「已停用的路由」在旧索引上继续可达。
func (h *TasksHandler) NativeRoutes() plugin.RouteIndex {
	return h.plugins.Snapshot().Routes()
}

// DispatchNativeRoute 执行一次已命中的原生路由。
//
// plugin key 来自路由索引而不是 URL 路径段，因此先写回 PathValue("key") 再
// 调用既有 handler：两个入口读到的是同一份路径值，插件准入与错误信封完全
// 一致。路径参数（如 query 路由的 task_id）同样写回 PathValue，供后续协议
// 适配（T6c 后续块）与本文的查询转发读取。
func (h *TasksHandler) DispatchNativeRoute(w http.ResponseWriter, r *http.Request, match plugin.RouteMatch) {
	requestID := newRelayRequestID()
	key := strings.TrimSpace(match.PluginKey)
	if key == "" {
		// 索引命中却没有插件 key 是构建期缺陷（registerRoutes 只会写入
		// 已注册插件）；fail-closed 404 而不是把空 key 交给提交路径。
		writeRelayError(w, http.StatusNotFound, requestID, "invalid_request_error", "unknown task plugin")
		return
	}
	for name, value := range match.PathParams {
		r.SetPathValue(name, value)
	}
	// 插件 key 必须最后写回：manifest 可以把参数段命名为 `{key}`，先写 key
	// 会被 URL 段值覆盖，提交就会落到另一个插件（复核 Minor-1）。
	r.SetPathValue("key", key)

	// Responses 创建出口优先于 kind：协议派生路由（Kind=responses）与
	// routes[] 显式声明的 POST /v1/responses 都必须返回 Responses 形状，
	// 不能因为声明方式不同而落到 202 task 提交出口（R-W18-07）。
	if isResponsesCreateRoute(match.Route.Method, match.Route.Path, match.Route.Kind) {
		h.handleResponsesCreate(w, r, match)
		return
	}

	switch match.Route.Kind {
	case "query":
		h.dispatchNativeQuery(w, r, match)
	case "submit", "dynamic":
		// submit / dynamic：两者都由宿主提交上游任务，复用提交路径。
		h.handleSubmit(w, r)
	default:
		// manifest 只允许 submit/query/dynamic（加载期已拒绝其它值）；走到
		// 这里说明 RouteIndex 与 manifest 校验分叉，fail-closed 而不是把它
		// 当 submit 静默执行（复核 Minor-2）。
		slog.Error("tasks: native route has unknown kind",
			"plugin", key, "method", match.Route.Method, "path", match.Route.Path, "kind", match.Route.Kind)
		writeRelayError(w, http.StatusInternalServerError, requestID, "internal_error",
			"native route kind is not dispatchable")
	}
}

// dispatchNativeQuery 把 query 路由转发到既有任务查询。
//
// 查询路由把任务 id 放在路径段（taskIdParam，默认 task_id），而 handleGet
// 统一从 ?task_id= 读取；这里克隆请求再补查询参数，不修改调用方的 URL
// （外层中间件/日志仍持有原始请求）。
func (h *TasksHandler) dispatchNativeQuery(w http.ResponseWriter, r *http.Request, match plugin.RouteMatch) {
	requestID := newRelayRequestID()
	param := match.Route.TaskIDParam
	if param == "" {
		param = "task_id"
	}
	taskID := strings.TrimSpace(match.PathParams[param])
	if taskID == "" {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", "task_id is required")
		return
	}
	cloned := r.Clone(r.Context())
	urlCopy := *r.URL
	query := urlCopy.Query()
	query.Set("task_id", taskID)
	urlCopy.RawQuery = query.Encode()
	cloned.URL = &urlCopy
	cloned.RequestURI = ""
	h.handleGet(w, cloned)
}
