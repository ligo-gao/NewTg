//go:build integration

package store

// 折扣仓储的集成测试：验证 spec §5.2 的解析语义（用户归属分组、有效期左闭
// 右开、停用/过期/悬挂目标不命中）与 §4.5 的级联辅助。
//
// 运行：NEWTG_TEST_DB_DSN=postgres://... go test -tags=integration ./internal/store/
//
// 测试库与其它 store 集成测试共享，因此每个用例都用纳秒后缀生成唯一的模型名、
// 分组名与用户名，并在 t.Cleanup 中按依赖顺序清理（折扣 → 用户 → 分组）。

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"testing"
	"time"
)

func discountTestSuffix() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func insertDiscountTestGroup(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO user_groups (name) VALUES ($1)`, name); err != nil {
		t.Fatalf("insert group %s: %v", name, err)
	}
}

// insertDiscountTestUser 建一个属于 group 的用户并返回 id。
func insertDiscountTestUser(t *testing.T, db *sql.DB, username, group string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(
		`INSERT INTO users (username, "group") VALUES ($1, $2) RETURNING id`,
		username, group).Scan(&id); err != nil {
		t.Fatalf("insert user %s: %v", username, err)
	}
	return id
}

// cleanupDiscountTestData 按依赖顺序清理：折扣无 FK，先按模型前缀删除；用户
// 先于分组删除（users.group → user_groups.name RESTRICT）；api_tokens 随用户
// 级联删除。
func cleanupDiscountTestData(t *testing.T, db *sql.DB, modelPrefix string, userID int64, groups ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		if modelPrefix != "" {
			_, _ = db.ExecContext(ctx, `DELETE FROM discounts WHERE model_name LIKE $1`, modelPrefix+"%")
		}
		if userID != 0 {
			_, _ = db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
		}
		for _, group := range groups {
			_, _ = db.ExecContext(ctx, `DELETE FROM user_groups WHERE name = $1`, group)
		}
	})
}

// TestDiscountRepoResolveUsesUserGroupNotTokenGroup：分组规则按用户归属分组
// （users."group"）匹配，而不是令牌上的 api_tokens."group"。令牌分组里放了
// 一条更低的 rate，若读取了令牌分组，它会胜出——命中 acme 的 80 即证明没有。
func TestDiscountRepoResolveUsesUserGroupNotTokenGroup(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	suffix := discountTestSuffix()
	userGroup := "usr" + suffix
	tokenGroup := "tok" + suffix
	model := "dsc" + suffix + "-group"

	insertDiscountTestGroup(t, db, userGroup)
	insertDiscountTestGroup(t, db, tokenGroup)
	userID := insertDiscountTestUser(t, db, "dsc"+suffix, userGroup)
	cleanupDiscountTestData(t, db, "dsc"+suffix, userID, userGroup, tokenGroup)
	if _, err := db.Exec(
		`INSERT INTO api_tokens (user_id, name, key_hash, "group") VALUES ($1, $2, $3, $4)`,
		userID, "dsc-token", "dsc-hash-"+suffix, tokenGroup); err != nil {
		t.Fatalf("insert api token: %v", err)
	}

	repo := NewCustomerDiscountRepo(db)
	if _, err := repo.Create(ctx, CustomerDiscountInput{
		ModelName: model, TargetType: "group", TargetID: userGroup,
		RatePercent: 80, Enabled: true,
	}); err != nil {
		t.Fatalf("create user-group rule: %v", err)
	}
	if _, err := repo.Create(ctx, CustomerDiscountInput{
		ModelName: model, TargetType: "group", TargetID: tokenGroup,
		RatePercent: 50, Enabled: true,
	}); err != nil {
		t.Fatalf("create token-group rule: %v", err)
	}

	got, ok, err := repo.Resolve(ctx, userID, []string{model}, time.Now().UTC())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !ok {
		t.Fatal("Resolve ok=false，期望命中用户归属分组的规则")
	}
	if got.Target() != "group:"+userGroup || got.RatePercent != 80 {
		t.Fatalf("Resolve = %s rate %d，期望 group:%s rate 80（不得使用令牌分组）",
			got.Target(), got.RatePercent, userGroup)
	}
}

// TestDiscountRepoResolveWindowRightOpen：有效期左闭右开——valid_until 当刻不
// 命中，前一微秒命中；valid_from 当刻命中，前一微秒不命中。
func TestDiscountRepoResolveWindowRightOpen(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	suffix := discountTestSuffix()
	group := "win" + suffix
	at := time.Now().UTC().Truncate(time.Microsecond)

	insertDiscountTestGroup(t, db, group)
	userID := insertDiscountTestUser(t, db, "dsc"+suffix, group)
	cleanupDiscountTestData(t, db, "dsc"+suffix, userID, group)

	repo := NewCustomerDiscountRepo(db)
	until := at
	modelUntil := "dsc" + suffix + "-until"
	if _, err := repo.Create(ctx, CustomerDiscountInput{
		ModelName: modelUntil, TargetType: "group", TargetID: group,
		RatePercent: 80, ValidUntil: &until, Enabled: true,
	}); err != nil {
		t.Fatalf("create rule with valid_until: %v", err)
	}

	t.Run("valid_until 当刻不命中", func(t *testing.T) {
		if _, ok, err := repo.Resolve(ctx, userID, []string{modelUntil}, at); err != nil {
			t.Fatalf("Resolve at valid_until: %v", err)
		} else if ok {
			t.Fatal("Resolve at valid_until 命中，期望右开不命中")
		}
	})
	t.Run("valid_until 前一微秒命中", func(t *testing.T) {
		if _, ok, err := repo.Resolve(ctx, userID, []string{modelUntil},
			at.Add(-time.Microsecond)); err != nil {
			t.Fatalf("Resolve before valid_until: %v", err)
		} else if !ok {
			t.Fatal("Resolve 在 valid_until 之前未命中，期望命中")
		}
	})

	from := at
	modelFrom := "dsc" + suffix + "-from"
	if _, err := repo.Create(ctx, CustomerDiscountInput{
		ModelName: modelFrom, TargetType: "group", TargetID: group,
		RatePercent: 80, ValidFrom: &from, Enabled: true,
	}); err != nil {
		t.Fatalf("create rule with valid_from: %v", err)
	}
	t.Run("valid_from 当刻命中", func(t *testing.T) {
		if _, ok, err := repo.Resolve(ctx, userID, []string{modelFrom}, at); err != nil {
			t.Fatalf("Resolve at valid_from: %v", err)
		} else if !ok {
			t.Fatal("Resolve at valid_from 未命中，期望左闭命中")
		}
	})
	t.Run("valid_from 前一微秒不命中", func(t *testing.T) {
		if _, ok, err := repo.Resolve(ctx, userID, []string{modelFrom},
			at.Add(-time.Microsecond)); err != nil {
			t.Fatalf("Resolve before valid_from: %v", err)
		} else if ok {
			t.Fatal("Resolve 在 valid_from 之前命中，期望不命中")
		}
	})
}

// TestDiscountRepoResolveIgnoresDisabledAndExpired：停用规则与已过期规则都不
// 参与匹配（原价）。
func TestDiscountRepoResolveIgnoresDisabledAndExpired(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	suffix := discountTestSuffix()
	group := "exp" + suffix
	at := time.Now().UTC().Truncate(time.Microsecond)

	insertDiscountTestGroup(t, db, group)
	userID := insertDiscountTestUser(t, db, "dsc"+suffix, group)
	cleanupDiscountTestData(t, db, "dsc"+suffix, userID, group)

	repo := NewCustomerDiscountRepo(db)
	modelDisabled := "dsc" + suffix + "-disabled"
	if _, err := repo.Create(ctx, CustomerDiscountInput{
		ModelName: modelDisabled, TargetType: "group", TargetID: group,
		RatePercent: 10, Enabled: false,
	}); err != nil {
		t.Fatalf("create disabled rule: %v", err)
	}
	modelExpired := "dsc" + suffix + "-expired"
	past := at.Add(-time.Hour)
	if _, err := repo.Create(ctx, CustomerDiscountInput{
		ModelName: modelExpired, TargetType: "group", TargetID: group,
		RatePercent: 10, ValidUntil: &past, Enabled: true,
	}); err != nil {
		t.Fatalf("create expired rule: %v", err)
	}

	for _, model := range []string{modelDisabled, modelExpired} {
		model := model
		t.Run(model, func(t *testing.T) {
			if got, ok, err := repo.Resolve(ctx, userID, []string{model}, at); err != nil {
				t.Fatalf("Resolve(%s): %v", model, err)
			} else if ok {
				t.Fatalf("Resolve(%s) 命中 %s，停用/过期规则必须不参与匹配", model, got.Target())
			}
		})
	}
}

// TestDiscountRepoResolveIgnoresDanglingTarget：指向不存在用户 id 的规则（手工
// 绕过应用层级的级联路径）不会命中真实用户，按原价结算。
func TestDiscountRepoResolveIgnoresDanglingTarget(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	suffix := discountTestSuffix()
	group := "dng" + suffix
	model := "dsc" + suffix + "-dangling"

	insertDiscountTestGroup(t, db, group)
	userID := insertDiscountTestUser(t, db, "dsc"+suffix, group)
	cleanupDiscountTestData(t, db, "dsc"+suffix, userID, group)

	repo := NewCustomerDiscountRepo(db)
	if _, err := repo.Create(ctx, CustomerDiscountInput{
		ModelName: model, TargetType: "user", TargetID: "999999999999",
		RatePercent: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("create dangling rule: %v", err)
	}
	got, ok, err := repo.Resolve(ctx, userID, []string{model}, time.Now().UTC())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ok {
		t.Fatalf("Resolve 命中悬挂规则 %s，期望不命中（原价）", got.Target())
	}
}

// TestDiscountRepoTargetExists：存在的用户/分组返回 true，不存在或非法 id
// 文本返回 false（不报错）。
func TestDiscountRepoTargetExists(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	suffix := discountTestSuffix()
	group := "exs" + suffix

	insertDiscountTestGroup(t, db, group)
	userID := insertDiscountTestUser(t, db, "dsc"+suffix, group)
	cleanupDiscountTestData(t, db, "", userID, group)

	repo := NewCustomerDiscountRepo(db)
	cases := []struct {
		name       string
		targetType string
		targetID   string
		want       bool
	}{
		{"存在用户", "user", strconv.FormatInt(userID, 10), true},
		{"不存在用户", "user", "999999999999", false},
		{"非法用户 id", "user", "not-a-number", false},
		{"存在分组", "group", group, true},
		{"不存在分组", "group", "missing" + suffix, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.TargetExists(ctx, tc.targetType, tc.targetID)
			if err != nil {
				t.Fatalf("TargetExists(%s,%s): %v", tc.targetType, tc.targetID, err)
			}
			if got != tc.want {
				t.Fatalf("TargetExists(%s,%s) = %v，期望 %v",
					tc.targetType, tc.targetID, got, tc.want)
			}
		})
	}
}

// TestDiscountRepoCreateDuplicateReturnsErr：同一 (model, target) 的第二条规则
// 被唯一索引拒绝，并映射为 ErrDiscountDuplicate。
func TestDiscountRepoCreateDuplicateReturnsErr(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	suffix := discountTestSuffix()
	group := "dup" + suffix
	model := "dsc" + suffix + "-dup"

	insertDiscountTestGroup(t, db, group)
	cleanupDiscountTestData(t, db, "dsc"+suffix, 0, group)

	repo := NewCustomerDiscountRepo(db)
	in := CustomerDiscountInput{
		ModelName: model, TargetType: "group", TargetID: group,
		RatePercent: 90, Enabled: true,
	}
	first, err := repo.Create(ctx, in)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if first.ID == 0 || first.RatePercent != 90 {
		t.Fatalf("first Create = %+v，期望回读 id 与 rate 90", first)
	}
	if _, err := repo.Create(ctx, in); !errors.Is(err, ErrDiscountDuplicate) {
		t.Fatalf("second Create = %v，期望 ErrDiscountDuplicate", err)
	}
}

// TestDiscountRepoCascadeRenameAndDelete：分组改名时规则跟到新名，删除目标时
// 规则清空（与 spec §4.5 的级联口径一致）。
func TestDiscountRepoCascadeRenameAndDelete(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	suffix := discountTestSuffix()
	oldName := "old" + suffix
	newName := "new" + suffix
	model := "dsc" + suffix + "-cascade"

	insertDiscountTestGroup(t, db, oldName)
	insertDiscountTestGroup(t, db, newName)
	cleanupDiscountTestData(t, db, "dsc"+suffix, 0, oldName, newName)

	repo := NewCustomerDiscountRepo(db)
	if _, err := repo.Create(ctx, CustomerDiscountInput{
		ModelName: model, TargetType: "group", TargetID: oldName,
		RatePercent: 70, Enabled: true,
	}); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	n, err := RenameGroupDiscountTargets(ctx, db, oldName, newName)
	if err != nil {
		t.Fatalf("RenameGroupDiscountTargets: %v", err)
	}
	if n != 1 {
		t.Fatalf("RenameGroupDiscountTargets 更新 %d 行，期望 1", n)
	}
	if rows, err := repo.ListForTarget(ctx, "group", oldName); err != nil {
		t.Fatalf("ListForTarget(old): %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("旧分组仍有 %d 条规则，期望 0", len(rows))
	}
	rows, err := repo.ListForTarget(ctx, "group", newName)
	if err != nil {
		t.Fatalf("ListForTarget(new): %v", err)
	}
	if len(rows) != 1 || rows[0].TargetID != newName || rows[0].ModelName != model {
		t.Fatalf("新分组规则 = %+v，期望 1 条指向 %s", rows, newName)
	}

	n, err = DeleteDiscountsForTarget(ctx, db, "group", newName)
	if err != nil {
		t.Fatalf("DeleteDiscountsForTarget: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteDiscountsForTarget 删除 %d 行，期望 1", n)
	}
	if rows, err := repo.ListForTarget(ctx, "group", newName); err != nil {
		t.Fatalf("ListForTarget after delete: %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("删除后仍有 %d 条规则，期望 0", len(rows))
	}
}

// TestDiscountRepoListEnabledModels：只列 model_pricing.status=1 的模型，q 做
// 大小写不敏感的包含匹配，结果按模型名排序。
func TestDiscountRepoListEnabledModels(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	suffix := discountTestSuffix()
	prefix := "dsc" + suffix
	alpha := prefix + "-alpha"
	beta := prefix + "-beta"
	disabled := prefix + "-gamma-disabled"

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM model_pricing WHERE model LIKE $1`, prefix+"%")
	})
	for model, status := range map[string]int{alpha: 1, beta: 1, disabled: 2} {
		if _, err := db.Exec(
			`INSERT INTO model_pricing (model, status) VALUES ($1, $2)`, model, status); err != nil {
			t.Fatalf("insert model_pricing %s: %v", model, err)
		}
	}

	repo := NewCustomerDiscountRepo(db)
	got, err := repo.ListEnabledModels(ctx, suffix, 100)
	if err != nil {
		t.Fatalf("ListEnabledModels: %v", err)
	}
	want := []string{alpha, beta}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ListEnabledModels = %v，期望 %v（仅 status=1 且按名称排序）", got, want)
	}

	got, err = repo.ListEnabledModels(ctx, suffix, 1)
	if err != nil {
		t.Fatalf("ListEnabledModels limit=1: %v", err)
	}
	if len(got) != 1 || got[0] != alpha {
		t.Fatalf("ListEnabledModels limit=1 = %v，期望 [%s]", got, alpha)
	}
}

