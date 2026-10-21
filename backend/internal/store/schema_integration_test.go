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
// M6c Task 13 给 users 追加 default_model（modules-02 §2.14），users 24→25。
// M6d Task 1 追加任务插件三表（modules-08 §3.10.2），表数 24→27。
// M6d Task 7a 给 channels 追加任务插件绑定 2 列（relay-06 §1.3），22→24。
// M6d Task 6a 追加任务数据面主表 tasks（relay-06 §7），表数 27→28。
// D1 追加模型目录两张表（platform-01 §22.7.1/§22.7.2），表数 28→30：
// vendors 7 列、model_meta 20 列。
// D5a 追加订阅套餐数据层三表（billing-08 §2.1/§6.1 + platform-04 §4.5 V10），
// 表数 30→33；并给 orders 补 biz_kind/biz_ref_id（23→25）、users 补
// billing_preference（25→26）。
// D7a 追加站点公告两表（modules-08 F-08-04 / R-D7-01/R-D7-02），表数 33→35。
// B5a 追加供应商时间序列表（rankings_author_timeseries），表数 35→36。
// C2 追加 GPU 资源租赁计费两表（platform-01 §22.13），表数 36→38。
// T4 追加任务批次聚合两表（task_batches / task_batches_items），表数 38→40。
// S1（W2）追加订阅事件流 subscription_events（5 列），表数 40→41。
// S4（W2）追加支付令牌表 payment_methods（11 列）并给 subscriptions 补 3 列
// 自动续费状态，表数 41→42、subscriptions 23→26。
//
//	subscription_plans 17 列、subscriptions 26 列（含 R-D5-03 的 5 个快照列
//	与 S4 的 3 个自动续费状态列）、
//	subscription_pre_consume_records 5 列（R-D5-02 的 subscription_id）。
var wantColumns = map[string]int{
	"users":            26,
	"api_tokens":       15,
	"user_groups":      9,
	"system_settings":  5,
	"channels":         24,
	"model_pricing":    13,
	"group_pricing":    10,
	"redemption_codes": 11,
	// 25 列（platform-01 §0.1）+ S3 退款的 4 列
	// （refunded_amount / refunded_at / refund_reason / refund_requested_at）。
	"orders":                  29,
	"wallet_transactions":     11,
	"rankings_snapshot_runs":  8,
	"rankings_model_stats":    10,
	"rankings_provider_stats": 5,
	"rankings_timeseries":     4,
	"rankings_entities":       3,
	// B5a：供应商时间序列（作者榜前 5 行的桶；自然唯一
	// period+author+granularity+bucket_start）。period 由 B5a 收口迁移加入，
	// 隔离 week/month 共用的 day 桶。
	"rankings_author_timeseries": 5,
	"request_registry":           9,
	// M1 认证族（modules-01 §3.5/§3.6/§3.13/§3.14/§3.19 + platform-01 §22.10）
	"login_sessions":         4,
	"login_encryption_keys":  6,
	"user_oauth_bindings":    7,
	"user_passkeys":          9,
	"custom_oauth_providers": 25,
	"checkins":               5,
	// M6c Task 4 通知模板体系（modules-08 §3.9.2 / §3.9.5）：
	// 模板 14 列、投递记录 15 列 + A5 的 payload/read_at 两列 = 17；
	// 两表都无 FK（逻辑引用 users.id）。
	"notification_templates":  14,
	"notification_deliveries": 17,
	// M6d Task 1 任务插件三表（modules-08 §3.10.2 B-11/B-12/B-13）：
	// 主表 10 列（冻结 DDL 的列清单；platform-01 表清单的「11」是系统侧
	// 参照行计数，本仓库以 modules-08 唯一定义处为准）、版本行 11 列、市场源 7 列。
	"task_plugins":               10,
	"task_plugin_versions":       11,
	"plugin_marketplace_sources": 7,
	// M6d Task 6a 任务数据面主表（relay-06 §7）：契约 §2.5 统一列 13 + 插件
	// 附加列 14 = 27 列。列数与设计表逐行对应，多一列少一列都视为漂移。
	"tasks": 27,
	// T4 / P-04 批次聚合：父表 9 列（归属/状态/计数/时间），关联表 2 列。
	// 子任务本身仍是 tasks 行，不在此复制任何上游/结果字段。
	"task_batches":       9,
	"task_batches_items": 2,
	// D1 模型目录与供应商（platform-01 §22.7.1 的 12+8 列 / §22.7.2 的 7 列）：
	// 两表都以 GENERATED ALWAYS AS IDENTITY 作 PK，且都不设 FK（vendor_id 是逻辑引用）。
	"vendors":    7,
	"model_meta": 20,
	// D5a 订阅套餐数据层（billing-08 §2.1 / §6.1 / platform-04 §4.5 V10）。
	// subscriptions 的列数含 R-D5-03 的 5 个快照列（period_days/reset_on_renew/
	// grace_days/model_scope/concurrency_limit）：少快照列会让「编辑套餐」
	// 追溯已购实例，是设计明确禁止的语义漂移。
	"subscription_plans":               17,
	"subscriptions":                    26,
	"subscription_pre_consume_records": 5,
	// S1 订阅状态事件流（W2）：subscription_id / seq / kind / detail /
	// created_at 五列，主键 (subscription_id, seq) 即读路径索引。
	"subscription_events": 5,
	// S4 支付令牌（P-14）：id/user_id/provider/token_cipher/brand/last4/
	// exp_month/exp_year/status/created_at/updated_at 共 11 列。
	"payment_methods": 11,
	// C2 / Task 5c：GPU 资源租赁计费两表（platform-01 §22.13）。
	// 列数与设计逐行对应：段 24 列、append-only 事实账本 19 列。
	"deployment_billing_segments": 24,
	"deployment_cost_facts":       19,
	// D7a 站点公告（modules-08 F-08-04 / R-D7-01/R-D7-02）：公告主表 13 列、
	// 已读记录 3 列；已读表无 FK，公告删除时由 repo 同事务清理。
	"announcements":      13,
	"announcement_reads": 3,
}

