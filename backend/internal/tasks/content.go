package tasks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"newtg/gateway/internal/plugin"
)

// 本文件是产物内容（artifacts content）的宿主侧实现（relay-06 §10.5）：
//   - 能力 URL 的 HMAC 签发与校验；
//   - 插件 buildContentRequest → 宿主出站 → **流式**返回；
//   - access 参数的日志脱敏工具。
//
// 设计要点：产物不落第二份存储，清单仍从 task.data 投影；content URL 只是
// 「持票即可取」的能力凭证，票据本身不携带任何产物内容。

const (
	// artifactAccessVersion 是 access 票据的版本前缀（便于未来换算法/轮换）。
	artifactAccessVersion = "v1"

	// artifactAccessMessagePrefix 是 HMAC 消息的域分隔前缀。
	//
	// 没有域分隔时，同一把主密钥若被其它功能复用，攻击者可以把一个合法
	// 签名搬到另一种语义上（签名混淆）。前缀把票据绑定到「产物内容」这一个
	// 用途。
	artifactAccessMessagePrefix = "newtg/task-artifact-content/v1\x00"
)

// artifactKeyPattern 是产物 key 的字符集（T6b 冻结）。
//
// 为什么限制：artifact_key 是 URL 路径段；含 `/`、`%`、控制字符的 key 会在
// Go ServeMux 的路径解码（按解码后路径匹配）与 PathEscape 之间产生歧义，
// 也可能被用来构造跨段路由。含 `/` 等非法字符直接 400，不再尝试转义。
var artifactKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// ValidArtifactKey 报告 key 是否落在冻结字符集内。
func ValidArtifactKey(key string) bool { return artifactKeyPattern.MatchString(key) }

// ArtifactSigner 签发/校验产物能力 URL。
//
// 密钥从现有 CREDENTIAL_ENCRYPTION_KEY 派生（brief 冻结：不新增明文配置）。
// 派生而不是直接复用主密钥：文件内容的签名域与渠道凭据加密域必须分开，
// 且**轮换主密钥即可整体撤销**所有已签发 URL（设计要求的撤销手段）。
type ArtifactSigner struct {
	key []byte
	ttl time.Duration
}

// NewArtifactSigner 构造签发器。ttl<=0 表示长期有效（设计 §10.5 冻结语义）；
// ttl>0 只作为未来可配置的收窄位，生产装配保持 0。
func NewArtifactSigner(secret string, ttl time.Duration) *ArtifactSigner {
	return &ArtifactSigner{key: deriveArtifactKey(secret), ttl: ttl}
}

func deriveArtifactKey(secret string) []byte {
	sum := sha256.Sum256([]byte(artifactAccessMessagePrefix + secret))
	return sum[:]
}

// Sign 为一个产物签发 access 票据。
//
// 消息含 pluginKey：能力 URL 的路径段带插件 key，若签名不绑定它，同一个
// task_id 的票据可以在另一个插件路径下重放（处理器虽会核对归属，但让签名
// 本身失效是更早、更便宜的一道闸）。
func (s *ArtifactSigner) Sign(pluginKey, taskID, artifactKey string, now time.Time) string {
	expiry := int64(0)
	if s != nil && s.ttl > 0 {
		expiry = now.Add(s.ttl).Unix()
	}
	mac := s.mac(pluginKey, taskID, artifactKey, expiry)
	return artifactAccessVersion + "." + strconv.FormatInt(expiry, 10) + "." +
		base64.RawURLEncoding.EncodeToString(mac)
}

// Verify 校验票据。任何格式错误、签名不符或已过期都返回 false。
//
// 过期判断放在签名校验之后：先证明票据是我们签的，再讨论时效，避免让
// 未签名输入触发差异化错误路径。
func (s *ArtifactSigner) Verify(pluginKey, taskID, artifactKey, access string, now time.Time) bool {
	if s == nil || access == "" {
		return false
	}
	parts := strings.Split(access, ".")
	if len(parts) != 3 || parts[0] != artifactAccessVersion {
		return false
	}
	expiry, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want := s.mac(pluginKey, taskID, artifactKey, expiry)
	if !hmac.Equal(got, want) {
		return false
	}
	if expiry != 0 && now.Unix() > expiry {
		return false
	}
	return true
}

func (s *ArtifactSigner) mac(pluginKey, taskID, artifactKey string, expiry int64) []byte {
	m := hmac.New(sha256.New, s.key)
	// 字段之间用 NUL 分隔：任取两个字段都含不了 NUL，拼接不会产生歧义
	// （例如 task "a" + artifact "bc" 与 task "ab" + artifact "c"）。
	io.WriteString(m, artifactAccessMessagePrefix)
	io.WriteString(m, pluginKey)
	m.Write([]byte{0})
	io.WriteString(m, taskID)
	m.Write([]byte{0})
	io.WriteString(m, artifactKey)
	m.Write([]byte{0})
	io.WriteString(m, strconv.FormatInt(expiry, 10))
	return m.Sum(nil)
}

