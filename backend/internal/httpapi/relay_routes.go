package httpapi

import (
	"net/http"
	"strings"
)

// 本文件由 relay_endpoints.go 按职责拆分（P1-10），仅移动代码，未改动逻辑。

// AuthMiddleware 返回 /v1 令牌鉴权中间件。
//
// 单独导出是为了让同一进程内的其它 /v1 端点族（M6d 的 /v1/tasks*）复用
// **同一个** RelayAuth 实例与同一份 trustedProxies 解析结果：复制一份配置
// 会让两条路径在代理信任策略上悄悄分叉。
func (h *RelayHandler) AuthMiddleware() func(http.Handler) http.Handler {
	return RelayAuthWithRecorder(h.tokens, h.access, h.trustedProxies, h.refusals, h.log)
}

// Register 把 /v1 路由挂到 mux；鉴权中间件只包裹这些端点（管理面不经过它）。
//
// 多模态端点族（M4 T1）复用同一个 RelayAuth 中间件：统一入口要求所有 /v1
// 端点先鉴权再归一化（relay-04 §0.2）。GET /v1/models 只在此处注册一次
// （R-09），多模态注册函数不得重复注册它。
func (h *RelayHandler) Register(mux *http.ServeMux) {
	authMiddleware := h.AuthMiddleware()
	mux.Handle("POST /v1/chat/completions", authMiddleware(http.HandlerFunc(h.handleChatCompletions)))
	mux.Handle("GET /v1/models", authMiddleware(http.HandlerFunc(h.handleModels)))
	// R-W18-13：模型列表/检索按协议头分流（relay-04 §10.1）。
	// 检索路径用 `{model...}` 通配：模型名允许包含 `/`（如
	// `Qwen/Qwen2.5-72B`、`meta-llama/Llama-3-70B`），单段 `{model}` 会让
	// 这类合法模型名落到 405。`/v1beta/models` 同理。
	mux.Handle("GET /v1/models/{model...}", authMiddleware(http.HandlerFunc(h.handleModel)))
	mux.Handle("GET /v1beta/models", authMiddleware(http.HandlerFunc(h.handleBetaModels)))
	mux.Handle("GET /v1beta/models/{model...}", authMiddleware(http.HandlerFunc(h.handleBetaModel)))
	mux.Handle("GET /v1beta/openai/models", authMiddleware(http.HandlerFunc(h.handleOpenAIModels)))
	// T2 / P-01：OpenAI Responses 创建端点——插件原生路由优先，未命中返回
	// 501（R-13；Responses 透传/转码另立 T2b，通用中继不承接）。
	// 精确模式注册（不是 /v1/ 子树），与 T1 的兜底不重叠：ServeMux 让精确模式
	// 胜出，兜底只接收没有其它路由认领的 /v1 请求。
	mux.Handle("POST /v1/responses", authMiddleware(http.HandlerFunc(h.handleResponses)))
	// R-W18-12：`POST /v1/responses/compact` 本波不提供，**刻意不注册**。
	// 不注册的路径会落到 /v1 兜底并返回 404；保留 501 半死桩会让客户端误以为
	// 「改天就能用」，也违反设计「不提供 = 404」的诚实失败口径。
	// W7/M1：Anthropic Messages 原生入口。与 chat 共用同一个 RelayAuth 中间件
	// 与同一个计费接缝（h.billing）；鉴权载体 x-api-key 的归一化已在
	// relay_auth.go / relay.credentialFromRequest 中按路径固化。
	mux.Handle("POST /v1/messages", authMiddleware(http.HandlerFunc(h.handleMessages)))
	// R-W18-06：零计费 count_tokens。ServeMux 的精确模式按路径区分，
	// `/v1/messages` 不会吞掉 `/v1/messages/count_tokens`；注册顺序仅为可读性，
	// 不是路由精确性依赖。鉴权沿用同一条 RelayAuth（x-api-key / Bearer）。
	mux.Handle("POST /v1/messages/count_tokens",
		authMiddleware(http.HandlerFunc(h.handleMessagesCountTokens)))

	// M4 T1 的多模态路由骨架：dispatcher 由 Task 4 注入；同时把 TokenLookup
	// 交给多模态 handler 以复用 BuildRelayContext 的分组回源路径。
	RegisterMultimodalRoutes(mux, authMiddleware, NewMultimodalHandler(MultimodalHandlerOptions{
		Tokens:      h.tokens,
		ModelStatus: h.modelStatus,
		Dispatcher:  h.multimodal,
		Groups:      h.userGroups,
		// W7/M5-b：多模态族与 chat 共用同一个限流接缝与同一份配置快照。
		RateLimiter: h.rateLimiter,
		Config:      h.config,
	}))

	// T6c-a：插件原生路由（manifest routes[]）的 /v1 兜底。
	//
	// 用无方法的子树模式 `/v1/`：更具体的精确模式（POST /v1/chat/completions、
	// GET /v1/models、POST /v1/tasks/{key} 等）按 ServeMux 优先级胜出，兜底只
	// 接收「没有其它路由认领」的 /v1 请求。它不是通用中继回退：命中插件路由
	// 走任务数据面，未命中一律 404 OpenAI 信封（P-05），绝不进入 chat 管线。
	mux.Handle(nativeRouteFallbackPattern, h.nativeRouteFallback(authMiddleware, mux))

	// R-W18-13：`/v1beta/*` 的未匹配方法/未知路径同样回 OpenAI 信封。
	//
	// 不注册这条兜底时，`PUT /v1beta/models/x` 会落到 Go ServeMux 的默认
	// 405（text/plain "Method Not Allowed"），与同一路径其它错误
	// （application/json + {error:{...}}）形状分裂。兜底只做路由求解，
	// 不鉴权、不分发，因此不会扩大可达面。
	mux.Handle(betaRouteFallbackPattern, h.betaRouteFallback(mux))
}