// 四个部分唯一索引（platform-04 §3.3.3；名称钉死）。
var wantPartialUniqueIndexes = []string{
	"users_email_uq",
	"wallet_release_refid_uq",
	"uq_wallet_tx_signup_grant",
	// M6d Task 1：同一插件至多一个 active 版本（R-M6D-05 / S4）。
	"task_plugin_versions_active_key_uq",
}

// GIN jsonb_path_ops 索引（platform-04 §3.3.3 / GC-5）。
var wantGINIndexes = []string{
	"channels_supported_models_gin",
	"channels_group_tags_gin",
	// M6d Task 7a：类型 60 多绑的反查（@> jsonb_build_array(key)）。
	"channels_task_extend_plugin_keys_gin",
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
	// 任务行是计费/审计事实：用户或渠道被硬删除后仍要保留历史任务，
	// user_id / channel_id 只做逻辑引用（M6d Task 6a）。
	"tasks",
	// D1：供应商删除由应用层引用保护（有引用 ⇒ 409 + 计数），
	// model_meta.vendor_id 不设 FK；model_meta 与 model_pricing 也不强制一一对应。
	"vendors",
	"model_meta",
	// D5a：订阅三表全部逻辑引用（user_id/plan_id/order_id/subscription_id），
	// 与 platform-04 §3.4 V5 一致；套餐删除保护由应用层在事务内完成。
	"subscription_plans",
	"subscriptions",
	"subscription_pre_consume_records",
	// S4：支付令牌只逻辑引用 users.id；删除令牌由应用层在同一事务内
	// fail-closed 关闭关联订阅（R-42），不设 FK。
	"payment_methods",
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
	// M6d Task 6a：tasks 以 GENERATED ALWAYS AS IDENTITY 作 PK。
	"tasks",
	// D1：模型目录两表同样使用单列 id identity 代理主键。
	"vendors",
	"model_meta",
	// D5a：套餐与订阅实例均为单列 id identity 代理主键。
	"subscription_plans",
	"subscriptions",
}

// identityPKExceptions：9 张「自然键/非 identity 例外」表的 PK（均非 identity）：
// system_settings."key" text 自然键（platform-01 §4）、request_registry.request_id
// uuid 自然键（§18 / platform-04 V9）、rankings_timeseries 与 rankings_entities 的
// 自然唯一键（§16/§17）、task_plugins."key" text 自然键（M6d B-11，主表以
// 插件 key 作 PK，不引入代理 id）、subscription_pre_consume_records.request_id
// uuid 自然键（D5a：幂等键就是主键，R-D5-02）。
//
// 另有两张以 run_id（FK → rankings_snapshot_runs.id，V5）领头的复合自然键表
// （runIDLedNaturalPKs），以及 uuid 代理主键表（uuidPKTables：id uuid
// DEFAULT gen_random_uuid()，属「代理主键但非 identity」）。合起来：40 表
// （M0 16 + M1 6 + M6c 2 + M6d 4 + D1 2 + D5a 3 + D7a 2 + B5a 1 + C2 2 + T4 2）
// = 23 张单列 id identity + 10 张自然键例外 + 4 张 uuid 代理键
// + 2 张 run_id 领头自然键 + 1 张自定义列名 identity（fact_id）；
// **非 identity PK 的表恰 16 张**，由 TestMainSchemaNonIdentityPKSet 断言。
var identityPKExceptions = map[string][]string{
	"system_settings":     {"key"},
	"rankings_timeseries": {"model", "granularity", "bucket_start"},
	"rankings_entities":   {"entity", "kind"},
	// B5a：作者时间序列的主键就是幂等键；period 由收口迁移加入并置于键首，
	// week/month 同桶不再互相覆盖。
	"rankings_author_timeseries":       {"period", "author", "granularity", "bucket_start"},
	"request_registry":                 {"request_id"},
	"task_plugins":                     {"key"},
	"subscription_pre_consume_records": {"request_id"},
	// D7a：已读表的复合主键就是幂等键（R-D7-02），不引入代理 id。
	"announcement_reads": {"user_id", "announcement_id"},
	// C2：计费段的本地段 ID 是幂等与计费锚点（platform-01 §22.13.1），
	// 自然键即主键，不引入额外代理 id。
	"deployment_billing_segments": {"segment_id"},
	// T4 / P-04：批次关联的主键就是「批次 × 子任务」这对自然键。
	"task_batches_items": {"batch_id", "task_id"},
	// S1：事件流的主键就是「订阅 × 同订阅内连续序号」这对自然键；seq 的
	// 无洞语义由订阅行锁保证，代理 id 不提供任何额外保证。
	"subscription_events": {"subscription_id", "seq"},
}

// identityPKCustomColumn：identity 代理主键但列名不是 "id" 的表。
// 设计把事实账本的主键定为 fact_id（platform-01 §22.13.2），仍使用
// GENERATED ALWAYS AS IDENTITY；登记在这里让 PK 断言既不误判为自然键，
// 也不要求把设计字段改名为 id。
var identityPKCustomColumn = map[string]string{
	"deployment_cost_facts": "fact_id",
}

// uuidPKTables：以 uuid 代理主键（单列 id + DEFAULT gen_random_uuid()）领头的表，
// 与 identity PK 表一样是代理键，但生成方式不是 GENERATED ALWAYS AS IDENTITY。
// 两者都非 identity，故都计入 non-identity 集合；分开登记是为了让「为什么
// 不是 identity」的原因在断言中可读（M6d B-12/B-13）。
var uuidPKTables = map[string][]string{
	"task_plugin_versions":       {"id"},
	"plugin_marketplace_sources": {"id"},
	// D7a：公告 id 由 gen_random_uuid() 生成（R-D7-01）。
	"announcements": {"id"},
	// T4：批次 id 由应用层 crypto/rand 生成（uuid v4），列上带安全默认值。
	"task_batches": {"id"},
	// S4：支付令牌 id 由 gen_random_uuid() 生成（外部可寻址的凭据句柄）。
	"payment_methods": {"id"},
}

