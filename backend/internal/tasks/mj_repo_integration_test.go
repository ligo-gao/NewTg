//go:build integration

package tasks

// W8/MJ-a2 的 MJ 数据面查询验收（真实 PostgreSQL）。
//
// 覆盖：
//   - GetByUserUpstreamID 的归属键（同用户命中、跨用户不命中、family 隔离）；
//   -MJChannelCandidates 的类型/模型/分组过滤（OpenAI 渠道即使误配 mj_* 模型
//     也不得参与；midjourney/midjourney_plus 才可见）；
//   - GetChannel 返回 status/type（锁定渠道校验 + Plus-only 能力判定需要）。
//
// 运行：NEWTG_TEST_DB_DSN=postgres://... go test -tags=integration ./internal/tasks/

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

const mjChannelTestPrefix = "mj-a2-channel-"

// insertMJTestChannel 写一条渠道测试行并返回 id。
func insertMJTestChannel(t *testing.T, db *sql.DB, name, typ string, models, groupTags string, priority int) int64 {
	t.Helper()
	var id int64
	err := db.QueryRowContext(context.Background(), `
		INSERT INTO channels (name, type, base_url, credentials, supported_models,
			group_tags, model_mapping, weight, priority, status)
		VALUES ($1,$2,$3,$4,$5::jsonb,NULLIF($6,'')::jsonb,'{}'::jsonb,1,$7,1)
		RETURNING id`,
		name, typ, "http://upstream.example", []byte("sealed"), models, groupTags, priority).Scan(&id)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	return id
}

