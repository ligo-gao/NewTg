//go:build integration

package store

import (
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	// pgx/v5 stdlib 驱动为 Task 4 冻结选型（platform-04 §9.1 同款）。
	_ "github.com/jackc/pgx/v5/stdlib"
)

// 本文件是主库 schema 集成验收：M0 init 迁移（platform-01 §0.1 正册 18 表减
// usage_logs/audit_logs = 16 表）+ M1 认证族 6 表（R-07）。
// 权威：docs/platform-01-数据库字段汇总.md 各 § + platform-04 DDL 规范。
// 运行：NEWTG_TEST_DB_DSN=postgres://... go test -tags=integration ./internal/store/ -v

// wantColumns 逐表列数（platform-01 §0.1 计数，硬约束）。
// M0 = 16 表；M1 追加认证族 6 表（R-07）并给 users/api_tokens 各补列（R-02/R-01），
// 故 users 23→24、api_tokens 13→15，表数 16→22。
// M6c Task 4 追加通知模板 2 表（modules-08 §3.9），表数 22→24。
var wantColumns = map[string]int{
	"users":                   24,
	"api_tokens":              15,
	"user_groups":             9,
	"system_settings":         5,
	"channels":                22,
	"model_pricing":           13,
	"group_pricing":           10,
	"redemption_codes":        11,
	"orders":                  23,
	"wallet_transactions":     11,
	"rankings_snapshot_runs":  8,
	"rankings_model_stats":    10,
	"rankings_provider_stats": 5,
	"rankings_timeseries":     4,
	"rankings_entities":       3,
	"request_registry":        9,
	// M1 认证族（modules-01 §3.5/§3.6/§3.13/§3.14/§3.19 + platform-01 §22.10）
	"login_sessions":         4,
	"login_encryption_keys":  6,
	"user_oauth_bindings":    7,
	"user_passkeys":          9,
	"custom_oauth_providers": 25,
	"checkins":               5,
	// M6c Task 4 通知模板体系（modules-08 §3.9.2 / §3.9.5）：
	// 模板 14 列、投递记录 15 列；两表都无 FK（逻辑引用 users.id）。
	"notification_templates":  14,
	"notification_deliveries": 15,
}

// 三个部分唯一索引（platform-04 §3.3.3；名称钉死）。
var wantPartialUniqueIndexes = []string{
	"users_email_uq",
	"wallet_release_refid_uq",
	"uq_wallet_tx_signup_grant",
}

// 两处 GIN jsonb_path_ops（platform-04 §3.3.3 / GC-5）。
var wantGINIndexes = []string{
	"channels_supported_models_gin",
	"channels_group_tags_gin",
}

// request_registry 的两个索引（platform-01 §18 / platform-04 §4.4）。
var wantRequestRegistryIndexes = map[string]bool{
	"request_registry_inflight_idx": true, // 部分索引
	"request_registry_created_idx":  false,
}

// 财务表无 FK（platform-04 §3.4 V5：逻辑引用，不设 FK）。
var wantNoFKTables = []string{
	"wallet_transactions",
	"orders",
	"redemption_codes",
}

// 以 GENERATED ALWAYS AS IDENTITY 作单列代理主键的表（platform-04 §3.1）：
// M0 10 张 + M1 认证族 6 张（含新增的 checkins）。
var wantIdentityPKTables = []string{
	"users",
	"api_tokens",
	"user_groups",
	"channels",
	"model_pricing",
	"group_pricing",
	"redemption_codes",
	"orders",
	"wallet_transactions",
	"rankings_snapshot_runs",
	"login_sessions",
	"login_encryption_keys",
	"user_oauth_bindings",
	"user_passkeys",
	"custom_oauth_providers",
	"checkins",
	// M6c Task 4：两张通知表都以 GENERATED ALWAYS AS IDENTITY 作 PK。
	"notification_templates",
	"notification_deliveries",
}

