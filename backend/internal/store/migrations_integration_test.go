//go:build integration

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"newtg/gateway/migrations"
)

// 本文件验证 W8/MJ-A0 迁移 20261023090000_tasks_mj_family 的 up/down 往返。
//
// 为什么不直接在当前测试库上 down 一步：schema_integration_test 的其余用例
// 依赖 tasks 的**最新**形状，回退会污染同一轮测试。这里在测试库里建一个
// 临时 schema，并通过 DSN 的 search_path 让 golang-migrate 把全部表
// （含 schema_migrations）建在该 schema 内；结束后 DROP SCHEMA CASCADE。
//
// 这个用例专门捕获「只改 up、不改 down」的回归：
//   - down 若为空：m.Steps(-1) 成功但 CHECK 仍是 {plugin,mj}，
//     随后「down 后插入 mj 必须失败」的断言会红；
//   - down 若恢复 CHECK 但漏掉 DELETE mj 行：ADD CONSTRAINT 会因存量行
//     违约而失败，m.Steps(-1) 直接红。
func TestTasksMJFamilyMigrationUpDown(t *testing.T) {
	baseDSN := os.Getenv("NEWTG_TEST_DB_DSN")
	if baseDSN == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}

	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	if err := admin.Ping(); err != nil {
		t.Fatalf("ping admin db: %v", err)
	}
	// 标识符由时间戳生成，只含数字，无注入面。
	schema := fmt.Sprintf("mja0_mig_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
	})

	dsn, err := dsnWithSearchPath(baseDSN, schema)
	if err != nil {
		t.Fatalf("build scoped dsn: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open scoped db: %v", err)
	}

	m := newTestMigrator(t, db)
	t.Cleanup(func() { _, _ = m.Close() })

	// 只前滚到本用例要验证的两个迁移（family 090000 + MJ 索引 091000），
	// 而不是全部迁移：后续迁移（如 W10-N9 的终态保留索引）排在后面，会改变
	// down 步数的语义。固定目标让用例只对这两步的 up/down 负责。
	//
	// 目标必须包含 091000：本用例断言它创建的
	// `tasks_mj_user_upstream_idx`/`tasks_mj_upstream_idx` 在 up 后存在、
	// down 后消失。只前滚到 family 迁移（090000）时这两个索引尚未创建，
	// 断言会在正确的迁移链上稳定失败（2026-10-22 W8/MJ-c 集成门禁发现）。
	if err := m.Migrate(20261023091000); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up to 20261023091000: %v", err)
	}

	// up 后：family='mj' 可写入，未知值仍被 CHECK 拒绝，领取索引存在。
	execTaskInsert(t, db, "mig_mj_before_down", "mj")
	if err := execTaskInsertErr(db, "mig_bogus", "bogus"); err == nil {
		t.Fatal("unknown family must be rejected by the extended CHECK")
	}
	if !indexExists(t, db, "tasks_family_pending_idx") {
		t.Fatal("tasks_family_pending_idx missing after up")
	}
	// W8/MJ-a2 的归属索引在同一链上（family 迁移之后的一步）。
	if !indexExists(t, db, "tasks_mj_user_upstream_idx") || !indexExists(t, db, "tasks_mj_upstream_idx") {
		t.Fatal("MJ upstream indexes missing after up")
	}

	// down 两步：先回退 W8/MJ-a2 的索引迁移，再回退 family 迁移。family 的
	// down 必须先删 MJ 行（否则 CHECK 恢复失败），再恢复单值 CHECK。
	// 步数写成 2 而不是「down 到 family 版本」：测试冻结的是当前迁移链的
	// 直接前驱关系；以后在 family 之后再加迁移时，这里必须同步增加步数
	//（新迁移不会改变 family down 的语义，只是多了要回退的一层）。
	if err := m.Steps(-2); err != nil {
		t.Fatalf("migrate down 2: %v", err)
	}
	var mjRows int
	if err := db.QueryRow(`SELECT count(*) FROM tasks WHERE family = 'mj'`).Scan(&mjRows); err != nil {
		t.Fatalf("count mj rows: %v", err)
	}
	if mjRows != 0 {
		t.Fatalf("down must remove mj rows before restoring the single-value CHECK, left %d", mjRows)
	}
	if err := execTaskInsertErr(db, "mig_mj_after_down", "mj"); err == nil {
		t.Fatal("down must restore family CHECK to {plugin}")
	}
	if indexExists(t, db, "tasks_family_pending_idx") {
		t.Fatal("down must drop tasks_family_pending_idx")
	}
	if indexExists(t, db, "tasks_mj_user_upstream_idx") || indexExists(t, db, "tasks_mj_upstream_idx") {
		t.Fatal("down must drop MJ upstream indexes")
	}
	// 插件行在 down 后仍可写（CHECK 没有误伤合法族）。
	execTaskInsert(t, db, "mig_plugin_after_down", "plugin")

	// up 两步：迁移可反转（down 后再次前滚仍接受 mj，索引恢复）。
	if err := m.Steps(2); err != nil {
		t.Fatalf("migrate up 2: %v", err)
	}
	execTaskInsert(t, db, "mig_mj_after_up", "mj")
	if !indexExists(t, db, "tasks_family_pending_idx") {
		t.Fatal("tasks_family_pending_idx missing after re-up")
	}
	if !indexExists(t, db, "tasks_mj_user_upstream_idx") || !indexExists(t, db, "tasks_mj_upstream_idx") {
		t.Fatal("MJ upstream indexes missing after re-up")
	}
}

