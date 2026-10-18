// RUN_MODE=migrate 的迁移执行器：以 golang-migrate 库模式（iofs + pgx driver）
// 依次执行主库、日志库两条迁移流。仅 migrate 角色与 gateway 的
// DB_MIGRATE_ON_START 路径调用；构建期即内嵌 SQL，运行时不依赖 migrate CLI（GC-5）。
package platform

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	// pgx/v5 stdlib 驱动（与 db.go 同款），供咨询锁与迁移 driver 复用。
	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/config"
	"newtg/gateway/migrations"
	migrationslogs "newtg/gateway/migrations-logs"
)

const (
	// migrateAdvisoryLockKey 是跨进程串行化的 PG 咨询锁键（本计划冻结）。
	migrateAdvisoryLockKey = 727272
	// migrateLockRetryInterval：咨询锁被占用时的重试间隔（本计划冻结）。
	migrateLockRetryInterval = 2 * time.Second
	// migrateUnlockTimeout：释放锁用独立短超时，不受启动 ctx 取消影响。
	migrateUnlockTimeout = 5 * time.Second
	// migrateVersionDigits：文件名版本前缀长度（platform-04 §2.3 V1）。
	migrateVersionDigits = 14
)

// RunMigrate 以 migrate 角色运行：主库 → 日志库，成功后返回（调用方 exit 0）。
// 全流程持有主库上的 pg_advisory_lock(migrateAdvisoryLockKey)，并发执行者
// 在锁上每 2s 重试，ctx 结束即放弃；迁移结束（无论成败）释放锁。
func RunMigrate(ctx context.Context, cfg *config.Config) error {
	return runMigrate(ctx, cfg, prodMigrationExecutor{})
}

// migrationExecutor 抽象迁移副作用，测试注入 fake 记录顺序/锁行为（GC-8：
// 单测不触真 PG）。
type migrationExecutor interface {
	// AcquireLock 在 dsn 上取咨询锁；拿不到时按间隔重试直到 ctx 结束。
	AcquireLock(ctx context.Context, dsn string) (migrationLock, error)
	// Up 把内嵌源迁移到最新版本（golang-migrate 的 ErrNoChange 视为成功）。
	Up(ctx context.Context, dsn string, src embed.FS) error
}

// migrationLock 是已持有的咨询锁；Release 必须可在迁移失败后调用。
type migrationLock interface {
	Release(ctx context.Context) error
}

func runMigrate(ctx context.Context, cfg *config.Config, exec migrationExecutor) (err error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	// DB_LOGS_DSN 是必填项（platform-03 §3.1 #22；config.Load 已 fail-fast）。
	// 这里再做一次防御性校验：迁移入口也可能被直接构造 Config 的调用方命中，
	// 而日志库迁移流**不可静默跳过**——跳过意味着 usage_logs / audit_logs 永远
	// 建不出来，运行期才以更难定位的方式失败。取锁前返回，避免无意义的锁往返。
	if cfg.DBLogsDSN == "" {
		return errors.New("migrate: DB_LOGS_DSN is empty; logs database migration must not be skipped")
	}

	lock, err := exec.AcquireLock(ctx, cfg.MainDSN())
	if err != nil {
		return fmt.Errorf("migrate: acquire advisory lock on main database: %w", err)
	}
	defer func() {
		// 释放不依赖调用方 ctx：即使迁移被取消也要解锁。
		releaseCtx, cancel := context.WithTimeout(context.Background(), migrateUnlockTimeout)
		defer cancel()
		if relErr := lock.Release(releaseCtx); relErr != nil {
			err = errors.Join(err, fmt.Errorf("migrate: release advisory lock: %w", relErr))
		}
	}()

	// 主库在前：日志表的逻辑外键引用主库对象（platform-03 §3.4）。
	if err := exec.Up(ctx, cfg.MainDSN(), migrations.FS); err != nil {
		return fmt.Errorf("migrate: main database: %w", err)
	}

	// 日志库在后：其逻辑外键引用主库对象（platform-03 §3.4）。
	if err := exec.Up(ctx, cfg.DBLogsDSN, migrationslogs.FS); err != nil {
		return fmt.Errorf("migrate: logs database: %w", err)
	}
	return nil
}

