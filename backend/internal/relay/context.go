// Package relay 实现中继核心：RelayContext 组装、渠道分派、执行与流式桥接。
//
// 依赖方向（relay-01 §0）：中间件层 → 归一化层 → 路由层 → 执行层 → 计费接缝层，
// 下层不得反向依赖；契约类型全部来自 internal/contract（冻结，不在本包内修改）。
package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/adapters"
)

// 定位错误一律用哨兵值而不是裸字符串：HTTP 状态码映射归 Task 8，
// 在此之前调用方（路由/端点层）只能依据 errors.Is 做语义判定。
var (
	// ErrPrincipalMissing 表示请求没有经过鉴权中间件（或注入失败）。
	// 必须早退而不是把零值身份当匿名用户：否则 UserID=0 会进入计费与日志。
	ErrPrincipalMissing = errors.New("relay: 缺少已认证身份")
	// ErrRequestMissing 表示调用方传入了 nil 请求（编程错误）。
	ErrRequestMissing = errors.New("relay: 缺少 HTTP 请求")
	// ErrTokenInvalid 覆盖 relay-01 §3.3 的 401 族：凭据缺失、令牌不存在、
	// 状态/过期/额度非法，以及存储不可用。对客户端不区分原因（§3.6），
	// 避免成为令牌存在性的探测信道。
	ErrTokenInvalid = errors.New("relay: 令牌缺失或无效")
	// ErrPinForbidden 是 relay-01 §3.5 的 403：普通用户使用 legacy
	// sk-<key>-<channelId> 后缀指定渠道。系统没有用户私有渠道，
	// 因此这不是「越权访问他人渠道」，而是「无权使用 pin 能力」（控制器裁决 R-P2a）。
	ErrPinForbidden = errors.New("relay: 普通用户不支持指定渠道")
	// ErrIdentityOutOfRange 表示 M1 的 int64 主键无法无损落到契约的 int。
	// 宁可拒绝请求，也不能让截断后的 id 指向另一个用户（见 idToInt）。
	ErrIdentityOutOfRange = errors.New("relay: 身份 id 超出 int 表示范围")
	// ErrAutoGroupsNotEnabled 表示 group=auto 的令牌没有可用的自动分组列表。
	//
	// R-61 / modules-02 I-20：历史遗留的空列表 auto 令牌必须在**请求期**
	// fail-closed（HTTP 403 auto_groups_not_enabled），绝不回退 users.group——
	// 否则一个保存期即应被拒绝的配置会在请求期静默按用户分组选路/计费。
	ErrAutoGroupsNotEnabled = errors.New("relay: 自动分组未启用")
)

// groupAuto 是令牌分组的自动哨兵（modules-02 §2.9）。
//
// 与 tokenstore.GroupAuto 同值；协议层不 import 持久化包（depguard
// relay-boundary），因此在此重复字面量并冻结语义：它只表示「按 AutoGroups
// 序列选路」，永远不是真实分组名，不得进入候选查询或定价。
const groupAuto = "auto"

// RelayFormat 的取值在契约里没有冻结字面量（contract.RelayFormat 只承诺是协议标识）。
// 这里给出「归一化前按路径可判定」的四个常用值；归一化层拿到完整请求后可以覆盖初值，
// 它仍是 RawFormat 的权威写入方（relay-01 §7.2）。
const (
	FormatOpenAI          contract.RelayFormat = "openai"
	FormatClaude          contract.RelayFormat = "claude"
	FormatGemini          contract.RelayFormat = "gemini"
	FormatOpenAIResponses contract.RelayFormat = "openai_responses"
)

// maxPeekBytes 是预读请求体的上限（1 MiB）。
// 取 1 MiB 的理由：正常的对话/嵌入请求远小于此；而图片/音频上传可能很大，
// 预读只为拿 stream 标志，不能把整个上传体读进内存。
const maxPeekBytes = 1 << 20