func TestMJChannelCandidatesFilterByTypeModelAndGroup(t *testing.T) {
	db := openTaskTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	// 清理历史失败用例残留（按前缀），否则它们会参与候选选择。
	_, _ = db.Exec(`DELETE FROM channels WHERE name LIKE 'mj-a2-channel-%'`)

	mjID := insertMJTestChannel(t, db,
		fmt.Sprintf("%s%d-mj", mjChannelTestPrefix, suffix), "midjourney",
		`["mj_imagine","mj_upscale"]`, `["default","vip"]`, 10)
	plusID := insertMJTestChannel(t, db,
		fmt.Sprintf("%s%d-plus", mjChannelTestPrefix, suffix), "midjourney_plus",
		`["mj_imagine","mj_modal"]`, ``, 5)
	disabledID := insertMJTestChannel(t, db,
		fmt.Sprintf("%s%d-disabled", mjChannelTestPrefix, suffix), "midjourney",
		`["mj_imagine"]`, ``, 99)
	if _, err := db.Exec(`UPDATE channels SET status = 2 WHERE id = $1`, disabledID); err != nil {
		t.Fatalf("disable channel: %v", err)
	}
	openaiID := insertMJTestChannel(t, db,
		fmt.Sprintf("%s%d-openai", mjChannelTestPrefix, suffix), "openai",
		`["mj_imagine"]`, ``, 100)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM channels WHERE id = ANY($1)`,
			[]int64{mjID, plusID, disabledID, openaiID})
	})

	candidates, err := repo.MJChannelCandidates(ctx, "mj_imagine", "default")
	if err != nil {
		t.Fatalf("MJChannelCandidates: %v", err)
	}
	ids := map[int64]bool{}
	for _, candidate := range candidates {
		ids[candidate.ID] = true
	}
	if !ids[mjID] || !ids[plusID] {
		t.Fatalf("candidates missing MJ/plus channels: %v", ids)
	}
	if ids[disabledID] {
		t.Fatalf("disabled channel must not be a candidate")
	}
	if ids[openaiID] {
		t.Fatalf("non-MJ channel must never be a candidate even with mj_* models")
	}

	// 模型命中：mj_modal 只有 plus 渠道支持。
	modalCandidates, err := repo.MJChannelCandidates(ctx, "mj_modal", "default")
	if err != nil {
		t.Fatalf("MJChannelCandidates(mj_modal): %v", err)
	}
	if len(modalCandidates) != 1 || modalCandidates[0].ID != plusID {
		t.Fatalf("mj_modal candidates = %+v, want only plus channel", modalCandidates)
	}

	// 分组：vip 只应看到 group_tags 为 NULL（全部分组）或含 vip 的渠道。
	vipCandidates, err := repo.MJChannelCandidates(ctx, "mj_imagine", "vip")
	if err != nil {
		t.Fatalf("MJChannelCandidates(vip): %v", err)
	}
	vipIDs := map[int64]bool{}
	for _, candidate := range vipCandidates {
		vipIDs[candidate.ID] = true
	}
	if !vipIDs[plusID] {
		t.Fatalf("NULL group_tags channel must serve every group")
	}
	// mj 渠道的 group_tags 含 vip，也应可见。
	if !vipIDs[mjID] {
		t.Fatalf("group_tags containing vip must be visible")
	}
	// 另一个分组不应看到 mj 渠道（group_tags 不含 other）。
	otherCandidates, err := repo.MJChannelCandidates(ctx, "mj_imagine", "other")
	if err != nil {
		t.Fatalf("MJChannelCandidates(other): %v", err)
	}
	for _, candidate := range otherCandidates {
		if candidate.ID == mjID {
			t.Fatalf("group_tags without other must not be visible to other")
		}
	}

	channel, err := repo.GetChannel(ctx, plusID)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if channel.Type != "midjourney_plus" || channel.Status != 1 {
		t.Fatalf("GetChannel type/status = %q/%d", channel.Type, channel.Status)
	}
}

func TestListByUserUpstreamIDsIsOwnerAndFamilyScoped(t *testing.T) {
	db := openTaskTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	userID := time.Now().UnixNano() % 1_000_000
	otherUser := userID + 1
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE user_id IN ($1,$2)`, userID, otherUser)
	})

	now := time.Now().UTC()
	insert := func(publicID string, owner int64, family, upstreamID string) *Task {
		t.Helper()
		task := &Task{
			PublicTaskID: publicID, UpstreamTaskID: upstreamID, Family: family,
			Action: "IMAGINE", PluginKey: "", FetchMode: "poll",
			State: StateSubmitted, UserID: owner, Group: "default", ChannelID: 1,
			Quota: 10, SubmitTime: &now,
			Properties: map[string]any{}, Data: map[string]any{"image_url": "https://cdn/" + upstreamID},
			PrivateData: map[string]any{"request_id": "req_" + publicID},
		}
		if family == FamilyPlugin {
			task.PluginKey = "sample"
			task.Action = "generate"
		}
		if err := repo.Insert(ctx, task); err != nil {
			t.Fatalf("insert %s: %v", publicID, err)
		}
		return task
	}

	ownA := insert(fmt.Sprintf("task_mj_b_list_%d_a", userID), userID, FamilyMJ, "mj_b_list_a")
	ownB := insert(fmt.Sprintf("task_mj_b_list_%d_b", userID), userID, FamilyMJ, "mj_b_list_b")
	foreign := insert(fmt.Sprintf("task_mj_b_list_%d_foreign", userID), otherUser, FamilyMJ, "mj_b_list_a")
	pluginRow := insert(fmt.Sprintf("task_mj_b_list_%d_plugin", userID), userID, FamilyPlugin, "mj_b_list_a")

	rows, err := repo.ListByUserUpstreamIDs(ctx, userID,
		[]string{"mj_b_list_a", "mj_b_list_b", "mj_b_missing"}, 100)
	if err != nil {
		t.Fatalf("ListByUserUpstreamIDs: %v", err)
	}
	got := map[int64]bool{}
	for _, row := range rows {
		got[row.ID] = true
	}
	if len(rows) != 2 || !got[ownA.ID] || !got[ownB.ID] {
		t.Fatalf("batch rows = %v, want only own MJ rows", taskIDs(rows))
	}
	if got[foreign.ID] || got[pluginRow.ID] {
		t.Fatalf("batch crossed owner/family boundary: %v", taskIDs(rows))
	}
	// 读面必须带 data（fetch DTO 依赖产物 URL）。
	for _, row := range rows {
		if row.Data["image_url"] == "" {
			t.Fatalf("batch row lost data projection: %+v", row.Data)
		}
	}

	mjItems, total, err := repo.ListByUser(ctx, userID, ListFilter{Family: FamilyMJ})
	if err != nil {
		t.Fatalf("ListByUser(mj): %v", err)
	}
	if total != 2 || len(mjItems) != 2 {
		t.Fatalf("family=mj list = %d/%d, want 2", len(mjItems), total)
	}
	for _, item := range mjItems {
		if item.Family != FamilyMJ {
			t.Fatalf("family=mj list contains %q", item.Family)
		}
	}
	pluginItems, _, err := repo.ListByUser(ctx, userID, ListFilter{Family: FamilyPlugin})
	if err != nil {
		t.Fatalf("ListByUser(plugin): %v", err)
	}
	pluginIDs := make([]int64, 0, len(pluginItems))
	for _, item := range pluginItems {
		pluginIDs = append(pluginIDs, item.ID)
	}
	if len(pluginItems) != 1 || pluginIDs[0] != pluginRow.ID {
		t.Fatalf("family=plugin list = %v", pluginIDs)
	}

	// 空 ids 不查库、返回空切片。
	empty, err := repo.ListByUserUpstreamIDs(ctx, userID, nil, 100)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty ids = %v/%v", empty, err)
	}
}

