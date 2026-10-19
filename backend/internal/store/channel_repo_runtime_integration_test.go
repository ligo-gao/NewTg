//go:build integration

package store

// 本文件是 channels 仓储 SQL 的**运行时**集成测试（连真 PostgreSQL）。
//
// 为什么需要它：`jsonb_build_array($n)` 在 pgx v5 stdlib 下不会给占位符推断
// 类型，PostgreSQL 会以 SQLSTATE 42P18（could not determine data type of
// parameter $N）拒绝执行。这类错误只改字符串形状的单测永远发现不了——只有
// 真正 Prepare/Execute 才会暴露。因此 Candidates（relay 热路径）与带 jsonb
// 筛选的 List 都必须在此跑一次。
//
// 运行（仓库自带的测试实例）：
//
//	NEWTG_TEST_DB_DSN=postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable \
//	  go test -tags=integration ./internal/store/ -run 'TestChannelRepo' -v
//
// DSN 未设置时跳过；表结构由 migrations 负责，本测试只插入带唯一前缀的行并在
// 结束时按 id 清理，不依赖库中既有数据。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	// pgx/v5 stdlib 驱动与生产一致（R-P10 / platform-04 §9.1）。
	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/contract"
)

// openChannelRuntimeTestDB 连接主库；NEWTG_TEST_DB_DSN 优先，兼容 TEST_DB_DSN。
func openChannelRuntimeTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		dsn = os.Getenv("TEST_DB_DSN")
	}
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN/TEST_DB_DSN 未设置，跳过 channels 运行时集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// insertRuntimeChannel 插入一行并登记按 id 清理；groupTags 为空串表示 SQL NULL
// （NULL = 全部分组，与筛选语义直接相关）。
func insertRuntimeChannel(t *testing.T, db *sql.DB, name string, status int16, supported, mapping, groupTags string) int64 {
	t.Helper()
	var groupArg any
	if groupTags != "" {
		groupArg = groupTags
	}
	var id int64
	err := db.QueryRow(`
		INSERT INTO channels (name, type, credentials, supported_models, group_tags, model_mapping,
			weight, priority, status, auto_ban)
		VALUES ($1, 'openai', $2, $3::jsonb, $4::jsonb, $5::jsonb, 1, 10, $6, true)
		RETURNING id`,
		name, []byte{0}, supported, groupArg, mapping, status).Scan(&id)
	if err != nil {
		t.Fatalf("insert channel %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM channels WHERE id = $1`, id); err != nil {
			t.Errorf("cleanup channel %d: %v", id, err)
		}
	})
	return id
}

func requireCandidateSet(t *testing.T, got []contract.ChannelCandidate, want map[int64]bool, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: 候选数 = %d，期望 %d（%+v）", label, len(got), len(want), got)
	}
	for id := range want {
		found := false
		for _, c := range got {
			if int64(c.ChannelID) == id {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s: 缺少渠道 %d（got=%+v）", label, id, got)
		}
	}
}

