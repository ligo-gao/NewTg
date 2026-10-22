package httpapi

// W8/MJ-b 查询端点的单元验收（全内存 fake + 假上游）。
//
// 覆盖：
//   - fetch/list 的 (user_id, MjId) 归属：本人可见、他人与不存在同为 404；
//   - list-by-condition 的空数组、顺序、重复、族隔离与 ids 上界；
//   - image-seed 锁定原任务渠道、禁用渠道拒绝、上游 body 透传；
//   - 所有错误都是 {code,description} 信封（R-MJ-17），不泄露上游正文。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/mj"
	"newtg/gateway/internal/tasks"
)

// fakeMJQuerier 记录 image-seed/query 请求并返回固定结果。
type fakeMJQuerier struct {
	mu        sync.Mutex
	seedBody  []byte
	seedErr   *mj.Error
	seedCalls []string
	seedCfgs  []mj.SubmitConfig

	queryResult *mj.QueryResult
	queryErr    *mj.Error
	queryCalls  []string
}

func (q *fakeMJQuerier) Query(_ context.Context, _ mj.SubmitConfig, mjID string) (*mj.QueryResult, *mj.Error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queryCalls = append(q.queryCalls, mjID)
	if q.queryErr != nil {
		return nil, q.queryErr
	}
	if q.queryResult == nil {
		return nil, &mj.Error{Code: mj.CodeUnknownError, Description: mj.DescriptionInternalError, Status: 500}
	}
	copied := *q.queryResult
	return &copied, nil
}

func (q *fakeMJQuerier) queries() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queryCalls)
}

func (q *fakeMJQuerier) ImageSeed(_ context.Context, cfg mj.SubmitConfig, mjID string) ([]byte, *mj.Error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seedCalls = append(q.seedCalls, mjID)
	q.seedCfgs = append(q.seedCfgs, cfg)
	if q.seedErr != nil {
		return nil, q.seedErr
	}
	return append([]byte(nil), q.seedBody...), nil
}

func (q *fakeMJQuerier) calls() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.seedCalls)
}

// newMJQueryHandler 构造带查询接线的 handler + mux（用户 42/43）。
func newMJQueryHandler(t *testing.T, store *fakeMJStore, channels *fakeMJChannels,
	querier MJQuerier) (*http.ServeMux, string) {
	t.Helper()
	sealer := NewSecretSealer("mj-query-test-key")
	tokens, key42, _ := newMJTokens()
	handler := NewMJHandler(MJHandlerConfig{
		Store: store, Channels: channels, Sealer: sealer, Tokens: tokens,
		Billing:     func(*contract.RelayContext) contract.BillingSession { return newTaskBillingRecorder() },
		Config:      mjConfigured(nil),
		ImageSigner: mj.NewImageSigner("mj-query-test-key"),
		Querier:     querier,
		Now:         func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) },
		Uint64N:     func(uint64) uint64 { return 0 },
	})
	mux := http.NewServeMux()
	handler.Register(mux, RelayAuth(tokens, fakeTaskAccess{}, nil))
	return mux, "Bearer " + key42
}

func mjQueryTask(userID int64, upstreamID string, state tasks.State) *tasks.Task {
	return &tasks.Task{
		ID: int64(len(upstreamID)) + userID, PublicTaskID: "task_" + upstreamID,
		UpstreamTaskID: upstreamID, Family: tasks.FamilyMJ, Action: string(mj.ActionImagine),
		UserID: userID, ChannelID: 10, State: state, RawState: "IN_PROGRESS",
		ProgressPercent: 45, Properties: map[string]any{mj.PropertyPrompt: "a cat"},
		Data: map[string]any{
			mj.DataImageURL: "https://cdn.example/" + upstreamID + ".png",
		},
	}
}

