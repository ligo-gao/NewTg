//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
)

// 本文件是 Task 7 的集成验收：SQLTwoFARepo 对真实 PG16 的条件 UPDATE 原子性，
// 以及 challenge 端点在真实 PG + Redis 上的完整链路。运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable'
//	NEWTG_TEST_REDIS_ADDR=localhost:56379
//	go test -tags=integration ./internal/httpapi/ -run TestIntegrationTwoFA -v
//
// 未设置环境变量时跳过（GC-8：未打标签的 go test ./... 不依赖外部服务）。

// twoFAIntegrationFixture 插入一个唯一的测试用户并登记清理。
func twoFAIntegrationFixture(t *testing.T) (int64, *SQLTwoFARepo, *redis.Client) {
	t.Helper()
	db, rdb := openAuthIntegration(t)
	ensureDefaultGroup(t, db)

	suffix := uniqueAuthSuffix()
	var userID int64
	if err := db.QueryRow(`
		INSERT INTO users (username, email, password_hash)
		VALUES ($1, $2, $3)
		RETURNING id`,
		"it2fa_"+suffix, "it2fa_"+suffix+"@example.com", "$argon2id$placeholder",
	).Scan(&userID); err != nil {
		t.Fatalf("insert test user: %v", err)
	}
	t.Cleanup(func() { cleanupAuthUser(t, db, userID) })
	return userID, NewSQLTwoFARepo(db), rdb
}

// TestIntegrationTwoFARepoLifecycle 覆盖 Enable（原子三写）→ Find（密文往返、
// 哈希数组）→ Consume（条件 UPDATE）→ Replace（旧码失效）→ Disable（清空）。
func TestIntegrationTwoFARepoLifecycle(t *testing.T) {
	userID, repo, _ := twoFAIntegrationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cipher, err := auth.NewSecretCipher("integration-credential-key")
	if err != nil {
		t.Fatalf("NewSecretCipher: %v", err)
	}
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	secretEnc, err := cipher.Encrypt(secret)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	plain, hashes, err := auth.GenerateBackupCodes(4)
	if err != nil {
		t.Fatalf("GenerateBackupCodes: %v", err)
	}
	_ = plain

	applied, err := repo.EnableTwoFA(ctx, userID, secretEnc, hashes)
	if err != nil || !applied {
		t.Fatalf("EnableTwoFA = (%v, %v)，期望 true", applied, err)
	}
	// 重复启用：条件 totp_enabled=false 不再成立。
	if applied, err := repo.EnableTwoFA(ctx, userID, secretEnc, hashes); err != nil || applied {
		t.Fatalf("重复 EnableTwoFA = (%v, %v)，期望 false", applied, err)
	}

	user, err := repo.FindUserByID(ctx, userID)
	if err != nil {
		t.Fatalf("FindUserByID: %v", err)
	}
	if user == nil || !user.TOTPEnabled {
		t.Fatalf("FindUserByID = %+v，期望已启用", user)
	}
	if user.TOTPSecretEnc != secretEnc {
		t.Fatalf("totp_secret 列往返不一致：%q != %q", user.TOTPSecretEnc, secretEnc)
	}
	gotSecret, err := cipher.Decrypt(user.TOTPSecretEnc)
	if err != nil || gotSecret != secret {
		t.Fatalf("解密库内种子 = (%q, %v)，期望 %q", gotSecret, err, secret)
	}
	if len(user.BackupCodes) != len(hashes) {
		t.Fatalf("backup_codes 数量 = %d，期望 %d", len(user.BackupCodes), len(hashes))
	}

	// 条件核销：第一次成功，第二次（同一哈希）失败。
	remaining := removeBackupHash(user.BackupCodes, hashes[0])
	if applied, err := repo.ConsumeBackupCode(ctx, userID, hashes[0], remaining); err != nil || !applied {
		t.Fatalf("首次 Consume = (%v, %v)，期望 true", applied, err)
	}
	if applied, err := repo.ConsumeBackupCode(ctx, userID, hashes[0], remaining); err != nil || applied {
		t.Fatalf("二次 Consume = (%v, %v)，期望 false", applied, err)
	}

	// 重生成：整个数组替换，旧哈希不再可核销。
	_, newHashes, err := auth.GenerateBackupCodes(4)
	if err != nil {
		t.Fatalf("GenerateBackupCodes(new): %v", err)
	}
	if applied, err := repo.ReplaceBackupCodes(ctx, userID, newHashes); err != nil || !applied {
		t.Fatalf("ReplaceBackupCodes = (%v, %v)，期望 true", applied, err)
	}
	if applied, err := repo.ConsumeBackupCode(ctx, userID, hashes[1], removeBackupHash(newHashes, hashes[1])); err != nil || applied {
		t.Fatalf("旧码在重生成后仍被核销 = (%v, %v)", applied, err)
	}

	// 禁用：清空全部 2FA 字段，之后不能再核销。
	if err := repo.DisableTwoFA(ctx, userID); err != nil {
		t.Fatalf("DisableTwoFA: %v", err)
	}
	user, err = repo.FindUserByID(ctx, userID)
	if err != nil {
		t.Fatalf("FindUserByID(disabled): %v", err)
	}
	if user.TOTPEnabled || user.TOTPSecretEnc != "" || len(user.BackupCodes) != 0 {
		t.Fatalf("禁用后状态 = %+v，期望清空", user)
	}
	if applied, err := repo.ConsumeBackupCode(ctx, userID, newHashes[0], nil); err != nil || applied {
		t.Fatalf("禁用后 Consume = (%v, %v)，期望 false", applied, err)
	}
}