// BuildRelayContext 组装一次中继请求的 RelayContext。
//
// 签名按控制器裁决 R-P2：principal 必须是 *auth.Principal（中间件注入的就是指针），
// nil 直接报错而不是退化成匿名身份。
//
// 本任务（Task 1）的写入边界（控制器裁决）：
//   - 身份组：RequestID/UserID/TokenID/UsingGroup/UserGroup（relay-01 §7.1）；
//   - 请求组：只填归一化前即可判定的 RequestPath/RawFormat/Streaming（§7.2）；
//   - 路由/渠道/计费/流式/任务/审计组保持零值——它们的唯一写入方是后续层
//     （§7.3–§7.8），组装阶段填任何值都等于伪造「已选渠道 / 已估算计费」；
//   - 不解析消息、不产出 IR：归一化层（Task 8）持有 Request/OriginModel 的写权。
//
// 模型白名单不在这里校验：auth.TokenLookup 返回的 TokenMeta 刻意不携带模型限制，
// 白名单按 relay-01 §3.4 注在路由阶段执行（pin 命中时还要跳过白名单）。
func BuildRelayContext(ctx context.Context, principal *auth.Principal, req *http.Request, tokenStore auth.TokenLookup) (*contract.RelayContext, error) {
	if principal == nil {
		return nil, ErrPrincipalMissing
	}
	if req == nil {
		return nil, ErrRequestMissing
	}

	userID, err := idToInt(principal.UserID)
	if err != nil {
		return nil, err
	}
	requestID, err := newRequestID()
	if err != nil {
		return nil, err
	}

	rc := &contract.RelayContext{}
	// 会话路径（TokenID == nil）先落 TokenID=0、分组留空；Bearer 路径在下面补齐。
	WithIdentity(rc, requestID, userID, 0, "", "")

	path := ""
	if req.URL != nil {
		path = req.URL.Path
	}
	// 请求组初值：这三个字段在解析请求体之前就能从请求本身判定，
	// 前置过滤器（如 request_path）需要它们；归一化层随后仍是权威写入方（§7.2）。
	rc.RequestPath = path
	rc.RawFormat = detectRawFormat(path)
	// R-W18-05：客户端 `anthropic-beta` 在 HTTP 边界解析并按白名单过滤一次，
	// 之后只以显式契约字段（RelayContext/Request.AnthropicBeta）流转。
	// 多个同名头与单头逗号分隔是等价语义，先拼成逗号串再统一过滤。
	// 该值不得写入日志/审计（与凭据同级纪律）。
	rc.AnthropicBeta = adapters.FilterAnthropicBeta(strings.Join(req.Header.Values("anthropic-beta"), ","))

	// 凭据只解析一次：Bearer 路径用它查令牌分组，会话路径只用它判定 pin。
	// ParseChannelPin 是路径无关的导出函数，路由层会独立消费 pin；因此
	// 「会话身份 + Authorization 头里的 pin 后缀」也必须在组装阶段拒绝，
	// 否则非管理员可以借一个本应被忽略的头把 pin 带进路由（R-P2a、relay-01 §3.5）。
	raw, hasCredential := credentialFromRequest(req)
	var key string
	var hasPin bool
	if hasCredential {
		key, _, hasPin = normalizeTokenCredential(raw)
	}

	if principal.TokenID != nil {
		tokenID, err := idToInt(*principal.TokenID)
		if err != nil {
			return nil, err
		}

		// Bearer 路径必须能从请求重新取回凭据：Principal 只带 TokenID，
		// 而分组在 TokenMeta 里。取不到说明身份与请求不一致，fail-closed。
		if !hasCredential {
			return nil, ErrTokenInvalid
		}

		if tokenStore == nil {
			return nil, ErrTokenInvalid
		}
		// P2-2：这里的第二次 Resolve 与鉴权中间件读的是同一个带缓存的
		// tokenstore.Store，命中缓存时不触库；保留它是因为身份一致性能
		// 在组装阶段被独立复核（鉴权后被换凭据 → 立即 fail-closed）。
		meta, err := tokenStore.Resolve(ctx, auth.HashTokenKey(key))
		if err != nil {
			// 对外仍是 401（不区分存储故障与坏令牌：区分二者会变成状态探测信道，
			// relay-01 §3.6、security-01 I12 的 fail-closed 姿势），
			// 但保留 cause 供日志区分「Redis/DB 故障」与「令牌不存在」两种排障场景。
			return nil, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
		}
		if meta == nil {
			return nil, ErrTokenInvalid
		}
		// 身份一致性：TokenID/UserID 来自 Principal，分组来自本次 Resolve。
		// 两次解析必须是同一令牌/同一用户；不一致说明凭据在鉴权后被替换或存储
		// 返回了错误行——混用会让分组（进而倍率/定价）落到别人名下，必须 fail-closed。
		if meta.ID != *principal.TokenID || meta.UserID != principal.UserID {
			return nil, ErrTokenInvalid
		}
		// 判定顺序遵循 relay-01 §3.3 → §3.4：令牌状态先于附加校验。
		// pin 是「管理员运维能力」，非管理员使用一律 403（§3.5，R-P2a）。
		if hasPin && !isAdmin(principal.Role) {
			return nil, ErrPinForbidden
		}

		rc.TokenID = tokenID
		// §3.17.2 演示账号识别：把令牌的不限额度投影带进 RelayContext，
		// 计费结算据此判断「演示模式 + unlimited_quota」的免单口径。非演示
		// 模式下该字段不参与任何判定。
		rc.TokenUnlimitedQuota = meta.UnlimitedQuota
		// R-61：group=auto 在鉴权层解析成「有序分组序列」。
		//   - 空列表 → fail-closed，绝不回退用户分组（I-20）；
		//   - 否则 UsingGroup = 有序第一项，全表写进 rc.AutoGroups 供路由层换组；
		//   - UserGroup 保持令牌分组的**字面值**（"auto"）：它是令牌投影占位，
		//     定价与日志只消费 UsingGroup（本任务不改 UserGroup 语义）。
		// 会话路径（TokenID == nil）没有自动分组语义，两个字段保持零值。
		if meta.Group == groupAuto {
			if len(meta.AutoGroups) == 0 {
				return nil, ErrAutoGroupsNotEnabled
			}
			rc.UsingGroup = meta.AutoGroups[0]
			rc.UserGroup = meta.Group
			rc.AutoGroups = append([]string(nil), meta.AutoGroups...)
			rc.CrossGroupRetry = meta.CrossGroupRetry
		} else {
			// 控制器裁决：Bearer 路径从 TokenMeta.Group 同时填 UsingGroup 与 UserGroup。
			// UserGroup 在此只是占位：TokenMeta 是令牌投影而非用户行，真正的用户分组
			// 必须由 Task 8 在加载用户后覆盖再进入定价（relay-01 §7.1 定义 UserGroup 为用户分组）。
			rc.UsingGroup = meta.Group
			rc.UserGroup = meta.Group
		}
	} else if hasPin && !isAdmin(principal.Role) {
		// 会话路径不做令牌查询；但 pin 仍要在这里拒绝：路由层的 ParseChannelPin
		// 只看请求头、不看身份来源，放行等于让非管理员会话用户绕过 §3.5。
		return nil, ErrPinForbidden
	}

	// 尽力而为的预读：失败不影响组装，权威值由归一化层从完整请求体重新解析。
	rc.Streaming = peekStreaming(req)
	return rc, nil
}