// runIDLedNaturalPKs：以 FK run_id 领头的复合自然唯一键（platform-01 §14/§15）。
var runIDLedNaturalPKs = map[string][]string{
	"rankings_model_stats":    {"run_id", "model"},
	"rankings_provider_stats": {"run_id", "author"},
}

// wantFKs 是 platform-04 §3.4 V5 的完整 FK 清单（V5 的 7 条 + C2 计费域 1 条；
// M6d Task 1 新增 task_plugin_versions → task_plugins 一条）。
// 键形如 "<table>.<column>-><ref_table>.<ref_column>:<ON DELETE>"。
var wantFKs = []string{
	"api_tokens.user_id->users.id:CASCADE",
	"api_tokens.group->user_groups.name:RESTRICT",
	"users.group->user_groups.name:RESTRICT",
	"group_pricing.group->user_groups.name:RESTRICT",
	"rankings_model_stats.run_id->rankings_snapshot_runs.id:CASCADE",
	"rankings_provider_stats.run_id->rankings_snapshot_runs.id:CASCADE",
	// M6d Task 1：删除插件主表行级联删除版本行（B-12 FK）。
	"task_plugin_versions.key->task_plugins.key:CASCADE",
	// T4 / P-04：关联行随批次或任务任一被删除而消失（聚合视图可重建）。
	"task_batches_items.batch_id->task_batches.id:CASCADE",
	"task_batches_items.task_id->tasks.id:CASCADE",
	// C2：事实账本的段必须存在；RESTRICT 阻止误删仍在账上的段
	// （platform-01 §22.13.2 的唯一一条计费域 FK）。
	"deployment_cost_facts.segment_id->deployment_billing_segments.segment_id:RESTRICT",
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

// TestMainSchemaTableCount：（a）主库用户表恰 42 张（M0 16 + M1 认证族 6 + M6c 2 + M6d 4 + D1 2 + D5a 3 + D7a 2 + B5a 1 + C2 2 + T4 2 + S1 1 + S4 1，除 schema_migrations）。
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

// TestMainSchemaColumnCounts：（b）逐表列数 == 登记册计数（38 表，共 38 个断言）。
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

// indexPredicate 返回部分索引的谓词表达式（pg_get_expr），用于精确比对；
// 非部分索引返回空串。
func indexPredicate(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var expr sql.NullString
	err := db.QueryRow(`SELECT pg_get_expr(i.indpred, i.indrelid)
		FROM pg_index i
		JOIN pg_class idx ON idx.oid = i.indexrelid
		JOIN pg_namespace nsp ON nsp.oid = idx.relnamespace
		WHERE nsp.nspname = current_schema() AND idx.relname = $1`, name).Scan(&expr)
	if err == sql.ErrNoRows || !expr.Valid {
		return ""
	}
	if err != nil {
		t.Fatalf("query index predicate %s: %v", name, err)
	}
	return expr.String
}

// normalizeIndexPredicate 去掉 PG 规范化产生的噪声（::text 强制转换、空白、
// 分组括号），只保留谓词结构；两边用同一函数规范化后做全串相等比较。
func normalizeIndexPredicate(expr string) string {
	expr = strings.ReplaceAll(expr, "::text", "")
	expr = strings.NewReplacer(" ", "", "\t", "", "\n", "", "\r", "", "(", "", ")", "").Replace(expr)
	return expr
}

// TestMainSchemaPartialUniqueIndexes：（c）四处部分唯一索引存在且确为 UNIQUE + 部分。
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

// TestMainSchemaNotificationDeliveryPrefixIndex：A5 残余补批（G-DOM-37 ⑨）。
//
// 公告删除按 dedupe_key 前缀 LIKE 清理投递行；dedupe_key 的 UNIQUE 索引
// 使用默认 collation ops，普通 LIKE 前缀匹配用不上。这里断言存在一条
// 非唯一、非部分的 text_pattern_ops 索引，支撑 `announcement:<id>:%`。
func TestMainSchemaNotificationDeliveryPrefixIndex(t *testing.T) {
	db := openTestDB(t)
	def, unique, partial, ok := indexInfo(t, db, "notification_deliveries_dedupe_prefix_idx")
	if !ok {
		t.Fatalf("notification_deliveries_dedupe_prefix_idx 不存在")
	}
	if unique || partial {
		t.Fatalf("prefix 索引 unique=%v partial=%v，want false/false（定义：%s）", unique, partial, def)
	}
	if !strings.Contains(def, "dedupe_key") || !strings.Contains(def, "text_pattern_ops") {
		t.Fatalf("prefix 索引定义不符，want (dedupe_key text_pattern_ops)：%s", def)
	}
}

// TestMainSchemaRankingsTimeseriesIndexes：B5a 收口的两个复合索引
// （migrations 20261020261000）。
//
// 清理与读取都按 `granularity = ? AND bucket_start < ?` 过滤；只有
// (granularity, bucket_start) 复合索引能先用等值前缀缩小范围。这里同时
// 断言作者表的旧单列索引已删除，防止「新索引加了、旧索引还留着」的重复
// 维护成本被静默接受。
func TestMainSchemaRankingsTimeseriesIndexes(t *testing.T) {
	db := openTestDB(t)
	for _, name := range []string{
		"rankings_timeseries_granularity_bucket_idx",
		"rankings_author_timeseries_granularity_bucket_idx",
	} {
		name := name
		t.Run(name, func(t *testing.T) {
			def, unique, partial, ok := indexInfo(t, db, name)
			if !ok {
				t.Fatalf("索引 %s 不存在", name)
			}
			if unique || partial {
				t.Fatalf("%s unique=%v partial=%v，want false/false（定义：%s）", name, unique, partial, def)
			}
			if !strings.Contains(def, "(granularity, bucket_start)") {
				t.Fatalf("%s 定义不符，want (granularity, bucket_start)：%s", name, def)
			}
		})
	}
	if _, _, _, ok := indexInfo(t, db, "rankings_author_timeseries_bucket_idx"); ok {
		t.Fatal("作者表单列 bucket_start 索引应已被复合索引替换")
	}
}

// TestMainSchemaTaskIndexes：（c.1）tasks 的读取索引必须精确存在。
//
// 各索引各有唯一用途，删掉任意一条都会让对应查询退化成全表扫描：
//   - tasks_user_public_idx：能力式读取按 (user_id, public_task_id) 定位；
//   - tasks_pending_idx：轮询器只扫未完成任务的**部分**索引；
//   - tasks_user_created_idx：客户端台账按创建时间倒序分页；
//   - tasks_billing_pending_idx（T6b）：补偿扫描只扫带待办标记的终态行。
//
// 这里不只断言「存在」，还断言列序与部分谓词，避免被同名但语义不同的
// 索引顶替（那会让「索引存在」变成假阳性）。
func TestMainSchemaTaskIndexes(t *testing.T) {
	db := openTestDB(t)
	cases := []struct {
		name        string
		wantColumns []string
		wantPartial bool
		wantPred    string
	}{
		{"tasks_user_public_idx", []string{"user_id", "public_task_id"}, false, ""},
		{"tasks_pending_idx", []string{"state"}, true, "success"},
		{"tasks_user_created_idx", []string{"user_id", "created_at"}, false, ""},
		// T6b：终态补偿扫描的部分索引（T6b 迁移）。谓词含 billing_pending。
		{"tasks_billing_pending_idx", []string{"id"}, true, "billing_pending"},
		// W8/MJ-A0：按 family 领取非终态行（R-MJ-04 的两族独立轮询路径）。
		// 设计建议形状是 (family, state, next_poll_at)，但 tasks 无
		// next_poll_at 列；非终态由部分谓词表达，id 支撑领取顺序。
		{"tasks_family_pending_idx", []string{"family", "id"}, true, "success"},
		// W8/MJ-a2：MJ 归属查询（R-MJ-05）。(user_id, upstream_task_id)
		// 服务 code 21 复用与动作类端点锁定原渠道；单列 upstream_task_id
		// 服务图片代理/批量查询的按 MjId 定位。两者都排除插件行与空 id。
		{"tasks_mj_user_upstream_idx", []string{"user_id", "upstream_task_id"}, true, "mj"},
		{"tasks_mj_upstream_idx", []string{"upstream_task_id"}, true, "mj"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			def, _, partial, ok := indexInfo(t, db, c.name)
			if !ok {
				t.Fatalf("索引 %s 不存在", c.name)
			}
			if partial != c.wantPartial {
				t.Fatalf("%s partial=%v，want %v（定义：%s）", c.name, partial, c.wantPartial, def)
			}
			for _, column := range c.wantColumns {
				if !strings.Contains(def, column) {
					t.Fatalf("%s 定义缺少列 %q：%s", c.name, column, def)
				}
			}
			if c.wantPred != "" && !strings.Contains(def, c.wantPred) {
				t.Fatalf("%s 部分谓词缺少 %q：%s", c.name, c.wantPred, def)
			}
		})
	}
}

