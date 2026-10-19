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

	user := &auth.Principal{UserID: 2, Role: rbac.RoleUser}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/system/status", "", user, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("普通用户 = %d，期望 403", rr.Code)
	}
}
