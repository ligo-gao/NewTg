package httpapi

// W8/MJ-a2 提交端点的单元验收（全内存：fake 渠道/任务存储 + httptest 假上游）。
//
// 覆盖冻结裁决：
//   - 12 条提交端点全部可达，动作/模型/渠道/锁定按设计映射（relay-05 §1/§4.1）；
//   - 提交时 PreConsume 恰好一次；提交失败不落行、Refund 恰好一次（R-MJ-01/13）；
//   - code 21 同用户复用、跨用户隔离（R-MJ-05/06 第一步）；
//   - upload-discord-images 生成 inline 终态行并在提交时结清（R-MJ-14）；
//   - 认证载体 mj-api-secret（哨兵 Authorization 并存）与固定错误信封（R-MJ-17）；
//   - 限流接缝：MJ 提交计入 B10/B11/B12（R-M5-07）；
//   - 输入上界在归一化层拒绝（R-MJ-16）。
//
// 真实 PostgreSQL（任务行/family/索引/结算锚点）的端到端断言在
// handlers_mj_integration_test.go（-tags=integration）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/egress"
	"newtg/gateway/internal/mj"
	"newtg/gateway/internal/ratelimit"
	"newtg/gateway/internal/tasks"
)

// ---------- fakes ----------

// mjTokenLookup 按 key hash 返回令牌，支持同一 handler 上切换用户。
type mjTokenLookup map[string]*auth.TokenMeta

func (m mjTokenLookup) Resolve(_ context.Context, keyHash string) (*auth.TokenMeta, error) {
	meta, ok := m[keyHash]
	if !ok || meta == nil {
		return nil, errors.New("mj test token lookup: not found")
	}
	copied := *meta
	return &copied, nil
}

// newMJTokens 构造两个用户（42/43）的令牌表。
func newMJTokens() (mjTokenLookup, string, string) {
	meta42 := &auth.TokenMeta{ID: 1, UserID: 42, Status: "enabled", Group: "default", UnlimitedQuota: true}
	meta43 := &auth.TokenMeta{ID: 2, UserID: 43, Status: "enabled", Group: "default", UnlimitedQuota: true}
	key42, key43 := "nkt-mj-user-42", "nkt-mj-user-43"
	return mjTokenLookup{
		auth.HashTokenKey(key42): meta42,
		auth.HashTokenKey(key43): meta43,
	}, key42, key43
}

type mjBillingUpdate struct {
	ID    int64
	Quota *int64
	Flags map[string]any
}

// fakeMJStore 是任务行存储的内存实现。
type fakeMJStore struct {
	mu             sync.Mutex
	inserted       []*tasks.Task
	byUserUpstream map[string]*tasks.Task
	updates        []mjBillingUpdate
	insertErr      error
	lookupErr      error
	nextID         int64
}

func newFakeMJStore() *fakeMJStore {
	return &fakeMJStore{byUserUpstream: map[string]*tasks.Task{}}
}

func mjUserUpstreamKey(userID int64, upstreamID string) string {
	return fmt.Sprintf("%d/%s", userID, upstreamID)
}

func (s *fakeMJStore) Insert(_ context.Context, t *tasks.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.insertErr != nil {
		return s.insertErr
	}
	s.nextID++
	t.ID = s.nextID
	s.inserted = append(s.inserted, t)
	return nil
}

func (s *fakeMJStore) GetByUserUpstreamID(_ context.Context, userID int64, upstreamID string) (*tasks.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	if task, ok := s.byUserUpstream[mjUserUpstreamKey(userID, upstreamID)]; ok {
		return task, nil
	}
	return nil, tasks.ErrTaskNotFound
}

// ListByUserUpstreamIDs 模仿生产 SQL 的 (user_id, family='mj') 过滤。
func (s *fakeMJStore) ListByUserUpstreamIDs(_ context.Context, userID int64, ids []string, _ int) ([]*tasks.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	out := make([]*tasks.Task, 0, len(ids))
	for _, id := range ids {
		if task, ok := s.byUserUpstream[mjUserUpstreamKey(userID, id)]; ok {
			if task.Family == tasks.FamilyMJ {
				out = append(out, task)
			}
		}
	}
	return out, nil
}

// ListOwnersByUpstreamID 模仿生产 SQL 的全局 MjId 查询：family='mj' 过滤、
// 按 (user_id, id) 固定排序并在 limit 处截断。
func (s *fakeMJStore) ListOwnersByUpstreamID(_ context.Context, upstreamID string, limit int) ([]*tasks.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	out := make([]*tasks.Task, 0, 4)
	for _, task := range s.byUserUpstream {
		if task.UpstreamTaskID == upstreamID && task.Family == tasks.FamilyMJ {
			out = append(out, task)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UserID != out[j].UserID {
			return out[i].UserID < out[j].UserID
		}
		return out[i].ID < out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *fakeMJStore) UpdateBilling(_ context.Context, id int64, quota *int64, private map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, mjBillingUpdate{ID: id, Quota: quota, Flags: private})
	return nil
}
func (s *fakeMJStore) insertedRows() []*tasks.Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*tasks.Task(nil), s.inserted...)
}

type fakeMJChannels struct {
	mu         sync.Mutex
	channels   map[int64]*tasks.ChannelTarget
	candidates []tasks.MJChannelCandidate
	getErr     error
	candErr    error
}

func (c *fakeMJChannels) GetChannel(_ context.Context, id int64) (*tasks.ChannelTarget, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.getErr != nil {
		return nil, c.getErr
	}
	channel, ok := c.channels[id]
	if !ok {
		return nil, tasks.ErrChannelNotFound
	}
	copied := *channel
	return &copied, nil
}

func (c *fakeMJChannels) MJChannelCandidates(_ context.Context, _, _ string) ([]tasks.MJChannelCandidate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.candErr != nil {
		return nil, c.candErr
	}
	return append([]tasks.MJChannelCandidate(nil), c.candidates...), nil
}

type fakeMJConfig map[string]json.RawMessage

func (c fakeMJConfig) Get(key string) (json.RawMessage, bool) {
	raw, ok := c[key]
	return raw, ok
}

func mjBoolSetting(value bool) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func mjIntSetting(value int64) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

// mjUpstream 记录请求并返回固定响应。
type mjUpstream struct {
	mu       sync.Mutex
	server   *httptest.Server
	paths    []string
	secrets  []string
	bodies   [][]byte
	response string
	status   int
}

func newMJUpstream(t *testing.T, response string) *mjUpstream {
	t.Helper()
	u := &mjUpstream{response: response, status: http.StatusOK}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAllLimited(r.Body)
		u.mu.Lock()
		u.paths = append(u.paths, r.URL.Path)
		u.secrets = append(u.secrets, r.Header.Get(mjAPIHeaderForTest))
		u.bodies = append(u.bodies, body)
		status, response := u.status, u.response
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(u.server.Close)
	return u
}

const mjAPIHeaderForTest = "mj-api-secret"

func readAllLimited(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}

func (u *mjUpstream) last() (string, string, []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.paths) == 0 {
		return "", "", nil
	}
	return u.paths[len(u.paths)-1], u.secrets[len(u.secrets)-1], u.bodies[len(u.bodies)-1]
}

func (u *mjUpstream) calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.paths)
}

// mjTestEnv 是一次用例的装配结果。
type mjTestEnv struct {
	mu        sync.Mutex
	mux       *http.ServeMux
	store     *fakeMJStore
	channels  *fakeMJChannels
	upstream  *mjUpstream
	recorders []*taskBillingRecorder
	// priceSnapshots 记录每次计费会话创建时 RelayContext 上的预置快照
	// （W8/MJ-d：免计费档位必须预置显式零价快照，付费档位必须留空交给
	// 严格按次解析）。
	priceSnapshots []*contract.PriceSnapshot
	sealer         *SecretSealer
	// defaultAuthorization 是 serveMJ 注入的 Bearer 头（默认用户 42）。
	defaultAuthorization string
}

func (e *mjTestEnv) lastRecorder(t *testing.T) *taskBillingRecorder {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.recorders) == 0 {
		t.Fatalf("no billing session was constructed")
	}
	return e.recorders[len(e.recorders)-1]
}

