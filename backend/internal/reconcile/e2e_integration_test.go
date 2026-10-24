//go:build integration

package reconcile_test

// 本文件是上游用量对账的端到端验收（Task 17）。它把真实主库、真实日志库、
// 真实仓储、真实 runner 与真实 HTTP 路由装配在一起，覆盖单测无法证明的
// 跨层契约：
//   - usage_logs 的 UTC 时刻按账号时区切日（2026-10-24T16:30:00Z 必须落在
//     Asia/Shanghai 的 2026-10-25，而不是 10-24）；
//   - 快照经 PUT /api/upstream-snapshots 落库后，POST /run 真的读得到；
//   - 超报模型同时满足 delta_bp > 100 与 maxAbs > 1000 才判 alert，匹配模型判 ok；
//   - 重跑生成新 run、键集合完全一致、每个 run 内无重复 (account, date, model)；
//   - 手动触发与 ack 真的在日志库留下 reconcile.run / reconcile.item_ack 审计；
//   - 所有对账相关路由（对账 5 条 + 账号 4 条 + 快照 2 条）对没有
//     settings:manage 的真实会话返回 403 problem+json，而不是 404。
//
// 权限用例刻意使用真实 users 行 + SQLAuthRepo 权限装载器：会话 Principal 的
// Permissions 为空，nil 在 RBAC 语义里等于 DB NULL=FULL，只有装载器按 UserID
// 读回权威值才能把「受限 admin」判成 403。这正是 Review Focus 4 的风险点。
//
// 运行（隔离库，先创建数据库；测试自身会执行迁移）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:<password>@localhost:55432/newtg_reconcile_e2e?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:<password>@localhost:55432/newtg_reconcile_e2e_logs?sslmode=disable' \
//	  go test -tags=integration ./internal/reconcile -run 'TestReconcile(EndToEnd|EndpointsRejectNonAdmin)' -v
//
// NEWTG_TEST_LOGS_DSN 缺失时直接失败而不是跳过：日志库断言（聚合与审计）是
// 本任务的核心验收，静默跳过等于没跑。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"newtg/gateway/internal/api/dto"
	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/httpapi"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/reconcile"
	"newtg/gateway/internal/store"
	"newtg/gateway/migrations"
	migrationslogs "newtg/gateway/migrations-logs"
)

// 固定时间：整套夹具不使用 now()，任何一天重跑都应得到相同的账期判定。
// 当前月分区由迁移在 2026-10 创建；若跨月后重跑而目标库没有 2026-10 分区，
// 插入会失败——届时必须新建分区或重建库，不能改成「当月」来掩盖时区断言。
var (
	e2eNow       = time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)
	e2eLogAt     = time.Date(2026, 10, 24, 16, 30, 0, 0, time.UTC)
	e2eUsageDate = time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
)

const (
	e2eMatchModel = "deepseek-chat"
	e2eOverModel  = "deepseek-reasoner"
)

// staticSettings 是 runner 需要的设置快照；用出厂默认值让容差断言可解释。
type staticSettings struct{ settings reconcile.Settings }

func (s staticSettings) ReconcileSettings(context.Context) (reconcile.Settings, error) {
	return s.settings, nil
}

// reconcileE2E 是一个完整装配的验收环境。
type reconcileE2E struct {
	t      *testing.T
	mainDB *sql.DB
	logsDB *sql.DB
	mux    http.Handler

	adminID   int64
	accountID int64
	channelID int64
	logIDs    []string
	userIDs   []int64

	// permissions 与中间件使用的是同一个 SQLAuthRepo 实例；权限用例直接经它
	// 装载，证明 403 来自「权限被解析成 users:read」而不是装载器报错后的 fail-closed。
	permissions *httpapi.SQLAuthRepo
}