// TestTasksTerminalRetentionIndexMigrationUpDown 验证 W10-N9 的保留索引迁移
// 可反转：up 建索引（含终态/待补偿谓词），down 删除，再次 up 恢复。
//
// 固定目标版本 20261023120000（当前 tip），与上面的 MJ 用例同一套临时
// schema 手法；断言的 finally 状态是「索引存在」，不依赖其它迁移的副作用。
func TestTasksTerminalRetentionIndexMigrationUpDown(t *testing.T) {
	baseDSN := os.Getenv("NEWTG_TEST_DB_DSN")
	if baseDSN == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}

	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	if err := admin.Ping(); err != nil {
		t.Fatalf("ping admin db: %v", err)
	}
	schema := fmt.Sprintf("w10n9_mig_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
	})

	dsn, err := dsnWithSearchPath(baseDSN, schema)
	if err != nil {
		t.Fatalf("build scoped dsn: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open scoped db: %v", err)
	}
	m := newTestMigrator(t, db)
	t.Cleanup(func() { _, _ = m.Close() })

	if err := m.Migrate(20261023120000); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up to 20261023120000: %v", err)
	}
	if !indexExists(t, db, "tasks_terminal_retention_idx") {
		t.Fatal("tasks_terminal_retention_idx missing after up")
	}
	var def string
	if err := db.QueryRow(`SELECT pg_get_indexdef('tasks_terminal_retention_idx'::regclass)`).Scan(&def); err != nil {
		t.Fatalf("read index definition: %v", err)
	}
	for _, want := range []string{"success", "failure", "billing_pending"} {
		if !strings.Contains(def, want) {
			t.Fatalf("index definition missing %q: %s", want, def)
		}
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate down 1: %v", err)
	}
	if indexExists(t, db, "tasks_terminal_retention_idx") {
		t.Fatal("down must drop tasks_terminal_retention_idx")
	}
	// down 不改变数据面：终态任务仍可写入。
	if err := execTaskInsertErr(db, "mig_terminal_after_down", "plugin"); err != nil {
		t.Fatalf("insert after down: %v", err)
	}

	if err := m.Steps(1); err != nil {
		t.Fatalf("migrate up 1: %v", err)
	}
	if !indexExists(t, db, "tasks_terminal_retention_idx") {
		t.Fatal("tasks_terminal_retention_idx missing after re-up")
	}
}

// ---- 上游账号回填（20261028100000_upstream_account_backfill）----

