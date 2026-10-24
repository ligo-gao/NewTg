//go:build integration

package store

// 本文件是 channels.upstream_account_id（上游用量对账设计 §4.1）的运行时集成测试。
//
// 为什么要连真 PG：归属列是 Task 1 迁移追加的可空 bigint + FK
// （channels_upstream_account_fk → upstream_accounts ON DELETE RESTRICT），
// channelColumns/scanChannel/Create/Update 的列序与 NULL 语义只有真正
// Prepare/Execute 才能证明；单测的 fake 不覆盖 SQL 形状。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	  go test -tags=integration ./internal/store/ -run 'TestChannelRepoUpstreamAccount' -v
//
// DSN 未设置时跳过；表结构由 migrations 负责，本测试只插入唯一命名的行并按
// id 清理（渠道先于账号删除，满足 FK RESTRICT）。

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// createChannelAttributionAccount 创建一个唯一命名的上游账号并登记清理。
// 清理顺序依赖 t.Cleanup 的 LIFO：调用方随后登记的渠道清理会先执行。
func createChannelAttributionAccount(t *testing.T, repo *UpstreamAccountRepo) *UpstreamAccount {
	t.Helper()
	account, err := repo.Create(context.Background(), UpstreamAccount{
		Name:     fmt.Sprintf("it-ch-attr-%d", time.Now().UnixNano()),
		Provider: "deepseek",
		Timezone: "Asia/Shanghai",
		Note:     "channel attribution integration test",
	})
	if err != nil {
		t.Fatalf("create upstream account: %v", err)
	}
	t.Cleanup(func() {
		if _, err := repo.db.Exec(`DELETE FROM upstream_accounts WHERE id = $1`, account.ID); err != nil {
			t.Errorf("cleanup upstream account %d: %v", account.ID, err)
		}
	})
	return account
}

// TestChannelRepoUpstreamAccount 覆盖归属列的完整读写面：
// create 带归属 → get 回读 → update 清空（NULL → nil）→ update 重新设置
// → 未携带 Set 的 PATCH 保持原值 → 未归属渠道读出 nil。
func TestChannelRepoUpstreamAccount(t *testing.T) {
	db := openChannelRuntimeTestDB(t)
	repo := NewChannelRepo(db)
	ctx := context.Background()

	account := createChannelAttributionAccount(t, NewUpstreamAccountRepo(db))
	accountID := account.ID

	created, err := repo.Create(ctx, &Channel{
		Name:              fmt.Sprintf("it-ch-attr-%d", time.Now().UnixNano()),
		Type:              "openai",
		Credentials:       []byte{0},
		SupportedModels:   []byte(`["it-model"]`),
		ModelMapping:      []byte(`{}`),
		Weight:            1,
		Priority:          10,
		Status:            1,
		AutoBan:           true,
		UpstreamAccountID: &accountID,
	})
	if err != nil {
		t.Fatalf("Create with attribution: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM channels WHERE id = $1`, created.ID); err != nil {
			t.Errorf("cleanup channel %d: %v", created.ID, err)
		}
	})
	if created.UpstreamAccountID == nil || *created.UpstreamAccountID != accountID {
		t.Fatalf("Create 返回 upstream_account_id = %v, want %d", created.UpstreamAccountID, accountID)
	}

	got, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	if got.UpstreamAccountID == nil || *got.UpstreamAccountID != accountID {
		t.Fatalf("Get upstream_account_id = %v, want %d", got.UpstreamAccountID, accountID)
	}

	cleared, err := repo.Update(ctx, created.ID, ChannelPatch{UpstreamAccountIDSet: true}, created.UpdatedAt)
	if err != nil {
		t.Fatalf("Update clear attribution: %v", err)
	}
	if cleared.UpstreamAccountID != nil {
		t.Fatalf("清空后 upstream_account_id = %v, want nil（SQL NULL）", cleared.UpstreamAccountID)
	}

	restored, err := repo.Update(ctx, created.ID,
		ChannelPatch{UpstreamAccountIDSet: true, UpstreamAccountID: &accountID}, cleared.UpdatedAt)
	if err != nil {
		t.Fatalf("Update restore attribution: %v", err)
	}
	if restored.UpstreamAccountID == nil || *restored.UpstreamAccountID != accountID {
		t.Fatalf("重新设置后 upstream_account_id = %v, want %d", restored.UpstreamAccountID, accountID)
	}

	// Set=false 的 PATCH 不得触碰归属列（与其它可空列一致）。
	untouched, err := repo.Update(ctx, created.ID,
		ChannelPatch{UpstreamAccountIDSet: false, UpstreamAccountID: nil}, restored.UpdatedAt)
	if err != nil {
		t.Fatalf("Update without attribution field: %v", err)
	}
	if untouched.UpstreamAccountID == nil || *untouched.UpstreamAccountID != accountID {
		t.Fatalf("未携带 Set 的 PATCH 改动归属: %v", untouched.UpstreamAccountID)
	}

	// 未归属渠道：NULL 扫描为 nil，而不是 0。
	plainID := insertRuntimeChannel(t, db, fmt.Sprintf("it-ch-noattr-%d", time.Now().UnixNano()),
		1, `["it-model"]`, `{}`, "")
	plain, err := repo.Get(ctx, plainID)
	if err != nil {
		t.Fatalf("Get unattributed channel: %v", err)
	}
	if plain.UpstreamAccountID != nil {
		t.Fatalf("未归属渠道 upstream_account_id = %v, want nil", plain.UpstreamAccountID)
	}
}