// openTestDBs 打开主库与日志库，并对两者执行嵌入迁移。
func openTestDBs(t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()
	mainDSN := os.Getenv("NEWTG_TEST_DB_DSN")
	if mainDSN == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过上游对账端到端集成测试")
	}
	logsDSN := os.Getenv("NEWTG_TEST_LOGS_DSN")
	if logsDSN == "" {
		t.Fatal("NEWTG_TEST_LOGS_DSN 未设置：日志库聚合与审计断言不允许静默跳过")
	}

	mainDB := openDB(t, mainDSN, "main")
	logsDB := openDB(t, logsDSN, "logs")
	// 迁移是破坏性/建表操作，绝不能因为两个 DSN 指向同一个库而互相污染。
	if mainDSN == logsDSN {
		t.Fatal("NEWTG_TEST_DB_DSN 与 NEWTG_TEST_LOGS_DSN 指向同一个数据库；必须分开")
	}
	migrateUp(t, mainDB, migrations.FS, "main")
	migrateUp(t, logsDB, migrationslogs.FS, "logs")
	return mainDB, logsDB
}

func openDB(t *testing.T, dsn, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s db: %v", name, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping %s db: %v", name, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// migrateUp 执行嵌入迁移；已是最新版本时 ErrNoChange 视为成功。
func migrateUp(t *testing.T, db *sql.DB, src fs.FS, name string) {
	t.Helper()
	source, err := iofs.New(src, ".")
	if err != nil {
		t.Fatalf("open embedded %s migrations: %v", name, err)
	}
	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		t.Fatalf("create %s migrate driver: %v", name, err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		_ = driver.Close()
		t.Fatalf("create %s migrator: %v", name, err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate %s up: %v", name, err)
	}
}

// newReconcileE2E 装配主库/日志库、账号与渠道、五个对账路由以及两条
// 账号/快照路由。管理员会话来自真实 users 行（admin_permissions NULL=FULL）。
func newReconcileE2E(t *testing.T) *reconcileE2E {
	t.Helper()
	mainDB, logsDB := openTestDBs(t)

	env := &reconcileE2E{t: t, mainDB: mainDB, logsDB: logsDB}
	// 清理必须在任何夹具写入之前注册：后续步骤失败时也不能把用户、账号、渠道、
	// usage_logs 或快照漏在共享测试库里。cleanup 容忍零 ID（DELETE 命中 0 行）。
	t.Cleanup(env.cleanup)
	suffix := uuid.NewString()[:8]

	env.adminID = env.insertUser("e2e_admin_" + strings.ReplaceAll(suffix, "-", "_"))
	env.userIDs = append(env.userIDs, env.adminID)

	accounts := store.NewUpstreamAccountRepo(mainDB)
	account, err := accounts.Create(context.Background(), store.UpstreamAccount{
		Name:     "e2e-upstream-" + suffix,
		Provider: "deepseek",
		Timezone: "Asia/Shanghai",
		Note:     "task-17 end-to-end fixture",
	})
	if err != nil {
		t.Fatalf("create upstream account: %v", err)
	}
	env.accountID = account.ID
	if account.Status != 1 {
		t.Fatalf("account status = %d, want 1 (enabled)", account.Status)
	}

	env.channelID = env.insertChannel("e2e-channel-" + suffix)

	env.logIDs = []string{
		env.insertUsageLog(e2eMatchModel, 1000, 100, 100, 1234),
		env.insertUsageLog(e2eOverModel, 10000, 500, 2000, 4321),
	}

	// 刻意不在 setup 里预置快照：快照行只能由 PUT /api/upstream-snapshots 写入，
	// 否则「PUT 返回 200 但什么都没落库」会被预置行掩盖，severity 断言照样通过。
	// 权限用例不使用快照，因此也不需要夹具。

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	auditLogger := audit.New(logsDB)
	authz := rbac.NewAuthzMiddleware()

	runner, err := reconcile.NewRunner(reconcile.RunnerConfig{
		Accounts:  accounts,
		Snapshots: store.NewUpstreamSnapshotRepo(mainDB),
		Usage:     store.NewUsageAggregateRepo(logsDB),
		Store:     store.NewReconcileRepo(mainDB),
		Settings:  staticSettings{settings: reconcile.DefaultSettings()},
		Clock:     func() time.Time { return e2eNow },
	})
	if err != nil {
		t.Fatalf("new reconcile runner: %v", err)
	}

	reconcileHandlers, err := httpapi.NewReconcileHandlers(httpapi.ReconcileHandlerConfig{
		DB: mainDB, Runner: runner, Authz: authz, Audit: auditLogger, Logger: logger,
		Now: func() time.Time { return e2eNow },
	})
	if err != nil {
		t.Fatalf("new reconcile handlers: %v", err)
	}
	accountHandlers, err := httpapi.NewUpstreamAccountHandlers(httpapi.UpstreamAccountHandlerConfig{
		DB: mainDB, Authz: authz, Audit: auditLogger, Logger: logger,
	})
	if err != nil {
		t.Fatalf("new upstream account handlers: %v", err)
	}
	snapshotHandlers, err := httpapi.NewUpstreamSnapshotHandlers(httpapi.UpstreamSnapshotHandlerConfig{
		DB: mainDB, Authz: authz, Audit: auditLogger, Logger: logger,
		Now:          func() time.Time { return e2eNow },
		LookbackDays: func(context.Context) int { return reconcile.DefaultLookbackDays },
	})
	if err != nil {
		t.Fatalf("new upstream snapshot handlers: %v", err)
	}

	// 会话 Principal 不带 Permissions；权威权限必须按 UserID 从 users 行装载，
	// 否则受限 admin 会被 nil=FULL 直接放行。
	env.permissions = httpapi.NewSQLAuthRepo(mainDB)
	authz.SetPermissionLoader(env.permissions)

	mux := http.NewServeMux()
	reconcileHandlers.RegisterRoutes(mux, nil)
	accountHandlers.RegisterRoutes(mux, nil)
	snapshotHandlers.RegisterRoutes(mux, nil)
	env.mux = mux

	return env
}

func (e *reconcileE2E) insertUser(username string) int64 {
	e.t.Helper()
	var id int64
	if err := e.mainDB.QueryRow(
		`INSERT INTO users (username, role) VALUES ($1, 'admin') RETURNING id`,
		username).Scan(&id); err != nil {
		e.t.Fatalf("insert user %s: %v", username, err)
	}
	return id
}

func (e *reconcileE2E) insertChannel(name string) int64 {
	e.t.Helper()
	var id int64
	err := e.mainDB.QueryRow(`
		INSERT INTO channels (name, type, credentials, supported_models, model_mapping,
			weight, priority, status, auto_ban, upstream_account_id)
		VALUES ($1, 'openai', $2, '["deepseek-chat","deepseek-reasoner"]'::jsonb,
			'{}'::jsonb, 1, 10, 1, true, $3)
		RETURNING id`, name, []byte{0}, e.accountID).Scan(&id)
	if err != nil {
		e.t.Fatalf("insert channel %s: %v", name, err)
	}
	return id
}

// insertUsageLog 写入一条成功消费日志。usage_semantic=0 表示 cache_read 是从
// prompt_tokens 里切出的命中部分，miss = prompt - cache_read。
func (e *reconcileE2E) insertUsageLog(model string, prompt, cacheRead, completion, cost int64) string {
	e.t.Helper()
	requestID := uuid.NewString()
	_, err := e.logsDB.Exec(`
		INSERT INTO usage_logs (request_id, user_id, channel_id, model,
			prompt_tokens, completion_tokens, cache_read_tokens, cost, status,
			upstream_model, upstream_account_id, usage_source, usage_semantic, attempts, created_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, 1, $9, $10, 1, 0, 1, $11)`,
		requestID, int64(900001), e.channelID, "client-"+model,
		prompt, completion, cacheRead, cost, model, e.accountID, e2eLogAt)
	if err != nil {
		e.t.Fatalf("insert usage_log %s: %v", model, err)
	}
	return requestID
}

func (e *reconcileE2E) cleanup() {
	// 顺序：明细随 run 级联删除，快照/渠道先于账号。
	queries := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM upstream_reconciliation_runs WHERE account_id = $1`, []any{e.accountID}},
		{`DELETE FROM upstream_usage_snapshots WHERE account_id = $1`, []any{e.accountID}},
		{`DELETE FROM channels WHERE id = $1`, []any{e.channelID}},
		{`DELETE FROM upstream_accounts WHERE id = $1`, []any{e.accountID}},
	}
	for _, q := range queries {
		if _, err := e.mainDB.Exec(q.sql, q.args...); err != nil {
			e.t.Errorf("cleanup %q: %v", q.sql, err)
		}
	}
	for _, requestID := range e.logIDs {
		if _, err := e.logsDB.Exec(`DELETE FROM usage_logs WHERE request_id = $1::uuid`, requestID); err != nil {
			e.t.Errorf("cleanup usage_log %s: %v", requestID, err)
		}
	}
	for _, userID := range e.userIDs {
		if _, err := e.logsDB.Exec(`DELETE FROM audit_logs WHERE user_id = $1`, userID); err != nil {
			e.t.Errorf("cleanup audit_logs for user %d: %v", userID, err)
		}
		if _, err := e.mainDB.Exec(`DELETE FROM users WHERE id = $1`, userID); err != nil {
			e.t.Errorf("cleanup user %d: %v", userID, err)
		}
	}
}

// do 发送一次带会话身份的请求。t 显式传入：403 子测试要用子测试自己的 t，
// 失败才能定位到具体路由而不是父测试。
func (e *reconcileE2E) do(t *testing.T, p *auth.Principal, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal %s %s body: %v", method, path, err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *reconcileE2E) admin() *auth.Principal {
	return &auth.Principal{UserID: e.adminID, Role: rbac.RoleAdmin}
}

func requireStatus(t *testing.T, rr *httptest.ResponseRecorder, want int, label string) {
	t.Helper()
	if rr.Code != want {
		t.Fatalf("%s status = %d, want %d（body=%s）", label, rr.Code, want, rr.Body.String())
	}
}

func TestReconcileEndToEnd(t *testing.T) {
	env := newReconcileE2E(t)
	admin := env.admin()

	// 1. 快照只经管理面 PUT 写入——setup 不预置任何快照行，所以 runner 读到的
	//    每一行都必须来自这次 PUT。PUT 返回 200 却不落库时，下面的 severity
	//    断言（missing_snapshot，而不是 ok/alert）会立刻失败。
	hitA, missA, outA := int64(100), int64(900), int64(100)
	hitB, missB, outB := int64(5500), int64(19500), int64(4000)
	amountA, amountB := int64(1234), int64(4321)
	upsert := dto.UpstreamSnapshotUpsert{
		AccountId: env.accountID,
		Items: []dto.UpstreamSnapshotWrite{
			{
				UsageDate: openapi_types.Date{Time: e2eUsageDate}, UpstreamModel: e2eMatchModel,
				InputCacheHitTokens: &hitA, InputCacheMissTokens: &missA,
				OutputTokens: &outA, AmountMinor: &amountA,
			},
			{
				UsageDate: openapi_types.Date{Time: e2eUsageDate}, UpstreamModel: e2eOverModel,
				InputCacheHitTokens: &hitB, InputCacheMissTokens: &missB,
				OutputTokens: &outB, AmountMinor: &amountB,
			},
		},
	}
	requireStatus(t, env.do(t, admin, http.MethodPut, "/api/upstream-snapshots", upsert),
		http.StatusOK, "PUT /api/upstream-snapshots")

	// 2. 触发一次手动对账。
	first := env.triggerRun(admin)
	if len(first.RunIds) != 1 {
		t.Fatalf("first run ids = %v, want exactly one run", first.RunIds)
	}
	if first.Items != 2 || first.Alerts != 1 {
		t.Fatalf("first run items/alerts = %d/%d, want 2/1", first.Items, first.Alerts)
	}
	firstRunID := first.RunIds[0]

	runs := env.listRuns(admin)
	if runs.Total != 1 || len(runs.Items) != 1 || runs.Items[0].Id != firstRunID {
		t.Fatalf("runs after first trigger = %+v (total %d), want exactly run %d",
			runs.Items, runs.Total, firstRunID)
	}
	// run 状态是确定性的：固定时钟 e2eNow 比最新日志晚 11.5 小时，远超 runner
	// 的 15 分钟日志流滞后阈值（runner.go 的 logStreamLagThreshold），所以必须是
	// partial。这里钉死具体值，避免「一律 partial」或「一律 succeeded」的回归漏过。
	firstRunStatus := runs.Items[0].Status
	if firstRunStatus != dto.ReconcileRunStatusPartial {
		t.Fatalf("first run status = %s, want partial（固定时钟比最新日志晚 11.5h）", firstRunStatus)
	}

	// 3. 断言日界：日志时刻 2026-10-24T16:30:00Z 在 Asia/Shanghai 是 10-25。
	items := env.listItems(admin, firstRunID)
	if len(items) != 2 {
		t.Fatalf("first run items = %d, want 2", len(items))
	}
	byModel := make(map[string]dto.ReconcileItem, len(items))
	for _, item := range items {
		byModel[item.UpstreamModel] = item
		if got := item.UsageDate.Time.Format("2006-01-02"); got != "2026-10-25" {
			t.Fatalf("%s usage_date = %s, want 2026-10-25（Asia/Shanghai 切日）",
				item.UpstreamModel, got)
		}
	}

	matching, ok := byModel[e2eMatchModel]
	if !ok {
		t.Fatalf("missing item for %s: %+v", e2eMatchModel, items)
	}
	if matching.Severity != dto.ReconcileItemSeverityOk {
		t.Fatalf("%s severity = %s, want ok（delta_bp=%v）",
			e2eMatchModel, matching.Severity, ptrString(matching.DeltaTokenBp))
	}
	if matching.DeltaTokenBp == nil || *matching.DeltaTokenBp != 0 {
		t.Fatalf("%s delta_token_bp = %v, want 0", e2eMatchModel, ptrString(matching.DeltaTokenBp))
	}

	over, ok := byModel[e2eOverModel]
	if !ok {
		t.Fatalf("missing item for %s: %+v", e2eOverModel, items)
	}
	// 先断言规则的两个输入，失败时能区分「规则错」与「夹具错」。
	if over.DeltaTokenBp == nil || *over.DeltaTokenBp <= int64(reconcile.DefaultTokenBP) {
		t.Fatalf("%s delta_token_bp = %v, want > %d", e2eOverModel,
			ptrString(over.DeltaTokenBp), reconcile.DefaultTokenBP)
	}
	maxAbs := maxDeltaAbs(over)
	if maxAbs <= reconcile.DefaultMinDeltaTokens {
		t.Fatalf("%s maxAbs = %d, want > %d", e2eOverModel, maxAbs, reconcile.DefaultMinDeltaTokens)
	}
	if over.Severity != dto.ReconcileItemSeverityAlert {
		t.Fatalf("%s severity = %s, want alert（delta_bp=%d maxAbs=%d）",
			e2eOverModel, over.Severity, *over.DeltaTokenBp, maxAbs)
	}

	// 4. 处置告警条目：ack 必须落库并写审计。
	requireStatus(t, env.do(t, admin, http.MethodPost,
		fmt.Sprintf("/api/upstream-reconciliations/items/%d/ack", over.Id),
		map[string]any{"status": "acknowledged", "note": "task-17 acceptance"}),
		http.StatusOK, "POST ack")

	acked := env.listItems(admin, firstRunID)
	ackedFound := false
	for _, item := range acked {
		if item.Id != over.Id {
			continue
		}
		ackedFound = true
		if item.Status != dto.ReconcileItemStatusAcknowledged || item.Note != "task-17 acceptance" {
			t.Fatalf("acked item = status %s note %q", item.Status, item.Note)
		}
	}
	if !ackedFound {
		// 缺了条目时上面的循环会空转通过；ack 后条目消失同样是失败。
		t.Fatalf("acked item %d missing from run %d items after ack: %+v",
			over.Id, firstRunID, acked)
	}

	// 5. 审计行必须真实存在且形状可追溯。
	if n := env.countAudit(admin.UserID, audit.ActionReconcileRun); n != 1 {
		t.Fatalf("audit %s rows = %d, want 1", audit.ActionReconcileRun, n)
	}
	var trigger, runStatus string
	if err := env.logsDB.QueryRow(
		`SELECT COALESCE(detail->>'trigger', ''), COALESCE(detail->>'status', '')
		 FROM audit_logs WHERE user_id = $1 AND detail->>'action' = $2
		 ORDER BY id DESC LIMIT 1`,
		admin.UserID, audit.ActionReconcileRun).Scan(&trigger, &runStatus); err != nil {
		t.Fatalf("read reconcile.run audit: %v", err)
	}
	if trigger != "manual" {
		t.Fatalf("reconcile.run audit trigger = %q, want manual", trigger)
	}
	// 审计里的 status 必须与 run 行的真实状态一致，且同样钉死为 partial：
	// 一个写死 / 拼错的审计状态不能藏在「非空且不是 timeout」背后。
	if runStatus != string(firstRunStatus) {
		t.Fatalf("reconcile.run audit status = %q, want %q（与 run %d 行一致）",
			runStatus, firstRunStatus, firstRunID)
	}
	if runStatus != "partial" {
		t.Fatalf("reconcile.run audit status = %q, want partial（固定时钟比最新日志晚 11.5h）", runStatus)
	}
	var ackBefore, ackAfter string
	if err := env.logsDB.QueryRow(
		`SELECT COALESCE(before->>'status', ''), COALESCE(after->>'status', '')
		 FROM audit_logs WHERE user_id = $1 AND detail->>'action' = $2
		 ORDER BY id DESC LIMIT 1`,
		admin.UserID, audit.ActionReconcileItemAck).Scan(&ackBefore, &ackAfter); err != nil {
		t.Fatalf("read reconcile.item_ack audit: %v", err)
	}
	if ackBefore != "open" || ackAfter != "acknowledged" {
		t.Fatalf("reconcile.item_ack before/after = %q / %q, want open -> acknowledged",
			ackBefore, ackAfter)
	}

	// 6. 重跑：新 run、键集合完全一致、每个 run 内无重复键，且不出现 500。
	second := env.triggerRun(admin)
	if len(second.RunIds) != 1 {
		t.Fatalf("second run ids = %v, want exactly one run", second.RunIds)
	}
	secondRunID := second.RunIds[0]
	if secondRunID == firstRunID {
		t.Fatalf("second run reused id %d; a re-run must create a new run", firstRunID)
	}

	runs = env.listRuns(admin)
	if runs.Total != 2 {
		t.Fatalf("runs after re-run = %d, want 2", runs.Total)
	}
	// 下面的状态与审计断言都遍历 runs.Items；少了这一条，空列表会让它们空转通过。
	if len(runs.Items) != 2 {
		t.Fatalf("runs after re-run = %d rows (total %d), want 2", len(runs.Items), runs.Total)
	}
	firstKeys := itemKeys(items)
	secondItems := env.listItems(admin, secondRunID)
	if len(secondItems) != 2 {
		t.Fatalf("second run items = %d, want 2", len(secondItems))
	}
	if !sameKeySet(firstKeys, itemKeys(secondItems)) {
		t.Fatalf("re-run keys = %v, want identical to %v", itemKeys(secondItems), firstKeys)
	}
	for _, runID := range []int64{firstRunID, secondRunID} {
		if dup := env.duplicateKeyGroups(runID); dup != 0 {
			t.Fatalf("run %d has %d duplicated (account, usage_date, model) keys", runID, dup)
		}
	}

	for _, run := range runs.Items {
		if run.Status != dto.ReconcileRunStatusPartial {
			t.Fatalf("run %d status = %s, want partial", run.Id, run.Status)
		}
	}
	// 第二次运行的审计行也必须与它自己的 run 行状态一致（最新一条就是它）。
	var rerunAuditStatus string
	if err := env.logsDB.QueryRow(
		`SELECT COALESCE(detail->>'status', '') FROM audit_logs
		 WHERE user_id = $1 AND detail->>'action' = $2
		 ORDER BY id DESC LIMIT 1`,
		admin.UserID, audit.ActionReconcileRun).Scan(&rerunAuditStatus); err != nil {
		t.Fatalf("read reconcile.run audit after re-run: %v", err)
	}
	rerunAuditMatched := false
	for _, run := range runs.Items {
		if run.Id != secondRunID {
			continue
		}
		rerunAuditMatched = true
		if rerunAuditStatus != string(run.Status) {
			t.Fatalf("re-run audit status = %q, want %q（与 run %d 行一致）",
				rerunAuditStatus, run.Status, secondRunID)
		}
	}
	if !rerunAuditMatched {
		t.Fatalf("second run %d missing from the run list: %+v", secondRunID, runs.Items)
	}
	if n := env.countAudit(admin.UserID, audit.ActionReconcileRun); n != 2 {
		t.Fatalf("audit %s rows after re-run = %d, want 2", audit.ActionReconcileRun, n)
	}
}

// TestReconcileEndpointsRejectNonAdmin 覆盖全部对账相关路由的权限门禁。
//
// 两个会话都来自真实 users 行，且 Principal 刻意不携带 Permissions：
//   - 普通用户：role=user，无论残留权限如何都必须拒绝；
//   - 受限 admin：role=admin + users.admin_permissions=["users:read"]，
//     会话里的 nil 只能由 SQLAuthRepo 装载成 ["users:read"]，不能当成 FULL。
func TestReconcileEndpointsRejectNonAdmin(t *testing.T) {
	env := newReconcileE2E(t)
	plainUserID := env.insertUser("e2e_plain_" + uuid.NewString()[:8])
	env.userIDs = append(env.userIDs, plainUserID)
	if _, err := env.mainDB.Exec(`UPDATE users SET role = 'user' WHERE id = $1`, plainUserID); err != nil {
		t.Fatalf("demote user: %v", err)
	}
	limitedAdminID := env.insertUser("e2e_limited_" + uuid.NewString()[:8])
	env.userIDs = append(env.userIDs, limitedAdminID)
	if _, err := env.mainDB.Exec(
		`UPDATE users SET admin_permissions = '["users:read"]'::jsonb WHERE id = $1`,
		limitedAdminID); err != nil {
		t.Fatalf("restrict admin: %v", err)
	}

	// 403 必须来自「权限被正确解析成 ["users:read"]」，而不是装载器报错后的
	// fail-closed。直接经中间件持有的同一个 SQLAuthRepo 装载：
	// nil 在 RBAC 里等于 FULL，空集才是「什么都不要」。
	limitedPerms, err := env.permissions.Load(context.Background(), limitedAdminID)
	if err != nil {
		t.Fatalf("load limited admin permissions: %v", err)
	}
	if limitedPerms == nil {
		t.Fatal(`limited admin permissions = nil（RBAC 语义里等于 FULL），want ["users:read"]`)
	}
	if len(limitedPerms) != 1 || limitedPerms[0] != "users:read" {
		t.Fatalf(`limited admin permissions = %v, want ["users:read"]`, limitedPerms)
	}
	plainPerms, err := env.permissions.Load(context.Background(), plainUserID)
	if err != nil {
		t.Fatalf("load plain user permissions: %v", err)
	}
	if plainPerms == nil || len(plainPerms) != 0 {
		t.Fatalf("plain user permissions = %v, want empty non-nil（不得回退 FULL）", plainPerms)
	}

	sessions := map[string]*auth.Principal{
		"plain user":    {UserID: plainUserID, Role: rbac.RoleUser},
		"limited admin": {UserID: limitedAdminID, Role: rbac.RoleAdmin},
	}
	routes := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"list runs", http.MethodGet, "/api/upstream-reconciliations/runs", nil},
		{"list items", http.MethodGet, "/api/upstream-reconciliations/runs/1/items", nil},
		{"summary", http.MethodGet, "/api/upstream-reconciliations/summary?run_id=1", nil},
		{"trigger run", http.MethodPost, "/api/upstream-reconciliations/run", map[string]any{}},
		{"ack item", http.MethodPost, "/api/upstream-reconciliations/items/1/ack",
			map[string]any{"status": "acknowledged", "note": ""}},
		{"list upstream accounts", http.MethodGet, "/api/upstream-accounts", nil},
		{"create upstream account", http.MethodPost, "/api/upstream-accounts",
			map[string]any{"name": "x", "provider": "y"}},
		{"update upstream account", http.MethodPatch, "/api/upstream-accounts/1",
			map[string]any{"note": ""}},
		{"delete upstream account", http.MethodDelete, "/api/upstream-accounts/1", nil},
		{"list upstream snapshots", http.MethodGet, "/api/upstream-snapshots?account_id=1", nil},
		{"upsert upstream snapshots", http.MethodPut, "/api/upstream-snapshots",
			map[string]any{"account_id": 1, "items": []any{}}},
	}

	for sessionName, principal := range sessions {
		for _, route := range routes {
			t.Run(sessionName+"/"+route.name, func(t *testing.T) {
				rr := env.do(t, principal, route.method, route.path, route.body)
				if rr.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403 (not 404；body=%s)", rr.Code, rr.Body.String())
				}
				if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
					t.Fatalf("Content-Type = %q, want application/problem+json", ct)
				}
				var problem dto.Problem
				if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
					t.Fatalf("decode problem: %v（body=%s）", err, rr.Body.String())
				}
				if problem.Status != http.StatusForbidden {
					t.Fatalf("problem.status = %d, want 403", problem.Status)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func (e *reconcileE2E) triggerRun(p *auth.Principal) dto.ReconcileRunResult {
	e.t.Helper()
	rr := e.do(e.t, p, http.MethodPost, "/api/upstream-reconciliations/run",
		map[string]any{"account_id": e.accountID})
	requireStatus(e.t, rr, http.StatusOK, "POST /api/upstream-reconciliations/run")
	var result dto.ReconcileRunResult
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		e.t.Fatalf("decode run result: %v（body=%s）", err, rr.Body.String())
	}
	return result
}

func (e *reconcileE2E) listRuns(p *auth.Principal) dto.ReconcileRunList {
	e.t.Helper()
	rr := e.do(e.t, p, http.MethodGet,
		fmt.Sprintf("/api/upstream-reconciliations/runs?account_id=%d", e.accountID), nil)
	requireStatus(e.t, rr, http.StatusOK, "GET runs")
	var list dto.ReconcileRunList
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		e.t.Fatalf("decode run list: %v（body=%s）", err, rr.Body.String())
	}
	return list
}

func (e *reconcileE2E) listItems(p *auth.Principal, runID int64) []dto.ReconcileItem {
	e.t.Helper()
	rr := e.do(e.t, p, http.MethodGet,
		fmt.Sprintf("/api/upstream-reconciliations/runs/%d/items", runID), nil)
	requireStatus(e.t, rr, http.StatusOK, "GET items")
	var list dto.ReconcileItemList
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		e.t.Fatalf("decode item list: %v（body=%s）", err, rr.Body.String())
	}
	return list.Items
}

func (e *reconcileE2E) countAudit(actor int64, action string) int64 {
	e.t.Helper()
	var n int64
	if err := e.logsDB.QueryRow(
		`SELECT count(*) FROM audit_logs WHERE user_id = $1 AND detail->>'action' = $2`,
		actor, action).Scan(&n); err != nil {
		e.t.Fatalf("count audit %s: %v", action, err)
	}
	return n
}

// duplicateKeyGroups 统计同一 run 内重复的 (account, usage_date, model) 组。
// 它守的是表上的唯一约束（迁移层），不是 runner 的键生成逻辑：约束在时它永远
// 返回 0；一旦约束被删且 runner 写出重复键，它才会失败。
func (e *reconcileE2E) duplicateKeyGroups(runID int64) int64 {
	e.t.Helper()
	var n int64
	if err := e.mainDB.QueryRow(`
		SELECT count(*) FROM (
			SELECT account_id, usage_date, upstream_model, count(*) AS c
			FROM upstream_reconciliation_items
			WHERE run_id = $1
			GROUP BY account_id, usage_date, upstream_model
			HAVING count(*) > 1
		) duplicated`, runID).Scan(&n); err != nil {
		e.t.Fatalf("count duplicate item keys in run %d: %v", runID, err)
	}
	return n
}

// maxDeltaAbs 复算 alert 规则里的 maxAbs（三个 token 维度绝对差的最大值）。
func maxDeltaAbs(item dto.ReconcileItem) int64 {
	var maxAbs int64
	for _, delta := range []*int64{
		item.DeltaCacheHitTokens, item.DeltaCacheMissTokens, item.DeltaOutputTokens,
	} {
		if delta == nil {
			continue
		}
		value := *delta
		if value < 0 {
			value = -value
		}
		if value > maxAbs {
			maxAbs = value
		}
	}
	return maxAbs
}

func itemKeys(items []dto.ReconcileItem) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, item.UsageDate.Time.Format("2006-01-02")+"|"+item.UpstreamModel)
	}
	return keys
}

func sameKeySet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	left := make(map[string]int, len(a))
	for _, key := range a {
		left[key]++
	}
	for _, key := range b {
		left[key]--
		if left[key] < 0 {
			return false
		}
	}
	return true
}

func ptrString(value *int64) string {
	if value == nil {
		return "null"
	}
	return fmt.Sprintf("%d", *value)
}