// identityPKExceptions：4 张「自然键例外」表的 PK（均非 identity）：
// system_settings."key" text 自然键（platform-01 §4）、request_registry.request_id
// uuid 自然键（§18 / platform-04 V9）、rankings_timeseries 与 rankings_entities 的
// 自然唯一键（§16/§17）。
//
// 另有两张以 run_id（FK → rankings_snapshot_runs.id，V5）领头的复合自然键表
// （runIDLedNaturalPKs）。合起来：22 表（M0 16 + M1 6）= 16 张单列 id identity
// + 4 张自然键 + 2 张 run_id 领头自然键；**非 identity PK 的表恰 6 张**，由
// TestMainSchemaNonIdentityPKSet 断言。
var identityPKExceptions = map[string][]string{
	"system_settings":     {"key"},
	"rankings_timeseries": {"model", "granularity", "bucket_start"},
	"rankings_entities":   {"entity", "kind"},
	"request_registry":    {"request_id"},
}

// runIDLedNaturalPKs：以 FK run_id 领头的复合自然唯一键（platform-01 §14/§15）。
var runIDLedNaturalPKs = map[string][]string{
	"rankings_model_stats":    {"run_id", "model"},
	"rankings_provider_stats": {"run_id", "author"},
}

// wantFKs 是 platform-04 §3.4 V5 的完整 FK 清单（全库真实外键恰 6 条）。
// 键形如 "<table>.<column>-><ref_table>.<ref_column>:<ON DELETE>"。
var wantFKs = []string{
	"api_tokens.user_id->users.id:CASCADE",
	"api_tokens.group->user_groups.name:RESTRICT",
	"users.group->user_groups.name:RESTRICT",
	"group_pricing.group->user_groups.name:RESTRICT",
	"rankings_model_stats.run_id->rankings_snapshot_runs.id:CASCADE",
	"rankings_provider_stats.run_id->rankings_snapshot_runs.id:CASCADE",
}

