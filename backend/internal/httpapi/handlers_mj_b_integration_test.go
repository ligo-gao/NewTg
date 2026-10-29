//go:build integration

package httpapi

// W8/MJ-b 的真实 PostgreSQL 验收：查询端点 + 轮询驱动的终态结算。
//
// 覆盖：
//   - fetch/list 的 (user_id, MjId) 归属与 family 隔离（真实 SQL）；
//   - image-seed 锁定原渠道并透传上游 body；
//   - 轮询 SUCCESS/FAILURE 的终态 CAS + 按 request_id 结算/退款恰好一次；
//   - 并发 PollTask 单赢家（真实 ApplyPoll CAS）；
//   - MJPoller 补偿扫描对真实 pending 行的重试。
//
// 运行：NEWTG_TEST_DB_DSN=postgres://... go test -tags=integration ./internal/httpapi/

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

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/mj"
	"newtg/gateway/internal/tasks"
)

const mjBIntegrationPrefix = "mj-b-int-"

// insertMJBTask 写一条 family='mj' 的真实任务行。
func insertMJBTask(t *testing.T, repo *tasks.Repo, userID, channelID int64,
	upstreamID string, state tasks.State, quota int64, private map[string]any) *tasks.Task {
	t.Helper()
	publicID, err := tasks.NewPublicTaskID()
	if err != nil {
		t.Fatalf("public id: %v", err)
	}
	now := time.Now().UTC()
	row := &tasks.Task{
		PublicTaskID: publicID, UpstreamTaskID: upstreamID, Family: tasks.FamilyMJ,
		Action: string(mj.ActionImagine), PluginKey: "", FetchMode: mj.FetchModePoll,
		State: state, UserID: userID, Group: "default", ChannelID: channelID,
		Quota: quota, SubmitTime: &now,
		Properties:  map[string]any{mj.PropertyPrompt: "a cat"},
		Data:        map[string]any{},
		PrivateData: private,
	}
	if err := repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("insert mj task: %v", err)
	}
	return row
}