func (e *mjTestEnv) recorderCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.recorders)
}

func (e *mjTestEnv) lastPriceSnapshot(t *testing.T) *contract.PriceSnapshot {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.priceSnapshots) == 0 {
		t.Fatalf("no billing session was constructed")
	}
	return e.priceSnapshots[len(e.priceSnapshots)-1]
}

// newMJTestEnv 构造 handler + mux。channels 由调用方预填（凭据已密封）。
func newMJTestEnv(t *testing.T, store *fakeMJStore, channels *fakeMJChannels,
	config fakeMJConfig, upstream *mjUpstream) *mjTestEnv {
	t.Helper()
	return newMJTestEnvWithBilling(t, store, channels, config, upstream, nil)
}

// newMJTestEnvWithBilling 允许用例注入 recorder 的错误/持有额。
func newMJTestEnvWithBilling(t *testing.T, store *fakeMJStore, channels *fakeMJChannels,
	config fakeMJConfig, upstream *mjUpstream, configure func(*taskBillingRecorder)) *mjTestEnv {
	t.Helper()
	return newMJTestEnvFull(t, store, channels, config, upstream, configure, nil, nil)
}

// newMJTestEnvFull 额外允许注入幂等存储与渠道限制器（W8/MJ-d 用例）。
func newMJTestEnvFull(t *testing.T, store *fakeMJStore, channels *fakeMJChannels,
	config fakeMJConfig, upstream *mjUpstream, configure func(*taskBillingRecorder),
	idempotency MJIdempotencyStore, limiter MJChannelLimiter) *mjTestEnv {
	t.Helper()
	sealer := NewSecretSealer("mj-handler-test-key")
	tokens, key42, _ := newMJTokens()
	// 既有用例关注提交语义而非渠道并发：默认把在飞上限调高，避免并发用例
	// 被 MJ-d 的限制器误判。专门验证限制器的用例显式覆盖该键。
	if config == nil {
		config = fakeMJConfig{}
	}
	if _, ok := config["mj.channel_max_inflight"]; !ok {
		config["mj.channel_max_inflight"] = mjIntSetting(1000)
	}
	env := &mjTestEnv{store: store, channels: channels, upstream: upstream, sealer: sealer}
	handler := NewMJHandler(MJHandlerConfig{
		Store:     store,
		Channels:  channels,
		Sealer:    sealer,
		Tokens:    tokens,
		Submitter: mjTestSubmitter(upstream),
		Billing: func(rc *contract.RelayContext) contract.BillingSession {
			// 快照在会话创建时冻结（此后引擎不再改 rc），记录指针即可。
			env.mu.Lock()
			env.priceSnapshots = append(env.priceSnapshots, rc.PriceSnapshot)
			env.mu.Unlock()
			recorder := newTaskBillingRecorder()
			recorder.hold = 50
			if configure != nil {
				configure(recorder)
			}
			env.mu.Lock()
			env.recorders = append(env.recorders, recorder)
			env.mu.Unlock()
			return recorder
		},
		Config:         config,
		ImageSigner:    mj.NewImageSigner("mj-handler-test-key"),
		RateLimiter:    nil,
		Idempotency:    idempotency,
		ChannelLimiter: limiter,
		Now:            func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) },
		Uint64N:        func(uint64) uint64 { return 0 },
	})
	mux := http.NewServeMux()
	handler.Register(mux, RelayAuth(tokens, fakeTaskAccess{}, nil))
	env.mux = mux
	env.defaultAuthorization = "Bearer " + key42
	return env
}

func sealedMJChannel(t *testing.T, sealer *SecretSealer, id int64, typ, baseURL string,
	models []string, priority int) (*tasks.ChannelTarget, tasks.MJChannelCandidate) {
	t.Helper()
	sealed, err := sealer.Seal([]byte(`{"api_key":"channel-key"}`))
	if err != nil {
		t.Fatalf("seal channel credential: %v", err)
	}
	target := &tasks.ChannelTarget{
		ID: id, BaseURL: baseURL, Credentials: sealed,
		Status: 1, Type: typ,
	}
	candidate := tasks.MJChannelCandidate{
		ID: id, Type: typ, Status: 1, Priority: priority, Weight: 1,
		SupportedModels: models,
	}
	return target, candidate
}

// mjAllModels 是 18 个动作模型名，供渠道候选使用。
func mjAllModels() []string {
	models := make([]string, 0, 18)
	for _, action := range mj.Actions() {
		if model, ok := action.Model(); ok {
			models = append(models, model)
		}
	}
	return models
}

func newMJRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer nkt-mj-user-42")
	return req
}

func serveMJ(t *testing.T, env *mjTestEnv, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req.Header.Set("Authorization", env.defaultAuthorization)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	return rec
}

func decodeMJEnvelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (body=%s)", err, rec.Body.String())
	}
	return body
}

func assertMJEnvelopeShape(t *testing.T, body map[string]any, wantCode int, wantResult any, wantResultPresent bool) {
	t.Helper()
	if len(body) != 2 && len(body) != 3 {
		t.Fatalf("envelope has %d fields, want 2 (error) or 3 (success): %v", len(body), body)
	}
	if got := int(body["code"].(float64)); got != wantCode {
		t.Fatalf("code = %d, want %d (body=%v)", got, wantCode, body)
	}
	if _, ok := body["description"]; !ok {
		t.Fatalf("envelope missing description: %v", body)
	}
	if wantResultPresent {
		got, ok := body["result"]
		if !ok {
			t.Fatalf("success envelope missing result: %v", body)
		}
		if fmt.Sprint(got) != fmt.Sprint(wantResult) {
			t.Fatalf("result = %v, want %v", got, wantResult)
		}
	} else if _, ok := body["result"]; ok {
		t.Fatalf("error envelope must not carry result: %v", body)
	}
}

// mjConfigured 返回 mj.enabled=true 的最小设置集。
func mjConfigured(extra map[string]json.RawMessage) fakeMJConfig {
	config := fakeMJConfig{"mj.enabled": mjBoolSetting(true)}
	for key, raw := range extra {
		config[key] = raw
	}
	return config
}

// ---------- 12 endpoints ----------