// ParseChannelPin 返回 legacy 令牌后缀指定的渠道 id（relay-01 §3.5）。
//
// 只有 legacy sk-<key>-<channelId> 形态带 pin；规范 sk-<48> 与历史 nkt-<48> 整段不切分（§3.2、G7-R51）。
// 返回 (0, false) 表示没有 pin，调用方走普通路由。
//
// 控制器裁决 R-P2a：pin 不写进 RelayContext（它属于路由约束而非身份状态），
// 由路由层作为 ChannelConstraints.PinChannelID 的输入消费；非管理员使用 pin 的
// 403 语义在 BuildRelayContext 中体现为 ErrPinForbidden，HTTP 映射归 Task 8。
//
// 本函数只看凭据、不看身份：它无法判断调用者是管理员还是会话用户。因此路由层
// 必须在 BuildRelayContext 通过之后消费 pin——后者已在 Bearer 与 Cookie 两条
// 身份路径上统一执行了非管理员拒绝（R-P2a、relay-01 §3.5）。
func ParseChannelPin(req *http.Request) (channelID int, ok bool) {
	if req == nil {
		return 0, false
	}
	raw, ok := credentialFromRequest(req)
	if !ok {
		return 0, false
	}
	_, pinID, hasPin := normalizeTokenCredential(raw)
	return pinID, hasPin
}

