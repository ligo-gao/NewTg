package rbac

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"

	"newtg/gateway/internal/api/dto"
	"newtg/gateway/internal/auth"
)

// RFC 7807 type URI 沿用全库 https://newtg.dev/problems/<slug> 约定。
// not_found 与业务 handler 的 404 必须同形：数据级拒绝如果带可区分的 type，
// 就等于告诉攻击者「这个 ID 存在，只是不属于你」，形成 ID 枚举信道
// （security-02 §3.2 DENY_CODE_POLICY / §3.3）。
const (
	problemTypeForbidden        = "https://newtg.dev/problems/forbidden"
	problemTypeNotFound         = "https://newtg.dev/problems/not_found"
	problemTypeFieldNotWritable = "https://newtg.dev/problems/field_not_writable"
)

// maxWriteBodyBytes 限制鉴权层为检查字段而缓冲的请求体大小。字段白名单必须看
// 完整 JSON 对象才能判定，但鉴权层不应成为内存放大器；超限按失败关闭拒绝。
const maxWriteBodyBytes = 1 << 20

// AuthzAction 描述一次授权请求的动作与目标。
//
//   - ResourceKind：资源类型（如 "user"/"channel"/"order"），与 ActionName 一起
//     标识字段策略登记项（security-02 §2.6.1 的「控制器/动作」粒度）；
//   - ActionName：功能级动作名。管理动作直接使用 20 键目录中的权限键；
//     自服务动作（如「查看本人订单」）留空，表示不要求管理权限键，仅要求已认证；
//   - TargetOwnerUserID：ID 寻址资源的归属；列表类/无归属动作用 0（absent）。
//
// ActionName 留空与「未知键」必须区分：前者是显式的自服务动作，后者是拼写错误
// 或未登记键，鉴权层对后者失败关闭。
type AuthzAction struct {
	ResourceKind      string
	ActionName        string
	TargetOwnerUserID int64
}

// AuthzCtx 是服务端采集的请求上下文，禁止取自客户端可伪造的字段。
// M1 只透传这些信号；风险带/step-up 判定属于 security-02 §2.5 的后续层，
// 因此 AuthzDecision.RequireStepUp 目前恒为 false。
type AuthzCtx struct {
	ClientIP  string
	UAHash    string
	SessionID string
}

// AuthzDecision 是三层判定的结果。Effect 为 "allow" 或 "deny"；
// Reason 携带机器可读拒绝原因，供审计与 RFC 7807 响应选择状态码。
type AuthzDecision struct {
	Effect         string
	RequireStepUp  bool
	StrippedFields []string
	Reason         string
}

// FieldPolicy 是某个控制器/动作的写侧字段白名单（BOPLA，security-02 §2.6.1）。
//
// 采用树形而不是平铺集合：嵌套对象必须逐层校验，不能因为顶层字段合法就放过
// 嵌套越权字段。Fields 的键是该层唯一允许出现的字段名；值为 nil 表示该字段的
// 内部结构不透明（标量、数组或自由形态 map，由 handler 的 schema 负责类型校验），
// 非 nil 表示继续按子树校验其对象/对象数组的键。
//
// 注意动作隔离：POST 与 PATCH 的可写集合可以不同，策略按 action 登记而不是按
// 资源或权限键登记，避免复用创建白名单作为更新白名单。
type FieldPolicy struct {
	Fields map[string]*FieldPolicy
}

// PermissionLoader 是 users.admin_permissions 的权威读取口，由装配层（Task 6）
// 在会话鉴权之后接线。
//
// 返回值的 nil 语义必须与数据库一致：
//   - (nil, nil) 表示数据库 NULL = FULL（ADMIN_PERMS_FULL）；
//   - ([]string{}, nil) 表示显式空集合（没有任何权限），不是 FULL；
//   - error 表示无法读取权威值，调用方必须拒绝（fail-closed），绝不能把
//     「未加载」当成 FULL（security-02 IS-B-6、GC-7）。
//
// 实现应按 UserID 每次读取权威值（可带 ≤1s 失效的缓存），不要在会话里复制
// 权限数组：那会在权限变更后形成第二真源（security-02 IS-B-5）。
type PermissionLoader interface {
	Load(ctx context.Context, userID int64) ([]string, error)
}

