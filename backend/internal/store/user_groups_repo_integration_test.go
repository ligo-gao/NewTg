//go:build integration

package store

// M6c Task 13 的分组目录 SQL 验收：真实 PostgreSQL。
//
// 覆盖 brief §2 冻结的口径：
//   - 可用分组 = status=1 且（user_selectable 或 name = 用户当前分组）；
//   - 模型 = 启用渠道 supported_models 并集（group_tags NULL 或包含该分组），
//     且仅保留 model_pricing.status=1 的模型；去重、排序；
//   - 无模型的分组仍返回（Models 为空）。
//
// 运行（隔离库，先 migrate up）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test_t13?sslmode=disable' \
//	  go test -tags=integration ./internal/store/ -run TestUserGroupsRepo -v

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestUserGroupsRepoUsableGroupsAndModels(t *testing.T) {
	db := openPublicPricingTestDB(t)
	repo := NewUserGroupsRepo(db)
	ctx := context.Background()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
	ownGroup := "t13own" + suffix
	selGroup := "t13sel" + suffix
	offGroup := "t13off" + suffix
	modelOn := "t13-m1-" + suffix
	modelOff := "t13-m2-" + suffix
	modelSel := "t13-m3-" + suffix

	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}
	// 分组：own=当前分组（不可选但必须出现）；sel=可选；off=停用。
	mustExec(`INSERT INTO user_groups (name, description, ratio, user_selectable, status)
		VALUES ($1, 'own desc', 100, false, 1), ($2, 'sel desc', 120, true, 1), ($3, 'off', 100, true, 2)`,
		ownGroup, selGroup, offGroup)
	var userID int64
	if err := db.QueryRow(`INSERT INTO users (username, "group") VALUES ($1, $2) RETURNING id`,
		"t13u"+suffix, ownGroup).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, userID)
		_, _ = db.Exec(`DELETE FROM channels WHERE name LIKE $1`, "t13ch-%"+suffix)
		_, _ = db.Exec(`DELETE FROM model_pricing WHERE model = ANY($1::text[])`,
			[]string{modelOn, modelOff, modelSel})
		_, _ = db.Exec(`DELETE FROM user_groups WHERE name = ANY($1::text[])`,
			[]string{ownGroup, selGroup, offGroup})
	})

	// 渠道：1) 全分组；2) 仅 sel；3) 停用；4) 只要 group_tags 包含对应组的模型
	// 才可见。supported_models 必须非空（CHECK）。
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	insertChannel := func(name string, status int, tags any, models []string) {
		if _, err := tx.Exec(`INSERT INTO channels (name, type, credentials, supported_models, group_tags, model_mapping, status)
			VALUES ($1, 'openai', '\x00'::bytea, $2::jsonb, $3::jsonb, '{}'::jsonb, $4)`,
			name, jsonArray(models), tags, status); err != nil {
			t.Fatalf("insert channel %s: %v", name, err)
		}
	}
	insertChannel("t13ch-all-"+suffix, 1, nil, []string{modelOn, modelOff})
	insertChannel("t13ch-sel-"+suffix, 1, []string{selGroup}, []string{modelSel, modelOff})
	insertChannel("t13ch-off-"+suffix, 2, nil, []string{modelSel})
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	mustExec(`INSERT INTO model_pricing (model, mode, prompt_price, author, status) VALUES
		($1, 0, 1, 't13', 1), ($2, 0, 1, 't13', 2), ($3, 0, 1, 't13', 1)`,
		modelOn, modelOff, modelSel)

	groups, err := repo.UsableGroupsForUser(ctx, userID)
	if err != nil {
		t.Fatalf("UsableGroupsForUser: %v", err)
	}
	// 隔离库仍继承 seed 的 default 分组（user_selectable=true），因此这里按
	// 名字断言本用例的分组，而不假设总数（共享库可能还有其它可选组）。
	byName := map[string]UsableGroup{}
	for _, group := range groups {
		byName[group.Name] = group
	}
	own, ok := byName[ownGroup]
	if !ok {
		t.Fatalf("当前分组 %q 未出现（user_selectable=false 但 name=当前分组必须包含）", ownGroup)
	}
	if own.Description != "own desc" || own.Ratio != 100 {
		t.Fatalf("own group = %+v", own)
	}
	if strings.Join(own.Models, ",") != modelOn {
		t.Fatalf("own models = %v, want [%s]（停用定价模型必须排除）", own.Models, modelOn)
	}
	sel, ok := byName[selGroup]
	if !ok {
		t.Fatalf("可选分组 %q 未出现", selGroup)
	}
	if strings.Join(sel.Models, ",") != modelOn+","+modelSel {
		t.Fatalf("sel models = %v, want 去重排序的 [%s %s]", sel.Models, modelOn, modelSel)
	}
	if _, ok := byName[offGroup]; ok {
		t.Fatalf("停用分组 %q 被错误下发", offGroup)
	}

	// 当前分组回源：启用用户返回分组；不存在返回 ErrUserNotFound。
	got, err := repo.UserGroup(ctx, userID)
	if err != nil || got != ownGroup {
		t.Fatalf("UserGroup = (%q, %v), want (%q, nil)", got, err, ownGroup)
	}
	if _, err := repo.UserGroup(ctx, 1<<62); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("missing user err = %v, want ErrUserNotFound", err)
	}
}

// jsonArray 生成 jsonb 数组字面量（测试夹具；值来自固定模型名，不含引号）。
func jsonArray(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = `"` + value + `"`
	}
	return "[" + strings.Join(quoted, ",") + "]"
}