// TestDiscountRepoCRUD：Create 回读、Get、Update（全字段覆盖并刷新
// updated_at）、Delete 以及不存在时的 ErrDiscountNotFound。
func TestDiscountRepoCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	suffix := discountTestSuffix()
	group := "crd" + suffix
	model := "dsc" + suffix + "-crud"
	otherModel := "dsc" + suffix + "-crud-2"

	insertDiscountTestGroup(t, db, group)
	cleanupDiscountTestData(t, db, "dsc"+suffix, 0, group)

	repo := NewCustomerDiscountRepo(db)
	created, err := repo.Create(ctx, CustomerDiscountInput{
		ModelName: model, TargetType: "group", TargetID: group,
		RatePercent: 90, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, ok, err := repo.Get(ctx, created.ID)
	if err != nil || !ok {
		t.Fatalf("Get = (%+v, %v, %v)，期望命中", got, ok, err)
	}
	if got.ModelName != model || got.RatePercent != 90 || !got.Enabled {
		t.Fatalf("Get = %+v，与 Create 写入值不符", got)
	}

	updated, err := repo.Update(ctx, created.ID, CustomerDiscountInput{
		ModelName: model, TargetType: "group", TargetID: group,
		RatePercent: 75, Enabled: false,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.RatePercent != 75 || updated.Enabled {
		t.Fatalf("Update = %+v，期望 rate 75 / disabled", updated)
	}
	if updated.UpdatedAt.Before(created.UpdatedAt) {
		t.Fatalf("Update updated_at = %v，早于创建时间 %v", updated.UpdatedAt, created.UpdatedAt)
	}

	// 第二条规则改到同 model 撞唯一键 → ErrDiscountDuplicate。
	second, err := repo.Create(ctx, CustomerDiscountInput{
		ModelName: otherModel, TargetType: "group", TargetID: group,
		RatePercent: 60, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	if _, err := repo.Update(ctx, second.ID, CustomerDiscountInput{
		ModelName: model, TargetType: "group", TargetID: group,
		RatePercent: 60, Enabled: true,
	}); !errors.Is(err, ErrDiscountDuplicate) {
		t.Fatalf("Update 撞唯一键 = %v，期望 ErrDiscountDuplicate", err)
	}

	if err := repo.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, err := repo.Get(ctx, created.ID); !errors.Is(err, ErrDiscountNotFound) || ok {
		t.Fatalf("Get after Delete = (ok=%v, err=%v)，期望 false/ErrDiscountNotFound", ok, err)
	}
	if err := repo.Delete(ctx, created.ID); !errors.Is(err, ErrDiscountNotFound) {
		t.Fatalf("Delete again = %v，期望 ErrDiscountNotFound", err)
	}
}