// AuthzMiddleware 持有字段策略登记表与可选的权限装载器。零值可用；
// 并发只读是安全的，但 SetWritePolicy / SetPermissionLoader 只在启动装配期
// 调用，装配完成后不得再写入。
type AuthzMiddleware struct {
	writePolicies    map[string]*FieldPolicy
	permissionLoader PermissionLoader
}

// NewAuthzMiddleware 返回带空登记表的中间件。零值同样可用，提供构造函数只是
// 让装配代码的意图更明确。
func NewAuthzMiddleware() *AuthzMiddleware {
	return &AuthzMiddleware{writePolicies: make(map[string]*FieldPolicy)}
}

// SetPermissionLoader 接线权限装载器。传 nil 表示「Principal 已由调用方填好」。
//
// 为什么需要这个 hook：会话中间件不加载 Permissions（Task 4 契约），而
// Principal.Permissions == nil 在 RBAC 语义里又是 DB NULL = FULL。若不接线，
// 「会话路径忘记加载权限」的 admin 会被当成全权管理员。接线后 Handler 每次
// 按 UserID 读取权威值并覆盖 Principal 里可能陈旧的副本；读取失败直接拒绝。
func (m *AuthzMiddleware) SetPermissionLoader(loader PermissionLoader) {
	if m == nil {
		return
	}
	m.permissionLoader = loader
}

func (m *AuthzMiddleware) permissionLoaderFor() PermissionLoader {
	if m == nil {
		return nil
	}
	return m.permissionLoader
}

// SetWritePolicy 登记（或传 nil 注销）某个动作的写字段白名单。
//
// 传 nil 是注销：此后该动作的写请求会因「登记表缺失」而失败关闭，而不是
// 回退到「不检查」。
func (m *AuthzMiddleware) SetWritePolicy(action AuthzAction, policy *FieldPolicy) {
	if m == nil {
		return
	}
	if policy == nil {
		delete(m.writePolicies, actionKey(action))
		return
	}
	if m.writePolicies == nil {
		m.writePolicies = make(map[string]*FieldPolicy)
	}
	m.writePolicies[actionKey(action)] = policy
}

// Authorize 执行功能级与数据级判定。字段级判定需要请求体，无法从
// AuthzAction 推导，因此由 CheckFields（或直接使用 Handler）完成；
// 调用方必须按「功能级 → 数据级 → 字段级」的顺序组合，字段级拒绝绝不能
// 抢先于功能级/数据级（security-02 §2.6.1 规则 4）。
//
// Permissions 契约（必须严格遵守）：p.Permissions 必须已经由上层的 DB 装载器
// 按 p.UserID 从 users.admin_permissions 填充；nil 的语义是「数据库 NULL =
// FULL」，不是「尚未加载」。未加载就调用本方法会把 admin 误判为全权管理员。
// 无法保证已加载时，请改用 ResolvePermissions（或走 Handler）并接线
// PermissionLoader：装载失败会返回 error，调用方必须拒绝，绝不能把
// 「未加载」当成 FULL。
func (m *AuthzMiddleware) Authorize(p auth.Principal, _ AuthzCtx, action AuthzAction) AuthzDecision {
	return authorize(p, action.ActionName, action)
}

// ResolvePermissions 在配置了 PermissionLoader 时按 UserID 读取权威权限集，
// 返回用于后续 Authorize/CheckFields 的 Principal 副本。
//
// 仅对 role=admin 装载：user 的权限数组不参与判定，省掉无意义的 DB 查询。
// 装载失败返回 error，调用方必须 fail-closed（Handler 会写 403 且不调用下游）。
// 未配置装载器时原样返回 p——这表示调用方明确保证 Permissions 已由 DB 填充。
func (m *AuthzMiddleware) ResolvePermissions(ctx context.Context, p auth.Principal) (auth.Principal, error) {
	loader := m.permissionLoaderFor()
	if loader == nil || p.Role != RoleAdmin {
		return p, nil
	}
	perms, err := loader.Load(ctx, p.UserID)
	if err != nil {
		return auth.Principal{}, fmt.Errorf("load admin permissions for user %d: %w", p.UserID, err)
	}
	// 覆盖而不是合并：DB 是唯一真源，Principal 里的副本（可能来自旧会话或
	// 上层误填）不得与权威值取并集，否则权限撤销无法即时生效（IS-B-5）。
	p.Permissions = perms
	return p, nil
}

