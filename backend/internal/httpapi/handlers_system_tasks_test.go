package httpapi

// W17-2 系统任务管理面的单元验收（billing-06 §13.5 / modules-08 F-08-21）。
//
// 覆盖：
//   - 列表/详情的数据源与 no-store；
//   - 未知任务 404、非白名单 403、运行中/停用 409（服务端，不依赖前端隐藏）；
//   - Idempotency-Key 必带与同键重放不产生第二条 pending 行；
//   - 每次触发（成功与失败）写一条 system.task.trigger 审计。
//
// 真实 SQL/Redis/租约行为在 handlers_system_tasks_integration_test.go。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/worker"
)

type fakeSystemTaskService struct {
	mu sync.Mutex

	listResult  worker.TaskPage
	detail      worker.TaskDetail
	trigger     worker.TriggerResult
	listErr     error
	detailErr   error
	triggerErr  error
	triggerSeen []string
}

func (f *fakeSystemTaskService) List(context.Context, int, int) (worker.TaskPage, error) {
	return f.listResult, f.listErr
}

func (f *fakeSystemTaskService) Detail(context.Context, string, int, int) (worker.TaskDetail, error) {
	return f.detail, f.detailErr
}

func (f *fakeSystemTaskService) Trigger(_ context.Context, name string) (worker.TriggerResult, error) {
	f.mu.Lock()
	f.triggerSeen = append(f.triggerSeen, name)
	f.mu.Unlock()
	return f.trigger, f.triggerErr
}

func (f *fakeSystemTaskService) triggerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.triggerSeen)
}

type systemTaskTestEnv struct {
	mux     *http.ServeMux
	service *fakeSystemTaskService
	audit   *captureExec
}

func newSystemTaskTestEnv(t *testing.T, service *fakeSystemTaskService) *systemTaskTestEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	capture := &captureExec{}
	handlers := NewSystemTaskHandlers(SystemTaskHandlerConfig{
		Service: service,
		Authz:   rbac.NewAuthzMiddleware(),
		Audit:   audit.New(capture),
		Idem:    NewRedisIdempotencyStore(rdb),
		Logger:  testLogger(),
	})
	mux := http.NewServeMux()
	handlers.Register(mux)
	return &systemTaskTestEnv{mux: mux, service: service, audit: capture}
}

func systemTaskAdmin() *auth.Principal {
	return &auth.Principal{UserID: 7, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
}

func systemTaskViewer() *auth.Principal {
	return &auth.Principal{UserID: 8, Role: rbac.RoleAdmin, Permissions: []string{}}
}

func doSystemTaskRequest(t *testing.T, env *systemTaskTestEnv, method, target string,
	principal *auth.Principal, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(method, target, "", principal, headers))
	return rr
}

func decodeTaskEnvelope[T any](t *testing.T, rr *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return out
}

func TestSystemTasksListRequiresSettingsManage(t *testing.T) {
	service := &fakeSystemTaskService{listResult: worker.TaskPage{
		Items: []worker.TaskSummary{{
			TaskType:             "worker_history_cleanup",
			Enabled:              true,
			IntervalSeconds:      86400,
			ManualTriggerAllowed: true,
		}},
		Total: 1, Page: 1, PageSize: 20,
	}}
	env := newSystemTaskTestEnv(t, service)

	denied := doSystemTaskRequest(t, env, http.MethodGet, "/api/system/tasks", systemTaskViewer(), nil)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("无 settings:manage = %d，期望 403", denied.Code)
	}

	ok := doSystemTaskRequest(t, env, http.MethodGet, "/api/system/tasks", systemTaskAdmin(), nil)
	if ok.Code != http.StatusOK {
		t.Fatalf("列表 = %d，期望 200", ok.Code)
	}
	if got := ok.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q，期望 no-store", got)
	}
	page := decodeTaskEnvelope[worker.TaskPage](t, ok)
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].TaskType != "worker_history_cleanup" {
		t.Fatalf("列表 = %+v", page)
	}
}

func TestSystemTaskDetailUnknownName404(t *testing.T) {
	service := &fakeSystemTaskService{detailErr: worker.ErrTaskNotFound}
	env := newSystemTaskTestEnv(t, service)
	resp := doSystemTaskRequest(t, env, http.MethodGet, "/api/system/tasks/no-such-task", systemTaskAdmin(), nil)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("未知任务 = %d，期望 404", resp.Code)
	}
}