// TestMainSchemaModelCatalogIndexes：（c.2）D1 两表的索引形状（platform-01
// §22.7.1/§22.7.2）：
//   - vendors 的唯一性必须落在 lower(name) 表达式上（大小写不敏感，停用行也占名）；
//   - model_meta.model_name 是大小写敏感的 UNIQUE；
//   - vendor_id / status 各一条普通索引；
//   - tags **不建** GIN（D1-05；新增 PG 特性须先登记）。
func TestMainSchemaModelCatalogIndexes(t *testing.T) {
	db := openTestDB(t)

	type wantIndex struct {
		name    string
		unique  bool
		partial bool
	}
	for _, want := range []wantIndex{
		{"vendors_name_lower_uq", true, false},
		{"vendors_status_idx", false, false},
		{"model_meta_model_name_uq", true, false},
		{"model_meta_vendor_idx", false, false},
		{"model_meta_status_idx", false, false},
	} {
		want := want
		t.Run(want.name, func(t *testing.T) {
			def, unique, partial, ok := indexInfo(t, db, want.name)
			if !ok {
				t.Fatalf("索引 %s 不存在", want.name)
			}
			if unique != want.unique || partial != want.partial {
				t.Fatalf("%s unique=%v partial=%v，want %v/%v（定义：%s）",
					want.name, unique, partial, want.unique, want.partial, def)
			}
			if want.name == "vendors_name_lower_uq" && !strings.Contains(def, "lower(") {
				t.Fatalf("vendors 唯一索引必须建在 lower(name) 上：%s", def)
			}
		})
	}

	t.Run("model_meta.tags 无 GIN（D1-05）", func(t *testing.T) {
		var n int
		err := db.QueryRow(`SELECT count(*)
			FROM pg_index i
			JOIN pg_class t ON t.oid = i.indrelid
			JOIN pg_class idx ON idx.oid = i.indexrelid
			JOIN pg_namespace nsp ON nsp.oid = t.relnamespace
			WHERE nsp.nspname = current_schema() AND t.relname = 'model_meta'
			  AND pg_get_indexdef(i.indexrelid) LIKE '%tags%'`).Scan(&n)
		if err != nil {
			t.Fatalf("query model_meta indexes: %v", err)
		}
		if n != 0 {
			t.Fatalf("model_meta.tags 不应建索引（D1-05），实际 %d 条", n)
		}
	})
}