// authorize 是 Authorize 与 Handler 的共同实现：required 由路由声明显式传入，
// 从而让 Handler(required, action) 中的 required 成为功能级的唯一真源；
// 直接调用 Authorize 时则取 action.ActionName。
func authorize(p auth.Principal, required string, action AuthzAction) AuthzDecision {
	if !functionalAllowed(p, required) {
		return denyDecision(ReasonPermissionDenied)
	}
	// 数据级：ID 寻址的他人资源，只有在调用者持有该资源管理键时才可访问。
	// 无管理键时返回 404 而不是 403——403 会确认资源存在，等同于允许攻击者
	// 用状态码差异枚举他人订单/令牌/流水（security-02 §2.2、TC-B-07）。
	if action.TargetOwnerUserID != 0 &&
		action.TargetOwnerUserID != p.UserID &&
		!managementAllowed(p, required) {
		return denyDecision(ReasonNotFound)
	}
	return AuthzDecision{Effect: EffectAllow}
}

// functionalAllowed 实现功能级判定。
//
// 设计取舍：
//   - required=="" 是显式自服务动作：只要求已认证（UserID!=0），数据级仍然生效；
//   - required 非空必须是目录内键：未知键失败关闭，防止拼写错误被 FULL 顺带放行；
//   - role=admin：FULL（NULL/["*"]）或持有该键才放行；
//   - role=user：无论 Permissions 里残留什么（包括 "*"）一律拒绝管理动作。
//     角色是外层门，不能靠权限数组反推提权（TC-B-01）。
func functionalAllowed(p auth.Principal, required string) bool {
	if p.UserID == 0 {
		return false // 未认证：零值身份不得当作匿名放行（security-02 §3.1 规则 7）
	}
	if required == "" {
		return true
	}
	if p.Role != RoleAdmin {
		return false
	}
	if !IsPermissionKey(required) {
		return false
	}
	return hasFullPermissions(p.Permissions) || containsPermission(p.Permissions, required)
}

// managementAllowed 回答数据级问题：「调用者是否有权跨用户访问该资源」。
// 只有显式的目录键才算管理权；required==""（自服务动作）永远没有跨用户权限。
func managementAllowed(p auth.Principal, required string) bool {
	if required == "" {
		return false
	}
	return HasPermission(p, required)
}

// CheckFields 单独执行字段级（写侧 BOPLA）判定，供非 HTTP 组装路径复用。
//
// 未登记策略的动作失败关闭（security-02 §2.6.1 规则 6）：返回
// field_not_writable 而不是放行，避免「忘了登记」变成静默的批量赋值漏洞。
func (m *AuthzMiddleware) CheckFields(action AuthzAction, body map[string]any) AuthzDecision {
	policy, ok := m.writePolicy(action)
	if !ok {
		return denyDecision(ReasonFieldNotWritable)
	}
	if len(offendingFields(policy, body)) > 0 {
		return denyDecision(ReasonFieldNotWritable)
	}
	return AuthzDecision{Effect: EffectAllow}
}

// AuthorizeGrant 校验「授予权限」请求（TC-B-04 / F-07-09）：授权者必须持有
// users:manage-roles，且授予集合必须是自身权限集合的子集；FULL（NULL/["*"]）
// 可以授予任意目录键。未知键一律拒绝，否则拼错的键会被写进用户行、等到
// 未来某次目录扩充后意外生效。
//
// "*" 是 FULL 的同义词，只有 FULL 授权者才能授予；非 FULL 管理员把 "*" 授予
// 他人等于绕过超集校验完成提权。
func (m *AuthzMiddleware) AuthorizeGrant(p auth.Principal, requested []string) AuthzDecision {
	if p.UserID == 0 || p.Role != RoleAdmin {
		return denyDecision(ReasonPermissionDenied)
	}
	full := hasFullPermissions(p.Permissions)
	if !full && !containsPermission(p.Permissions, PermUsersManageRoles) {
		return denyDecision(ReasonPermissionDenied)
	}
	for _, key := range requested {
		if key == WildcardPermission {
			if !full {
				return denyDecision(ReasonPermissionDenied)
			}
			continue
		}
		if !IsPermissionKey(key) {
			return denyDecision(ReasonPermissionDenied)
		}
		if !full && !containsPermission(p.Permissions, key) {
			return denyDecision(ReasonPermissionDenied)
		}
	}
	return AuthzDecision{Effect: EffectAllow}
}

