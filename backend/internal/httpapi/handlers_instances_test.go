package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
)

// B3 验收（modules-08 §3.7 F-08-21）：
//   - 心跳写入字段、TTL 90s、停机删除；
//   - GET /api/instances 含当前实例、按角色区分、过期实例不返回；
//   - Redis 故障 → 503 固定文案；权限 settings:manage。

type instanceTestEnv struct {
	mux    *http.ServeMux
	mr     *miniredis.Miniredis
	rdb    *redis.Client
	handle *InstanceHandlers
}

func newInstanceTestEnv(t *testing.T, instanceID, role string, authz *rbac.AuthzMiddleware) *instanceTestEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	handlers := NewInstanceHandlers(InstanceHandlerConfig{
		Redis:      rdb,
		InstanceID: instanceID,
		Role:       role,
		Version:    "1.5.0",
		StartedAt:  time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC),
		Authz:      authz,
		Logger:     testLogger(),
	})
	mux := http.NewServeMux()
	handlers.Register(mux)
	return &instanceTestEnv{mux: mux, mr: mr, rdb: rdb, handle: handlers}
}

func instanceManager() *auth.Principal {
	return &auth.Principal{UserID: 1, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
}

func listInstancesRequest(t *testing.T, env *instanceTestEnv, principal *auth.Principal) (*httptest.ResponseRecorder, instanceListResponse) {
	t.Helper()
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/instances", "", principal, nil))
	var body instanceListResponse
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode instances: %v", err)
		}
	}
	return rr, body
}

func TestInstanceHeartbeatWritesTTLAndDeletesOnStop(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	started := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	hb, err := StartInstanceHeartbeat(t.Context(), InstanceHeartbeatConfig{
		Redis:      rdb,
		InstanceID: "node-a",
		Role:       "worker",
		Version:    "1.4.2",
		StartedAt:  started,
		Now:        func() time.Time { return started.Add(5 * time.Second) },
		Logger:     testLogger(),
	})
	if err != nil {
		t.Fatalf("start heartbeat: %v", err)
	}

	key := InstanceHeartbeatKey("node-a")
	values, err := rdb.HGetAll(t.Context(), key).Result()
	if err != nil {
		t.Fatalf("hgetall: %v", err)
	}
	if values["status"] != "active" || values["role"] != "worker" || values["version"] != "1.4.2" {
		t.Fatalf("heartbeat fields = %+v", values)
	}
	if values["started_at"] != started.Format(time.RFC3339Nano) {
		t.Fatalf("started_at = %q", values["started_at"])
	}
	if values["last_seen"] != started.Add(5*time.Second).Format(time.RFC3339Nano) {
		t.Fatalf("last_seen = %q", values["last_seen"])
	}
	if ttl := mr.TTL(key); ttl != InstanceHeartbeatTTL {
		t.Fatalf("ttl = %s，期望 %s", ttl, InstanceHeartbeatTTL)
	}

	hb.Stop()
	if mr.Exists(key) {
		t.Fatal("停机后心跳 key 必须删除")
	}
	// Stop 幂等。
	hb.Stop()
}

func TestInstanceListIncludesCurrentAndBothRoles(t *testing.T) {
	env := newInstanceTestEnv(t, "gateway-1", "gateway", rbac.NewAuthzMiddleware())

	if err := env.rdb.HSet(t.Context(), InstanceHeartbeatKey("worker-1"), map[string]interface{}{
		"status":     "active",
		"role":       "worker",
		"version":    "1.4.1",
		"started_at": "2026-10-20T10:00:00Z",
		"last_seen":  "2026-10-20T10:00:30Z",
	}).Err(); err != nil {
		t.Fatalf("seed worker heartbeat: %v", err)
	}
	// 当前实例没有心跳：仍必须出现在列表里。
	rr, body := listInstancesRequest(t, env, instanceManager())
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if body.CurrentInstanceID != "gateway-1" {
		t.Fatalf("current_instance_id = %q", body.CurrentInstanceID)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %+v，期望 2 条", body.Items)
	}
	roles := map[string]string{}
	for _, item := range body.Items {
		roles[item.InstanceID] = item.Role
	}
	if roles["gateway-1"] != "gateway" || roles["worker-1"] != "worker" {
		t.Fatalf("roles = %+v", roles)
	}
	// 契约按 instance_id 升序；兜底注入的行必须参与排序，不能直接 append。
	if body.Items[0].InstanceID != "gateway-1" || body.Items[1].InstanceID != "worker-1" {
		t.Fatalf("items 顺序 = [%s, %s]，期望升序", body.Items[0].InstanceID, body.Items[1].InstanceID)
	}
	// 注入行必须与心跳行同形：零值时间会被前端当真实日期渲染。
	injected := body.Items[0]
	if injected.Version == "" || injected.StartedAt.IsZero() || injected.LastSeen.IsZero() {
		t.Fatalf("注入的当前实例字段不完整：%+v", injected)
	}
}

