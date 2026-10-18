//go:build integration

package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sort"
	"testing"

	// pgx/v5 stdlib 驱动为 Task 4 冻结选型（platform-04 §9.1 同款）。
	_ "github.com/jackc/pgx/v5/stdlib"
)

// 本文件是日志库 init 迁移（platform-01 §5 audit_logs / §12 usage_logs）的
// schema 集成验收。权威：docs/platform-01-数据库字段汇总.md 与
// docs/platform-04-数据模型迁移-设计.md（§3.2/§3.3/§5）。
//
// 运行（DSN 的 dbname 会被测试内替换成日志库 newtg_logs，本计划冻结此手法）：
//
//	NEWTG_TEST_DB_DSN=postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable \
//	  go test -tags=integration ./internal/store/ -run TestLogsSchema -v

// logsWantColumns 日志库逐表列数（platform-01 §5 = 14、§12 = 25）。
var logsWantColumns = map[string]int{
	"usage_logs": 25,
	"audit_logs": 14,
}

// openLogsTestDB 连接日志库：取 NEWTG_TEST_DB_DSN 并把 dbname 换成 newtg_logs。
func openLogsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		t.Fatalf("解析 NEWTG_TEST_DB_DSN 失败: %v", err)
	}
	// 计划冻结的测试手法：测试内把 DSN 的 dbname 替换为日志库。
	u.Path = "/newtg_logs"
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping logs db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestLogsSchema 日志库 schema 验收（GC-5 / platform-04 §5）。
func TestLogsSchema(t *testing.T) {
	db := openLogsTestDB(t)

	// (a) 恰两张逻辑表（分区子表不算独立表），usage_logs 为分区表（relkind='p'）。
	t.Run("tables", func(t *testing.T) {
		rows, err := db.Query(`SELECT c.relname
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = current_schema()
			  AND c.relkind IN ('r', 'p') AND NOT c.relispartition
			  AND c.relname <> 'schema_migrations'
			ORDER BY c.relname`)
		if err != nil {
			t.Fatalf("query tables: %v", err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		want := []string{"audit_logs", "usage_logs"}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("日志库表集合 = %v，want %v", got, want)
		}

		var relkind string
		err = db.QueryRow(`SELECT c.relkind::text
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = current_schema() AND c.relname = 'usage_logs'`).Scan(&relkind)
		if err != nil {
			t.Fatalf("query usage_logs relkind: %v", err)
		}
		if relkind != "p" {
			t.Fatalf("usage_logs relkind = %q，want \"p\"（声明式分区表）", relkind)
		}
	})

	// (b) 分区恰两个，名字 == 当月/下月 YYYYMM（期望值同用 date_trunc 计算，UTC）。
	t.Run("partitions", func(t *testing.T) {
		var cur, next string
		err := db.QueryRow(`SELECT
				to_char(date_trunc('month', now() AT TIME ZONE 'UTC'), 'YYYYMM'),
				to_char(date_trunc('month', now() AT TIME ZONE 'UTC') + interval '1 month', 'YYYYMM')`).
			Scan(&cur, &next)
		if err != nil {
			t.Fatalf("compute expected months: %v", err)
		}

		rows, err := db.Query(`SELECT c.relname
			FROM pg_inherits i
			JOIN pg_class c ON c.oid = i.inhrelid
			JOIN pg_class p ON p.oid = i.inhparent
			JOIN pg_namespace n ON n.oid = p.relnamespace
			WHERE p.relname = 'usage_logs' AND n.nspname = current_schema()
			ORDER BY c.relname`)
		if err != nil {
			t.Fatalf("query partitions: %v", err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		want := []string{"usage_logs_" + cur, "usage_logs_" + next}
		sort.Strings(want)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("分区集合 = %v，want %v（当月+下月，UTC）", got, want)
		}
	})

	// (c) 边界写入：当月最后 1 微秒与次月第一个微秒分别落入对应分区（查 tableoid）。
	t.Run("month_boundary", func(t *testing.T) {
		var cur, next string
		err := db.QueryRow(`SELECT
				to_char(date_trunc('month', now() AT TIME ZONE 'UTC'), 'YYYYMM'),
				to_char(date_trunc('month', now() AT TIME ZONE 'UTC') + interval '1 month', 'YYYYMM')`).
			Scan(&cur, &next)
		if err != nil {
			t.Fatalf("compute expected months: %v", err)
		}

		insert := `INSERT INTO usage_logs (request_id, user_id, model, status, created_at)
			VALUES (gen_random_uuid(), 1, 'logs-schema-test', 1, ` + `%s` + `)
			RETURNING request_id`
		lastMicrosecond := `(date_trunc('month', now() AT TIME ZONE 'UTC') + interval '1 month' - interval '1 microsecond') AT TIME ZONE 'UTC'`
		firstOfNext := `(date_trunc('month', now() AT TIME ZONE 'UTC') + interval '1 month') AT TIME ZONE 'UTC'`

		cases := []struct {
			expr string
			want string
		}{
			{lastMicrosecond, "usage_logs_" + cur},
			{firstOfNext, "usage_logs_" + next},
		}
		for _, tc := range cases {
			var requestID string
			if err := db.QueryRow(fmt.Sprintf(insert, tc.expr)).Scan(&requestID); err != nil {
				t.Fatalf("insert boundary row (%s): %v", tc.expr, err)
			}
			var part string
			err := db.QueryRow(`SELECT p.relname
				FROM usage_logs u
				JOIN pg_class p ON p.oid = u.tableoid
				WHERE u.request_id = $1`, requestID).Scan(&part)
			if err != nil {
				t.Fatalf("query tableoid: %v", err)
			}
			if part != tc.want {
				t.Fatalf("边界行落入分区 %q，want %q", part, tc.want)
			}
		}
	})

	// (d) usage_logs.request_id 上无 UNIQUE 约束；普通索引存在（R5）。
	t.Run("request_id_not_unique", func(t *testing.T) {
		var n int
		err := db.QueryRow(`SELECT count(*)
			FROM pg_index i
			JOIN pg_class t ON t.oid = i.indrelid
			JOIN pg_namespace ns ON ns.oid = t.relnamespace
			WHERE t.relname = 'usage_logs' AND ns.nspname = current_schema()
			  AND i.indisunique
			  AND pg_get_indexdef(i.indexrelid) ~ '\yrequest_id\y'`).Scan(&n)
		if err != nil {
			t.Fatalf("query unique indexes on request_id: %v", err)
		}
		if n != 0 {
			t.Fatalf("usage_logs.request_id 上存在 %d 个 UNIQUE 索引，want 0（R5）", n)
		}

		for _, idx := range []string{"usage_logs_request_id_idx", "usage_logs_cursor_idx"} {
			err := db.QueryRow(`SELECT count(*) FROM pg_indexes
				WHERE schemaname = current_schema() AND tablename = 'usage_logs' AND indexname = $1`, idx).Scan(&n)
			if err != nil {
				t.Fatalf("query index %s: %v", idx, err)
			}
			if n != 1 {
				t.Fatalf("索引 %s 不存在（count=%d）", idx, n)
			}
		}
	})

	// (e) 无 DEFAULT 分区（V11）；pg_partition_tree 叶子节点恰 2 个。
	t.Run("no_default_partition", func(t *testing.T) {
		var leaves, defaults int
		err := db.QueryRow(`SELECT count(*) FROM pg_partition_tree('usage_logs') WHERE isleaf`).Scan(&leaves)
		if err != nil {
			t.Fatalf("query partition tree: %v", err)
		}
		if leaves != 2 {
			t.Fatalf("usage_logs 叶子分区数 = %d，want 2", leaves)
		}

		err = db.QueryRow(`SELECT count(*)
			FROM pg_partition_tree('usage_logs') pt
			JOIN pg_class c ON c.oid = pt.relid
			WHERE pt.isleaf AND pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT'`).Scan(&defaults)
		if err != nil {
			t.Fatalf("query default partition: %v", err)
		}
		if defaults != 0 {
			t.Fatalf("usage_logs 存在 DEFAULT 分区，违反 V11")
		}
	})

	// (f) 列数 25 / 14；audit_logs 无 request_id 列（platform-05 U-05-7 冻结）。
	t.Run("column_counts", func(t *testing.T) {
		tables := make([]string, 0, len(logsWantColumns))
		for table := range logsWantColumns {
			tables = append(tables, table)
		}
		sort.Strings(tables)
		for _, table := range tables {
			table := table
			t.Run(table, func(t *testing.T) {
				var n int
				err := db.QueryRow(`SELECT count(*) FROM information_schema.columns
					WHERE table_schema = current_schema() AND table_name = $1`, table).Scan(&n)
				if err != nil {
					t.Fatalf("query columns: %v", err)
				}
				if n != logsWantColumns[table] {
					t.Fatalf("%s 列数 = %d，want %d（platform-01）", table, n, logsWantColumns[table])
				}
			})
		}

		var n int
		err := db.QueryRow(`SELECT count(*) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'audit_logs'
			  AND column_name = 'request_id'`).Scan(&n)
		if err != nil {
			t.Fatalf("query audit_logs.request_id: %v", err)
		}
		if n != 0 {
			t.Fatalf("audit_logs 不应有 request_id 列（platform-05 U-05-7）")
		}
	})
}
