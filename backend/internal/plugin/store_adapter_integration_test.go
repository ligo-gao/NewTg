//go:build integration

package plugin

// SQLVersionLoader 的数据库级验收：active 投影、created_at 作为 revision 源、
// 不存在版本返回 (nil,nil)（dryrun 需要区分业务结果与内部故障）。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_m6d_t5?sslmode=disable' \
//	  go test -tags=integration ./internal/plugin/ -run TestSQLVersionLoader -v

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func openPluginTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
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

func TestSQLVersionLoader(t *testing.T) {
	db := openPluginTestDB(t)
	ctx := context.Background()
	key := "it-loader-" + time.Now().UTC().Format("150405")
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	for _, query := range []string{
		`DELETE FROM task_plugin_versions WHERE "key" = $1`,
		`DELETE FROM task_plugins WHERE "key" = $1`,
	} {
		if _, err := db.ExecContext(ctx, query, key); err != nil {
			t.Fatalf("clean: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM task_plugin_versions WHERE "key" = $1`, key)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM task_plugins WHERE "key" = $1`, key)
	})

	if _, err := db.ExecContext(ctx, `INSERT INTO task_plugins ("key", name, source) VALUES ($1, 'Loader', 'upload')`, key); err != nil {
		t.Fatalf("insert plugin: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO task_plugin_versions
		("key", version, source_text, source_hash, enabled, active, uploaded_by)
		VALUES ($1, '1.0.0', 'source', $2, true, true, 1)`, key, hash); err != nil {
		t.Fatalf("insert version: %v", err)
	}

	loader := NewSQLVersionLoader(db)
	active, err := loader.LoadActive(ctx)
	if err != nil {
		t.Fatalf("LoadActive: %v", err)
	}
	var found *ActiveVersion
	for i := range active {
		if active[i].Key == key {
			found = &active[i]
		}
	}
	if found == nil {
		t.Fatalf("LoadActive 未返回 %s：%+v", key, active)
	}
	if found.Version != "1.0.0" || !found.Enabled || !found.Active || found.UpdatedAt.IsZero() {
		t.Fatalf("active 投影不符：%+v", found)
	}

	version, err := loader.LoadVersion(ctx, key, "1.0.0")
	if err != nil || version == nil || version.SourceText != "source" || version.SourceHash != hash {
		t.Fatalf("LoadVersion = %+v, %v", version, err)
	}
	// B-11 enabled=false 必须让运行时投影变 disabled（/status 的数据面语义）。
	if _, err := db.ExecContext(ctx,
		`UPDATE task_plugins SET enabled = false WHERE "key" = $1`, key); err != nil {
		t.Fatalf("disable plugin: %v", err)
	}
	active, err = loader.LoadActive(ctx)
	if err != nil {
		t.Fatalf("LoadActive after disable: %v", err)
	}
	for _, v := range active {
		if v.Key == key && v.Enabled {
			t.Fatalf("插件主表禁用后投影仍 enabled=true：%+v", v)
		}
	}

	missing, err := loader.LoadVersion(ctx, key, "9.9.9")
	if err != nil || missing != nil {
		t.Fatalf("不存在版本应返回 (nil,nil)，得到 %+v, %v", missing, err)
	}
}
