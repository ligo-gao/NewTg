//go:build integration

package platform

// W10-EMAIL 装配断言：worker 启动期必须补种通知模板，不能依赖 gateway
// 装配或 channel health 的 best-effort 路径。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w10e?sslmode=disable' \
//	  go test -tags=integration ./internal/platform/ -run TestSeedWorkerNotifyTemplates -v

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/notify"
)

func TestSeedWorkerNotifyTemplatesSeedsEmailRowsIdempotently(t *testing.T) {
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过模板播种装配测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM notification_templates`); err != nil {
		t.Fatalf("clean templates: %v", err)
	}
	store, err := notify.NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	seedNotifyTemplates(context.Background(), store, nil)

	countEmail := func() int {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM notification_templates WHERE channel = 'email'`).Scan(&n); err != nil {
			t.Fatalf("count email templates: %v", err)
		}
		return n
	}
	first := countEmail()
	if first == 0 {
		t.Fatal("worker 启动期未补种任何 email 模板")
	}
	// 幂等：再播种一次不新增行（ON CONFLICT DO NOTHING）。
	seedNotifyTemplates(context.Background(), store, nil)
	if second := countEmail(); second != first {
		t.Fatalf("重复播种改变了行数：%d → %d", first, second)
	}
	// 至少有一个 worker 真正会用的类别可渲染。
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM notification_templates
		WHERE "key" = 'billing.payment_failed' AND channel = 'email' AND locale = 'en'
	)`).Scan(&exists); err != nil {
		t.Fatalf("check billing.payment_failed template: %v", err)
	}
	if !exists {
		t.Fatal("billing.payment_failed/email/en 未播种")
	}
}