// TestUpstreamAccountBackfillMigrationUpDown 验证升级回填：「每个现有渠道建
// 一个同名上游账号并回填 channels.upstream_account_id」（设计 §12.1）。
//
// 冻结的行为：
//   - up 后每个渠道都归属到自己的账号，账号 provider 等于渠道 type，
//     币种/时区为设计规定的 CNY / Asia/Shanghai；
//   - 账号 note 精确指出来源渠道 id —— down 就靠这个溯源标记识别本次迁移
//     建的账号；
//   - down 清空链接、删除无历史数据的账号；再次 up 可重放。
//
// 为什么固定到 20261028100000 而不是「再走一步」：20261028090400
// （usage_integrity_findings）也排在回填迁移之前，回填迁移的编号刻意与相邻
// 迁移拉开（100000）；直接写目标版本不受将来在该区间追加迁移的影响。
func TestUpstreamAccountBackfillMigrationUpDown(t *testing.T) {
	db, m := newUpstreamBackfillDB(t, "upacct_backfill")

	if err := m.Migrate(20261028090300); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up to 20261028090300: %v", err)
	}
	// 升级前的既有数据：两个渠道，都没有上游账号归属。
	channels := []struct {
		id   int64
		name string
		typ  string
	}{
		{insertChannel(t, db, "openai-prod", "openai"), "openai-prod", "openai"},
		{insertChannel(t, db, "anthropic-prod", "anthropic"), "anthropic-prod", "anthropic"},
	}

	if err := m.Migrate(20261028100000); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up to 20261028100000: %v", err)
	}

	var accounts int
	if err := db.QueryRow(`SELECT count(*) FROM upstream_accounts`).Scan(&accounts); err != nil {
		t.Fatalf("count upstream accounts: %v", err)
	}
	if accounts != len(channels) {
		t.Fatalf("want %d upstream accounts after backfill, got %d", len(channels), accounts)
	}

	for _, ch := range channels {
		var (
			accName  string
			provider string
			currency string
			timezone string
			note     string
		)
		// JOIN 命中即证明 upstream_account_id 非空（外键指向存在的账号）。
		err := db.QueryRow(`
			SELECT a.name, a.provider, a.currency, a.timezone, a.note
			FROM channels c
			JOIN upstream_accounts a ON a.id = c.upstream_account_id
			WHERE c.id = $1`, ch.id).Scan(&accName, &provider, &currency, &timezone, &note)
		if err != nil {
			t.Fatalf("channel %d (%s) must be linked to an upstream account: %v", ch.id, ch.name, err)
		}
		if accName != ch.name {
			t.Fatalf("account for channel %d: want name %q, got %q", ch.id, ch.name, accName)
		}
		if provider != ch.typ {
			t.Fatalf("account for channel %d: want provider %q (= channel type), got %q", ch.id, ch.typ, provider)
		}
		if currency != "CNY" || timezone != "Asia/Shanghai" {
			t.Fatalf("account for channel %d: want CNY/Asia/Shanghai, got %q/%q", ch.id, currency, timezone)
		}
		if want := fmt.Sprintf("migrated from channel #%d", ch.id); note != want {
			t.Fatalf("account for channel %d: want note %q, got %q", ch.id, want, note)
		}
	}

	// down：链接清空、账号删除（本次迁移建的账号都没有快照/run，可安全删除）。
	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate down 1: %v", err)
	}
	var linked int
	if err := db.QueryRow(`SELECT count(*) FROM channels WHERE upstream_account_id IS NOT NULL`).Scan(&linked); err != nil {
		t.Fatalf("count linked channels: %v", err)
	}
	if linked != 0 {
		t.Fatalf("down must clear channels.upstream_account_id, %d links left", linked)
	}
	if err := db.QueryRow(`SELECT count(*) FROM upstream_accounts`).Scan(&accounts); err != nil {
		t.Fatalf("count upstream accounts after down: %v", err)
	}
	if accounts != 0 {
		t.Fatalf("down must delete the migrated accounts, %d left", accounts)
	}

	// 再 up：渠道还在，回填可重放（渠道名唯一，因此不会撞名）。
	if err := m.Steps(1); err != nil {
		t.Fatalf("migrate up 1 after down: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM upstream_accounts`).Scan(&accounts); err != nil {
		t.Fatalf("count upstream accounts after re-up: %v", err)
	}
	if accounts != len(channels) {
		t.Fatalf("re-up must recreate %d upstream accounts, got %d", len(channels), accounts)
	}
	if err := db.QueryRow(`SELECT count(*) FROM channels WHERE upstream_account_id IS NOT NULL`).Scan(&linked); err != nil {
		t.Fatalf("count linked channels after re-up: %v", err)
	}
	if linked != len(channels) {
		t.Fatalf("re-up must relink %d channels, got %d", len(channels), linked)
	}
}