// TestIntegrationTwoFABackupCodeConcurrentConsume 是 brief (b) 的核心断言：
// 多个请求并发提交同一个备用码时，条件 UPDATE 必须只让一个成功。
func TestIntegrationTwoFABackupCodeConcurrentConsume(t *testing.T) {
	userID, repo, _ := twoFAIntegrationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cipher, err := auth.NewSecretCipher("integration-credential-key")
	if err != nil {
		t.Fatalf("NewSecretCipher: %v", err)
	}
	secretEnc, err := cipher.Encrypt("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	_, hashes, err := auth.GenerateBackupCodes(4)
	if err != nil {
		t.Fatalf("GenerateBackupCodes: %v", err)
	}
	if applied, err := repo.EnableTwoFA(ctx, userID, secretEnc, hashes); err != nil || !applied {
		t.Fatalf("EnableTwoFA = (%v, %v)", applied, err)
	}
	user, err := repo.FindUserByID(ctx, userID)
	if err != nil || user == nil {
		t.Fatalf("FindUserByID: %v", err)
	}
	remaining := removeBackupHash(user.BackupCodes, hashes[0])

	const workers = 8
	var wg sync.WaitGroup
	results := make([]bool, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 尽量同时发出，放大竞态窗口
			results[i], errs[i] = repo.ConsumeBackupCode(ctx, userID, hashes[0], remaining)
		}(i)
	}
	close(start)
	wg.Wait()

	successes := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if results[i] {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("并发核销成功次数 = %d，期望恰 1", successes)
	}

	user, err = repo.FindUserByID(ctx, userID)
	if err != nil {
		t.Fatalf("FindUserByID(final): %v", err)
	}
	if len(user.BackupCodes) != len(hashes)-1 {
		t.Fatalf("剩余备用码 = %d，期望 %d", len(user.BackupCodes), len(hashes)-1)
	}
	for _, h := range user.BackupCodes {
		if h == hashes[0] {
			t.Fatal("被核销的哈希仍在数组中")
		}
	}
}

// TestIntegrationTwoFAChallengeFlow 在真实 PG + Redis 上跑通
// enable → ticket → challenge（TOTP）→ 会话，并验证 ticket 被消费与重放被拒。
func TestIntegrationTwoFAChallengeFlow(t *testing.T) {
	userID, repo, rdb := twoFAIntegrationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cipher, err := auth.NewSecretCipher("integration-credential-key")
	if err != nil {
		t.Fatalf("NewSecretCipher: %v", err)
	}
	secretEnc, err := cipher.Encrypt(twoFATestSecret)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	_, hashes, err := auth.GenerateBackupCodes(2)
	if err != nil {
		t.Fatalf("GenerateBackupCodes: %v", err)
	}
	if applied, err := repo.EnableTwoFA(ctx, userID, secretEnc, hashes); err != nil || !applied {
		t.Fatalf("EnableTwoFA = (%v, %v)", applied, err)
	}

	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	h, err := NewTwoFAHandlers(TwoFAConfig{
		Repo:          repo,
		Redis:         rdb,
		Sessions:      sessions,
		EncryptionKey: "integration-credential-key",
		Now:           func() time.Time { return twoFATestNow },
	})
	if err != nil {
		t.Fatalf("NewTwoFAHandlers: %v", err)
	}

	issueTicket := func() string {
		buf := make([]byte, 32)
		if _, err := readFullRand(buf); err != nil {
			t.Fatalf("rand: %v", err)
		}
		ticket := base64.RawURLEncoding.EncodeToString(buf)
		if err := rdb.Set(ctx, twoFATicketKeyPrefix+ticket, itoa(userID), pending2FATTL).Err(); err != nil {
			t.Fatalf("store ticket: %v", err)
		}
		return ticket
	}

	post := func(ticket, code, typ string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"ticket": ticket, "code": code, "type": typ})
		req := httptest.NewRequest(http.MethodPost, "/api/auth/2fa/challenge", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.HandleChallenge(rr, req)
		return rr
	}

	ticket := issueTicket()
	rr := post(ticket, twoFATestCode, "totp")
	if rr.Code != http.StatusOK {
		t.Fatalf("challenge status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var sid string
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			sid = c.Value
		}
	}
	if sid == "" {
		t.Fatal("challenge 未下发会话 Cookie")
	}
	t.Cleanup(func() {
		_ = rdb.Del(context.Background(),
			"sess:"+sid,
			"sess:user:"+itoa(userID),
			twoFATOTPUsedPrefix+itoa(userID)+":"+sha256Hex(twoFATestCode)).Err()
	})
	if n, _ := rdb.Exists(ctx, twoFATicketKeyPrefix+ticket).Result(); n != 0 {
		t.Fatal("challenge 成功后 ticket 仍存在")
	}
	if n, _ := rdb.Exists(ctx, "sess:"+sid).Result(); n != 1 {
		t.Fatal("challenge 成功后 Redis 中不存在正式会话")
	}

	// 同一步内的码重放（新 ticket）必须被重放防护拒绝。
	if rr := post(issueTicket(), twoFATestCode, "totp"); rr.Code != http.StatusBadRequest {
		t.Fatalf("重放 challenge status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
