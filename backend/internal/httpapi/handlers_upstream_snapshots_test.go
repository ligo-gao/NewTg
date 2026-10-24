package httpapi

// 本文件是 Task 8「上游用量快照」端点的验收：权限门（settings:manage）、
// 服务端盖章的溯源字段、一次批量只写一条审计、读取窗口默认值、写入校验。
//
// 复用 handlers_upstream_accounts_test.go 的 fakeUpstream*Repo 与
// newUpstreamTestEnv，以及 handlers_users_test.go 的审计捕获/Principal 助手。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/api/dto"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

// ---------------------------------------------------------------------------
// 溯源 + 审计（brief Step 1 的第四项）
// ---------------------------------------------------------------------------

func TestUpsertSnapshotsWritesExactlyOneAuditEvent(t *testing.T) {
	env := newUpstreamTestEnv(t)
	env.accounts.seed(store.UpstreamAccount{
		ID: 1, Name: "openai-main", Provider: "openai",
		Currency: "USD", Timezone: "Asia/Shanghai",
	})

	rr := env.request(fullAdmin(), http.MethodPut, "/api/upstream-snapshots", map[string]any{
		"account_id": 1,
		"items": []map[string]any{
			{"usage_date": "2026-10-20", "upstream_model": "gpt-5", "input_cache_hit_tokens": 10},
			{"usage_date": "2026-10-21", "upstream_model": "gpt-5", "output_tokens": 20},
			{"usage_date": "2026-10-22", "upstream_model": "gpt-5-mini", "requests": 3},
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("upsert status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}

	var result dto.UpstreamSnapshotUpsertResult
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("upsert 响应 JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	if result.Updated != 3 {
		t.Fatalf("updated = %d，期望 3", result.Updated)
	}
	if env.snapshots.upsertCalls != 1 {
		t.Fatalf("仓储 Upsert 调用 %d 次，期望 1 次（单事务批量）", env.snapshots.upsertCalls)
	}

	// 一次批量导入只允许一条审计事件：逐行写会把 500 行导入放大成 500 条。
	calls := env.audit.allCalls()
	if len(calls) != 1 {
		t.Fatalf("审计写库 %d 次，期望恰好 1 次", len(calls))
	}
	if args := env.audit.concatenatedArgs(); !strings.Contains(args, "upstream_snapshot.upsert") {
		t.Fatalf("审计实参中没有 upstream_snapshot.upsert: %s", args)
	}

	if len(env.snapshots.lastItems) != 3 {
		t.Fatalf("写入 %d 行，期望 3 行", len(env.snapshots.lastItems))
	}
	for index, item := range env.snapshots.lastItems {
		if item.AccountID != 1 {
			t.Fatalf("items[%d].AccountID = %d，期望 1", index, item.AccountID)
		}
		// entered_by 来自当前 principal，客户端不可提交。
		if item.EnteredBy == nil || *item.EnteredBy != adminTestActorID {
			t.Fatalf("items[%d].EnteredBy = %v，期望当前管理员 %d", index, item.EnteredBy, adminTestActorID)
		}
		if item.Source != string(dto.UpstreamSnapshotSourceManual) {
			t.Fatalf("items[%d].Source = %q，期望 manual", index, item.Source)
		}
		if !item.EnteredAt.Equal(env.now) {
			t.Fatalf("items[%d].EnteredAt = %v，期望 %v", index, item.EnteredAt, env.now)
		}
		// currency 缺省继承账号币种。
		if item.Currency != "USD" {
			t.Fatalf("items[%d].Currency = %q，期望继承账号币种 USD", index, item.Currency)
		}
	}
}

// ---------------------------------------------------------------------------
// 权限门
// ---------------------------------------------------------------------------

func TestUpstreamSnapshotEndpointsRequireSettingsManage(t *testing.T) {
	env := newUpstreamTestEnv(t)
	env.accounts.seed(store.UpstreamAccount{Name: "openai-main", Provider: "openai"})

	upsertBody := map[string]any{
		"account_id": 1,
		"items":      []map[string]any{{"usage_date": "2026-10-20", "upstream_model": "gpt-5"}},
	}
	principals := []struct {
		name string
		p    *auth.Principal
	}{
		{"缺权限的管理员", limitedAdmin(rbac.PermUsersRead)},
		{"普通用户", plainUser()},
	}
	for _, tc := range principals {
		t.Run(tc.name+"/list", func(t *testing.T) {
			rr := env.request(tc.p, http.MethodGet, "/api/upstream-snapshots?account_id=1", nil)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
			}
			if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
				t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
			}
		})
		t.Run(tc.name+"/upsert", func(t *testing.T) {
			rr := env.request(tc.p, http.MethodPut, "/api/upstream-snapshots", upsertBody)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
			}
			if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
				t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
			}
		})
	}
	if env.snapshots.upsertCalls != 0 {
		t.Fatalf("被拒绝的请求仍写入了 %d 次", env.snapshots.upsertCalls)
	}

	// 持有 settings:manage 的受限管理员两条路都放行。
	admin := limitedAdmin(rbac.PermSettingsManage)
	if rr := env.request(admin, http.MethodGet, "/api/upstream-snapshots?account_id=1", nil); rr.Code != http.StatusOK {
		t.Fatalf("list status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if rr := env.request(admin, http.MethodPut, "/api/upstream-snapshots", upsertBody); rr.Code != http.StatusOK {
		t.Fatalf("upsert status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 读取：默认窗口按账号时区与回看天数
// ---------------------------------------------------------------------------

func TestListUpstreamSnapshotsDefaultsToAccountTimeZoneLookback(t *testing.T) {
	env := newUpstreamTestEnv(t)
	env.accounts.seed(store.UpstreamAccount{
		ID: 1, Name: "openai-main", Provider: "openai",
		Currency: "CNY", Timezone: "Asia/Shanghai",
	})
	hitTokens := int64(7)
	enteredBy := int64(adminTestActorID)
	env.snapshots.listRows = []store.UpstreamSnapshot{{
		ID:             9,
		AccountID:      1,
		UsageDate:      time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC),
		UpstreamModel:  "gpt-5",
		Currency:       "CNY",
		Source:         string(dto.UpstreamSnapshotSourceManual),
		CacheHitTokens: &hitTokens,
		EnteredBy:      &enteredBy,
		EnteredAt:      env.now,
		Note:           "import",
	}}

	rr := env.request(limitedAdmin(rbac.PermSettingsManage), http.MethodGet,
		"/api/upstream-snapshots?account_id=1", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}

	// 2026-10-24 12:00 UTC = 2026-10-24 20:00 Asia/Shanghai；默认窗口是
	// [账号时区的今天 - 3 天, 账号时区的今天]。
	wantFrom := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	if env.snapshots.listAccount != 1 {
		t.Fatalf("ListRange account = %d，期望 1", env.snapshots.listAccount)
	}
	if !env.snapshots.listFrom.Equal(wantFrom) || !env.snapshots.listTo.Equal(wantTo) {
		t.Fatalf("ListRange 窗口 = [%v, %v]，期望 [%v, %v]",
			env.snapshots.listFrom, env.snapshots.listTo, wantFrom, wantTo)
	}

	var list dto.UpstreamSnapshotList
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("list 响应 JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	if len(list.Items) != 1 {
		t.Fatalf("items 数量 = %d，期望 1", len(list.Items))
	}
	got := list.Items[0]
	if got.UsageDate.Time.Format("2006-01-02") != "2026-10-22" {
		t.Fatalf("usage_date = %v，期望 2026-10-22", got.UsageDate.Time)
	}
	if got.InputCacheHitTokens == nil || *got.InputCacheHitTokens != 7 {
		t.Fatalf("input_cache_hit_tokens = %v，期望 7", got.InputCacheHitTokens)
	}
	if got.Source != dto.UpstreamSnapshotSourceManual {
		t.Fatalf("source = %q，期望 manual", got.Source)
	}
}

func TestListUpstreamSnapshotsHonorsExplicitRange(t *testing.T) {
	env := newUpstreamTestEnv(t)
	env.accounts.seed(store.UpstreamAccount{ID: 1, Name: "openai-main", Provider: "openai"})

	rr := env.request(fullAdmin(), http.MethodGet,
		"/api/upstream-snapshots?account_id=1&from=2026-10-01&to=2026-10-05", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	wantFrom := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	if !env.snapshots.listFrom.Equal(wantFrom) || !env.snapshots.listTo.Equal(wantTo) {
		t.Fatalf("ListRange 窗口 = [%v, %v]，期望 [%v, %v]",
			env.snapshots.listFrom, env.snapshots.listTo, wantFrom, wantTo)
	}
}

func TestListUpstreamSnapshotsRejectsBadRange(t *testing.T) {
	env := newUpstreamTestEnv(t)
	env.accounts.seed(store.UpstreamAccount{ID: 1, Name: "openai-main", Provider: "openai"})

	cases := []string{
		"/api/upstream-snapshots",
		"/api/upstream-snapshots?account_id=abc",
		"/api/upstream-snapshots?account_id=1&from=2026-13-01",
		"/api/upstream-snapshots?account_id=1&from=2026-10-10&to=2026-10-01",
	}
	for _, path := range cases {
		rr := env.request(fullAdmin(), http.MethodGet, path, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d，期望 400（body=%q）", path, rr.Code, rr.Body.String())
		}
	}
}

// ---------------------------------------------------------------------------
// 写入校验
// ---------------------------------------------------------------------------

func TestUpsertSnapshotsRejectsInvalidItems(t *testing.T) {
	env := newUpstreamTestEnv(t)
	env.accounts.seed(store.UpstreamAccount{
		ID: 1, Name: "openai-main", Provider: "openai",
		Currency: "CNY", Timezone: "Asia/Shanghai",
	})

	rr := env.request(fullAdmin(), http.MethodPut, "/api/upstream-snapshots", map[string]any{
		"account_id": 1,
		"items": []map[string]any{
			// 模型名 trim 后为空。
			{"usage_date": "2026-10-20", "upstream_model": "   "},
			// 未来日期（账号时区今天是 2026-10-24）、负数金额、非法币种。
			{
				"usage_date": "2026-10-25", "upstream_model": "gpt-5",
				"amount_minor": -1, "currency": "US",
			},
		},
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	problem := decodeUpstreamProblem(t, rr)
	if problemType, _ := problem["type"].(string); !strings.HasSuffix(problemType, "/invalid-field") {
		t.Fatalf("problem.type = %v，期望以 /invalid-field 结尾", problem["type"])
	}
	fields := problemFields(problem)
	for _, want := range []string{
		"items[0].upstream_model",
		"items[1].usage_date",
		"items[1].amount_minor",
		"items[1].currency",
	} {
		if !containsString(fields, want) {
			t.Fatalf("problem.fields = %v，期望包含 %s", fields, want)
		}
	}
	if env.snapshots.upsertCalls != 0 {
		t.Fatalf("校验失败仍调用了仓储 %d 次", env.snapshots.upsertCalls)
	}
}

func TestUpsertSnapshotsRejectsMoreThan500Items(t *testing.T) {
	env := newUpstreamTestEnv(t)
	env.accounts.seed(store.UpstreamAccount{ID: 1, Name: "openai-main", Provider: "openai"})

	items := make([]map[string]any, 501)
	for index := range items {
		items[index] = map[string]any{"usage_date": "2026-10-20", "upstream_model": "gpt-5"}
	}
	rr := env.request(fullAdmin(), http.MethodPut, "/api/upstream-snapshots", map[string]any{
		"account_id": 1,
		"items":      items,
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	if fields := problemFields(decodeUpstreamProblem(t, rr)); !containsString(fields, "items") {
		t.Fatalf("problem.fields = %v，期望包含 items", fields)
	}
	if env.snapshots.upsertCalls != 0 {
		t.Fatalf("超限请求仍调用了仓储 %d 次", env.snapshots.upsertCalls)
	}
}

func TestUpsertSnapshotsUnknownAccountReturns404(t *testing.T) {
	env := newUpstreamTestEnv(t)

	rr := env.request(fullAdmin(), http.MethodPut, "/api/upstream-snapshots", map[string]any{
		"account_id": 999,
		"items":      []map[string]any{{"usage_date": "2026-10-20", "upstream_model": "gpt-5"}},
	})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d，期望 404（body=%q）", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
	}
	if env.snapshots.upsertCalls != 0 {
		t.Fatalf("未知账号仍调用了仓储 %d 次", env.snapshots.upsertCalls)
	}
}
