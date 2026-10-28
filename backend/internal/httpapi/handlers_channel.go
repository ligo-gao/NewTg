package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/api/dto"
	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/channelhealth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// 本文件是 Task 8 phase 8A 的管理面渠道端点：
//
//	GET    /api/channels                  channels:read
//	POST   /api/channels                  channels:manage        + Idempotency-Key
//	      └ 请求体含 credentials / task_plugin_key / task_extend_plugin_keys
//	        时额外要求 channels:manage_sensitive
//	GET    /api/channels/{id}             channels:read
//	PATCH  /api/channels/{id}             channels:manage        + expected_version（R-P8）
//	      └ 同上：含敏感绑定字段时额外要求 channels:manage_sensitive
//	DELETE /api/channels/{id}             channels:manage_sensitive
//	PATCH  /api/channels/batch            channels:manage        {action, ids}
//	POST   /api/channels/batch-delete     channels:manage_sensitive + Idempotency-Key
//	POST   /api/channels/{id}/test        channels:manage
//	POST   /api/channels/{id}/credentials/reveal
//	                                      channels:manage_sensitive + 步进认证 + 限流
//
// 凭据揭示是「常规接口永不返回明文」的唯一例外（security-03 §2.10）：
// 只读、只授权本次请求（不写再认证窗口）、响应 no-store、审计只记元数据。
//
// 端点形状按 R-P5（modules-06 的 REST 形态，不是计划草案的 colon action）。
// 安全不变量（GC-6/GC-8）：响应只走白名单字段；base_url 与凭据明文永不出现；
// 错误体不含上游 URL、密钥或内部 SQL 文本。
//
// 字段级写授权（BOPLA，G-M6D-23 / security-02 §2.6.1）：凭据与任务插件绑定是
// 敏感字段分区，仅 channels:manage 不足以写入；命中时必须在任何校验通过/写库
// 之前整笔 403，绝不部分写入（见 checkSensitiveWriteFields）。

// ---------- 存储 / 权限 / 测试接缝 ----------

// ChannelStore 是 handler 依赖的渠道存储面；生产实现是 *store.ChannelRepo，
// 测试注入内存 fake（R-P10：单测不要求真 PG）。
type ChannelStore interface {
	List(ctx context.Context, filter store.ChannelFilter) ([]store.Channel, int, error)
	Get(ctx context.Context, id int64) (*store.Channel, error)
	Create(ctx context.Context, ch *store.Channel) (*store.Channel, error)
	Update(ctx context.Context, id int64, patch store.ChannelPatch, expectedVersion time.Time) (*store.Channel, error)
	Delete(ctx context.Context, id int64) error
	BatchStatus(ctx context.Context, ids []int64, status int16) (store.BatchResult, error)
	BatchDelete(ctx context.Context, ids []int64) (store.BatchResult, error)
	UpdateTestResult(ctx context.Context, id int64, at time.Time, success bool) error
}

// 编译期断言：phase 8B 的 wiring 直接传 *store.ChannelRepo / *store.TokenRepo；
// 接口漂移在编译期暴露，而不是运行时才发现方法签名不一致。
var (
	_ ChannelStore    = (*store.ChannelRepo)(nil)
	_ UserAccessStore = (*store.TokenRepo)(nil)
)

// ChannelMetricsStore 是渠道指标（F-06-11）的日志库读取面。生产实现是
// *store.ChannelMetricsRepo（查 usage_logs）；测试注入内存 fake 以验证
// handler 契约（缓存、窗口、权限、错误映射）而不连真日志库。
//
// 两个方法都接收 now 作为时钟接缝：桶边界必须由调用方决定，避免 handler
// 与仓储各自取时导致测试边界抖动。
type ChannelMetricsStore interface {
	Get(ctx context.Context, channelID int64, window string, now time.Time) (*store.ChannelMetrics, error)
	Summaries(ctx context.Context, channelIDs []int64, now time.Time) (map[int64]store.ChannelSummary, error)
}

// 编译期断言：production wiring 直接传 *store.ChannelMetricsRepo。
var _ ChannelMetricsStore = (*store.ChannelMetricsRepo)(nil)

// ChannelTester 是连通性测试接缝（modules-06 F-06-06）。phase 8B 的真实实现
// 负责构造适配器、以 max_tokens=1 发一次上游请求；本任务只约束 handler 的错误
// 映射（失败 → 泛化 RFC7807，绝不回传上游 URL/key）。
type ChannelTester func(ctx context.Context, ch *store.Channel, model string) (latencyMS int, err error)

// ChannelModelLister 是「从上游拉取支持模型」的接缝。生产实现是
// *channelhealth.ModelLister；测试注入内存实现，避免 HTTP 契约依赖真实上游。
//
// 用 ListDetailed 而不是 List：响应除了模型 ID，还要带上游声明的事实
// （显示名、上下文上限、端点等），由前端向导决定是否采纳。
type ChannelModelLister interface {
	ListDetailed(ctx context.Context, req channelhealth.ModelListRequest) ([]channelhealth.ModelInfo, error)
}

// ProbeFailureReporter 是手工连通性测试结果的自动禁用接缝（W7/M4-b / R-M4-15）。
//
// 生产实现是 *channelhealth.Reporter：与中继/体检共用同一判定
// （DecideProbeFailure），但**只禁不禁**——手工测试成功绝不触发自动恢复
// （R-M4-15 冻结）。实现方必须非阻塞：handler 在写响应前同步调用它。
type ProbeFailureReporter interface {
	ReportProbeFailure(ctx context.Context, channelID int64, autoBan bool, result channelhealth.ProbeResult)
}

// TaskPluginVersionLookup 是「有效 active 上传版本」的存在性读取面
// （生产实现 *store.TaskPluginRepo）。渠道绑定校验用它补足注册表的上传侧。
type TaskPluginVersionLookup interface {
	HasActiveVersion(ctx context.Context, key string) (bool, error)
}

// TaskPluginCatalog 判断 key 是否存在于任务插件注册表（relay-06 §1.3）：
// factory 层（编译期内置）∪ 有效 active 上传版本。渠道绑定必须是已注册 key；
// 是否当前可执行（enabled/编译成功）由运行时快照决定，不在此处判定。
type TaskPluginCatalog interface {
	Exists(ctx context.Context, key string) (bool, error)
}

type taskPluginCatalog struct {
	factory  map[string]struct{}
	versions TaskPluginVersionLookup
}

// NewTaskPluginCatalog 构造绑定校验用的注册表视图。versions 可以为 nil
// （纯 factory 装配/测试）。
func NewTaskPluginCatalog(factoryKeys []string, versions TaskPluginVersionLookup) TaskPluginCatalog {
	factory := make(map[string]struct{}, len(factoryKeys))
	for _, key := range factoryKeys {
		factory[key] = struct{}{}
	}
	return &taskPluginCatalog{factory: factory, versions: versions}
}

// Exists 先查 factory 层（命中即存在），未命中再查 active 上传版本。
// 查询失败必须向上传播（fail-closed：不能把「查不到」当成「不存在」，
// 否则一次 DB 抖动会让合法绑定被误拒为 400）。
func (c *taskPluginCatalog) Exists(ctx context.Context, key string) (bool, error) {
	if c == nil {
		return false, nil
	}
	if _, ok := c.factory[key]; ok {
		return true, nil
	}
	if c.versions == nil {
		return false, nil
	}
	return c.versions.HasActiveVersion(ctx, key)
}

// IdempotentResponse 是首次响应快照（status + body）。
type IdempotentResponse struct {
	Status int    `json:"status"`
	Body   []byte `json:"body"`
}

// IdempotencyClaimStatus 是 Claim 的三种结果。
type IdempotencyClaimStatus int

const (
	// IdempotencyClaimAcquired 表示本请求抢到了执行权（已写入 pending 标记）。
	IdempotencyClaimAcquired IdempotencyClaimStatus = iota
	// IdempotencyClaimReplay 表示同键已有完成响应，直接重放。
	IdempotencyClaimReplay
	// IdempotencyClaimInFlight 表示同键的另一个请求正在执行（pending 标记）。
	IdempotencyClaimInFlight
)

// IdempotencyClaim 是 Claim 的返回值。
type IdempotencyClaim struct {
	Status   IdempotencyClaimStatus
	Response IdempotentResponse // 仅 Replay 时有意义
	Token    string             // 仅 Acquired 时非空；Complete/Release 用它校验所有权
}

// IdempotencyStore 是幂等执行接缝（POST /api/channels、POST /api/channels/batch-delete）。
//
// 生命周期（M2 final review finding 2 / 二轮复审 finding 2）：
//  1. Claim：SET NX 写入 pending 标记（短 TTL IdempotencyPendingTTL）。
//     抢到 → Acquired（带 token）；同键已有完成响应 → Replay；同键 pending → InFlight。
//  2. Acquired 的请求执行 build()；2xx 时 Complete 用响应替换 pending（24h TTL），
//     非 2xx/错误时 Release 删除标记，让客户端修正参数后可以重试。
//  3. 另一个并发同键请求在 1 就已返回 InFlight，**不会**执行 build——这正是修复
//     「先执行后 SetIfAbsent 导致双方都执行」的关键。
//  4. 进程崩溃/panic 留下的 pending 标记最多存活 60s，不会把键锁 24h。
type IdempotencyStore interface {
	Claim(ctx context.Context, key string, ttl time.Duration) (IdempotencyClaim, error)
	Complete(ctx context.Context, key, token string, resp IdempotentResponse, ttl time.Duration) error
	Release(ctx context.Context, key, token string) error
}

// IdempotencyTTL 冻结 24h：**完成响应**的保留时长（重放窗口）。
// 过期后同键可再次执行。
const IdempotencyTTL = 24 * time.Hour

// IdempotencyPendingTTL 是 pending 标记的短 TTL（60s）。
//
// pending 只表示「某个请求正在执行」，不需要覆盖整个 24h 重放窗口。旧实现
// 用 IdempotencyTTL 写 pending：进程崩溃、panic（修复前无释放兜底）或
// finalize 失败留下的标记会把同一 Idempotency-Key 持续 409 卡住 24h。
// 60s 覆盖一次管理面写操作（建渠道/批量删除）的正常执行时间；真正开始执行
// 的请求在完成时立即用 Complete 替换标记，不会等满这个 TTL。
const IdempotencyPendingTTL = 60 * time.Second

// idempotencyFinalizeTimeout 是 Complete/Release 的独立超时。
// finalize 用 context.WithoutCancel(r.Context()) + 本上限：客户端断开
// 不应阻止标记清理或响应固化，但也不能让 Redis 故障无限阻塞请求返回。
const idempotencyFinalizeTimeout = 5 * time.Second

const idempotencyKeyPrefix = "newtg:idem:"

// idempotencyPendingPrefix 是 pending 标记的值前缀（后接随机 token）。
// 完成后的响应是 JSON 对象（以 '{' 开头），因此前缀判断不会与响应混淆。
const idempotencyPendingPrefix = "pending:"

// MemoryIdempotencyStore 是进程内实现：单测与文档用。
//
// 局限（必须在 8B wiring 时避免）：只在当前进程内生效，多副本部署不保证重放；
// 生产必须注入 NewRedisIdempotencyStore。作为 handler 在未注入时的兜底，
// 它至少能让单副本部署的重试不重复写库。
type MemoryIdempotencyStore struct {
	mu      sync.Mutex
	entries map[string]memoryIdempotencyEntry
}

type memoryIdempotencyEntry struct {
	response  IdempotentResponse
	pending   bool
	token     string
	expiresAt time.Time
}

// NewMemoryIdempotencyStore 构造进程内幂等存储。
func NewMemoryIdempotencyStore() *MemoryIdempotencyStore {
	return &MemoryIdempotencyStore{entries: map[string]memoryIdempotencyEntry{}}
}

func (s *MemoryIdempotencyStore) Claim(_ context.Context, key string, ttl time.Duration) (IdempotencyClaim, error) {
	if ttl <= 0 {
		ttl = IdempotencyTTL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key]; ok {
		if e.expiresAt.IsZero() || time.Now().Before(e.expiresAt) {
			if e.pending {
				return IdempotencyClaim{Status: IdempotencyClaimInFlight}, nil
			}
			return IdempotencyClaim{Status: IdempotencyClaimReplay, Response: cloneIdempotentResponse(e.response)}, nil
		}
		delete(s.entries, key)
	}
	token := newRelayRequestID()
	if token == "" {
		return IdempotencyClaim{}, errors.New("httpapi: cannot generate idempotency claim token")
	}
	s.entries[key] = memoryIdempotencyEntry{
		pending:   true,
		token:     token,
		expiresAt: time.Now().Add(ttl),
	}
	return IdempotencyClaim{Status: IdempotencyClaimAcquired, Token: token}, nil
}

func (s *MemoryIdempotencyStore) Complete(_ context.Context, key, token string, resp IdempotentResponse, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = IdempotencyTTL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	// token 不匹配说明标记已过期并被另一个请求重新认领：不得覆盖别人的会话。
	if !ok || !e.pending || e.token != token {
		return nil
	}
	s.entries[key] = memoryIdempotencyEntry{
		response:  cloneIdempotentResponse(resp),
		expiresAt: time.Now().Add(ttl),
	}
	return nil
}

func (s *MemoryIdempotencyStore) Release(_ context.Context, key, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key]; ok && e.pending && e.token == token {
		delete(s.entries, key)
	}
	return nil
}

func cloneIdempotentResponse(r IdempotentResponse) IdempotentResponse {
	return IdempotentResponse{Status: r.Status, Body: append([]byte(nil), r.Body...)}
}

// RedisIdempotencyStore 是生产实现：pending 标记用短 TTL
// （IdempotencyPendingTTL），完成响应用 IdempotencyTTL（24h）。
//
// Claim 用 SET NX 抢占；Complete/Release 用 Lua 比较 token 后原子替换/删除，
// 避免 TTL 过期后另一个请求已重新认领时被旧请求覆盖（CAS 语义）。
// Redis 故障时返回错误；handler 选择 fail-open（见 serveIdempotent），
// 因为幂等存储不可用不应阻断创建/删除，重复风险由 channels_name_uq 等约束兜底。
type RedisIdempotencyStore struct {
	client *redis.Client
}

// NewRedisIdempotencyStore 构造 Redis 幂等存储；phase 8B 从 platform 资源注入。
func NewRedisIdempotencyStore(client *redis.Client) *RedisIdempotencyStore {
	return &RedisIdempotencyStore{client: client}
}

// completeIdempotencyScript：仅当当前值仍是自己写入的 pending 标记时才替换。
var completeIdempotencyScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
	redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
	return 1
end
return 0`)

// releaseIdempotencyScript：仅删除自己持有的 pending 标记。
var releaseIdempotencyScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
	redis.call('DEL', KEYS[1])
	return 1
end
return 0`)

func (s *RedisIdempotencyStore) Claim(ctx context.Context, key string, ttl time.Duration) (IdempotencyClaim, error) {
	if s == nil || s.client == nil {
		return IdempotencyClaim{}, errors.New("httpapi: redis idempotency store is not configured")
	}
	if ttl <= 0 {
		ttl = IdempotencyTTL
	}
	token := newRelayRequestID()
	if token == "" {
		return IdempotencyClaim{}, errors.New("httpapi: cannot generate idempotency claim token")
	}
	redisKey := idempotencyKeyPrefix + key
	pendingValue := idempotencyPendingPrefix + token

	claimed, err := s.client.SetNX(ctx, redisKey, pendingValue, ttl).Result()
	if err != nil {
		return IdempotencyClaim{}, err
	}
	if claimed {
		return IdempotencyClaim{Status: IdempotencyClaimAcquired, Token: token}, nil
	}

	raw, err := s.client.Get(ctx, redisKey).Bytes()
	if errors.Is(err, redis.Nil) {
		// 两次命令之间过期：再抢一次，仍失败就返回错误而不是伪造结果。
		claimed, err = s.client.SetNX(ctx, redisKey, pendingValue, ttl).Result()
		if err != nil {
			return IdempotencyClaim{}, err
		}
		if claimed {
			return IdempotencyClaim{Status: IdempotencyClaimAcquired, Token: token}, nil
		}
		return IdempotencyClaim{}, errors.New("httpapi: idempotency key vanished during claim")
	}
	if err != nil {
		return IdempotencyClaim{}, err
	}
	if strings.HasPrefix(string(raw), idempotencyPendingPrefix) {
		return IdempotencyClaim{Status: IdempotencyClaimInFlight}, nil
	}
	var resp IdempotentResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return IdempotencyClaim{}, err
	}
	return IdempotencyClaim{Status: IdempotencyClaimReplay, Response: resp}, nil
}