func openTestDB(t *testing.T) *sql.DB {
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
	t.Cleanup(func() { db.Close() })
	return db
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestMainSchemaTableCount：（a）主库用户表恰 22 张（M0 16 + M1 认证族 6，除 schema_migrations）。
func TestMainSchemaTableCount(t *testing.T) {
	db := openTestDB(t)
	rows, err := db.Query(`SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if n != "schema_migrations" { // golang-migrate 版本位表
			got = append(got, n)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := sortedKeys(wantColumns)
	sort.Strings(got)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("表集合不符\n got: %v\nwant: %v", got, want)
	}
}

// TestMainSchemaColumnCounts：（b）逐表列数 == 登记册计数（M0 16 表 + M1 6 表，共 22 个断言）。
func TestMainSchemaColumnCounts(t *testing.T) {
	db := openTestDB(t)
	for _, table := range sortedKeys(wantColumns) {
		table := table
		t.Run(table, func(t *testing.T) {
			var n int
			err := db.QueryRow(`SELECT count(*) FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = $1`, table).Scan(&n)
			if err != nil {
				t.Fatalf("query columns: %v", err)
			}
			if n != wantColumns[table] {
				t.Fatalf("%s 列数 = %d，want %d（platform-01 §0.1）", table, n, wantColumns[table])
			}
		})
	}
}

// indexInfo 读取单个索引的唯一性 / 部分性 / 定义。
func indexInfo(t *testing.T, db *sql.DB, name string) (def string, unique, partial bool, ok bool) {
	t.Helper()
	err := db.QueryRow(`SELECT pg_get_indexdef(i.indexrelid), i.indisunique, i.indpred IS NOT NULL
		FROM pg_index i
		JOIN pg_class idx ON idx.oid = i.indexrelid
		JOIN pg_namespace nsp ON nsp.oid = idx.relnamespace
		WHERE nsp.nspname = current_schema() AND idx.relname = $1`, name).Scan(&def, &unique, &partial)
	if err == sql.ErrNoRows {
		return "", false, false, false
	}
	if err != nil {
		t.Fatalf("query index %s: %v", name, err)
	}
	return def, unique, partial, true
}

// TestMainSchemaPartialUniqueIndexes：（c）三处部分唯一索引存在且确为 UNIQUE + 部分。
func TestMainSchemaPartialUniqueIndexes(t *testing.T) {
	db := openTestDB(t)
	for _, idx := range wantPartialUniqueIndexes {
		idx := idx
		t.Run(idx, func(t *testing.T) {
			_, unique, partial, ok := indexInfo(t, db, idx)
			if !ok {
				t.Fatalf("部分唯一索引 %s 不存在", idx)
			}
			if !unique || !partial {
				t.Fatalf("%s unique=%v partial=%v，want true/true", idx, unique, partial)
			}
		})
	}
}

// TestMainSchemaRuntimeIndexes：channels 两处 GIN jsonb_path_ops + request_registry 两索引。
func TestMainSchemaRuntimeIndexes(t *testing.T) {
	db := openTestDB(t)
	for _, idx := range wantGINIndexes {
		idx := idx
		t.Run(idx, func(t *testing.T) {
			def, unique, _, ok := indexInfo(t, db, idx)
			if !ok {
				t.Fatalf("GIN 索引 %s 不存在", idx)
			}
			if unique {
				t.Fatalf("%s 不应为 UNIQUE", idx)
			}
			if !strings.Contains(def, "USING gin") || !strings.Contains(def, "jsonb_path_ops") {
				t.Fatalf("%s 定义不符，want GIN jsonb_path_ops：%s", idx, def)
			}
		})
	}
	for idx, wantPartial := range wantRequestRegistryIndexes {
		idx, wantPartial := idx, wantPartial
		t.Run(idx, func(t *testing.T) {
			def, _, partial, ok := indexInfo(t, db, idx)
			if !ok {
				t.Fatalf("request_registry 索引 %s 不存在", idx)
			}
			if partial != wantPartial {
				t.Fatalf("%s partial=%v，want %v（定义：%s）", idx, partial, wantPartial, def)
			}
		})
	}
}

// TestMainSchemaNoFKOnFinancialTables：（d）财务表无 FK（V5 财务逻辑引用）。
func TestMainSchemaNoFKOnFinancialTables(t *testing.T) {
	db := openTestDB(t)
	for _, table := range wantNoFKTables {
		table := table
		t.Run(table, func(t *testing.T) {
			var n int
			err := db.QueryRow(`SELECT count(*) FROM information_schema.table_constraints
				WHERE constraint_type = 'FOREIGN KEY'
				  AND table_schema = current_schema() AND table_name = $1`, table).Scan(&n)
			if err != nil {
				t.Fatalf("query fk: %v", err)
			}
			if n != 0 {
				t.Fatalf("%s 上存在 %d 个 FK，want 0（platform-04 §3.4 V5）", table, n)
			}
		})
	}
}

// TestMainSchemaFKsMatchV5：全库真实 FK 恰 6 条，引用列与 ON DELETE 逐条对齐 V5。
func TestMainSchemaFKsMatchV5(t *testing.T) {
	db := openTestDB(t)
	rows, err := db.Query(`SELECT cl.relname,
			(SELECT a.attname FROM pg_attribute a WHERE a.attrelid = con.conrelid AND a.attnum = con.conkey[1]),
			fcl.relname,
			(SELECT a.attname FROM pg_attribute a WHERE a.attrelid = con.confrelid AND a.attnum = con.confkey[1]),
			con.confdeltype
		FROM pg_constraint con
		JOIN pg_class cl ON cl.oid = con.conrelid
		JOIN pg_namespace nsp ON nsp.oid = cl.relnamespace
		JOIN pg_class fcl ON fcl.oid = con.confrelid
		WHERE con.contype = 'f' AND nsp.nspname = current_schema()`)
	if err != nil {
		t.Fatalf("query fks: %v", err)
	}
	defer rows.Close()
	deleteAction := map[string]string{"c": "CASCADE", "r": "RESTRICT", "a": "NO ACTION", "n": "SET NULL", "d": "SET DEFAULT"}
	var got []string
	for rows.Next() {
		var table, column, refTable, refColumn, action string
		if err := rows.Scan(&table, &column, &refTable, &refColumn, &action); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, fmt.Sprintf("%s.%s->%s.%s:%s", table, column, refTable, refColumn, deleteAction[action]))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(got)
	want := append([]string{}, wantFKs...)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("FK 集合不符（platform-04 §3.4 V5）\n got: %v\nwant: %v", got, want)
	}
}

type pkCol struct {
	Name     string
	IsIdent  string
	IdentGen string
}

func pkColumns(t *testing.T, db *sql.DB, table string) []pkCol {
	t.Helper()
	rows, err := db.Query(`SELECT kcu.column_name, c.is_identity, c.identity_generation
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_schema = kcu.constraint_schema
		 AND tc.constraint_name  = kcu.constraint_name
		 AND tc.table_name       = kcu.table_name
		JOIN information_schema.columns c
		  ON c.table_schema = kcu.table_schema
		 AND c.table_name   = kcu.table_name
		 AND c.column_name  = kcu.column_name
		WHERE tc.constraint_type = 'PRIMARY KEY'
		  AND tc.table_schema = current_schema() AND tc.table_name = $1
		ORDER BY kcu.ordinal_position`, table)
	if err != nil {
		t.Fatalf("query pk of %s: %v", table, err)
	}
	defer rows.Close()
	var out []pkCol
	for rows.Next() {
		var col pkCol
		var gen sql.NullString
		if err := rows.Scan(&col.Name, &col.IsIdent, &gen); err != nil {
			t.Fatalf("scan: %v", err)
		}
		col.IdentGen = gen.String
		out = append(out, col)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestMainSchemaIdentityPK：（e）identity PK + 例外清单恰 4 项 + run_id 领头复合自然键。
func TestMainSchemaIdentityPK(t *testing.T) {
	if len(identityPKExceptions) != 4 {
		t.Fatalf("自然键例外清单须恰 4 项（另有 2 张 run_id 领头的复合自然键），实际 %d 项", len(identityPKExceptions))
	}
	db := openTestDB(t)

	// (e.1) 16 张代理主键表（M0 10 + M1 6）：单列 id 且 GENERATED ALWAYS AS IDENTITY。
	for _, table := range wantIdentityPKTables {
		table := table
		t.Run(table, func(t *testing.T) {
			cols := pkColumns(t, db, table)
			if len(cols) != 1 || cols[0].Name != "id" {
				names := make([]string, 0, len(cols))
				for _, c := range cols {
					names = append(names, c.Name)
				}
				t.Fatalf("%s PK = %v，want 单列 id", table, names)
			}
			if cols[0].IsIdent != "YES" || cols[0].IdentGen != "ALWAYS" {
				t.Fatalf("%s.id is_identity=%q identity_generation=%q，want YES/ALWAYS（GENERATED ALWAYS AS IDENTITY）",
					table, cols[0].IsIdent, cols[0].IdentGen)
			}
		})
	}

	// (e.2) 4 项例外：自然键组成与 spec 逐列一致，且均非 identity。
	for table, wantCols := range identityPKExceptions {
		table, wantCols := table, wantCols
		t.Run(table, func(t *testing.T) {
			cols := pkColumns(t, db, table)
			got := make([]string, 0, len(cols))
			for _, c := range cols {
				if c.IsIdent == "YES" {
					t.Fatalf("%s.%s 不应为 identity（自然键例外）", table, c.Name)
				}
				got = append(got, c.Name)
			}
			if fmt.Sprint(got) != fmt.Sprint(wantCols) {
				t.Fatalf("%s PK = %v，want %v", table, got, wantCols)
			}
		})
	}

	// (e.3) run_id 领头的复合自然键：组成正确、无 identity、run_id 上 FK 存在（V5）。
	for table, wantCols := range runIDLedNaturalPKs {
		table, wantCols := table, wantCols
		t.Run(table, func(t *testing.T) {
			cols := pkColumns(t, db, table)
			got := make([]string, 0, len(cols))
			for _, c := range cols {
				if c.IsIdent == "YES" {
					t.Fatalf("%s.%s 不应为 identity（复合自然键，不引入代理 id）", table, c.Name)
				}
				got = append(got, c.Name)
			}
			if fmt.Sprint(got) != fmt.Sprint(wantCols) {
				t.Fatalf("%s PK = %v，want %v", table, got, wantCols)
			}
			var n int
			err := db.QueryRow(`SELECT count(*) FROM information_schema.table_constraints
				WHERE constraint_type = 'FOREIGN KEY'
				  AND table_schema = current_schema() AND table_name = $1`, table).Scan(&n)
			if err != nil {
				t.Fatalf("query fk: %v", err)
			}
			if n != 1 {
				t.Fatalf("%s 上 FK 数 = %d，want 1（run_id → rankings_snapshot_runs(id) CASCADE）", table, n)
			}
		})
	}
}

// TestMainSchemaNonIdentityPKSet：非 identity PK 的表恰 6 张 =
// 4 张自然键例外（identityPKExceptions）+ 2 张 run_id 领头的复合自然键
// （runIDLedNaturalPKs）；其余（M0 10 + M1 6）均为单列 id + GENERATED ALWAYS AS IDENTITY。
func TestMainSchemaNonIdentityPKSet(t *testing.T) {
	db := openTestDB(t)
	wantNonIdentity := map[string]bool{}
	for table := range identityPKExceptions {
		wantNonIdentity[table] = true
	}
	for table := range runIDLedNaturalPKs {
		wantNonIdentity[table] = true
	}
	if len(wantNonIdentity) != 6 {
		t.Fatalf("非 identity PK 表须恰 6 张（4 自然键 + 2 run_id 领头），实际 %d 张", len(wantNonIdentity))
	}

	var gotNonIdentity, gotIdentity []string
	for _, table := range sortedKeys(wantColumns) {
		cols := pkColumns(t, db, table)
		if len(cols) == 1 && cols[0].Name == "id" && cols[0].IsIdent == "YES" && cols[0].IdentGen == "ALWAYS" {
			gotIdentity = append(gotIdentity, table)
			continue
		}
		gotNonIdentity = append(gotNonIdentity, table)
	}
	wantIdentity := append([]string{}, wantIdentityPKTables...)
	sort.Strings(wantIdentity)
	if fmt.Sprint(gotIdentity) != fmt.Sprint(wantIdentity) {
		t.Fatalf("identity PK 表集合不符\n got: %v\nwant: %v", gotIdentity, wantIdentity)
	}
	if fmt.Sprint(gotNonIdentity) != fmt.Sprint(sortedKeys(wantNonIdentity)) {
		t.Fatalf("非 identity PK 表集合不符\n got: %v\nwant: %v",
			gotNonIdentity, sortedKeys(wantNonIdentity))
	}
}

// documentedChecks：platform-01 明列、易被遗漏的非枚举 CHECK（评审补充覆盖）。
// R-24 的 6 条 §6/§10 约束一并登记：后续迁移若静默删除或放宽其中任一条，
// 这里按「约束存在 + 定义片段」双断言直接变红。
var documentedChecks = []struct {
	Table     string
	Name      string
	Fragments []string
}{
	{
		Table: "wallet_transactions", Name: "wallet_transactions_note_len_chk",
		Fragments: []string{"note", "char_length", "200"},
	},
	{
		Table: "wallet_transactions", Name: "wallet_transactions_admin_note_chk",
		Fragments: []string{"admin_adjustment", "note IS NOT NULL"},
	},
	{
		Table: "channels", Name: "channels_group_tags_chk",
		Fragments: []string{"group_tags", "jsonb_typeof", "jsonb_array_length", ">= 1"},
	},
	// R-24 / platform-01 §6
	{
		Table: "channels", Name: "channels_name_len_chk",
		Fragments: []string{"char_length", "btrim", "64"},
	},
	{
		Table: "channels", Name: "channels_test_model_in_models_chk",
		Fragments: []string{"test_model", "supported_models", "?"},
	},
	{
		Table: "channels", Name: "channels_tag_chk",
		Fragments: []string{"char_length", "btrim", "[[:alnum:]"},
	},
	// R-24 / platform-01 §10
	{
		Table: "orders", Name: "orders_currency_chk",
		Fragments: []string{"currency", "A-Z"},
	},
	{
		Table: "orders", Name: "orders_provider_currency_chk",
		Fragments: []string{"provider_currency IS NULL", "A-Z"},
	},
	{
		Table: "orders", Name: "orders_fx_rate_pair_chk",
		Fragments: []string{"fx_rate_num IS NULL", "fx_rate_den IS NULL"},
	},
}

func expectExecError(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()
	if _, err := db.Exec(stmt); err == nil {
		t.Fatalf("语句应被 CHECK 拒绝但成功执行：%s", stmt)
	}
}

// TestMainSchemaDocumentedConstraints：platform-01 §6 / §10 / §11 的文档语义必须由
// DB 兜底（约束存在 + 负向 INSERT 被拒），防止「只声明未落 DDL」的遗漏再次发生。
func TestMainSchemaDocumentedConstraints(t *testing.T) {
	db := openTestDB(t)
	checks := loadChecks(t, db)
	for _, want := range documentedChecks {
		found := false
		for _, c := range checks[want.Table] {
			if c.Name != want.Name {
				continue
			}
			found = true
			for _, frag := range want.Fragments {
				if !strings.Contains(c.Def, frag) {
					t.Fatalf("%s 定义缺少 %q：%s", want.Name, frag, c.Def)
				}
			}
		}
		if !found {
			t.Fatalf("%s.%s 不存在", want.Table, want.Name)
		}
	}

	t.Run("admin_adjustment 缺 note 被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO wallet_transactions (user_id, type, amount, balance_after)
			VALUES (1, 'admin_adjustment', 1, 1)`)
	})
	t.Run("空字符串 note 被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO wallet_transactions (user_id, type, amount, balance_after, note)
			VALUES (1, 'topup', 1, 1, '')`)
	})
	t.Run("group_tags 空数组被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO channels (name, type, credentials, supported_models, group_tags)
			VALUES ('chk_probe', 'openai', decode('00', 'hex'), '["m"]'::jsonb, '[]'::jsonb)`)
	})

	// R-24 负向覆盖：§6 / §10 的约束必须实际拒绝违法数据，而不只是存在于 catalog。
	t.Run("纯空白 name 被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO channels (name, type, credentials, supported_models)
			VALUES (E' \t\n', 'openai', decode('00', 'hex'), '["m"]'::jsonb)`)
	})
	t.Run("test_model 非 supported_models 成员被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO channels (name, type, credentials, supported_models, test_model)
			VALUES ('chk_tm', 'openai', decode('00', 'hex'), '["m"]'::jsonb, 'not-a-model')`)
	})
	t.Run("空字符串 tag 被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO channels (name, type, credentials, supported_models, tag)
			VALUES ('chk_tag_empty', 'openai', decode('00', 'hex'), '["m"]'::jsonb, '')`)
	})
	t.Run("纯空白 tag 被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO channels (name, type, credentials, supported_models, tag)
			VALUES ('chk_tag_space', 'openai', decode('00', 'hex'), '["m"]'::jsonb, '   ')`)
	})
	// 白名单字符集分支：长度/btrim 都合法，但 `/` 不在允许集合内；不测这一条时
	// 约束被放宽成「只查长度与空白」也会保持绿色。
	t.Run("tag 非法字符被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO channels (name, type, credentials, supported_models, tag)
			VALUES ('chk_tag_charset', 'openai', decode('00', 'hex'), '["m"]'::jsonb, 'bad/tag')`)
	})
	// 白名单约束是双向的：拒绝非法字符的同时必须接收集合内字符。CJK 与连字符走
	// 显式 Unicode 区段（不依赖 DB locale），用回滚事务验证正向分支，避免在多包共享
	// 的库中留下数据（channels.name 全局唯一，重复跑会撞唯一键）。
	t.Run("CJK 与连字符 tag 被接受", func(t *testing.T) {
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`INSERT INTO channels (name, type, credentials, supported_models, tag)
			VALUES ('chk_tag_cjk', 'openai', decode('00', 'hex'), '["m"]'::jsonb, '中文-渠道_1')`); err != nil {
			t.Fatalf("合法 tag 被误拒：%v", err)
		}
	})
	t.Run("小写 currency 被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO orders (user_id, provider, amount, currency, idempotency_key)
			VALUES (1, 'stripe', 1, 'usd', 'chk_currency_lower')`)
	})
	// provider_currency 是独立的字符集分支（NULL 与非 NULL 两态），只测 currency
	// 无法证明渠道结算币种也被约束；小写三字母必须在非 NULL 分支被拒。
	t.Run("小写 provider_currency 被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO orders (user_id, provider, amount, currency, idempotency_key, provider_currency)
			VALUES (1, 'stripe', 1, 'USD', 'chk_provider_currency_lower', 'usd')`)
	})
	t.Run("fx_rate 半对被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO orders (user_id, provider, amount, currency, idempotency_key, fx_rate_num)
			VALUES (1, 'stripe', 1, 'USD', 'chk_fx_half', 710)`)
	})
}