// TestChannelRepoCandidatesAndListRuntime 覆盖 finding 1：Candidates 的两条
// jsonb 谓词与 buildChannelWhere 的两条筛选谓词都必须在真 PG 上执行成功。
func TestChannelRepoCandidatesAndListRuntime(t *testing.T) {
	db := openChannelRuntimeTestDB(t)
	repo := NewChannelRepo(db)
	ctx := context.Background()

	suffix := time.Now().UnixNano()
	model := fmt.Sprintf("it-model-%d", suffix)
	alias := fmt.Sprintf("it-alias-%d", suffix)
	group := fmt.Sprintf("it-group-%d", suffix)

	// A：启用、分组命中、模型命中（alias 指向 model）；B：启用、group_tags NULL；
	// C：禁用，不应出现在任何结果里。
	idA := insertRuntimeChannel(t, db,
		fmt.Sprintf("it-runtime-a-%d", suffix), 1,
		fmt.Sprintf(`["%s"]`, model), fmt.Sprintf(`{"%s":"%s"}`, alias, model), fmt.Sprintf(`["%s"]`, group))
	idB := insertRuntimeChannel(t, db,
		fmt.Sprintf("it-runtime-b-%d", suffix), 1,
		fmt.Sprintf(`["%s"]`, model), `{}`, "")
	idC := insertRuntimeChannel(t, db,
		fmt.Sprintf("it-runtime-c-%d", suffix), 2,
		fmt.Sprintf(`["%s"]`, model), `{}`, fmt.Sprintf(`["%s"]`, group))

	t.Run("Candidates without group", func(t *testing.T) {
		got, err := repo.Candidates(ctx, model, "")
		if err != nil {
			// 42P18 会在这里出现（修复前）。
			t.Fatalf("Candidates(%q): %v", model, err)
		}
		requireCandidateSet(t, got, map[int64]bool{idA: true, idB: true}, "no-group")
	})

	t.Run("Candidates with group", func(t *testing.T) {
		got, err := repo.Candidates(ctx, model, group)
		if err != nil {
			t.Fatalf("Candidates(%q,%q): %v", model, group, err)
		}
		// A 的 group_tags 命中；B 为 NULL（全部分组）；C 被 status=2 排除。
		requireCandidateSet(t, got, map[int64]bool{idA: true, idB: true}, "with-group")
	})

	t.Run("filtered List", func(t *testing.T) {
		enabled := int16(1)
		items, total, err := repo.List(ctx, ChannelFilter{
			Page: 1, PageSize: 50, Status: &enabled, Group: group, Model: model,
		})
		if err != nil {
			t.Fatalf("List(group,model): %v", err)
		}
		if total != 2 {
			t.Fatalf("List total = %d，期望 2", total)
		}
		got := make(map[int64]bool, len(items))
		for _, item := range items {
			got[item.ID] = true
		}
		if !got[idA] || !got[idB] || got[idC] {
			t.Fatalf("List 结果 = %v，期望仅 %d/%d", got, idA, idB)
		}
	})
}

// TestChannelRepoCandidatesAliasRouting 覆盖 finding 3：/v1/models 会宣告
// model_mapping 的键，路由候选必须同样把别名视为可路由（否则列表里有、请求 503）。
func TestChannelRepoCandidatesAliasRouting(t *testing.T) {
	db := openChannelRuntimeTestDB(t)
	repo := NewChannelRepo(db)
	ctx := context.Background()

	suffix := time.Now().UnixNano()
	model := fmt.Sprintf("it-real-%d", suffix)
	alias := fmt.Sprintf("it-alias-only-%d", suffix)

	id := insertRuntimeChannel(t, db,
		fmt.Sprintf("it-alias-row-%d", suffix), 1,
		fmt.Sprintf(`["%s"]`, model), fmt.Sprintf(`{"%s":"%s"}`, alias, model), "")

	got, err := repo.Candidates(ctx, alias, "")
	if err != nil {
		t.Fatalf("Candidates(alias %q): %v", alias, err)
	}
	requireCandidateSet(t, got, map[int64]bool{id: true}, "alias")

	// 真实模型名仍然命中同一条渠道（supported_models 分支，不能被 OR 改坏）。
	got, err = repo.Candidates(ctx, model, "")
	if err != nil {
		t.Fatalf("Candidates(model %q): %v", model, err)
	}
	requireCandidateSet(t, got, map[int64]bool{id: true}, "real model")
}

