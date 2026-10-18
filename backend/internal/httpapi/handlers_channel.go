package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/api/dto"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// 本文件是 Task 8 phase 8A 的管理面渠道端点：
//
//	GET    /api/channels                  channels:read
//	POST   /api/channels                  channels:manage        + Idempotency-Key
//	GET    /api/channels/{id}             channels:read
//	PATCH  /api/channels/{id}             channels:manage        + expected_version（R-P8）
//	DELETE /api/channels/{id}             channels:manage_sensitive
//	PATCH  /api/channels/batch            channels:manage        {action, ids}
//	POST   /api/channels/batch-delete     channels:manage_sensitive + Idempotency-Key
//	POST   /api/channels/{id}/test        channels:manage
//
// 端点形状按 R-P5（modules-06 的 REST 形态，不是计划草案的 colon action）。
// 安全不变量（GC-6/GC-8）：响应只走白名单字段；base_url 与凭据明文永不出现；
// 错误体不含上游 URL、密钥或内部 SQL 文本。

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

// ChannelTester 是连通性测试接缝（modules-06 F-06-06）。phase 8B 的真实实现
// 负责构造适配器、以 max_tokens=1 发一次上游请求；本任务只约束 handler 的错误
// 映射（失败 → 泛化 RFC7807，绝不回传上游 URL/key）。
type ChannelTester func(ctx context.Context, ch *store.Channel, model string) (latencyMS int, err error)

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
// 生命周期（M2 final review finding 2）：
//  1. Claim：SET NX 写入 pending 标记。抢到 → Acquired（带 token）；
//     同键已有完成响应 → Replay；同键 pending → InFlight。
//  2. Acquired 的请求执行 build()；2xx 时 Complete 用响应替换 pending（24h TTL），
//     非 2xx/错误时 Release 删除标记，让客户端修正参数后可以重试。
//  3. 另一个并发同键请求在 1 就已返回 InFlight，**不会**执行 build——这正是修复
//     「先执行后 SetIfAbsent 导致双方都执行」的关键。
type IdempotencyStore interface {
	Claim(ctx context.Context, key string, ttl time.Duration) (IdempotencyClaim, error)
	Complete(ctx context.Context, key, token string, resp IdempotentResponse, ttl time.Duration) error
	Release(ctx context.Context, key, token string) error
}

// IdempotencyTTL 冻结 24h：与参照侧幂等窗口同量级；过期后同键可再次执行。
const IdempotencyTTL = 24 * time.Hour

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

// RedisIdempotencyStore 是生产实现：pending/响应值共用 24h TTL。
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
// 刻意不包含 base_url、credentials、model_mapping、auto_disabled_reason。
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
	LastTestAt      *string  `json:"last_test_at"`
	LastTestResult  *bool    `json:"last_test_result"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
	Version         string   `json:"version"` // R-P8：derived updated_at（RFC3339Nano UTC）

	CredentialsMasked *ChannelCredentialsMasked `json:"credentials_masked,omitempty"`
	CredentialsSet    *bool                     `json:"credentials_set,omitempty"`
}

type channelListResponse struct {
	Items    []ChannelResponse `json:"items"`
	Total    int               `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
}

