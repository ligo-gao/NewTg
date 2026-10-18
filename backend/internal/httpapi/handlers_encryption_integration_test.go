//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/auth"
)

// 本文件是 Task 13 的 SQL 密钥库集成验收：SQLLoginKeyStore 是 I-7 双槽与
// 轮换事务的唯一生产实现，必须对真实 PostgreSQL 验证。运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	go test -tags=integration ./internal/httpapi/ -run TestIntegrationLoginKey -v
//
// 未设置 DSN 时跳过（GC-8：未打标签的 go test ./... 不依赖外部服务）。

const loginKeyIntegrationMaster = "task-13-integration-master-key"

func openLoginKeyIntegration(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// resetLoginKeys 清空密钥槽表。该表只服务登录前端加密，集成测试独占使用；
// 每个子测试重新开始，避免上一个子测试的双槽影响断言。
func resetLoginKeys(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`DELETE FROM login_encryption_keys`); err != nil {
		t.Fatalf("清理 login_encryption_keys: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM login_encryption_keys`) })
}

func newLoginKeyManagerForIntegration(t *testing.T, db *sql.DB) *auth.LoginKeyManager {
	t.Helper()
	cipher, err := auth.NewSecretCipher(loginKeyIntegrationMaster)
	if err != nil {
		t.Fatalf("NewSecretCipher: %v", err)
	}
	return auth.NewLoginKeyManager(NewSQLLoginKeyStore(db), cipher)
}

func loginKeyCounts(t *testing.T, db *sql.DB) (active, inactive int) {
	t.Helper()
	err := db.QueryRow(`SELECT count(*) FILTER (WHERE active),
		count(*) FILTER (WHERE NOT active) FROM login_encryption_keys`).Scan(&active, &inactive)
	if err != nil {
		t.Fatalf("统计密钥槽: %v", err)
	}
	return active, inactive
}

// TestIntegrationLoginKeyStore 覆盖：首次 bootstrap 插入、轮换事务（旧槽降级 +
// 新槽插入、0 行轮换被拒）、部分唯一索引并发争用、previous 槽清理。
func TestIntegrationLoginKeyStore(t *testing.T) {
	db := openLoginKeyIntegration(t)
	ctx := context.Background()
	store := NewSQLLoginKeyStore(db)

	t.Run("bootstrap", func(t *testing.T) {
		resetLoginKeys(t, db)
		mgr := newLoginKeyManagerForIntegration(t, db)

		kid, publicPEM, err := mgr.PublicKey(ctx)
		if err != nil {
			t.Fatalf("PublicKey bootstrap: %v", err)
		}
		if len(kid) != 32 {
			t.Fatalf("kid = %q，期望 32 位十六进制", kid)
		}
		if active, inactive := loginKeyCounts(t, db); active != 1 || inactive != 0 {
			t.Fatalf("bootstrap 后槽数 = active %d / previous %d，期望 1/0", active, inactive)
		}

		// 私钥必须以 AES-256-GCM envelope 落库，且能被 CREDENTIAL_ENCRYPTION_KEY 打开。
		var sealed []byte
		if err := db.QueryRow(`SELECT private_key_enc FROM login_encryption_keys
			WHERE active`).Scan(&sealed); err != nil {
			t.Fatalf("读取 private_key_enc: %v", err)
		}
		if bytes.Contains(sealed, []byte("BEGIN PRIVATE KEY")) {
			t.Fatal("private_key_enc 不得包含明文 PEM")
		}
		cipher, _ := auth.NewSecretCipher(loginKeyIntegrationMaster)
		if _, err := cipher.Decrypt(string(sealed)); err != nil {
			t.Fatalf("private_key_enc 无法解密: %v", err)
		}

		kid2, publicPEM2, err := mgr.PublicKey(ctx)
		if err != nil || kid2 != kid || publicPEM2 != publicPEM {
			t.Fatalf("bootstrap 必须幂等：(%q,%v) vs (%q,%v)", kid2, err, kid, nil)
		}
		if active, _ := loginKeyCounts(t, db); active != 1 {
			t.Fatalf("幂等调用后 active 槽 = %d，期望 1", active)
		}
	})

	t.Run("rotate", func(t *testing.T) {
		resetLoginKeys(t, db)
		mgr := newLoginKeyManagerForIntegration(t, db)

		kid1, publicPEM1, err := mgr.PublicKey(ctx)
		if err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		publicKey, err := auth.ParseLoginPublicKeyPEM(publicPEM1)
		if err != nil {
			t.Fatalf("ParseLoginPublicKeyPEM: %v", err)
		}
		// 轮换前用第一代公钥加密，轮换后必须仍能通过 previous 槽解密。
		payload, err := auth.EncryptPasswordV2(publicKey, kid1, "CorrectHorse9!")
		if err != nil {
			t.Fatalf("EncryptPasswordV2: %v", err)
		}

		kid2, err := mgr.Rotate(ctx)
		if err != nil {
			t.Fatalf("Rotate: %v", err)
		}
		if kid2 == kid1 {
			t.Fatal("轮换后 kid 不得相同")
		}
		if active, inactive := loginKeyCounts(t, db); active != 1 || inactive != 1 {
			t.Fatalf("轮换后槽数 = active %d / previous %d，期望 1/1", active, inactive)
		}
		var activeKid string
		if err := db.QueryRow(`SELECT kid FROM login_encryption_keys WHERE active`).Scan(&activeKid); err != nil {
			t.Fatalf("读取 active kid: %v", err)
		}
		if activeKid != kid2 {
			t.Fatalf("active kid = %q，期望新 kid %q", activeKid, kid2)
		}

		got, err := auth.DecodePasswordPayload(ctx, true, mgr.Lookup, "", payload, kid1)
		if err != nil || got != "CorrectHorse9!" {
			t.Fatalf("previous 槽解密失败：(%q, %v)", got, err)
		}

		// 0 行轮换（目标已不是 active）必须被拒，且不产生第三个槽。
		var oldID int64
		if err := db.QueryRow(`SELECT id FROM login_encryption_keys
			WHERE NOT active AND kid = $1`, kid1).Scan(&oldID); err != nil {
			t.Fatalf("读取 previous 槽 id: %v", err)
		}
		dummy := auth.StoredLoginKey{
			Active:        true,
			PublicKeyPEM:  "probe-pem",
			PrivateKeyEnc: []byte("probe"),
			Kid:           "probe-kid",
			CreatedAt:     time.Now(),
		}
		if err := store.RotateLoginKey(ctx, oldID, dummy); err == nil {
			t.Fatal("对非 active 槽轮换必须返回错误（0 行更新）")
		}
		if active, inactive := loginKeyCounts(t, db); active != 1 || inactive != 1 {
			t.Fatalf("失败轮换不得改变槽数：active %d / previous %d", active, inactive)
		}
		if err := db.QueryRow(`SELECT kid FROM login_encryption_keys WHERE active`).Scan(&activeKid); err != nil {
			t.Fatalf("读取 active kid: %v", err)
		}
		if activeKid != kid2 {
			t.Fatalf("失败轮换不得改变 active：%q != %q", activeKid, kid2)
		}
	})

	t.Run("concurrent bootstrap", func(t *testing.T) {
		resetLoginKeys(t, db)
		cipher, err := auth.NewSecretCipher(loginKeyIntegrationMaster)
		if err != nil {
			t.Fatalf("NewSecretCipher: %v", err)
		}

		const replicas = 5
		kids := make([]string, replicas)
		errs := make([]error, replicas)
		var wg sync.WaitGroup
		for i := 0; i < replicas; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				// 每个副本独立的管理器，模拟多进程同时首次启动。
				mgr := auth.NewLoginKeyManager(NewSQLLoginKeyStore(db), cipher)
				kids[i], _, errs[i] = mgr.PublicKey(ctx)
			}(i)
		}
		wg.Wait()

		for i := range errs {
			if errs[i] != nil {
				t.Fatalf("副本 %d bootstrap: %v", i, errs[i])
			}
			if kids[i] != kids[0] {
				t.Fatalf("副本 %d 得到不同 kid：%q vs %q", i, kids[i], kids[0])
			}
		}
		if active, inactive := loginKeyCounts(t, db); active != 1 || inactive != 0 {
			t.Fatalf("并发 bootstrap 后槽数 = active %d / previous %d，期望 1/0（部分唯一索引）",
				active, inactive)
		}
	})

	t.Run("prune previous", func(t *testing.T) {
		resetLoginKeys(t, db)
		mgr := newLoginKeyManagerForIntegration(t, db)

		kid1, _, err := mgr.PublicKey(ctx)
		if err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		kid2, err := mgr.Rotate(ctx)
		if err != nil {
			t.Fatalf("Rotate: %v", err)
		}

		// 窗口内（刚轮换）不得清理。
		if n, err := mgr.PrunePrevious(ctx); err != nil || n != 0 {
			t.Fatalf("窗口内 PrunePrevious = (%d, %v)，期望 (0, nil)", n, err)
		}

		// 把 active 的创建时间推到 16 分钟前，模拟轮换窗口已过。
		if _, err := db.Exec(`UPDATE login_encryption_keys
			SET created_at = now() - interval '16 minutes' WHERE kid = $1`, kid2); err != nil {
			t.Fatalf("老化 active 槽: %v", err)
		}
		n, err := mgr.PrunePrevious(ctx)
		if err != nil || n != 1 {
			t.Fatalf("超窗 PrunePrevious = (%d, %v)，期望清理 1 个 previous", n, err)
		}
		if active, inactive := loginKeyCounts(t, db); active != 1 || inactive != 0 {
			t.Fatalf("清理后槽数 = active %d / previous %d，期望 1/0", active, inactive)
		}
		if _, err := mgr.Lookup(ctx, kid1); !errors.Is(err, auth.ErrUnknownLoginKey) {
			t.Fatalf("超窗后 previous kid 应失配，实际 %v", err)
		}
		if _, err := mgr.Lookup(ctx, kid2); err != nil {
			t.Fatalf("active 槽应仍可用：%v", err)
		}
	})
}
