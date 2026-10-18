//go:build integration

package configcenter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	// pgx/v5 stdlib 驱动为 Task 4 冻结选型（platform-04 §9.1 同款）。
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
)

// 本文件是 BE-6 的集成验收：两个 Center 实例共享真实 PG + Redis。
// 运行：
//
//	NEWTG_TEST_DB_DSN=postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration ./internal/configcenter/ -v

func openIntegration(t *testing.T) (*sql.DB, *redis.Client) {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	addr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("NEWTG_TEST_REDIS_ADDR 未设置，跳过集成测试")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		rdb.Close()
		t.Fatalf("ping test redis: %v", err)
	}
	t.Cleanup(func() { rdb.Close() })

	return db, rdb
}

// TestIntegrationPubSubAndOptimisticLock：A 写入后 B 在 1s（轮询上限 950ms）内看到新值；
// 未知键 ok=false；陈旧 updated_at 写入返回 ErrConflict。
func TestIntegrationPubSubAndOptimisticLock(t *testing.T) {
	db, rdb := openIntegration(t)

	key := fmt.Sprintf("test.be6.%d", time.Now().UnixNano())
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM system_settings WHERE "key" = $1`, key); err != nil {
			t.Logf("cleanup %q: %v", key, err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	a := New(db, rdb)
	b := New(db, rdb)
	if err := a.Load(ctx); err != nil {
		t.Fatalf("实例 A Load: %v", err)
	}
	if err := b.Load(ctx); err != nil {
		t.Fatalf("实例 B Load: %v", err)
	}
	if err := b.Subscribe(ctx); err != nil {
		t.Fatalf("实例 B Subscribe: %v", err)
	}

	// （c）未知键 Get → ok=false。
	if _, ok := b.Get(key); ok {
		t.Fatalf("未知键 %q Get 应返回 ok=false", key)
	}
	if _, ok := b.Version(key); ok {
		t.Fatalf("未知键 %q Version 应返回 ok=false", key)
	}

	type payload struct {
		N int `json:"n"`
	}

	// 首次写入（expectUpdatedAt=nil → upsert）。
	if err := a.Save(ctx, key, payload{N: 1}, nil); err != nil {
		t.Fatalf("实例 A Save: %v", err)
	}

	// 实例 B 必须 ≤1s 生效；轮询断言上限 950ms。
	deadline := time.Now().Add(950 * time.Millisecond)
	var got json.RawMessage
	var ok bool
	for {
		got, ok = b.Get(key)
		if ok {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("实例 B 在 950ms 内未通过 pub/sub 看到 key %q 的新值", key)
		}
		time.Sleep(5 * time.Millisecond)
	}
	var p payload
	if err := json.Unmarshal(got, &p); err != nil {
		t.Fatalf("实例 B 读到的值不是合法 JSON: %v (%s)", err, got)
	}
	if p.N != 1 {
		t.Fatalf("实例 B 读到 n=%d，期望 1", p.N)
	}

	// 乐观锁：用版本 1 的 updated_at 写入版本 2，再用同一陈旧版本写入 → ErrConflict。
	version, ok := a.Version(key)
	if !ok {
		t.Fatalf("实例 A 未记录 key %q 的版本", key)
	}
	if err := a.Save(ctx, key, payload{N: 2}, &version); err != nil {
		t.Fatalf("实例 A 带期望版本 Save: %v", err)
	}
	if err := b.Save(ctx, key, payload{N: 3}, &version); !errors.Is(err, ErrConflict) {
		t.Fatalf("陈旧 updated_at 写入应返回 ErrConflict，实际 %v", err)
	}

	// 版本 2 也应经 pub/sub 到达 B。
	deadline = time.Now().Add(950 * time.Millisecond)
	for {
		got, ok = b.Get(key)
		if ok {
			p = payload{}
			if err := json.Unmarshal(got, &p); err != nil {
				t.Fatalf("实例 B 读到的值不是合法 JSON: %v (%s)", err, got)
			}
			if p.N == 2 {
				break
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("实例 B 在 950ms 内未看到版本 2（当前 %s）", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
