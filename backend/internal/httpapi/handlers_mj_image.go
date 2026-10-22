// Package httpapi 的绘图任务族图片代理（W8/MJ-c）。
//
// 端点面（relay-05 §1.3）：
//
//	GET /mj/image/{id}?sig=&exp=     签名 URL 的图片流式代理
//
// 【授权模型（R-MJ-05/09/11）】路径 id 是 MjId（不是本网关的 public_task_id）。
// 签名绑定 `mj_id ‖ owner_user_id ‖ exp`，因此验签前必须先知道候选 owner：
// 按 MjId 全局取候选行（上限 100），对每个候选复算 HMAC，任一匹配即通过。
// id 畸形、任务不存在、签名缺失/篡改/过期、他人任务**一律 404**，且共用同一
// 固定文案——任何更细的区分都会把 /mj/image 变成 MjId 的存在性/归属 oracle。
//
// 【出站（R-MJ-10）】目标 URL 来自上游任务的 data.image_url，属于上游可影响
// 的数据（不是管理员填写），因此地址规则**始终**执行：私网/环回/link-local/
// 元数据/CGNAT/保留段、逐跳地址校验、反 DNS rebinding 都由 egress.Guard
// 完成。`sec.ssrf.mode=off` 与 `allow_private_ip` 均不得豁免 /mj/image；
// 域名模式固定 DomainAll（不依赖管理员 allowlist，否则 Discord CDN 默认不可用）。
// 目标被策略拒绝 → 403；解析失败/上游失败 → 502。
//
// 【响应（R-MJ-11）】成功 200 流式回写，Content-Type 限图片类型（缺失按
// image/jpeg）；存储 URL 非法/上游非 2xx/非图片类型/超限 → 502，**不透传**
// 上游状态码或错误体（4xx/5xx 透传会变成归属/令牌 oracle）。错误不缓存，
// 日志不记录目标 URL 与渠道凭据。
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/egress"
	"newtg/gateway/internal/mj"
	"newtg/gateway/internal/tasks"
)

const (
	// maxMJImageBytes 是图片代理的流式上限（32MiB）。
	//
	// 图片产物远大于 JSON 控制面，必须边读边写；没有上限则单个上游响应
	// 可以把带宽/连接占满。32MiB 覆盖常见 MJ 产物（含 4 张拼图/放大图），
	// 同时把失控响应挡在 502 而不是整段读入内存。
	maxMJImageBytes = 32 << 20

	// mjImageFetchTimeout 是单次图片出站的时限（含连接与响应头）。
	//
	// 与查询的 15s 分开：图片体积大、CDN 可能慢，但仍要有界，避免请求
	// goroutine 被一个不返回的上游永久占住。
	mjImageFetchTimeout = 30 * time.Second
)

// mjImagePolicy 把 sec.ssrf.* 收敛为图片代理专用策略。
//
// 与其它出站调用方（通知/插件市场）的关键差别：
//   - `sec.ssrf.mode=off` 不适用：目标 URL 由上游 DTO 提供，关闭地址规则
//     等于把内部网络暴露给上游可控数据（R-MJ-10 冻结）；
//   - `allow_private_ip` 不适用：同一理由，私网/环回/元数据必须始终拒绝；
//   - 域名模式固定 DomainAll：MJ 产物通常在 Discord CDN，默认 allowlist
//     只有 7 个 API 域，沿用会让端点开箱不可用；地址规则才是主防护。
func mjImagePolicy(cfg ConfigReader) egress.Policy {
	policy := egress.PolicyFromConfig(cfg)
	if policy.Mode == egress.ModeOff {
		policy.Mode = egress.ModeBasic
	}
	policy.DomainMode = egress.DomainAll
	policy.DomainList = nil
	policy.AllowPrivateIP = false
	return policy
}