func TestGetByUserUpstreamIDIsFamilyAndOwnerScoped(t *testing.T) {
	db := openTaskTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	userID := time.Now().UnixNano() % 1_000_000
	otherUser := userID + 1
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE user_id IN ($1,$2)`, userID, otherUser)
	})

	now := time.Now().UTC()
	mjTask := &Task{
		PublicTaskID: fmt.Sprintf("task_mj_a2_%d", userID), UpstreamTaskID: "mj_shared",
		Family: FamilyMJ, Action: "IMAGINE", PluginKey: "", FetchMode: "poll",
		State: StateSubmitted, UserID: userID, Group: "default", ChannelID: 1,
		SubmitTime: &now, Properties: map[string]any{}, PrivateData: map[string]any{},
		Data: map[string]any{},
	}
	if err := repo.Insert(ctx, mjTask); err != nil {
		t.Fatalf("insert mj task: %v", err)
	}
	// 另一个用户持有同一 MjId（R-MJ-05：upstream_task_id 不是全局唯一键）。
	otherTask := *mjTask
	otherTask.ID = 0
	otherTask.PublicTaskID = fmt.Sprintf("task_mj_a2_other_%d", userID)
	otherTask.UserID = otherUser
	if err := repo.Insert(ctx, &otherTask); err != nil {
		t.Fatalf("insert other user's mj task: %v", err)
	}

	got, err := repo.GetByUserUpstreamID(ctx, userID, "mj_shared")
	if err != nil {
		t.Fatalf("GetByUserUpstreamID: %v", err)
	}
	if got.ID != mjTask.ID || got.UserID != userID {
		t.Fatalf("lookup returned wrong owner's row: %+v", got)
	}

	if _, err := repo.GetByUserUpstreamID(ctx, userID+1000, "mj_shared"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("unknown user lookup error = %v, want ErrTaskNotFound", err)
	}
	if _, err := repo.GetByUserUpstreamID(ctx, userID, ""); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("empty upstream id lookup error = %v, want ErrTaskNotFound", err)
	}

	// family 隔离：plugin 行即便有同名 upstream id 也不得被 MJ 复用查询命中。
	pluginTask := &Task{
		PublicTaskID: fmt.Sprintf("task_mj_a2_plugin_%d", userID), UpstreamTaskID: "mj_plugin_same",
		Family: FamilyPlugin, Action: "generate", PluginKey: "sample", FetchMode: "poll",
		State: StateSubmitted, UserID: userID, Group: "default", ChannelID: 1,
		SubmitTime: &now, Properties: map[string]any{}, PrivateData: map[string]any{},
		Data: map[string]any{},
	}
	if err := repo.Insert(ctx, pluginTask); err != nil {
		t.Fatalf("insert plugin task: %v", err)
	}
	if _, err := repo.GetByUserUpstreamID(ctx, userID, "mj_plugin_same"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("plugin row must not be visible to MJ reuse lookup, err = %v", err)
	}
}

// TestListOwnersByUpstreamIDReturnsAllOwners 是 W8/MJ-c 图片代理的归属候选
// 查询验收（真实 SQL）：同一 MjId 的多位 owner 全部返回、family 隔离、
// 固定排序与 limit 截断、空 id 不命中 inline 上传行。
func TestListOwnersByUpstreamIDReturnsAllOwners(t *testing.T) {
	db := openTaskTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	base := time.Now().UnixNano() % 1_000_000
	users := []int64{base, base + 1, base + 2}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tasks WHERE user_id = ANY($1)`, users)
	})

	now := time.Now().UTC()
	makeRow := func(userID int64, upstreamID, family, pluginKey string) *Task {
		return &Task{
			PublicTaskID:   fmt.Sprintf("task_mjc_%d_%s_%s", userID, upstreamID, family),
			UpstreamTaskID: upstreamID, Family: family, Action: "IMAGINE",
			PluginKey: pluginKey, FetchMode: "poll", State: StateSuccess,
			UserID: userID, Group: "default", ChannelID: 1, SubmitTime: &now,
			Properties: map[string]any{}, PrivateData: map[string]any{},
			Data: map[string]any{"image_url": "https://cdn.example/" + upstreamID + ".png"},
		}
	}
	for _, userID := range users {
		if err := repo.Insert(ctx, makeRow(userID, "mj_c_shared", FamilyMJ, "")); err != nil {
			t.Fatalf("insert mj row for %d: %v", userID, err)
		}
	}
	// 插件行即便同名也不得成为图片候选。
	if err := repo.Insert(ctx, makeRow(users[0], "mj_c_plugin", FamilyPlugin, "sample")); err != nil {
		t.Fatalf("insert plugin row: %v", err)
	}

	rows, err := repo.ListOwnersByUpstreamID(ctx, "mj_c_shared", 100)
	if err != nil {
		t.Fatalf("ListOwnersByUpstreamID: %v", err)
	}
	if len(rows) != len(users) {
		t.Fatalf("candidates = %d, want %d", len(rows), len(users))
	}
	for i, row := range rows {
		if row.UserID != users[i] {
			t.Fatalf("candidate[%d].UserID = %d, want %d (ordered by user_id)", i, row.UserID, users[i])
		}
		if row.Data["image_url"] == "" {
			t.Fatalf("candidate[%d] lost data.image_url: %v", i, row.Data)
		}
	}

	// limit 截断是确定的（按 user_id 升序）。
	limited, err := repo.ListOwnersByUpstreamID(ctx, "mj_c_shared", 2)
	if err != nil {
		t.Fatalf("limited query: %v", err)
	}
	if len(limited) != 2 || limited[0].UserID != users[0] || limited[1].UserID != users[1] {
		t.Fatalf("limited candidates = %+v", limited)
	}

	// family 隔离与空 id。
	pluginRows, err := repo.ListOwnersByUpstreamID(ctx, "mj_c_plugin", 100)
	if err != nil {
		t.Fatalf("plugin query: %v", err)
	}
	if len(pluginRows) != 0 {
		t.Fatalf("plugin row leaked into image candidates: %+v", pluginRows)
	}
	emptyRows, err := repo.ListOwnersByUpstreamID(ctx, "", 100)
	if err != nil || len(emptyRows) != 0 {
		t.Fatalf("empty id query = (%v,%v), want empty", emptyRows, err)
	}
}