// Handler 包装业务 handler，按三层判定结果写 RFC 7807 响应：
// 功能级 403、数据级 404、字段级 403 field_not_writable。
//
// required 是路由声明的功能级权限键（自服务动作为空）；action 提供资源类型、
// 动作名与目标归属。字段策略按 action 登记，因此 POST/PATCH 可以各自定义白名单。
// 配置了 PermissionLoader 时，Handler 会先按 UserID 解析权威权限（失败即 403），
// 再执行三层判定，避免把会话路径未加载的 nil 误当 FULL。
func (m *AuthzMiddleware) Handler(required string, action AuthzAction) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := auth.PrincipalFrom(r.Context())
			if !ok || p == nil {
				// 认证中间件缺失或顺序装配错误：失败关闭，绝不把零值身份当匿名。
				writeProblem(w, http.StatusForbidden, problemTypeForbidden, "Forbidden", "", nil)
				return
			}

			// 权限必须先解析再判定：会话路径的 Principal.Permissions 为空，
			// 而空（nil）在 RBAC 语义里是 DB NULL=FULL。接线了装载器就按
			// UserID 覆盖为权威值；装载失败即拒绝，绝不把「未加载」当 FULL。
			resolved, err := m.ResolvePermissions(r.Context(), *p)
			if err != nil {
				writeProblem(w, http.StatusForbidden, problemTypeForbidden, "Forbidden", "", nil)
				return
			}

			// 顺序不可交换：功能级/数据级拒绝优先于字段级（§2.6.1 规则 4）。
			if d := authorize(resolved, required, action); d.Effect != EffectAllow {
				writeDecision(w, d, nil)
				return
			}

			if isWriteMethod(r.Method) {
				body, present, err := readJSONObject(r)
				if err != nil {
					// 无法检查字段就无法证明请求体合法 → 失败关闭。
					writeProblem(w, http.StatusForbidden, problemTypeFieldNotWritable,
						"Field not writable", err.Error(), nil)
					return
				}
				if present {
					policy, registered := m.writePolicy(action)
					if !registered {
						// 未登记可写字段的动作默认拒绝写请求（§2.6.1 规则 6）。
						writeProblem(w, http.StatusForbidden, problemTypeFieldNotWritable,
							"Field not writable",
							"no writable-field policy is registered for this action", nil)
						return
					}
					if bad := offendingFields(policy, body); len(bad) > 0 {
						sort.Strings(bad)
						writeProblem(w, http.StatusForbidden, problemTypeFieldNotWritable,
							"Field not writable",
							"request body contains fields that this action may not write", bad)
						return
					}
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

func (m *AuthzMiddleware) writePolicy(action AuthzAction) (*FieldPolicy, bool) {
	if m == nil || m.writePolicies == nil {
		return nil, false
	}
	policy, ok := m.writePolicies[actionKey(action)]
	return policy, ok && policy != nil
}

// actionKey 用 NUL 分隔字段，避免 ("a","b:c") 与 ("a:b","c") 这类拼接碰撞。
func actionKey(action AuthzAction) string {
	return action.ResourceKind + "\x00" + action.ActionName
}

// offendingFields 返回请求体中不在白名单内的字段路径（如 "profile.email"）。
//
// 递归规则：命中的字段若登记了子树，则对象逐键继续校验、对象数组逐元素校验；
// 未登记字段直接记为越权。非对象值没有「内部字段」可校验，类型问题留给
// handler 的 schema；这里只回答授权问题。
func offendingFields(policy *FieldPolicy, body map[string]any) []string {
	var bad []string
	for key, value := range body {
		child, writable := policy.Fields[key]
		if !writable {
			bad = append(bad, key)
			continue
		}
		if child == nil {
			continue // 不透明叶子：handler 负责其内部结构与类型
		}
		switch v := value.(type) {
		case map[string]any:
			bad = append(bad, prefixFields(key, offendingFields(child, v))...)
		case []any:
			for i, item := range v {
				obj, ok := item.(map[string]any)
				if !ok {
					continue
				}
				bad = append(bad, prefixFields(fieldPath(key, i), offendingFields(child, obj))...)
			}
		}
	}
	return bad
}

func prefixFields(prefix string, fields []string) []string {
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = prefix + "." + f
	}
	return out
}

func fieldPath(key string, index int) string {
	return key + "[" + strconv.Itoa(index) + "]"
}

// readJSONObject 读取并缓存请求体，再把 r.Body 还原给下游 handler。
//
// present=false 表示请求没有 body（或全为空白）：没有字段可写，字段层无事可做。
// 返回 error 的三种情况（读取失败、超限、不是 JSON 对象）都按失败关闭处理：
// 无法证明字段合法时必须拒绝，而不是放行给 handler 自行解释。
func readJSONObject(r *http.Request) (body map[string]any, present bool, err error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, false, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxWriteBodyBytes+1))
	// 无论成功与否都还原 body：成功路径下游还要读；失败路径虽然不会调用下游，
	// 但保留下游可能依赖的 Body 语义（例如日志中间件）。
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return nil, false, errors.New("unable to inspect request body")
	}
	if len(raw) > maxWriteBodyBytes {
		return nil, true, errors.New("request body exceeds the authorization inspection limit")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, true, errors.New("request body is not a JSON object")
	}
	return obj, true, nil
}

