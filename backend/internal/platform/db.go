package platform

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	// pgx/v5 stdlib 驱动为 Task 4 冻结选型（platform 计划）。
	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/config"
)

// OpenDB 以 pgx/v5 stdlib 驱动打开主库连接池，应用 DB_MAX_OPEN_CONNS /
// DB_MAX_IDLE_CONNS，并在 3s 内 Ping 验证；失败时关闭连接池不泄漏。
func OpenDB(ctx context.Context, cfg *config.Config) (*sql.DB, error) {
	db, err := sql.Open("pgx", cfg.MainDSN())
	if err != nil {
		return nil, fmt.Errorf("platform: open db: %w", err)
	}
	db.SetMaxOpenConns(cfg.DBMaxOpenConns)
	db.SetMaxIdleConns(cfg.DBMaxIdleConns)
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		// 正在返回 Ping 的主错误；清理连接池的失败是次要信息。
		_ = db.Close()
		return nil, fmt.Errorf("platform: ping db: %w", err)
	}
	return db, nil
}