func (s *RedisIdempotencyStore) Complete(ctx context.Context, key, token string, resp IdempotentResponse, ttl time.Duration) error {
	if s == nil || s.client == nil {
		return errors.New("httpapi: redis idempotency store is not configured")
	}
	if ttl <= 0 {
		ttl = IdempotencyTTL
	}
	payload, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	_, err = completeIdempotencyScript.Run(ctx, s.client,
		[]string{idempotencyKeyPrefix + key},
		idempotencyPendingPrefix+token, payload, ttl.Milliseconds()).Result()
	return err
}

func (s *RedisIdempotencyStore) Release(ctx context.Context, key, token string) error {
	if s == nil || s.client == nil {
		return errors.New("httpapi: redis idempotency store is not configured")
	}
	_, err := releaseIdempotencyScript.Run(ctx, s.client,
		[]string{idempotencyKeyPrefix + key}, idempotencyPendingPrefix+token).Result()
	return err
}

// ---------- HTTP DTO ----------

// ChannelResponse 是 GC-6 白名单投影。列表与详情共用：
// 列表永不带 credentials_*（omitempty + 不赋值）；详情额外带掩码与「已设置」标记。
// 刻意不包含 base_url、credentials、model_mapping。
//
// auto_disabled_reason 是稳定 reason code（R-M4-05），不是秘密：列表与详情都返回，
// 前端按 code 本地化；为 null 时按仓内既有可空字段风格序列化为 JSON null。
type ChannelResponse struct {
	ID              int64    `json:"id"`
	Name            string   `json:"name"`
	Type            string   `json:"type"`
	Status          int16    `json:"status"`
	Priority        int16    `json:"priority"`
	Weight          int16    `json:"weight"`
	SupportedModels []string `json:"supported_models"`
	GroupTags       []string `json:"group_tags"` // null = 全部分组
	RateLimit       *int32   `json:"rate_limit"`
	AutoBan         bool     `json:"auto_ban"`
	TestModel       *string  `json:"test_model"`
	Remark          *string  `json:"remark"`
	Tag             *string  `json:"tag"`
	FailCount       int32    `json:"fail_count"`
	// AutoDisabledReason 是系统自动禁用的稳定 code（R-M4-05）；非自动禁用时为 nil。
	// 展示时间复用 updated_at，不新增 auto_disabled_at 列（R-M4-05）。
	AutoDisabledReason *string `json:"auto_disabled_reason"`
	LastTestAt         *string `json:"last_test_at"`
	LastTestResult     *bool   `json:"last_test_result"`
	// 任务插件绑定（M6d Task 7a / relay-06 §1.3）。两者互斥，按渠道类型
	// 投影：类型 61 单绑、类型 60 多绑、其余渠道均为空。它们不是秘密。
	TaskPluginKey        *string  `json:"task_plugin_key"`
	TaskExtendPluginKeys []string `json:"task_extend_plugin_keys"`
	// 上游账号归属（上游用量对账设计 §4.1）；nil 序列化为 null（未归属）。
	UpstreamAccountID *int64 `json:"upstream_account_id"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
	Version           string `json:"version"` // R-P8：derived updated_at（RFC3339Nano UTC）

	CredentialsMasked *ChannelCredentialsMasked `json:"credentials_masked,omitempty"`
	CredentialsSet    *bool                     `json:"credentials_set,omitempty"`

	// Metrics 是列表内嵌的 24h 概要（W16-1；modules-06 F-06-11 列表列）。
	//
	// 可选：日志库不可用时整批省略（nil），前端列显示 —，渠道管理主功能
	// 不受影响（F-06-11 异常流程）。批量成功时每个渠道都带对象——无流量
	// 渠道是 {request_count:0, success_rate:null, avg_latency_ms:null}，
	// 以便区分「0 次请求」与「指标不可用」。详情端点不返回本字段。
	Metrics *ChannelListMetrics `json:"metrics,omitempty"`
}

// ChannelListMetrics 是渠道行的 24h 概要（列表批量查询投影）。
// success_rate / avg_latency_ms 为 nil 表示该窗口无样本（显示 —）。
type ChannelListMetrics struct {
	RequestCount int64    `json:"request_count"`
	SuccessRate  *float64 `json:"success_rate"`
	AvgLatencyMS *float64 `json:"avg_latency_ms"`
}

type channelListResponse struct {
	Items    []ChannelResponse `json:"items"`
	Total    int               `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
}

type createChannelRequest struct {
	Name    string  `json:"name"`
	Type    string  `json:"type"`
	BaseURL *string `json:"base_url"`
	// Credentials 用 RawMessage 而不是 *ChannelCredentials：DTO 级
	// DisallowUnknownFields 只会把嵌套未知字段报成顶层 unknown field（403），
	// 而 G-M6D-23 冻结的口径是嵌套凭据未知字段 400。原始载荷交给
	// decodeCredentialsPayload 做严格解码，才能把两种拒绝区分开。
	Credentials     json.RawMessage `json:"credentials"`
	SupportedModels []string        `json:"supported_models"`
	GroupTags       json.RawMessage `json:"group_tags"`
	ModelMapping    json.RawMessage `json:"model_mapping"`
	Weight          *int16          `json:"weight"`
	Priority        *int16          `json:"priority"`
	RateLimit       json.RawMessage `json:"rate_limit"`
	Status          *int16          `json:"status"` // 创建恒为 1；此字段仅为兼容前端全量表单
	AutoBan         *bool           `json:"auto_ban"`
	TestModel       *string         `json:"test_model"`
	Remark          *string         `json:"remark"`
	Tag             *string         `json:"tag"`
	// 任务插件绑定（relay-06 §1.3）：类型 61 必填 key；类型 60 必填
	// extend 数组；其余类型两者都必须缺省/为空。
	//
	// 两个字段都用 RawMessage 而不是 *string / []string：BOPLA 的敏感门按
	// “字段是否出现”判定（G-M6D-23 复核 I-1/M-1），显式 null 与缺省必须
	// 可区分；同时避免 DTO 解码把 null 折叠成零值。严格解码在
	// channelFromCreate 内完成，错误统一 400。
	TaskPluginKey        json.RawMessage `json:"task_plugin_key"`
	TaskExtendPluginKeys json.RawMessage `json:"task_extend_plugin_keys"`
	// 上游账号归属（上游用量对账设计 §4.1）：null/缺省 = 未归属。
	UpstreamAccountID *int64 `json:"upstream_account_id"`
}

type updateChannelRequest struct {
	ExpectedVersion string          `json:"expected_version"`
	Name            *string         `json:"name"`
	Type            *string         `json:"type"`
	BaseURL         json.RawMessage `json:"base_url"`
	Credentials     json.RawMessage `json:"credentials"`
	SupportedModels *[]string       `json:"supported_models"`
	GroupTags       json.RawMessage `json:"group_tags"`
	ModelMapping    json.RawMessage `json:"model_mapping"`
	Weight          *int16          `json:"weight"`
	Priority        *int16          `json:"priority"`
	RateLimit       json.RawMessage `json:"rate_limit"`
	Status          *int16          `json:"status"`
	AutoBan         *bool           `json:"auto_ban"`
	TestModel       json.RawMessage `json:"test_model"`
	Remark          json.RawMessage `json:"remark"`
	Tag             json.RawMessage `json:"tag"`
	// RawMessage 区分「缺省」（不修改）、`null`（清空）与具体值。
	TaskPluginKey        json.RawMessage `json:"task_plugin_key"`
	TaskExtendPluginKeys json.RawMessage `json:"task_extend_plugin_keys"`
	// 上游账号归属：RawMessage 区分「缺省」（不修改）与 `null`（清空归属）。
	UpstreamAccountID json.RawMessage `json:"upstream_account_id"`
}

type batchStatusRequest struct {
	Action string  `json:"action"`
	IDs    []int64 `json:"ids"`
}

type batchDeleteRequest struct {
	IDs []int64 `json:"ids"`
}

type channelTestRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"` // 设计字段；当前实现固定非流式最小请求
}

type channelTestResponse struct {
	Success    bool   `json:"success"`
	LatencyMS  int    `json:"latency_ms"`
	Model      string `json:"model"`
	ErrorClass string `json:"error_class,omitempty"`
}

// revealCredentialsRequest 是凭据揭示的请求体（security-03 §2.10）。
//
// Password / TOTPCode 用 *string 而不是 string：设计冻结「恰好一个」因子，
// 缺省、显式 null 与空串都必须能与「提交了非空因子」区分开；用零值会
// 把「没给」和「给了空串」折叠成同一种请求，从而无法给出 422 字段级错误。
type revealCredentialsRequest struct {
	Reason   string  `json:"reason"`
	Password *string `json:"password"`
	TOTPCode *string `json:"totp_code"`
}

// ---------- Handler ----------

// ChannelHandlerOptions 是 phase 8B wiring 的注入点。
type ChannelHandlerOptions struct {
	Tester ChannelTester
	// ModelLister 是渠道表单「从上游获取模型」的出口。nil 时构造默认实现
	// （OpenAI 兼容 / Anthropic / Gemini 三类协议）。
	ModelLister ChannelModelLister
	Idempotency IdempotencyStore
	// ProbeReporter 是手工连通性测试的自动禁用接缝（W7/M4-b）。nil 时只回写
	// last_test_*、不做状态判定（测试/降级装配；生产必须注入）。
	ProbeReporter ProbeFailureReporter
	// Config 读取 rl.health 设置（阈值/关键词）。nil 时按注册表默认（阈值
	// 5s）。复核 M-6：手工测试的 last_test_result 与响应 success 必须与
	// 体检共用同一 ProbeSucceeded 判定（含 headers-only 阈值），避免
	// 「测试成功随后被自动禁用」。
	Config ConfigReader
	// Plugins 是任务插件注册表（渠道绑定校验）。nil 时任何 60/61 绑定都
	// 视为未注册（fail-closed）；生产装配必须注入。
	Plugins TaskPluginCatalog
	// Redis 是凭据揭示的管理员限流桶（reveal 桶：10 次/小时/管理员）。nil 时揭示
	// 端点一律 503（fail-closed）：限流是敏感操作的强制控制，不能因为漏装配
	// 而静默变成「无限次揭示」。
	Redis *redis.Client
	// Reauth 是凭据揭示的步进认证门（security-03 §2.10）。nil 时揭示端点
	// 一律 503（fail-closed），绝不退化成「仅权限」——权限与步进认证是两个
	// 独立门，缺一不可。
	Reauth *auth.ReauthGate
	// Audit 记录渠道写操作与凭据揭示（O-1 缺口 + security-03 §2.10）。nil 时
	// 跳过（仅未装配日志库的测试）；生产必须接线。
	Audit *audit.Logger
	// Metrics 是渠道指标的日志库读取面（W16-1 / F-06-11）。nil 时指标端点
	// 返回 503（日志库不可用），列表省略 metrics 字段——绝不伪造 0。
	Metrics ChannelMetricsStore
	// Performer 是渠道测速的执行接缝（模型性能测试）。nil 时 POST 测速
	// 返回 503，绝不伪造样本。
	Performer ChannelPerformer
	// Performance 是测速结果的主库存储面。nil 时测速端点返回 503。
	Performance store.ModelPerformanceStore
	// Now 是指标桶边界的时钟接缝；nil 时使用 time.Now。
	Now func() time.Time
	// Logger 记录幂等 finalize 失败等不可返回给客户端的内部错误；
	// nil 时使用 slog.Default()（不得静默丢弃错误）。
	Logger *slog.Logger
}

// ChannelHandler 持有渠道端点依赖。字段不导出，phase 8B 通过构造函数注入。
type ChannelHandler struct {
	channels      ChannelStore
	access        UserAccessStore
	sealer        *SecretSealer
	tester        ChannelTester
	modelLister   ChannelModelLister
	probeReporter ProbeFailureReporter
	config        ConfigReader
	plugins       TaskPluginCatalog
	idem          IdempotencyStore
	rdb           *redis.Client
	reauth        *auth.ReauthGate
	audit         *audit.Logger
	metrics       ChannelMetricsStore
	performer     ChannelPerformer
	performance   store.ModelPerformanceStore
	now           func() time.Time
	log           *slog.Logger
}

// NewChannelHandler 构造 handler。
//
// 不注入 Idempotency 时退回进程内实现并记录其局限（见 MemoryIdempotencyStore）；
// 不注入 sealer 是编程错误：静默使用空密钥会把 GC-8 降级为「可预测密钥」，
// 因此直接 panic。
func NewChannelHandler(channels ChannelStore, access UserAccessStore, sealer *SecretSealer, opts ChannelHandlerOptions) *ChannelHandler {
	if sealer == nil {
		panic("httpapi: NewChannelHandler requires a credential sealer (GC-8)")
	}
	idem := opts.Idempotency
	if idem == nil {
		idem = NewMemoryIdempotencyStore()
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	modelLister := opts.ModelLister
	if modelLister == nil {
		modelLister = channelhealth.NewModelLister()
	}
	return &ChannelHandler{
		channels:      channels,
		access:        access,
		sealer:        sealer,
		tester:        opts.Tester,
		modelLister:   modelLister,
		probeReporter: opts.ProbeReporter,
		config:        opts.Config,
		plugins:       opts.Plugins,
		idem:          idem,
		rdb:           opts.Redis,
		reauth:        opts.Reauth,
		audit:         opts.Audit,
		metrics:       opts.Metrics,
		performer:     opts.Performer,
		performance:   opts.Performance,
		now:           now,
		log:           logger,
	}
}

// Register 注册 Go 1.22 ServeMux 方法+通配符模式。
// 字面量 /batch 比 /{id} 更具体，ServeMux 会优先匹配前者。
func (h *ChannelHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/channels", h.handleList)
	mux.HandleFunc("POST /api/channels", h.handleCreate)
	mux.HandleFunc("POST /api/channels/fetch-models", h.handleFetchModels)
	mux.HandleFunc("PATCH /api/channels/batch", h.handleBatchStatus)
	mux.HandleFunc("POST /api/channels/batch-delete", h.handleBatchDelete)
	mux.HandleFunc("GET /api/channels/{id}", h.handleGet)
	mux.HandleFunc("PATCH /api/channels/{id}", h.handleUpdate)
	mux.HandleFunc("DELETE /api/channels/{id}", h.handleDelete)
	mux.HandleFunc("POST /api/channels/{id}/test", h.handleTest)
	// 渠道指标（W16-1 / F-06-11）：只读，权限与渠道读取同源。
	mux.HandleFunc("GET /api/channels/{id}/metrics", h.handleMetrics)
	// 渠道测速（模型性能测试）：POST 会真实消耗上游配额，与 /test 共用限流桶。
	mux.HandleFunc("POST /api/channels/{id}/performance-tests", h.handleCreatePerformanceTest)
	mux.HandleFunc("GET /api/channels/{id}/performance-tests", h.handleListPerformanceTests)
	// 凭据揭示是只读受控例外（security-03 §2.10）；路径按 G7-R13 资源命名。
	mux.HandleFunc("POST /api/channels/{id}/credentials/reveal", h.handleRevealCredentials)
}

// ---------- 列表 / 详情 ----------

func (h *ChannelHandler) handleList(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r, PermChannelsRead) {
		return
	}
	filter, e := parseChannelFilter(r)
	if e != nil {
		writeAPIError(w, e)
		return
	}
	items, total, err := h.channels.List(r.Context(), filter)
	if err != nil {
		writeAPIError(w, internalAPIError())
		return
	}
	out := channelListResponse{
		Items:    make([]ChannelResponse, 0, len(items)),
		Total:    total,
		Page:     filter.Page,
		PageSize: filter.PageSize,
	}
	for i := range items {
		resp, err := channelToResponse(&items[i])
		if err != nil {
			writeAPIError(w, internalAPIError())
			return
		}
		out.Items = append(out.Items, resp)
	}
	// W16-1：24h 概要随列表返回，但**一次**批量查询（绝不逐渠道 N+1）。
	// 失败时整批省略，列表仍 200（F-06-11 异常流程：指标不可用不阻断管理）。
	h.attachChannelSummaries(r.Context(), out.Items)
	writeChannelJSON(w, http.StatusOK, out)
}