func TestMJSubmitAllEndpointsSucceed(t *testing.T) {
	type endpointCase struct {
		name          string
		endpoint      mj.Endpoint
		body          string
		action        mj.Action
		originChannel int64
		originState   tasks.State
		plusOnly      bool
		upload        bool
	}
	cases := []endpointCase{
		{"imagine", mj.EndpointImagine, `{"prompt":"a cat"}`, mj.ActionImagine, 0, "", false, false},
		{"change", mj.EndpointChange, `{"taskId":"mj_origin_1","action":"UPSCALE","index":1}`, mj.ActionUpscale, 10, tasks.StateSuccess, false, false},
		{"simple-change", mj.EndpointSimpleChange, `{"content":"mj_origin_1 u1"}`, mj.ActionUpscale, 10, tasks.StateSuccess, false, false},
		{"action", mj.EndpointAction, `{"customId":"MJ::JOB::upsample::1::uuid"}`, mj.ActionUpscale, 0, "", true, false},
		{"modal", mj.EndpointModal, `{"taskId":"mj_origin_1","prompt":"redo"}`, mj.ActionModal, 11, tasks.StateSuccess, true, false},
		{"shorten", mj.EndpointShorten, `{"prompt":"x"}`, mj.ActionShorten, 0, "", true, false},
		{"describe", mj.EndpointDescribe, `{"base64Array":["aGk="]}`, mj.ActionDescribe, 0, "", false, false},
		{"blend", mj.EndpointBlend, `{"base64Array":["aGk=","aGk="]}`, mj.ActionBlend, 0, "", false, false},
		{"edits", mj.EndpointEdits, `{"prompt":"x","base64Array":["aGk="]}`, mj.ActionEdits, 0, "", false, false},
		{"video", mj.EndpointVideo, `{"taskId":"mj_origin_1"}`, mj.ActionVideo, 10, tasks.StateSuccess, false, false},
		{"upload", mj.EndpointUploadDiscord, `{"base64Array":["aGk="]}`, mj.ActionUpload, 0, "", false, true},
		{"swap", mj.EndpointInsightFace, `{"sourceBase64":"aGk=","targetBase64":"aGk="}`, mj.ActionSwapFace, 0, "", true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeMJStore()
			if tc.originChannel != 0 {
				store.byUserUpstream[mjUserUpstreamKey(42, "mj_origin_1")] = &tasks.Task{
					ID: 900, UpstreamTaskID: "mj_origin_1", Family: tasks.FamilyMJ,
					UserID: 42, ChannelID: tc.originChannel, State: tc.originState,
				}
			}
			upstream := newMJUpstream(t, `{"code":1,"description":"ok","result":"mj_up_1"}`)
			if tc.upload {
				upstream.response = `{"code":1,"description":"ok","result":["https://cdn.example/u1.png"]}`
			}
			sealer := NewSecretSealer("mj-handler-test-key")
			normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
			plus, plusCand := sealedMJChannel(t, sealer, 11, mj.ChannelTypeMidjourneyPlus, upstream.server.URL, mjAllModels(), 5)
			channels := &fakeMJChannels{
				channels:   map[int64]*tasks.ChannelTarget{10: normal, 11: plus},
				candidates: []tasks.MJChannelCandidate{normalCand, plusCand},
			}
			env := newMJTestEnv(t, store, channels, mjConfigured(nil), upstream)

			rec := serveMJ(t, env, newMJRequest(t, http.MethodPost, tc.endpoint.Path(), tc.body))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			body := decodeMJEnvelope(t, rec)
			if tc.upload {
				assertMJEnvelopeShape(t, body, 1, []any{"https://cdn.example/u1.png"}, true)
			} else {
				assertMJEnvelopeShape(t, body, 1, "mj_up_1", true)
			}

			// 上游只调用一次（提交不重试），路径与渠道密钥正确。
			if calls := upstream.calls(); calls != 1 {
				t.Fatalf("upstream calls = %d, want 1", calls)
			}
			path, secret, _ := upstream.last()
			if path != "/mj/"+string(tc.endpoint) {
				t.Fatalf("upstream path = %q, want %q", path, "/mj/"+string(tc.endpoint))
			}
			if secret != "channel-key" {
				t.Fatalf("upstream mj-api-secret = %q, want channel credential", secret)
			}

			rows := store.insertedRows()
			if len(rows) != 1 {
				t.Fatalf("inserted rows = %d, want 1", len(rows))
			}
			row := rows[0]
			if row.Family != tasks.FamilyMJ || row.PluginKey != "" || row.PluginGeneration != 0 {
				t.Fatalf("row family/plugin binding = %+v, want family=mj/no plugin", row)
			}
			if row.Action != string(tc.action) {
				t.Fatalf("row action = %q, want %q", row.Action, tc.action)
			}
			wantChannel := int64(10)
			if tc.plusOnly {
				wantChannel = 11
			}
			if row.ChannelID != wantChannel {
				t.Fatalf("row channel = %d, want %d", row.ChannelID, wantChannel)
			}
			if tc.originChannel != 0 {
				if row.LockedChannelID != tc.originChannel {
					t.Fatalf("locked channel = %d, want %d", row.LockedChannelID, tc.originChannel)
				}
				if len(row.OriginTaskIDs) != 1 || row.OriginTaskIDs[0] != "mj_origin_1" {
					t.Fatalf("origin ids = %v, want [mj_origin_1]", row.OriginTaskIDs)
				}
			} else if row.LockedChannelID != 0 {
				t.Fatalf("unlocked row has locked channel %d", row.LockedChannelID)
			}
			if tc.upload {
				if row.FetchMode != mj.FetchModeInline || row.State != tasks.StateSuccess {
					t.Fatalf("upload row fetch_mode/state = %q/%q, want inline/success", row.FetchMode, row.State)
				}
				if row.UpstreamTaskID != "" {
					t.Fatalf("upload row upstream id = %q, want empty", row.UpstreamTaskID)
				}
				urls, _ := row.Data[mj.DataUploadURLs].([]string)
				if len(urls) != 1 || urls[0] != "https://cdn.example/u1.png" {
					t.Fatalf("upload urls = %v", row.Data[mj.DataUploadURLs])
				}
			} else if row.UpstreamTaskID != "mj_up_1" || row.Quota != 50 {
				t.Fatalf("row upstream/quota = %q/%d, want mj_up_1/50", row.UpstreamTaskID, row.Quota)
			}

			recorder := env.lastRecorder(t)
			calls := recorder.ReserveCalls()
			if countCall(calls, "preconsume") != 1 {
				t.Fatalf("PreConsume calls = %v, want exactly one", calls)
			}
			if countCall(calls, "refund") != 0 {
				t.Fatalf("success path must not refund: %v", calls)
			}
			if tc.upload {
				if countCall(calls, "detach") != 1 || countCall(calls, "terminate_by_id") != 1 {
					t.Fatalf("inline upload billing calls = %v, want detach + terminate_by_id", calls)
				}
				if len(store.updates) != 1 || store.updates[0].Flags["billing_final"] != true {
					t.Fatalf("inline upload billing flags = %+v, want billing_final", store.updates)
				}
			} else if countCall(calls, "detach") != 1 {
				t.Fatalf("normal submit must detach exactly once: %v", calls)
			}
		})
	}
}

func countCall(calls []string, name string) int {
	count := 0
	for _, call := range calls {
		if call == name {
			count++
		}
	}
	return count
}

// ---------- failures / bounds / gating ----------

