package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
)

func TestSystemStatusEndpoint(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	started := time.Now().Add(-90 * time.Second)
	handler := NewSystemStatusHandlers(SystemStatusConfig{
		Redis: rdb, NodeName: "node-test", StartedAt: started,
	})
	mux := http.NewServeMux()
	handler.Register(mux)

	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/system/status", "", admin, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body struct {
		Version   string `json:"version"`
		Uptime    int64  `json:"uptime"`
		Instances []struct {
			Name   string `json:"name"`
			Role   string `json:"role"`
			Status string `json:"status"`
		} `json:"instances"`
		Dependencies map[string]string `json:"dependencies"`
		MFA          struct {
			Available        bool `json:"available"`
			TotalUsers       int  `json:"total_users"`
			TotpEnabledUsers int  `json:"totp_enabled_users"`
		} `json:"mfa"`
		Polling struct {
			IntervalSeconds              int       `json:"interval_seconds"`
			HeartbeatIntervalSeconds     int       `json:"heartbeat_interval_seconds"`
			UpdateCheckBackgroundPolling bool      `json:"update_check_background_polling"`
			ServerTime                   time.Time `json:"server_time"`
		} `json:"polling"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("JSON 非法: %v", err)
	}
	if body.Version == "" {
		t.Fatal("version 不得为空")
	}
	if body.Uptime < 90 {
		t.Fatalf("uptime = %d，期望 >= 90", body.Uptime)
	}
	if len(body.Instances) != 1 || body.Instances[0].Name != "node-test" || body.Instances[0].Status != "active" {
		t.Fatalf("instances = %+v", body.Instances)
	}
	if body.Dependencies["redis"] != "ok" {
		t.Fatalf("redis 依赖 = %q，期望 ok", body.Dependencies["redis"])
	}
	// 没有 DB / worker 心跳时必须如实报 fail/unknown，不能伪装健康。
	if body.Dependencies["db"] != "failed" {
		t.Fatalf("db 依赖 = %q，期望 failed", body.Dependencies["db"])
	}
	if body.Dependencies["worker"] != "unknown" {
		t.Fatalf("worker 依赖 = %q，期望 unknown", body.Dependencies["worker"])
	}
	// 无 DB 时 MFA 必须显式不可用，不能把「没查」呈现成 0% 覆盖。
	if body.MFA.Available {
		t.Fatal("无 DB 时 mfa.available 必须为 false")
	}
	if body.Polling.IntervalSeconds != systemStatusPollIntervalSeconds ||
		body.Polling.HeartbeatIntervalSeconds != int(InstanceHeartbeatInterval/time.Second) ||
		body.Polling.UpdateCheckBackgroundPolling {
		t.Fatalf("polling = %+v", body.Polling)
	}
	if body.Polling.ServerTime.IsZero() {
		t.Fatal("polling.server_time 不得为零值")
	}

	user := &auth.Principal{UserID: 2, Role: rbac.RoleUser}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/system/status", "", user, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("普通用户 = %d，期望 403", rr.Code)
	}
}

// system status 与 GET /api/instances 读同一 Redis 心跳源：另一个副本写入
// 心跳后必须出现在聚合结果里，而当前实例即使没有心跳也要保留。
func TestSystemStatusInstancesReadRedisHeartbeats(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	if err := rdb.HSet(t.Context(), InstanceHeartbeatKey("worker-1"), map[string]interface{}{
		"status":     "active",
		"role":       "worker",
		"version":    "1.4.1",
		"started_at": "2026-10-20T10:00:00Z",
		"last_seen":  "2026-10-20T10:00:30Z",
	}).Err(); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}

	handler := NewSystemStatusHandlers(SystemStatusConfig{
		Redis: rdb, InstanceID: "gateway-1", Role: "gateway", StartedAt: time.Now(),
	})
	mux := http.NewServeMux()
	handler.Register(mux)

	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/system/status", "", admin, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body struct {
		Instances []struct {
			Name   string `json:"name"`
			Role   string `json:"role"`
			Status string `json:"status"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("JSON 非法: %v", err)
	}
	if len(body.Instances) != 2 {
		t.Fatalf("instances = %+v，期望 2 条", body.Instances)
	}
	roles := map[string]string{}
	for _, item := range body.Instances {
		roles[item.Name] = item.Role
	}
	if roles["gateway-1"] != "gateway" || roles["worker-1"] != "worker" {
		t.Fatalf("roles = %+v", roles)
	}
}
