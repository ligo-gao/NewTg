package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"newtg/gateway/internal/rbac"
)

// 本文件是 Task 14 分组端点的单元验收（modules-07 F-07-10）：
// name 格式/唯一性、系统组保护、引用检查（409）、权限门与字段白名单。
// 依赖复用 handlers_users_test.go 的 newAdminTestEnv/fakeAdminRepo（内存实现）。

func decodeGroupView(t *testing.T, rr *httptest.ResponseRecorder) adminGroupView {
	t.Helper()
	var view adminGroupView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("group view JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	return view
}

func TestGroupsCRUD(t *testing.T) {
	env := newAdminTestEnv(t)
	admin := fullAdmin()

	rr := env.request(admin, http.MethodGet, "/api/groups", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var list adminGroupListView
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("list JSON: %v", err)
	}
	if list.Total != 2 || list.Page != 1 || list.PageSize != adminPageSizeDefault {
		t.Fatalf("初始分组列表 = %+v，期望 total=2（default/auto）", list)
	}

	rr = env.request(admin, http.MethodPost, "/api/groups",
		map[string]any{"name": "vip", "description": "VIP users"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}
	view := decodeGroupView(t, rr)
	if view.Name != "vip" || view.Description == nil || *view.Description != "VIP users" || view.IsSystem {
		t.Fatalf("create view = %+v", view)
	}

	// 重名 → 409。
	rr = env.request(admin, http.MethodPost, "/api/groups", map[string]any{"name": "vip"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("重名 status = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}

	// name 规则：小写、2–32、[a-z0-9_-]。
	for _, bad := range []string{"V", "VIP", "v i p", "a", "x/y"} {
		rr = env.request(admin, http.MethodPost, "/api/groups", map[string]any{"name": bad})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("非法 name %q status = %d，期望 400", bad, rr.Code)
		}
	}

	// 编辑描述。
	rr = env.request(admin, http.MethodPatch, "/api/groups/vip",
		map[string]any{"description": "updated"})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch description status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	view = decodeGroupView(t, rr)
	if view.Description == nil || *view.Description != "updated" {
		t.Fatalf("description = %v，期望 updated", view.Description)
	}

	// 改名（未被引用时允许）。
	rr = env.request(admin, http.MethodPatch, "/api/groups/vip", map[string]any{"name": "vip2"})
	if rr.Code != http.StatusOK {
		t.Fatalf("rename status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if decodeGroupView(t, rr).Name != "vip2" {
		t.Fatalf("改名失败")
	}

	// 空 body → 400。
	rr = env.request(admin, http.MethodPatch, "/api/groups/vip2", map[string]any{})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("空 PATCH status = %d，期望 400", rr.Code)
	}

	// 删除空分组 → 204。
	rr = env.request(admin, http.MethodDelete, "/api/groups/vip2", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
	if _, err := env.repo.GetGroup(t.Context(), "vip2"); err == nil {
		t.Fatal("删除后分组仍存在")
	}
	// 不存在的分组 → 404。
	rr = env.request(admin, http.MethodDelete, "/api/groups/nope", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("删除不存在分组 status = %d，期望 404", rr.Code)
	}
}

func TestGroupsSystemAndReferenceRules(t *testing.T) {
	env := newAdminTestEnv(t)
	admin := fullAdmin()

	for _, name := range []string{groupNameDefault, groupNameAuto} {
		rr := env.request(admin, http.MethodDelete, "/api/groups/"+name, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("删除系统组 %s status = %d，期望 400（body=%q）", name, rr.Code, rr.Body.String())
		}
	}
	rr := env.request(admin, http.MethodPatch, "/api/groups/default",
		map[string]any{"name": "default2"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("改名系统组 status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}

	env.repo.addGroup("vip", false)
	env.repo.mu.Lock()
	env.repo.groupRef["vip"] = 3
	env.repo.mu.Unlock()

	rr = env.request(admin, http.MethodDelete, "/api/groups/vip", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("删除被引用分组 status = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}
	rr = env.request(admin, http.MethodPatch, "/api/groups/vip", map[string]any{"name": "vip2"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("改名被引用分组 status = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}
	// 描述仍可编辑。
	rr = env.request(admin, http.MethodPatch, "/api/groups/vip", map[string]any{"description": "still ok"})
	if rr.Code != http.StatusOK {
		t.Fatalf("编辑被引用分组描述 status = %d，期望 200", rr.Code)
	}
}

func TestGroupsRequirePermissionAndFieldPolicy(t *testing.T) {
	env := newAdminTestEnv(t)

	rr := env.request(plainUser(), http.MethodGet, "/api/groups", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("user 角色 status = %d，期望 403", rr.Code)
	}
	rr = env.request(limitedAdmin(rbac.PermUsersRead), http.MethodPost, "/api/groups",
		map[string]any{"name": "vip"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 groups:manage status = %d，期望 403", rr.Code)
	}
	rr = env.request(limitedAdmin(rbac.PermGroupsManage), http.MethodPost, "/api/groups",
		map[string]any{"name": "vip"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("持 groups:manage status = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}

	// 字段层：未登记字段 403。
	rr = env.request(fullAdmin(), http.MethodPost, "/api/groups",
		map[string]any{"name": "vip2", "ratio": 120})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("未登记字段 status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestGroupsPagination(t *testing.T) {
	env := newAdminTestEnv(t)
	rr := env.request(fullAdmin(), http.MethodGet, "/api/groups?p=2&page_size=1", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var list adminGroupListView
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if list.Page != 2 || list.PageSize != 1 || list.Total != 2 || len(list.Items) != 1 {
		t.Fatalf("分页 = %+v，期望 page=2 size=1 total=2 items=1", list)
	}
	for _, path := range []string{"/api/groups?p=0", "/api/groups?page_size=101"} {
		rr = env.request(fullAdmin(), http.MethodGet, path, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d，期望 400", path, rr.Code)
		}
	}
}