func TestMJSubmitUpstreamFailureRefundsWithoutRow(t *testing.T) {
	store := newFakeMJStore()
	upstream := newMJUpstream(t, `{"code":24,"description":"sensitive word: secret-url"}`)
	sealer := NewSecretSealer("mj-handler-test-key")
	normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
	channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
		candidates: []tasks.MJChannelCandidate{normalCand}}
	env := newMJTestEnv(t, store, channels, mjConfigured(nil), upstream)

	rec := serveMJ(t, env, newMJRequest(t, http.MethodPost, mj.EndpointImagine.Path(), `{"prompt":"a cat"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	body := decodeMJEnvelope(t, rec)
	assertMJEnvelopeShape(t, body, 24, nil, false)
	if body["description"] != "sensitive_word_detected" {
		t.Fatalf("description = %v, want fixed %q (upstream text must not leak)", body["description"], "sensitive_word_detected")
	}
	if rows := store.insertedRows(); len(rows) != 0 {
		t.Fatalf("upstream failure must not insert a task row: %+v", rows)
	}
	calls := env.lastRecorder(t).ReserveCalls()
	if countCall(calls, "preconsume") != 1 || countCall(calls, "refund") != 1 {
		t.Fatalf("billing calls = %v, want one preconsume + one refund", calls)
	}
	if countCall(calls, "detach") != 0 {
		t.Fatalf("failed submit must not detach: %v", calls)
	}
}

func TestMJSubmitDisabledGateIsFailClosed(t *testing.T) {
	store := newFakeMJStore()
	upstream := newMJUpstream(t, `{"code":1,"result":"mj_up_1"}`)
	sealer := NewSecretSealer("mj-handler-test-key")
	normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
	channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
		candidates: []tasks.MJChannelCandidate{normalCand}}

	for name, config := range map[string]fakeMJConfig{
		"missing":  {},
		"explicit": {"mj.enabled": mjBoolSetting(false)},
	} {
		t.Run(name, func(t *testing.T) {
			env := newMJTestEnv(t, store, channels, config, upstream)
			rec := serveMJ(t, env, newMJRequest(t, http.MethodPost, mj.EndpointImagine.Path(), `{"prompt":"a cat"}`))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
			}
			body := decodeMJEnvelope(t, rec)
			assertMJEnvelopeShape(t, body, mj.CodeRequestError, nil, false)
			if body["description"] != "mj_not_enabled" {
				t.Fatalf("description = %v, want %q", body["description"], "mj_not_enabled")
			}
			if upstream.calls() != 0 || len(store.insertedRows()) != 0 || len(env.recorders) != 0 {
				t.Fatalf("disabled gate must be zero upstream/row/billing side effects")
			}
		})
	}
}

func TestMJSubmitBoundsAndUnknownEndpoint(t *testing.T) {
	store := newFakeMJStore()
	upstream := newMJUpstream(t, `{"code":1,"result":"mj_up_1"}`)
	sealer := NewSecretSealer("mj-handler-test-key")
	normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
	channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
		candidates: []tasks.MJChannelCandidate{normalCand}}
	env := newMJTestEnv(t, store, channels, mjConfigured(nil), upstream)

	hugePrompt, _ := json.Marshal(map[string]string{"prompt": strings.Repeat("a", mj.MaxPromptBytes+1)})
	nineImages, _ := json.Marshal(map[string]any{"base64Array": []string{
		"a", "b", "c", "d", "e", "f", "g", "h", "i",
	}})
	cases := []struct {
		name string
		path string
		body string
		desc string
	}{
		{"prompt too long", mj.EndpointImagine.Path(), string(hugePrompt), "prompt_too_long"},
		{"too many images", mj.EndpointDescribe.Path(), string(nineImages), "base64_array_too_large"},
		{"invalid task id", mj.EndpointChange.Path(), `{"taskId":"bad/id","action":"UPSCALE","index":1}`, "invalid_task_id"},
		{"missing prompt", mj.EndpointImagine.Path(), `{}`, "prompt_is_required"},
		{"unknown action", mj.EndpointChange.Path(), `{"taskId":"mj_1","action":"NOPE","index":1}`, "unknown_action"},
		{"unknown endpoint", "/mj/submit/nope", `{"prompt":"x"}`, "unknown_endpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveMJ(t, env, newMJRequest(t, http.MethodPost, tc.path, tc.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			body := decodeMJEnvelope(t, rec)
			assertMJEnvelopeShape(t, body, mj.CodeRequestError, nil, false)
			if body["description"] != tc.desc {
				t.Fatalf("description = %v, want %q", body["description"], tc.desc)
			}
		})
	}
	if upstream.calls() != 0 || len(store.insertedRows()) != 0 {
		t.Fatalf("invalid submissions must not reach upstream or persist rows")
	}
}

func TestMJSubmitLockRejectionsHappenBeforeBilling(t *testing.T) {
	cases := []struct {
		name       string
		origin     *tasks.Task
		channel    *tasks.ChannelTarget
		config     fakeMJConfig
		wantStatus int
		wantDesc   string
	}{
		{
			name:       "origin missing",
			origin:     nil,
			channel:    nil,
			config:     mjConfigured(nil),
			wantStatus: http.StatusBadRequest,
			wantDesc:   "get_channel_info_failed",
		},
		{
			name:       "origin channel disabled",
			origin:     &tasks.Task{ID: 1, UpstreamTaskID: "mj_origin_1", Family: tasks.FamilyMJ, UserID: 42, ChannelID: 10, State: tasks.StateSuccess},
			channel:    &tasks.ChannelTarget{ID: 10, BaseURL: "http://invalid", Status: 2, Type: mj.ChannelTypeMidjourney},
			config:     mjConfigured(nil),
			wantStatus: http.StatusBadRequest,
			wantDesc:   "channel_disabled",
		},
		{
			name:       "origin channel type drifted",
			origin:     &tasks.Task{ID: 1, UpstreamTaskID: "mj_origin_1", Family: tasks.FamilyMJ, UserID: 42, ChannelID: 10, State: tasks.StateSuccess},
			channel:    &tasks.ChannelTarget{ID: 10, BaseURL: "http://invalid", Status: 1, Type: "openai"},
			config:     mjConfigured(nil),
			wantStatus: http.StatusBadRequest,
			wantDesc:   "channel_disabled",
		},
		{
			name:       "origin not success with switch on",
			origin:     &tasks.Task{ID: 1, UpstreamTaskID: "mj_origin_1", Family: tasks.FamilyMJ, UserID: 42, ChannelID: 10, State: tasks.StateInProgress},
			channel:    &tasks.ChannelTarget{ID: 10, BaseURL: "http://invalid", Status: 1, Type: mj.ChannelTypeMidjourney},
			config:     mjConfigured(map[string]json.RawMessage{"mj.action_check_success_enabled": mjBoolSetting(true)}),
			wantStatus: http.StatusBadRequest,
			wantDesc:   "action_task_not_success",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeMJStore()
			if tc.origin != nil {
				store.byUserUpstream[mjUserUpstreamKey(42, "mj_origin_1")] = tc.origin
			}
			upstream := newMJUpstream(t, `{"code":1,"result":"mj_up_1"}`)
			channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{}, candidates: nil}
			if tc.channel != nil {
				channels.channels[10] = tc.channel
			}
			env := newMJTestEnv(t, store, channels, tc.config, upstream)
			rec := serveMJ(t, env, newMJRequest(t, http.MethodPost, mj.EndpointChange.Path(),
				`{"taskId":"mj_origin_1","action":"UPSCALE","index":1}`))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			body := decodeMJEnvelope(t, rec)
			assertMJEnvelopeShape(t, body, mj.CodeRequestError, nil, false)
			if body["description"] != tc.wantDesc {
				t.Fatalf("description = %v, want %q", body["description"], tc.wantDesc)
			}
			if len(env.recorders) != 0 || upstream.calls() != 0 || len(store.insertedRows()) != 0 {
				t.Fatalf("lock rejection must happen before billing/upstream/persist")
			}
		})
	}
}

func TestMJSubmitRejectsMalformedUpstreamMjID(t *testing.T) {
	store := newFakeMJStore()
	upstream := newMJUpstream(t, `{"code":1,"result":"../not-a-mjid"}`)
	sealer := NewSecretSealer("mj-handler-test-key")
	normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
	channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
		candidates: []tasks.MJChannelCandidate{normalCand}}
	env := newMJTestEnv(t, store, channels, mjConfigured(nil), upstream)

	rec := serveMJ(t, env, newMJRequest(t, http.MethodPost, mj.EndpointImagine.Path(), `{"prompt":"a cat"}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	if rows := store.insertedRows(); len(rows) != 0 {
		t.Fatalf("malformed upstream id must not persist a row: %+v", rows)
	}
	calls := env.lastRecorder(t).ReserveCalls()
	if countCall(calls, "refund") != 1 {
		t.Fatalf("malformed upstream id must refund the hold: %v", calls)
	}
}

// ---------- code 21 reuse ----------

func TestMJCode21SameUserReusesAndCrossUserIsIsolated(t *testing.T) {
	t.Run("same user reuses", func(t *testing.T) {
		store := newFakeMJStore()
		existing := &tasks.Task{
			ID: 7, PublicTaskID: "task_existing", UpstreamTaskID: "mj_existing",
			Family: tasks.FamilyMJ, Action: string(mj.ActionImagine), UserID: 42, ChannelID: 10,
		}
		store.byUserUpstream[mjUserUpstreamKey(42, "mj_existing")] = existing
		upstream := newMJUpstream(t, `{"code":21,"description":"already exists","result":"mj_existing"}`)
		sealer := NewSecretSealer("mj-handler-test-key")
		normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
		channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
			candidates: []tasks.MJChannelCandidate{normalCand}}
		env := newMJTestEnv(t, store, channels, mjConfigured(nil), upstream)

		rec := serveMJ(t, env, newMJRequest(t, http.MethodPost, mj.EndpointImagine.Path(), `{"prompt":"a cat"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
		}
		body := decodeMJEnvelope(t, rec)
		assertMJEnvelopeShape(t, body, 1, "mj_existing", true)
		if rows := store.insertedRows(); len(rows) != 0 {
			t.Fatalf("same-user code 21 must reuse without a new row: %+v", rows)
		}
		calls := env.lastRecorder(t).ReserveCalls()
		if countCall(calls, "preconsume") != 1 || countCall(calls, "refund") != 1 {
			t.Fatalf("reuse must hold once then refund once: %v", calls)
		}
		if countCall(calls, "detach") != 0 {
			t.Fatalf("reuse must not detach a new task: %v", calls)
		}
	})

	t.Run("cross user does not reuse", func(t *testing.T) {
		store := newFakeMJStore()
		// 同一 MjId 已属于用户 99；用户 42 的 code 21 不得复用这一行。
		store.byUserUpstream[mjUserUpstreamKey(99, "mj_shared")] = &tasks.Task{
			ID: 8, PublicTaskID: "task_other", UpstreamTaskID: "mj_shared",
			Family: tasks.FamilyMJ, UserID: 99, ChannelID: 10,
		}
		upstream := newMJUpstream(t, `{"code":21,"description":"already exists","result":"mj_shared"}`)
		sealer := NewSecretSealer("mj-handler-test-key")
		normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
		channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
			candidates: []tasks.MJChannelCandidate{normalCand}}
		env := newMJTestEnv(t, store, channels, mjConfigured(nil), upstream)

		rec := serveMJ(t, env, newMJRequest(t, http.MethodPost, mj.EndpointImagine.Path(), `{"prompt":"a cat"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
		}
		rows := store.insertedRows()
		if len(rows) != 1 || rows[0].UserID != 42 || rows[0].UpstreamTaskID != "mj_shared" {
			t.Fatalf("cross-user code 21 must persist a row owned by the caller: %+v", rows)
		}
		calls := env.lastRecorder(t).ReserveCalls()
		if countCall(calls, "refund") != 0 || countCall(calls, "detach") != 1 {
			t.Fatalf("cross-user code 21 is a normal accepted task: %v", calls)
		}
	})
}

// ---------- inline upload settlement ----------

func TestMJUploadSettlementFailureKeepsHoldAndMarksPending(t *testing.T) {
	store := newFakeMJStore()
	upstream := newMJUpstream(t, `{"code":1,"result":["https://cdn.example/u1.png"]}`)
	sealer := NewSecretSealer("mj-handler-test-key")
	normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
	channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
		candidates: []tasks.MJChannelCandidate{normalCand}}
	env := newMJTestEnv(t, store, channels, mjConfigured(nil), upstream)

	// 注入一个「结算失败」的会话。
	tokens, key42, _ := newMJTokens()
	handler := NewMJHandler(MJHandlerConfig{
		Store: store, Channels: channels, Sealer: sealer, Tokens: tokens,
		Billing: func(*contract.RelayContext) contract.BillingSession {
			recorder := newTaskBillingRecorder()
			recorder.hold = 50
			recorder.terminateByIDErr = errors.New("boom")
			return recorder
		},
		Config:      mjConfigured(nil),
		Submitter:   mjTestSubmitter(upstream),
		ImageSigner: mj.NewImageSigner("mj-handler-test-key"),
		Uint64N:     func(uint64) uint64 { return 0 },
	})
	mux := http.NewServeMux()
	handler.Register(mux, RelayAuth(tokens, fakeTaskAccess{}, nil))
	env.mux = mux
	env.defaultAuthorization = "Bearer " + key42

	rec := serveMJ(t, env, newMJRequest(t, http.MethodPost, mj.EndpointUploadDiscord.Path(), `{"base64Array":["aGk="]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	rows := store.insertedRows()
	if len(rows) != 1 || rows[0].FetchMode != mj.FetchModeInline {
		t.Fatalf("inline upload row missing: %+v", rows)
	}
	if len(store.updates) != 1 || store.updates[0].Flags["settlement_pending"] != true {
		t.Fatalf("settlement failure must mark settlement_pending: %+v", store.updates)
	}
}

// ---------- auth carrier / envelope ----------

func TestMJAuthCarrierAndEnvelope(t *testing.T) {
	store := newFakeMJStore()
	upstream := newMJUpstream(t, `{"code":1,"result":"mj_up_1"}`)
	sealer := NewSecretSealer("mj-handler-test-key")
	normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
	channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
		candidates: []tasks.MJChannelCandidate{normalCand}}
	env := newMJTestEnv(t, store, channels, mjConfigured(nil), upstream)

	t.Run("no credentials", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, mj.EndpointImagine.Path(), strings.NewReader(`{"prompt":"x"}`))
		rec := httptest.NewRecorder()
		env.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		body := decodeMJEnvelope(t, rec)
		assertMJEnvelopeShape(t, body, mj.CodeUnknownError, nil, false)
		if body["description"] != "invalid_token" {
			t.Fatalf("description = %v, want %q", body["description"], "invalid_token")
		}
	})

	t.Run("mj-api-secret with sentinel authorization", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, mj.EndpointImagine.Path(), strings.NewReader(`{"prompt":"x"}`))
		req.Header.Set("Authorization", "Bearer midjourney-proxy")
		req.Header.Set(mjAPISecretHeader, "nkt-mj-user-42")
		rec := httptest.NewRecorder()
		env.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
		}
		// 客户端令牌绝不能透传到上游。
		_, secret, _ := upstream.last()
		if secret != "channel-key" {
			t.Fatalf("upstream secret = %q, want channel credential", secret)
		}
	})

	t.Run("mj-api-secret without authorization", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, mj.EndpointImagine.Path(), strings.NewReader(`{"prompt":"x"}`))
		req.Header.Set(mjAPISecretHeader, "nkt-mj-user-42")
		rec := httptest.NewRecorder()
		env.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
		}
	})
}

func TestMJRateLimitRejectsBeforeChannelAndBilling(t *testing.T) {
	store := newFakeMJStore()
	upstream := newMJUpstream(t, `{"code":1,"result":"mj_up_1"}`)
	sealer := NewSecretSealer("mj-handler-test-key")
	normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
	channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
		candidates: []tasks.MJChannelCandidate{normalCand}}

	tokens, key42, _ := newMJTokens()
	handler := NewMJHandler(MJHandlerConfig{
		Store: store, Channels: channels, Sealer: sealer, Tokens: tokens,
		Submitter: mjTestSubmitter(upstream),
		Billing: func(*contract.RelayContext) contract.BillingSession {
			t.Fatalf("rate-limited request must not construct a billing session")
			return nil
		},
		Config:      mjConfigured(nil),
		ImageSigner: mj.NewImageSigner("mj-handler-test-key"),
		RateLimiter: rejectingMJRatelimiter{},
		Uint64N:     func(uint64) uint64 { return 0 },
	})
	mux := http.NewServeMux()
	handler.Register(mux, RelayAuth(tokens, fakeTaskAccess{}, nil))

	req := httptest.NewRequest(http.MethodPost, mj.EndpointImagine.Path(), strings.NewReader(`{"prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer "+key42)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body=%s)", rec.Code, rec.Body.String())
	}
	body := decodeMJEnvelope(t, rec)
	assertMJEnvelopeShape(t, body, mj.CodeUnknownError, nil, false)
	if body["description"] != "rate_limited" {
		t.Fatalf("description = %v, want %q", body["description"], "rate_limited")
	}
	if upstream.calls() != 0 || len(store.insertedRows()) != 0 {
		t.Fatalf("rate-limited request must not reach upstream or persist")
	}
}

// rejectingMJRatelimiter 模拟 B11/B10/B12 中的任意一档超限。
type rejectingMJRatelimiter struct{}

func (rejectingMJRatelimiter) Allow(context.Context, ratelimit.Bucket, string, int) (ratelimit.Decision, error) {
	return ratelimit.Decision{Allowed: false, RetryAfter: time.Second}, nil
}

// ---------- carrier path detection ----------

func TestMJPathDetection(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/mj/submit/imagine", true},
		{"/mj", true},
		{"/foo/mj/submit/imagine", true},
		{"/v1/mj/submit/imagine", false},
		{"/api/mj/submit/imagine", false},
		{"/pg/mj/submit/imagine", false},
		{"/healthz/mj", false},
		{"/readyz/mj", false},
		{"/v1/models", false},
	}
	for _, tc := range cases {
		if got := isMJPath(tc.path); got != tc.want {
			t.Errorf("isMJPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// ---------- C1: auth wrapper state is per request ----------

// TestMJEnvelopeAuthPerRequestState 复现 C1：同一路由先成功一次、再无凭据，
// 失败请求仍必须是 401 MJ 信封（不能沿用上一次成功留下的 passed 标志）。
func TestMJEnvelopeAuthPerRequestState(t *testing.T) {
	store := newFakeMJStore()
	upstream := newMJUpstream(t, `{"code":1,"result":"mj_up_1"}`)
	sealer := NewSecretSealer("mj-handler-test-key")
	normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
	channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
		candidates: []tasks.MJChannelCandidate{normalCand}}
	env := newMJTestEnv(t, store, channels, mjConfigured(nil), upstream)

	// 1) 成功请求：让旧实现的共享 passed 置位。
	okRec := serveMJ(t, env, newMJRequest(t, http.MethodPost, mj.EndpointImagine.Path(), `{"prompt":"x"}`))
	if okRec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", okRec.Code)
	}

	// 2) 同路由无凭据：必须是 401 + MJ 信封（不是 OpenAI 信封/200）。
	req := httptest.NewRequest(http.MethodPost, mj.EndpointImagine.Path(), strings.NewReader(`{"prompt":"x"}`))
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("second request status = %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	body := decodeMJEnvelope(t, rec)
	assertMJEnvelopeShape(t, body, mj.CodeUnknownError, nil, false)
	if body["description"] != "invalid_token" {
		t.Fatalf("description = %v, want invalid_token", body["description"])
	}
}

// TestMJEnvelopeAuthConcurrentRequests 让成功/失败请求并发穿过同一路由：
// 请求级状态必须让每个请求得到自己的结果（旧共享标志会串味并有数据竞争）。
func TestMJEnvelopeAuthConcurrentRequests(t *testing.T) {
	store := newFakeMJStore()
	upstream := newMJUpstream(t, `{"code":1,"result":"mj_up_1"}`)
	sealer := NewSecretSealer("mj-handler-test-key")
	normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
	channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
		candidates: []tasks.MJChannelCandidate{normalCand}}
	env := newMJTestEnv(t, store, channels, mjConfigured(nil), upstream)

	const requests = 80
	var wg sync.WaitGroup
	errs := make(chan string, requests)
	for i := 0; i < requests; i++ {
		authorized := i%2 == 0
		wg.Add(1)
		go func(authorized bool) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, mj.EndpointImagine.Path(), strings.NewReader(`{"prompt":"x"}`))
			if authorized {
				req.Header.Set("Authorization", env.defaultAuthorization)
			}
			rec := httptest.NewRecorder()
			env.mux.ServeHTTP(rec, req)
			body := decodeMJEnvelope(t, rec)
			if authorized {
				if rec.Code != http.StatusOK || int(body["code"].(float64)) != 1 {
					errs <- fmt.Sprintf("authorized request got %d/%v", rec.Code, body)
				}
				return
			}
			if rec.Code != http.StatusUnauthorized || int(body["code"].(float64)) != mj.CodeUnknownError {
				errs <- fmt.Sprintf("unauthorized request got %d/%v", rec.Code, body)
			}
		}(authorized)
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
}

// ---------- I1: terminal submit bodies are settled at submit time ----------

func TestMJSubmitTerminalBodiesSettleAtSubmit(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		wantState  tasks.State
		wantCall   string
		wantQuota  *int64
		wantUpdate string
	}{
		{"success", "SUCCESS", tasks.StateSuccess, "terminate_by_id", nil, "billing_final"},
		{"failure", "FAILURE", tasks.StateFailure, "refund_by_id", int64Ptr(0), "billing_final"},
		{"cancel", "CANCEL", tasks.StateFailure, "refund_by_id", int64Ptr(0), "billing_final"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeMJStore()
			upstream := newMJUpstream(t, fmt.Sprintf(
				`{"code":1,"description":"ok","result":"mj_up_1","properties":{"status":%q,"progress":"100%%"}}`, tc.status))
			sealer := NewSecretSealer("mj-handler-test-key")
			normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
			channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
				candidates: []tasks.MJChannelCandidate{normalCand}}
			env := newMJTestEnv(t, store, channels, mjConfigured(nil), upstream)

			rec := serveMJ(t, env, newMJRequest(t, http.MethodPost, mj.EndpointImagine.Path(), `{"prompt":"x"}`))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
			}
			rows := store.insertedRows()
			if len(rows) != 1 || rows[0].State != tc.wantState || rows[0].RawState != tc.status {
				t.Fatalf("row state/raw = %+v, want %s/%s", rows, tc.wantState, tc.status)
			}
			// 终态行不能再进入待轮询集合（证明「无人结算」的路径确实被关闭）。
			if rows[0].State.Terminal() != true {
				t.Fatalf("expected terminal row")
			}
			calls := env.lastRecorder(t).ReserveCalls()
			if countCall(calls, "detach") != 1 || countCall(calls, tc.wantCall) != 1 {
				t.Fatalf("billing calls = %v, want detach + %s", calls, tc.wantCall)
			}
			if countCall(calls, "refund") != 0 {
				t.Fatalf("terminal settlement must not use the session Refund deferred path: %v", calls)
			}
			if len(store.updates) != 1 {
				t.Fatalf("billing updates = %+v, want exactly one", store.updates)
			}
			update := store.updates[0]
			if update.Flags[tc.wantUpdate] != true {
				t.Fatalf("billing flags = %+v, want %s", update.Flags, tc.wantUpdate)
			}
			if tc.wantQuota == nil {
				if update.Quota != nil {
					t.Fatalf("billing quota = %v, want unchanged (nil)", *update.Quota)
				}
			} else if update.Quota == nil || *update.Quota != *tc.wantQuota {
				t.Fatalf("billing quota = %v, want %d", update.Quota, *tc.wantQuota)
			}
		})
	}
}