var (
	// PostgreSQL 的 pg_get_constraintdef 把 IN (...) 规范化为 = ANY (ARRAY[...])；
	// varchar 列还会被包成 = ANY ((ARRAY[...])::text[])（实测 PG16），故容忍额外括号与 cast。
	reAnyArray = regexp.MustCompile(`(?is)=\s*ANY\s*\(\s*\(*\s*ARRAY\s*\[([^\]]*)\]`)
	reInClause = regexp.MustCompile(`(?is)\bIN\s*\(`)
	reStrLit   = regexp.MustCompile(`'((?:[^']|'')*)'`)
	reIntLit   = regexp.MustCompile(`-?\d+`)
	// \btype\b 不会命中 jsonb_typeof（_ 为词字符，两侧均无词边界）。
	reTypeColumn = regexp.MustCompile(`\btype\b`)
)

func parseEnumList(inner string) []string {
	if strs := reStrLit.FindAllStringSubmatch(inner, -1); len(strs) > 0 {
		out := make([]string, 0, len(strs))
		for _, s := range strs {
			out = append(out, strings.ReplaceAll(s[1], "''", "'"))
		}
		return out
	}
	return reIntLit.FindAllString(inner, -1)
}

// parseEnumMembers 把 CHECK 文本的枚举列表解析为成员集合（容差：文本解析）。
func parseEnumMembers(def string) ([]string, bool) {
	if m := reAnyArray.FindStringSubmatch(def); m != nil {
		members := parseEnumList(m[1])
		return members, len(members) > 0
	}
	loc := reInClause.FindStringIndex(def)
	if loc == nil {
		return nil, false
	}
	rest := def[loc[1]:]
	depth := 1
	end := -1
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return nil, false
	}
	members := parseEnumList(rest[:end])
	return members, len(members) > 0
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ca, cb := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(ca)
	sort.Strings(cb)
	for i := range ca {
		if ca[i] != cb[i] {
			return false
		}
	}
	return true
}

