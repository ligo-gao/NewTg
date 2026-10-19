package ionet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件锁定新增 provider 方法的 HTTP 契约：固定路径、方法、请求体形状、
// 写操作不重试、秘密值从错误正文中抹除，以及 adapter 对两种响应形状
// （直接 / {"data":...}）与纯文本日志的容忍。真实响应字段仍待实测（R-M6D-13）。

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(Config{APIKey: "test-key", BaseURL: srv.URL, Timeout: 2 * time.Second})
}

func TestDeploymentClientPathsAndMethods(t *testing.T) {
	var calls []string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == PathDeployments && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":{"deployments":[{"id":"d1","name":"n1","status":"Running"}],"total":1}}`))
		case r.URL.Path == PathDeployments && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"deployment_id":"d1","status":"requested"}`))
		case r.URL.Path == deploymentPath("d1") && r.Method == http.MethodPatch:
			_, _ = w.Write([]byte(`{"deployment_id":"d1","status":"updated"}`))
		case r.URL.Path == deploymentPath("d1") && r.Method == http.MethodDelete:
			_, _ = w.Write([]byte(`{"status":"terminated"}`))
		case r.URL.Path == deploymentPath("d1")+"/extend":
			_, _ = w.Write([]byte(`{"id":"d1","status":"extended"}`))
		case r.URL.Path == apiBasePath+"/clusters/d1/update-name":
			_, _ = w.Write([]byte(`{"status":"renamed"}`))
		case r.URL.Path == PathCheckName:
			_, _ = w.Write([]byte(`true`))
		case r.URL.Path == PathPriceEstimation:
			_, _ = w.Write([]byte(`{"data":{"total_cost_usdc":4.25,"ionet_fee":0.25}}`))
		case r.URL.Path == PathAvailableReplicas:
			_, _ = w.Write([]byte(`{"data":[{"id":1,"name":"us","available_replicas":3}]}`))
		case r.URL.Path == deploymentPath("d1"):
			_, _ = w.Write([]byte(`{"id":"d1","status":"running","created_at":"2026-10-20T10:00:00"}`))
		case r.URL.Path == deploymentPath("d1")+"/containers":
			_, _ = w.Write([]byte(`{"data":{"total":1,"workers":[{"container_id":"c1","status":"running"}]}}`))
		case r.URL.Path == deploymentPath("d1")+"/container/c1":
			_, _ = w.Write([]byte(`{"container_id":"c1","status":"running"}`))
		case strings.HasPrefix(r.URL.Path, publicAPIPath+"/deployment/d1/log/"):
			_, _ = w.Write([]byte("line one\n\nline two\n"))
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()

	page, err := client.ListDeployments(ctx, DeploymentListOptions{Page: 2, PageSize: 50, Status: "running"})
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Status != "running" || page.Total != 1 {
		t.Fatalf("page = %+v", page)
	}

	if _, err := client.CreateDeployment(ctx, CreateDeploymentRequest{
		Name: "n1", HardwareID: 1, LocationIDs: []int{1}, GPUsPerContainer: 1,
		ReplicaCount: 1, DurationHours: 1, ImageURL: "https://img",
	}); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if _, err := client.UpdateDeployment(ctx, "d1", UpdateDeploymentRequest{}); err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}
	if action, err := client.ExtendDeployment(ctx, "d1", 12); err != nil || action.DeploymentID != "d1" {
		t.Fatalf("ExtendDeployment = %+v, %v", action, err)
	}
	if _, err := client.RenameDeployment(ctx, "d1", "new"); err != nil {
		t.Fatalf("RenameDeployment: %v", err)
	}
	if action, err := client.DeleteDeployment(ctx, "d1"); err != nil || action.Status != "terminated" {
		t.Fatalf("DeleteDeployment = %+v, %v", action, err)
	}
	if available, err := client.CheckName(ctx, "n1"); err != nil || !available {
		t.Fatalf("CheckName = %v, %v", available, err)
	}
	price, err := client.EstimatePrice(ctx, PriceEstimationRequest{
		HardwareID: 1, LocationIDs: []int{1}, GPUsPerContainer: 1, ReplicaCount: 1, DurationHours: 24,
	})
	if err != nil {
		t.Fatalf("EstimatePrice: %v", err)
	}
	if price.EstimatedCost != 4.25 || price.Currency != "USDC" || price.PriceBreakdown.IONetFee == nil {
		t.Fatalf("price = %+v", price)
	}
	replicas, err := client.AvailableReplicas(ctx, 1, 2)
	if err != nil || len(replicas.Replicas) != 1 || replicas.Replicas[0].AvailableCount != 3 {
		t.Fatalf("AvailableReplicas = %+v, %v", replicas, err)
	}
	detail, err := client.GetDeployment(ctx, "d1")
	if err != nil || detail.ID != "d1" {
		t.Fatalf("GetDeployment = %+v, %v", detail, err)
	}
	if detail.CreatedAt == nil {
		t.Fatal("无时区时间字符串必须被解析")
	}
	containers, err := client.ListContainers(ctx, "d1")
	if err != nil || len(containers.Containers) != 1 {
		t.Fatalf("ListContainers = %+v, %v", containers, err)
	}
	if _, err := client.GetContainer(ctx, "d1", "c1"); err != nil {
		t.Fatalf("GetContainer: %v", err)
	}
	logs, err := client.GetLogs(ctx, "d1", LogsOptions{ContainerID: "c1", Limit: 100})
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if len(logs.Lines) != 2 || logs.Lines[1] != "line two" {
		t.Fatalf("logs = %+v", logs)
	}

	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"GET " + PathDeployments,
		"POST " + PathDeployments,
		"PATCH " + deploymentPath("d1"),
		"POST " + deploymentPath("d1") + "/extend",
		"PUT " + apiBasePath + "/clusters/d1/update-name",
		"DELETE " + deploymentPath("d1"),
		"GET " + PathPriceEstimation,
		"GET " + PathAvailableReplicas,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少调用 %s：\n%s", want, joined)
		}
	}
}

func TestDeploymentWriteDoesNotRetryAndRedactsSecrets(t *testing.T) {
	var attempts int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"registry_secret":"top-secret"`) {
			t.Errorf("请求体缺少仓库密钥: %s", body)
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"failed with top-secret and env-value"}`))
	})

	_, err := client.CreateDeployment(context.Background(), CreateDeploymentRequest{
		Name: "n", HardwareID: 1, LocationIDs: []int{1}, GPUsPerContainer: 1,
		ReplicaCount: 1, DurationHours: 1, ImageURL: "https://img",
		RegistrySecret:     "top-secret",
		SecretEnvVariables: map[string]string{"KEY": "env-value"},
	})
	if err == nil {
		t.Fatal("期望 provider 错误")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("写操作尝试次数 = %d，期望 1（不自动重试）", got)
	}
	if strings.Contains(err.Error(), "top-secret") || strings.Contains(err.Error(), "env-value") {
		t.Fatalf("错误消息泄漏秘密: %v", err)
	}
}

func TestCheckNameTreatsHttpErrorAsError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"bad key"}`))
	})
	if _, err := client.CheckName(context.Background(), "x"); err == nil {
		t.Fatal("401 必须映射为错误，而不是 available=false")
	}
	var apiErr *APIError
	if !errors.As(mustCheckNameErr(t, client), &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatal("期望 APIError 401")
	}
}