func TestMJSubmitTerminalSettlementFailureKeepsHold(t *testing.T) {
	t.Run("success settlement failure", func(t *testing.T) {
		store := newFakeMJStore()
		upstream := newMJUpstream(t, `{"code":1,"result":"mj_up_1","properties":{"status":"SUCCESS"}}`)
		sealer := NewSecretSealer("mj-handler-test-key")
		normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
		channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
			candidates: []tasks.MJChannelCandidate{normalCand}}
		env := newMJTestEnvWithBilling(t, store, channels, mjConfigured(nil), upstream,
			func(r *taskBillingRecorder) { r.terminateByIDErr = errors.New("boom") })

		rec := serveMJ(t, env, newMJRequest(t, http.MethodPost, mj.EndpointImagine.Path(), `{"prompt":"x"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if len(store.updates) != 1 || store.updates[0].Flags["settlement_pending"] != true {
			t.Fatalf("updates = %+v, want settlement_pending", store.updates)
		}
		if calls := env.lastRecorder(t).ReserveCalls(); countCall(calls, "refund") != 0 {
			t.Fatalf("must keep hold, not refund: %v", calls)
		}
	})

	t.Run("failure refund failure", func(t *testing.T) {
		store := newFakeMJStore()
		upstream := newMJUpstream(t, `{"code":1,"result":"mj_up_1","properties":{"status":"FAILURE"}}`)
		sealer := NewSecretSealer("mj-handler-test-key")
		normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
		channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
			candidates: []tasks.MJChannelCandidate{normalCand}}
		env := newMJTestEnvWithBilling(t, store, channels, mjConfigured(nil), upstream,
			func(r *taskBillingRecorder) { r.refundByIDErr = errors.New("boom") })

		rec := serveMJ(t, env, newMJRequest(t, http.MethodPost, mj.EndpointImagine.Path(), `{"prompt":"x"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if len(store.updates) != 1 || store.updates[0].Flags["refund_pending"] != true {
			t.Fatalf("updates = %+v, want refund_pending", store.updates)
		}
		if calls := env.lastRecorder(t).ReserveCalls(); countCall(calls, "refund") != 0 {
			t.Fatalf("committed row must not fall through to the deferred refund: %v", calls)
		}
	})
}

// ---------- M1/M3/M4/M5 ----------

type mjLimiterCall struct {
	Bucket ratelimit.Bucket
	Key    string
	Cost   int
}

type recordingMJRatelimiter struct {
	mu    sync.Mutex
	calls []mjLimiterCall
}

func (r *recordingMJRatelimiter) Allow(_ context.Context, bucket ratelimit.Bucket, key string, cost int) (ratelimit.Decision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, mjLimiterCall{Bucket: bucket, Key: key, Cost: cost})
	return ratelimit.Decision{Allowed: true}, nil
}

// TestMJSubmitUsesStandardRateLimitBuckets 断言 MJ 提交真的落在 B11/B10/B12
// 的标准键与阈值上（M1），而不只是「接缝被调用过」。
func TestMJSubmitUsesStandardRateLimitBuckets(t *testing.T) {
	store := newFakeMJStore()
	upstream := newMJUpstream(t, `{"code":1,"result":"mj_up_1"}`)
	sealer := NewSecretSealer("mj-handler-test-key")
	normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
	channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
		candidates: []tasks.MJChannelCandidate{normalCand}}

	tokens, key42, _ := newMJTokens()
	config := mjConfigured(nil)
	limiter := &recordingMJRatelimiter{}
	handler := NewMJHandler(MJHandlerConfig{
		Store: store, Channels: channels, Sealer: sealer, Tokens: tokens,
		Billing: func(*contract.RelayContext) contract.BillingSession { return newTaskBillingRecorder() },
		Config:  config, RateLimiter: limiter,
		Submitter:   mjTestSubmitter(upstream),
		ImageSigner: mj.NewImageSigner("mj-handler-test-key"),
		Uint64N:     func(uint64) uint64 { return 0 },
	})
	mux := http.NewServeMux()
	handler.Register(mux, RelayAuth(tokens, fakeTaskAccess{}, nil))

	req := httptest.NewRequest(http.MethodPost, mj.EndpointImagine.Path(), strings.NewReader(`{"prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer "+key42)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	settings := ratelimit.Resolve(config, slog.Default())
	limiter.mu.Lock()
	calls := append([]mjLimiterCall(nil), limiter.calls...)
	limiter.mu.Unlock()
	want := []struct {
		name  string
		key   string
		limit int
	}{
		{ratelimit.BucketToken, ratelimit.TokenKey(1), settings.TokenLimit()},
		{ratelimit.BucketUserModel, ratelimit.UserKey(42), settings.UserModelLimit("default")},
		{ratelimit.BucketUserGlobal, ratelimit.UserGlobalKey(42), settings.UserGlobalLimit()},
	}
	if len(calls) != len(want) {
		t.Fatalf("limiter calls = %+v, want %d buckets", calls, len(want))
	}
	for i, expected := range want {
		got := calls[i]
		if got.Bucket.Name != expected.name || got.Key != expected.key || got.Bucket.Limit != expected.limit || got.Cost != 1 {
			t.Fatalf("call[%d] = %+v, want name=%s key=%s limit=%d cost=1",
				i, got, expected.name, expected.key, expected.limit)
		}
		if got.Bucket.Window != relayRateLimitWindow || got.Bucket.FailMode != ratelimit.FailClosed {
			t.Fatalf("call[%d] window/failmode = %v/%v", i, got.Bucket.Window, got.Bucket.FailMode)
		}
	}
}

func TestMJSubmitBillingErrorMappings(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantDesc   string
	}{
		{"quota not enough", billing.ErrInsufficientQuota, http.StatusPaymentRequired, "quota_not_enough"},
		{"pricing unavailable", errors.New("no price"), http.StatusBadRequest, "task_pricing_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeMJStore()
			upstream := newMJUpstream(t, `{"code":1,"result":"mj_up_1"}`)
			sealer := NewSecretSealer("mj-handler-test-key")
			normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
			channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
				candidates: []tasks.MJChannelCandidate{normalCand}}
			env := newMJTestEnvWithBilling(t, store, channels, mjConfigured(nil), upstream,
				func(r *taskBillingRecorder) { r.preConsumeErr = tc.err })

			rec := serveMJ(t, env, newMJRequest(t, http.MethodPost, mj.EndpointImagine.Path(), `{"prompt":"x"}`))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			body := decodeMJEnvelope(t, rec)
			assertMJEnvelopeShape(t, body, mj.CodeRequestError, nil, false)
			if body["description"] != tc.wantDesc {
				t.Fatalf("description = %v, want %s", body["description"], tc.wantDesc)
			}
			if upstream.calls() != 0 || len(store.insertedRows()) != 0 {
				t.Fatalf("pre-consume failure must not reach upstream or persist")
			}
		})
	}
}

func TestMJPlusOnlyWithoutPlusChannelIsForbidden(t *testing.T) {
	store := newFakeMJStore()
	upstream := newMJUpstream(t, `{"code":1,"result":"mj_up_1"}`)
	sealer := NewSecretSealer("mj-handler-test-key")
	normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
	channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
		candidates: []tasks.MJChannelCandidate{normalCand}}
	env := newMJTestEnv(t, store, channels, mjConfigured(nil), upstream)

	rec := serveMJ(t, env, newMJRequest(t, http.MethodPost, mj.EndpointAction.Path(),
		`{"customId":"MJ::JOB::upsample::1::uuid"}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	body := decodeMJEnvelope(t, rec)
	assertMJEnvelopeShape(t, body, mj.CodeRequestError, nil, false)
	if body["description"] != "plus_channel_required" {
		t.Fatalf("description = %v, want plus_channel_required", body["description"])
	}
	if upstream.calls() != 0 || len(store.insertedRows()) != 0 || env.recorderCount() != 0 {
		t.Fatalf("plus capability rejection must be zero-cost")
	}
}

// TestMJHandlerRejectsPrivateChannelWithoutOptIn 覆盖 R-01 在 MJ 提交链路的
// 默认拒绝：生产默认客户端出站前拦截，请求不触达上游。
//
// opt-in 只放行 RFC1918/ULA；loopback（httptest 的 127.0.0.1）即使开启也
// 必须拒绝，这里一并锁定。私网放行本身由 egress 单测覆盖。
func TestMJHandlerRejectsPrivateChannelWithoutOptIn(t *testing.T) {
	cases := []struct {
		name     string
		optIn    bool
		wantCode int
		wantHits int
	}{
		{"默认拒绝 loopback", false, http.StatusBadGateway, 0},
		{"loopback 即使 opt-in 也拒绝", true, http.StatusBadGateway, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeMJStore()
			upstream := newMJUpstream(t, `{"code":1,"result":"mj_up_1"}`)
			sealer := NewSecretSealer("mj-handler-test-key")
			normal, cand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney,
				upstream.server.URL, mjAllModels(), 10)
			normal.AllowPrivateNetwork = tc.optIn
			channels := &fakeMJChannels{
				channels:   map[int64]*tasks.ChannelTarget{10: normal},
				candidates: []tasks.MJChannelCandidate{cand},
			}
			tokens, key42, _ := newMJTokens()
			env := &mjTestEnv{store: store, channels: channels, upstream: upstream, sealer: sealer}
			handler := NewMJHandler(MJHandlerConfig{
				Store: store, Channels: channels, Sealer: sealer, Tokens: tokens,
				Billing: func(*contract.RelayContext) contract.BillingSession { return newTaskBillingRecorder() },
				Config:  mjConfigured(nil),
				// 刻意不注入 Submitter：走生产默认的 R-01 加固客户端。
				ImageSigner: mj.NewImageSigner("mj-handler-test-key"),
				Uint64N:     func(uint64) uint64 { return 0 },
			})
			mux := http.NewServeMux()
			handler.Register(mux, RelayAuth(tokens, fakeTaskAccess{}, nil))
			env.mux = mux
			env.defaultAuthorization = "Bearer " + key42

			rec := serveMJ(t, env, newMJRequest(t, http.MethodPost, "/mj/submit/imagine", `{"prompt":"cat"}`))
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d，期望 %d（body=%s）", rec.Code, tc.wantCode, rec.Body.String())
			}
			if got := upstream.calls(); got != tc.wantHits {
				t.Fatalf("上游命中 %d 次，期望 %d", got, tc.wantHits)
			}
		})
	}
}