// attachChannelSummaries 给列表页填 24h 概要。
//
// 所有渠道共用一次 Summaries 调用；无样本渠道填零值概要（request_count=0、
// 比率为 null），从而把「没流量」与「日志库不可用」（Metrics 保持 nil）区分开。
func (h *ChannelHandler) attachChannelSummaries(ctx context.Context, items []ChannelResponse) {
	if h.metrics == nil || len(items) == 0 {
		return
	}
	ids := make([]int64, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	summaries, err := h.metrics.Summaries(ctx, ids, h.now().UTC())
	if err != nil {
		// fail-open：指标是可选展示列，日志库故障不得让渠道管理整体 500。
		h.log.Warn("channel list metrics unavailable; omitting summaries", "error", err)
		return
	}
	for i := range items {
		s := summaries[items[i].ID]
		items[i].Metrics = &ChannelListMetrics{
			RequestCount: s.RequestCount,
			SuccessRate:  s.SuccessRate,
			AvgLatencyMS: s.AvgLatencyMS,
		}
	}
}

func (h *ChannelHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r, PermChannelsRead) {
		return
	}
	id, ok := channelIDFromRequest(r)
	if !ok {
		writeAPIError(w, channelNotFoundError())
		return
	}
	ch, err := h.channels.Get(r.Context(), id)
	if err != nil {
		writeAPIError(w, storeErrorResponse(err))
		return
	}
	resp, err := h.detailResponse(ch)
	if err != nil {
		writeAPIError(w, internalAPIError())
		return
	}
	writeChannelJSON(w, http.StatusOK, resp)
}

// ---------- 指标（F-06-11） ----------

// channelMetricsResponse 是 GET /api/channels/{id}/metrics 的响应。
//
// points 恒为 24 或 168 个整点桶（空桶补零/比率 null），summary 是窗口汇总；
// last_test_* 来自主库渠道行（不缓存在指标缓存里，避免测试结果被缓存拖旧）。
type channelMetricsResponse struct {
	ChannelID      int64                       `json:"channel_id"`
	Window         string                      `json:"window"`
	BucketSeconds  int                         `json:"bucket_seconds"`
	Points         []store.ChannelMetricsPoint `json:"points"`
	Summary        store.ChannelMetricsSummary `json:"summary"`
	LastTestAt     *string                     `json:"last_test_at"`
	LastTestResult *bool                       `json:"last_test_result"`
}

// channelMetricsCacheTTL 冻结 60s：直查模式下的成本与新鲜度折中。
// 设计原本要求 worker 预聚合到 Redis（modules-06:551）；W16-1 用「直查 +
// 60s 缓存」落地，读契约不变，后续 worker 写同一键即可切换（不做预聚合，
// 避免与 W17-2 调度框架顺序耦合）。
const channelMetricsCacheTTL = 60 * time.Second

// channelMetricsCacheKey 形状冻结为 newtg:channel_metrics:<id>:<window>:<bucket>。
//
// window 与 bucket 都必须进键：只按 id 缓存会让 24h/7d 互相命中（变异点 ①
// 的守护用例），bucket 进键为将来多粒度（如 5m）预留且让键自描述。
func channelMetricsCacheKey(channelID int64, window string) string {
	return fmt.Sprintf("newtg:channel_metrics:%d:%s:1h", channelID, window)
}

func (h *ChannelHandler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r, PermChannelsRead) {
		return
	}
	id, ok := channelIDFromRequest(r)
	if !ok {
		writeAPIError(w, channelNotFoundError())
		return
	}
	// 默认 24h；非法值 422 而不是静默回退（静默回退会让前端 Tab 与数据不一致）。
	window := strings.TrimSpace(r.URL.Query().Get("window"))
	if window == "" {
		window = store.ChannelMetricsWindow24h
	}
	buckets, ok := store.ChannelMetricsWindowBuckets(window)
	if !ok {
		writeAPIError(w, validationError("window", "window must be one of 24h or 7d"))
		return
	}
	// 渠道不存在必须 404，且先于日志库查询（不把 404 伪装成 503）。
	ch, err := h.channels.Get(r.Context(), id)
	if err != nil {
		writeAPIError(w, storeErrorResponse(err))
		return
	}
	if h.metrics == nil {
		// 日志库未装配：按「指标服务不可用」处理（F-06-11 异常流程）。
		h.log.Error("channel metrics requested but logs database is not configured", "channel_id", id)
		writeChannelMetricsUnavailable(w)
		return
	}
	metrics, err := h.loadChannelMetrics(r.Context(), id, window)
	if err != nil {
		// 日志库不可用 → 503 + RFC7807；主库渠道列表仍可用（测试矩阵 W16-1）。
		h.log.Error("load channel metrics failed", "channel_id", id, "window", window, "error", err)
		writeChannelMetricsUnavailable(w)
		return
	}
	resp := channelMetricsResponse{
		ChannelID:      id,
		Window:         window,
		BucketSeconds:  3600,
		Points:         metrics.Points,
		Summary:        metrics.Summary,
		LastTestAt:     formatChannelTimePtr(ch.LastTestAt),
		LastTestResult: ch.LastTestResult,
	}
	if resp.Points == nil {
		// 仓储契约保证非 nil；防御性兜底，避免 JSON 出现 points:null。
		resp.Points = make([]store.ChannelMetricsPoint, 0, buckets)
	}
	writeChannelJSON(w, http.StatusOK, resp)
}

// loadChannelMetrics 是「Redis 缓存 → 日志库直查 → 回填缓存」的读路径。
//
// Redis 故障一律 fail-open：读缓存失败/写缓存失败都只记 WARN，然后回落直查
// 并正常返回。指标是只读展示数据，不能让缓存层故障变成管理面 503。
func (h *ChannelHandler) loadChannelMetrics(ctx context.Context, channelID int64, window string) (*store.ChannelMetrics, error) {
	key := channelMetricsCacheKey(channelID, window)
	if h.rdb != nil {
		raw, err := h.rdb.Get(ctx, key).Bytes()
		switch {
		case err == nil:
			var cached store.ChannelMetrics
			if err := json.Unmarshal(raw, &cached); err == nil {
				return &cached, nil
			}
			// 缓存内容损坏（版本升级/手工写入）：丢弃并回源，不返回错误。
			h.log.Warn("channel metrics cache entry is corrupt; querying logs database",
				"channel_id", channelID, "window", window)
		case !errors.Is(err, redis.Nil):
			h.log.Warn("channel metrics cache read failed; querying logs database",
				"channel_id", channelID, "window", window, "error", err)
		}
	}
	metrics, err := h.metrics.Get(ctx, channelID, window, h.now().UTC())
	if err != nil {
		return nil, err
	}
	if h.rdb != nil {
		if raw, err := json.Marshal(metrics); err == nil {
			if err := h.rdb.Set(ctx, key, raw, channelMetricsCacheTTL).Err(); err != nil {
				h.log.Warn("channel metrics cache write failed",
					"channel_id", channelID, "window", window, "error", err)
			}
		}
	}
	return metrics, nil
}

// writeChannelMetricsUnavailable 输出指标服务不可用（503，RFC7807）。
func writeChannelMetricsUnavailable(w http.ResponseWriter) {
	writeAPIError(w, &apiError{
		Status: http.StatusServiceUnavailable,
		Type:   "https://newtg.dev/problems/metrics_unavailable",
		Title:  "Channel metrics unavailable",
	})
}

// ---------- 创建 ----------

func (h *ChannelHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r, PermChannelsManage) {
		return
	}
	h.serveIdempotent(w, r, func() (int, []byte, bool) {
		return h.createChannel(r)
	})
}

func (h *ChannelHandler) createChannel(r *http.Request) (int, []byte, bool) {
	var req createChannelRequest
	if err := decodeChannelJSON(r, &req); err != nil {
		if field, ok := unknownJSONField(err); ok {
			return fieldNotWritableResponse(field)
		}
		return errorResponse(invalidRequestError("request body is not valid JSON"))
	}

	// G-M6D-23：凭据/插件绑定属于 credential 字段分区。权限门必须先于
	// 任何业务校验与写库——低权限管理员携带这些字段时整笔 403，
	// 不得先校验通过再在写库处拒绝（那会留下部分写入或歧义响应）。
	if apiErr := h.checkSensitiveWriteFields(r.Context(), req.Credentials != nil, req.TaskPluginKey != nil, req.TaskExtendPluginKeys != nil); apiErr != nil {
		return errorResponse(apiErr)
	}

	v := &validator{}
	ch, apiErr := h.channelFromCreate(r.Context(), v, &req)
	if apiErr != nil {
		return apiErr.Status, apiErr.marshal(), false
	}
	if e := v.err(); e != nil {
		return e.Status, e.marshal(), false
	}

	created, err := h.channels.Create(r.Context(), ch)
	if err != nil {
		return errorResponse(storeErrorResponse(err))
	}
	// O-1：渠道写操作必须留审计。detail 只含 id/name 与「凭据已写入」布尔，
	// 绝不记录凭据值、密文或 base_url（审计保留 12 个月，不能成为密钥副本）。
	h.writeChannelAuditEvent(r, audit.ActionChannelCreate, http.StatusCreated, "success", map[string]any{
		"channel_id":          created.ID,
		"name":                created.Name,
		"credentials_written": len(created.Credentials) > 0,
	})
	resp, err := h.detailResponse(created)
	if err != nil {
		return errorResponse(internalAPIError())
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return errorResponse(internalAPIError())
	}
	return http.StatusCreated, body, true
}

// channelFromCreate 校验并构造待插入行；所有用户可见的校验错误写入 v。
// 第二个返回值只在注册表查询失败等「请求本身没问题但服务端无法完成校验」
// 的场景非空（映射为 500，而不是把 DB 故障伪装成 422）。
func (h *ChannelHandler) channelFromCreate(ctx context.Context, v *validator, req *createChannelRequest) (*store.Channel, *apiError) {
	name := strings.TrimSpace(req.Name)
	if name == "" || utf8.RuneCountInString(name) > 64 {
		v.add("name", "name is required and must be at most 64 characters")
	}

	ct := contract.ChannelType(req.Type)
	if !ct.Valid() {
		// modules-06 §1.3：60 类型白名单唯一真源是 contract.ChannelType.Valid()。
		v.add("type", "unknown channel type")
	}

	var baseURL *string
	rawBase := ""
	if req.BaseURL != nil {
		rawBase = strings.TrimSpace(*req.BaseURL)
	}
	if rawBase != "" {
		if msg := validateBaseURL(rawBase); msg != "" {
			v.add("base_url", msg)
		} else {
			baseURL = &rawBase
		}
	}
	if requiresBaseURL(ct) && baseURL == nil {
		// relay-02 §7.1「必填」组：azure_openai/custom/sunoapi/volcengine/
		// doubao_video/newapi/task_plugin/vllm/sglang。
		v.add("base_url", "base_url is required for this channel type")
	}

	models, modelsJSON := validateSupportedModels(v, req.SupportedModels)
	groupTags, _ := parseGroupTags(v, req.GroupTags)
	mapping, _ := parseModelMappingCreate(v, req.ModelMapping)
	// F-06-10：映射键是客户端可见别名，必须在 supported_models 里；否则
	// /v1/models 宣告的模型在路由层永远不会被 Candidates 命中（finding 3）。
	validateModelMappingCoverage(v, mapping, models)
	weight := int16(1)
	if req.Weight != nil {
		weight = *req.Weight
	}
	if weight < 0 || weight > 1000 {
		v.add("weight", "weight must be between 0 and 1000")
	}
	priority := int16(0)
	if req.Priority != nil {
		priority = *req.Priority
	}
	if priority < -1000 || priority > 1000 {
		v.add("priority", "priority must be between -1000 and 1000")
	}
	rateLimit, _ := parseRateLimit(v, req.RateLimit)
	autoBan := true
	if req.AutoBan != nil {
		autoBan = *req.AutoBan
	}
	// 上游账号归属：null/缺省 = 未归属；显式值必须是正整数（不存在的 id 由
	// channels_upstream_account_fk 兜底）。
	if req.UpstreamAccountID != nil && *req.UpstreamAccountID <= 0 {
		v.add("upstream_account_id", "upstream_account_id must be a positive integer or null")
	}

	testModel := normalizeOptionalChannelText(req.TestModel)
	if testModel != nil {
		if len(models) == 0 || !slices.Contains(models, *testModel) {
			// DB 的 channels_test_model_in_models_chk 兜底；这里提前给字段级错误。
			v.add("test_model", "test_model must be one of supported_models")
		}
	}
	remark := normalizeOptionalChannelText(req.Remark)
	if remark != nil && utf8.RuneCountInString(*remark) > 255 {
		v.add("remark", "remark must be at most 255 characters")
	}
	tag := normalizeOptionalChannelText(req.Tag)
	if tag != nil && utf8.RuneCountInString(*tag) > 32 {
		v.add("tag", "tag must be at most 32 characters")
	}

	// 任务插件绑定（relay-06 §1.3）：类型决定形态，跨类型误填一律 400。
	// RawMessage 需要在这里严格解码：null 与缺省都规范化为“未提供”，
	// 非字符串/非字符串数组的载荷按 invalid_request 400 拒绝（与旧 DTO
	// 解码的 400 口径一致，但保留“字段是否出现”供敏感门使用）。
	rawPluginKey, keyErr := decodeOptionalTaskPluginKey(req.TaskPluginKey)
	if keyErr != nil {
		return nil, invalidRequestError("task_plugin_key must be a string or null")
	}
	extendInput, extendErr := decodeTaskExtendPluginKeys(req.TaskExtendPluginKeys)
	if extendErr != nil {
		return nil, invalidRequestError("task_extend_plugin_keys must be an array of strings or null")
	}
	pluginKey, extendKeys, bindErr := h.validatePluginBinding(ctx, ct, rawPluginKey, extendInput)
	if bindErr != nil {
		return nil, bindErr
	}

	var sealed []byte
	creds, hasCredentials, credErr := decodeCredentialsPayload(req.Credentials)
	if credErr != nil {
		// 未知嵌套字段是 BOPLA 的 fail-closed 形态（G-M6D-23）：旧实现
		// 静默忽略未知字段，拼错的 api_key 会退化成「没给密钥」，从而
		// 悄悄沿用旧值。其它形状错误沿用创建端点的 400 invalid_request。
		if _, ok := unknownJSONField(credErr); ok {
			return nil, unknownCredentialsFieldError()
		}
		return nil, invalidRequestError("credentials must be an object")
	}
	if !hasCredentials {
		v.add("credentials", "credentials are required")
	} else {
		v.addFields(validateCredentials(ct, creds))
		if len(v.fields) == 0 {
			sealed = h.sealCredentials(v, ct, creds)
		}
	}

	return &store.Channel{
		Name:                 name,
		Type:                 string(ct),
		BaseURL:              baseURL,
		Credentials:          sealed,
		SupportedModels:      modelsJSON,
		GroupTags:            groupTags,
		ModelMapping:         mapping,
		Weight:               weight,
		Priority:             priority,
		RateLimit:            rateLimit,
		Status:               1, // F-06-03：创建时 status 恒为 enabled(1)
		AutoBan:              autoBan,
		TestModel:            testModel,
		Remark:               remark,
		Tag:                  tag,
		TaskPluginKey:        pluginKey,
		TaskExtendPluginKeys: extendKeys,
		UpstreamAccountID:    req.UpstreamAccountID,
	}, nil
}

// ---------- 更新 ----------

func (h *ChannelHandler) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r, PermChannelsManage) {
		return
	}
	id, ok := channelIDFromRequest(r)
	if !ok {
		writeAPIError(w, channelNotFoundError())
		return
	}

	var req updateChannelRequest
	if err := decodeChannelJSON(r, &req); err != nil {
		if field, ok := unknownJSONField(err); ok {
			writeFieldNotWritableProblem(w, field)
			return
		}
		writeAPIError(w, invalidRequestError("request body is not valid JSON"))
		return
	}

	// G-M6D-23：字段级敏感写门必须先于 expected_version 校验与渠道读取，
	// 让无 channels:manage_sensitive 的调用者得到固定的 403（而不是先被
	// 版本/存在性校验带偏），并保证 store 零调用、无部分写入。
	if apiErr := h.checkSensitiveWriteFields(r.Context(), req.Credentials != nil, req.TaskPluginKey != nil, req.TaskExtendPluginKeys != nil); apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}

	// R-P8：PATCH 必须带 expected_version；缺失即 422，而不是退化成无条件覆盖。
	if strings.TrimSpace(req.ExpectedVersion) == "" {
		writeAPIError(w, validationError("expected_version", "expected_version is required"))
		return
	}
	expected, err := time.Parse(time.RFC3339Nano, req.ExpectedVersion)
	if err != nil {
		writeAPIError(w, validationError("expected_version", "expected_version must be an RFC3339Nano timestamp"))
		return
	}
	expected = expected.UTC()

	existing, err := h.channels.Get(r.Context(), id)
	if err != nil {
		writeAPIError(w, storeErrorResponse(err))
		return
	}

	v := &validator{}
	patch, apiErr := h.buildPatch(r.Context(), v, existing, &req)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	if e := v.err(); e != nil {
		writeAPIError(w, e)
		return
	}

	updated, err := h.channels.Update(r.Context(), id, patch, expected)
	if err != nil {
		writeAPIError(w, storeErrorResponse(err))
		return
	}
	// O-1：写审计只记「哪些字段被改」的字段名与凭据是否轮换，不记任何值。
	h.writeChannelAuditEvent(r, audit.ActionChannelUpdate, http.StatusOK, "success", map[string]any{
		"channel_id":          updated.ID,
		"fields":              changedChannelFields(&req),
		"credentials_written": patch.Credentials != nil,
	})
	resp, err := h.detailResponse(updated)
	if err != nil {
		writeAPIError(w, internalAPIError())
		return
	}
	writeChannelJSON(w, http.StatusOK, resp)
}