// isWriteMethod 按 RFC 9110 安全方法白名单判定：GET/HEAD/OPTIONS/TRACE 之外
// 一律按写请求处理。未知/自定义方法因此默认受字段层约束（fail-closed）。
func isWriteMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return false
	default:
		return true
	}
}

// authzCtxFromRequest 采集服务端可信上下文。
//
// ClientIP 只取 RemoteAddr：X-Forwarded-For 等头在未配置可信代理时可由客户端
// 任意伪造，用它做风控信号等于把攻击者可控值送进风险带。UA 只存 SHA-256，
// 与 security-01 的会话元数据口径一致，避免把设备指纹原文落库/落日志。
func authzCtxFromRequest(r *http.Request) AuthzCtx {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	sum := sha256.Sum256([]byte(r.UserAgent()))
	sid := ""
	if c, err := r.Cookie(auth.SessionCookieName); err == nil {
		sid = c.Value
	}
	return AuthzCtx{
		ClientIP:  ip,
		UAHash:    hex.EncodeToString(sum[:]),
		SessionID: sid,
	}
}

func denyDecision(reason string) AuthzDecision {
	return AuthzDecision{Effect: EffectDeny, Reason: reason}
}

// writeDecision 把拒绝结果映射为 HTTP 语义。数据级 404 的响应体不包含任何
// 归属信息（与真实资源不存在的响应同形），字段级额外带上 fields 扩展成员，
// 便于前端精确定位被拒字段——但仍是一个整体 403，不做部分写入。
func writeDecision(w http.ResponseWriter, d AuthzDecision, fields []string) {
	switch d.Reason {
	case ReasonNotFound:
		writeProblem(w, http.StatusNotFound, problemTypeNotFound, "Not Found", "", nil)
	case ReasonFieldNotWritable:
		writeProblem(w, http.StatusForbidden, problemTypeFieldNotWritable, "Field not writable", "", fields)
	default:
		writeProblem(w, http.StatusForbidden, problemTypeForbidden, "Forbidden", "", nil)
	}
}

// writeProblem 输出 RFC 7807 Problem（GC-6）。fields 作为扩展成员随响应返回；
// Problem schema 明确允许扩展成员（如 request_id）。
func writeProblem(w http.ResponseWriter, status int, problemType, title, detail string, fields []string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	problem := dto.Problem{Type: problemType, Title: title, Status: status}
	if detail != "" {
		problem.Detail = &detail
	}
	if len(fields) > 0 {
		problem.Set("fields", fields)
	}
	// 响应已开始写出；编码失败（客户端断开）无法再改状态码，忽略即可。
	_ = json.NewEncoder(w).Encode(problem)
}