func TestMJFetchOwnershipAndProjection(t *testing.T) {
	store := newFakeMJStore()
	channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{}}
	store.byUserUpstream[mjUserUpstreamKey(42, "mj_1")] = mjQueryTask(42, "mj_1", tasks.StateInProgress)
	store.byUserUpstream[mjUserUpstreamKey(43, "mj_1")] = mjQueryTask(43, "mj_1", tasks.StateSuccess)
	// 网关自身判定的失败：raw_state 为空，状态必须投影为 FAILURE（I-2）。
	forced := mjQueryTask(42, "mj_forced", tasks.StateFailure)
	forced.RawState = ""
	forced.FailReason = "task_timeout"
	store.byUserUpstream[mjUserUpstreamKey(42, "mj_forced")] = forced
	mux, auth := newMJQueryHandler(t, store, channels, &fakeMJQuerier{})

	serve := func(userAuth, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", userAuth)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	t.Run("own task", func(t *testing.T) {
		rec := serve(auth, "/mj/task/mj_1/fetch")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
		}
		body := decodeMJEnvelope(t, rec)
		if int(body["code"].(float64)) != mj.CodeSuccess {
			t.Fatalf("code = %v, want 1", body["code"])
		}
		result, ok := body["result"].(map[string]any)
		if !ok {
			t.Fatalf("result is not an object: %v", body["result"])
		}
		if result["id"] != "mj_1" || result["status"] != "IN_PROGRESS" || result["progress"] != "45%" {
			t.Fatalf("dto = %v", result)
		}
		if result["imageUrl"] != "https://cdn.example/mj_1.png" {
			t.Fatalf("imageUrl = %v", result["imageUrl"])
		}
		if result["prompt"] != "a cat" {
			t.Fatalf("prompt = %v", result["prompt"])
		}
	})

	t.Run("gateway forced failure projects FAILURE", func(t *testing.T) {
		rec := serve(auth, "/mj/task/mj_forced/fetch")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
		}
		body := decodeMJEnvelope(t, rec)
		result, ok := body["result"].(map[string]any)
		if !ok {
			t.Fatalf("result is not an object: %v", body["result"])
		}
		if result["status"] != "FAILURE" {
			t.Fatalf("gateway-forced failure status = %v, want FAILURE (raw_state must be empty)", result["status"])
		}
		if result["failReason"] != "task_timeout" {
			t.Fatalf("failReason = %v", result["failReason"])
		}
	})

	t.Run("foreign task is 404", func(t *testing.T) {
		rec := serve(auth, "/mj/task/mj_other/fetch")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		body := decodeMJEnvelope(t, rec)
		assertMJEnvelopeShape(t, body, mj.CodeRequestError, nil, false)
		if body["description"] != mj.DescriptionTaskNotFound {
			t.Fatalf("description = %v", body["description"])
		}
	})

	t.Run("invalid id", func(t *testing.T) {
		rec := serve(auth, "/mj/task/bad%2Fid/fetch")
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400/404", rec.Code)
		}
		// 无论路由匹配成 404 还是 handler 的 400，都必须是 MJ 信封。
		body := decodeMJEnvelope(t, rec)
		if int(body["code"].(float64)) == 0 {
			t.Fatalf("invalid id must use code != 0: %v", body)
		}
	})

	t.Run("no credentials", func(t *testing.T) {
		rec := serve("", "/mj/task/mj_1/fetch")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		body := decodeMJEnvelope(t, rec)
		if body["description"] != mj.DescriptionInvalidToken {
			t.Fatalf("description = %v", body["description"])
		}
	})
}