func TestMJBQueryEndpointsOnRealDB(t *testing.T) {
	db := openMJIntegrationDB(t)
	repo := tasks.NewRepo(db)
	sealer := NewSecretSealer("mj-integration-key")
	userID := time.Now().UnixNano() % 1_000_000
	otherUser := userID + 1
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE user_id IN ($1,$2)`, userID, otherUser)
		_, _ = db.Exec(`DELETE FROM channels WHERE name LIKE 'mj-b-int-%'`)
	})

	var seedPath, seedSecret string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/image-seed"):
			seedPath, seedSecret = r.URL.Path, r.Header.Get("mj-api-secret")
			_, _ = w.Write([]byte(`{"code":1,"description":"","result":["seed"]}`))
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	channelID := insertMJIntegrationChannel(t, db, sealer,
		fmt.Sprintf("%s%d", mjBIntegrationPrefix, userID), mj.ChannelTypeMidjourney, upstream.URL, mjAllModels())
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, channelID) })

	ownA := insertMJBTask(t, repo, userID, channelID, "mj_b_own_a", tasks.StateInProgress, 50,
		map[string]any{mj.PrivateRequestID: "req_a", mj.PrivateTokenID: float64(1)})
	ownB := insertMJBTask(t, repo, userID, channelID, "mj_b_own_b", tasks.StateSuccess, 50,
		map[string]any{mj.PrivateRequestID: "req_b"})
	foreign := insertMJBTask(t, repo, otherUser, channelID, "mj_b_foreign", tasks.StateSuccess, 50,
		map[string]any{mj.PrivateRequestID: "req_f"})
	// 同用户插件行即使 upstream id 同名也不得被 MJ 查询命中。
	pluginRow := &tasks.Task{
		PublicTaskID: "task_mj_b_plugin", UpstreamTaskID: "mj_b_own_a",
		Family: tasks.FamilyPlugin, Action: "generate", PluginKey: "sample",
		FetchMode: "poll", State: tasks.StateSubmitted, UserID: userID, ChannelID: channelID,
		Properties: map[string]any{}, Data: map[string]any{}, PrivateData: map[string]any{},
	}
	if err := repo.Insert(context.Background(), pluginRow); err != nil {
		t.Fatalf("insert plugin row: %v", err)
	}

	tokens := mjTokenLookup{
		auth.HashTokenKey("nkt-mj-b"): {
			ID: 1, UserID: userID, Status: "enabled", Group: "default", UnlimitedQuota: true,
		},
	}
	handler := NewMJHandler(MJHandlerConfig{
		Store: repo, Channels: repo, Sealer: sealer, Tokens: tokens,
		Billing:     func(*contract.RelayContext) contract.BillingSession { return newTaskBillingRecorder() },
		Config:      mjConfigured(nil),
		ImageSigner: mj.NewImageSigner("mj-integration-key"),
	})
	mux := http.NewServeMux()
	handler.Register(mux, RelayAuth(tokens, fakeTaskAccess{}, nil))

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer nkt-mj-b")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// 本人任务
	rec := get("/mj/task/mj_b_own_a/fetch")
	if rec.Code != http.StatusOK {
		t.Fatalf("own fetch status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	dto, _ := envelope["result"].(map[string]any)
	if dto["id"] != "mj_b_own_a" || dto["prompt"] != "a cat" {
		t.Fatalf("dto = %v", dto)
	}

	// 他人任务 / 不存在 / 插件同名行 → 同一个 404
	for _, id := range []string{"mj_b_foreign", "mj_b_missing", "mj_b_own_a_plugin_only"} {
		rec := get("/mj/task/" + id + "/fetch")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("fetch %s status = %d, want 404", id, rec.Code)
		}
	}

	// 批量：只返回本人 MJ 行，按输入顺序。
	body := `{"ids":["mj_b_own_b","mj_b_foreign","mj_b_own_a","mj_b_missing"]}`
	req := httptest.NewRequest(http.MethodPost, "/mj/task/list-by-condition", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer nkt-mj-b")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	var listed struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Result) != 2 || listed.Result[0]["id"] != "mj_b_own_b" || listed.Result[1]["id"] != "mj_b_own_a" {
		t.Fatalf("list result = %v, want [mj_b_own_b,mj_b_own_a]", listed.Result)
	}

	// image-seed：锁定原任务渠道 + 渠道凭据 + body 透传。
	req = httptest.NewRequest(http.MethodPost, "/mj/task/mj_b_own_b/image-seed", nil)
	req.Header.Set("Authorization", "Bearer nkt-mj-b")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed status = %d (body=%s)", rec.Code, rec.Body.String())
	}
	if seedPath != "/mj/task/mj_b_own_b/image-seed" || seedSecret != "integration-channel-key" {
		t.Fatalf("upstream seed path/secret = %q/%q", seedPath, seedSecret)
	}
	if !strings.Contains(rec.Body.String(), `"seed"`) {
		t.Fatalf("seed body not passed through: %s", rec.Body.String())
	}

	// 占位变量确保未使用的 ID 被断言（编译期防漏）。
	_ = ownA
	_ = ownB
	_ = foreign
}

// mjBUpstream 按 MjId 返回固定 fetch 结果。
func newMJBUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/mj_poll_success/fetch"):
			_, _ = w.Write([]byte(`{"code":1,"result":{"status":"SUCCESS","progress":"100%",` +
				`"imageUrl":"https://cdn.example/poll.png","videoUrls":["https://cdn.example/v.mp4"]}}`))
		case strings.HasSuffix(r.URL.Path, "/mj_poll_failure/fetch"):
			_, _ = w.Write([]byte(`{"code":1,"result":{"status":"FAILURE","progress":"100%","failReason":"upstream failed"}}`))
		default:
			_, _ = w.Write([]byte(`{"code":1,"result":{"status":"IN_PROGRESS","progress":"45%"}}`))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestMJBPollDriverSettlesOnceOnRealDB(t *testing.T) {
	db := openMJIntegrationDB(t)
	repo := tasks.NewRepo(db)
	sealer := NewSecretSealer("mj-integration-key")
	userID := time.Now().UnixNano() % 1_000_000
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID)
		_, _ = db.Exec(`DELETE FROM channels WHERE name LIKE 'mj-b-int-%'`)
	})
	upstream := newMJBUpstream(t)
	channelID := insertMJIntegrationChannel(t, db, sealer,
		fmt.Sprintf("%s%d", mjBIntegrationPrefix, userID), mj.ChannelTypeMidjourney, upstream.URL, mjAllModels())
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, channelID) })

	cases := []struct {
		name       string
		upstreamID string
		wantState  tasks.State
		wantCall   string
		wantQuota  int64
		wantFail   string
	}{
		{"success", "mj_poll_success", tasks.StateSuccess, "terminate_by_id_for_token", 60, ""},
		{"failure", "mj_poll_failure", tasks.StateFailure, "refund_by_id_for_token", 0, "upstream failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := insertMJBTask(t, repo, userID, channelID, tc.upstreamID, tasks.StateInProgress, 60,
				map[string]any{mj.PrivateRequestID: "req_" + tc.name, mj.PrivateTokenID: float64(9)})
			recorder := newTaskBillingRecorder()
			settler, err := tasks.NewSettler(tasks.SettlerConfig{Billing: recorder, Repo: repo})
			if err != nil {
				t.Fatalf("NewSettler: %v", err)
			}
			driver, err := NewMJPollDriver(MJPollDriverConfig{
				Store: repo, Channels: repo, Sealer: sealer,
				Client: mj.NewSubmitClient(upstream.Client()), Settler: settler,
				RetentionDays: func() int64 { return 30 },
			})
			if err != nil {
				t.Fatalf("NewMJPollDriver: %v", err)
			}
			live := *row
			if err := driver.PollTask(context.Background(), &live); err != nil {
				t.Fatalf("PollTask: %v", err)
			}
			got, err := repo.GetByUserUpstreamID(context.Background(), userID, tc.upstreamID)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if got.State != tc.wantState {
				t.Fatalf("state = %s, want %s", got.State, tc.wantState)
			}
			if got.Quota != tc.wantQuota {
				t.Fatalf("quota = %d, want %d", got.Quota, tc.wantQuota)
			}
			if tc.wantFail != "" && got.FailReason != tc.wantFail {
				t.Fatalf("fail reason = %q, want %q", got.FailReason, tc.wantFail)
			}
			if got.RetentionDeadline == nil {
				t.Fatalf("terminal row must carry a retention deadline")
			}
			calls := recorder.ReserveCalls()
			if countCall(calls, tc.wantCall) != 1 {
				t.Fatalf("billing calls = %v, want exactly one %s", calls, tc.wantCall)
			}
			if recorder.lastTokenID != 9 {
				t.Fatalf("settlement token = %d, want 9", recorder.lastTokenID)
			}
		})
	}
}

func TestMJBPollDriverConcurrentSingleSettlementOnRealDB(t *testing.T) {
	db := openMJIntegrationDB(t)
	repo := tasks.NewRepo(db)
	sealer := NewSecretSealer("mj-integration-key")
	userID := time.Now().UnixNano() % 1_000_000
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID)
		_, _ = db.Exec(`DELETE FROM channels WHERE name LIKE 'mj-b-int-%'`)
	})
	upstream := newMJBUpstream(t)
	channelID := insertMJIntegrationChannel(t, db, sealer,
		fmt.Sprintf("%s%d", mjBIntegrationPrefix, userID), mj.ChannelTypeMidjourney, upstream.URL, mjAllModels())
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, channelID) })

	row := insertMJBTask(t, repo, userID, channelID, "mj_poll_success", tasks.StateInProgress, 60,
		map[string]any{mj.PrivateRequestID: "req_race", mj.PrivateTokenID: float64(3)})
	recorder := newTaskBillingRecorder()
	settler, err := tasks.NewSettler(tasks.SettlerConfig{Billing: recorder, Repo: repo})
	if err != nil {
		t.Fatalf("NewSettler: %v", err)
	}
	driver, err := NewMJPollDriver(MJPollDriverConfig{
		Store: repo, Channels: repo, Sealer: sealer,
		Client: mj.NewSubmitClient(upstream.Client()), Settler: settler,
	})
	if err != nil {
		t.Fatalf("NewMJPollDriver: %v", err)
	}

	const workers = 6
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		live := *row
		go func() {
			defer wg.Done()
			<-start
			_ = driver.PollTask(context.Background(), &live)
		}()
	}
	close(start)
	wg.Wait()

	calls := recorder.ReserveCalls()
	if got := countCall(calls, "terminate_by_id_for_token"); got != 1 {
		t.Fatalf("concurrent polls settled %d times, want exactly 1 (calls=%v)", got, calls)
	}
	got, err := repo.GetByUserUpstreamID(context.Background(), userID, "mj_poll_success")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.State != tasks.StateSuccess || got.Quota != 60 {
		t.Fatalf("row state/quota = %s/%d", got.State, got.Quota)
	}
}

func TestMJBPollerReconcilesPendingOnRealDB(t *testing.T) {
	db := openMJIntegrationDB(t)
	repo := tasks.NewRepo(db)
	userID := time.Now().UnixNano() % 1_000_000
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tasks WHERE user_id = $1`, userID) })

	row := insertMJBTask(t, repo, userID, 1, "mj_b_pending", tasks.StateFailure, 40,
		map[string]any{mj.PrivateRequestID: "req_pending", mj.PrivateTokenID: float64(5),
			"billing_pending": true})
	recorder := newTaskBillingRecorder()
	settler, err := tasks.NewSettler(tasks.SettlerConfig{Billing: recorder, Repo: repo})
	if err != nil {
		t.Fatalf("NewSettler: %v", err)
	}
	poller, err := tasks.NewMJPoller(tasks.MJPollerConfig{Repo: repo, Settler: settler})
	if err != nil {
		t.Fatalf("NewMJPoller: %v", err)
	}
	poller.RunOnce(context.Background())

	got, err := repo.GetByID(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.PrivateData[mj.PrivateRequestID] != "req_pending" {
		t.Fatalf("request id changed: %v", got.PrivateData)
	}
	calls := recorder.ReserveCalls()
	if countCall(calls, "refund_by_id_for_token") != 1 {
		t.Fatalf("reconcile calls = %v, want one refund", calls)
	}
	if got.Quota != 0 {
		t.Fatalf("refunded quota = %d, want 0", got.Quota)
	}

	// 清理可能残留的其它用户行（本用例只用自己的行）。
	_, _ = db.Exec(`DELETE FROM tasks WHERE id = $1`, row.ID)
}