func TestSystemTaskTriggerWhitelistAndConflicts(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		outcome    string
	}{
		{"non-whitelist", worker.ErrTaskManualNotAllowed, http.StatusForbidden, "not_manual"},
		{"already-running", worker.ErrTaskAlreadyRunning, http.StatusConflict, "already_running"},
		{"disabled", worker.ErrTaskDisabled, http.StatusConflict, "disabled"},
		{"unknown", worker.ErrTaskNotFound, http.StatusNotFound, "not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := &fakeSystemTaskService{triggerErr: tc.err}
			env := newSystemTaskTestEnv(t, service)
			resp := doSystemTaskRequest(t, env, http.MethodPost,
				"/api/system/tasks/async_task_poll:trigger", systemTaskAdmin(),
				map[string]string{"Idempotency-Key": "k-" + tc.name})
			if resp.Code != tc.wantStatus {
				t.Fatalf("状态 = %d，期望 %d", resp.Code, tc.wantStatus)
			}
			// 每次失败都必须写审计（来源 TC-F-08-21-07）。
			calls := env.audit.snapshot()
			if len(calls) != 1 {
				t.Fatalf("审计条数 = %d，期望 1", len(calls))
			}
			detail := decodeAuditDetail(t, calls[0][9])
			if detail["action"] != audit.ActionSystemTaskTrigger {
				t.Fatalf("审计 action = %v", detail["action"])
			}
			if detail["outcome"] != tc.outcome {
				t.Fatalf("审计 outcome = %v，期望 %s", detail["outcome"], tc.outcome)
			}
			if result := calls[0][8].(string); result != "failed" {
				t.Fatalf("审计 result = %q，期望 failed", result)
			}
		})
	}
}

func TestSystemTaskTriggerAcceptedAndIdempotent(t *testing.T) {
	service := &fakeSystemTaskService{trigger: worker.TriggerResult{
		RunID: 42, TaskType: "worker_history_cleanup", Trigger: worker.TriggerManual,
	}}
	env := newSystemTaskTestEnv(t, service)
	headers := map[string]string{"Idempotency-Key": "same-key"}

	first := doSystemTaskRequest(t, env, http.MethodPost,
		"/api/system/tasks/worker_history_cleanup:trigger", systemTaskAdmin(), headers)
	if first.Code != http.StatusAccepted {
		t.Fatalf("触发 = %d，期望 202", first.Code)
	}
	body := decodeTaskEnvelope[worker.TriggerResult](t, first)
	if body.RunID != 42 || body.Trigger != worker.TriggerManual {
		t.Fatalf("触发响应 = %+v", body)
	}
	if service.triggerCount() != 1 {
		t.Fatalf("service 调用 = %d，期望 1", service.triggerCount())
	}

	// 同键重放：不执行第二次 Trigger，响应逐字节一致。
	second := doSystemTaskRequest(t, env, http.MethodPost,
		"/api/system/tasks/worker_history_cleanup:trigger", systemTaskAdmin(), headers)
	if second.Code != http.StatusAccepted {
		t.Fatalf("重放 = %d，期望 202", second.Code)
	}
	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("重放响应必须带 Idempotent-Replayed: true")
	}
	if service.triggerCount() != 1 {
		t.Fatalf("重放后 service 调用 = %d，期望仍为 1", service.triggerCount())
	}
	calls := env.audit.snapshot()
	if len(calls) != 1 {
		t.Fatalf("审计条数 = %d，期望 1（重放不重复写审计）", len(calls))
	}
	detail := decodeAuditDetail(t, calls[0][9])
	if detail["outcome"] != "accepted" || detail["task_type"] != "worker_history_cleanup" {
		t.Fatalf("审计 detail = %+v", detail)
	}
	if runID, ok := detail["run_id"].(float64); !ok || int64(runID) != 42 {
		t.Fatalf("审计 run_id = %v", detail["run_id"])
	}
}

func TestSystemTaskTriggerRequiresIdempotencyKey(t *testing.T) {
	service := &fakeSystemTaskService{}
	env := newSystemTaskTestEnv(t, service)
	resp := doSystemTaskRequest(t, env, http.MethodPost,
		"/api/system/tasks/worker_history_cleanup:trigger", systemTaskAdmin(), nil)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("缺 Idempotency-Key = %d，期望 400", resp.Code)
	}
	if service.triggerCount() != 0 {
		t.Fatal("缺幂等键不得调用服务")
	}
}

func TestSystemTaskTriggerRejectsMissingSuffix(t *testing.T) {
	service := &fakeSystemTaskService{}
	env := newSystemTaskTestEnv(t, service)
	resp := doSystemTaskRequest(t, env, http.MethodPost,
		"/api/system/tasks/worker_history_cleanup", systemTaskAdmin(),
		map[string]string{"Idempotency-Key": "no-suffix"})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("缺 :trigger 后缀 = %d，期望 404", resp.Code)
	}
	if service.triggerCount() != 0 {
		t.Fatal("缺后缀不得调用服务")
	}
}

func decodeAuditDetail(t *testing.T, arg any) map[string]any {
	t.Helper()
	switch raw := arg.(type) {
	case json.RawMessage:
		var detail map[string]any
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatalf("审计 detail 不是合法 JSON: %v", err)
		}
		return detail
	case []byte:
		var detail map[string]any
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatalf("审计 detail 不是合法 JSON: %v", err)
		}
		return detail
	case string:
		var detail map[string]any
		if err := json.Unmarshal([]byte(raw), &detail); err != nil {
			t.Fatalf("审计 detail 不是合法 JSON: %v", err)
		}
		return detail
	default:
		t.Fatalf("审计 detail 类型 = %T", arg)
		return nil
	}
}

// 编译期断言：fake 满足接口。
var _ SystemTaskService = (*fakeSystemTaskService)(nil)