// nativeRouteFallbackPattern 是原生路由兜底的 ServeMux 模式。
const nativeRouteFallbackPattern = "/v1/"

// betaRouteFallbackPattern 是 Gemini 兼容路径的兜底模式（R-W18-13）。
const betaRouteFallbackPattern = "/v1beta/"

// betaRouteFallback 把 /v1beta/* 的 404/405 统一成 /v1 的 OpenAI 错误信封。
//
// 405 语义与 nativeRouteFallback 相同：子树模式匹配所有方法，ServeMux 自己
// 不会再产生 405，必须用路由表探测补回（探测不执行 handler）。
func (h *RelayHandler) betaRouteFallback(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newRelayRequestID()
		if allowed := methodMismatchAllowed(mux, r); len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeRelayError(w, http.StatusMethodNotAllowed, requestID, "method_not_allowed",
				"method not allowed for this endpoint")
			return
		}
		writeRelayError(w, http.StatusNotFound, requestID, "invalid_request_error", "unknown endpoint")
	})
}

// nativeRouteFallback 构建 /v1/ 兜底 handler。
//
// 鉴权只包裹**命中**的原生路由（与 /v1/tasks 同一条 RelayAuth 链）：未知
// 路径保持「无令牌也 404」的既有可见性——兜底上线之前根 mux 对未知 /v1
// 就是 404，能力探测面不应因为加了兜底而变成 401/404 二值 oracle。
//
// 405 语义必须由这里补回：Go 的 ServeMux 只有在「路径匹配某个模式但方法不
// 匹配」时才返回 405，而 `/v1/` 匹配所有方法，若不显式探测，GET /v1/models
// 这类 GET-only 路径的其它方法会从 405 退化成 404。探测只查路由表
// （mux.Handler），不执行任何 handler。
func (h *RelayHandler) nativeRouteFallback(authMiddleware func(http.Handler) http.Handler, mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newRelayRequestID()
		if h.nativeRoutes != nil {
			if match, ok := h.nativeRoutes.NativeRoutes().Match(r.Method, r.URL.Path); ok {
				// 每次命中现建一个只用一次的 handler 链：中间件本身无状态，
				// 包住 match 比把 RouteMatch 塞进 context 更直接、更易审计。
				authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					h.nativeRoutes.DispatchNativeRoute(w, r, match)
				})).ServeHTTP(w, r)
				return
			}
		}
		// R-W18-12：明确不提供的 Responses 形状必须 404，而不是被下面的
		// 405 探测变成「方法不允许」。GET /v1/responses（WebSocket）与
		// POST /v1/responses/compact 在本波没有 handler，路径存在（POST
		// /v1/responses 已注册）会让探测返回 405——那会把「没提供」误报成
		// 「换个方法就行」，因此在这里先收敛为 404。
		if responsesEndpointUnprovided(r.Method, r.URL.Path) {
			writeRelayError(w, http.StatusNotFound, requestID, "invalid_request_error", "unknown endpoint")
			return
		}
		// R-W18-04：405/404 也属于端点响应，必须按路径选择信封——
		// GET /v1/messages、POST /v1/messages/unknown 属于 Claude 面。
		writeErr := writeRelayErrorForRequest(r)
		if allowed := methodMismatchAllowed(mux, r); len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeErr(w, http.StatusMethodNotAllowed, requestID, "method_not_allowed",
				"method not allowed for this endpoint")
			return
		}
		writeErr(w, http.StatusNotFound, requestID, "invalid_request_error", "unknown endpoint")
	})
}

