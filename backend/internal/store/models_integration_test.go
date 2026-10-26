//go:build integration

package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// TestModelZeroValuesPersist 钉死 A1 的回归点：GORM 模型不得用 default: 标签
// 改写有意义的零值。
//
// 为什么在集成层验证：default 标签的行为发生在 INSERT 生成阶段，只有真实
// PostgreSQL 才能证明「显式 0 落库为 0」而不是被 GORM 补成 100000。
func TestModelZeroValuesPersist(t *testing.T) {
	db := openTestDB(t)
	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open gorm: %v", err)
	}

	suffix := randomSuffix(t)

	user := User{
		Username:     "zero" + suffix,
		Role:         "user",
		Status:       "enabled",
		Group:        "default",
		CustomerType: "general", // NOT NULL CHECK 列没有 default: 标签，GORM 显式写入零值 ''
		Language:     "en",
		RecordIPLog:  false, // 显式 false 必须原样落库，不能被 default:true 覆盖
	}
	if err := gormDB.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM api_tokens WHERE user_id = $1`, user.ID)
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
	})

	var recordIPLog bool
	if err := db.QueryRow(`SELECT record_ip_log FROM users WHERE id = $1`, user.ID).Scan(&recordIPLog); err != nil {
		t.Fatalf("read record_ip_log: %v", err)
	}
	if recordIPLog {
		t.Fatalf("record_ip_log = true，显式 false 被默认值改写")
	}

	zero := int64(0)
	token := APIToken{
		UserID:      user.ID,
		Name:        "zero-quota",
		KeyHash:     "hash-" + suffix,
		Status:      "enabled",
		Group:       "default",
		RemainQuota: &zero,
	}
	if err := gormDB.Create(&token).Error; err != nil {
		t.Fatalf("create token with zero quota: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM api_tokens WHERE id = $1`, token.ID)
	})

	var stored sql.NullInt64
	if err := db.QueryRow(`SELECT remain_quota FROM api_tokens WHERE id = $1`, token.ID).Scan(&stored); err != nil {
		t.Fatalf("read remain_quota: %v", err)
	}
	if !stored.Valid || stored.Int64 != 0 {
		t.Fatalf("remain_quota = %v, want 0（显式 0 被默认值改写）", stored)
	}

	// NULL 必须与 0 可区分：不限额度的令牌 depend on this.
	unlimited := APIToken{
		UserID:      user.ID,
		Name:        "unlimited",
		KeyHash:     "hash-u-" + suffix,
		Status:      "enabled",
		Group:       "default",
		RemainQuota: nil,
	}
	if err := gormDB.Create(&unlimited).Error; err != nil {
		t.Fatalf("create unlimited token: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM api_tokens WHERE id = $1`, unlimited.ID)
	})
	var nilQuota sql.NullInt64
	if err := db.QueryRow(`SELECT remain_quota FROM api_tokens WHERE id = $1`, unlimited.ID).
		Scan(&nilQuota); err != nil {
		t.Fatalf("read unlimited remain_quota: %v", err)
	}
	if nilQuota.Valid {
		t.Fatalf("remain_quota = %d, want NULL（nil 被写成默认值）", nilQuota.Int64)
	}

	group := UserGroup{
		Name:           "zg" + suffix,
		Ratio:          100,
		UserSelectable: false,
		IsSystem:       false,
		Status:         1,
	}
	if err := gormDB.Create(&group).Error; err != nil {
		t.Fatalf("create user group: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM user_groups WHERE id = $1`, group.ID) })

	var userSelectable, isSystem bool
	if err := db.QueryRow(`SELECT user_selectable, is_system FROM user_groups WHERE id = $1`, group.ID).
		Scan(&userSelectable, &isSystem); err != nil {
		t.Fatalf("read user group flags: %v", err)
	}
	if userSelectable || isSystem {
		t.Fatalf("user_selectable/is_system = %v/%v，显式 false 被默认值改写", userSelectable, isSystem)
	}
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("crypto/rand: %v", err)
	}
	return hex.EncodeToString(buf)
}