// TestUpstreamAccountBackfillGuards 验证回填的两条护栏：
//
//  1. 管理员预先建了同名但 provider 不同的账号时，渠道**不得**挂上去 ——
//     否则对账会拿错误供应商的上游控制台账单去比对（同名不等于同一账期主体）。
//  2. provider 为空或超过 varchar(64) 的渠道被跳过而不是中止升级 ——
//     channels.type 是无长度约束的 text，而 upstream_accounts.provider 是
//     varchar(64)；一条病态数据不该让整个升级失败。
//
// down 对这两类账号都不动：它们没有本次迁移写入的溯源 note。
func TestUpstreamAccountBackfillGuards(t *testing.T) {
	db, m := newUpstreamBackfillDB(t, "upacct_guard")

	if err := m.Migrate(20261028090300); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up to 20261028090300: %v", err)
	}
	var preexistingID int64
	if err := db.QueryRow(`INSERT INTO upstream_accounts (name, provider, note)
		VALUES ('openai-prod', 'anthropic', 'admin created') RETURNING id`).Scan(&preexistingID); err != nil {
		t.Fatalf("insert pre-existing account: %v", err)
	}
	collisionID := insertChannel(t, db, "openai-prod", "openai")
	wideTypeID := insertChannel(t, db, "wide-provider", strings.Repeat("x", 65))
	emptyTypeID := insertChannel(t, db, "empty-provider", "")

	// 超长 type 若未被过滤，这一步会因 provider 超长而中止整个升级；
	// 空 type 不会中止，但会建出 provider = '' 的哑账号，同样应当跳过。
	if err := m.Migrate(20261028100000); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up to 20261028100000 must not abort on pathological channel types: %v", err)
	}

	for _, id := range []int64{collisionID, wideTypeID, emptyTypeID} {
		var accID sql.NullInt64
		if err := db.QueryRow(`SELECT upstream_account_id FROM channels WHERE id = $1`, id).Scan(&accID); err != nil {
			t.Fatalf("read channel %d link: %v", id, err)
		}
		if accID.Valid {
			t.Fatalf("channel %d must stay unlinked (未归属), got account %d", id, accID.Int64)
		}
	}
	var accounts int
	if err := db.QueryRow(`SELECT count(*) FROM upstream_accounts`).Scan(&accounts); err != nil {
		t.Fatalf("count upstream accounts: %v", err)
	}
	if accounts != 1 {
		t.Fatalf("no account may be created for guarded channels, want 1 (the pre-existing one), got %d", accounts)
	}
	var provider, note string
	if err := db.QueryRow(`SELECT provider, note FROM upstream_accounts WHERE id = $1`, preexistingID).Scan(&provider, &note); err != nil {
		t.Fatalf("read pre-existing account: %v", err)
	}
	if provider != "anthropic" || note != "admin created" {
		t.Fatalf("pre-existing account must be untouched, got provider=%q note=%q", provider, note)
	}

	// down：非溯源账号（以及它的任何链接）不在回退范围内。
	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate down 1: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM upstream_accounts`).Scan(&accounts); err != nil {
		t.Fatalf("count upstream accounts after down: %v", err)
	}
	if accounts != 1 {
		t.Fatalf("down must keep accounts without the provenance note, want 1, got %d", accounts)
	}
}

// TestUpstreamAccountBackfillDownKeepsHistory 验证 down 在「升级后已经积累
// 对账数据」的库上仍能成功，且不丢数据：
//
// 迁移建的账号一旦有了快照或 run，就已被 ON DELETE RESTRICT 引用；down 若
// 直接 DELETE 会中止。正确行为是只清空链接、保留账号（有意为之：数据是
// 升级后的真实对账记录），仅删除没有历史的溯源账号。
func TestUpstreamAccountBackfillDownKeepsHistory(t *testing.T) {
	db, m := newUpstreamBackfillDB(t, "upacct_down_history")

	if err := m.Migrate(20261028090300); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up to 20261028090300: %v", err)
	}
	withHistoryID := insertChannel(t, db, "openai-prod", "openai")
	insertChannel(t, db, "anthropic-prod", "anthropic")
	if err := m.Migrate(20261028100000); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up to 20261028100000: %v", err)
	}

	var keptAccountID int64
	if err := db.QueryRow(`SELECT upstream_account_id FROM channels WHERE id = $1`, withHistoryID).Scan(&keptAccountID); err != nil {
		t.Fatalf("read backfilled account id: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO upstream_usage_snapshots
		(account_id, usage_date, upstream_model) VALUES ($1, '2026-10-24', 'gpt-4o')`, keptAccountID); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO upstream_reconciliation_runs
		(account_id, window_start, window_end, triggered_by, token_tolerance_bp,
		 amount_tolerance_minor, quota_per_unit, timezone)
		VALUES ($1, '2026-10-20', '2026-10-23', 'manual', 0, 0, 500000, 'Asia/Shanghai')`, keptAccountID); err != nil {
		t.Fatalf("insert reconciliation run: %v", err)
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("down must succeed with reconciliation data present: %v", err)
	}
	var accounts int
	if err := db.QueryRow(`SELECT count(*) FROM upstream_accounts`).Scan(&accounts); err != nil {
		t.Fatalf("count upstream accounts after down: %v", err)
	}
	if accounts != 1 {
		t.Fatalf("down must keep exactly the account with history, got %d accounts", accounts)
	}
	var survivedID int64
	if err := db.QueryRow(`SELECT id FROM upstream_accounts`).Scan(&survivedID); err != nil {
		t.Fatalf("read surviving account: %v", err)
	}
	if survivedID != keptAccountID {
		t.Fatalf("down must keep account %d (has snapshots/runs), got %d", keptAccountID, survivedID)
	}
	var linked int
	if err := db.QueryRow(`SELECT count(*) FROM channels WHERE upstream_account_id IS NOT NULL`).Scan(&linked); err != nil {
		t.Fatalf("count linked channels: %v", err)
	}
	if linked != 0 {
		t.Fatalf("down must still clear links to migrated accounts, %d left", linked)
	}
	var snapshotCount, runCount int
	if err := db.QueryRow(`SELECT count(*) FROM upstream_usage_snapshots`).Scan(&snapshotCount); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM upstream_reconciliation_runs`).Scan(&runCount); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if snapshotCount != 1 || runCount != 1 {
		t.Fatalf("down must not delete reconciliation data, snapshots=%d runs=%d", snapshotCount, runCount)
	}
}

