//go:build integration

package store

import (
	"database/sql"
	"strings"
	"testing"
)

// 本文件是 M1 认证迁移（{ts}_m1_auth）的 schema 集成验收。
// 权威：docs/modules-01-账户与认证-子功能设计.md §3.5/§3.6/§3.13/§3.19（R-07）
// + docs/platform-01-数据库字段汇总.md §1/§2/§3；R-01/R-02 为补列裁决。
// 运行：NEWTG_TEST_DB_DSN=postgres://... go test -tags=integration ./internal/store/ -run TestM1Schema -v

// m1ColumnInfo 读取单列的 PG 类型 / 可空性 / 默认值；列不存在时 ok=false。
func m1ColumnInfo(t *testing.T, db *sql.DB, table, column string) (dataType, nullable string, def sql.NullString, ok bool) {
	t.Helper()
	err := db.QueryRow(`SELECT data_type, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`,
		table, column).Scan(&dataType, &nullable, &def)
	if err == sql.ErrNoRows {
		return "", "", def, false
	}
	if err != nil {
		t.Fatalf("query column %s.%s: %v", table, column, err)
	}
	return dataType, nullable, def, true
}

// m1UniqueOnColumn 判断某列上是否存在 UNIQUE 索引（含唯一约束自动建的索引）。
func m1UniqueOnColumn(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var n int
	err := db.QueryRow(`SELECT count(*)
		FROM pg_index i
		JOIN pg_class t ON t.oid = i.indrelid
		JOIN pg_namespace nsp ON nsp.oid = t.relnamespace
		JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = ANY (i.indkey)
		WHERE nsp.nspname = current_schema() AND t.relname = $1
		  AND a.attname = $2 AND i.indisunique`, table, column).Scan(&n)
	if err != nil {
		t.Fatalf("query unique index on %s.%s: %v", table, column, err)
	}
	return n > 0
}

// m1TableExists 判断当前 schema 中是否存在该表。
func m1TableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var n int
	err := db.QueryRow(`SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name = $1`, table).Scan(&n)
	if err != nil {
		t.Fatalf("query table %s: %v", table, err)
	}
	return n > 0
}

// m1FKCount 统计某表上的真实外键数量。
func m1FKCount(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	err := db.QueryRow(`SELECT count(*) FROM information_schema.table_constraints
		WHERE constraint_type = 'FOREIGN KEY'
		  AND table_schema = current_schema() AND table_name = $1`, table).Scan(&n)
	if err != nil {
		t.Fatalf("query fk on %s: %v", table, err)
	}
	return n
}