// credentialFromRequest 提取请求携带的令牌明文（relay-01 §3.1 的规范载体）。
//
// Authorization 头永远生效：剥离 Bearer 前缀后使用，裸 key 直用。
// x-api-key 只在 /v1/messages 或 /v1/models 路径生效（§3.1 的载体表）。
// ?key= / x-goog-api-key / mj-api-secret / WS 子协议等兼容载体由鉴权中间件
// 归一化后再调用本函数——组装层不重复实现载体规则，也不碰需要日志脱敏的查询参数。
func credentialFromRequest(r *http.Request) (string, bool) {
	if v := strings.TrimSpace(r.Header.Get("Authorization")); v != "" {
		if i := strings.IndexByte(v, ' '); i >= 0 {
			// 只认 Bearer 前缀；带空格却不是 Bearer 的形态（Basic/自定义方案）
			// 不构成令牌凭据，继续尝试 x-api-key 兼容载体，而不是直接判定无凭据。
			if strings.EqualFold(v[:i], "Bearer") {
				if raw := strings.TrimSpace(v[i+1:]); raw != "" {
					return raw, true
				}
			}
		} else {
			// §3.1：裸 key 直用。
			return v, true
		}
	}

	path := ""
	if r.URL != nil {
		path = r.URL.Path
	}
	if strings.Contains(path, "/v1/messages") || strings.Contains(path, "/v1/models") {
		if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
			return v, true
		}
	}
	return "", false
}

// normalizeTokenCredential 按 relay-01 §3.2 归一化「用于查询的令牌 key」，
// 并按 §3.5 解析 legacy pin 后缀。规则顺序即语义：
//
//  1. 规范 sk-<48>：48 位 Base62 body 不含 '-'，按 legacy pin 规则切分后
//     仍得到整段。
//  2. 历史 nkt-<48> 整段不切分。
//  3. legacy sk-<key>[-<channelId>] 的末段为纯数字时才作为渠道 id
//     （G7-R51 裁决，2026-10-17）。
//  4. 裸 key 直用。
//
// 注意：pin 后缀必须从查询 key 中剥掉，否则会拿 sk-<key>-<channelId> 去哈希，
// 永远查不到令牌。
func normalizeTokenCredential(raw string) (key string, pinID int, hasPin bool) {
	switch {
	case strings.HasPrefix(raw, "nkt-"):
		return raw, 0, false
	case strings.HasPrefix(raw, "sk-"):
		body := strings.TrimPrefix(raw, "sk-")
		if body == "" {
			return raw, 0, false
		}
		parts := strings.Split(body, "-")
		key = "sk-" + parts[0]
		if len(parts) >= 2 {
			if id, ok := parseChannelID(parts[len(parts)-1]); ok {
				return key, id, true
			}
		}
		return key, 0, false
	default:
		return raw, 0, false
	}
}