func TestMJListByConditionOwnershipOrderAndBounds(t *testing.T) {
	store := newFakeMJStore()
	channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{}}
	store.byUserUpstream[mjUserUpstreamKey(42, "mj_1")] = mjQueryTask(42, "mj_1", tasks.StateInProgress)
	store.byUserUpstream[mjUserUpstreamKey(42, "mj_2")] = mjQueryTask(42, "mj_2", tasks.StateSuccess)
	store.byUserUpstream[mjUserUpstreamKey(43, "mj_other")] = mjQueryTask(43, "mj_other", tasks.StateSuccess)
	// 插件行即便有同名 upstream id 也不得被 MJ 批量查询命中。
	store.byUserUpstream[mjUserUpstreamKey(42, "mj_plugin")] = &tasks.Task{
		ID: 999, PublicTaskID: "task_plugin", UpstreamTaskID: "mj_plugin",
		Family: tasks.FamilyPlugin, UserID: 42, State: tasks.StateSubmitted,
	}
	mux, auth := newMJQueryHandler(t, store, channels, &fakeMJQuerier{})

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mj/task/list-by-condition", strings.NewReader(body))
		req.Header.Set("Authorization", auth)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	t.Run("own rows only in input order", func(t *testing.T) {
		rec := post(`{"ids":["mj_2","mj_other","mj_1","mj_missing","mj_plugin"]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
		}
		body := decodeMJEnvelope(t, rec)
		results, ok := body["result"].([]any)
		if !ok {
			t.Fatalf("result is not an array: %v", body["result"])
		}
		if len(results) != 2 {
			t.Fatalf("results = %v, want exactly the two own rows", results)
		}
		first := results[0].(map[string]any)
		second := results[1].(map[string]any)
		if first["id"] != "mj_2" || second["id"] != "mj_1" {
			t.Fatalf("order = %v/%v, want mj_2,mj_1", first["id"], second["id"])
		}
	})

	t.Run("empty ids", func(t *testing.T) {
		rec := post(`{"ids":[]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if strings.TrimSpace(rec.Body.String()) != `{"code":1,"description":"","result":[]}` {
			t.Fatalf("empty ids body = %s", rec.Body.String())
		}
	})

	t.Run("duplicate ids preserved", func(t *testing.T) {
		rec := post(`{"ids":["mj_1","mj_1"]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		body := decodeMJEnvelope(t, rec)
		results, _ := body["result"].([]any)
		if len(results) != 2 || results[0].(map[string]any)["id"] != "mj_1" || results[1].(map[string]any)["id"] != "mj_1" {
			t.Fatalf("duplicate ids must be preserved in order: %v", results)
		}
	})

	t.Run("missing or null ids", func(t *testing.T) {
		for _, body := range []string{`{}`, `{"ids":null}`} {
			rec := post(body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("body %s status = %d, want 400 (OpenAPI requires ids)", body, rec.Code)
			}
			envelope := decodeMJEnvelope(t, rec)
			if envelope["description"] != mj.DescriptionInvalidRequest {
				t.Fatalf("body %s description = %v", body, envelope["description"])
			}
		}
	})

	t.Run("oversized body uses dedicated description", func(t *testing.T) {
		// 合法 JSON 形状 + 超长字符串：解码器必须读到 MaxBytesReader 上限
		// 之后才失败（纯非 JSON 正文会先报语法错误）。
		rec := post(`{"ids":["` + strings.Repeat("a", maxMJListBodyBytes+1) + `"]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		body := decodeMJEnvelope(t, rec)
		if body["description"] != mj.DescriptionBodyTooLarge {
			t.Fatalf("description = %v, want %s", body["description"], mj.DescriptionBodyTooLarge)
		}
	})

	t.Run("too many ids", func(t *testing.T) {
		ids := make([]string, mj.MaxListIDs+1)
		for i := range ids {
			ids[i] = fmt.Sprintf("mj_%d", i)
		}
		raw, _ := json.Marshal(map[string]any{"ids": ids})
		rec := post(string(raw))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		body := decodeMJEnvelope(t, rec)
		if body["description"] != mj.DescriptionTooManyIDs {
			t.Fatalf("description = %v", body["description"])
		}
	})

	t.Run("invalid id", func(t *testing.T) {
		rec := post(`{"ids":["bad/id"]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		body := decodeMJEnvelope(t, rec)
		if body["description"] != mj.DescriptionInvalidID {
			t.Fatalf("description = %v", body["description"])
		}
	})
}

func TestMJImageSeedLocksOriginChannelAndPassesThrough(t *testing.T) {
	sealer := NewSecretSealer("mj-query-test-key")
	sealed, err := sealer.Seal([]byte(`{"api_key":"origin-channel-key"}`))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	store := newFakeMJStore()
	task := mjQueryTask(42, "mj_1", tasks.StateSuccess)
	task.LockedChannelID = 7
	task.ChannelID = 10
	store.byUserUpstream[mjUserUpstreamKey(42, "mj_1")] = task
	store.byUserUpstream[mjUserUpstreamKey(43, "mj_1")] = mjQueryTask(43, "mj_1", tasks.StateSuccess)
	channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{
		7: {ID: 7, BaseURL: "http://origin.example", Credentials: sealed, Status: 1, Type: mj.ChannelTypeMidjourney},
	}}
	querier := &fakeMJQuerier{seedBody: []byte(`{"code":1,"description":"","result":["seed"]}`)}
	mux, auth := newMJQueryHandler(t, store, channels, querier)

	post := func(userAuth, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", userAuth)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	rec := post(auth, "/mj/task/mj_1/image-seed")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"seed"`) {
		t.Fatalf("seed body not passed through: %s", rec.Body.String())
	}
	if querier.calls() != 1 {
		t.Fatalf("seed calls = %d, want 1", querier.calls())
	}
	cfg := querier.seedCfgs[0]
	if cfg.BaseURL != "http://origin.example" || cfg.APIKey != "origin-channel-key" {
		t.Fatalf("seed used wrong channel: %+v", cfg)
	}

	t.Run("foreign task", func(t *testing.T) {
		before := querier.calls()
		rec := post(auth, "/mj/task/mj_2/image-seed")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if querier.calls() != before {
			t.Fatalf("foreign task must not reach upstream")
		}
	})

	t.Run("disabled channel", func(t *testing.T) {
		disabled := *channels.channels[7]
		disabled.Status = 2
		channels.channels[7] = &disabled
		before := querier.calls()
		rec := post(auth, "/mj/task/mj_1/image-seed")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		body := decodeMJEnvelope(t, rec)
		if body["description"] != mj.DescriptionChannelDisabled {
			t.Fatalf("description = %v", body["description"])
		}
		if querier.calls() != before {
			t.Fatalf("disabled channel must not reach upstream")
		}
	})

	t.Run("channel type drift", func(t *testing.T) {
		drifted := *channels.channels[7]
		drifted.Status = 1
		drifted.Type = "openai"
		channels.channels[7] = &drifted
		before := querier.calls()
		rec := post(auth, "/mj/task/mj_1/image-seed")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		body := decodeMJEnvelope(t, rec)
		if body["description"] != mj.DescriptionChannelDisabled {
			t.Fatalf("description = %v", body["description"])
		}
		if querier.calls() != before {
			t.Fatalf("non-MJ channel must not reach upstream")
		}
	})
}

func TestMJImageSeedUpstreamErrorIsEnvelope(t *testing.T) {
	sealer := NewSecretSealer("mj-query-test-key")
	sealed, _ := sealer.Seal([]byte(`{"api_key":"k"}`))
	store := newFakeMJStore()
	store.byUserUpstream[mjUserUpstreamKey(42, "mj_1")] = mjQueryTask(42, "mj_1", tasks.StateSuccess)
	channels := &fakeMJChannels{channels: map[int64]*tasks.ChannelTarget{
		10: {ID: 10, BaseURL: "http://origin.example", Credentials: sealed, Status: 1, Type: mj.ChannelTypeMidjourney},
	}}
	querier := &fakeMJQuerier{seedErr: &mj.Error{
		Code: mj.CodeUnknownError, Description: mj.DescriptionUpstreamError, Status: http.StatusBadGateway,
	}}
	mux, auth := newMJQueryHandler(t, store, channels, querier)

	req := httptest.NewRequest(http.MethodPost, "/mj/task/mj_1/image-seed", nil)
	req.Header.Set("Authorization", auth)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	body := decodeMJEnvelope(t, rec)
	assertMJEnvelopeShape(t, body, mj.CodeUnknownError, nil, false)
	if body["description"] != mj.DescriptionUpstreamError {
		t.Fatalf("description = %v", body["description"])
	}
}
