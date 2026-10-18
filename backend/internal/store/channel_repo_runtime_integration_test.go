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
