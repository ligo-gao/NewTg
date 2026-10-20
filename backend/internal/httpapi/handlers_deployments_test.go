package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/deploy/ionet"
	"newtg/gateway/internal/rbac"
)

// 本文件验收 T4 部署管理端全族（modules-08 §3.20 / F-08-52～F-08-58）：
// 禁用门、权限门、全量搜索与截断、幂等重放/在途、报价门、秘密零回显/零审计、
// provider 错误映射与日志游标映射。全部使用 fake provider，不触真实 io.net。

type deployMgmtEnv struct {
	mux      *http.ServeMux
	service  *fakeSettingsService
	provider *ionet.FakeProvider
	secrets  *fakeDeploySecretStore
	audit    *fakeAuditExec
	idem     IdempotencyStore
}

func newDeployMgmtEnv(t *testing.T) *deployMgmtEnv {
	t.Helper()
	return newDeployMgmtEnvWithIdem(t, NewMemoryIdempotencyStore())
}

func newDeployMgmtEnvWithIdem(t *testing.T, idem IdempotencyStore) *deployMgmtEnv {
	t.Helper()
	service := &fakeSettingsService{getView: sampleDeployView(true, true)}
	provider := &ionet.FakeProvider{NameAvailable: true}
	secrets := &fakeDeploySecretStore{key: "stored-key", ok: true}
	exec := &fakeAuditExec{}
	env := &deployMgmtEnv{
		service: service, provider: provider, secrets: secrets, audit: exec, idem: idem,
	}
	handlers, err := NewDeployManagementHandlers(DeployManagementHandlerConfig{
		Settings: service,
		Secrets:  secrets,
		Provider: func(string) ionet.DeploymentProvider { return provider },
		Authz:    rbac.NewAuthzMiddleware(),
		Audit:    audit.New(exec),
		Idem:     idem,
		Logger:   testLogger(),
		Now:      func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewDeployManagementHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	env.mux = mux
	return env
}

func (e *deployMgmtEnv) do(method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, settingsRequest(method, target, body, deployManager(), headers))
	return rr
}

func deployWriteHeaders() map[string]string {
	return map[string]string{"Idempotency-Key": "idem-key-1"}
}

func decodeDeployProblem(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var problem map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
		t.Fatalf("problem decode: %v（body=%s）", err, rr.Body.String())
	}
	return problem
}

// inFlightIdemStore 让 Claim 恒返回「同键在途」，用于覆盖 409 分支。
type inFlightIdemStore struct{}

func (inFlightIdemStore) Claim(context.Context, string, time.Duration) (IdempotencyClaim, error) {
	return IdempotencyClaim{Status: IdempotencyClaimInFlight}, nil
}
func (inFlightIdemStore) Complete(context.Context, string, string, IdempotentResponse, time.Duration) error {
	return nil
}
func (inFlightIdemStore) Release(context.Context, string, string) error { return nil }

// ---------- 权限 / 禁用门 ----------

func TestDeployManagementRequiresSettingsManage(t *testing.T) {
	env := newDeployMgmtEnv(t)
	noPerm := &auth.Principal{UserID: 2, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermUsersRead}}
	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/api/deployments"},
		{http.MethodGet, "/api/deployments/hardware-types"},
		{http.MethodGet, "/api/deployments/dep-1"},
		{http.MethodPost, "/api/deployments"},
	} {
		rr := httptest.NewRecorder()
		env.mux.ServeHTTP(rr, settingsRequest(tc.method, tc.target, "{}", noPerm, nil))
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s %s = %d，期望 403", tc.method, tc.target, rr.Code)
		}
	}
}

func TestDeployManagementDisabledGate(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.service.getView = sampleDeployView(false, true)

	rr := env.do(http.MethodGet, "/api/deployments", "", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d，期望 409（body=%s）", rr.Code, rr.Body.String())
	}
	if got := decodeDeployProblem(t, rr)["type"]; got != problemTypeDeployDisabled {
		t.Fatalf("problem.type = %v", got)
	}

	// 写端点同样被禁用门拦住，且不产生 provider 调用。
	rr = env.do(http.MethodPost, "/api/deployments", `{"name":"x"}`, deployWriteHeaders())
	if rr.Code != http.StatusConflict {
		t.Fatalf("create status = %d，期望 409", rr.Code)
	}
	if env.provider.CreateCalls != 0 {
		t.Fatalf("禁用状态不得调用 provider 写: %d", env.provider.CreateCalls)
	}
}

func TestDeployManagementSecretMissing(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.secrets.ok = false
	rr := env.do(http.MethodGet, "/api/deployments", "", nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d，期望 422", rr.Code)
	}
	if got := decodeDeployProblem(t, rr)["type"]; got != problemTypeDeploySecretMissing {
		t.Fatalf("problem.type = %v", got)
	}
}

// ---------- 列表 / 全量搜索 ----------