// buildPatch 把 PATCH 载荷（含 null/缺省区分）转成 store.ChannelPatch，
// 并对「合并后的有效状态」做校验（例如已有 azure 渠道只改 remark 时，
// 仍要保证 effective base_url 非空）。
func (h *ChannelHandler) buildPatch(ctx context.Context, v *validator, existing *store.Channel, req *updateChannelRequest) (store.ChannelPatch, *apiError) {
	patch := store.ChannelPatch{}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || utf8.RuneCountInString(name) > 64 {
			v.add("name", "name is required and must be at most 64 characters")
		}
		patch.Name = &name
	}

	ct := contract.ChannelType(existing.Type)
	typeChanged := false
	if req.Type != nil {
		next := contract.ChannelType(*req.Type)
		if !next.Valid() {
			v.add("type", "unknown channel type")
		}
		typeChanged = string(next) != existing.Type
		ct = next
		typeStr := string(next)
		patch.Type = &typeStr
	}

	baseURL := existing.BaseURL
	if req.BaseURL != nil {
		if isJSONNull(req.BaseURL) {
			patch.BaseURLSet = true
			patch.BaseURL = nil
			baseURL = nil
		} else {
			var raw string
			if err := json.Unmarshal(req.BaseURL, &raw); err != nil {
				v.add("base_url", "base_url must be a string or null")
			} else {
				raw = strings.TrimSpace(raw)
				patch.BaseURLSet = true
				if raw == "" {
					patch.BaseURL = nil
					baseURL = nil
				} else if msg := validateBaseURL(raw); msg != "" {
					v.add("base_url", msg)
				} else {
					patch.BaseURL = &raw
					baseURL = &raw
				}
			}
		}
	}
	if requiresBaseURL(ct) && (baseURL == nil || strings.TrimSpace(*baseURL) == "") {
		v.add("base_url", "base_url is required for this channel type")
	}

	models := decodeStringSlice(existing.SupportedModels)
	if req.SupportedModels != nil {
		var modelsJSON []byte
		models, modelsJSON = validateSupportedModels(v, *req.SupportedModels)
		patch.SupportedModels = modelsJSON
	}

	if req.GroupTags != nil {
		groupTags, _ := parseGroupTags(v, req.GroupTags)
		patch.GroupTagsSet = true
		patch.GroupTags = groupTags
	}

	if req.ModelMapping != nil {
		mapping, _ := parseModelMappingPatch(v, req.ModelMapping)
		patch.ModelMapping = mapping
	}

	if req.Weight != nil {
		if *req.Weight < 0 || *req.Weight > 1000 {
			v.add("weight", "weight must be between 0 and 1000")
		}
		patch.Weight = req.Weight
	}
	if req.Priority != nil {
		if *req.Priority < -1000 || *req.Priority > 1000 {
			v.add("priority", "priority must be between -1000 and 1000")
		}
		patch.Priority = req.Priority
	}
	if req.RateLimit != nil {
		rateLimit, _ := parseRateLimit(v, req.RateLimit)
		patch.RateLimitSet = true
		patch.RateLimit = rateLimit
	}
	if req.Status != nil {
		// F-06-08：管理员只可在 enabled/manual_disabled 之间切换；
		// auto_disabled(3) 是系统状态，只能由系统写入（恢复也是系统行为）。
		if *req.Status != 1 && *req.Status != 2 {
			v.add("status", "status must be 1 (enabled) or 2 (manual_disabled)")
		}
		patch.Status = req.Status
		switch *req.Status {
		case 1:
			// 手动启用同时清零失败计数与自动禁用原因（F-06-08/TC-F-06-08-02）。
			zero := int32(0)
			patch.FailCount = &zero
			patch.AutoDisabledReasonSet = true
			patch.AutoDisabledReason = nil
		case 2:
			// R-M4-16：auto_disabled → manual_disabled 的直连 PATCH 必须清空
			// auto_disabled_reason。否则人工禁用后会继续展示「最近一次自动禁用
			// 原因」，与 manual_disabled 的语义冲突。fail_count 保持原值（该列
			// 在 M4 只由启用路径清零，R-M4-04）。
			patch.AutoDisabledReasonSet = true
			patch.AutoDisabledReason = nil
		}
	}
	if req.AutoBan != nil {
		patch.AutoBan = req.AutoBan
	}

	if req.TestModel != nil {
		testModel, validRaw := parseOptionalStringRaw(req.TestModel)
		if !validRaw {
			v.add("test_model", "test_model must be a string or null")
		} else {
			patch.TestModelSet = true
			patch.TestModel = testModel
			if testModel != nil && !slices.Contains(models, *testModel) {
				v.add("test_model", "test_model must be one of supported_models")
			}
		}
	}

	if req.Remark != nil {
		remark, validRaw := parseOptionalStringRaw(req.Remark)
		if !validRaw {
			v.add("remark", "remark must be a string or null")
		} else {
			if remark != nil {
				trimmed := strings.TrimSpace(*remark)
				switch {
				case trimmed == "":
					remark = nil // 空白串 = 清空（与 null 等价）
				case utf8.RuneCountInString(trimmed) > 255:
					v.add("remark", "remark must be at most 255 characters")
				default:
					remark = &trimmed
				}
			}
			patch.RemarkSet = true
			patch.Remark = remark
		}
	}

	if req.Tag != nil {
		tag, validRaw := parseOptionalStringRaw(req.Tag)
		if !validRaw {
			v.add("tag", "tag must be a string or null")
		} else {
			if tag != nil {
				trimmed := strings.TrimSpace(*tag)
				if trimmed == "" {
					tag = nil
				} else if utf8.RuneCountInString(trimmed) > 32 {
					v.add("tag", "tag must be at most 32 characters")
				} else {
					tag = &trimmed
				}
			}
			patch.TagSet = true
			patch.Tag = tag
		}
	}

	// 上游账号归属（上游用量对账设计 §4.1）：`null` 清空，正整数写入；
	// 其它形状（0/负数/非整数）是字段级 422，绝不静默忽略。
	if req.UpstreamAccountID != nil {
		if isJSONNull(req.UpstreamAccountID) {
			patch.UpstreamAccountIDSet = true
			patch.UpstreamAccountID = nil
		} else {
			var accountID int64
			if err := json.Unmarshal(req.UpstreamAccountID, &accountID); err != nil || accountID <= 0 {
				v.add("upstream_account_id", "upstream_account_id must be a positive integer or null")
			} else {
				patch.UpstreamAccountIDSet = true
				patch.UpstreamAccountID = &accountID
			}
		}
	}

	credentialsProvided := false
	if req.Credentials != nil {
		// F-06-03：null / 缺省 / 空 api_key → 保持原值，不重新加密。
		// 只有非空 api_key 才触发校验与封装。
		if !isJSONNull(req.Credentials) {
			creds, hasValue, credErr := decodeCredentialsPayload(req.Credentials)
			if credErr != nil {
				// G-M6D-23：嵌套未知字段 400（fail-closed），不静默忽略。
				// 其它形状错误沿用既有 422 字段错误（凭据输入非法）。
				if _, ok := unknownJSONField(credErr); ok {
					return store.ChannelPatch{}, unknownCredentialsFieldError()
				}
				v.add("credentials", "credentials must be an object or null")
			} else if hasValue && creds.APIKey != "" {
				credentialsProvided = true
				// azure 只改 api_key 时，请求体通常不带 deployment/api_version；
				// 它们仍是有效配置（在旧密文里）。先与已解密的凭据合并，再校验，
				// 否则「只轮换密钥」会被误判成 422（F-06-03）。
				//
				// finding 10：类型变更时**绝不**合并旧密文——旧字段属于旧类型，
				// 例如 openai→azure 时旧 api_key 不是 azure 凭据，合并出的
				// deployment/api_version 会缺失或语义错误。新类型必须显式给全。
				if !typeChanged {
					if existingCreds, ok := h.existingCredentials(existing); ok {
						if creds.Deployment == "" {
							creds.Deployment = existingCreds.Deployment
						}
						if creds.APIVersion == "" {
							creds.APIVersion = existingCreds.APIVersion
						}
					}
				}
				v.addFields(validateCredentials(ct, creds))
				if len(v.fields) == 0 {
					patch.Credentials = h.sealCredentials(v, ct, creds)
				}
			}
		}
	}
	// finding 10：改 type 却沿用旧密文会让新类型带着旧类型的凭据运行，
	// 且绕过新类型的字段校验（例如 azure 的 deployment/api_version）。
	// 要求在同一 PATCH 里重新提交新类型凭据；否则 422 要求重新录入。
	if typeChanged && !credentialsProvided {
		v.add("credentials", "credentials are required when changing channel type")
	}

	// F-06-10 的 PATCH 兜底：校验合并后的**有效状态**，而不是只看本请求。
	// 两种情况都要拒绝：
	//   - 新 model_mapping 的键不在（新的或既有的）supported_models 里；
	//   - 修改 supported_models 时删掉了仍被既有 mapping 当作键的模型。
	//
	// re-review fix 5：只在相关字段**实际变化**时校验。控制台每次编辑都会
	// 重发未改动的 supported_models；若只看「字段出现」，历史遗留的不一致行
	// （mapping 键不在 supported_models）连改备注都会 422，永远无法从控制台
	// 保存。真实变更仍必须保持一致——校验的是「本次修改是否引入/保留不一致」。
	supportedModelsChanged := req.SupportedModels != nil &&
		!slices.Equal(models, decodeStringSlice(existing.SupportedModels))
	mappingChanged := req.ModelMapping != nil &&
		!jsonRawEqual(patch.ModelMapping, existing.ModelMapping)
	if supportedModelsChanged || mappingChanged {
		effectiveMapping := existing.ModelMapping
		if req.ModelMapping != nil {
			effectiveMapping = patch.ModelMapping
		}
		validateModelMappingCoverage(v, effectiveMapping, models)
	}

	// 任务插件绑定（M6d Task 7a）：只在本请求**真正改变绑定**（或改变渠道
	// 类型）时校验并写回两列。
	//
	// 存量兼容（G-M6D-24）：迁移前已存在的 type 61 行可能 task_plugin_key
	// 为空、type 60 行可能多绑为空。若沿用「校验合并后有效状态」的写法，
	// 任何无关 PATCH（如只改 remark）都会被要求补齐绑定，控制台永远无法
	// 保存。绑定插件日后从注册表消失时同理：只有真正换绑的请求才应 400。
	//
	// raw 字段区分「缺省」（保持原值）与 `null`（清空）。控制台全量回传
	// 表单时常见「原值重发」，这不构成修改，不应触发注册表存在性校验。
	effectiveKey := existing.TaskPluginKey
	effectiveExtend := decodeStringSlice(existing.TaskExtendPluginKeys)
	bindingChanged := false
	bindingParseFailed := false
	if req.TaskPluginKey != nil {
		if isJSONNull(req.TaskPluginKey) {
			effectiveKey = nil
		} else {
			var raw string
			if err := json.Unmarshal(req.TaskPluginKey, &raw); err != nil {
				v.add("task_plugin_key", "task_plugin_key must be a string or null")
				bindingParseFailed = true
			} else {
				trimmed := strings.TrimSpace(raw)
				if trimmed == "" {
					effectiveKey = nil
				} else {
					effectiveKey = &trimmed
				}
			}
		}
		bindingChanged = !samePluginKey(existing.TaskPluginKey, effectiveKey)
	}
	if req.TaskExtendPluginKeys != nil {
		if isJSONNull(req.TaskExtendPluginKeys) {
			effectiveExtend = nil
		} else {
			var raw []string
			if err := json.Unmarshal(req.TaskExtendPluginKeys, &raw); err != nil {
				v.add("task_extend_plugin_keys", "task_extend_plugin_keys must be an array of strings or null")
				bindingParseFailed = true
			} else {
				effectiveExtend = raw
			}
		}
		if !samePluginKeyList(decodeStringSlice(existing.TaskExtendPluginKeys), effectiveExtend) {
			bindingChanged = true
		}
	}
	if bindingParseFailed {
		// 形状错误已进入字段错误；不再调用注册表（避免把 400 变成 500）。
		return patch, nil
	}
	// 类型变化必须重写两列：从 61/60 切到普通类型时，旧的绑定值即使
	// 未在请求中出现也要被规范化清空，避免留下跨类型的悬空绑定。
	if !bindingChanged && !typeChanged {
		return patch, nil
	}
	pluginKey, extendKeys, bindErr := h.validatePluginBinding(ctx, ct, effectiveKey, effectiveExtend)
	if bindErr != nil {
		return store.ChannelPatch{}, bindErr
	}
	patch.TaskPluginKeySet = true
	patch.TaskPluginKey = pluginKey
	patch.TaskExtendPluginKeys = extendKeys

	return patch, nil
}

// changedChannelFields 返回 PATCH 载荷中出现过的字段名（审计用）。
//
// 刻意**排除 credentials**：凭据值（及其密文/掩码/长度）永不进入审计，
// 轮换事实由调用侧的 credentials_written 布尔单独表达。字段名本身不是
// 秘密，但把 "credentials" 写进 fields 会让审计消费方误以为能据此检索
// 凭据变更——布尔更准确。
func changedChannelFields(req *updateChannelRequest) []string {
	fields := make([]string, 0, 16)
	add := func(present bool, name string) {
		if present {
			fields = append(fields, name)
		}
	}
	add(req.Name != nil, "name")
	add(req.Type != nil, "type")
	add(req.BaseURL != nil, "base_url")
	add(req.SupportedModels != nil, "supported_models")
	add(req.GroupTags != nil, "group_tags")
	add(req.ModelMapping != nil, "model_mapping")
	add(req.Weight != nil, "weight")
	add(req.Priority != nil, "priority")
	add(req.RateLimit != nil, "rate_limit")
	add(req.Status != nil, "status")
	add(req.AutoBan != nil, "auto_ban")
	add(req.TestModel != nil, "test_model")
	add(req.Remark != nil, "remark")
	add(req.Tag != nil, "tag")
	add(req.TaskPluginKey != nil, "task_plugin_key")
	add(req.TaskExtendPluginKeys != nil, "task_extend_plugin_keys")
	add(req.UpstreamAccountID != nil, "upstream_account_id")
	return fields
}

// jsonRawEqual 语义比较两份 JSON 文档（键顺序与空白不敏感），用于判断
// PATCH 是否真的修改了 model_mapping。nil 与 `{}` 都解码为 nil
// map[string]any，视为相同——store 会把缺失的映射规范化为 `{}`。
// 任一侧无法解码时返回 false（按「有变化」处理，宁严勿宽）。
func jsonRawEqual(a, b []byte) bool {
	var left, right any
	if err := json.Unmarshal(a, &left); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &right); err != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