// parseChannelID 只接受正的十进制整数。
// 先逐字节判定纯数字再交给 strconv：Atoi 会接受 "+5" 这类形态，
// 那不是「末段为纯数字」的 pin 后缀（G7-R51），不能误判成渠道 id。
func parseChannelID(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// idToInt 把 M1 的 int64 主键落到契约冻结的 int 字段（控制器裁决 R-P2）。
//
// 32 位平台上 int 只有 32 位，越界转换会静默截断；截断后的 id 可能指向另一个
// 用户/令牌，属于身份混淆，必须显式拒绝而不是带病继续。
func idToInt(v int64) (int, error) {
	id := int(v)
	if int64(id) != v {
		return 0, ErrIdentityOutOfRange
	}
	return id, nil
}

// newRequestID 生成计费幂等键（relay-01 §7.1；契约 §5.4）。
//
// 格式选择：16 字节 crypto/rand 的小写十六进制（32 字符，128 位熵）。
// 不引入 UUID 依赖；不用时间戳/自增序号——RequestID 会进日志与幂等键，
// 可预测的 id 会泄露请求量与顺序，也可能被用于碰撞重放。
func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read 在支持的平台上不会失败；保留检查是为了不在熵源
		// 异常时静默退化成全零（可预测）id。
		return "", fmt.Errorf("relay: 生成 RequestID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// detectRawFormat 在归一化前按路径判定入站协议，供前置过滤器与分派使用。
//
// 覆盖 relay-04 的端点族：Claude 的 /v1/messages；Gemini 的 /v1beta/、
// /v1/models/ 下带 :action 的形态与 /v1/engines/；Responses 的 /v1/responses
// （relay-07）。其余 /v1/* 按 OpenAI 兼容处理（chat/completions、embeddings 等）。
func detectRawFormat(path string) contract.RelayFormat {
	switch {
	case strings.HasPrefix(path, "/v1/messages"):
		return FormatClaude
	case strings.HasPrefix(path, "/v1/responses"):
		return FormatOpenAIResponses
	case strings.HasPrefix(path, "/v1beta/openai/"):
		// Gemini 网关的 OpenAI 兼容路由：入站是 OpenAI 协议，不是 Gemini 原生
		// （relay-04 的 /v1beta/openai/models* 载体）。
		return FormatOpenAI
	case strings.HasPrefix(path, "/v1beta/"),
		strings.HasPrefix(path, "/v1/engines/"),
		strings.HasPrefix(path, "/v1/models/") && strings.ContainsRune(path, ':'):
		return FormatGemini
	case strings.Contains(path, ":generateContent"),
		strings.Contains(path, ":streamGenerateContent"),
		strings.Contains(path, ":embedContent"),
		strings.Contains(path, ":batchEmbedContents"):
		return FormatGemini
	default:
		return FormatOpenAI
	}
}

// peekStreaming 从请求体 JSON 的 stream 字段读取流式标志。
//
// 请求体只读前 maxPeekBytes，并立即把「已读前缀 + 未读余段」拼回去：
// 下游 handler/归一化层必须还能读到完整 body，否则协议解析会拿到空体。
//
// 解析失败（非 JSON、multipart、超限截断）一律按 false 处理且不报错：
// 这是尽力而为的预读，Streaming 的权威值由归一化层在拿到完整请求后写入
// （relay-01 §7.2）。用 json.Decoder 而不是 json.Unmarshal，是为了在超大
// 请求体被截断时仍能取到位于前缀里的 stream 字段。
func peekStreaming(req *http.Request) bool {
	if req.Body == nil {
		return false
	}
	original := req.Body
	prefix, _ := io.ReadAll(io.LimitReader(original, maxPeekBytes))
	req.Body = &peekedBody{
		Reader:   io.MultiReader(bytes.NewReader(prefix), original),
		original: original,
	}
	if len(prefix) == 0 {
		return false
	}
	var payload struct {
		Stream bool `json:"stream"`
	}
	_ = json.NewDecoder(bytes.NewReader(prefix)).Decode(&payload)
	return payload.Stream
}

// peekedBody 把「已读取的前缀 + 原始余段」重新拼成请求体，并把 Close 透传给
// 原始 body：net/http 依赖 body 被关闭来复用连接，包装后丢失 Close 会泄漏连接。
type peekedBody struct {
	io.Reader
	original io.ReadCloser
}

func (b *peekedBody) Close() error { return b.original.Close() }

// isAdmin 与 M1 的会话档位判定同口径（auth.profileForRole 只把 "admin" 视为管理员）。
// 不用 Permissions 判定：pin 是管理员运维能力，与细粒度权限无关（relay-01 §3.5）。
func isAdmin(role string) bool { return role == "admin" }

// ============================================================================
// 字段写权 helper（控制器裁决 R-P1a）
//
// Go 禁止在外部包的类型上定义方法，因此 contract.RelayContext 的写权以本包的
// 自由函数表达，而不是 `(rc *RelayContext) WithXxx(...)`。每个 helper 只写
// relay-01 §7 表里标注给同一写入方的字段组，调用点据此保持「一个字段一个写入方」。
// ============================================================================

// WithIdentity 写身份组（唯一写入方：鉴权层，relay-01 §7.1）。
func WithIdentity(rc *contract.RelayContext, requestID string, userID, tokenID int, usingGroup, userGroup string) {
	rc.RequestID = requestID
	rc.UserID = userID
	rc.TokenID = tokenID
	rc.UsingGroup = usingGroup
	rc.UserGroup = userGroup
}

// WithUsingGroup 只改写 UsingGroup：路由层在 auto 跨组重试时的唯一改写点（relay-01 §7.1）。
func WithUsingGroup(rc *contract.RelayContext, group string) {
	rc.UsingGroup = group
}

// WithRequest 写请求组（唯一写入方：归一化层，relay-01 §7.2）。
// OriginModel/Streaming 由归一化后的 IR 统一派生，避免与 Request 内的值漂移。
func WithRequest(rc *contract.RelayContext, req *contract.Request, rawFormat contract.RelayFormat, requestPath string) {
	rc.Request = req
	rc.RawFormat = rawFormat
	rc.RequestPath = requestPath
	if req != nil {
		rc.OriginModel = req.Model
		rc.Streaming = req.Stream
		// 契约字段从 RelayContext 单向复制：beta 的权威写入方是
		// BuildRelayContext 的 HTTP 边界；归一化层不重新解析请求头。
		req.AnthropicBeta = rc.AnthropicBeta
	}
}

// WithRoute 写路由组（唯一写入方：路由层，relay-01 §7.3；执行层只读）。
func WithRoute(rc *contract.RelayContext, plan *contract.AttemptPlan, attemptIndex int, lastErr *contract.RelayError) {
	rc.AttemptPlan = plan
	rc.AttemptIndex = attemptIndex
	rc.LastError = lastErr
}

// ChannelAssignment 是执行层每轮写渠道组所需的一次赋值（relay-01 §7.4）。
// 单独定义而不是复用 contract.ChannelSnapshot：后者是连接级会话快照，
// 语义与「本轮请求的渠道组」不同（且缺 MultiKeyIndex/CreatedTime）。
type ChannelAssignment struct {
	ChannelID     int
	ChannelType   contract.ChannelType
	APIBase       string
	Credential    string
	MultiKeyIndex int
	UpstreamModel string
	// UpstreamAccountID 是渠道归属的上游账号 id；0 = 未归属。
	UpstreamAccountID int64
	GroupRatio        float64
	CreatedTime       time.Time
	// AutoBan 是渠道级自动禁用开关（channels.auto_ban，默认 true）。
	// 它**不写入 RelayContext**：自动禁用判定属于执行层失败回调的入参，
	// 适配器/计费/日志都无需感知。执行层每轮从 assignment 直接读取，
	// 保证「本轮真正使用的渠道」的开关值就是判定用的值（R-M4-01 C）。
	AutoBan bool
	// AllowPrivateNetwork 是渠道级私网出站 opt-in（channels.allow_private_network，
	// R-01）。与 AutoBan 同理不写入 RelayContext：适配器/计费/日志无需感知，
	// 执行层只在本轮发起请求前用它给 context 打上 egress 标记。
	AllowPrivateNetwork bool
}

// WithChannel 一次性写渠道组（唯一写入方：执行层，每轮重试重写，relay-01 §7.4）。
//
// 整体赋值而不是让调用方逐字段设置：换渠道重试时必须同时清掉上一轮的
// MultiKeyIndex/UpstreamModel/Credential，漏字段会让适配器读到上一轮的凭据。
func WithChannel(rc *contract.RelayContext, ch ChannelAssignment) {
	rc.ChannelID = ch.ChannelID
	rc.ChannelType = ch.ChannelType
	rc.APIBase = ch.APIBase
	rc.Credential = ch.Credential
	rc.MultiKeyIndex = ch.MultiKeyIndex
	rc.UpstreamModel = ch.UpstreamModel
	rc.UpstreamAccountID = ch.UpstreamAccountID
	rc.GroupRatio = ch.GroupRatio
	rc.CreatedTime = ch.CreatedTime
}

// WithPriceSnapshot 写定价快照（唯一写入方：计费接缝，relay-01 §7.5）。
// 中继只持有与透传，不读字段（契约 §6.3）。
func WithPriceSnapshot(rc *contract.RelayContext, snap *contract.PriceSnapshot) {
	rc.PriceSnapshot = snap
}

// WithQuotaEstimate 写预扣估算输入（唯一写入方：计费接缝，relay-01 §7.5）。
func WithQuotaEstimate(rc *contract.RelayContext, est *contract.QuotaEstimate) {
	rc.QuotaEstimate = est
}

// WithUsageFact 写实际用量事实（唯一写入方：计费接缝，relay-01 §7.5）。
func WithUsageFact(rc *contract.RelayContext, fact *contract.UsageFact) {
	rc.UsageFact = fact
}

// WithQuotaClamp 写钳制事件：由产生钳制的一方写入，计费接缝汇总（relay-01 §7.5）。
func WithQuotaClamp(rc *contract.RelayContext, clamp *contract.QuotaClamp) {
	rc.QuotaClamp = clamp
}

// WithDiscount 写每请求折扣倍率（唯一写入方：计费接缝，relay-01 §7.5）。
func WithDiscount(rc *contract.RelayContext, rate float64) {
	rc.Discount = rate
}

// WithStream 写流式组（唯一写入方：执行层，relay-01 §7.6）。
// firstTokenMS 非流式必须传 nil，不得填占位值（契约 §7.2 甲类修正）。
func WithStream(rc *contract.RelayContext, status *contract.StreamStatus, sendCount, recvCount int, firstTokenMS *int) {
	rc.StreamStatus = status
	rc.SendCount = sendCount
	rc.RecvCount = recvCount
	rc.FirstTokenMS = firstTokenMS
}

// WithTask 写任务组（唯一写入方：任务路径，relay-01 §7.7）。
func WithTask(rc *contract.RelayContext, handle *contract.TaskHandle, originTaskIDs []string, lockedChannelID int) {
	rc.TaskHandle = handle
	rc.OriginTaskIDs = originTaskIDs
	rc.LockedChannelID = lockedChannelID
}

// AppendConversion 追加一步协议转换审计（审计组是追加式，relay-01 §7.8）。
func AppendConversion(rc *contract.RelayContext, step contract.ConversionStepEntry) {
	rc.ConversionChain = append(rc.ConversionChain, step)
}

// AppendOverride 追加一条参数/请求头覆盖审计（追加式，relay-01 §7.8）。
func AppendOverride(rc *contract.RelayContext, entry contract.OverrideAuditEntry) {
	rc.OverrideAudit = append(rc.OverrideAudit, entry)
}

// AppendDiagnostic 追加一条转换诊断（追加式，relay-01 §7.8）。
func AppendDiagnostic(rc *contract.RelayContext, entry contract.DiagnosticEntry) {
	rc.Diagnostics = append(rc.Diagnostics, entry)
}