func mustCheckNameErr(t *testing.T, client *Client) error {
	t.Helper()
	_, err := client.CheckName(context.Background(), "x")
	return err
}

func TestUpdateRequestSerializesClearFlags(t *testing.T) {
	var body map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	_, err := client.UpdateDeployment(context.Background(), "d1", UpdateDeploymentRequest{
		ClearRegistrySecret: true, ClearSecretEnv: true,
	})
	if err != nil {
		t.Fatalf("UpdateDeployment: %v", err)
	}
	if _, ok := body["registry_secret"]; !ok {
		t.Fatal("显式清除必须发送 registry_secret 空值（omitempty 会静默丢掉）")
	}
	if _, ok := body["secret_env_variables"]; !ok {
		t.Fatal("显式清除必须发送 secret_env_variables 空对象")
	}
}

func TestDecodeDeploymentPageKeepsUnknownTotal(t *testing.T) {
	// provider 不返回 total 时必须保留 0：伪造成本页条数会让上层把「只扫了
	// 第一页」误判成「已取全」（复核 HIGH-1）。
	page, err := decodeDeploymentPage([]byte(`{"deployments":[{"id":"d1"},{"id":"d2"}]}`))
	if err != nil {
		t.Fatalf("decodeDeploymentPage: %v", err)
	}
	if page.Total != 0 {
		t.Fatalf("Total = %d，期望 0（未提供）", page.Total)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %d", len(page.Items))
	}

	page, err = decodeDeploymentPage([]byte(`{"data":{"deployments":[{"id":"d1"}],"total":42}}`))
	if err != nil {
		t.Fatalf("decode wrapped: %v", err)
	}
	if page.Total != 42 {
		t.Fatalf("Total = %d，期望 42", page.Total)
	}
}

func TestDecodePriceNestedBreakdown(t *testing.T) {
	response, err := decodePriceEstimation([]byte(`{
		"estimated_cost": 12.5,
		"currency": "usd",
		"estimation_valid": true,
		"price_breakdown": {"compute_cost": 10, "network_cost": 0, "total_cost": 12.5, "hourly_rate": 0.52}
	}`), PriceEstimationRequest{Currency: "usdc"})
	if err != nil {
		t.Fatalf("decodePriceEstimation: %v", err)
	}
	if response.Currency != "USD" || response.EstimatedCost != 12.5 || !response.EstimationValid {
		t.Fatalf("response = %+v", response)
	}
	if response.PriceBreakdown.ComputeCost == nil || *response.PriceBreakdown.ComputeCost != 10 {
		t.Fatal("嵌套 price_breakdown 未映射")
	}
	if response.PriceBreakdown.NetworkCost == nil || *response.PriceBreakdown.NetworkCost != 0 {
		t.Fatal("显式 0 的费用项必须保留（不能用 omitempty 误删）")
	}
	if response.PriceBreakdown.StorageCost != nil {
		t.Fatal("缺失的费用项不得伪造")
	}
}