// handleMJImage 处理 GET /mj/image/{id}。
//
// 【授权只有签名】本端点不挂 RelayAuth（设计明确不采纳强制鉴权）：签名 URL
// 就是能力凭证，浏览器 `<img src>` 不会携带令牌。签名与归属校验全部在这里
// 完成；任何失败都是 404。
func (h *MJHandler) handleMJImage(w http.ResponseWriter, r *http.Request) {
	id, mjErr := mj.ParseMjID(r.PathValue("id"))
	if mjErr != nil {
		h.writeMJImageNotFound(w, r)
		return
	}
	expiry, sig, ok := mj.ParseImageTicket(r.URL.Query())
	if !ok {
		h.writeMJImageNotFound(w, r)
		return
	}

	candidates, err := h.store.ListOwnersByUpstreamID(r.Context(), id, mj.MaxOwnerCandidates)
	if err != nil {
		h.log.Error("mj: read image owner candidates failed", "error", err)
		h.writeMJImageError(w, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInternalError, Status: http.StatusInternalServerError})
		return
	}
	// 逐个候选复算 HMAC：只有知道 owner 才能验签，而同一 MjId 可能对应多个
	// 用户（R-MJ-05）。任一匹配即通过；全部不匹配与「没有候选」同走 404。
	now := h.now()
	var task *tasks.Task
	for _, candidate := range candidates {
		if candidate == nil || candidate.Family != tasks.FamilyMJ {
			continue
		}
		if h.imageSigner.Verify(id, candidate.UserID, expiry, sig, now) {
			task = candidate
			break
		}
	}
	if task == nil {
		h.writeMJImageNotFound(w, r)
		return
	}

	rawURL, ok := mj.ImageURL(task)
	if !ok {
		// 行存在但产物地址缺失/非字符串：这是网关/上游数据缺陷，不是 404。
		h.writeMJImageBadGateway(w)
		return
	}
	// 先区分「结构上不是 URL」与「被策略拒绝」：前者 502，后者 403。
	// guard.ValidateURLShape 会把畸形 URL 也归入 ErrBlocked，直接用它会把
	// 数据缺陷误报成 SSRF 拒绝，故这里先行解析。
	parsed, parseErr := url.Parse(rawURL)
	if parseErr != nil || parsed.Host == "" || parsed.Hostname() == "" {
		h.writeMJImageBadGateway(w)
		return
	}

	// 无 DNS 的形状校验：scheme/port/userinfo/域名策略。放在拨号之前是
	// 为了给「存储 URL 本身不合法/策略不允许」一个确定的 403，而不是把它
	// 混进连接失败；真正的地址校验在 DialContext（只解析一次，反 rebinding）。
	if _, err := h.imageGuard.ValidateURLShape(rawURL); err != nil {
		h.writeMJImagePolicyError(w, err)
		return
	}

	// 出站客户端在装配期构造（禁跟随重定向、禁代理、复用连接池）。
	// 地址规则由 Transport.DialContext 在每次连接时执行：解析一次、校验
	// 全部候选 IP、只拨已校验的 IP（反 rebinding）。
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, parsed.String(), nil)
	if err != nil {
		h.writeMJImageBadGateway(w)
		return
	}
	if r.Method == http.MethodHead {
		// HEAD 只需上游头部：不为「问大小」把整份图片下载一遍（与产物
		// content 的 HEAD 语义一致）。
		req.Method = http.MethodHead
	}
	resp, err := h.imageClient.Do(req)
	if err != nil {
		h.writeMJImagePolicyError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// 3xx 也在内：客户端不跟随重定向，重定向响应按上游失败折叠为 502，
		// 绝不跟随到未经校验的下一跳。同时不透传上游 4xx/5xx：客户端无法
		// 据状态码区分「任务存在但上游失败」与「归属不符」，透传会重新
		// 打开归属 oracle。
		h.writeMJImageBadGateway(w)
		return
	}
	contentType, ok := imageContentType(resp.Header.Get("Content-Type"))
	if !ok {
		h.writeMJImageBadGateway(w)
		return
	}
	if resp.ContentLength > maxMJImageBytes {
		h.writeMJImageBadGateway(w)
		return
	}

	w.Header().Set("Content-Type", contentType)
	// nosniff 与生产 SecurityHeaders 重复，但本处理器也可能被单独挂载/测试；
	// 图片字节被当成 HTML/脚本解释是 XSS 面。
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// 能力 URL 带 owner 签名：允许浏览器短缓存，但禁止共享缓存跨用户复用。
	w.Header().Set("Cache-Control", "private, max-age=60")
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(http.StatusOK)
	// ServeMux 的 GET 模式也匹配 HEAD：只回头部，不为「问大小」下载整份图片。
	if r.Method == http.MethodHead {
		return
	}

	buf := make([]byte, 32<<10)
	// remaining 达到上限时 Read 返回固定错误，复制会中止；头已发出，因此
	// 只能断开连接——不完整的流不能被当成完整图片（与产物 content 同口径）。
	if _, err := io.CopyBuffer(w, &mjImageLimitReader{body: resp.Body, remaining: maxMJImageBytes}, buf); err != nil {
		h.log.Warn("mj: image stream interrupted", "task_id", task.PublicTaskID, "error", err)
	}
}