type dbCheck struct {
	Name    string
	Def     string
	Members []string
	IsEnum  bool
}

// loadChecks 读取主库 wantColumns 表集上的全部 CHECK 约束。
func loadChecks(t *testing.T, db *sql.DB) map[string][]dbCheck {
	return loadChecksOn(t, db, wantColumns)
}

// loadChecksOn 读取 tables 所列表上的全部 CHECK 约束。tables 同时充当「本库
// 有哪些表」的白名单：主库传 wantColumns，日志库传 logsWantColumns（R-13）。
// 解析与容差口径（pg_get_constraintdef 的 ANY/ARRAY 规范化）对两库共用。
func loadChecksOn(t *testing.T, db *sql.DB, tables map[string]int) map[string][]dbCheck {
	t.Helper()
	rows, err := db.Query(`SELECT rel.relname, con.conname, pg_get_constraintdef(con.oid)
		FROM pg_constraint con
		JOIN pg_class rel ON rel.oid = con.conrelid
		JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
		WHERE nsp.nspname = current_schema() AND con.contype = 'c'
		ORDER BY rel.relname, con.conname`)
	if err != nil {
		t.Fatalf("query checks: %v", err)
	}
	defer rows.Close()
	out := map[string][]dbCheck{}
	for rows.Next() {
		var table string
		var c dbCheck
		if err := rows.Scan(&table, &c.Name, &c.Def); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if _, ok := tables[table]; !ok {
			continue
		}
		c.Members, c.IsEnum = parseEnumMembers(c.Def)
		out[table] = append(out[table], c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestEnumChecksMatchDB：（f）EnumChecks 与 pg_constraint 的 CHECK 枚举子句逐条集合相等
// （双向：EnumChecks 每项必须有同名 CHECK；DB 每个枚举 CHECK 必须登记在 EnumChecks）；
// 另断言 channels.type 无 DB CHECK（platform-04 §6 V6 Tier-2）。
func TestEnumChecksMatchDB(t *testing.T) {
	db := openTestDB(t)
	checks := loadChecks(t, db)

	enumByName := map[string]map[string][]string{} // table -> constraint name -> members
	for table, list := range checks {
		enumByName[table] = map[string][]string{}
		for _, c := range list {
			if c.IsEnum {
				enumByName[table][c.Name] = c.Members
			}
		}
	}

	for _, key := range sortedKeys(EnumChecks) {
		table, column, ok := strings.Cut(key, ".")
		if !ok || table == "" || column == "" {
			t.Fatalf("EnumChecks 键 %q 须为 <table>.<column>", key)
		}
		if _, ok := wantColumns[table]; !ok {
			t.Fatalf("EnumChecks 键 %q 指向 wantColumns 之外的表", key)
		}
		want := EnumChecks[key]
		conname := table + "_" + column + "_chk"
		got, ok := enumByName[table][conname]
		if !ok {
			t.Fatalf("%s：找不到 CHECK 约束 %s（DB 枚举 CHECK：%v）", key, conname, sortedKeys(enumByName[table]))
		}
		if !sameSet(got, want) {
			t.Fatalf("%s：DB CHECK=%v，EnumChecks=%v", key, got, want)
		}
	}

	// 反向：DB 上每个枚举型 CHECK 必须恰对应一个 EnumChecks 键（column-for-column 相等）。
	for table, byName := range enumByName {
		prefix, suffix := table+"_", "_chk"
		for conname, members := range byName {
			if !strings.HasPrefix(conname, prefix) || !strings.HasSuffix(conname, suffix) {
				t.Fatalf("%s 上的枚举 CHECK %s 不符合 <table>_<column>_chk 命名约定", table, conname)
			}
			column := strings.TrimSuffix(strings.TrimPrefix(conname, prefix), suffix)
			key := table + "." + column
			want, ok := EnumChecks[key]
			if !ok {
				t.Fatalf("DB 枚举 CHECK %s 未登记在 EnumChecks（Members=%v）", conname, members)
			}
			if !sameSet(members, want) {
				t.Fatalf("%s：DB CHECK=%v，EnumChecks=%v", key, members, want)
			}
		}
	}

	t.Run("channels.type 无 DB CHECK（Tier-2，V6）", func(t *testing.T) {
		if _, ok := EnumChecks["channels.type"]; ok {
			t.Fatalf("EnumChecks 不得包含 channels.type（Tier-2 由 contract.ChannelType.Valid() 兜底）")
		}
		for _, c := range checks["channels"] {
			if c.Name == "channels_type_chk" || (c.IsEnum && reTypeColumn.MatchString(c.Def)) {
				t.Fatalf("channels.type 不应有 CHECK（V6），发现：%s %s", c.Name, c.Def)
			}
		}
	})
}

// TestEnumChecksValuesSanity：smallint 枚举成员必须是数字文本（与 CHECK 解析口径一致）。
func TestEnumChecksValuesSanity(t *testing.T) {
	intKeys := map[string]bool{
		"user_groups.status":                true,
		"custom_oauth_providers.auth_style": true,
		"channels.status":                   true,
		"model_pricing.mode":                true,
		"model_pricing.status":              true,
		"group_pricing.status":              true,
	}
	for key, members := range EnumChecks {
		for _, m := range members {
			_, err := strconv.Atoi(m)
			if intKeys[key] && err != nil {
				t.Fatalf("%s 为 smallint 枚举，成员 %q 须为数字", key, m)
			}
		}
	}
}