// TestChannelRepoPluginBindingRuntime 覆盖 M6d Task 7a 的 SQL 路径：
//   - Create/Update/Get 对 task_plugin_key / task_extend_plugin_keys 的读写；
//   - CountBoundChannels 的单绑/多绑/停用渠道口径与 jsonb 包含谓词。
func TestChannelRepoPluginBindingRuntime(t *testing.T) {
	db := openChannelRuntimeTestDB(t)
	repo := NewChannelRepo(db)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	name := fmt.Sprintf("it-binding-%d", suffix)

	created, err := repo.Create(ctx, &Channel{
		Name:                 name,
		Type:                 string(contract.ChannelTypeTaskPlugin),
		Credentials:          []byte("ciphertext"),
		SupportedModels:      []byte(`["video-x"]`),
		TaskPluginKey:        stringPtrForTest("kling"),
		TaskExtendPluginKeys: []byte(`[]`),
		Weight:               1,
		Status:               1,
		AutoBan:              true,
	})
	if err != nil {
		t.Fatalf("Create binding channel: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, created.ID) })
	if created.TaskPluginKey == nil || *created.TaskPluginKey != "kling" {
		t.Fatalf("created task_plugin_key = %v", created.TaskPluginKey)
	}
	if string(created.TaskExtendPluginKeys) != "[]" {
		t.Fatalf("created extend = %s，期望 []", created.TaskExtendPluginKeys)
	}

	// 清空单绑并写多绑（换类型 60）。
	patch := ChannelPatch{
		Type:                 stringPtrForTest("newapi"),
		TaskPluginKeySet:     true,
		TaskPluginKey:        nil,
		TaskExtendPluginKeys: []byte(`["kling","sora"]`),
	}
	updated, err := repo.Update(ctx, created.ID, patch, created.UpdatedAt)
	if err != nil {
		t.Fatalf("Update binding channel: %v", err)
	}
	if updated.TaskPluginKey != nil {
		t.Fatalf("task_plugin_key 未清空：%v", *updated.TaskPluginKey)
	}
	got, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get binding channel: %v", err)
	}
	if string(got.TaskExtendPluginKeys) != `["kling", "sora"]` &&
		string(got.TaskExtendPluginKeys) != `["kling","sora"]` {
		t.Fatalf("extend 持久化 = %s", got.TaskExtendPluginKeys)
	}

	// 计数：单绑 + 多绑 + 停用排除 + 同一渠道两种形态命中同一 key 只计一次。
	// key 带本次运行的唯一后缀，共享库里其它行的绑定不会影响精确断言。
	k1 := fmt.Sprintf("it-k1-%d", suffix)
	k2 := fmt.Sprintf("it-k2-%d", suffix)
	k3 := fmt.Sprintf("it-k3-%d", suffix)
	single := insertRuntimeChannel(t, db, fmt.Sprintf("it-bind-single-%d", suffix), 1, `["video-x"]`, `{}`, "")
	multi := insertRuntimeChannel(t, db, fmt.Sprintf("it-bind-multi-%d", suffix), 1, `["video-x"]`, `{}`, "")
	both := insertRuntimeChannel(t, db, fmt.Sprintf("it-bind-both-%d", suffix), 1, `["video-x"]`, `{}`, "")
	disabled := insertRuntimeChannel(t, db, fmt.Sprintf("it-bind-disabled-%d", suffix), 2, `["video-x"]`, `{}`, "")
	for id, stmt := range map[int64]string{
		single:   `UPDATE channels SET task_plugin_key = $2, task_extend_plugin_keys = '[]'::jsonb WHERE id = $1`,
		multi:    `UPDATE channels SET task_extend_plugin_keys = jsonb_build_array($2::text, $3::text) WHERE id = $1`,
		both:     `UPDATE channels SET task_plugin_key = $2, task_extend_plugin_keys = jsonb_build_array($2::text, $3::text) WHERE id = $1`,
		disabled: `UPDATE channels SET task_plugin_key = $2 WHERE id = $1`,
	} {
		var args []any
		switch id {
		case single, disabled:
			args = []any{id, k1}
		case multi:
			args = []any{id, k1, k2}
		case both:
			args = []any{id, k3, fmt.Sprintf("%s-other", k3)}
		}
		if _, err := db.Exec(stmt, args...); err != nil {
			t.Fatalf("bind channel %d: %v", id, err)
		}
	}
	counts, err := repo.CountBoundChannels(ctx, []string{k1, k2, k3, "it-missing"})
	if err != nil {
		t.Fatalf("CountBoundChannels: %v", err)
	}
	// k1：single + multi（disabled 不计）= 2；k2：multi = 1；
	// k3：both 的两种形态命中同一渠道，DISTINCT 后 = 1。
	if counts[k1] != 2 {
		t.Fatalf("counts[%s] = %d，期望 2（停用渠道不计）", k1, counts[k1])
	}
	if counts[k2] != 1 {
		t.Fatalf("counts[%s] = %d，期望 1", k2, counts[k2])
	}
	if counts[k3] != 1 {
		t.Fatalf("counts[%s] = %d，期望 1（单绑+多绑同 key 只计一次）", k3, counts[k3])
	}
	if _, ok := counts["it-missing"]; !ok || counts["it-missing"] != 0 {
		t.Fatalf("未绑定 key 必须返回 0：%v", counts)
	}
}

func stringPtrForTest(v string) *string { return &v }