// ContentURL 构造宿主签发的产物内容地址。
//
// baseURL 为空时返回相对路径（单测/未配置公网基址）：不猜域名，避免把
// 请求 Host 头（可伪造）写进长期能力 URL。设计明确「网关不构造产物对外
// 地址」——这里构造的是**网关自己的** content 端点，产物真实地址仍由插件
// 在出站时给出。
func (s *ArtifactSigner) ContentURL(baseURL, pluginKey string, task *Task, artifactKey string, now time.Time) string {
	if s == nil || task == nil || artifactKey == "" {
		return ""
	}
	access := s.Sign(pluginKey, task.PublicTaskID, artifactKey, now)
	path := "/v1/tasks/" + url.PathEscape(pluginKey) + "/artifacts/" +
		url.PathEscape(artifactKey) + "/content"
	query := url.Values{}
	query.Set("task_id", task.PublicTaskID)
	query.Set("access", access)
	return strings.TrimRight(baseURL, "/") + path + "?" + query.Encode()
}

// RedactAccess 脱敏 URL/查询串里的 access 票据（relay-06 §10.5）。
//
// 能力 URL 等同于短期口令：一旦进日志，任何能看到日志的人都能取产物。
// 这里同时处理原始查询串与完整 URL；解析失败时退回保守的整串替换，
// 绝不把原文留在日志里。
func RedactAccess(raw string) string {
	if raw == "" {
		return raw
	}
	if strings.Contains(raw, "access=") {
		if u, err := url.Parse(raw); err == nil {
			q := u.Query()
			if q.Has("access") {
				q.Set("access", "REDACTED")
				u.RawQuery = q.Encode()
				return u.String()
			}
		}
		return "REDACTED"
	}
	return raw
}

// ContentInput 是一次产物内容代理所需的全部输入。
type ContentInput struct {
	Plugin         *plugin.CompiledPlugin
	Channel        ChannelTarget
	Model          string
	UpstreamModel  string
	UpstreamTaskID string
	ArtifactKey    string
	BaseURL        string
	APIKey         string
	Method         string // GET / HEAD
	AllowPrivate   bool   // 仅测试
}

// FetchContent 执行插件的 buildContentRequest 并流式出站。
//
// HEAD 请求把方法改成 HEAD：客户端只问「能不能取/多大」，宿主不应为了
// 返回头部就把整份产物下载一遍。
func FetchContent(ctx context.Context, client *http.Client, in ContentInput) (*StreamResponse, error) {
	if in.Plugin == nil {
		return nil, fmt.Errorf("tasks: content without a compiled plugin")
	}
	if !in.Plugin.HasHook("buildContentRequest") {
		return nil, fmt.Errorf("tasks: plugin %q does not export buildContentRequest", in.Plugin.Key)
	}
	if client == nil {
		client = NewOutboundClient()
	}
	method := strings.ToUpper(strings.TrimSpace(in.Method))
	if method == "" {
		method = http.MethodGet
	}
	ctxMap := buildContentContext(in)
	rawDesc, err := in.Plugin.Call(ctx, "buildContentRequest", ctxMap)
	if err != nil {
		return nil, fmt.Errorf("%w: buildContentRequest", ErrPluginHook)
	}
	desc, err := DecodeRequestDescriptor(rawDesc)
	if err != nil {
		return nil, err
	}
	if method == http.MethodHead {
		desc.Method = http.MethodHead
		desc.Body = nil
	}
	allowed := AllowedHosts(in.BaseURL, in.Plugin.Manifest().AllowedHosts)
	return DoOutboundStream(ctx, client, desc, allowed, in.AllowPrivate)
}

func buildContentContext(in ContentInput) map[string]any {
	authHeader, auth := "", ""
	if in.Plugin != nil {
		switch in.Plugin.Manifest().Auth.Type {
		case "api_key":
			authHeader, auth = "Authorization", "Bearer "+in.APIKey
		}
	}
	return map[string]any{
		"model":         in.Model,
		"upstreamModel": in.UpstreamModel,
		"baseUrl":       in.BaseURL,
		"apiKey":        in.APIKey,
		"authHeader":    authHeader,
		"auth":          auth,
		"taskId":        in.UpstreamTaskID,
		"artifactKey":   in.ArtifactKey,
	}
}
