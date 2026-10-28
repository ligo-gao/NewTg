package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/store"
)

// 本文件由 relay_endpoints.go 按职责拆分（P1-10），仅移动代码，未改动逻辑。

// ---------- /pg/*（Playground 会话中继，relay-01 §1.5） ----------

// RegisterPlayground 把 /pg/* 挂到根 mux。
//
// 与 Register 的区别只在鉴权链：/pg 只接受浏览器会话（PlaygroundSessionAuth），
// 不接受 API 令牌；通过后把路径重写到 /v1/*，交给**同一批**转发函数
// （handleChatCompletions / handleModels）。本函数刻意不复制路由、计费、
// 流式桥接逻辑：任何 /v1 侧的修复都会自动作用于 /pg。
//
// 挂载面按 relay-01 §1.5 收敛为**对话族**：只注册 `POST /pg/chat/completions`
// 与 `GET /pg/models`，其余 `/pg/*`（含 /v1 的多模态族 images/edits/
// embeddings/rerank/audio/engines/v1beta）一律 404 OpenAI 信封。
// 不把多模态族一并暴露的理由：设计只承诺对话族，且 OpenAPI 只登记这两条；
// 多挂的路径既无契约也无 UI 消费方，等于把未评审的计费面开给浏览器会话。
//
// 会话写路径沿用 /api/* 的 CSRF + OriginRefererCheck（Task 13 brief §3）：
// Playground 的 POST 是浏览器写操作，必须与其它会话写请求同口径。
func (h *RelayHandler) RegisterPlayground(mux *http.ServeMux, sessions *auth.SessionManager, publicBaseURL string) {
	if mux == nil || sessions == nil {
		panic("httpapi: RegisterPlayground requires a mux and a session manager")
	}
	// 内部转发表只注册对话族两条路由；会话鉴权已在最外层完成，故这里直接
	// 注册裸 handler，不再引入 RelayAuth 的令牌链。未注册的 /pg/* 由
	// handlePlayground 的 pattern=="" 分支统一回 404。
	inner := http.NewServeMux()
	inner.Handle("POST /v1/chat/completions", http.HandlerFunc(h.handleChatCompletions))
	inner.Handle("GET /v1/models", http.HandlerFunc(h.handleModels))
	h.playground = inner

	chain := PlaygroundSessionAuth(sessions)(
		CSRF(sessions)(
			OriginRefererCheck(parseAllowedOrigins(publicBaseURL))(
				http.HandlerFunc(h.handlePlayground))))
	mux.Handle("/pg/", chain)
	// 精确 /pg（无尾斜杠）也要落到同一 404 语义，而不是根 mux 的纯文本 404。
	mux.Handle("/pg", chain)
}

// handlePlayground 把 /pg 前缀重写为 /v1 后分派给内部转发表。
//
// 未支持的 /pg/* 返回 404 + OpenAI 错误信封（/pg 是 OpenAI 协议面，
// 不使用管理面的 RFC 7807 problem）。
func (h *RelayHandler) handlePlayground(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/pg")
	if suffix == "" || !strings.HasPrefix(suffix, "/") {
		writeRelayError(w, http.StatusNotFound, "", "not_found", "unsupported playground endpoint")
		return
	}
	// Clone 而不是原地改写：外层中间件/日志仍持有原始请求，且 RequestURI
	// 必须清空——它在服务端请求里是只读的权威字段，留着 /pg 会让下游
	// 任何基于 RequestURI 的判断看到未重写路径。
	rewritten := r.Clone(r.Context())
	urlCopy := *r.URL
	urlCopy.Path = "/v1" + suffix
	urlCopy.RawPath = ""
	rewritten.URL = &urlCopy
	rewritten.RequestURI = ""

	target, pattern := h.playground.Handler(rewritten)
	if pattern == "" {
		writeRelayError(w, http.StatusNotFound, "", "not_found", "unsupported playground endpoint")
		return
	}
	target.ServeHTTP(w, rewritten)
}

// applyPlaygroundSession 为会话身份补分组并执行模型准入。
//
// 返回 (status, code, message, ok)；ok=true 时 rc 已完成身份补齐。
// 判定顺序即语义：身份/用户状态（401/500）→ 分组回源 → 模型准入（403，
// 发生在任何 PreConsume 之前，因此不产生资金变动或 usage_logs 行）。
//
// 模型准入只认**当前分组**的模型清单：会话请求没有分组选择器，路由也用
// rc.UsingGroup，若按「任一可用分组」放行会出现「页面允许、路由必失败」
// 的两套答案。默认模型校验（PUT /api/self/setting）则按 brief 冻结的
// 「全部可用分组」口径，两者差异是有意的。
func applyPlaygroundSession(
	ctx context.Context,
	groups PlaygroundUserGroups,
	principal *auth.Principal,
	rc *contract.RelayContext,
	model string,
) (int, string, string, bool) {
	if principal == nil || principal.TokenID != nil {
		return 0, "", "", true
	}
	if groups == nil {
		// 生产装配必须提供；缺失时 fail-closed，绝不把空分组当「全部可用」。
		return http.StatusInternalServerError, "database_error", "user group lookup is unavailable", false
	}
	group, err := groups.UserGroup(ctx, principal.UserID)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			// 会话指向已删除/停用用户：身份失效，与坏令牌同语义（401）。
			return http.StatusUnauthorized, "", "invalid token", false
		}
		return http.StatusInternalServerError, "database_error", "failed to resolve user group", false
	}
	relay.WithIdentity(rc, rc.RequestID, rc.UserID, rc.TokenID, group, group)

	available, err := groups.UsableGroupsForUser(ctx, principal.UserID)
	if err != nil {
		return http.StatusInternalServerError, "database_error", "failed to resolve available models", false
	}
	for _, candidate := range available {
		if candidate.Name != group {
			continue
		}
		for _, allowed := range candidate.Models {
			if allowed == model {
				return 0, "", "", true
			}
		}
	}
	return http.StatusForbidden, "model_not_available",
		"the requested model is not available in the current group", false
}
