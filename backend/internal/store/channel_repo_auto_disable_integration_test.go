//go:build integration

package store

// W7/M4-a：AutoDisable 条件更新的真实 PostgreSQL 验收。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_m4?sslmode=disable' \
//	  go test -tags=integration -p 1 ./internal/store/ -run TestChannelRepoAutoDisable -v
//
// 为什么要连真 PG：条件 UPDATE 的并发语义（行锁 + WHERE status=1）和
// `RETURNING` 的 NULL/时间精度无法用 fake 证明；本文件的并发用例是
// 「并发只迁移一次」的权威断言。

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// insertAutoDisableChannel 插入一行并登记清理；reason 为空表示 SQL NULL。
func insertAutoDisableChannel(t *testing.T, repo *ChannelRepo, name string, status int16, autoBan bool, reason string) int64 {
	t.Helper()
	var reasonArg any
	if reason != "" {
		reasonArg = reason
	}
	var id int64
	err := repo.db.QueryRow(`
		INSERT INTO channels (name, type, credentials, supported_models, model_mapping,
			weight, priority, status, auto_ban, auto_disabled_reason)
		VALUES ($1, 'openai', $2, '["gpt-test"]'::jsonb, '{}'::jsonb, 1, 0, $3, $4, $5)
		RETURNING id`,
		name, []byte{0}, status, autoBan, reasonArg).Scan(&id)
	if err != nil {
		t.Fatalf("insert channel %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := repo.db.Exec(`DELETE FROM channels WHERE id = $1`, id); err != nil {
			t.Errorf("cleanup channel %d: %v", id, err)
		}
	})
	return id
}

// TestChannelRepoAutoDisableConditions 覆盖四条守卫：
// enabled+auto_ban → 迁移；manual_disabled / auto_disabled / auto_ban=false
// 一律 no-op 且不覆盖既有 reason。
func TestChannelRepoAutoDisableConditions(t *testing.T) {
	db := openChannelRuntimeTestDB(t)
	repo := NewChannelRepo(db)
	ctx := context.Background()

	t.Run("enabled-migrates", func(t *testing.T) {
		id := insertAutoDisableChannel(t, repo, "m4a-enabled", 1, true, "")
		before, err := repo.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get before: %v", err)
		}
		channel, changed, err := repo.AutoDisable(ctx, id, "channel_auth_error")
		if err != nil {
			t.Fatalf("AutoDisable: %v", err)
		}
		if !changed || channel == nil {
			t.Fatalf("changed=%v channel=%v, want true/non-nil", changed, channel)
		}
		if channel.Status != 3 {
			t.Fatalf("status = %d, want 3（auto_disabled）", channel.Status)
		}
		if channel.AutoDisabledReason == nil || *channel.AutoDisabledReason != "channel_auth_error" {
			t.Fatalf("reason = %v, want channel_auth_error", channel.AutoDisabledReason)
		}
		if channel.UpdatedAt.Before(before.UpdatedAt) {
			t.Fatalf("updated_at 倒退: before=%v after=%v", before.UpdatedAt, channel.UpdatedAt)
		}
		// 二次调用必须 no-op（并发上报的第二种路径）。
		again, changedAgain, err := repo.AutoDisable(ctx, id, "channel_keyword")
		if err != nil || changedAgain || again != nil {
			t.Fatalf("二次 AutoDisable = (%v,%v,%v), want (nil,false,nil)", again, changedAgain, err)
		}
	})

	t.Run("manual-disabled-unchanged", func(t *testing.T) {
		id := insertAutoDisableChannel(t, repo, "m4a-manual", 2, true, "")
		channel, changed, err := repo.AutoDisable(ctx, id, "channel_auth_error")
		if err != nil || changed || channel != nil {
			t.Fatalf("manual_disabled 被改写: (%v,%v,%v)", channel, changed, err)
		}
		after, err := repo.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if after.Status != 2 || after.AutoDisabledReason != nil {
			t.Fatalf("manual_disabled 行 = status %d reason %v", after.Status, after.AutoDisabledReason)
		}
	})

	t.Run("auto-disabled-reason-preserved", func(t *testing.T) {
		id := insertAutoDisableChannel(t, repo, "m4a-auto", 3, true, "channel_keyword")
		channel, changed, err := repo.AutoDisable(ctx, id, "channel_auth_error")
		if err != nil || changed || channel != nil {
			t.Fatalf("auto_disabled 被重复迁移: (%v,%v,%v)", channel, changed, err)
		}
		after, err := repo.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if after.AutoDisabledReason == nil || *after.AutoDisabledReason != "channel_keyword" {
			t.Fatalf("既有 reason 被覆盖: %v", after.AutoDisabledReason)
		}
	})

	t.Run("auto-ban-false-unchanged", func(t *testing.T) {
		id := insertAutoDisableChannel(t, repo, "m4a-autoban-off", 1, false, "")
		channel, changed, err := repo.AutoDisable(ctx, id, "channel_auth_error")
		if err != nil || changed || channel != nil {
			t.Fatalf("auto_ban=false 渠道被禁用: (%v,%v,%v)", channel, changed, err)
		}
		after, err := repo.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if after.Status != 1 {
			t.Fatalf("status = %d, want 1（保持 enabled）", after.Status)
		}
	})

	t.Run("missing-id-no-op", func(t *testing.T) {
		channel, changed, err := repo.AutoDisable(ctx, 1<<62, "channel_auth_error")
		if err != nil || changed || channel != nil {
			t.Fatalf("不存在的 id = (%v,%v,%v), want (nil,false,nil)", channel, changed, err)
		}
	})
}

// TestChannelRepoAutoDisableConcurrentSingleTransition：20 个并发调用只有
// 恰好一个 changed=true（变异：去掉 WHERE status=1 → 多次迁移/覆盖）。
func TestChannelRepoAutoDisableConcurrentSingleTransition(t *testing.T) {
	db := openChannelRuntimeTestDB(t)
	repo := NewChannelRepo(db)
	id := insertAutoDisableChannel(t, repo, "m4a-concurrent", 1, true, "")

	const workers = 20
	var changedCount atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, changed, err := repo.AutoDisable(context.Background(), id, "channel_auth_error")
			if err != nil {
				errs <- err
				return
			}
			if changed {
				changedCount.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("AutoDisable: %v", err)
	}
	if got := changedCount.Load(); got != 1 {
		t.Fatalf("changed=true 次数 = %d, want 1（并发只迁移一次）", got)
	}
	after, err := repo.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Status != 3 || after.AutoDisabledReason == nil || *after.AutoDisabledReason != "channel_auth_error" {
		t.Fatalf("最终行 = status %d reason %v", after.Status, after.AutoDisabledReason)
	}
}
