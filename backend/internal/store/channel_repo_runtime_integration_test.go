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

// TestPickTopPrefersPriorityAndSkipsDisabled 覆盖 PickTop 的选取口径：只在
// 启用渠道中按 priority DESC, id ASC 取第一条，并返回 Get 加载的完整行
// （而不只是 id）。
//
// 三个启用候选的插入顺序即 id 顺序：low(优先级 1) < high(9) < tie(9)。
// 高优先级同分时 id ASC 应选 high；若排序被改成 id DESC 就会选 tie，改成
// id ASC 或 priority ASC 则会选 low，因此 ch.ID == high 能把排序钉死。
// 「禁用后落到下一个候选」由每次调用重新走 Candidates 的 SQL status = 1
// 过滤达成；Get 之后的 Status 复核只负责返回 ok=false（不补位），本测试
// 不覆盖该竞态。
func TestPickTopPrefersPriorityAndSkipsDisabled(t *testing.T) {
	db := openChannelRuntimeTestDB(t)
	repo := NewChannelRepo(db)
	suffix := time.Now().UnixNano()
	model := fmt.Sprintf("t7/picktop-%d", suffix)
	supported := fmt.Sprintf(`["%s"]`, model)

	lowName := fmt.Sprintf("t7-pick-low-%d", suffix)
	highName := fmt.Sprintf("t7-pick-high-%d", suffix)
	tieName := fmt.Sprintf("t7-pick-tie-%d", suffix)

	low := insertRuntimeChannel(t, db, lowName, 1, supported, `{}`, "")
	high := insertRuntimeChannel(t, db, highName, 1, supported, `{}`, "")
	tie := insertRuntimeChannel(t, db, tieName, 1, supported, `{}`, "")
	disabled := insertRuntimeChannel(t, db, fmt.Sprintf("t7-pick-off-%d", suffix), 2, supported, `{}`, "")

	// tie 与 high 同优先级：id 顺序 low < high < tie，只有
	// priority DESC, id ASC 才会选中 high。
	for id, priority := range map[int64]int16{low: 1, high: 9, tie: 9, disabled: 99} {
		if _, err := db.Exec(`UPDATE channels SET priority = $2 WHERE id = $1`, id, priority); err != nil {
			t.Fatalf("set priority for %d: %v", id, err)
		}
	}

	ch, ok, err := repo.PickTop(context.Background(), model)
	if err != nil || !ok {
		t.Fatalf("PickTop = %v, %v", ok, err)
	}
	if ch.ID != high {
		t.Fatalf("picked channel %d, want priority winner %d (low=%d tie=%d)", ch.ID, high, low, tie)
	}
	// 必须返回完整行（下游测速要用凭据/model_mapping/type），而不是骨架行。
	if ch.Name != highName {
		t.Fatalf("picked channel name %q, want %q", ch.Name, highName)
	}

	// 同时禁用 high 与 tie，让下一个候选只能是最低优先级的 low。这一步验证的是
	// 「每次调用重新执行 Candidates」里的 SQL status = 1 过滤（禁用行不再进入候选
	// 列表）；不是 Get 之后的那次 Status 复核——后者只会返回 ok=false、不会补位，
	// 且不在本测试覆盖范围内。
	if _, err := db.Exec(`UPDATE channels SET status = 2 WHERE id IN ($1, $2)`, high, tie); err != nil {
		t.Fatalf("disable winner and tie: %v", err)
	}
	ch, ok, err = repo.PickTop(context.Background(), model)
	if err != nil || !ok || ch.ID != low {
		t.Fatalf("after disabling high+tie got %v/%v/%v, want %d", ch, ok, err, low)
	}

	if _, ok, err := repo.PickTop(context.Background(), fmt.Sprintf("t7/none-%d", suffix)); err != nil {
		t.Fatalf("PickTop(no channel model): %v", err)
	} else if ok {
		t.Fatal("model without channels must return ok=false")
	}
}

func stringPtrForTest(v string) *string { return &v }

// TestChannelRepoCountRoutableChannels 验证性能页 models 表 channels 列的真实
// SQL 语义：supported_models 与 model_mapping 的键都算模型全集，按渠道去重，
// 禁用渠道不计入。
func TestChannelRepoCountRoutableChannels(t *testing.T) {
	db := openChannelRuntimeTestDB(t)
	repo := NewChannelRepo(db)
	ctx := context.Background()

	suffix := time.Now().UnixNano()
	shared := fmt.Sprintf("it-crc-shared-%d", suffix)
	alias := fmt.Sprintf("it-crc-alias-%d", suffix)
	upstream := fmt.Sprintf("it-crc-upstream-%d", suffix)

	// A：启用；supported 含 shared，mapping 键 alias → upstream。
	insertRuntimeChannel(t, db, fmt.Sprintf("it-crc-a-%d", suffix), 1,
		fmt.Sprintf(`["%s"]`, shared), fmt.Sprintf(`{"%s":"%s"}`, alias, upstream), "")
	// B：启用；supported 含 shared。
	insertRuntimeChannel(t, db, fmt.Sprintf("it-crc-b-%d", suffix), 1,
		fmt.Sprintf(`["%s"]`, shared), `{}`, "")
	// C：禁用；不得计入任何模型。
	insertRuntimeChannel(t, db, fmt.Sprintf("it-crc-c-%d", suffix), 2,
		fmt.Sprintf(`["%s"]`, shared), `{}`, "")

	counts, err := repo.CountRoutableChannels(ctx)
	if err != nil {
		t.Fatalf("CountRoutableChannels: %v", err)
	}
	if got := counts[shared]; got != 2 {
		t.Errorf("counts[%s] = %d, want 2（两条启用渠道，禁用渠道不计）", shared, got)
	}
	if got := counts[alias]; got != 1 {
		t.Errorf("counts[%s] = %d, want 1（映射键也是可路由模型）", alias, got)
	}
	if _, ok := counts[upstream]; ok {
		t.Errorf("mapping 的值 %s 不应被当作对外模型", upstream)
	}
}