type createChannelRequest struct {
	Name            string              `json:"name"`
	Type            string              `json:"type"`
	BaseURL         *string             `json:"base_url"`
	Credentials     *ChannelCredentials `json:"credentials"`
	SupportedModels []string            `json:"supported_models"`
	GroupTags       json.RawMessage     `json:"group_tags"`
	ModelMapping    json.RawMessage     `json:"model_mapping"`
	Weight          *int16              `json:"weight"`
	Priority        *int16              `json:"priority"`
	RateLimit       json.RawMessage     `json:"rate_limit"`
	Status          *int16              `json:"status"` // 创建恒为 1；此字段仅为兼容前端全量表单
	AutoBan         *bool               `json:"auto_ban"`
	TestModel       *string             `json:"test_model"`
	Remark          *string             `json:"remark"`
	Tag             *string             `json:"tag"`
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

// ---------- Handler ----------

// ChannelHandlerOptions 是 phase 8B wiring 的注入点。
type ChannelHandlerOptions struct {
	Tester      ChannelTester
	Idempotency IdempotencyStore
}

// ChannelHandler 持有渠道端点依赖。字段不导出，phase 8B 通过构造函数注入。
type ChannelHandler struct {
	channels ChannelStore
	access   UserAccessStore
	sealer   *SecretSealer
	tester   ChannelTester
	idem     IdempotencyStore
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
	return &ChannelHandler{
		channels: channels,
		access:   access,
		sealer:   sealer,
		tester:   opts.Tester,
		idem:     idem,
	}
}

// Register 注册 Go 1.22 ServeMux 方法+通配符模式。
// 字面量 /batch 比 /{id} 更具体，ServeMux 会优先匹配前者。
func (h *ChannelHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/channels", h.handleList)
	mux.HandleFunc("POST /api/channels", h.handleCreate)
	mux.HandleFunc("PATCH /api/channels/batch", h.handleBatchStatus)
	mux.HandleFunc("POST /api/channels/batch-delete", h.handleBatchDelete)
	mux.HandleFunc("GET /api/channels/{id}", h.handleGet)
	mux.HandleFunc("PATCH /api/channels/{id}", h.handleUpdate)
	mux.HandleFunc("DELETE /api/channels/{id}", h.handleDelete)
	mux.HandleFunc("POST /api/channels/{id}/test", h.handleTest)
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
	writeJSON(w, http.StatusOK, out)
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
	writeJSON(w, http.StatusOK, resp)
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
		return errorResponse(invalidRequestError("request body is not valid JSON"))
	}

	v := &validator{}
	ch := h.channelFromCreate(v, &req)
	if e := v.err(); e != nil {
		return e.Status, e.marshal(), false
	}

	created, err := h.channels.Create(r.Context(), ch)
	if err != nil {
		return errorResponse(storeErrorResponse(err))
	}
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
func (h *ChannelHandler) channelFromCreate(v *validator, req *createChannelRequest) *store.Channel {
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

	testModel := normalizeOptionalText(req.TestModel)
	if testModel != nil {
		if len(models) == 0 || !slices.Contains(models, *testModel) {
			// DB 的 channels_test_model_in_models_chk 兜底；这里提前给字段级错误。
			v.add("test_model", "test_model must be one of supported_models")
		}
	}
	remark := normalizeOptionalText(req.Remark)
	if remark != nil && utf8.RuneCountInString(*remark) > 255 {
		v.add("remark", "remark must be at most 255 characters")
	}
	tag := normalizeOptionalText(req.Tag)
	if tag != nil && utf8.RuneCountInString(*tag) > 32 {
		v.add("tag", "tag must be at most 32 characters")
	}

	var sealed []byte
	if req.Credentials == nil {
		v.add("credentials", "credentials are required")
	} else {
		v.addFields(validateCredentials(ct, *req.Credentials))
		if len(v.fields) == 0 {
			sealed = h.sealCredentials(v, ct, *req.Credentials)
		}
	}

	return &store.Channel{
		Name:            name,
		Type:            string(ct),
		BaseURL:         baseURL,
		Credentials:     sealed,
		SupportedModels: modelsJSON,
		GroupTags:       groupTags,
		ModelMapping:    mapping,
		Weight:          weight,
		Priority:        priority,
		RateLimit:       rateLimit,
		Status:          1, // F-06-03：创建时 status 恒为 enabled(1)
		AutoBan:         autoBan,
		TestModel:       testModel,
		Remark:          remark,
		Tag:             tag,
	}
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
		writeAPIError(w, invalidRequestError("request body is not valid JSON"))
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
	patch := h.buildPatch(v, existing, &req)
	if e := v.err(); e != nil {
		writeAPIError(w, e)
		return
	}

	updated, err := h.channels.Update(r.Context(), id, patch, expected)
	if err != nil {
		writeAPIError(w, storeErrorResponse(err))
		return
	}
	resp, err := h.detailResponse(updated)
	if err != nil {
		writeAPIError(w, internalAPIError())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildPatch 把 PATCH 载荷（含 null/缺省区分）转成 store.ChannelPatch，
// 并对「合并后的有效状态」做校验（例如已有 azure 渠道只改 remark 时，
// 仍要保证 effective base_url 非空）。
func (h *ChannelHandler) buildPatch(v *validator, existing *store.Channel, req *updateChannelRequest) store.ChannelPatch {
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
		if *req.Status == 1 {
			// 手动启用同时清零失败计数与自动禁用原因（F-06-08/TC-F-06-08-02）。
			zero := int32(0)
			patch.FailCount = &zero
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

	credentialsProvided := false
	if req.Credentials != nil {
		// F-06-03：null / 缺省 / 空 api_key → 保持原值，不重新加密。
		// 只有非空 api_key 才触发校验与封装。
		if !isJSONNull(req.Credentials) {
			var creds ChannelCredentials
			if err := json.Unmarshal(req.Credentials, &creds); err != nil {
				v.add("credentials", "credentials must be an object or null")
			} else if creds.APIKey != "" {
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

	return patch
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
	writeJSON(w, http.StatusOK, res)
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
	if testErr != nil {
		// 仍回写失败结果（F-06-06 主流程 3）；回写失败不覆盖测试错误。
		_ = h.channels.UpdateTestResult(r.Context(), id, now, false)
		// modules-06 F-06-06：连通性失败是**业务结果**而不是 HTTP 故障，
		// 返回 200 {success:false, error_class}，便于前端展示失败原因。
		// GC-6：只给泛化枚举 + 固定字段，原始错误（可能含 URL/key）整体丢弃。
		writeJSON(w, http.StatusOK, channelTestResponse{
			Success:    false,
			LatencyMS:  latency,
			Model:      model,
			ErrorClass: channelTestErrorClass(testErr),
		})
		return
	}
	if err := h.channels.UpdateTestResult(r.Context(), id, now, true); err != nil {
		writeAPIError(w, storeErrorResponse(err))
		return
	}
	writeJSON(w, http.StatusOK, channelTestResponse{Success: true, LatencyMS: latency, Model: model})
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
	version := formatChannelTime(ch.UpdatedAt)
	return ChannelResponse{
		ID:              ch.ID,
		Name:            ch.Name,
		Type:            ch.Type,
		Status:          ch.Status,
		Priority:        ch.Priority,
		Weight:          ch.Weight,
		SupportedModels: models,
		GroupTags:       groups,
		RateLimit:       ch.RateLimit,
		AutoBan:         ch.AutoBan,
		TestModel:       ch.TestModel,
		Remark:          ch.Remark,
		Tag:             ch.Tag,
		FailCount:       ch.FailCount,
		LastTestAt:      formatChannelTimePtr(ch.LastTestAt),
		LastTestResult:  ch.LastTestResult,
		CreatedAt:       formatChannelTime(ch.CreatedAt),
		UpdatedAt:       version,
		Version:         version,
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

// ---------- 字段校验 ----------

const maxBaseURLBytes = 512

var modelNameRe = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

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

func validateSupportedModels(v *validator, in []string) ([]string, []byte) {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for i, m := range in {
		if m == "" || len(m) > 128 || !modelNameRe.MatchString(m) {
			// modules-06 F-06-04：1–128 字符，字母数字与 ._-: 允许。
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
	// 映射键覆盖校验（F-06-10）属后续任务；此处只保证 JSON 对象形状。
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

func normalizeOptionalText(p *string) *string {
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

func writeJSON(w http.ResponseWriter, status int, v any) {
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
const (
	problemTypeValidationFailed       = "https://newtg.dev/problems/validation_failed"
	problemTypeInvalidRequest         = "https://newtg.dev/problems/invalid_request"
	problemTypeInternal               = "https://newtg.dev/problems/internal_error"
	problemTypeChannelNotFound        = "https://newtg.dev/problems/channel_not_found"
	problemTypeChannelNameConflict    = "https://newtg.dev/problems/channel_name_conflict"
	problemTypeChannelVersionConflict = "https://newtg.dev/problems/channel_version_conflict"
	problemTypeIdempotencyInProgress  = "https://newtg.dev/problems/idempotency_in_progress"
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
	claim, err := h.idem.Claim(r.Context(), key, IdempotencyTTL)
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

	status, body, cacheable := build()
	if cacheable && status >= http.StatusOK && status < http.StatusMultipleChoices {
		// 完成：用响应替换 pending 标记并续 24h TTL（失败不影响已生成的响应）。
		_ = h.idem.Complete(r.Context(), key, claim.Token, IdempotentResponse{Status: status, Body: body}, IdempotencyTTL)
	} else {
		// 非 2xx 不固化：释放标记，允许客户端修正参数后用同一个 key 重试。
		_ = h.idem.Release(r.Context(), key, claim.Token)
	}
	writeJSONBytes(w, status, body)
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