// mjTestSubmitter 返回指向假上游的提交客户端。
//
// R-01 起提交客户端默认经 egress 加固（拒绝 loopback/私网），httptest 的
// 127.0.0.1 目标必须显式注入 srv.Client()；出站策略本身由 egress 单测与
// TestMJHandlerRejectsPrivateChannelWithoutOptIn 覆盖。
func mjTestSubmitter(upstream *mjUpstream) *mj.SubmitClient {
	if upstream == nil {
		// 用例自建 httptest 上游（如并发阻塞），直接用允许 loopback 的测试运输层。
		return mj.NewSubmitClient(mjTestHTTPClient())
	}
	client := *upstream.server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return mj.NewSubmitClient(&client)
}

// mjTestHTTPClient 返回允许 loopback 的 HTTP 客户端，仅供测试环境使用。
//
// 生产默认客户端（NewSubmitClient(nil)）经 R-01 加固，会拒绝 httptest 的
// 127.0.0.1；需要真实出站的用例必须显式注入本客户端或 srv.Client()。
// 拒绝语义由 TestMJHandlerRejectsPrivateChannelWithoutOptIn 覆盖。
func mjTestHTTPClient() *http.Client {
	guard := egress.NewGuard(egress.Policy{Mode: egress.ModeOff})
	return &http.Client{Transport: guard.HardenTransport(nil)}
}