// ExpectedSchemaVersion 返回嵌入目录中 *.up.sql 的最高 14 位时间戳版本号
// （platform-04 §2.3 V1：文件名 {ts}_name.up.sql，ts 为 14 位 UTC 时间戳）。
// 该值与 schema_migrations.version 同口径，供 /readyz R3 与测试共用；
// dirty 状态由 DB 判定，此处不涉及。无 up 文件或前缀非 14 位数字时返回错误。
func ExpectedSchemaVersion(dir embed.FS) (uint, error) {
	return expectedSchemaVersionFS(dir)
}

// expectedSchemaVersionFS 是 ExpectedSchemaVersion 的 fs.FS 实现，便于用
// fstest.MapFS fixture 测试。
func expectedSchemaVersionFS(fsys fs.FS) (uint, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return 0, fmt.Errorf("migrate: read embedded migrations: %w", err)
	}
	var (
		highest uint
		found   bool
	)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if !ok || len(prefix) != migrateVersionDigits || !isDigits(prefix) {
			return 0, fmt.Errorf(
				"migrate: migration file %q has no %d-digit timestamp version prefix",
				entry.Name(), migrateVersionDigits)
		}
		version, err := strconv.ParseUint(prefix, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("migrate: parse version from %q: %w", entry.Name(), err)
		}
		if !found || uint(version) > highest {
			highest = uint(version)
			found = true
		}
	}
	if !found {
		return 0, errors.New("migrate: embedded migrations contain no *.up.sql files")
	}
	return highest, nil
}

// isDigits 判断字符串是否全为 ASCII 数字（ParseUint 也接受正负号等，此处收紧）。
func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// prodMigrationExecutor 是生产实现：咨询锁走独立 *sql.Conn（会话级锁），
// 迁移走 golang-migrate 的 iofs 源 + pgx/v5 driver。
type prodMigrationExecutor struct{}

// AcquireLock 以 pg_try_advisory_lock 轮询获取会话级咨询锁；拿不到时每
// migrateLockRetryInterval 重试一次，ctx 结束返回其错误。
func (prodMigrationExecutor) AcquireLock(ctx context.Context, dsn string) (migrationLock, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		// 正在返回打开连接的主错误；关闭连接池失败是次要信息。
		_ = db.Close()
		return nil, fmt.Errorf("open database connection: %w", err)
	}
	lock := &pgAdvisoryLock{db: db, conn: conn}

	for {
		var acquired bool
		if err := conn.QueryRowContext(ctx,
			"SELECT pg_try_advisory_lock($1)", migrateAdvisoryLockKey).Scan(&acquired); err != nil {
			// 错误路径：锁获取失败的主错误优先，关闭连接的失败不覆盖它。
			_ = lock.close()
			return nil, fmt.Errorf("try advisory lock: %w", err)
		}
		if acquired {
			return lock, nil
		}
		timer := time.NewTimer(migrateLockRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = lock.close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// pgAdvisoryLock 持有一个专用连接：PG 咨询锁是会话级的，unlock 必须同连接。
type pgAdvisoryLock struct {
	db   *sql.DB
	conn *sql.Conn
}

func (l *pgAdvisoryLock) Release(ctx context.Context) error {
	_, unlockErr := l.conn.ExecContext(ctx,
		"SELECT pg_advisory_unlock($1)", migrateAdvisoryLockKey)
	closeErr := l.close()
	return errors.Join(unlockErr, closeErr)
}

func (l *pgAdvisoryLock) close() error {
	return errors.Join(l.conn.Close(), l.db.Close())
}

// Up 用 golang-migrate 库模式执行内嵌迁移。pgx driver 在无参数 Exec 时走
// simple protocol，单个 up 文件内的多语句 DDL 可整体执行（platform-04 §2.4
// 的 init 迁移即单文件多语句）。
func (prodMigrationExecutor) Up(ctx context.Context, dsn string, srcFS embed.FS) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	src, err := iofs.New(srcFS, ".")
	if err != nil {
		return fmt.Errorf("open embedded migration source: %w", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		// 正在返回 Ping 的主错误；关闭连接池失败是次要信息。
		_ = db.Close()
		return fmt.Errorf("ping database: %w", err)
	}
	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("create migrate driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		_ = driver.Close()
		return fmt.Errorf("create migrate instance: %w", err)
	}
	// m.Close 同时关闭 source 与 database driver（含底层 *sql.DB）。
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