// validateModelMappingCoverage 实现 modules-06 F-06-10：model_mapping 的每个键
// （客户端可见别名）必须出现在 supported_models 中。两层原因：
//   - /v1/models 会宣告映射键（enabledModelsQuery）；
//   - Candidates 以别名路由到该渠道（jsonb_exists(model_mapping, $1)）。
//
// 若键不在 supported_models，创建/更新会制造一个「列表里有、请求 503」的模型。
//
// 错误按字典序报告，保证同一请求的错误顺序稳定（map 迭代顺序随机）。
func validateModelMappingCoverage(v *validator, mapping []byte, models []string) {
	if len(mapping) == 0 {
		return
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(mapping, &keys); err != nil {
		// parseModelMappingCreate/Patch 已校验对象形状；这里不重复报错。
		return
	}
	supported := make(map[string]struct{}, len(models))
	for _, m := range models {
		supported[m] = struct{}{}
	}
	sorted := make([]string, 0, len(keys))
	for key := range keys {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)
	for _, key := range sorted {
		if _, ok := supported[key]; !ok {
			v.add("model_mapping", fmt.Sprintf("model_mapping key %q must be one of supported_models", key))
		}
	}
}

// taskPluginKeyRe 与 task_plugins.key / R-M6D-01 的 DB CHECK 同源
// （^[a-z0-9][a-z0-9-]{0,29}$）：key 会进入 URL 段、路由索引与前端选择器。
var taskPluginKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,29}$`)

// normalizePluginKey 把「未绑定」统一成空串，并去掉历史行可能残留的空白，
// 用于判断 PATCH 是否真的改了绑定（G-M6D-24）。
func normalizePluginKey(key *string) string {
	if key == nil {
		return ""
	}
	return strings.TrimSpace(*key)
}

func samePluginKey(a, b *string) bool {
	return normalizePluginKey(a) == normalizePluginKey(b)
}

// normalizePluginKeyList 与 validatePluginBinding 的规范化口径一致：
// 去空白、丢空项、按首次出现去重。用于「原值重发」不触发校验/写回。
func normalizePluginKeyList(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, raw := range items {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		if _, dup := seen[item]; dup {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func samePluginKeyList(a, b []string) bool {
	return slices.Equal(normalizePluginKeyList(a), normalizePluginKeyList(b))
}

// validatePluginBinding 校验并规范化渠道的任务插件绑定（relay-06 §1.3）。
//
// 形态矩阵：
//   - 类型 61（task_plugin）：task_plugin_key 必填；extend 必须为空；
//   - 类型 60（newapi）：extend 非空、元素为已注册 key、去重；key 必须为空；
//   - 其他类型：两者都必须为空/缺省。
//
// 返回值：规范化的单绑 key（未绑定为 nil）与多绑 JSON（始终是数组文本）。
// 字段错误统一 400（绑定的「存在性」属于请求语义校验，不是 DB 约束失败）；
// 注册表查询失败返回 500，绝不把基础设施故障伪装成 422/400。
func (h *ChannelHandler) validatePluginBinding(ctx context.Context, ct contract.ChannelType,
	key *string, extend []string) (*string, []byte, *apiError) {
	fields := make([]fieldError, 0, 2)
	add := func(field, message string) { fields = append(fields, fieldError{Field: field, Message: message}) }

	normalizedKey := ""
	if key != nil {
		normalizedKey = strings.TrimSpace(*key)
	}

	single := ct == contract.ChannelTypeTaskPlugin
	multi := ct == contract.ChannelTypeNewAPI

	switch {
	case !single && !multi:
		if normalizedKey != "" {
			add("task_plugin_key", "task_plugin_key is only allowed for task_plugin channels")
		}
		if len(extend) > 0 {
			add("task_extend_plugin_keys", "task_extend_plugin_keys is only allowed for newapi channels")
		}
	case single:
		if normalizedKey == "" {
			add("task_plugin_key", "task_plugin_key is required for task_plugin channels")
		}
		if len(extend) > 0 {
			add("task_extend_plugin_keys", "task_extend_plugin_keys must be empty for task_plugin channels")
		}
	case multi:
		if normalizedKey != "" {
			add("task_plugin_key", "task_plugin_key must be empty for newapi channels")
		}
		if len(extend) == 0 {
			add("task_extend_plugin_keys", "task_extend_plugin_keys must contain at least one plugin key for newapi channels")
		}
	}

	if len(fields) > 0 {
		return nil, nil, badBindingError(fields)
	}

	switch {
	case single:
		if !taskPluginKeyRe.MatchString(normalizedKey) {
			return nil, nil, badBindingError([]fieldError{{Field: "task_plugin_key",
				Message: "task_plugin_key must match ^[a-z0-9][a-z0-9-]{0,29}$"}})
		}
		registered, apiErr := h.pluginRegistered(ctx, normalizedKey)
		if apiErr != nil {
			return nil, nil, apiErr
		}
		if !registered {
			return nil, nil, badBindingError([]fieldError{{Field: "task_plugin_key",
				Message: fmt.Sprintf("task_plugin_key %q is not a registered task plugin", normalizedKey)}})
		}
		bound := normalizedKey
		return &bound, []byte(`[]`), nil
	case multi:
		seen := make(map[string]struct{}, len(extend))
		deduped := make([]string, 0, len(extend))
		for i, raw := range extend {
			item := strings.TrimSpace(raw)
			if !taskPluginKeyRe.MatchString(item) {
				add("task_extend_plugin_keys", fmt.Sprintf(
					"task_extend_plugin_keys[%d] must match ^[a-z0-9][a-z0-9-]{0,29}$", i))
				continue
			}
			if _, dup := seen[item]; dup {
				// 重复元素按「去重」处理（brief 冻结语义）：同 key 只保留
				// 首次出现的位置，不制造第二条执行候选。
				continue
			}
			seen[item] = struct{}{}
			deduped = append(deduped, item)
		}
		if len(fields) == 0 {
			for _, item := range deduped {
				registered, apiErr := h.pluginRegistered(ctx, item)
				if apiErr != nil {
					return nil, nil, apiErr
				}
				if !registered {
					add("task_extend_plugin_keys", fmt.Sprintf(
						"task_extend_plugin_keys entry %q is not a registered task plugin", item))
				}
			}
		}
		if len(fields) > 0 {
			return nil, nil, badBindingError(fields)
		}
		encoded, err := json.Marshal(deduped)
		if err != nil {
			return nil, nil, internalAPIError()
		}
		return nil, encoded, nil
	default:
		// 其他类型：两列都必须为空。key 用 NULL、extend 用空数组规范化。
		return nil, []byte(`[]`), nil
	}
}

// pluginRegistered 查询注册表；nil 依赖按 fail-closed 处理（未装配即未注册）。
func (h *ChannelHandler) pluginRegistered(ctx context.Context, key string) (bool, *apiError) {
	if h.plugins == nil {
		return false, nil
	}
	ok, err := h.plugins.Exists(ctx, key)
	if err != nil {
		// GC-6：不把内部错误文本带给客户端。
		h.log.Error("check task plugin registry", "key", key, "error", err)
		return false, internalAPIError()
	}
	return ok, nil
}

// badBindingError 把绑定字段错误封装为 400（brief 冻结「跨类型误填 400」）。
func badBindingError(fields []fieldError) *apiError {
	return &apiError{
		Status: http.StatusBadRequest,
		Type:   problemTypeValidationFailed,
		Title:  "Validation failed",
		Fields: fields,
	}
}

// existingCredentials 解出渠道当前的凭据明文，用于 PATCH 的字段级合并。
// 解封失败返回 ok=false：调用方按「没有可合并的旧值」处理，校验仍然会
// 要求 azure 的 deployment/api_version 齐全（fail-closed）。
func (h *ChannelHandler) existingCredentials(ch *store.Channel) (ChannelCredentials, bool) {
	if ch == nil || len(ch.Credentials) == 0 {
		return ChannelCredentials{}, false
	}
	plaintext, err := h.sealer.Open(ch.Credentials)
	if err != nil {
		return ChannelCredentials{}, false
	}
	var creds ChannelCredentials
	if err := json.Unmarshal(plaintext, &creds); err != nil {
		return ChannelCredentials{}, false
	}
	return creds, true
}

// ---------- 删除 / 批量 ----------

func (h *ChannelHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r, PermChannelsManageSensitive) {
		return
	}
	id, ok := channelIDFromRequest(r)
	if !ok {
		writeAPIError(w, channelNotFoundError())
		return
	}
	if err := h.channels.Delete(r.Context(), id); err != nil {
		writeAPIError(w, storeErrorResponse(err))
		return
	}
	// O-1：删除必须可追溯；channel_id 是唯一必要的标识（行已不存在，不能再查）。
	h.writeChannelAuditEvent(r, audit.ActionChannelDelete, http.StatusNoContent, "success", map[string]any{
		"channel_id": id,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *ChannelHandler) handleBatchStatus(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r, PermChannelsManage) {
		return
	}
	var req batchStatusRequest
	if err := decodeChannelJSON(r, &req); err != nil {
		writeAPIError(w, invalidRequestError("request body is not valid JSON"))
		return
	}
	var target int16
	switch req.Action {
	case "enable":
		target = 1
	case "disable":
		target = 2 // manual_disabled；auto_disabled(3) 是系统状态（R-P5/modules-06 §1.4）
	default:
		writeAPIError(w, validationError("action", `action must be "enable" or "disable"`))
		return
	}
	if e := validateBatchIDs(req.IDs); e != nil {
		writeAPIError(w, e)
		return
	}
	res, err := h.channels.BatchStatus(r.Context(), req.IDs, target)
	if err != nil {
		writeAPIError(w, internalAPIError())
		return
	}
	writeChannelJSON(w, http.StatusOK, res)
}

func (h *ChannelHandler) handleBatchDelete(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r, PermChannelsManageSensitive) {
		return
	}
	h.serveIdempotent(w, r, func() (int, []byte, bool) {
		var req batchDeleteRequest
		if err := decodeChannelJSON(r, &req); err != nil {
			return errorResponse(invalidRequestError("request body is not valid JSON"))
		}
		if e := validateBatchIDs(req.IDs); e != nil {
			return e.Status, e.marshal(), false
		}
		res, err := h.channels.BatchDelete(r.Context(), req.IDs)
		if err != nil {
			return errorResponse(internalAPIError())
		}
		body, err := json.Marshal(res)
		if err != nil {
			return errorResponse(internalAPIError())
		}
		return http.StatusOK, body, true
	})
}

// ---------- 上游模型目录 ----------

// fetchChannelModelsRequest 是「从上游获取模型」的请求。
//
// 创建模式传 type/base_url/credentials；编辑模式可以只传 channel_id，
// 由服务端从已保存渠道解出 base_url 与机密凭据（前端详情接口不回显它们）。
type fetchChannelModelsRequest struct {
	ChannelID   int64   `json:"channel_id"`
	Type        string  `json:"type"`
	BaseURL     *string `json:"base_url"`
	Credentials struct {
		APIKey     string `json:"api_key"`
		Deployment string `json:"deployment"`
		APIVersion string `json:"api_version"`
	} `json:"credentials"`
}

// handleFetchModels 用表单当前值调用上游模型目录。只读探针：不落库、
// 不改渠道；失败不触发自动禁用。
func (h *ChannelHandler) handleFetchModels(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r, PermChannelsManageSensitive) {
		return
	}
	// 与渠道测试同族的独立桶（10/min/管理员，Redis 故障 fail-open）。
	if h.rdb != nil {
		over, retryAfter, err := allowRequestRate(r.Context(), h.rdb,
			ChannelModelFetchRateLimitKey(actorID(r)), ChannelModelFetchRateLimit, ChannelModelFetchRateWindow)
		if err != nil {
			h.log.Warn("channel model fetch rate limit check failed; allowing request (fail-open)", "error", err)
		} else if over {
			writeRateLimited(w, retryAfter, "Too many model fetch requests")
			return
		}
	} else {
		h.log.Warn("channel model fetch rate limit store is not configured; allowing request (fail-open)")
	}

	var req fetchChannelModelsRequest
	if err := decodeChannelJSON(r, &req); err != nil {
		writeAPIError(w, invalidRequestError("request body is not valid JSON"))
		return
	}

	var ch *store.Channel
	if req.ChannelID > 0 {
		loaded, err := h.channels.Get(r.Context(), req.ChannelID)
		if err != nil {
			writeAPIError(w, storeErrorResponse(err))
			return
		}
		ch = loaded
	}

	rawType := strings.TrimSpace(req.Type)
	if rawType == "" && ch != nil {
		rawType = ch.Type
	}
	channelType := contract.ChannelType(rawType)
	if !channelType.Valid() {
		writeAPIError(w, validationError("type", "type is required"))
		return
	}

	baseURL := ""
	if req.BaseURL != nil {
		baseURL = strings.TrimSpace(*req.BaseURL)
	}
	if baseURL == "" && ch != nil && ch.BaseURL != nil {
		baseURL = strings.TrimSpace(*ch.BaseURL)
	}
	if baseURL != "" {
		if message := validateBaseURL(baseURL); message != "" {
			writeAPIError(w, validationError("base_url", message))
			return
		}
	}

	apiKey := strings.TrimSpace(req.Credentials.APIKey)
	apiVersion := strings.TrimSpace(req.Credentials.APIVersion)
	// 只有表单 base_url 与渠道库里的地址一致时，才复用库里解密出的密钥。
	// 否则任何持 channels:sensitive 的管理员都能把已存密钥发到任意 host
	// （凭据外带 + SSRF）；换地址时必须显式重新提供 api_key。
	reuseStored := ch != nil &&
		normalizeChannelBaseURL(channelBaseURL(ch)) == normalizeChannelBaseURL(baseURL)
	if (apiKey == "" || apiVersion == "") && reuseStored {
		if creds, ok := h.existingCredentials(ch); ok {
			if apiKey == "" {
				apiKey = strings.TrimSpace(creds.APIKey)
			}
			if apiVersion == "" {
				apiVersion = strings.TrimSpace(creds.APIVersion)
			}
		}
	}
	if apiKey == "" {
		message := "api_key is required"
		if ch != nil && !reuseStored {
			message = "api_key is required when base_url differs from the channel's stored base_url"
		}
		writeAPIError(w, validationError("credentials.api_key", message))
		return
	}

	infos, err := h.modelLister.ListDetailed(r.Context(), channelhealth.ModelListRequest{
		ChannelType: channelType,
		BaseURL:     baseURL,
		APIKey:      apiKey,
		APIVersion:  apiVersion,
	})
	if err != nil {
		if errors.Is(err, channelhealth.ErrModelListUnsupported) ||
			errors.Is(err, channelhealth.ErrModelListConfig) {
			writeAPIError(w, &apiError{
				Status: http.StatusBadRequest,
				Type:   problemTypeValidationFailed,
				Title:  "Validation failed",
				Detail: err.Error(),
			})
			return
		}
		writeProblemDetail(w, http.StatusBadGateway,
			"https://newtg.dev/problems/upstream-models",
			"Failed to fetch models", err.Error())
		return
	}

	// models 保持旧形状（[]string），entries 是新增的逐模型事实；旧客户端
	// 只读 models，完全不受影响。
	models := make([]string, 0, len(infos))
	for _, info := range infos {
		models = append(models, info.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models":  models,
		"entries": buildChannelModelEntries(infos, channelType),
	})
}

// channelModelEntry 是探针响应里的一条上游模型事实。字段语义与
// channelhealth.ModelInfo 一一对应；空值表示上游没给，绝不猜测。
type channelModelEntry struct {
	ID               string   `json:"id"`
	DisplayName      string   `json:"display_name"`
	Description      string   `json:"description"`
	ContextLength    *int64   `json:"context_length"`
	MaxOutput        *int64   `json:"max_output"`
	ReleasedAt       string   `json:"released_at"`
	Endpoints        []string `json:"endpoints"`
	VendorSuggestion string   `json:"vendor_suggestion"`
	FactsSource      string   `json:"facts_source"`
}

// buildChannelModelEntries 把上游事实投影成响应条目，顺序与 models 一致。
// display_name / description 按 model_meta 的长度上限截断；endpoints 为空时
// 用非 nil 空切片，保证 JSON 里是 [] 而不是 null。返回切片恒非 nil。
func buildChannelModelEntries(infos []channelhealth.ModelInfo, t contract.ChannelType) []channelModelEntry {
	entries := make([]channelModelEntry, 0, len(infos))
	vendor := channelhealth.SuggestVendor(t)
	for _, info := range infos {
		endpoints := info.Endpoints
		if len(endpoints) == 0 {
			endpoints = []string{}
		}
		entries = append(entries, channelModelEntry{
			ID:               info.ID,
			DisplayName:      truncateRunes(info.DisplayName, channelModelDisplayNameMaxRunes),
			Description:      truncateRunes(info.Description, channelModelDescriptionMaxRunes),
			ContextLength:    info.ContextLength,
			MaxOutput:        info.MaxOutput,
			ReleasedAt:       info.ReleasedAt,
			Endpoints:        endpoints,
			VendorSuggestion: vendor,
			FactsSource:      info.Source,
		})
	}
	return entries
}

// 与 model_meta 的应用层长度上限保持一致，避免前端拿到上游的长文本写进
// 档案时被 store 校验拒绝。description 的口径来源是
// store.maxModelMetaDescriptionRunes（backend/internal/store/model_meta_validate.go
// 的 512 字符；该常量未导出，跨包只能在此保留字面量并靠测试对齐）；
// display_name 另与 model_meta 的 varchar(64) 列宽一致。
const (
	channelModelDisplayNameMaxRunes = 64
	channelModelDescriptionMaxRunes = 512
)

// ---------- 连通性测试 ----------

func (h *ChannelHandler) handleTest(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r, PermChannelsManage) {
		return
	}
	id, ok := channelIDFromRequest(r)
	if !ok {
		writeAPIError(w, channelNotFoundError())
		return
	}
	// W16-1 / R-W16-02：测试接口独立桶 10 次/分钟/管理员（modules-08 F-08-16
	// 冻结值）。批量测试由前端逐模型调用，因此按请求计数；桶在请求体解析前
	// 判定，超限不消耗任何上游/解析成本。
	//
	// Redis 不可用时 **fail-open**（只记 WARN）：测试是只读探针，不是敏感数据
	// 出口，与 reveal 的 fail-closed 刻意不同；不能让 Redis 抖动把管理员的
	// 连通性测试整体打成 503。
	if h.rdb != nil {
		over, retryAfter, err := allowRequestRate(r.Context(), h.rdb,
			ChannelTestRateLimitKey(actorID(r)), ChannelTestRateLimit, ChannelTestRateWindow)
		if err != nil {
			h.log.Warn("channel test rate limit check failed; allowing request (fail-open)", "error", err)
		} else if over {
			writeRateLimited(w, retryAfter, "Too many channel test requests")
			return
		}
	} else {
		h.log.Warn("channel test rate limit store is not configured; allowing request (fail-open)")
	}
	var req channelTestRequest
	if err := decodeChannelJSON(r, &req); err != nil {
		writeAPIError(w, invalidRequestError("request body is not valid JSON"))
		return
	}
	ch, err := h.channels.Get(r.Context(), id)
	if err != nil {
		writeAPIError(w, storeErrorResponse(err))
		return
	}
	models := decodeStringSlice(ch.SupportedModels)
	model := strings.TrimSpace(req.Model)
	if model == "" {
		if ch.TestModel != nil {
			model = *ch.TestModel
		} else if len(models) > 0 {
			model = models[0]
		}
	}
	if model == "" {
		writeAPIError(w, validationError("model", "model is required"))
		return
	}
	if !slices.Contains(models, model) {
		writeAPIError(w, validationError("model", "model must be one of supported_models"))
		return
	}
	if h.tester == nil {
		// phase 8A 不装配真实 tester；wiring 应注入 8B 的适配器实现。
		writeAPIError(w, &apiError{
			Status: http.StatusServiceUnavailable,
			Type:   "https://newtg.dev/problems/channel_test_unavailable",
			Title:  "Channel test unavailable",
		})
		return
	}

	latency, testErr := h.tester(r.Context(), ch, model)
	now := time.Now().UTC()
	result, errorClass := probeOutcome(latency, testErr)
	// 复核 M-6：手工测试与体检共用 ProbeSucceeded（含 headers-only 阈值）。
	// 「2xx 但慢于阈值」按失败记：last_test_result=false、响应 success=false
	// 且 error_class=timeout——否则会出现「测试成功随后被自动禁用」的矛盾。
	if !channelhealth.ProbeSucceeded(result, channelhealth.LoadWorkerSettings(h.config)) {
		// 仍回写失败结果（F-06-06 主流程 3）；回写失败不覆盖测试错误。
		_ = h.channels.UpdateTestResult(r.Context(), id, now, false)
		// W7/M4-b / R-M4-15：手工测试失败可以触发自动禁用（与中继/体检共用
		// 判定），但绝不触发自动恢复。非阻塞入队，响应不等副作用。
		h.reportProbeResult(r.Context(), ch, result)
		if errorClass == "" {
			// 唯一没有 tester 错误的失败来源就是阈值超限（R-M4-02/14）。
			errorClass = channelTestClassTimeout
		}
		// modules-06 F-06-06：连通性失败是**业务结果**而不是 HTTP 故障，
		// 返回 200 {success:false, error_class}，便于前端展示失败原因。
		// GC-6：只给泛化枚举 + 固定字段，原始错误（可能含 URL/key）整体丢弃。
		writeChannelJSON(w, http.StatusOK, channelTestResponse{
			Success:    false,
			LatencyMS:  latency,
			Model:      model,
			ErrorClass: errorClass,
		})
		return
	}
	if err := h.channels.UpdateTestResult(r.Context(), id, now, true); err != nil {
		writeAPIError(w, storeErrorResponse(err))
		return
	}
	// 成功也上报：判定层不会因成功禁用；保留接缝以便未来扩展（如恢复观察）。
	h.reportProbeResult(r.Context(), ch, result)
	writeChannelJSON(w, http.StatusOK, channelTestResponse{Success: true, LatencyMS: latency, Model: model})
}

// probeOutcome 把 tester 的 (latency, error) 投影为共享的 ProbeResult。
//
// 错误分类来自 tester 的 typed error：上游非 2xx 携带 status/class/body，
// 传输失败携带 timeout/network；其它错误（nil 渠道/未配置 sealer）保持零值，
// 判定层不会据此禁用。errorClass 是 F-06-06 的五值枚举（空串表示待判定）。
func probeOutcome(latencyMS int, testErr error) (channelhealth.ProbeResult, string) {
	result := channelhealth.ProbeResult{LatencyMS: latencyMS}
	if testErr == nil {
		result.Success = true
		return result, ""
	}
	var upstream *channelTestUpstreamError
	if errors.As(testErr, &upstream) {
		result.StatusCode = upstream.StatusCode
		result.ErrorClass = upstream.Class
		result.Body = upstream.Body
		return result, channelTestErrorClass(testErr)
	}
	var transport *channelTestTransportError
	if errors.As(testErr, &transport) {
		result.TransportClass = transport.Class
	}
	return result, channelTestErrorClass(testErr)
}

// reportProbeResult 把已经构建好的探测结果交给自动禁用接缝（非阻塞）。
func (h *ChannelHandler) reportProbeResult(ctx context.Context, ch *store.Channel, result channelhealth.ProbeResult) {
	if h.probeReporter == nil || ch == nil {
		return
	}
	h.probeReporter.ReportProbeFailure(ctx, ch.ID, ch.AutoBan, result)
}

// ---------- 凭据揭示（受控例外） ----------

// handleRevealCredentials 实现 POST /api/channels/{id}/credentials/reveal
// （security-03 §2.10；modules-06 F-06-03）。
//
// 顺序是安全语义的一部分：
//  1. channels:manage_sensitive —— 权限不足在步进认证之前判定，且不解析
//     请求体（错误体不可能含调用方输入）；
//  2. id 形态 + 管理员维限流（reveal 桶，Redis 故障 fail-closed）；
//  3. 请求体形状与 reason（1–200 字符）；
//  4. 渠道存在性（404；设计允许泄露「是否存在」，但不泄露任何凭据信息）；
//  5. 步进认证（恰好一个因子且匹配账号 MFA 状态；**只授权本次请求**）；
//  6. 审计成功事件（**fail-closed**：审计写不进去就不返回明文）；
//  7. 解密并一次性返回，响应 no-store + Pragma no-cache，明文不写日志。
//
// 失败尝试一律 best-effort 审计（失败响应本就不含明文）；只有成功响应把
// 审计当作硬前置：没有审计记录的揭示不能被放行（I-1 复核）。detail 只含
// 元数据：channel_id / reason / revealed_fields / failure，**永不含明文、
// 片段、掩码、长度或哈希**（审计保留 12 个月，不能成为密钥副本）。
func (h *ChannelHandler) handleRevealCredentials(w http.ResponseWriter, r *http.Request) {
	// 1) 权限门先于一切。这里不复用 authorize：需要在 403 时补一条 best-effort
	//    拒绝审计（M-1 复核），而 authorize 只写响应不返回原因。401（无身份/
	//    身份失效）没有可归责的真实管理员，不写审计；存储故障 500。
	switch err := CheckPermission(r.Context(), h.access, PermChannelsManageSensitive); {
	case err == nil:
		// 通过。
	case errors.Is(err, ErrNoPrincipal), errors.Is(err, ErrUserDeleted):
		writeUnauthorized(w)
		return
	case errors.Is(err, ErrAccessStore):
		// 不把 DB 故障伪装成 403（与 authorize 同口径）。
		writeProblem(w, http.StatusInternalServerError, problemTypeInternal, "Internal Server Error")
		return
	default:
		_ = h.auditReveal(r, http.StatusForbidden, "failed", map[string]any{"failure": "forbidden"})
		writeForbidden(w)
		return
	}
	actor := actorID(r)

	// 2) id 形态：非法与不存在同形（404），避免成为路径枚举信道。
	id, ok := channelIDFromRequest(r)
	if !ok {
		h.auditReveal(r, http.StatusNotFound, "failed", map[string]any{"failure": "invalid_channel_id"})
		writeAPIError(w, channelNotFoundError())
		return
	}

	// 3) 限流必须在 KDF/解密之前：超限请求不消耗昂贵资源。Redis 不可用时
	//    fail-closed（503）：限流是敏感操作的强制控制，不能静默降级为无限次。
	if h.rdb == nil {
		h.log.Error("channel credential reveal has no rate limit store configured")
		h.auditReveal(r, http.StatusServiceUnavailable, "failed",
			map[string]any{"channel_id": id, "failure": "rate_limit_unavailable"})
		h.writeRevealUnavailable(w)
		return
	}
	over, retryAfter, err := allowRequestRate(r.Context(), h.rdb,
		ChannelRevealRateLimitKey(actor), ChannelRevealRateLimit, ChannelRevealRateWindow)
	if err != nil {
		h.log.Error("channel credential reveal rate limit check failed", "error", err)
		h.auditReveal(r, http.StatusServiceUnavailable, "failed",
			map[string]any{"channel_id": id, "failure": "rate_limit_unavailable"})
		h.writeRevealUnavailable(w)
		return
	}
	if over {
		h.auditReveal(r, http.StatusTooManyRequests, "failed",
			map[string]any{"channel_id": id, "failure": "rate_limited"})
		writeRateLimited(w, retryAfter, "Too many credential reveal attempts")
		return
	}

	// 4) 请求体：字段白名单 + 尾随数据拒绝（decodeChannelJSON）。
	var req revealCredentialsRequest
	if err := decodeChannelJSON(r, &req); err != nil {
		if field, unknown := unknownJSONField(err); unknown {
			h.auditReveal(r, http.StatusForbidden, "failed",
				map[string]any{"channel_id": id, "failure": "field_not_writable"})
			writeFieldNotWritableProblem(w, field)
			return
		}
		h.auditReveal(r, http.StatusBadRequest, "failed",
			map[string]any{"channel_id": id, "failure": "invalid_body"})
		writeAPIError(w, invalidRequestError("request body is not valid JSON"))
		return
	}

	// reason 是运营说明，不是凭据；仍按普通文本转义入库（audit writer 负责）。
	reason := strings.TrimSpace(req.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > 200 {
		h.auditReveal(r, http.StatusUnprocessableEntity, "failed",
			map[string]any{"channel_id": id, "failure": "reason_invalid"})
		writeAPIError(w, validationError("reason",
			"reason is required and must be 1-200 characters"))
		return
	}

	password := ""
	if req.Password != nil {
		// 口令不 trim：与登录/Authorize 的原样比较一致，否则首尾带空格
		// 的合法口令能登录却永远无法步进认证（M-7 复核）。
		password = *req.Password
	}
	totpCode := ""
	if req.TOTPCode != nil {
		totpCode = strings.TrimSpace(*req.TOTPCode)
	}
	// 设计冻结「恰好一个」因子：同时提供或都缺失都是 422 字段级错误，
	// **不执行解密**。形态是否匹配账号 MFA 状态由步进门判定（422）。
	if (password == "") == (totpCode == "") {
		h.auditReveal(r, http.StatusUnprocessableEntity, "failed",
			map[string]any{"channel_id": id, "reason": reason, "failure": "factor_shape"})
		writeAPIError(w, &apiError{
			Status: http.StatusUnprocessableEntity,
			Type:   problemTypeValidationFailed,
			Title:  "Validation failed",
			Fields: []fieldError{
				{Field: "password", Message: "exactly one of password or totp_code is required"},
				{Field: "totp_code", Message: "exactly one of password or totp_code is required"},
			},
		})
		return
	}

	// 5) 渠道存在性：先读渠道再步进认证，不存在时 404 且不消耗 KDF。
	ch, err := h.channels.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrChannelNotFound) {
			h.auditReveal(r, http.StatusNotFound, "failed",
				map[string]any{"channel_id": id, "reason": reason, "failure": "channel_not_found"})
			writeAPIError(w, channelNotFoundError())
			return
		}
		h.log.Error("load channel for credential reveal failed", "channel_id", id, "error", err)
		h.auditReveal(r, http.StatusInternalServerError, "failed",
			map[string]any{"channel_id": id, "reason": reason, "failure": "channel_load_failed"})
		writeAPIError(w, internalAPIError())
		return
	}

	// 6) 步进认证：独立于 RBAC 的第二道门，缺失时 fail-closed。
	if h.reauth == nil {
		h.log.Error("channel credential reveal has no reauth gate configured")
		h.auditReveal(r, http.StatusServiceUnavailable, "failed",
			map[string]any{"channel_id": id, "reason": reason, "failure": "step_up_unavailable"})
		h.writeRevealUnavailable(w)
		return
	}
	err = h.reauth.AuthorizeOnce(r.Context(), actor, auth.ReauthFactor{
		Password: password,
		TOTPCode: totpCode,
	})
	switch {
	case err == nil:
		// 通过：继续解密。注意 AuthorizeOnce 不写再认证窗口。
	case errors.Is(err, auth.ErrReauthFactorShape):
		var shape *auth.ReauthFactorShapeError
		field := "password"
		if errors.As(err, &shape) && shape.Required != "" {
			field = shape.Required
		}
		h.auditReveal(r, http.StatusUnprocessableEntity, "failed", map[string]any{
			"channel_id": id, "reason": reason, "failure": "factor_shape",
		})
		writeAPIError(w, &apiError{
			Status: http.StatusUnprocessableEntity,
			Type:   problemTypeValidationFailed,
			Title:  "Validation failed",
			Fields: []fieldError{{Field: field,
				Message: "the submitted factor does not match the account MFA state"}},
		})
		return
	case errors.Is(err, auth.ErrReauthLocked):
		h.auditReveal(r, http.StatusTooManyRequests, "failed",
			map[string]any{"channel_id": id, "reason": reason, "failure": "step_up_locked"})
		writeReauthFailure(w, err)
		return
	case errors.Is(err, auth.ErrReauthRequired):
		h.auditReveal(r, http.StatusForbidden, "failed",
			map[string]any{"channel_id": id, "reason": reason, "failure": "step_up_failed"})
		writeReauthFailure(w, err)
		return
	default:
		h.log.Error("channel credential reveal step-up failed", "channel_id", id, "error", err)
		h.auditReveal(r, http.StatusServiceUnavailable, "failed",
			map[string]any{"channel_id": id, "reason": reason, "failure": "step_up_unavailable"})
		writeReauthFailure(w, err)
		return
	}

	// 7) 解密。空密文/损坏密文/非法 JSON 都是服务端数据完整性问题：
	//    统一 500，绝不回显密文、错误详情或任何凭据形态。
	if len(ch.Credentials) == 0 {
		h.auditReveal(r, http.StatusInternalServerError, "failed",
			map[string]any{"channel_id": id, "reason": reason, "failure": "credentials_missing"})
		writeAPIError(w, internalAPIError())
		return
	}
	plaintext, err := h.sealer.Open(ch.Credentials)
	if err != nil {
		h.auditReveal(r, http.StatusInternalServerError, "failed",
			map[string]any{"channel_id": id, "reason": reason, "failure": "decrypt_failed"})
		writeAPIError(w, internalAPIError())
		return
	}
	var creds ChannelCredentials
	if err := json.Unmarshal(plaintext, &creds); err != nil {
		h.auditReveal(r, http.StatusInternalServerError, "failed",
			map[string]any{"channel_id": id, "reason": reason, "failure": "decode_failed"})
		writeAPIError(w, internalAPIError())
		return
	}

	// 6) 成功审计是硬前置（I-1 复核）：审计写不进去就不返回明文。失败响应
	//    本就不含明文，因此只有这条路径 fail-closed。未装配审计器同样拒绝。
	if h.audit == nil {
		h.log.Error("channel credential reveal has no audit logger configured")
		h.writeRevealNoStoreUnavailable(w)
		return
	}
	// 审计只记字段名（api_key/deployment/api_version），不记值。
	if err := h.auditReveal(r, http.StatusOK, "success", map[string]any{
		"channel_id":      id,
		"reason":          reason,
		"revealed_fields": revealedCredentialFields(creds),
	}); err != nil {
		h.log.Error("channel credential reveal audit write failed; refusing to return plaintext",
			"channel_id", id, "error", err)
		h.writeRevealNoStoreUnavailable(w)
		return
	}

	// 7) 唯一的明文出口：一次响应 + no-store。响应体就是凭据对象本身
	// （与详情接口的 credentials_masked 同一字段形状），不夹带渠道其他配置。
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeChannelJSON(w, http.StatusOK, creds)
}

// auditReveal 写凭据揭示审计（best-effort 调用方可忽略返回）。detail 由
// 调用方构造；本函数不接触明文，调用方也绝不能把凭据值放进 detail。
//
// 成功路径必须检查返回值：审计写失败时不得返回明文（I-1）。
func (h *ChannelHandler) auditReveal(r *http.Request, status int, result string, detail map[string]any) error {
	return h.writeChannelAuditEvent(r, audit.ActionChannelCredentialsReveal, status, result, detail)
}

// writeChannelAuditEvent 是渠道管理面审计的统一出口，返回写入错误供调用方
// 决定 fail 策略（揭示成功路径 fail-closed；其余 best-effort）。
//
// best-effort 语义来自 audit_util：日志库独立于业务库，写入失败不回滚业务，
// 但有界超时 + 记录告警，避免静默丢失（GC-6：错误文本不带请求内容）。
func (h *ChannelHandler) writeChannelAuditEvent(r *http.Request, action string, status int, result string, detail map[string]any) error {
	if h.audit == nil {
		return nil
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		h.log.Warn("marshal channel audit detail failed", "action", action, "error", err)
		return err
	}
	event := audit.Event{
		Action:     action,
		Actor:      actorID(r),
		Result:     result,
		IP:         clientIP(r),
		UserAgent:  r.UserAgent(),
		Method:     r.Method,
		Route:      r.Pattern,
		StatusCode: status,
		Detail:     raw,
	}
	if err := writeAuditEvent(r.Context(), h.audit, event); err != nil {
		h.log.Warn("channel audit write failed", "action", action, "error", err)
		return err
	}
	return nil
}

// revealedCredentialFields 返回本次揭示涉及的字段名（只用于审计）。
func revealedCredentialFields(creds ChannelCredentials) []string {
	fields := []string{"api_key"}
	if creds.Deployment != "" {
		fields = append(fields, "deployment")
	}
	if creds.APIVersion != "" {
		fields = append(fields, "api_version")
	}
	return fields
}

// writeRevealUnavailable 是揭示端点的基础设施故障出口（fail-closed）。
func (h *ChannelHandler) writeRevealUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	writeProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
		"Credential reveal is temporarily unavailable")
}

// writeRevealNoStoreUnavailable 是「解密已完成但审计写不进去」的出口：
// 仍按 503 fail-closed，并显式 no-store，避免 5xx 响应被任何中间层缓存。
func (h *ChannelHandler) writeRevealNoStoreUnavailable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	h.writeRevealUnavailable(w)
}

// ---------- 列表筛选解析 ----------

func parseChannelFilter(r *http.Request) (store.ChannelFilter, *apiError) {
	q := r.URL.Query()
	f := store.ChannelFilter{
		Page:     1,
		PageSize: 20,
		Keyword:  strings.TrimSpace(q.Get("keyword")),
		Group:    strings.TrimSpace(q.Get("group")),
		Model:    strings.TrimSpace(q.Get("model")),
		Sort:     q.Get("sort"),
	}
	if raw := q.Get("p"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return f, validationError("p", "p must be a positive integer")
		}
		f.Page = n
	}
	if raw := q.Get("page_size"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return f, validationError("page_size", "page_size must be a positive integer")
		}
		f.PageSize = n
		if f.PageSize > 100 {
			f.PageSize = 100 // modules-06 F-06-01 上限
		}
	}
	if raw := q.Get("status"); raw != "" && raw != "all" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 3 {
			return f, validationError("status", "status must be one of 1, 2, 3 or all")
		}
		v := int16(n)
		f.Status = &v
	}
	if rawSort := q.Get("sort"); rawSort != "" && rawSort != "priority" && rawSort != "name" && rawSort != "response_time" {
		// response_time 是 F-06-01 前端会发送的第三种排序；M2 尚无 24h 聚合
		// 数据源，接受该值并落回默认 priority 顺序（不是静默拒绝，也不是
		// 假装按响应时间排序——store 只对 name 特判）。
		return f, validationError("sort", `sort must be "priority", "name" or "response_time"`)
	}

	typeValues := append([]string{}, q["type"]...)
	typeValues = append(typeValues, q["type[]"]...)
	types := make([]string, 0, len(typeValues))
	for _, raw := range typeValues {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !contract.ChannelType(part).Valid() {
				return f, validationError("type", "unknown channel type")
			}
			types = append(types, part)
		}
	}
	f.Types = types
	return f, nil
}

// ---------- 响应投影 ----------

func channelToResponse(ch *store.Channel) (ChannelResponse, error) {
	models := []string{}
	if len(ch.SupportedModels) > 0 {
		if err := json.Unmarshal(ch.SupportedModels, &models); err != nil {
			return ChannelResponse{}, fmt.Errorf("decode supported_models: %w", err)
		}
	}
	var groups []string
	if ch.GroupTags != nil {
		if err := json.Unmarshal(ch.GroupTags, &groups); err != nil {
			return ChannelResponse{}, fmt.Errorf("decode group_tags: %w", err)
		}
	}
	// 绑定投影：不透明 JSON 损坏时按空处理而不是 500（列表不能因单行损坏
	// 整体不可用）；写入路径的 CHECK 已保证数组形态。
	extendKeys := []string{}
	if len(ch.TaskExtendPluginKeys) > 0 {
		_ = json.Unmarshal(ch.TaskExtendPluginKeys, &extendKeys)
	}
	version := formatChannelTime(ch.UpdatedAt)
	return ChannelResponse{
		ID:                   ch.ID,
		Name:                 ch.Name,
		Type:                 ch.Type,
		Status:               ch.Status,
		Priority:             ch.Priority,
		Weight:               ch.Weight,
		SupportedModels:      models,
		GroupTags:            groups,
		RateLimit:            ch.RateLimit,
		AutoBan:              ch.AutoBan,
		TestModel:            ch.TestModel,
		Remark:               ch.Remark,
		Tag:                  ch.Tag,
		FailCount:            ch.FailCount,
		AutoDisabledReason:   ch.AutoDisabledReason,
		LastTestAt:           formatChannelTimePtr(ch.LastTestAt),
		LastTestResult:       ch.LastTestResult,
		TaskPluginKey:        ch.TaskPluginKey,
		TaskExtendPluginKeys: extendKeys,
		UpstreamAccountID:    ch.UpstreamAccountID,
		CreatedAt:            formatChannelTime(ch.CreatedAt),
		UpdatedAt:            version,
		Version:              version,
	}, nil
}

// detailResponse 在列表白名单上加 credentials_masked / credentials_set（GC-6 详情例外）。
//
// 解密失败（密钥轮换/数据损坏）时不报 500：credentials_set 仍如实反映「已配置」，
// 掩码留空；这样列表/详情不会因为一个坏行整体不可用，且绝不回传密文。
func (h *ChannelHandler) detailResponse(ch *store.Channel) (ChannelResponse, error) {
	resp, err := channelToResponse(ch)
	if err != nil {
		return ChannelResponse{}, err
	}
	credentialsSet := len(ch.Credentials) > 0
	resp.CredentialsSet = &credentialsSet
	masked := ChannelCredentialsMasked{}
	if credentialsSet {
		if plaintext, err := h.sealer.Open(ch.Credentials); err == nil {
			var creds ChannelCredentials
			if json.Unmarshal(plaintext, &creds) == nil {
				masked = maskCredentials(creds)
			}
		}
	}
	resp.CredentialsMasked = &masked
	return resp, nil
}

func formatChannelTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func formatChannelTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := formatChannelTime(*t)
	return &s
}

// ---------- 凭证封装 ----------

func (h *ChannelHandler) sealCredentials(v *validator, ct contract.ChannelType, creds ChannelCredentials) []byte {
	normalized := normalizeCredentials(ct, creds)
	raw, err := json.Marshal(normalized)
	if err != nil {
		v.add("credentials", "failed to encode credentials")
		return nil
	}
	sealed, err := h.sealer.Seal(raw)
	if err != nil {
		v.add("credentials", "failed to seal credentials")
		return nil
	}
	return sealed
}

// validateCredentials 实现 modules-06 F-06-03 的凭据校验。
// 返回字段级错误（不包含密钥值）。
func validateCredentials(ct contract.ChannelType, creds ChannelCredentials) []fieldError {
	var fields []fieldError
	if creds.APIKey == "" {
		fields = append(fields, fieldError{Field: "credentials.api_key", Message: "api_key is required"})
	} else if utf8.RuneCountInString(creds.APIKey) < 8 {
		fields = append(fields, fieldError{Field: "credentials.api_key", Message: "api_key must be at least 8 characters"})
	}
	if ct == contract.ChannelTypeAzure {
		deployment := strings.TrimSpace(creds.Deployment)
		if deployment == "" {
			fields = append(fields, fieldError{Field: "credentials.deployment", Message: "deployment is required for azure_openai"})
		} else if utf8.RuneCountInString(deployment) > 128 {
			fields = append(fields, fieldError{Field: "credentials.deployment", Message: "deployment must be at most 128 characters"})
		}
		apiVersion := strings.TrimSpace(creds.APIVersion)
		if apiVersion == "" {
			fields = append(fields, fieldError{Field: "credentials.api_version", Message: "api_version is required for azure_openai"})
		} else if _, err := time.Parse("2006-01-02", apiVersion); err != nil {
			fields = append(fields, fieldError{Field: "credentials.api_version", Message: "api_version must be YYYY-MM-DD"})
		}
	}
	return fields
}

// normalizeCredentials 只保留该类型有意义的字段（deployment/api_version 仅 azure）。
func normalizeCredentials(ct contract.ChannelType, creds ChannelCredentials) ChannelCredentials {
	if ct == contract.ChannelTypeAzure {
		return ChannelCredentials{
			APIKey:     creds.APIKey,
			Deployment: strings.TrimSpace(creds.Deployment),
			APIVersion: strings.TrimSpace(creds.APIVersion),
		}
	}
	return ChannelCredentials{APIKey: creds.APIKey}
}

// decodeCredentialsPayload 严格解码 credentials 输入（G-M6D-23）。
//
// DisallowUnknownFields 是本批次冻结的 fail-closed 口径：嵌套凭据里的未知
// 字段必须 400，而不是像旧实现那样静默忽略。静默忽略会把 `apiKey` 这类
// 拼写错误变成「没给密钥」，更新时悄悄保留旧凭据、创建时给出误导性的
// 「api_key is required」，掩盖真实的配置错误。
//
// 返回 (creds, hasValue, err)：
//   - hasValue=false：缺省或 JSON null（创建视为未提供；更新表示保持原值）；
//   - err 非空：结构非法。调用方用 unknownJSONField 区分「未知字段」（400）
//     与「类型错误」（沿用各端点既有的 400/422）。错误文本只来自
//     encoding/json，不含任何字段值，绝不把密钥片段回显到响应或日志。
func decodeCredentialsPayload(raw json.RawMessage) (ChannelCredentials, bool, error) {
	if raw == nil || isJSONNull(raw) {
		return ChannelCredentials{}, false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var creds ChannelCredentials
	if err := dec.Decode(&creds); err != nil {
		return ChannelCredentials{}, false, err
	}
	// 防御尾随数据：父对象解码已保证单个值，这里仍拒绝多值载荷，
	// 避免不同解码层对同一字节流产生不同视图（parser differential）。
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return ChannelCredentials{}, false, errors.New("trailing data after credentials")
	}
	return creds, true, nil
}

// unknownCredentialsFieldError 是嵌套凭据未知字段的固定 400 文案。
// 不携带字段名或值：错误体可能被前端日志/网关访问日志记录，凭据结构里
// 任何位置都可能存放密钥片段。
func unknownCredentialsFieldError() *apiError {
	return invalidRequestError("credentials contains an unknown field")
}

// decodeOptionalTaskPluginKey 严格解码 create 的 task_plugin_key。
//
// 缺省与 JSON null 都返回 (nil, nil)：两者在校验语义上都是「未提供/清空」，
// 但调用方用 raw != nil 单独判定字段是否出现（G-M6D-23 复核 M-1：显式 null
// 也必须触发敏感门）。其它类型返回 error，由调用方映射为 400。
func decodeOptionalTaskPluginKey(raw json.RawMessage) (*string, error) {
	if raw == nil || isJSONNull(raw) {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

// decodeTaskExtendPluginKeys 严格解码 create 的 task_extend_plugin_keys。
//
// 缺省与 JSON null 都返回 nil；数组解码为 []string；其它类型返回 error。
// 与 update 的 RawMessage 处理保持同一口径（数组元素类型由 json 包严格校验），
// 避免 create/update 对同一载荷给出不同判定。
func decodeTaskExtendPluginKeys(raw json.RawMessage) ([]string, error) {
	if raw == nil || isJSONNull(raw) {
		return nil, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	return values, nil
}

// ---------- 字段校验 ----------

const maxBaseURLBytes = 512

// modelNameRe 与 store/model_meta_validate.go 的模型名字符集一致（1–128 字符）。
//
// 【为什么允许 "/"】公开模型名带厂商前缀（如 `openai/gpt-5`）是既有命名，
// model_meta 与公开目录已按此字符集工作；渠道侧继续拒绝 "/" 会让这些模型
// 无法被任何渠道承载（参照侧缺陷修复）。
var modelNameRe = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)

// requiredBaseURLTypes 是 relay-02 §7.1 「必填」组（基线序号 3,8,36,45,54,60,61,62,63）。
var requiredBaseURLTypes = map[contract.ChannelType]struct{}{
	contract.ChannelTypeAzure:       {},
	contract.ChannelTypeCustom:      {},
	contract.ChannelTypeSunoAPI:     {},
	contract.ChannelTypeVolcEngine:  {},
	contract.ChannelTypeDoubaoVideo: {},
	contract.ChannelTypeNewAPI:      {},
	contract.ChannelTypeTaskPlugin:  {},
	contract.ChannelTypeVLLM:        {},
	contract.ChannelTypeSGLang:      {},
}

func requiresBaseURL(t contract.ChannelType) bool {
	_, ok := requiredBaseURLTypes[t]
	return ok
}

// validateBaseURL 是保存期的基础校验（协议/主机/userinfo/长度）。
// 完整的 SSRF 策略（私网段、端口白名单、域名三态）属 security-04 的
// EgressGuard.ValidateURL，不在本任务实现；这里不解析 DNS、不回显 IP。
func validateBaseURL(raw string) string {
	if len(raw) > maxBaseURLBytes {
		return "base_url must be at most 512 characters"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "base_url must be a valid http/https URL"
	}
	if u.User != nil {
		return "base_url must not contain userinfo"
	}
	return ""
}

// channelBaseURL 返回渠道库里存的 base_url（nil 视为空串）。
func channelBaseURL(ch *store.Channel) string {
	if ch == nil || ch.BaseURL == nil {
		return ""
	}
	return strings.TrimSpace(*ch.BaseURL)
}

// normalizeChannelBaseURL 把 base_url 归一成可比较的形式：去尾部斜杠、
// scheme 与 host 小写。只用于同进程内的「是不是同一个上游」比较，不改变
// 真正发给上游的地址（路径大小写仍然敏感）。
func normalizeChannelBaseURL(raw string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return ""
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return trimmed
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return strings.TrimRight(u.String(), "/")
}

func validateSupportedModels(v *validator, in []string) ([]string, []byte) {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, m := range in {
		if m == "" || len(m) > 128 || !modelNameRe.MatchString(m) {
			// modules-06 F-06-04：1–128 字符，字母数字与 ._-:/ 允许（"/" 见
			// modelNameRe 的注释）。
			v.add("supported_models", fmt.Sprintf("supported_models[%d] is invalid", i))
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	if len(out) == 0 {
		v.add("supported_models", "at least one model is required")
	}
	raw, _ := json.Marshal(out)
	return out, raw
}

// parseGroupTags：nil/JSON null → (nil,false)；数组 → (JSON,true)。
// group_tags NULL = 全部分组，空数组被 DB CHECK 拒绝（modules-06 F-06-04）。
func parseGroupTags(v *validator, raw json.RawMessage) ([]byte, bool) {
	if raw == nil || isJSONNull(raw) {
		return nil, false
	}
	var tags []string
	if err := json.Unmarshal(raw, &tags); err != nil {
		v.add("group_tags", "group_tags must be an array of strings or null")
		return nil, true
	}
	out := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			v.add("group_tags", "group_tags must not contain empty values")
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	if len(out) == 0 {
		v.add("group_tags", "group_tags must contain at least one group or be null")
		return nil, true
	}
	encoded, _ := json.Marshal(out)
	return encoded, true
}

// parseModelMappingCreate：创建时缺省/null → '{}'（DB NOT NULL）。
func parseModelMappingCreate(v *validator, raw json.RawMessage) ([]byte, bool) {
	if raw == nil || isJSONNull(raw) {
		return []byte(`{}`), false
	}
	if err := validateJSONObject(raw, "model_mapping", v); err != nil {
		return nil, false
	}
	// 对象形状在此校验；键 ⊆ supported_models 的覆盖校验由调用方
	// validateModelMappingCoverage 完成（F-06-10）。
	return raw, true
}

// parseModelMappingPatch：缺省 → 不修改；null → 重置为 '{}'。
func parseModelMappingPatch(v *validator, raw json.RawMessage) ([]byte, bool) {
	if isJSONNull(raw) {
		return []byte(`{}`), true
	}
	if err := validateJSONObject(raw, "model_mapping", v); err != nil {
		return nil, false
	}
	return raw, true
}

func validateJSONObject(raw json.RawMessage, field string, v *validator) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		v.add(field, field+" must be a JSON object")
		return errors.New("not an object")
	}
	return nil
}

// parseRateLimit：nil → 不设置；null → 清空；数字 → 1..100000。
// 返回 (值, 是否出现)。
func parseRateLimit(v *validator, raw json.RawMessage) (*int32, bool) {
	if raw == nil {
		return nil, false
	}
	if isJSONNull(raw) {
		return nil, true
	}
	var n int32
	if err := json.Unmarshal(raw, &n); err != nil {
		v.add("rate_limit", "rate_limit must be an integer or null")
		return nil, true
	}
	if n < 1 || n > 100000 {
		v.add("rate_limit", "rate_limit must be between 1 and 100000")
		return nil, true
	}
	return &n, true
}

// parseOptionalStringRaw 区分 "缺省"（调用方不调用本函数）、null、字符串。
func parseOptionalStringRaw(raw json.RawMessage) (*string, bool) {
	if isJSONNull(raw) {
		return nil, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, false
	}
	return &s, true
}

func normalizeOptionalChannelText(p *string) *string {
	if p == nil {
		return nil
	}
	s := strings.TrimSpace(*p)
	if s == "" {
		return nil
	}
	return &s
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func decodeStringSlice(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// ---------- HTTP 工具 ----------

const maxChannelBodyBytes = 1 << 20

func decodeChannelJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxChannelBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	// 拒绝尾随数据，避免「前一个对象合法、后面夹带内容」的歧义请求。
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing data after JSON body")
	}
	return nil
}

// unknownJSONField 从 encoding/json 的 unknown-field 错误中提取字段名。
//
// 渠道写端点的 DTO 是写侧字段白名单（BOPLA，security-02 §2.6.1）：未登记
// 字段必须 403 field_not_writable，而不是静默丢弃或退化成普通 400。用错误
// 文本解析而不是维护第二份字段清单，保证「DTO 加了字段」与「白名单加了
// 字段」永远是同一处修改（清单漂移不可能发生）。
func unknownJSONField(err error) (string, bool) {
	const prefix = `json: unknown field "`
	msg := err.Error()
	idx := strings.Index(msg, prefix)
	if idx < 0 {
		return "", false
	}
	rest := msg[idx+len(prefix):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// fieldNotWritableResponse 生成 403 field_not_writable（供幂等执行体返回
// status+body 的调用路径使用）。
func fieldNotWritableResponse(field string) (int, []byte, bool) {
	return http.StatusForbidden, fieldNotWritableProblem(field).marshal(), false
}

func fieldNotWritableProblem(field string) *apiError {
	return &apiError{
		Status: http.StatusForbidden,
		Type:   problemTypeFieldNotWritable,
		Title:  "Field not writable",
		Detail: "request body contains fields that this action may not write",
		Fields: []fieldError{{Field: field, Message: "field is not writable"}},
	}
}

func writeFieldNotWritableProblem(w http.ResponseWriter, field string) {
	e := fieldNotWritableProblem(field)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(e.Status)
	_, _ = w.Write(e.marshal())
}

type fieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

type validator struct {
	fields []fieldError
}

func (v *validator) add(field, message string) {
	v.fields = append(v.fields, fieldError{Field: field, Message: message})
}

func (v *validator) addFields(fields []fieldError) {
	v.fields = append(v.fields, fields...)
}

func (v *validator) err() *apiError {
	if len(v.fields) == 0 {
		return nil
	}
	return &apiError{
		Status: http.StatusUnprocessableEntity,
		Type:   problemTypeValidationFailed,
		Title:  "Validation failed",
		Fields: v.fields,
	}
}

// apiError 是可序列化为 RFC7807 problem+json 的内部错误。
type apiError struct {
	Status int
	Type   string
	Title  string
	Detail string
	Fields []fieldError
}

func (e *apiError) marshal() []byte {
	p := dto.Problem{Type: e.Type, Title: e.Title, Status: e.Status}
	if e.Detail != "" {
		detail := e.Detail
		p.Detail = &detail
	}
	if len(e.Fields) > 0 {
		p.Set("errors", e.Fields)
	}
	body, err := json.Marshal(p)
	if err != nil {
		return []byte(`{"type":"https://newtg.dev/problems/internal_error","title":"Internal Server Error","status":500}`)
	}
	return body
}

func errorResponse(e *apiError) (int, []byte, bool) {
	return e.Status, e.marshal(), false
}

func writeAPIError(w http.ResponseWriter, e *apiError) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(e.Status)
	_, _ = w.Write(e.marshal())
}

func writeChannelJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeAPIError(w, internalAPIError())
		return
	}
	writeJSONBytes(w, status, body)
}

func writeJSONBytes(w http.ResponseWriter, status int, body []byte) {
	// 4xx/5xx 一律按 RFC7807 problem+json 输出：幂等重放路径与直写路径
	// 必须给出相同的 Content-Type，否则同一响应在首次/重放之间会漂移。
	contentType := "application/json"
	if status >= 400 {
		contentType = "application/problem+json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// 错误 type URI 采用 modules-03/09 的 https://newtg.dev/problems/<slug> 约定。
// problemTypeInvalidRequest 复用 handlers_token.go 的既有常量（值相同），
// 避免同名常量重复声明。
const (
	problemTypeValidationFailed       = "https://newtg.dev/problems/validation_failed"
	problemTypeInternal               = "https://newtg.dev/problems/internal_error"
	problemTypeChannelNotFound        = "https://newtg.dev/problems/channel_not_found"
	problemTypeChannelNameConflict    = "https://newtg.dev/problems/channel_name_conflict"
	problemTypeChannelVersionConflict = "https://newtg.dev/problems/channel_version_conflict"
	problemTypeIdempotencyInProgress  = "https://newtg.dev/problems/idempotency_in_progress"
	problemTypeFieldNotWritable       = "https://newtg.dev/problems/field_not_writable"
)

func validationError(field, message string) *apiError {
	return &apiError{
		Status: http.StatusUnprocessableEntity,
		Type:   problemTypeValidationFailed,
		Title:  "Validation failed",
		Fields: []fieldError{{Field: field, Message: message}},
	}
}

func invalidRequestError(detail string) *apiError {
	return &apiError{
		Status: http.StatusBadRequest,
		Type:   problemTypeInvalidRequest,
		Title:  "Invalid request",
		Detail: detail,
	}
}

func internalAPIError() *apiError {
	// 不携带任何内部错误文本（GC-6）。
	return &apiError{
		Status: http.StatusInternalServerError,
		Type:   problemTypeInternal,
		Title:  "Internal Server Error",
	}
}

func channelNotFoundError() *apiError {
	return &apiError{
		Status: http.StatusNotFound,
		Type:   problemTypeChannelNotFound,
		Title:  "Channel not found",
	}
}

func storeErrorResponse(err error) *apiError {
	switch {
	case errors.Is(err, store.ErrChannelNotFound):
		return channelNotFoundError()
	case errors.Is(err, store.ErrNameConflict):
		return &apiError{
			Status: http.StatusConflict,
			Type:   problemTypeChannelNameConflict,
			Title:  "Channel name already exists",
		}
	case errors.Is(err, store.ErrConflict):
		return &apiError{
			Status: http.StatusConflict,
			Type:   problemTypeChannelVersionConflict,
			Title:  "Channel version conflict",
			Detail: "the channel was modified by someone else; refresh and retry",
		}
	case errors.Is(err, store.ErrInvalidChannel):
		return &apiError{
			Status: http.StatusUnprocessableEntity,
			Type:   problemTypeValidationFailed,
			Title:  "Validation failed",
			Detail: "the channel violates a data constraint",
		}
	default:
		return internalAPIError()
	}
}

func channelIDFromRequest(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func validateBatchIDs(ids []int64) *apiError {
	if len(ids) == 0 {
		return validationError("ids", "ids must not be empty")
	}
	if len(ids) > 200 {
		// modules-06 F-06-02：单次 ≤200 条。
		return validationError("ids", "at most 200 ids are allowed per batch operation")
	}
	for _, id := range ids {
		if id <= 0 {
			return validationError("ids", "ids must be positive integers")
		}
	}
	return nil
}

// serveIdempotent 把「抢占 → 执行 → 写回/释放」封装为统一流程。
//
// finding 2（幂等竞态）：旧实现先 build() 再 SetIfAbsent，两个并发同键请求
// 都会执行副作用。现在先 Claim（SET NX pending 标记）：
//   - Acquired → 执行 build；2xx 用 Complete 把 pending 替换为响应（24h TTL），
//     非 2xx/error 用 Release 删除标记，客户端修正参数后可以重试；
//   - Replay   → 直接返回首次响应（Idempotent-Replayed: true）；
//   - InFlight → 409 + Retry-After，且**不执行** build（副作用只有一次）。
//
// 只缓存 2xx：4xx/5xx 不应被固化 24h（客户端修正参数后应能重试）。
// 缓存故障 fail-open：幂等层不可用不阻断业务；多副本场景的重复写由
// channels_name_uq 等约束兜底。这是有意的可用性权衡，phase 8B 应注入 Redis 实现。
//
// 幂等键作用域（安全边界）：客户端提供的 Idempotency-Key 只是**命名空间内的名字**，
// 存储键必须再绑定「用户 + 操作（方法/路径）」。否则：
//   - 同一个 key 用在 POST /api/channels 与 POST /api/channels/batch-delete 时，
//     第二个请求会重放第一个的响应并跳过删除；
//   - 另一个管理员获知 key 后可以读到他无权发起的首次响应。
func (h *ChannelHandler) serveIdempotent(w http.ResponseWriter, r *http.Request, build func() (int, []byte, bool)) {
	headerKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if headerKey == "" {
		status, body, _ := build()
		writeJSONBytes(w, status, body)
		return
	}
	key := idempotencyScopeKey(r, headerKey)
	// pending 用短 TTL（finding 2）：崩溃/panic 留下的标记最多锁 60s。
	claim, err := h.idem.Claim(r.Context(), key, IdempotencyPendingTTL)
	if err != nil {
		// fail-open：幂等存储不可用不阻断业务。
		status, body, _ := build()
		writeJSONBytes(w, status, body)
		return
	}

	switch claim.Status {
	case IdempotencyClaimReplay:
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSONBytes(w, claim.Response.Status, claim.Response.Body)
		return
	case IdempotencyClaimInFlight:
		// 同键请求仍在执行：绝不能并发执行 build（副作用翻倍）。
		// 409 + Retry-After 提示客户端稍后用**同一个 key** 重试，届时会命中重放。
		w.Header().Set("Retry-After", "1")
		writeAPIError(w, &apiError{
			Status: http.StatusConflict,
			Type:   problemTypeIdempotencyInProgress,
			Title:  "Idempotency key is in progress",
			Detail: "another request with this Idempotency-Key is still being processed; retry shortly",
		})
		return
	}

	status, body, _ := h.executeClaimed(r, key, claim.Token, build)
	writeJSONBytes(w, status, body)
}

// executeClaimed 在持有 pending 标记时执行 build，并保证每个出口都 finalize：
//   - 2xx → Complete（响应保留 IdempotencyTTL=24h）；
//   - 非 2xx → Release（允许修正参数后用同一个 key 重试）；
//   - build panic 或任何未显式 finalize 的出口 → defer Release 兜底；
//   - finalize 一律使用 context.WithoutCancel(r.Context()) + 5s 超时：
//     客户端断开不应阻止标记清理/响应固化，否则同键客户端只能干等 TTL；
//   - finalize 失败写日志（不丢弃），让 Redis 故障可观测。
func (h *ChannelHandler) executeClaimed(r *http.Request, key, token string, build func() (int, []byte, bool)) (int, []byte, bool) {
	finalized := false
	// panic 兜底：显式 finalize 之前的所有出口（含 panic）都释放标记。
	defer func() {
		if finalized {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), idempotencyFinalizeTimeout)
		defer cancel()
		if err := h.idem.Release(ctx, key, token); err != nil {
			h.log.Error("httpapi: idempotency release after panic failed", "err", err)
		}
	}()

	status, body, cacheable := build()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), idempotencyFinalizeTimeout)
	defer cancel()
	if cacheable && status >= http.StatusOK && status < http.StatusMultipleChoices {
		// 完成：用响应替换 pending 标记并续 24h TTL（失败不影响已生成的响应）。
		if err := h.idem.Complete(ctx, key, token,
			IdempotentResponse{Status: status, Body: body}, IdempotencyTTL); err != nil {
			h.log.Error("httpapi: idempotency complete failed", "err", err)
		}
	} else {
		// 非 2xx 不固化：释放标记，允许客户端修正参数后用同一个 key 重试。
		if err := h.idem.Release(ctx, key, token); err != nil {
			h.log.Error("httpapi: idempotency release failed", "err", err)
		}
	}
	finalized = true
	return status, body, cacheable
}