// TestM1Schema 覆盖任务 brief 的断言 (a)–(e)。
func TestM1Schema(t *testing.T) {
	db := openTestDB(t)

	// (a) users.must_change_password（R-02）：boolean NOT NULL DEFAULT false。
	t.Run("(a) users.must_change_password", func(t *testing.T) {
		dataType, nullable, def, ok := m1ColumnInfo(t, db, "users", "must_change_password")
		if !ok {
			t.Fatalf("users.must_change_password 不存在（R-02）")
		}
		if dataType != "boolean" {
			t.Fatalf("users.must_change_password 类型 = %q，want boolean", dataType)
		}
		if nullable != "NO" {
			t.Fatalf("users.must_change_password 可空性 = %q，want NO", nullable)
		}
		if !def.Valid || def.String != "false" {
			t.Fatalf("users.must_change_password 默认值 = %v，want false", def)
		}
	})

	// (b) api_tokens 补列（R-01）且列数 == 15。
	t.Run("(b) api_tokens 15 列与补列", func(t *testing.T) {
		for _, col := range []string{"auto_groups", "cross_group_retry"} {
			if _, _, _, ok := m1ColumnInfo(t, db, "api_tokens", col); !ok {
				t.Fatalf("api_tokens.%s 不存在（R-01）", col)
			}
		}
		dataType, nullable, _, ok := m1ColumnInfo(t, db, "api_tokens", "auto_groups")
		if !ok {
			t.Fatalf("api_tokens.auto_groups 不存在（R-01）")
		}
		if dataType != "text" {
			t.Fatalf("api_tokens.auto_groups 类型 = %q，want text（JSON 数组文本）", dataType)
		}
		if nullable != "YES" {
			t.Fatalf("api_tokens.auto_groups 可空性 = %q，want YES", nullable)
		}
		dataType, nullable, def, _ := m1ColumnInfo(t, db, "api_tokens", "cross_group_retry")
		if dataType != "boolean" || nullable != "NO" || !def.Valid || def.String != "false" {
			t.Fatalf("api_tokens.cross_group_retry = (%s, %s, %v)，want (boolean, NO, false)",
				dataType, nullable, def)
		}
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'api_tokens'`).Scan(&n); err != nil {
			t.Fatalf("query api_tokens columns: %v", err)
		}
		if n != 15 {
			t.Fatalf("api_tokens 列数 = %d，want 15（13 + R-01 两列）", n)
		}
	})

	// (c) login_sessions：存在、session_id_hash UNIQUE、无 FK（§22.10 逻辑引用）。
	t.Run("(c) login_sessions", func(t *testing.T) {
		if !m1TableExists(t, db, "login_sessions") {
			t.Fatalf("login_sessions 不存在")
		}
		if !m1UniqueOnColumn(t, db, "login_sessions", "session_id_hash") {
			t.Fatalf("login_sessions.session_id_hash 缺少 UNIQUE 约束")
		}
		if n := m1FKCount(t, db, "login_sessions"); n != 0 {
			t.Fatalf("login_sessions 上存在 %d 个 FK，want 0（platform-01 §22.10 逻辑引用）", n)
		}
	})

	// (d) login_encryption_keys.kid UNIQUE（I-7 双槽按 kid 选私钥）。
	t.Run("(d) login_encryption_keys.kid", func(t *testing.T) {
		if !m1TableExists(t, db, "login_encryption_keys") {
			t.Fatalf("login_encryption_keys 不存在")
		}
		if !m1UniqueOnColumn(t, db, "login_encryption_keys", "kid") {
			t.Fatalf("login_encryption_keys.kid 缺少 UNIQUE 约束")
		}
	})

	// (e) EnumChecks 与 DB CHECK 逐列相等（沿 M0 Task 7 双向口径）。
	t.Run("(e) EnumChecks 与 DB CHECK 逐列相等", func(t *testing.T) {
		checks := loadChecks(t, db)
		keys := []string{"users.role", "users.status", "api_tokens.status", "user_groups.status"}
		// 正向：EnumChecks 的每个键都必须有同名 CHECK，且成员集合相等。
		for _, key := range keys {
			table, column, ok := strings.Cut(key, ".")
			if !ok {
				t.Fatalf("EnumChecks 键 %q 须为 <table>.<column>", key)
			}
			conname := table + "_" + column + "_chk"
			found := false
			for _, c := range checks[table] {
				if c.Name != conname {
					continue
				}
				found = true
				members, isEnum := parseEnumMembers(c.Def)
				if !isEnum {
					t.Fatalf("%s 不是可解析的枚举 CHECK：%s", conname, c.Def)
				}
				if !sameSet(members, EnumChecks[key]) {
					t.Fatalf("%s：DB CHECK=%v，EnumChecks=%v", key, members, EnumChecks[key])
				}
			}
			if !found {
				t.Fatalf("%s：找不到 CHECK 约束 %s", key, conname)
			}
		}
		// 反向：这三张表上的每个枚举 CHECK 都必须登记在 EnumChecks。
		for _, table := range []string{"users", "api_tokens", "user_groups"} {
			for _, c := range checks[table] {
				if !c.IsEnum {
					continue
				}
				column := strings.TrimSuffix(strings.TrimPrefix(c.Name, table+"_"), "_chk")
				key := table + "." + column
				want, ok := EnumChecks[key]
				if !ok {
					t.Fatalf("DB 枚举 CHECK %s 未登记在 EnumChecks（Members=%v）", c.Name, c.Members)
				}
				if !sameSet(c.Members, want) {
					t.Fatalf("%s：DB CHECK=%v，EnumChecks=%v", key, c.Members, want)
				}
			}
		}
	})
}