// TestMainSchemaSubscriptionIndexes：（c.3）D5a 订阅三表的索引形状
// （billing-08 §2.1 展示索引、§3.1/§4.2 活跃订阅热路径、§7 B12 扫描、
// R-D5-02 预扣记录反查）。
//
// 这里断言列与部分谓词而不只断言「存在」：同名但语义不同的索引（例如把
// subscriptions_active_idx 建在全表 user_id 上）会让热路径查询退化为扫描，
// 「索引存在」的断言就成了假阳性。
func TestMainSchemaSubscriptionIndexes(t *testing.T) {
	db := openTestDB(t)
	cases := []struct {
		name        string
		wantColumns []string
		wantUnique  bool
		wantPartial bool
		wantPred    string
		// wantPredExpr 对部分谓词做**精确**比对（规范化空白/括号/::text 后
		// 全串相等）。wantPred 的子串断言无法区分 `next_reset_at` 列与
		// `next_reset_at IS NOT NULL` 条件，也会漏掉多出来的 OR 分支。
		wantPredExpr string
	}{
		{"subscription_plans_plan_key_uq", []string{"plan_key"}, true, false, "", ""},
		{"subscription_plans_status_sort_idx", []string{"status", "sort_order"}, false, false, "", ""},
		{"subscriptions_order_id_uq", []string{"order_id"}, true, false, "", ""},
		{"subscriptions_active_idx", []string{"user_id", "end_at"}, false, true, "active", ""},
		{"subscriptions_expiry_idx", []string{"status", "end_at"}, false, true, "active", ""},
		{"subscriptions_reset_idx", []string{"status", "next_reset_at"}, false, true, "",
			"status = 'active' AND next_reset_at IS NOT NULL"},
		{"subscriptions_plan_idx", []string{"plan_id"}, false, false, "", ""},
		{"subscription_pre_consume_records_subscription_idx", []string{"subscription_id"}, false, false, "", ""},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			def, unique, partial, ok := indexInfo(t, db, c.name)
			if !ok {
				t.Fatalf("索引 %s 不存在", c.name)
			}
			if unique != c.wantUnique || partial != c.wantPartial {
				t.Fatalf("%s unique=%v partial=%v，want %v/%v（定义：%s）",
					c.name, unique, partial, c.wantUnique, c.wantPartial, def)
			}
			for _, column := range c.wantColumns {
				if !strings.Contains(def, column) {
					t.Fatalf("%s 定义缺少列 %q：%s", c.name, column, def)
				}
			}
			if c.wantPred != "" && !strings.Contains(def, c.wantPred) {
				t.Fatalf("%s 部分谓词缺少 %q：%s", c.name, c.wantPred, def)
			}
			if c.wantPredExpr != "" {
				got := indexPredicate(t, db, c.name)
				if normalizeIndexPredicate(got) != normalizeIndexPredicate(c.wantPredExpr) {
					t.Fatalf("%s 部分谓词 = %q，want 精确等于 %q",
						c.name, got, c.wantPredExpr)
				}
			}
		})
	}

	// TC-H-01：套餐表不得携带任何模型价/倍率/折扣字段（billing-08 §2.2 硬约束）。
	// 订阅只能选账户，不能成为第二定价 owner；出现这些列名即设计被破坏。
	t.Run("套餐表无定价字段", func(t *testing.T) {
		rows, err := db.Query(`SELECT column_name FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'subscription_plans'`)
		if err != nil {
			t.Fatalf("query subscription_plans columns: %v", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var column string
			if err := rows.Scan(&column); err != nil {
				t.Fatalf("scan column: %v", err)
			}
			if rePlanPricingColumn.MatchString(column) {
				t.Fatalf("subscription_plans 不得有定价字段 %q（billing-08 §2.2）", column)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate subscription_plans columns: %v", err)
		}
	})
}

// rePlanPricingColumn 命中「单价/倍率/折扣」类列名（大小写不敏感）：
// price/ratio/discount 允许出现在 price_amount（真实货币售价）之外均是违规。
// price_amount 是购买层售价，不是模型单价，故显式放行。
var rePlanPricingColumn = regexp.MustCompile(`(?i)^(model_?price|model_?ratio|ratio|discount|discount_?rate)$`)

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

// TestMainSchemaFKsMatchV5：全库真实 FK 恰 8 条（V5 的 7 条 + C2 计费域 1 条），
// 引用列与 ON DELETE 逐条对齐。
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

// TestMainSchemaIdentityPK：（e）identity PK + 例外清单恰 11 项 + uuid 代理键 5 项
// + run_id 领头复合自然键 2 项 + 自定义列名 identity 1 项。
func TestMainSchemaIdentityPK(t *testing.T) {
	if len(identityPKExceptions) != 11 {
		t.Fatalf("自然键例外清单须恰 11 项（另有 5 张 uuid 代理键 + 2 张 run_id 领头的复合自然键 + 1 张自定义列名 identity），实际 %d 项", len(identityPKExceptions))
	}
	db := openTestDB(t)

	// (e.0) identity 代理主键但列名不是 id 的表（C2：fact_id）。
	for table, wantCol := range identityPKCustomColumn {
		table, wantCol := table, wantCol
		t.Run(table, func(t *testing.T) {
			cols := pkColumns(t, db, table)
			if len(cols) != 1 || cols[0].Name != wantCol {
				names := make([]string, 0, len(cols))
				for _, c := range cols {
					names = append(names, c.Name)
				}
				t.Fatalf("%s PK = %v，want 单列 %s", table, names, wantCol)
			}
			if cols[0].IsIdent != "YES" || cols[0].IdentGen != "ALWAYS" {
				t.Fatalf("%s.%s is_identity=%q identity_generation=%q，want YES/ALWAYS",
					table, wantCol, cols[0].IsIdent, cols[0].IdentGen)
			}
		})
	}

	// (e.1) 23 张代理主键表（M0 10 + M1 6 + M6c 2 + M6d 1 + D1 2 + D5a 2）：单列 id 且 GENERATED ALWAYS AS IDENTITY。
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

	// (e.2) 5 项例外：自然键组成与 spec 逐列一致，且均非 identity。
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

	// (e.3) uuid 代理主键：单列 id、类型 uuid、DEFAULT gen_random_uuid()，
	// 且**不是** identity（M6d B-12/B-13）。
	if len(uuidPKTables) != 5 {
		t.Fatalf("uuid 代理主键表须恰 5 张，实际 %d 张", len(uuidPKTables))
	}
	for table, wantCols := range uuidPKTables {
		table, wantCols := table, wantCols
		t.Run(table, func(t *testing.T) {
			cols := pkColumns(t, db, table)
			got := make([]string, 0, len(cols))
			for _, c := range cols {
				if c.IsIdent == "YES" {
					t.Fatalf("%s.%s 不应为 identity（uuid 代理键由 gen_random_uuid() 生成）", table, c.Name)
				}
				got = append(got, c.Name)
			}
			if fmt.Sprint(got) != fmt.Sprint(wantCols) {
				t.Fatalf("%s PK = %v，want %v", table, got, wantCols)
			}
			var dataType, colDefault string
			if err := db.QueryRow(`SELECT data_type, COALESCE(column_default, '')
				FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`,
				table, wantCols[0]).Scan(&dataType, &colDefault); err != nil {
				t.Fatalf("query %s.%s column: %v", table, wantCols[0], err)
			}
			if dataType != "uuid" {
				t.Fatalf("%s.%s data_type = %q，want uuid", table, wantCols[0], dataType)
			}
			if !strings.Contains(colDefault, "gen_random_uuid()") {
				t.Fatalf("%s.%s default = %q，want gen_random_uuid()", table, wantCols[0], colDefault)
			}
		})
	}

	// (e.4) run_id 领头的复合自然键：组成正确、无 identity、run_id 上 FK 存在（V5）。
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

// TestMainSchemaNonIdentityPKSet：非 identity PK 的表恰 18 张 =
// 11 张自然键例外（identityPKExceptions）+ 5 张 uuid 代理键（uuidPKTables）
// + 2 张 run_id 领头的复合自然键（runIDLedNaturalPKs）；其余均为 identity
// 单列代理主键（其中 deployment_cost_facts 的列名是 fact_id）。
func TestMainSchemaNonIdentityPKSet(t *testing.T) {
	db := openTestDB(t)
	wantNonIdentity := map[string]bool{}
	for table := range identityPKExceptions {
		wantNonIdentity[table] = true
	}
	for table := range uuidPKTables {
		wantNonIdentity[table] = true
	}
	for table := range runIDLedNaturalPKs {
		wantNonIdentity[table] = true
	}
	if len(wantNonIdentity) != 18 {
		t.Fatalf("非 identity PK 表须恰 18 张（11 自然键 + 5 uuid 代理键 + 2 run_id 领头），实际 %d 张", len(wantNonIdentity))
	}

	var gotNonIdentity, gotIdentity []string
	for _, table := range sortedKeys(wantColumns) {
		cols := pkColumns(t, db, table)
		identity := len(cols) == 1 && cols[0].IsIdent == "YES" && cols[0].IdentGen == "ALWAYS"
		if custom, ok := identityPKCustomColumn[table]; ok {
			identity = identity && cols[0].Name == custom
		} else {
			identity = identity && cols[0].Name == "id"
		}
		if identity {
			gotIdentity = append(gotIdentity, table)
			continue
		}
		gotNonIdentity = append(gotNonIdentity, table)
	}
	wantIdentity := append([]string{}, wantIdentityPKTables...)
	for table := range identityPKCustomColumn {
		wantIdentity = append(wantIdentity, table)
	}
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
	// M6d Task 7a / relay-06 §1.3：插件绑定 key 形态与多绑数组形态。
	{
		Table: "channels", Name: "channels_task_plugin_key_chk",
		Fragments: []string{"task_plugin_key", "a-z0-9", "29"},
	},
	{
		Table: "channels", Name: "channels_task_extend_plugin_keys_chk",
		Fragments: []string{"jsonb_typeof", "array"},
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
	// M6d Task 1 / modules-08 §3.10.2：市场源 URL 仅 https 且 ≤1024。
	{
		Table: "plugin_marketplace_sources", Name: "plugin_marketplace_sources_url_len_chk",
		Fragments: []string{"char_length", "url", "1024"},
	},
	// M6d Task 6a / relay-06 §7：进度必须落在 0–100，防止上游字符串进度
	// 被误写为越界数值（进度参与轮询的「未完成」判定）。
	{
		Table: "tasks", Name: "tasks_progress_percent_chk",
		Fragments: []string{"progress_percent", ">= 0", "<= 100"},
	},
	// D1 / platform-01 §22.7.1：模型名字符集必须放行 `/`（公开模型名
	// openai/gpt-5），三个数组列必须是 jsonb 数组（D1-03/D1-04）。
	// C2：单成员 CHECK 会被 PG 规范化为 `col = 'value'::text`，不呈枚举形态，
	// 故不走 EnumChecks；这里断言约束存在且取值未被放宽。
	{
		Table: "deployment_billing_segments", Name: "deployment_billing_segments_price_source_chk",
		Fragments: []string{"provider_quote"},
	},
	{
		Table: "deployment_cost_facts", Name: "deployment_cost_facts_unit_chk",
		Fragments: []string{"gpu_second"},
	},
	{
		Table: "model_meta", Name: "model_meta_model_name_chk",
		Fragments: []string{"model_name", "A-Za-z0-9", "128"},
	},
	{
		Table: "model_meta", Name: "model_meta_tags_chk",
		Fragments: []string{"tags", "jsonb_typeof", "array"},
	},
	{
		Table: "model_meta", Name: "model_meta_capabilities_chk",
		Fragments: []string{"capabilities", "jsonb_typeof", "array"},
	},
	{
		Table: "model_meta", Name: "model_meta_modalities_chk",
		Fragments: []string{"modalities", "jsonb_typeof", "array"},
	},
	{
		Table: "model_meta", Name: "model_meta_parameters_chk",
		Fragments: []string{"parameters", "jsonb_typeof", "array"},
	},
	{
		Table: "model_meta", Name: "model_meta_knowledge_cutoff_chk",
		Fragments: []string{"knowledge_cutoff", "0[1-9]"},
	},
	{
		Table: "model_meta", Name: "model_meta_context_length_chk",
		Fragments: []string{"context_length", ">= 0"},
	},
	{
		Table: "model_meta", Name: "model_meta_max_output_chk",
		Fragments: []string{"max_output", ">= 0"},
	},
	// D5a / billing-08 §2.1/§6.1/§7 + R-D5-07/09：套餐与实例的边界约束。
	// 枚举 CHECK 由 EnumChecks 对账；这里登记非枚举下界/长度/形态约束，
	// 防止后续迁移把它们静默放宽（例如把 period_days >= 1 删成无约束）。
	{
		Table: "subscription_plans", Name: "subscription_plans_title_len_chk",
		Fragments: []string{"char_length", "title", "64"},
	},
	{
		Table: "subscription_plans", Name: "subscription_plans_description_len_chk",
		Fragments: []string{"char_length", "description", "255"},
	},
	{
		Table: "subscription_plans", Name: "subscription_plans_period_days_chk",
		Fragments: []string{"period_days", ">= 1"},
	},
	{
		Table: "subscription_plans", Name: "subscription_plans_price_amount_chk",
		Fragments: []string{"price_amount", ">= 0"},
	},
	{
		Table: "subscription_plans", Name: "subscription_plans_grant_credits_chk",
		Fragments: []string{"grant_credits", ">= 0"},
	},
	{
		Table: "subscription_plans", Name: "subscription_plans_grace_days_chk",
		Fragments: []string{"grace_days", ">= 0"},
	},
	{
		Table: "subscription_plans", Name: "subscription_plans_model_scope_chk",
		Fragments: []string{"model_scope", "jsonb_typeof", "array"},
	},
	// R-D5-07：S1 只支持 CNY。单成员 CHECK 会被 PG 规范化为 `= 'CNY'::bpchar`，
	// 不呈枚举形态，故按普通约束登记（见 enum.go 的说明）。
	{
		Table: "subscription_plans", Name: "subscription_plans_currency_chk",
		Fragments: []string{"currency", "CNY"},
	},
	{
		Table: "subscriptions", Name: "subscriptions_currency_chk",
		Fragments: []string{"currency", "CNY"},
	},
	// R-D5-09：时间列 timestamptz、到期必须晚于生效。
	{
		Table: "subscriptions", Name: "subscriptions_end_at_chk",
		Fragments: []string{"end_at", "start_at"},
	},
	{
		Table: "subscriptions", Name: "subscriptions_amount_total_chk",
		Fragments: []string{"amount_total", ">= 0"},
	},
	{
		Table: "subscriptions", Name: "subscriptions_amount_used_chk",
		Fragments: []string{"amount_used", ">= 0"},
	},
	{
		Table: "subscriptions", Name: "subscriptions_price_paid_amount_chk",
		Fragments: []string{"price_paid_amount", ">= 0"},
	},
	{
		Table: "subscriptions", Name: "subscriptions_period_days_chk",
		Fragments: []string{"period_days", ">= 1"},
	},
	{
		Table: "subscriptions", Name: "subscriptions_grace_days_chk",
		Fragments: []string{"grace_days", ">= 0"},
	},
	{
		Table: "subscriptions", Name: "subscriptions_model_scope_chk",
		Fragments: []string{"model_scope", "jsonb_typeof", "array"},
	},
	// S1：事件流的非枚举约束（kind 的八值白名单由 EnumChecks 对账）。
	// seq 从 1 开始且同订阅内连续；detail 必须是对象，读侧才能按字段取值。
	{
		Table: "subscription_events", Name: "subscription_events_seq_chk",
		Fragments: []string{"seq", ">= 1"},
	},
	{
		Table: "subscription_events", Name: "subscription_events_detail_chk",
		Fragments: []string{"detail", "jsonb_typeof", "object"},
	},
	// D7a / modules-08 F-08-04：标题非空且 ≤80；正文 64KiB 硬兜底
	// （应用层 16KB）；过期必须晚于生效（R-D7-01）。
	{
		Table: "announcements", Name: "announcements_title_len_chk",
		Fragments: []string{"char_length", "btrim", "80"},
	},
	{
		Table: "announcements", Name: "announcements_content_len_chk",
		Fragments: []string{"octet_length", "65536"},
	},
	{
		Table: "announcements", Name: "announcements_expire_after_publish_chk",
		Fragments: []string{"expire_at", "publish_at"},
	},
	// A5 / 迁移 20261020200000：站内通知投递的契约扩展。payload 只允许
	// JSON 对象（读取端按固定键取值）；read_at 只对 inapp 行有意义，
	// email/webhook 行不得携带已读时刻。
	{
		Table: "notification_deliveries", Name: "notification_deliveries_payload_obj_chk",
		Fragments: []string{"payload", "jsonb_typeof", "object"},
	},
	{
		Table: "notification_deliveries", Name: "notification_deliveries_read_at_chk",
		Fragments: []string{"read_at", "channel", "inapp"},
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

	// M6d Task 1：市场源 URL 的两个独立分支（scheme / 长度）各有一条负向探针。
	t.Run("市场源非 https URL 被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO plugin_marketplace_sources (name, url)
			VALUES ('chk_url_scheme', 'http://example.com/index.json')`)
	})
	t.Run("市场源 URL 超长被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO plugin_marketplace_sources (name, url)
			VALUES ('chk_url_len', 'https://' || repeat('a', 1024))`)
	})

	// M6d Task 6a：tasks 的三个取值域（state / family / progress）各一条负向
	// 探针。枚举 CHECK 已由 EnumChecks 对账覆盖，这里额外证明**写入路径**
	// 确实被 DB 拒绝（而不是约束只存在于目录里）。
	t.Run("任务 state 越界被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO tasks
			(public_task_id, family, action, plugin_key, fetch_mode, state, user_id)
			VALUES ('chk_task_state', 'plugin', 'generate', 'sample', 'poll', 'bogus', 1)`)
	})
	t.Run("任务 family 越界被拒", func(t *testing.T) {
		// W8/MJ-A0：'mj' 已成为合法族（20261023090000），未知值用 'bogus'
		// 证明 CHECK 仍然真实生效，而不是只把约束删掉了。
		expectExecError(t, db, `INSERT INTO tasks
			(public_task_id, family, action, plugin_key, fetch_mode, state, user_id)
			VALUES ('chk_task_family', 'bogus', 'generate', 'sample', 'poll', 'submitted', 1)`)
	})
	t.Run("任务 family 接受 mj", func(t *testing.T) {
		// 正向探针：MJ 族共用 tasks 表（R-MJ-03），必须能真实写入。
		// 用回滚事务避免留下数据（public_task_id 全局唯一）。
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`INSERT INTO tasks
			(public_task_id, family, action, plugin_key, fetch_mode, state, user_id)
			VALUES ('chk_task_family_mj', 'mj', 'IMAGINE', '', 'poll', 'submitted', 1)`); err != nil {
			t.Fatalf("mj family must be accepted: %v", err)
		}
	})
	t.Run("任务 progress 越界被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO tasks
			(public_task_id, family, action, plugin_key, fetch_mode, state, progress_percent, user_id)
			VALUES ('chk_task_progress', 'plugin', 'generate', 'sample', 'poll', 'submitted', 101, 1)`)
	})

	// D1 / platform-01 §22.7.1：目录 CHECK 的写入路径同样必须真实拒绝，
	// 而不是只存在于 catalog（与 R-24 的负向覆盖同口径）。
	t.Run("模型名含空格被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO model_meta (model_name) VALUES ('bad model')`)
	})
	t.Run("模型名含 / 被接受", func(t *testing.T) {
		// 公开模型名（如 openai/gpt-5）必须可建档；用回滚事务避免留下数据。
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`INSERT INTO model_meta (model_name) VALUES ('openai/chk-model_1')`); err != nil {
			t.Fatalf("含 / 的合法模型名被误拒：%v", err)
		}
	})
	t.Run("tags 非数组被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO model_meta (model_name, tags)
			VALUES ('chk_tags_object', '{"a":1}'::jsonb)`)
	})
	t.Run("knowledge_cutoff 非法月份被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO model_meta (model_name, knowledge_cutoff)
			VALUES ('chk_cutoff', '2024-13')`)
	})
	t.Run("context_length 负数被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO model_meta (model_name, context_length)
			VALUES ('chk_context_neg', -1)`)
	})
	t.Run("供应商 status 越界被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO vendors (name, status) VALUES ('chk_vendor_status', 3)`)
	})

	// D5a：订阅数据层的写入路径同样必须被真实拒绝，而不只是约束存在于
	// catalog。套餐/实例都不设 FK，负向探针可以直接插入最小行。
	t.Run("套餐价格为负被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO subscription_plans
			(plan_key, title, period_days, price_amount, currency)
			VALUES ('chk_plan_price', 'x', 30, -1, 'CNY')`)
	})
	t.Run("套餐周期为 0 被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO subscription_plans
			(plan_key, title, period_days, price_amount, currency)
			VALUES ('chk_plan_period', 'x', 0, 0, 'CNY')`)
	})
	t.Run("套餐标题为空被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO subscription_plans
			(plan_key, title, period_days, price_amount, currency)
			VALUES ('chk_plan_title', '', 30, 0, 'CNY')`)
	})
	t.Run("套餐非 CNY 币种被拒（R-D5-07）", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO subscription_plans
			(plan_key, title, period_days, price_amount, currency)
			VALUES ('chk_plan_currency', 'x', 30, 0, 'USD')`)
	})
	t.Run("订阅 end_at 不晚于 start_at 被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO subscriptions
			(user_id, plan_id, plan_title_snapshot, start_at, end_at, currency,
			 period_days, price_paid_amount)
			VALUES (1, 1, 'x', now(), now(), 'CNY', 30, 0)`)
	})
	t.Run("订阅 amount_used 为负被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO subscriptions
			(user_id, plan_id, plan_title_snapshot, start_at, end_at, currency,
			 period_days, price_paid_amount, amount_used)
			VALUES (1, 1, 'x', now(), now() + interval '30 days', 'CNY', 30, 0, -1)`)
	})
	t.Run("订阅状态越界被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO subscriptions
			(user_id, plan_id, plan_title_snapshot, status, start_at, end_at,
			 currency, period_days, price_paid_amount)
			VALUES (1, 1, 'x', 'bogus', now(), now() + interval '30 days', 'CNY', 30, 0)`)
	})
	t.Run("预扣记录状态越界被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO subscription_pre_consume_records
			(request_id, subscription_id, status, pre_consumed)
			VALUES ('00000000-0000-0000-0000-000000000001', 1, 'bogus', 1)`)
	})
	// S1：事件流的写入路径同样必须真实拒绝（seq 下界 / detail 形态）。
	t.Run("事件 seq 为 0 被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO subscription_events (subscription_id, seq, kind)
			VALUES (1, 0, 'created')`)
	})
	t.Run("事件 detail 非对象被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO subscription_events (subscription_id, seq, kind, detail)
			VALUES (1, 1, 'created', '[]'::jsonb)`)
	})
	t.Run("orders.biz_kind 越界被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO orders
			(user_id, provider, amount, currency, idempotency_key, biz_kind)
			VALUES (1, 'stripe', 1, 'USD', 'chk_biz_kind', 'bogus')`)
	})
	t.Run("users.billing_preference 越界被拒", func(t *testing.T) {
		expectExecError(t, db, `INSERT INTO users (username, billing_preference)
			VALUES ('chk_billing_pref', 'bogus')`)
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
		// D1 的三列 smallint 枚举（vendors.status / model_meta.status / name_rule）。
		"vendors.status":       true,
		"model_meta.status":    true,
		"model_meta.name_rule": true,
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