// mjImageSigRejectedDetail 是安全审计的固定 detail：只表达「签名/归属失败」
// 这一事实，不携带 URL、MjId、sig 或任何请求片段（避免审计表成为枚举工具）。
const mjImageSigRejectedDetail = `{"event":"mj_image_sig_rejected"}`

// writeMJImageNotFound 是 id/签名/归属类失败的唯一出口（R-MJ-11）。
//
// 除 404 外还写一条 best-effort 安全审计（security-02 §2.4：category=security
// 的 mj_image_sig_rejected）。审计失败不影响响应——日志库独立于业务库，
// 不能因为审计不可用把 404 变成 500。
func (h *MJHandler) writeMJImageNotFound(w http.ResponseWriter, r *http.Request) {
	if h.audit != nil && r != nil {
		_ = writeAuditEvent(r.Context(), h.audit, audit.Event{
			Action:     audit.ActionMJImageSigRejected,
			Result:     "failed",
			IP:         clientIP(r),
			UserAgent:  r.UserAgent(),
			Method:     r.Method,
			Route:      "/mj/image/{id}", // 固定模板：绝不写入路径里的 MjId
			StatusCode: http.StatusNotFound,
			Detail:     json.RawMessage(mjImageSigRejectedDetail),
		})
	}
	h.writeMJImageError(w, mjTaskNotFound())
}

// writeMJImageBadGateway 是存储 URL 非法/上游失败/非图片类型的出口。
func (h *MJHandler) writeMJImageBadGateway(w http.ResponseWriter) {
	h.writeMJImageError(w, &mj.Error{
		Code: mj.CodeUnknownError, Description: mj.DescriptionUpstreamError,
		Status: http.StatusBadGateway,
	})
}

// writeMJImagePolicyError 把出站错误映射为 403/502（R-MJ-10）。
//
// 只有 ErrBlocked 是策略拒绝（403）。重定向不被跟随：3xx 会作为普通响应
// 返回，并在上层按「上游非 2xx」折叠为 502；ErrRedirect 只可能出现在
// FollowRedirects=true 的调用方，本端点不会产生，故不设该分支。
//
// 按错误类别而不是文本映射：*url.Error 的 Error() 会拼上完整目标 URL
// （可能含 query 凭据），绝不能写进响应或日志。
func (h *MJHandler) writeMJImagePolicyError(w http.ResponseWriter, err error) {
	if errors.Is(err, egress.ErrBlocked) {
		h.writeMJImageError(w, mj.Forbidden(mj.DescriptionForbidden))
		return
	}
	// ErrUnresolved 与其余传输失败（连接拒绝/超时/TLS）都不可归因于客户端，
	// 统一 502；不把底层错误文本外传。
	h.writeMJImageBadGateway(w)
}

// writeMJImageError 是图片代理所有错误响应的唯一写出口：统一 no-store。
//
// 错误体不含目标 URL/凭据/MjId；no-store 防止中间缓存把一次归属失败或
// 上游故障缓存成后续请求的假结果。
func (h *MJHandler) writeMJImageError(w http.ResponseWriter, err error) {
	w.Header().Set("Cache-Control", "no-store")
	writeMJError(w, err)
}

// imageContentType 归一上游 Content-Type，只接受栅格图片类型。
//
// 缺失时按设计默认 image/jpeg；显式给出的值必须能被 mime.ParseMediaType
// 解析且以 image/ 开头。SVG 被拒绝：它可以在网关同源内联执行脚本，而 MJ
// 产物是栅格图，没有兼容性理由为它打开 XSS 面。
func imageContentType(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "image/jpeg", true
	}
	if len(raw) > 128 {
		return "", false
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return "", false
	}
	mediaType = strings.ToLower(mediaType)
	if !strings.HasPrefix(mediaType, "image/") || mediaType == "image/svg+xml" {
		return "", false
	}
	return mediaType, true
}

// mjImageLimitReader 在上限处截断流并返回固定错误。
//
// 不把它交给 io.LimitReader：LimitReader 到达上限后返回 EOF，调用方会把
// 「被截断的图片」误认为完整内容。这里返回错误，让复制中止并留下日志。
type mjImageLimitReader struct {
	body      io.Reader
	remaining int64
}

var errMJImageTooLarge = errors.New("mj: image exceeds the proxy size limit")

func (r *mjImageLimitReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		// 已读满上限：再探测 1 字节以区分「恰好 max 字节」与「超限」。
		// 直接返回错误会把恰好等于上限的完整图片误判成超限。
		var probe [1]byte
		n, err := r.body.Read(probe[:])
		if n > 0 {
			return 0, errMJImageTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.body.Read(p)
	r.remaining -= int64(n)
	return n, err
}