// cancellingSubmitter 在返回成功前取消请求上下文，模拟「上游已受理、客户端
// 断开」。落库与 Detach 必须仍完成（R-16 同口径）。
type cancellingSubmitter struct {
	cancel context.CancelFunc
	result *mj.SubmitResponse
}

func (s *cancellingSubmitter) Submit(_ context.Context, _ mj.SubmitConfig, _ mj.Endpoint,
	_ *mj.NormalizedSubmit) (*mj.SubmitResponse, *mj.Error) {
	s.cancel()
	return s.result, nil
}

func TestMJSubmitPersistsAfterClientDisconnect(t *testing.T) {
	store := newFakeMJStore()
	upstream := newMJUpstream(t, `{"code":1,"result":"mj_up_1"}`) // 只用于渠道 URL
	sealer := NewSecretSealer("mj-handler-test-key")
	normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
	channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
		candidates: []tasks.MJChannelCandidate{normalCand}}

	ctx, cancel := context.WithCancel(context.Background())
	tokens, key42, _ := newMJTokens()
	recorder := newTaskBillingRecorder()
	recorder.hold = 50
	handler := NewMJHandler(MJHandlerConfig{
		Store: store, Channels: channels, Sealer: sealer, Tokens: tokens,
		Billing:     func(*contract.RelayContext) contract.BillingSession { return recorder },
		Config:      mjConfigured(nil),
		ImageSigner: mj.NewImageSigner("mj-handler-test-key"),
		Submitter:   &cancellingSubmitter{cancel: cancel, result: &mj.SubmitResponse{Code: mj.CodeSuccess, Result: "mj_up_1"}},
		Uint64N:     func(uint64) uint64 { return 0 },
	})
	mux := http.NewServeMux()
	handler.Register(mux, RelayAuth(tokens, fakeTaskAccess{}, nil))

	req := httptest.NewRequest(http.MethodPost, mj.EndpointImagine.Path(), strings.NewReader(`{"prompt":"x"}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+key42)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if rows := store.insertedRows(); len(rows) != 1 || rows[0].UpstreamTaskID != "mj_up_1" {
		t.Fatalf("disconnect must still persist the accepted task: %+v", rows)
	}
	if calls := recorder.ReserveCalls(); countCall(calls, "detach") != 1 {
		t.Fatalf("disconnect must still detach: %v", calls)
	}
}

func TestMJContextErrorMapsBadRequest(t *testing.T) {
	err := mjContextError(http.StatusBadRequest)
	if err == nil || err.Status != http.StatusBadRequest || err.Code != mj.CodeRequestError || err.Description != "invalid_request" {
		t.Fatalf("mjContextError(400) = %+v, want 400/code4/invalid_request", err)
	}
}

func TestMJUploadEmptyResultIsRejected(t *testing.T) {
	store := newFakeMJStore()
	upstream := newMJUpstream(t, `{"code":1,"result":[]}`)
	sealer := NewSecretSealer("mj-handler-test-key")
	normal, normalCand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney, upstream.server.URL, mjAllModels(), 10)
	channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{10: normal},
		candidates: []tasks.MJChannelCandidate{normalCand}}
	env := newMJTestEnv(t, store, channels, mjConfigured(nil), upstream)

	rec := serveMJ(t, env, newMJRequest(t, http.MethodPost, mj.EndpointUploadDiscord.Path(), `{"base64Array":["aGk="]}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	if rows := store.insertedRows(); len(rows) != 0 {
		t.Fatalf("empty upload result must not persist: %+v", rows)
	}
	if calls := env.lastRecorder(t).ReserveCalls(); countCall(calls, "refund") != 1 {
		t.Fatalf("empty upload result must refund the hold: %v", calls)
	}
}

// TestMJBillingSnapshotFollowsPriceTier 钉死 relay-05 §7.2 的档位在计费面的
// 落地：免计费档位预置显式零价快照；付费档位必须留空，交给严格按次解析。
//
// 变异点：把免计费档位也留空 → INPAINT/CUSTOM_ZOOM 在缺价时会 400（与设计
// 「不产生扣费」不符）；把付费档位也预置零价 → 所有动作变免费，本用例红。
func TestMJBillingSnapshotFollowsPriceTier(t *testing.T) {
	cases := []struct {
		name string
		body string
		free bool
	}{
		{"inpaint free", `{"taskId":"mj_origin_1","action":"INPAINT","index":1}`, true},
		{"custom zoom free", `{"taskId":"mj_origin_1","action":"CUSTOM_ZOOM","index":1}`, true},
		{"imagine paid", `{"prompt":"a cat"}`, false},
		{"video paid", `{"taskId":"mj_origin_1"}`, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeMJStore()
			store.byUserUpstream[mjUserUpstreamKey(42, "mj_origin_1")] = &tasks.Task{
				ID: 900, UpstreamTaskID: "mj_origin_1", Family: tasks.FamilyMJ,
				UserID: 42, ChannelID: 10, State: tasks.StateSuccess,
			}
			upstream := newMJUpstream(t, `{"code":1,"description":"ok","result":"mj_tier_1"}`)
			sealer := NewSecretSealer("mj-handler-test-key")
			normal, cand := sealedMJChannel(t, sealer, 10, mj.ChannelTypeMidjourney,
				upstream.server.URL, mjAllModels(), 10)
			channels := &fakeMJChannels{
				channels:   map[int64]*tasks.ChannelTarget{10: normal},
				candidates: []tasks.MJChannelCandidate{cand},
			}
			env := newMJTestEnv(t, store, channels, mjConfigured(nil), upstream)

			path := "/mj/submit/imagine"
			if tc.name == "inpaint free" || tc.name == "custom zoom free" {
				path = "/mj/submit/change"
			}
			if tc.name == "video paid" {
				path = "/mj/submit/video"
			}
			rec := serveMJ(t, env, newMJRequest(t, http.MethodPost, path, tc.body))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
			}
			snap := env.lastPriceSnapshot(t)
			if tc.free {
				if snap == nil {
					t.Fatal("免计费档位没有预置零价快照（缺价时会误判 400）")
				}
				if snap.Mode != contract.BillingModePerCall || !snap.Price.UsePrice || snap.Price.ModelPrice != 0 {
					t.Fatalf("免计费快照 = mode=%v use_price=%v price=%v",
						snap.Mode, snap.Price.UsePrice, snap.Price.ModelPrice)
				}
				return
			}
			if snap != nil {
				t.Fatalf("付费档位预置了快照 %+v，绕过严格按次解析会让缺价静默成免费", snap)
			}
		})
	}
}