func TestDeployListScansAllPagesAndTruncates(t *testing.T) {
	env := newDeployMgmtEnv(t)
	for i := 0; i < 1500; i++ {
		status := "running"
		if i%2 == 1 {
			status = "completed"
		}
		env.provider.Deployments = append(env.provider.Deployments, ionet.DeploymentSummary{
			ID: fmt.Sprintf("dep-%04d", i), Name: fmt.Sprintf("deploy-%04d", i), Status: status,
		})
	}
	env.provider.DeploymentsTotal = 1500

	rr := env.do(http.MethodGet, "/api/deployments?p=1&page_size=20", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body deployListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.SearchTruncated {
		t.Fatal("1500 条超出 1000 上限，search_truncated 必须为 true（R-M6D-14）")
	}
	if body.Total != deployScanMaxItems {
		t.Fatalf("total = %d，期望扫描上限 %d", body.Total, deployScanMaxItems)
	}
	if len(body.Items) != 20 {
		t.Fatalf("items = %d，期望 20", len(body.Items))
	}
	if body.StatusCounts["all"] != int64(deployScanMaxItems) {
		t.Fatalf("status_counts.all = %d", body.StatusCounts["all"])
	}
	if body.StatusCounts["running"]+body.StatusCounts["completed"] != int64(deployScanMaxItems) {
		t.Fatalf("status_counts 总和与扫描量不一致: %+v", body.StatusCounts)
	}
	if env.provider.DeploymentsCalls != deployScanMaxPages {
		t.Fatalf("扫描页数 = %d，期望 %d", env.provider.DeploymentsCalls, deployScanMaxPages)
	}
}

func TestDeployListKeywordFiltersAcrossAllPages(t *testing.T) {
	env := newDeployMgmtEnv(t)
	for i := 0; i < 300; i++ {
		name := fmt.Sprintf("deploy-%03d", i)
		if i == 250 {
			name = "target-model"
		}
		env.provider.Deployments = append(env.provider.Deployments, ionet.DeploymentSummary{
			ID: fmt.Sprintf("dep-%03d", i), Name: name, Status: "running",
		})
	}
	env.provider.DeploymentsTotal = 300

	rr := env.do(http.MethodGet, "/api/deployments?keyword=TARGET&p=1&page_size=20", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body deployListResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.Total != 1 || len(body.Items) != 1 || body.Items[0].ID != "dep-250" {
		t.Fatalf("keyword 必须作用于全结果集（命中第 3 页）: %+v", body)
	}
	if body.SearchTruncated {
		t.Fatal("300 条未超上限，不应标记截断")
	}
}

func TestDeployListPageSizeClamped(t *testing.T) {
	env := newDeployMgmtEnv(t)
	rr := env.do(http.MethodGet, "/api/deployments?page_size=500", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var body deployListResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.PageSize != deployListMaxRaw {
		t.Fatalf("page_size = %d，期望截断为 %d", body.PageSize, deployListMaxRaw)
	}
}

// ---------- 创建：名称查重 / 报价门 / 幂等 ----------

func validCreateBody() string {
	return `{"name":"my-deploy","hardware_id":7,"location_ids":[1,2],"gpus_per_container":1,` +
		`"replica_count":2,"duration_hours":24,"image_url":"https://user:pass@registry.example/img:tag?token=abc",` +
		`"registry_username":"robot","registry_secret":"s3cr3t-value",` +
		`"secret_env_variables":{"DB_PASSWORD":"hunter2"}}`
}

func TestDeployCreateSuccessChecksNameTwiceAndPriceFirst(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.provider.CreateResult = ionet.DeploymentAction{DeploymentID: "dep-new", Status: "deployment requested"}
	env.provider.Price = ionet.PriceEstimationResponse{EstimatedCost: ionet.Money{Decimal: decimal.RequireFromString("12.5")}, Currency: "USDC"}

	rr := env.do(http.MethodPost, "/api/deployments", validCreateBody(), deployWriteHeaders())
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body struct {
		DeploymentID string `json:"deployment_id"`
		Status       string `json:"status"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.DeploymentID != "dep-new" || body.Status != "deployment requested" {
		t.Fatalf("response = %+v", body)
	}
	if env.provider.NameCalls != 2 {
		t.Fatalf("名称查重次数 = %d，期望创建前 + 提交前各一次", env.provider.NameCalls)
	}
	if env.provider.PriceCalls != 1 || env.provider.CreateCalls != 1 {
		t.Fatalf("price/create calls = %d/%d", env.provider.PriceCalls, env.provider.CreateCalls)
	}
	if env.provider.LastCreateRequest.RegistrySecret != "s3cr3t-value" {
		t.Fatal("仓库密钥必须透传给 provider")
	}

	// 审计：镜像 URL 剥离 userinfo/query；秘密值与环境变量键名不出现。
	rows := env.audit.snapshot()
	if len(rows) != 1 {
		t.Fatalf("审计行数 = %d", len(rows))
	}
	detail := stringifyAuditRow(rows[0])
	for _, forbidden := range []string{"s3cr3t-value", "hunter2", "DB_PASSWORD", "user:pass", "token=abc"} {
		if strings.Contains(detail, forbidden) {
			t.Fatalf("审计 detail 泄漏了 %q: %s", forbidden, detail)
		}
	}
	if !strings.Contains(detail, "registry.example/img:tag") {
		t.Fatalf("审计应记录脱敏后的 image_ref: %s", detail)
	}
}

func TestDeployCreatePriceFailureDoesNotWrite(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.provider.PriceErr = errors.New("pricing unavailable")

	rr := env.do(http.MethodPost, "/api/deployments", validCreateBody(), deployWriteHeaders())
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
	}
	if got := decodeDeployProblem(t, rr)["type"]; got != problemTypeDeployPriceUnavailable {
		t.Fatalf("problem.type = %v", got)
	}
	if env.provider.CreateCalls != 0 {
		t.Fatalf("报价失败不得发起 provider 写: %d", env.provider.CreateCalls)
	}
}

func TestDeployCreateNameTaken(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.provider.NameAvailable = false
	rr := env.do(http.MethodPost, "/api/deployments", validCreateBody(), deployWriteHeaders())
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d，期望 409", rr.Code)
	}
	if got := decodeDeployProblem(t, rr)["type"]; got != problemTypeDeployNameTaken {
		t.Fatalf("problem.type = %v", got)
	}
	if env.provider.CreateCalls != 0 || env.provider.PriceCalls != 0 {
		t.Fatal("名称被占用时不得报价/写入")
	}
}

func TestDeployWriteRequiresIdempotencyKey(t *testing.T) {
	env := newDeployMgmtEnv(t)
	rr := env.do(http.MethodPost, "/api/deployments", validCreateBody(), nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d，期望 400", rr.Code)
	}
	if env.provider.CreateCalls != 0 {
		t.Fatal("缺 Idempotency-Key 不得执行写")
	}
}

func TestDeployCreateIdempotentReplay(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.provider.CreateResult = ionet.DeploymentAction{DeploymentID: "dep-1", Status: "requested"}

	first := env.do(http.MethodPost, "/api/deployments", validCreateBody(), deployWriteHeaders())
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d", first.Code)
	}
	second := env.do(http.MethodPost, "/api/deployments", validCreateBody(), deployWriteHeaders())
	if second.Code != http.StatusAccepted {
		t.Fatalf("replay status = %d", second.Code)
	}
	if env.provider.CreateCalls != 1 || env.provider.PriceCalls != 1 {
		t.Fatalf("同 key 重放不得重复执行: create=%d price=%d", env.provider.CreateCalls, env.provider.PriceCalls)
	}
	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("重放必须标记 Idempotent-Replayed")
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("重放响应不一致: %q vs %q", first.Body.String(), second.Body.String())
	}
}

func TestDeployCreateInFlight(t *testing.T) {
	env := newDeployMgmtEnvWithIdem(t, inFlightIdemStore{})
	rr := env.do(http.MethodPost, "/api/deployments", validCreateBody(), deployWriteHeaders())
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d，期望 409", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("在途冲突必须带 Retry-After")
	}
	if env.provider.CreateCalls != 0 {
		t.Fatal("在途请求不得执行 provider 写")
	}
}

// ---------- provider 错误映射 ----------

func TestDeployCreateProviderErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantType   string
	}{
		{"not found", &ionet.APIError{StatusCode: http.StatusNotFound}, http.StatusNotFound, problemTypeDeployNotFound},
		{"name taken (create 409)", &ionet.APIError{StatusCode: http.StatusConflict}, http.StatusConflict, problemTypeDeployNameTaken},
		{"secret invalid", &ionet.APIError{StatusCode: http.StatusUnauthorized}, http.StatusUnprocessableEntity, problemTypeDeploySecretInvalid},
		{"rate limited", &ionet.APIError{StatusCode: http.StatusTooManyRequests, RetryAfter: 7 * time.Second}, http.StatusTooManyRequests, problemTypeDeployRateLimited},
		{"provider error", &ionet.APIError{StatusCode: http.StatusInternalServerError}, http.StatusBadGateway, problemTypeDeployProviderError},
		{"timeout", context.DeadlineExceeded, http.StatusGatewayTimeout, problemTypeDeployProviderTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newDeployMgmtEnv(t)
			env.provider.CreateErr = tc.err
			rr := env.do(http.MethodPost, "/api/deployments", validCreateBody(), deployWriteHeaders())
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d，期望 %d（body=%s）", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if got := decodeDeployProblem(t, rr)["type"]; got != tc.wantType {
				t.Fatalf("problem.type = %v，期望 %s", got, tc.wantType)
			}
			if tc.wantStatus == http.StatusTooManyRequests && rr.Header().Get("Retry-After") != "7" {
				t.Fatalf("Retry-After = %q，期望 7", rr.Header().Get("Retry-After"))
			}
			// 写操作只发送一次：任何错误都不得自动重试（§3.20.7）。
			if env.provider.CreateCalls != 1 {
				t.Fatalf("provider 写调用 = %d，期望 1（不得自动重试）", env.provider.CreateCalls)
			}
		})
	}
}

func TestDeployTimeoutIsNotCachedAndNotRetried(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.provider.CreateErr = context.DeadlineExceeded

	first := env.do(http.MethodPost, "/api/deployments", validCreateBody(), deployWriteHeaders())
	if first.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d，期望 504", first.Code)
	}
	if env.provider.CreateCalls != 1 {
		t.Fatalf("写调用 = %d，期望 1（不自动重试）", env.provider.CreateCalls)
	}
	// 504「结果未知」不得被固化：同 key 再次提交必须重新执行（管理员应先刷新，
	// 这里是网关侧的语义断言：缓存里没有成功结果）。
	second := env.do(http.MethodPost, "/api/deployments", validCreateBody(), deployWriteHeaders())
	if second.Code != http.StatusGatewayTimeout {
		t.Fatalf("second status = %d", second.Code)
	}
	if env.provider.CreateCalls != 2 {
		t.Fatalf("超时不得固化成功结果: calls = %d", env.provider.CreateCalls)
	}
}

// ---------- 更新：秘密语义与审计 ----------

func TestDeployUpdateSecretStaysOutOfResponseAndAudit(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.provider.UpdateResult = ionet.DeploymentAction{DeploymentID: "dep-1", Status: "updating"}
	env.provider.DeploymentDetail = ionet.DeploymentDetail{
		ID: "dep-1", ContainerConfig: ionet.DeploymentContainerConfig{ImageURL: "https://old.example/img"},
	}
	body := `{"image_url":"https://new.example/img","registry_secret":"new-s3cr3t",` +
		`"secret_env_variables":{"API_TOKEN":"tok-value"},"traffic_port":8080}`

	rr := env.do(http.MethodPatch, "/api/deployments/dep-1", body, deployWriteHeaders())
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "new-s3cr3t") || strings.Contains(rr.Body.String(), "tok-value") {
		t.Fatalf("响应泄漏秘密: %s", rr.Body.String())
	}
	if env.provider.LastUpdateRequest.RegistrySecret == nil ||
		*env.provider.LastUpdateRequest.RegistrySecret != "new-s3cr3t" {
		t.Fatal("仓库密钥必须透传 provider")
	}
	if env.provider.LastUpdateRequest.SecretEnvVariables == nil ||
		(*env.provider.LastUpdateRequest.SecretEnvVariables)["API_TOKEN"] != "tok-value" {
		t.Fatal("秘密环境变量必须透传 provider")
	}

	rows := env.audit.snapshot()
	if len(rows) == 0 {
		t.Fatal("更新必须写审计")
	}
	for _, row := range rows {
		detail := stringifyAuditRow(row)
		for _, forbidden := range []string{"new-s3cr3t", "tok-value", "API_TOKEN"} {
			if strings.Contains(detail, forbidden) {
				t.Fatalf("审计泄漏秘密 %q: %s", forbidden, detail)
			}
		}
		if !strings.Contains(detail, "image_url") {
			t.Fatalf("审计应记录 changed_fields: %s", detail)
		}
	}
}

func TestDeployUpdateBlankSecretDoesNotUpdate(t *testing.T) {
	env := newDeployMgmtEnv(t)
	rr := env.do(http.MethodPatch, "/api/deployments/dep-1",
		`{"registry_secret":"","secret_env_variables":{}}`, deployWriteHeaders())
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if env.provider.LastUpdateRequest.RegistrySecret != nil {
		t.Fatal("留空 = 不更新，不得把空值当清除")
	}
	if env.provider.LastUpdateRequest.SecretEnvVariables != nil {
		t.Fatal("空对象 = 不更新")
	}
}

func TestDeployUpdateClearFlagsPassThrough(t *testing.T) {
	env := newDeployMgmtEnv(t)
	rr := env.do(http.MethodPatch, "/api/deployments/dep-1",
		`{"clear_registry_secret":true,"clear_secret_env":true}`, deployWriteHeaders())
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	req := env.provider.LastUpdateRequest
	if !req.ClearRegistrySecret || !req.ClearSecretEnv {
		t.Fatalf("显式清除位未透传: %+v", req)
	}
}

// ---------- 改名 / 续期 / 终止 ----------

func TestDeployRenameChecksName(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.provider.Deployments = []ionet.DeploymentSummary{{ID: "dep-1", Name: "old-name"}}
	env.provider.RenameResult = ionet.DeploymentAction{DeploymentID: "dep-1", Status: "renamed"}

	rr := env.do(http.MethodPatch, "/api/deployments/dep-1/name", `{"name":"new-name"}`, deployWriteHeaders())
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if env.provider.LastRenameName != "new-name" {
		t.Fatal("改名未透传")
	}
	rows := env.audit.snapshot()
	detail := stringifyAuditRow(rows[len(rows)-1])
	if !strings.Contains(detail, "old-name") || !strings.Contains(detail, "new-name") {
		t.Fatalf("改名审计必须记录 old/new: %s", detail)
	}

	env.provider.NameAvailable = false
	rr = env.do(http.MethodPatch, "/api/deployments/dep-1/name", `{"name":"taken"}`,
		map[string]string{"Idempotency-Key": "idem-key-2"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("名称占用 = %d，期望 409", rr.Code)
	}
}

func TestDeployExtendRequiresPriceAndWrites(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.provider.DeploymentDetail = ionet.DeploymentDetail{
		ID: "dep-1", HardwareID: 7, GPUsPerContainer: 1, TotalContainers: 2,
		Locations: []ionet.DeploymentLocation{{ID: 1}, {ID: 2}},
	}
	env.provider.Price = ionet.PriceEstimationResponse{EstimatedCost: ionet.Money{Decimal: decimal.RequireFromString("3.5")}, Currency: "USDC"}
	env.provider.ExtendResult = ionet.DeploymentAction{DeploymentID: "dep-1", Status: "extending"}

	rr := env.do(http.MethodPost, "/api/deployments/dep-1/extend", `{"duration_hours":12}`, deployWriteHeaders())
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if env.provider.ExtendCalls != 1 || env.provider.LastExtendHours != 12 {
		t.Fatalf("extend calls/hours = %d/%d", env.provider.ExtendCalls, env.provider.LastExtendHours)
	}
	if env.provider.LastPriceRequest.DurationHours != 12 ||
		env.provider.LastPriceRequest.HardwareID != 7 ||
		env.provider.LastPriceRequest.ReplicaCount != 2 {
		t.Fatalf("续期报价参数 = %+v", env.provider.LastPriceRequest)
	}

	env.provider.PriceErr = errors.New("no price")
	env.provider.ExtendCalls = 0
	rr = env.do(http.MethodPost, "/api/deployments/dep-1/extend", `{"duration_hours":12}`,
		map[string]string{"Idempotency-Key": "idem-key-3"})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("报价失败 = %d，期望 422", rr.Code)
	}
	if env.provider.ExtendCalls != 0 {
		t.Fatal("报价失败不得续期")
	}
}

func TestDeployDeleteAccepted(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.provider.DeleteResult = ionet.DeploymentAction{DeploymentID: "dep-1", Status: "termination requested"}
	rr := env.do(http.MethodDelete, "/api/deployments/dep-1", "", deployWriteHeaders())
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if env.provider.LastDeleteID != "dep-1" {
		t.Fatal("删除未透传 ID")
	}
}

// ---------- 只读：详情 / 容器 / 日志 / 目录 / 价格 ----------

func TestDeployDetailHasNoSecretFields(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.provider.DeploymentDetail = ionet.DeploymentDetail{
		ID: "dep-1", Status: "running", TotalContainers: 2,
		ContainerConfig: ionet.DeploymentContainerConfig{
			ImageURL: "https://registry.example/img", TrafficPort: 8000,
			EnvVariables: map[string]any{"PLAIN": "value"},
		},
	}
	rr := env.do(http.MethodGet, "/api/deployments/dep-1", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, forbidden := range []string{"registry_secret", "secret_env", "api_key"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("详情泄漏秘密面 %q: %s", forbidden, body)
		}
	}
}

func TestDeployContainersAndContainerDetail(t *testing.T) {
	env := newDeployMgmtEnv(t)
	created := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	env.provider.Containers = ionet.ContainerList{
		Total: 1,
		Containers: []ionet.Container{{
			ContainerID: "ctr-1", Status: "running", GPUsPerContainer: 1,
			CreatedAt: &created, PublicURL: "https://abc.example",
		}},
	}
	rr := env.do(http.MethodGet, "/api/deployments/dep-1/containers", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"runtime_seconds":7200`) {
		t.Fatalf("运行时长应由 created_at 推导: %s", rr.Body.String())
	}

	env.provider.Container = ionet.Container{ContainerID: "ctr-1", Status: "running"}
	rr = env.do(http.MethodGet, "/api/deployments/dep-1/containers/ctr-1", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("container status = %d", rr.Code)
	}
	if env.provider.LastContainerLookup != [2]string{"dep-1", "ctr-1"} {
		t.Fatalf("容器查找参数 = %v", env.provider.LastContainerLookup)
	}
}

func TestDeployLogsMapsCursorAndClampsLimit(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.provider.Logs = ionet.LogsPage{
		Lines: []string{"line-1", "line-2"}, NextPageToken: "cursor-2", HasMore: true,
	}
	rr := env.do(http.MethodGet,
		"/api/deployments/dep-1/logs?container_id=ctr-1&page_token=cursor-1&limit=5000&level=error&stream=stderr", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if env.provider.LastLogsOptions.PageToken != "cursor-1" {
		t.Fatalf("page_token 应映射为 provider cursor: %q", env.provider.LastLogsOptions.PageToken)
	}
	if env.provider.LastLogsOptions.Limit != deployLogsMaxLimit {
		t.Fatalf("limit = %d，期望上限 %d", env.provider.LastLogsOptions.Limit, deployLogsMaxLimit)
	}
	var body struct {
		Logs          []string `json:"logs"`
		NextPageToken string   `json:"next_page_token"`
		HasMore       bool     `json:"has_more"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if len(body.Logs) != 2 || body.NextPageToken != "cursor-2" || !body.HasMore {
		t.Fatalf("logs response = %+v", body)
	}

	rr = env.do(http.MethodGet, "/api/deployments/dep-1/logs", "", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 container_id = %d，期望 400", rr.Code)
	}
}

func TestDeployCatalogAndPriceEndpoints(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.provider.Hardware = ionet.HardwareTypesResponse{
		HardwareTypes: []ionet.HardwareType{{ID: 7, Name: "A100", AvailableReplicas: 3}},
		Total:         1, TotalAvailable: 3,
	}
	env.provider.Locations = ionet.LocationsResponse{
		Locations: []ionet.Location{{ID: 1, Name: "us-east"}}, Total: 1,
	}
	env.provider.Replicas = ionet.AvailableReplicas{Replicas: []ionet.AvailableReplica{{LocationID: 1, AvailableCount: 3}}}
	env.provider.Price = ionet.PriceEstimationResponse{EstimatedCost: ionet.Money{Decimal: decimal.RequireFromString("9.99")}, Currency: "USDC"}

	for _, target := range []string{
		"/api/deployments/hardware-types",
		"/api/deployments/locations",
		"/api/deployments/available-replicas?hardware_id=7&gpu_count=2",
		"/api/deployments/check-name?name=my-deploy",
	} {
		rr := env.do(http.MethodGet, target, "", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s = %d（body=%s）", target, rr.Code, rr.Body.String())
		}
	}
	if env.provider.LastAvailableID != 7 || env.provider.LastAvailableGPU != 2 {
		t.Fatalf("副本余量参数 = %d/%d", env.provider.LastAvailableID, env.provider.LastAvailableGPU)
	}

	rr := env.do(http.MethodPost, "/api/deployments/price-estimation",
		`{"hardware_id":7,"location_ids":[1],"gpus_per_container":1,"replica_count":1,"duration_hours":24}`,
		map[string]string{"Idempotency-Key": "ignored-for-post-price"})
	if rr.Code != http.StatusOK {
		t.Fatalf("price status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"estimated_cost":9.99`) {
		t.Fatalf("报价金额必须原样返回: %s", rr.Body.String())
	}

	rr = env.do(http.MethodGet, "/api/deployments/available-replicas", "", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 hardware_id = %d，期望 400", rr.Code)
	}
}

func TestDeployCheckNameUnavailableIs200(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.provider.NameAvailable = false
	rr := env.do(http.MethodGet, "/api/deployments/check-name?name=used", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"available":false`) {
		t.Fatalf("body = %s", rr.Body.String())
	}
}

func TestDeployListProviderErrorMaps502(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.provider.DeploymentsErr = &ionet.APIError{StatusCode: http.StatusBadGateway}
	rr := env.do(http.MethodGet, "/api/deployments", "", nil)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d，期望 502", rr.Code)
	}
}

// stringifyAuditRow 把审计写入参数整体转成字符串，用于「秘密不得出现」断言。
func stringifyAuditRow(row []any) string {
	var builder strings.Builder
	for _, arg := range row {
		switch value := arg.(type) {
		case []byte:
			builder.Write(value)
		case string:
			builder.WriteString(value)
		default:
			fmt.Fprint(&builder, value)
		}
		builder.WriteString(" | ")
	}
	return builder.String()
}

// ---------- 复核收口：无 total 的全量扫描（HIGH-1） ----------

func TestDeployListKeywordScansPastFirstPageWithoutTotal(t *testing.T) {
	env := newDeployMgmtEnv(t)
	for i := 0; i < 300; i++ {
		name := fmt.Sprintf("deploy-%03d", i)
		if i == 250 {
			name = "target-model"
		}
		env.provider.Deployments = append(env.provider.Deployments, ionet.DeploymentSummary{
			ID: fmt.Sprintf("dep-%03d", i), Name: name, Status: "running",
		})
	}
	// 模拟 provider 不返回 total：扫描只能靠「本页不足一页」终止。
	env.provider.OmitDeploymentsTotal = true

	rr := env.do(http.MethodGet, "/api/deployments?keyword=target", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body deployListResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.Total != 1 || len(body.Items) != 1 || body.Items[0].ID != "dep-250" {
		t.Fatalf("缺 total 时必须继续扫描到第 3 页: %+v", body)
	}
	if body.SearchTruncated {
		t.Fatal("300 条可完整取全（第 4 页为空），不应标记截断")
	}
	if env.provider.DeploymentsCalls != 4 {
		t.Fatalf("扫描调用 = %d，期望 4（3 满页 + 1 空页）", env.provider.DeploymentsCalls)
	}
}

func TestDeployListWithoutTotalTruncatesAtScanLimit(t *testing.T) {
	env := newDeployMgmtEnv(t)
	for i := 0; i < 1500; i++ {
		env.provider.Deployments = append(env.provider.Deployments, ionet.DeploymentSummary{
			ID: fmt.Sprintf("dep-%04d", i), Name: fmt.Sprintf("deploy-%04d", i), Status: "running",
		})
	}
	env.provider.OmitDeploymentsTotal = true

	rr := env.do(http.MethodGet, "/api/deployments", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var body deployListResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if !body.SearchTruncated {
		t.Fatal("缺 total 且扫满上限时必须标记 search_truncated")
	}
	if body.Total != deployScanMaxItems {
		t.Fatalf("total = %d，期望已扫描窗口 %d", body.Total, deployScanMaxItems)
	}
}

// ---------- 复核收口：写操作幂等矩阵（LOW-3） ----------

func TestDeployWriteIdempotencyMatrix(t *testing.T) {
	type writeCase struct {
		name   string
		method string
		target string
		body   string
		calls  func(*ionet.FakeProvider) int
	}
	cases := []writeCase{
		{"update", http.MethodPatch, "/api/deployments/dep-1", `{"traffic_port":8080}`,
			func(p *ionet.FakeProvider) int { return p.UpdateCalls }},
		{"rename", http.MethodPatch, "/api/deployments/dep-1/name", `{"name":"new-name"}`,
			func(p *ionet.FakeProvider) int { return p.RenameCalls }},
		{"extend", http.MethodPost, "/api/deployments/dep-1/extend", `{"duration_hours":6}`,
			func(p *ionet.FakeProvider) int { return p.ExtendCalls }},
		{"delete", http.MethodDelete, "/api/deployments/dep-1", ``,
			func(p *ionet.FakeProvider) int { return p.DeleteCalls }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newDeployMgmtEnv(t)
			env.provider.DeploymentDetail = ionet.DeploymentDetail{
				ID: "dep-1", HardwareID: 7, GPUsPerContainer: 1, TotalContainers: 1,
				Locations: []ionet.DeploymentLocation{{ID: 1}},
			}
			env.provider.Price = ionet.PriceEstimationResponse{EstimatedCost: ionet.Money{Decimal: decimal.NewFromInt(1)}, Currency: "USDC"}

			missing := env.do(tc.method, tc.target, tc.body, nil)
			if missing.Code != http.StatusBadRequest {
				t.Fatalf("缺 key = %d，期望 400", missing.Code)
			}
			if got := tc.calls(env.provider); got != 0 {
				t.Fatalf("缺 key 不得执行写: calls=%d", got)
			}

			first := env.do(tc.method, tc.target, tc.body, deployWriteHeaders())
			if first.Code < 200 || first.Code >= 300 {
				t.Fatalf("first status = %d（body=%s）", first.Code, first.Body.String())
			}
			second := env.do(tc.method, tc.target, tc.body, deployWriteHeaders())
			if second.Code != first.Code {
				t.Fatalf("replay status = %d，期望 %d", second.Code, first.Code)
			}
			if got := tc.calls(env.provider); got != 1 {
				t.Fatalf("同 key 重放不得重复执行: calls=%d", got)
			}
			if second.Header().Get("Idempotent-Replayed") != "true" {
				t.Fatal("重放必须标记 Idempotent-Replayed")
			}
		})
	}
}

func TestDeployWriteInFlightMatrix(t *testing.T) {
	for _, tc := range []struct{ method, target, body string }{
		{http.MethodPatch, "/api/deployments/dep-1", `{"traffic_port":8080}`},
		{http.MethodPatch, "/api/deployments/dep-1/name", `{"name":"x"}`},
		{http.MethodPost, "/api/deployments/dep-1/extend", `{"duration_hours":6}`},
		{http.MethodDelete, "/api/deployments/dep-1", ``},
	} {
		env := newDeployMgmtEnvWithIdem(t, inFlightIdemStore{})
		rr := env.do(tc.method, tc.target, tc.body, deployWriteHeaders())
		if rr.Code != http.StatusConflict {
			t.Fatalf("%s %s = %d，期望 409", tc.method, tc.target, rr.Code)
		}
		if rr.Header().Get("Retry-After") == "" {
			t.Fatalf("%s 在途冲突缺少 Retry-After", tc.target)
		}
	}
}

// ---------- 复核收口：禁用门/权限覆盖全部端点（LOW-3） ----------

func TestDeployAllEndpointsDisabledGate(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.service.getView = sampleDeployView(false, true)
	endpoints := []struct{ method, target, body string }{
		{http.MethodGet, "/api/deployments", ""},
		{http.MethodGet, "/api/deployments/hardware-types", ""},
		{http.MethodGet, "/api/deployments/locations", ""},
		{http.MethodGet, "/api/deployments/available-replicas?hardware_id=1", ""},
		{http.MethodPost, "/api/deployments/price-estimation",
			`{"hardware_id":1,"location_ids":[1],"gpus_per_container":1,"replica_count":1,"duration_hours":1}`},
		{http.MethodGet, "/api/deployments/check-name?name=x", ""},
		{http.MethodPost, "/api/deployments", validCreateBody()},
		{http.MethodGet, "/api/deployments/dep-1", ""},
		{http.MethodGet, "/api/deployments/dep-1/containers", ""},
		{http.MethodGet, "/api/deployments/dep-1/containers/ctr-1", ""},
		{http.MethodGet, "/api/deployments/dep-1/logs?container_id=ctr-1", ""},
		{http.MethodPatch, "/api/deployments/dep-1", `{"traffic_port":8080}`},
		{http.MethodPatch, "/api/deployments/dep-1/name", `{"name":"x"}`},
		{http.MethodPost, "/api/deployments/dep-1/extend", `{"duration_hours":1}`},
		{http.MethodDelete, "/api/deployments/dep-1", ""},
	}
	for _, ep := range endpoints {
		headers := map[string]string{}
		if ep.method != http.MethodGet {
			headers["Idempotency-Key"] = "k-" + ep.method + ep.target
		}
		rr := env.do(ep.method, ep.target, ep.body, headers)
		if rr.Code != http.StatusConflict {
			t.Fatalf("%s %s = %d，期望 409（body=%s）", ep.method, ep.target, rr.Code, rr.Body.String())
		}
	}
}

func TestDeployAllEndpointsRequireSettingsManage(t *testing.T) {
	env := newDeployMgmtEnv(t)
	noPerm := &auth.Principal{UserID: 2, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermUsersRead}}
	endpoints := []struct{ method, target, body string }{
		{http.MethodGet, "/api/deployments", ""},
		{http.MethodGet, "/api/deployments/hardware-types", ""},
		{http.MethodGet, "/api/deployments/locations", ""},
		{http.MethodGet, "/api/deployments/available-replicas?hardware_id=1", ""},
		{http.MethodPost, "/api/deployments/price-estimation", `{}`},
		{http.MethodGet, "/api/deployments/check-name?name=x", ""},
		{http.MethodPost, "/api/deployments", `{}`},
		{http.MethodGet, "/api/deployments/dep-1", ""},
		{http.MethodGet, "/api/deployments/dep-1/containers", ""},
		{http.MethodGet, "/api/deployments/dep-1/containers/ctr-1", ""},
		{http.MethodGet, "/api/deployments/dep-1/logs?container_id=ctr-1", ""},
		{http.MethodPatch, "/api/deployments/dep-1", `{}`},
		{http.MethodPatch, "/api/deployments/dep-1/name", `{}`},
		{http.MethodPost, "/api/deployments/dep-1/extend", `{}`},
		{http.MethodDelete, "/api/deployments/dep-1", ""},
	}
	for _, ep := range endpoints {
		rr := httptest.NewRecorder()
		env.mux.ServeHTTP(rr, settingsRequest(ep.method, ep.target, ep.body, noPerm, nil))
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s %s = %d，期望 403", ep.method, ep.target, rr.Code)
		}
	}
}

// ---------- 复核收口：409 按操作区分（LOW-1）与失败审计（LOW-2） ----------

func TestDeployNameConflictMapsToNameTaken(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.provider.Price = ionet.PriceEstimationResponse{EstimatedCost: ionet.Money{Decimal: decimal.NewFromInt(1)}, Currency: "USDC"}
	env.provider.CreateErr = &ionet.APIError{StatusCode: http.StatusConflict}
	rr := env.do(http.MethodPost, "/api/deployments", validCreateBody(), deployWriteHeaders())
	if rr.Code != http.StatusConflict {
		t.Fatalf("create status = %d", rr.Code)
	}
	if got := decodeDeployProblem(t, rr)["type"]; got != problemTypeDeployNameTaken {
		t.Fatalf("create 409 problem.type = %v，期望 name_taken", got)
	}
	if !strings.Contains(stringifyAuditRow(env.audit.snapshot()[0]), "deploy_name_taken") {
		t.Fatal("名称冲突失败必须记审计 error_code")
	}

	env2 := newDeployMgmtEnv(t)
	env2.provider.RenameErr = &ionet.APIError{StatusCode: http.StatusConflict}
	rr = env2.do(http.MethodPatch, "/api/deployments/dep-1/name", `{"name":"taken"}`, deployWriteHeaders())
	if rr.Code != http.StatusConflict {
		t.Fatalf("rename status = %d", rr.Code)
	}
	if got := decodeDeployProblem(t, rr)["type"]; got != problemTypeDeployNameTaken {
		t.Fatalf("rename 409 problem.type = %v，期望 name_taken", got)
	}
}

func TestDeployRenameNameTakenIsAudited(t *testing.T) {
	env := newDeployMgmtEnv(t)
	env.provider.NameAvailable = false
	rr := env.do(http.MethodPatch, "/api/deployments/dep-1/name", `{"name":"taken"}`, deployWriteHeaders())
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d", rr.Code)
	}
	rows := env.audit.snapshot()
	if len(rows) != 1 {
		t.Fatalf("审计行数 = %d", len(rows))
	}
	if !strings.Contains(stringifyAuditRow(rows[0]), "deploy_name_taken") {
		t.Fatalf("名称占用失败必须审计: %s", stringifyAuditRow(rows[0]))
	}
}
