package httpapi

// 本文件用 miniredis 验证 RedisIdempotencyStore 的 pending/完成/释放生命周期
// （finding 2 的生产实现路径）。不依赖真实 Redis。

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestRedisIdempotencyStoreLifecycle(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = client.Close() }()
	idem := NewRedisIdempotencyStore(client)
	ctx := context.Background()

	claim, err := idem.Claim(ctx, "scope:key", time.Hour)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claim.Status != IdempotencyClaimAcquired || claim.Token == "" {
		t.Fatalf("first claim = %+v, want Acquired with token", claim)
	}

	// 同键并发请求看到 pending → InFlight，绝不执行 build。
	inFlight, err := idem.Claim(ctx, "scope:key", time.Hour)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if inFlight.Status != IdempotencyClaimInFlight {
		t.Fatalf("second claim status = %d, want InFlight", inFlight.Status)
	}

	// 旧 token 不得覆盖当前 pending 标记（CAS）。
	resp := IdempotentResponse{Status: 201, Body: []byte(`{"id":1}`)}
	if err := idem.Complete(ctx, "scope:key", "stale-token", resp, time.Hour); err != nil {
		t.Fatalf("Complete with stale token: %v", err)
	}
	if again, _ := idem.Claim(ctx, "scope:key", time.Hour); again.Status != IdempotencyClaimInFlight {
		t.Fatalf("stale Complete clobbered the pending marker: status=%d", again.Status)
	}

	// 正确 token 完成 → 后续请求重放响应。
	if err := idem.Complete(ctx, "scope:key", claim.Token, resp, time.Hour); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	replay, err := idem.Claim(ctx, "scope:key", time.Hour)
	if err != nil {
		t.Fatalf("replay Claim: %v", err)
	}
	if replay.Status != IdempotencyClaimReplay || replay.Response.Status != 201 || string(replay.Response.Body) != `{"id":1}` {
		t.Fatalf("replay = %+v, want the stored response", replay)
	}

	// Release 删除自己持有的 pending，键可再次被抢占。
	fresh, err := idem.Claim(ctx, "scope:other", time.Hour)
	if err != nil || fresh.Status != IdempotencyClaimAcquired {
		t.Fatalf("fresh claim = %+v, %v", fresh, err)
	}
	if err := idem.Release(ctx, "scope:other", fresh.Token); err != nil {
		t.Fatalf("Release: %v", err)
	}
	reclaimed, err := idem.Claim(ctx, "scope:other", time.Hour)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if reclaimed.Status != IdempotencyClaimAcquired {
		t.Fatalf("after Release status = %d, want Acquired", reclaimed.Status)
	}
}