// TestUpstreamAccountBackfillKeepsProvenanceAfterRename 是「归属确定性」的
// 回归用例：down 会保留带历史数据的迁移账号；若渠道之后改名再前滚，
// 按名字去重挡不住第二个账号（名字已不同），一个渠道就有两个同 provider 的
// 候选账号。此时必须靠溯源 note 挂回原账号，且不得新造账号 ——
// 否则控制台会拿错误的上游账号对账。
func TestUpstreamAccountBackfillKeepsProvenanceAfterRename(t *testing.T) {
	db, m := newUpstreamBackfillDB(t, "upacct_rename")

	if err := m.Migrate(20261028090400); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up to 20261028090400: %v", err)
	}
	channelID := insertChannel(t, db, "openai-prod", "openai")
	if err := m.Migrate(20261028100000); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate up to 20261028100000: %v", err)
	}
	var accountID int64
	if err := db.QueryRow(`SELECT upstream_account_id FROM channels WHERE id = $1`, channelID).Scan(&accountID); err != nil {
		t.Fatalf("read initial link: %v", err)
	}

	// 账号带上历史数据（down 因此保留它，只清链接），随后渠道改名。
	if _, err := db.Exec(`INSERT INTO upstream_usage_snapshots
		(account_id, usage_date, upstream_model) VALUES ($1, '2026-10-24', 'gpt-4o')`, accountID); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
	if _, err := db.Exec(`UPDATE channels SET name = 'openai-prod-renamed' WHERE id = $1`, channelID); err != nil {
		t.Fatalf("rename channel: %v", err)
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate down 1: %v", err)
	}
	var accounts int
	if err := db.QueryRow(`SELECT count(*) FROM upstream_accounts`).Scan(&accounts); err != nil {
		t.Fatalf("count accounts after down: %v", err)
	}
	if accounts != 1 {
		t.Fatalf("down must keep the note-bearing account that has history, got %d accounts", accounts)
	}

	// 重新前滚：名字已变，ON CONFLICT (name) 挡不住重复，只能靠溯源护栏。
	if err := m.Steps(1); err != nil {
		t.Fatalf("migrate up 1 after down: %v", err)
	}
	var relinked int64
	if err := db.QueryRow(`SELECT upstream_account_id FROM channels WHERE id = $1`, channelID).Scan(&relinked); err != nil {
		t.Fatalf("read relinked account: %v", err)
	}
	if relinked != accountID {
		t.Fatalf("re-up must relink channel %d to the note-bearing account %d, got %d", channelID, accountID, relinked)
	}
	if err := db.QueryRow(`SELECT count(*) FROM upstream_accounts`).Scan(&accounts); err != nil {
		t.Fatalf("count accounts after re-up: %v", err)
	}
	if accounts != 1 {
		t.Fatalf("re-up must not mint a second account for the same channel, got %d accounts", accounts)
	}
	var withNote int
	wantNote := fmt.Sprintf("migrated from channel #%d", channelID)
	if err := db.QueryRow(`SELECT count(*) FROM upstream_accounts WHERE note = $1`, wantNote).Scan(&withNote); err != nil {
		t.Fatalf("count provenance accounts: %v", err)
	}
	if withNote != 1 {
		t.Fatalf("want exactly one provenance account for channel %d, got %d", channelID, withNote)
	}
}