// idempotencyScopeKey 返回存储层使用的幂等键：`<userID>:<method> <path>:<headerKey>`。
//
// userID 取自已认证 Principal（authorize 已保证存在）；匿名（理论上不可达）用 0，
// 仍然按操作隔离。路径区分操作，方法冗余但便于运维直读 Redis 键时排障。
func idempotencyScopeKey(r *http.Request, headerKey string) string {
	userID := int64(0)
	if p, ok := auth.PrincipalFrom(r.Context()); ok && p != nil {
		userID = p.UserID
	}
	return fmt.Sprintf("%d:%s %s:%s", userID, r.Method, r.URL.Path, headerKey)
}

// checkSensitiveWriteFields 实现渠道写端点的字段级授权（BOPLA，G-M6D-23；
// security-02 §2.6.1；modules-06 F-06-03 凭据写保护）。
//
// 冻结口径：请求体一旦包含 credentials（含其嵌套字段）、task_plugin_key
// 或 task_extend_plugin_keys（类型 60 多绑，复核 I-1），调用者除
// channels:manage 外还必须持有 channels:manage_sensitive；缺失时返回固定
// 的 403 problem，并在任何校验通过/写库之前拒绝整笔请求。
//
// 「出现即敏感」是与设计一致的保守选择：显式 null / 空对象在更新时虽表示
// 「保持原值」，但控制台全量回传表单时若允许无敏感位的管理员提交，凭据分区
// 的写入面就重新打开（也掩盖了调用者确实意图操作凭据字段）。
// 非敏感字段（name/base_url/model_mapping/status 等）的权限完全不变。
func (h *ChannelHandler) checkSensitiveWriteFields(ctx context.Context, credentialsPresent, taskPluginKeyPresent, taskExtendPluginKeysPresent bool) *apiError {
	if !credentialsPresent && !taskPluginKeyPresent && !taskExtendPluginKeysPresent {
		return nil
	}
	err := CheckPermission(ctx, h.access, PermChannelsManageSensitive)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNoPrincipal), errors.Is(err, ErrUserDeleted):
		// 与 authorize 的映射一致：身份失效不是权限不足。
		return &apiError{
			Status: http.StatusUnauthorized,
			Type:   problemTypeUnauthorized,
			Title:  "Unauthorized",
		}
	case errors.Is(err, ErrAccessStore):
		// 不把 DB 故障伪装成 403（与 authorize 同口径）。
		return internalAPIError()
	default:
		return sensitiveFieldForbiddenError()
	}
}