// responsesEndpointUnprovided 判定是否为 R-W18-12 明确不提供的端点。
//
//   - `GET /v1/responses`：WebSocket 升级，本波不做（无状态集群下需要连接
//     粘滞与排空语义），不注册路由 → 404；
//   - `POST /v1/responses/compact`：独立计费口径的压缩端点，本波不做 →
//     不注册路由 → 404（501 半死桩已删除）。
//
// 只有 GET /v1/responses 收敛为 404：其它方法（PUT/DELETE…）走既有 405
// 探测（POST /v1/responses 已注册，Allow 必须如实列出 POST）。compact 的
// 路径不存在，任何方法都应 404。
//
// 只匹配精确路径（尾斜杠归一）；`/v1/responses/{id}` 属于已提供的检索面，
// 不得被这条规则误伤。
func responsesEndpointUnprovided(method, path string) bool {
	normalized := strings.TrimSuffix(path, "/")
	switch normalized {
	case "/v1/responses/compact":
		return true
	case "/v1/responses":
		return strings.EqualFold(method, http.MethodGet)
	}
	return false
}

// nativeProbeMethods 是方法不匹配探测的候选集合。
//
// 覆盖 /v1 面已注册的全部方法族（含 OPTIONS，供将来的 CORS 端点）；HEAD 单独
// 探测是因为 Go 的 GET 模式同时匹配 HEAD，不探测就无法在 Allow 里体现它。
var nativeProbeMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

// methodMismatchAllowed 返回「此路径存在、但当前方法不匹配」时允许的方法。
//
// 返回空切片表示路径本身没有注册过（真正的 404）。探测通过 mux.Handler
// 只做路由求解，不会执行 handler，也不消耗 body（请求未动，浅拷贝仅改 Method）。
func methodMismatchAllowed(mux *http.ServeMux, r *http.Request) []string {
	if mux == nil {
		return nil
	}
	allowed := make([]string, 0, len(nativeProbeMethods))
	for _, method := range nativeProbeMethods {
		if method == r.Method {
			continue
		}
		probe := new(http.Request)
		*probe = *r
		probe.Method = method
		_, pattern := mux.Handler(probe)
		if pattern == "" || pattern == nativeRouteFallbackPattern || pattern == betaRouteFallbackPattern {
			continue
		}
		allowed = append(allowed, method)
	}
	return allowed
}

// Handler 返回挂好路由的独立 mux（测试与嵌入式装配用）。
func (h *RelayHandler) Handler() http.Handler {
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}
