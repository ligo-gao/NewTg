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