// sensitiveFieldForbiddenError 是字段级授权拒绝的固定 problem。
//
// 类型沿用 security-02 §2.6.1 的 field_not_writable：调用者通过了功能级
// channels:manage，但没有该字段分区的门 channels:manage_sensitive。文案固定、
// 不含字段名或字段值，避免错误体/访问日志回显请求内容（可能含密钥）。
func sensitiveFieldForbiddenError() *apiError {
	return &apiError{
		Status: http.StatusForbidden,
		Type:   problemTypeFieldNotWritable,
		Title:  "Field not writable",
		Detail: "credential fields require the channels:manage_sensitive permission",
	}
}

// authorize 把 CheckPermission 的三种结果映射为 HTTP：
// 未鉴权 401、权限不足 403、存储故障 500（不把 DB 故障伪装成 403）。
func (h *ChannelHandler) authorize(w http.ResponseWriter, r *http.Request, required string) bool {
	err := CheckPermission(r.Context(), h.access, required)
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrNoPrincipal), errors.Is(err, ErrUserDeleted):
		writeUnauthorized(w)
	case errors.Is(err, ErrAccessStore):
		writeProblem(w, http.StatusInternalServerError, problemTypeInternal, "Internal Server Error")
	default:
		writeForbidden(w)
	}
	return false
}