// newUpstreamBackfillDB 建一个临时 schema，返回连到该 schema 的 *sql.DB 与
// 迁移器；schema 在清理阶段 DROP CASCADE。手法与上面的用例一致。
func newUpstreamBackfillDB(t *testing.T, prefix string) (*sql.DB, *migrate.Migrate) {
	t.Helper()
	baseDSN := os.Getenv("NEWTG_TEST_DB_DSN")
	if baseDSN == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	if err := admin.Ping(); err != nil {
		t.Fatalf("ping admin db: %v", err)
	}
	schema := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
	})

	dsn, err := dsnWithSearchPath(baseDSN, schema)
	if err != nil {
		t.Fatalf("build scoped dsn: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open scoped db: %v", err)
	}
	m := newTestMigrator(t, db)
	t.Cleanup(func() { _, _ = m.Close() })
	return db, m
}

// insertChannel 插入一个满足 channels NOT NULL 约束的渠道（name、type、
// credentials、supported_models），返回自增 id。
func insertChannel(t *testing.T, db *sql.DB, name, channelType string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`INSERT INTO channels (name, type, credentials, supported_models)
		VALUES ($1, $2, '\x2a'::bytea, '["gpt-4o"]'::jsonb)
		RETURNING id`, name, channelType).Scan(&id); err != nil {
		t.Fatalf("insert channel %q (type %q): %v", name, channelType, err)
	}
	return id
}

// dsnWithSearchPath 在 DSN 上追加 search_path 运行时参数。
//
// 保留原 query（sslmode 等）并覆盖 search_path：pgx 把它作为会话参数下发，
// 因此无 schema 限定的 DDL 与 CURRENT_SCHEMA() 都落在临时 schema 内。
func dsnWithSearchPath(base, schema string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func newTestMigrator(t *testing.T, db *sql.DB) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("open embedded migrations: %v", err)
	}
	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		t.Fatalf("create migrate driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		_ = driver.Close()
		t.Fatalf("create migrator: %v", err)
	}
	return m
}

func execTaskInsert(t *testing.T, db *sql.DB, publicID, family string) {
	t.Helper()
	if err := execTaskInsertErr(db, publicID, family); err != nil {
		t.Fatalf("insert task (family=%s): %v", family, err)
	}
}

func execTaskInsertErr(db *sql.DB, publicID, family string) error {
	_, err := db.Exec(`INSERT INTO tasks
		(public_task_id, family, action, plugin_key, fetch_mode, state, user_id)
		VALUES ($1, $2, 'generate', '', 'poll', 'submitted', 1)`, publicID, family)
	return err
}

func indexExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pg_indexes
		WHERE schemaname = current_schema() AND indexname = $1`, name).Scan(&n); err != nil {
		t.Fatalf("query index %s: %v", name, err)
	}
	return n > 0
}