// TestInstanceListSortsInjectedCurrentInstance 覆盖「当前实例 id 排在已有
// 心跳之后」的方向：若实现只做 append，本用例的 items[0] 会是 worker-1。
func TestInstanceListSortsInjectedCurrentInstance(t *testing.T) {
	env := newInstanceTestEnv(t, "aaa-gateway", "gateway", rbac.NewAuthzMiddleware())
	if err := env.rdb.HSet(t.Context(), InstanceHeartbeatKey("worker-1"), map[string]interface{}{
		"status":     "active",
		"role":       "worker",
		"version":    "1.4.1",
		"started_at": "2026-10-20T10:00:00Z",
		"last_seen":  "2026-10-20T10:00:30Z",
	}).Err(); err != nil {
		t.Fatalf("seed worker heartbeat: %v", err)
	}

	_, body := listInstancesRequest(t, env, instanceManager())
	if len(body.Items) != 2 {
		t.Fatalf("items = %+v，期望 2 条", body.Items)
	}
	if body.Items[0].InstanceID != "aaa-gateway" || body.Items[1].InstanceID != "worker-1" {
		t.Fatalf("items 顺序 = [%s, %s]，期望升序", body.Items[0].InstanceID, body.Items[1].InstanceID)
	}
}

func TestInstanceListOmitsExpiredHeartbeats(t *testing.T) {
	env := newInstanceTestEnv(t, "gateway-1", "gateway", rbac.NewAuthzMiddleware())
	key := InstanceHeartbeatKey("gone")
	if err := env.rdb.HSet(t.Context(), key, map[string]interface{}{
		"status": "active", "role": "worker", "last_seen": "2026-10-20T10:00:00Z",
	}).Err(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := env.rdb.Expire(t.Context(), key, InstanceHeartbeatTTL).Err(); err != nil {
		t.Fatalf("expire: %v", err)
	}
	env.mr.FastForward(InstanceHeartbeatTTL + time.Second)

	_, body := listInstancesRequest(t, env, instanceManager())
	for _, item := range body.Items {
		if item.InstanceID == "gone" {
			t.Fatalf("过期实例不得返回：%+v", body.Items)
		}
	}
}

func TestInstanceListRedisFailureReturns503WithFixedDetail(t *testing.T) {
	env := newInstanceTestEnv(t, "gateway-1", "gateway", rbac.NewAuthzMiddleware())
	addr := env.mr.Addr()
	env.mr.Close()

	rr, _ := listInstancesRequest(t, env, instanceManager())
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d，期望 503（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), instanceListUnavailableDetail) {
		t.Fatalf("固定文案缺失：%s", rr.Body.String())
	}
	// 不泄露 Redis 地址/内部错误。
	if strings.Contains(rr.Body.String(), addr) || strings.Contains(rr.Body.String(), "connection refused") {
		t.Fatalf("响应泄露内部细节：%s", rr.Body.String())
	}
}

func TestInstanceListRequiresSettingsManage(t *testing.T) {
	env := newInstanceTestEnv(t, "gateway-1", "gateway", rbac.NewAuthzMiddleware())

	user := &auth.Principal{UserID: 2, Role: rbac.RoleUser}
	rr, _ := listInstancesRequest(t, env, user)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("普通用户 = %d，期望 403", rr.Code)
	}

	adminWithout := &auth.Principal{UserID: 3, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermChannelsRead}}
	rr, _ = listInstancesRequest(t, env, adminWithout)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 settings:manage 的 admin = %d，期望 403", rr.Code)
	}
}
