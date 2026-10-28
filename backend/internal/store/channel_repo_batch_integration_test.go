//go:build integration

package store

// P2-2：中继的跨协议候选过滤要一次取回全部候选渠道，而不是逐条点查。
// 本文件用真 PG 证明 GetMany 的行集/缺行/空入参语义。

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestChannelRepoGetMany(t *testing.T) {
	db := openChannelRuntimeTestDB(t)
	repo := NewChannelRepo(db)
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	a := insertRuntimeChannel(t, db, fmt.Sprintf("it-batch-a-%d", suffix), 1, `["m"]`, `{}`, "")
	b := insertRuntimeChannel(t, db, fmt.Sprintf("it-batch-b-%d", suffix), 3, `["m"]`, `{}`, "")

	got, err := repo.GetMany(ctx, []int64{a, b, a + 1_000_000})
	if err != nil {
		t.Fatalf("GetMany: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("GetMany 行数 = %d，期望 2（缺行不得报错）", len(got))
	}
	if ch, ok := got[a]; !ok || ch.ID != a || ch.Status != 1 {
		t.Fatalf("渠道 %d 读取结果 = %+v", a, got[a])
	}
	if ch, ok := got[b]; !ok || ch.ID != b || ch.Status != 3 {
		t.Fatalf("渠道 %d 读取结果 = %+v", b, got[b])
	}

	// 空入参不得到数据库（避免 ANY($1) 对空数组的全表语义）；返回空 map。
	empty, err := repo.GetMany(ctx, nil)
	if err != nil {
		t.Fatalf("GetMany(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("空入参结果 = %v，期望空 map", empty)
	}
}
