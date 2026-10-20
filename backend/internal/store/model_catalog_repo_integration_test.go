//go:build integration

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 本文件是 D1 模型目录仓储的集成验收：
//   - vendors：大小写不敏感唯一性、派生 version 乐观锁、引用保护（含停用模型）；
//   - assign/merge/delete 的批量事务语义（D1-08）；
//   - model_meta：CRUD、列表/搜索、缺失检测、批量删除的副作用、name_rule 解析优先级。
//
// 运行：NEWTG_TEST_DB_DSN=... go test -tags=integration ./internal/store/ -run TestModelCatalog -v

func catalogCtx() context.Context { return context.Background() }

// catalogUnique 返回本次运行独占的标识后缀；模型名合法字符集只含
// [A-Za-z0-9._:/-]，因此只用数字与连字符。
func catalogUnique(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func strPtr(s string) *string { return &s }
func i64Ptr(v int64) *int64   { return &v }

// applyWithFreshVersion 先 preview 取指纹，再 apply（与 D1b 的调用顺序一致）。
func applyWithFreshVersion(t *testing.T, repo *SQLModelCatalogRepo, op VendorOperation) (VendorOperationResult, error) {
	t.Helper()
	preview, err := repo.PreviewVendorOperations(catalogCtx(), op)
	if err != nil {
		return VendorOperationResult{}, err
	}
	op.ExpectedVersion = preview.ExpectedVersion
	return repo.ApplyVendorOperations(catalogCtx(), op)
}

func TestModelCatalogVendorCRUD(t *testing.T) {
	db := openTestDB(t)
	repo := NewModelCatalogRepo(db)
	ctx := catalogCtx()

	name := catalogUnique("vnd")
	v, err := repo.CreateVendor(ctx, VendorInput{Name: "  " + name + "  "})
	if err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM model_meta WHERE vendor_id = $1`, v.ID)
		_, _ = db.Exec(`DELETE FROM vendors WHERE id = $1`, v.ID)
	})
	if v.Status != 1 {
		t.Fatalf("默认 status = %d，want 1", v.Status)
	}
	// L-4：名称落库为 trim 后的值。
	if v.Name != name {
		t.Fatalf("name = %q，want %q（首尾裁剪）", v.Name, name)
	}

	// 大小写不同的同名必须 409（F-06-23 / TC-F-06-23-01）。
	if _, err := repo.CreateVendor(ctx, VendorInput{Name: strings.ToUpper(name)}); !errors.Is(err, ErrVendorDuplicate) {
		t.Fatalf("大小写重名 err = %v，want ErrVendorDuplicate", err)
	}

	got, err := repo.GetVendor(ctx, v.ID)
	if err != nil {
		t.Fatalf("get vendor: %v", err)
	}
	if got.ModelCount != 0 {
		t.Fatalf("新供应商 ModelCount = %d，want 0", got.ModelCount)
	}

	// 错误 version → 409；正确 version → 成功且版本变化（TC-F-06-23-03）。
	if _, err := repo.UpdateVendor(ctx, v.ID, VendorUpdate{Name: name, Status: 2}, "stale"); !errors.Is(err, ErrVendorConflict) {
		t.Fatalf("过期 version err = %v，want ErrVendorConflict", err)
	}
	updated, err := repo.UpdateVendor(ctx, v.ID,
		VendorUpdate{Name: name, Description: strPtr("desc"), Status: 2}, v.Version())
	if err != nil {
		t.Fatalf("update vendor: %v", err)
	}
	if updated.Version() == v.Version() {
		t.Fatalf("更新后 version 未变化")
	}
	if updated.Status != 2 || updated.Description == nil || *updated.Description != "desc" {
		t.Fatalf("更新结果不符：%+v", updated)
	}

	// 引用保护：含停用模型也算引用（D1-07 / TC-F-06-23-02）。
	meta, err := repo.CreateModelMeta(ctx, ModelMetaInput{
		ModelName: catalogUnique("model"), VendorID: &v.ID, Status: int16Ptr(2),
	})
	if err != nil {
		t.Fatalf("create meta: %v", err)
	}
	var ref ErrVendorReferenced
	if err := repo.DeleteVendor(ctx, v.ID); !errors.As(err, &ref) {
		t.Fatalf("引用中删除 err = %v，want ErrVendorReferenced", err)
	} else if ref.ReferenceCount != 1 {
		t.Fatalf("ReferenceCount = %d，want 1", ref.ReferenceCount)
	}

	// 解除引用后可删。
	if _, err := repo.UpdateModelMeta(ctx, meta.ID, ModelMetaPatch{VendorIDSet: true, VendorID: nil}, meta.UpdatedAt); err != nil {
		t.Fatalf("clear vendor ref: %v", err)
	}
	if err := repo.DeleteVendor(ctx, v.ID); err != nil {
		t.Fatalf("删除无引用供应商: %v", err)
	}
}

func int16Ptr(v int16) *int16 { return &v }

func TestModelCatalogVendorOperations(t *testing.T) {
	db := openTestDB(t)
	repo := NewModelCatalogRepo(db)
	ctx := catalogCtx()

	a, err := repo.CreateVendor(ctx, VendorInput{Name: catalogUnique("vop-a")})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	b, err := repo.CreateVendor(ctx, VendorInput{Name: catalogUnique("vop-b")})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	var metas []ModelMeta
	for i := 0; i < 2; i++ {
		m, err := repo.CreateModelMeta(ctx, ModelMetaInput{
			ModelName: catalogUnique(fmt.Sprintf("vop-model-%d", i)), VendorID: &a.ID,
		})
		if err != nil {
			t.Fatalf("create meta %d: %v", i, err)
		}
		metas = append(metas, m)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM model_meta WHERE id = ANY($1)`, []int64{metas[0].ID, metas[1].ID})
		_, _ = db.Exec(`DELETE FROM vendors WHERE id = ANY($1)`, []int64{a.ID, b.ID})
	})

	// assign：改挂到 B。
	ids := []int64{metas[0].ID, metas[1].ID}
	res, err := applyWithFreshVersion(t, repo, VendorOperation{
		Action: VendorActionAssign, ModelIDs: ids, TargetVendorID: b.ID,
	})
	if err != nil || res.Affected != 2 {
		t.Fatalf("assign = (%+v, %v)，want affected=2", res, err)
	}
	for _, id := range ids {
		got, _ := repo.GetModelMeta(ctx, id)
		if got.VendorID == nil || *got.VendorID != b.ID {
			t.Fatalf("assign 后 model %d vendor = %v，want %d", id, got.VendorID, b.ID)
		}
	}

	// assign target=0：清空归属（TC-F-06-24-03）。
	if _, err := applyWithFreshVersion(t, repo, VendorOperation{
		Action: VendorActionAssign, ModelIDs: ids,
	}); err != nil {
		t.Fatalf("assign clear: %v", err)
	}
	if got, _ := repo.GetModelMeta(ctx, ids[0]); got.VendorID != nil {
		t.Fatalf("assign target=0 后 vendor_id = %v，want NULL", got.VendorID)
	}

	// merge：把 A 作为源合并进 B；A 删除、模型改挂 B 或保持 NULL 不受影响。
	if _, err := applyWithFreshVersion(t, repo, VendorOperation{
		Action: VendorActionAssign, ModelIDs: []int64{ids[0]}, TargetVendorID: a.ID,
	}); err != nil {
		t.Fatalf("reassign to A: %v", err)
	}
	res, err = applyWithFreshVersion(t, repo, VendorOperation{
		Action: VendorActionMerge, VendorIDs: []int64{a.ID}, TargetVendorID: b.ID,
	})
	if err != nil || res.Affected != 1 {
		t.Fatalf("merge = (%+v, %v)，want affected=1", res, err)
	}
	if _, err := repo.GetVendor(ctx, a.ID); !errors.Is(err, ErrVendorNotFound) {
		t.Fatalf("merge 后源供应商 err = %v，want ErrVendorNotFound", err)
	}
	if got, _ := repo.GetModelMeta(ctx, ids[0]); got.VendorID == nil || *got.VendorID != b.ID {
		t.Fatalf("merge 后 model vendor = %v，want %d", got.VendorID, b.ID)
	}

	// delete 有引用 → 409 + 计数；空选择 → 422。
	if _, err := applyWithFreshVersion(t, repo, VendorOperation{
		Action: VendorActionDelete, VendorIDs: []int64{b.ID},
	}); !errors.Is(err, ErrVendorReferenced{}) {
		t.Fatalf("delete referenced err = %v，want ErrVendorReferenced", err)
	}
	if _, err := repo.ApplyVendorOperations(ctx, VendorOperation{Action: VendorActionAssign}); !errors.Is(err, ErrCatalogEmptySelection) {
		t.Fatalf("empty assign err = %v，want ErrCatalogEmptySelection", err)
	}
	if _, err := repo.ApplyVendorOperations(ctx, VendorOperation{
		Action: VendorActionAssign, ModelIDs: []int64{ids[0]}, TargetVendorID: 999999999,
	}); !errors.Is(err, ErrVendorNotFound) {
		t.Fatalf("assign 到不存在供应商 err = %v，want ErrVendorNotFound", err)
	}
}

func TestModelCatalogModelMetaCRUD(t *testing.T) {
	db := openTestDB(t)
	repo := NewModelCatalogRepo(db)
	ctx := catalogCtx()

	name := "openai/" + catalogUnique("meta")
	rec, err := repo.CreateModelMeta(ctx, ModelMetaInput{ModelName: "  " + name + "  "})
	if err != nil {
		t.Fatalf("create meta: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM model_meta WHERE id = $1`, rec.ID) })

	// 首尾空白裁剪 + 列默认值。
	if rec.ModelName != name {
		t.Fatalf("model_name = %q，want %q（首尾裁剪）", rec.ModelName, name)
	}
	if string(rec.Tags) != "[]" || string(rec.Endpoints) != "{}" ||
		string(rec.Capabilities) != "[]" || string(rec.Modalities) != "[]" || string(rec.Parameters) != "[]" {
		t.Fatalf("jsonb 默认值不符：tags=%s endpoints=%s", rec.Tags, rec.Endpoints)
	}
	if rec.Status != 1 || !rec.SyncOfficial || rec.NameRule != 0 {
		t.Fatalf("列默认值不符：status=%d sync=%v rule=%d", rec.Status, rec.SyncOfficial, rec.NameRule)
	}

	if _, err := repo.CreateModelMeta(ctx, ModelMetaInput{ModelName: name}); !errors.Is(err, ErrModelMetaDuplicate) {
		t.Fatalf("重名 err = %v，want ErrModelMetaDuplicate", err)
	}
	if _, err := repo.CreateModelMeta(ctx, ModelMetaInput{
		ModelName: catalogUnique("meta-bad-vendor"), VendorID: i64Ptr(999999999),
	}); !errors.Is(err, ErrVendorNotFound) {
		t.Fatalf("不存在 vendor err = %v，want ErrVendorNotFound", err)
	}

	list, total, err := repo.ListModelMeta(ctx, ModelMetaFilter{Page: 1, PageSize: 10, Keyword: name, Status: int16Ptr(1)})
	if err != nil || total != 1 || len(list) != 1 {
		t.Fatalf("list = (%d, %v, %v)，want 1 条", len(list), total, err)
	}
	found, err := repo.SearchModelMeta(ctx, name, 5)
	if err != nil || len(found) != 1 {
		t.Fatalf("search = (%d, %v)，want 1 条", len(found), err)
	}

	// 乐观锁：过期 updated_at → 409。
	if _, err := repo.UpdateModelMeta(ctx, rec.ID,
		ModelMetaPatch{Status: int16Ptr(2)}, rec.UpdatedAt.Add(-time.Hour)); !errors.Is(err, ErrModelMetaConflict) {
		t.Fatalf("过期 updated_at err = %v，want ErrModelMetaConflict", err)
	}
	updated, err := repo.UpdateModelMeta(ctx, rec.ID, ModelMetaPatch{Status: int16Ptr(2), TagsSet: true, Tags: []byte(`["a","b"]`)}, rec.UpdatedAt)
	if err != nil {
		t.Fatalf("update meta: %v", err)
	}
	if updated.Status != 2 || string(updated.Tags) != `["a", "b"]` {
		t.Fatalf("update 结果不符：status=%d tags=%s", updated.Status, updated.Tags)
	}

	if err := repo.DeleteModelMeta(ctx, rec.ID); err != nil {
		t.Fatalf("delete meta: %v", err)
	}
	if _, err := repo.GetModelMeta(ctx, rec.ID); !errors.Is(err, ErrModelMetaNotFound) {
		t.Fatalf("删除后 get err = %v，want ErrModelMetaNotFound", err)
	}
}

func TestModelCatalogResolveAndMissing(t *testing.T) {
	db := openTestDB(t)
	repo := NewModelCatalogRepo(db)
	ctx := catalogCtx()

	base := catalogUnique("mr")
	mustCreate := func(name string, rule int16, status int16) ModelMeta {
		rec, err := repo.CreateModelMeta(ctx, ModelMetaInput{
			ModelName: name, NameRule: rule, Status: int16Ptr(status),
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return rec
	}
	exact := mustCreate(base+"-exact", 0, 1)
	prefixShort := mustCreate(base+"-p", 1, 1)
	prefixLong := mustCreate(base+"-pp", 1, 1)
	suffix := mustCreate(base+"-suf", 3, 1)
	contains := mustCreate(base+"-mid", 2, 1)
	disabled := mustCreate(base+"-off", 0, 2)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM model_meta WHERE id = ANY($1)`,
			[]int64{exact.ID, prefixShort.ID, prefixLong.ID, suffix.ID, contains.ID, disabled.ID})
	})

	// 精确 > 前缀 > 后缀 > 包含；同规则内更长模式优先（长度降序）。
	resolved, err := repo.ResolveMeta(ctx, []string{
		base + "-exact",
		base + "-ppZ",
		"Z" + base + "-suf",
		"Z" + base + "-midZ",
		base + "-off",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := resolved[base+"-exact"]; got.ID != exact.ID {
		t.Fatalf("精确解析到 %d，want %d", got.ID, exact.ID)
	}
	if got := resolved[base+"-ppZ"]; got.ID != prefixLong.ID {
		t.Fatalf("前缀 tie-break 解析到 %d，want 更长的 %d", got.ID, prefixLong.ID)
	}
	if got := resolved["Z"+base+"-suf"]; got.ID != suffix.ID {
		t.Fatalf("后缀解析到 %d，want %d", got.ID, suffix.ID)
	}
	if got := resolved["Z"+base+"-midZ"]; got.ID != contains.ID {
		t.Fatalf("包含解析到 %d，want %d", got.ID, contains.ID)
	}
	if _, ok := resolved[base+"-off"]; ok {
		t.Fatalf("停用档案不应被解析")
	}

	// 缺失检测：启用渠道并集 − 精确档案；停用渠道不计（TC-F-06-26-01/02）。
	absent := catalogUnique("missing")
	supported := []byte(fmt.Sprintf(`[%q, %q]`, absent, exact.ModelName))
	disabledSupported := []byte(fmt.Sprintf(`[%q]`, catalogUnique("missing-disabled")))
	if _, err := db.Exec(`INSERT INTO channels (name, type, credentials, supported_models, status)
		VALUES ($1, 'openai', decode('00','hex'), $2, 1)`, catalogUnique("ch-on"), supported); err != nil {
		t.Fatalf("insert enabled channel: %v", err)
	}
	var disabledChannelID int64
	if err := db.QueryRow(`INSERT INTO channels (name, type, credentials, supported_models, status)
		VALUES ($1, 'openai', decode('00','hex'), $2, 3) RETURNING id`,
		catalogUnique("ch-off"), disabledSupported).Scan(&disabledChannelID); err != nil {
		t.Fatalf("insert disabled channel: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM channels WHERE supported_models @> $1::jsonb`, supported)
		_, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, disabledChannelID)
	})

	missing, err := repo.MissingModels(ctx)
	if err != nil {
		t.Fatalf("missing: %v", err)
	}
	byName := map[string]MissingModel{}
	for _, m := range missing.Items {
		byName[m.ModelName] = m
	}
	row, ok := byName[absent]
	if !ok {
		t.Fatalf("缺失模型 %s 未出现", absent)
	}
	if row.ChannelCount != 1 || row.HasPricing {
		t.Fatalf("缺失行 = %+v，want channel_count=1 has_pricing=false", row)
	}
	if _, ok := byName[exact.ModelName]; ok {
		t.Fatalf("已建档模型不应出现在缺失清单")
	}
}

func TestModelCatalogBatchDeleteSideEffects(t *testing.T) {
	db := openTestDB(t)
	repo := NewModelCatalogRepo(db)
	ctx := catalogCtx()

	name := catalogUnique("bd")
	other := catalogUnique("bd-other")
	rec, err := repo.CreateModelMeta(ctx, ModelMetaInput{ModelName: name})
	if err != nil {
		t.Fatalf("create meta: %v", err)
	}
	_, err = db.Exec(`INSERT INTO model_pricing (model, prompt_price, completion_price) VALUES ($1, 100, 200)`, name)
	if err != nil {
		t.Fatalf("insert pricing: %v", err)
	}
	_, err = db.Exec(`INSERT INTO group_pricing ("group", model) SELECT name, $1 FROM user_groups LIMIT 1`, name)
	if err != nil {
		t.Fatalf("insert group pricing: %v", err)
	}
	supported, _ := json.Marshal([]string{name, other})
	var channelID int64
	if err := db.QueryRow(`INSERT INTO channels (name, type, credentials, supported_models, status)
		VALUES ($1, 'openai', decode('00','hex'), $2, 1) RETURNING id`,
		catalogUnique("ch-bd"), supported).Scan(&channelID); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM group_pricing WHERE model = ANY($1)`, []string{name, other})
		_, _ = db.Exec(`DELETE FROM model_pricing WHERE model = ANY($1)`, []string{name, other})
		_, _ = db.Exec(`DELETE FROM model_meta WHERE id = $1`, rec.ID)
		_, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, channelID)
	})

	res, err := repo.BatchDeleteModelMeta(ctx, []int64{rec.ID}, true, true)
	if err != nil {
		t.Fatalf("batch delete: %v", err)
	}
	if res.Requested != 1 || res.Deleted != 1 || res.ChannelsUpdated != 1 || res.PricingDeleted != 1 || res.GroupPricingDeleted != 1 {
		t.Fatalf("结果 = %+v，want requested/deleted/channels/pricing/group=1", res)
	}
	var models []byte
	if err := db.QueryRow(`SELECT supported_models FROM channels WHERE id = $1`, channelID).Scan(&models); err != nil {
		t.Fatalf("read channel: %v", err)
	}
	var got []string
	if err := json.Unmarshal(models, &got); err != nil {
		t.Fatalf("decode supported_models: %v", err)
	}
	if len(got) != 1 || got[0] != other {
		t.Fatalf("supported_models = %v，want [%s]", got, other)
	}
	var pricing int
	if err := db.QueryRow(`SELECT count(*) FROM model_pricing WHERE model = $1`, name).Scan(&pricing); err != nil {
		t.Fatalf("count pricing: %v", err)
	}
	if pricing != 0 {
		t.Fatalf("定价未清除")
	}
}

// insertCatalogChannel 插入一条测试渠道；credentials 用空字节占位（列 NOT NULL）。
func insertCatalogChannel(t *testing.T, db *sql.DB, supported []byte, status int16) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`INSERT INTO channels (name, type, credentials, supported_models, status)
		VALUES ($1, 'openai', decode('00','hex'), $2, $3) RETURNING id`,
		catalogUnique("ch-d1a"), supported, status).Scan(&id); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	return id
}

// TestModelCatalogVendorVersionAndPreview 覆盖 M-1：操作指纹的 preview/apply
// 一致性与冲突报告。
func TestModelCatalogVendorVersionAndPreview(t *testing.T) {
	db := openTestDB(t)
	repo := NewModelCatalogRepo(db)
	ctx := catalogCtx()

	a, err := repo.CreateVendor(ctx, VendorInput{Name: catalogUnique("vv-a")})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	b, err := repo.CreateVendor(ctx, VendorInput{Name: catalogUnique("vv-b")})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	meta, err := repo.CreateModelMeta(ctx, ModelMetaInput{ModelName: catalogUnique("vv-model")})
	if err != nil {
		t.Fatalf("create meta: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM model_meta WHERE id = $1`, meta.ID)
		_, _ = db.Exec(`DELETE FROM vendors WHERE id = ANY($1)`, []int64{a.ID, b.ID})
	})

	op := VendorOperation{Action: VendorActionAssign, ModelIDs: []int64{meta.ID}, TargetVendorID: a.ID}
	preview, err := repo.PreviewVendorOperations(ctx, op)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if preview.Affected != 1 {
		t.Fatalf("preview Affected = %d，want 1", preview.Affected)
	}
	if len(preview.ExpectedVersion) != 64 {
		t.Fatalf("ExpectedVersion = %q，want 64 hex", preview.ExpectedVersion)
	}
	if len(preview.Vendors) != 1 || preview.Vendors[0].VendorID != a.ID {
		t.Fatalf("preview Vendors = %+v，want 只有 A", preview.Vendors)
	}

	// 空 expected_version 一律冲突，且不产生写入。
	_, err = repo.ApplyVendorOperations(ctx, op)
	var conflict ErrVendorVersionConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("空 expected err = %v，want ErrVendorVersionConflict", err)
	}
	if len(conflict.Conflicts) != 1 || conflict.Conflicts[0].VendorID != a.ID {
		t.Fatalf("Conflicts = %+v，want A 一条", conflict.Conflicts)
	}
	if got, _ := repo.GetModelMeta(ctx, meta.ID); got.VendorID != nil {
		t.Fatalf("冲突时 model 已改挂：%v", got.VendorID)
	}

	// 供应商版本漂移后，旧指纹失效；preview 也应报告冲突。
	if _, err := repo.UpdateVendor(ctx, a.ID,
		VendorUpdate{Name: a.Name, Description: strPtr("changed"), Status: 1}, a.Version()); err != nil {
		t.Fatalf("update vendor: %v", err)
	}
	op.ExpectedVersion = preview.ExpectedVersion
	_, err = repo.ApplyVendorOperations(ctx, op)
	if !errors.As(err, &conflict) {
		t.Fatalf("过期指纹 err = %v，want ErrVendorVersionConflict", err)
	}
	if len(conflict.Conflicts) != 1 || conflict.Conflicts[0].VendorID != a.ID {
		t.Fatalf("过期 Conflicts = %+v，want A 一条", conflict.Conflicts)
	}
	if conflict.Conflicts[0].CurrentVersion == preview.Vendors[0].Version {
		t.Fatalf("冲突报告应携带新版本")
	}
	stalePreview, err := repo.PreviewVendorOperations(ctx, op)
	if err != nil {
		t.Fatalf("stale preview: %v", err)
	}
	if len(stalePreview.Conflicts) != 1 {
		t.Fatalf("stale preview Conflicts = %+v，want 1", stalePreview.Conflicts)
	}

	// 新指纹可执行；merge 的指纹覆盖 target + sources。
	op.ExpectedVersion = stalePreview.ExpectedVersion
	if _, err := repo.ApplyVendorOperations(ctx, op); err != nil {
		t.Fatalf("fresh apply: %v", err)
	}
	mergeOp := VendorOperation{Action: VendorActionMerge, VendorIDs: []int64{a.ID}, TargetVendorID: b.ID}
	mergePreview, err := repo.PreviewVendorOperations(ctx, mergeOp)
	if err != nil {
		t.Fatalf("merge preview: %v", err)
	}
	if mergePreview.Affected != 1 || len(mergePreview.Vendors) != 2 {
		t.Fatalf("merge preview = %+v，want affected=1 / vendors=2", mergePreview)
	}
	mergeOp.ExpectedVersion = mergePreview.ExpectedVersion
	if res, err := repo.ApplyVendorOperations(ctx, mergeOp); err != nil || res.Affected != 1 {
		t.Fatalf("merge apply = (%+v, %v)", res, err)
	}
	// merge 后只剩 B；delete preview 的影响数 = 供应商行数。
	deleteOp := VendorOperation{Action: VendorActionDelete, VendorIDs: []int64{b.ID}}
	deletePreview, err := repo.PreviewVendorOperations(ctx, deleteOp)
	if err != nil {
		t.Fatalf("delete preview: %v", err)
	}
	if deletePreview.Affected != 1 {
		t.Fatalf("delete preview Affected = %d，want 1", deletePreview.Affected)
	}
}

// TestModelCatalogVendorOperationBoundaries 覆盖 M-2 的边界与哨兵优先级。
func TestModelCatalogVendorOperationBoundaries(t *testing.T) {
	db := openTestDB(t)
	repo := NewModelCatalogRepo(db)
	ctx := catalogCtx()

	// 空选择优先于版本冲突。
	if _, err := repo.ApplyVendorOperations(ctx, VendorOperation{Action: VendorActionAssign}); !errors.Is(err, ErrCatalogEmptySelection) {
		t.Fatalf("空选择 err = %v，want ErrCatalogEmptySelection", err)
	}
	// 1001 条 → TooLarge（版本校验之前）。
	tooMany := make([]int64, 0, 1001)
	for i := int64(1); i <= 1001; i++ {
		tooMany = append(tooMany, i)
	}
	if _, err := repo.ApplyVendorOperations(ctx, VendorOperation{
		Action: VendorActionAssign, ModelIDs: tooMany,
	}); !errors.Is(err, ErrCatalogTooLarge) {
		t.Fatalf("1001 条 err = %v，want ErrCatalogTooLarge", err)
	}

	a, err := repo.CreateVendor(ctx, VendorInput{Name: catalogUnique("vb-a")})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	meta, err := repo.CreateModelMeta(ctx, ModelMetaInput{ModelName: catalogUnique("vb-model")})
	if err != nil {
		t.Fatalf("create meta: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM model_meta WHERE id = $1`, meta.ID)
		_, _ = db.Exec(`DELETE FROM vendors WHERE id = $1`, a.ID)
	})

	// merge 的 target 不能是源。
	if _, err := repo.ApplyVendorOperations(ctx, VendorOperation{
		Action: VendorActionMerge, VendorIDs: []int64{a.ID}, TargetVendorID: a.ID,
	}); !errors.Is(err, ErrVendorOperationInvalid) {
		t.Fatalf("merge target=source err = %v，want ErrVendorOperationInvalid", err)
	}
	// assign 含不存在 id → 可识别的 NotFound（且整体回滚）。
	op := VendorOperation{Action: VendorActionAssign,
		ModelIDs: []int64{meta.ID, 999999999}, TargetVendorID: a.ID}
	preview, err := repo.PreviewVendorOperations(ctx, op)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	op.ExpectedVersion = preview.ExpectedVersion
	if _, err := repo.ApplyVendorOperations(ctx, op); !errors.Is(err, ErrModelMetaNotFound) {
		t.Fatalf("不存在 model err = %v，want ErrModelMetaNotFound", err)
	}
	if got, _ := repo.GetModelMeta(ctx, meta.ID); got.VendorID != nil {
		t.Fatalf("失败操作不应部分生效：vendor=%v", got.VendorID)
	}
}

// isContextTimeout 识别「行锁等待被 context 截止」的错误（驱动措辞随版本变化）。
func isContextTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "context canceled")
}

// TestModelCatalogVendorRowLocking 覆盖 M-2：两个独立连接池下，供应商行的
// FOR UPDATE 锁会让 delete/assign 阻塞；释放后同一操作成功。
func TestModelCatalogVendorRowLocking(t *testing.T) {
	dbA := openTestDB(t)
	dbB := openTestDB(t)
	repoA := NewModelCatalogRepo(dbA)
	repoB := NewModelCatalogRepo(dbB)
	ctx := catalogCtx()

	v, err := repoA.CreateVendor(ctx, VendorInput{Name: catalogUnique("lock-v")})
	if err != nil {
		t.Fatalf("create vendor: %v", err)
	}
	meta, err := repoA.CreateModelMeta(ctx, ModelMetaInput{ModelName: catalogUnique("lock-model")})
	if err != nil {
		t.Fatalf("create meta: %v", err)
	}
	t.Cleanup(func() {
		_, _ = dbA.Exec(`DELETE FROM model_meta WHERE id = $1`, meta.ID)
		_, _ = dbA.Exec(`DELETE FROM vendors WHERE id = $1`, v.ID)
	})

	op := VendorOperation{Action: VendorActionAssign, ModelIDs: []int64{meta.ID}, TargetVendorID: v.ID}
	preview, err := repoB.PreviewVendorOperations(ctx, op)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	op.ExpectedVersion = preview.ExpectedVersion

	tx, err := dbA.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var locked int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM vendors WHERE id = $1 FOR UPDATE`, v.ID).Scan(&locked); err != nil {
		t.Fatalf("lock vendor: %v", err)
	}

	t.Run("delete blocked", func(t *testing.T) {
		ctxB, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
		defer cancel()
		if err := repoB.DeleteVendor(ctxB, v.ID); !isContextTimeout(err) {
			t.Fatalf("持锁期间 delete err = %v，want 超时", err)
		}
	})
	t.Run("assign blocked", func(t *testing.T) {
		ctxB, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
		defer cancel()
		if _, err := repoB.ApplyVendorOperations(ctxB, op); !isContextTimeout(err) {
			t.Fatalf("持锁期间 assign err = %v，want 超时", err)
		}
	})
	t.Run("not found variant", func(t *testing.T) {
		// 行在锁等待期间不可见为「不存在」也是可接受的可识别结果。
		ctxB, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		err := repoB.DeleteVendor(ctxB, v.ID)
		if !isContextTimeout(err) && !errors.Is(err, ErrVendorNotFound) {
			t.Fatalf("delete err = %v，want 超时或 ErrVendorNotFound", err)
		}
	})

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	// 锁释放后同一操作成功（版本未变，指纹仍有效）。
	res, err := repoB.ApplyVendorOperations(ctx, op)
	if err != nil || res.Affected != 1 {
		t.Fatalf("解锁后 assign = (%+v, %v)", res, err)
	}
	got, err := repoB.GetModelMeta(ctx, meta.ID)
	if err != nil || got.VendorID == nil || *got.VendorID != v.ID {
		t.Fatalf("解锁后 model vendor = %v（err=%v），want %d", got.VendorID, err, v.ID)
	}
}

// TestModelCatalogBatchDeleteGuards 覆盖 H-1 空渠道哨兵与两个「不生效」边界。
func TestModelCatalogBatchDeleteGuards(t *testing.T) {
	db := openTestDB(t)
	repo := NewModelCatalogRepo(db)
	ctx := catalogCtx()

	// 1) 单模型渠道：移除会让渠道变空 → 哨兵 + model_meta/model_pricing 均保留。
	name := catalogUnique("bd-single")
	rec, err := repo.CreateModelMeta(ctx, ModelMetaInput{ModelName: name})
	if err != nil {
		t.Fatalf("create meta: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO model_pricing (model, prompt_price, completion_price) VALUES ($1, 100, 200)`, name); err != nil {
		t.Fatalf("insert pricing: %v", err)
	}
	singleModel := []byte(fmt.Sprintf(`[%q]`, name))
	channelID := insertCatalogChannel(t, db, singleModel, 1)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM model_pricing WHERE model = $1`, name)
		_, _ = db.Exec(`DELETE FROM model_meta WHERE id = $1`, rec.ID)
		_, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, channelID)
	})

	_, err = repo.BatchDeleteModelMeta(ctx, []int64{rec.ID}, true, true)
	var empty ErrChannelModelsEmpty
	if !errors.As(err, &empty) {
		t.Fatalf("单模型渠道 err = %v，want ErrChannelModelsEmpty", err)
	}
	if len(empty.ChannelIDs) != 1 || empty.ChannelIDs[0] != channelID {
		t.Fatalf("ChannelIDs = %v，want [%d]", empty.ChannelIDs, channelID)
	}
	if _, err := repo.GetModelMeta(ctx, rec.ID); err != nil {
		t.Fatalf("哨兵返回后 model_meta 不应被删：%v", err)
	}
	var pricing int
	if err := db.QueryRow(`SELECT count(*) FROM model_pricing WHERE model = $1`, name).Scan(&pricing); err != nil || pricing != 1 {
		t.Fatalf("哨兵返回后定价不应被删：count=%d err=%v", pricing, err)
	}

	// 2) removeFromChannels=false：允许删除，渠道清单不变。
	nameKeep := catalogUnique("bd-keep")
	recKeep, err := repo.CreateModelMeta(ctx, ModelMetaInput{ModelName: nameKeep})
	if err != nil {
		t.Fatalf("create keep meta: %v", err)
	}
	keepChannel := insertCatalogChannel(t, db, []byte(fmt.Sprintf(`[%q]`, nameKeep)), 1)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM model_meta WHERE id = $1`, recKeep.ID)
		_, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, keepChannel)
	})
	res, err := repo.BatchDeleteModelMeta(ctx, []int64{recKeep.ID}, false, false)
	if err != nil || res.Deleted != 1 || res.ChannelsUpdated != 0 {
		t.Fatalf("removeFromChannels=false = (%+v, %v)，want deleted=1/channels=0", res, err)
	}
	var keepModels []byte
	if err := db.QueryRow(`SELECT supported_models FROM channels WHERE id = $1`, keepChannel).Scan(&keepModels); err != nil {
		t.Fatalf("read channel: %v", err)
	}
	var keepList []string
	if err := json.Unmarshal(keepModels, &keepList); err != nil || len(keepList) != 1 || keepList[0] != nameKeep {
		t.Fatalf("渠道清单被改动：%s（err=%v）", keepModels, err)
	}

	// 3) name_rule≠0：不从渠道移除（只有精确规则才有「移除同名模型」的语义）。
	nameRule := catalogUnique("bd-rule")
	recRule, err := repo.CreateModelMeta(ctx, ModelMetaInput{ModelName: nameRule, NameRule: 2})
	if err != nil {
		t.Fatalf("create rule meta: %v", err)
	}
	ruleChannel := insertCatalogChannel(t, db, []byte(fmt.Sprintf(`[%q]`, nameRule)), 1)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM model_meta WHERE id = $1`, recRule.ID)
		_, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, ruleChannel)
	})
	res, err = repo.BatchDeleteModelMeta(ctx, []int64{recRule.ID}, true, false)
	if err != nil || res.Deleted != 1 || res.ChannelsUpdated != 0 {
		t.Fatalf("name_rule=2 = (%+v, %v)，want deleted=1/channels=0", res, err)
	}
	var ruleModels []byte
	if err := db.QueryRow(`SELECT supported_models FROM channels WHERE id = $1`, ruleChannel).Scan(&ruleModels); err != nil {
		t.Fatalf("read rule channel: %v", err)
	}
	var ruleList []string
	if err := json.Unmarshal(ruleModels, &ruleList); err != nil || len(ruleList) != 1 || ruleList[0] != nameRule {
		t.Fatalf("非精确规则不应改渠道：%s（err=%v）", ruleModels, err)
	}
}

// TestModelCatalogBatchDeleteCounts 覆盖 L-5：部分 id 不存在按计数返回。
func TestModelCatalogBatchDeleteCounts(t *testing.T) {
	db := openTestDB(t)
	repo := NewModelCatalogRepo(db)
	ctx := catalogCtx()

	var ids []int64
	for i := 0; i < 2; i++ {
		rec, err := repo.CreateModelMeta(ctx, ModelMetaInput{ModelName: catalogUnique(fmt.Sprintf("bd-count-%d", i))})
		if err != nil {
			t.Fatalf("create meta %d: %v", i, err)
		}
		ids = append(ids, rec.ID)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM model_meta WHERE id = ANY($1)`, ids) })

	requested := []int64{ids[0], ids[1], 999999999, ids[0]}
	res, err := repo.BatchDeleteModelMeta(ctx, requested, false, false)
	if err != nil {
		t.Fatalf("batch delete: %v", err)
	}
	if res.Requested != 3 || res.Deleted != 2 {
		t.Fatalf("结果 = %+v，want requested=3 deleted=2（去重 + 部分不存在）", res)
	}
}

// TestModelCatalogMissingSkipsInvalidChannels 覆盖 M-3：含非法元素的渠道整渠道跳过。
func TestModelCatalogMissingSkipsInvalidChannels(t *testing.T) {
	db := openTestDB(t)
	repo := NewModelCatalogRepo(db)
	ctx := catalogCtx()

	before, err := repo.MissingModels(ctx)
	if err != nil {
		t.Fatalf("missing before: %v", err)
	}

	invalidName := catalogUnique("missing-invalid")
	invalid := []byte(fmt.Sprintf(`[%q, {"object": true}, "   ", 42]`, invalidName))
	invalidChannel := insertCatalogChannel(t, db, invalid, 1)
	validName := catalogUnique("missing-valid")
	validChannel := insertCatalogChannel(t, db, []byte(fmt.Sprintf(`[%q]`, validName)), 1)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM channels WHERE id = ANY($1)`, []int64{invalidChannel, validChannel})
	})

	after, err := repo.MissingModels(ctx)
	if err != nil {
		t.Fatalf("missing after: %v", err)
	}
	if after.SkippedChannels != before.SkippedChannels+1 {
		t.Fatalf("SkippedChannels = %d，want %d（+1）", after.SkippedChannels, before.SkippedChannels)
	}
	byName := map[string]MissingModel{}
	for _, m := range after.Items {
		byName[m.ModelName] = m
	}
	if _, ok := byName[invalidName]; ok {
		t.Fatalf("非法渠道的模型不应计入缺失清单")
	}
	row, ok := byName[validName]
	if !ok || row.ChannelCount != 1 || row.HasPricing {
		t.Fatalf("合法渠道行 = %+v ok=%v，want count=1 / has_pricing=false", row, ok)
	}
}

// TestValidateModelMetaInput 是 L-3 的表驱动单测：每条规则至少一正一反。
// 纯逻辑，不依赖数据库（仅因与集成用例同文件而随 integration 标签运行）。
func TestValidateModelMetaInput(t *testing.T) {
	tags := func(parts ...string) []byte {
		raw, _ := json.Marshal(parts)
		return raw
	}
	longTag := strings.Repeat("t", 33)
	manyTags := make([]string, 0, 11)
	for i := 0; i < 11; i++ {
		manyTags = append(manyTags, fmt.Sprintf("tag-%d", i))
	}

	cases := []struct {
		name      string
		mutate    func(*ModelMeta)
		wantField string // 空表示应通过
	}{
		{"description 512 ok", func(m *ModelMeta) { m.Description = strPtr(strings.Repeat("d", 512)) }, ""},
		{"description 513 rejected", func(m *ModelMeta) { m.Description = strPtr(strings.Repeat("d", 513)) }, "description"},
		{"display_name 64 ok", func(m *ModelMeta) { m.DisplayName = strPtr(strings.Repeat("n", 64)) }, ""},
		{"display_name 65 rejected", func(m *ModelMeta) { m.DisplayName = strPtr(strings.Repeat("n", 65)) }, "display_name"},
		{"display_name empty ok", func(m *ModelMeta) { m.DisplayName = strPtr("") }, ""},
		{"tags ok", func(m *ModelMeta) { m.Tags = tags("a", "B") }, ""},
		{"tags empty ok", func(m *ModelMeta) { m.Tags = []byte(`[]`) }, ""},
		{"tags 10 ok", func(m *ModelMeta) { m.Tags = tags(manyTags[:10]...) }, ""},
		{"tags 11 rejected", func(m *ModelMeta) { m.Tags = tags(manyTags...) }, "tags"},
		{"tags non-string rejected", func(m *ModelMeta) { m.Tags = []byte(`["a", 1]`) }, "tags"},
		{"tags not array rejected", func(m *ModelMeta) { m.Tags = []byte(`"a"`) }, "tags"},
		{"tag blank rejected", func(m *ModelMeta) { m.Tags = []byte(`["  "]`) }, "tags"},
		{"tag too long rejected", func(m *ModelMeta) { m.Tags = tags(longTag) }, "tags"},
		{"tag duplicate rejected", func(m *ModelMeta) { m.Tags = tags("a", "a") }, "tags"},
		{"tag case-sensitive distinct ok", func(m *ModelMeta) { m.Tags = tags("a", "A") }, ""},
		{"tag trimmed ok", func(m *ModelMeta) { m.Tags = tags("  a  ") }, ""},
		{"endpoints ok", func(m *ModelMeta) { m.Endpoints = []byte(`{"/v1/chat/completions":["POST","get"]}`) }, ""},
		{"endpoints empty object ok", func(m *ModelMeta) { m.Endpoints = []byte(`{}`) }, ""},
		{"endpoints array rejected", func(m *ModelMeta) { m.Endpoints = []byte(`[]`) }, "endpoints"},
		{"endpoint path without slash rejected", func(m *ModelMeta) { m.Endpoints = []byte(`{"v1/x":["GET"]}`) }, "endpoints"},
		{"endpoint empty methods rejected", func(m *ModelMeta) { m.Endpoints = []byte(`{"/x":[]}`) }, "endpoints"},
		{"endpoint unknown method rejected", func(m *ModelMeta) { m.Endpoints = []byte(`{"/x":["TRACE"]}`) }, "endpoints"},
		{"endpoint non-array rejected", func(m *ModelMeta) { m.Endpoints = []byte(`{"/x":"GET"}`) }, "endpoints"},
		{"capabilities ok", func(m *ModelMeta) { m.Capabilities = []byte(`["text","vision","function_calling"]`) }, ""},
		{"capability unknown rejected", func(m *ModelMeta) { m.Capabilities = []byte(`["telepathy"]`) }, "capabilities"},
		{"capability non-string rejected", func(m *ModelMeta) { m.Capabilities = []byte(`[1]`) }, "capabilities"},
		{"modalities ok", func(m *ModelMeta) { m.Modalities = []byte(`["text","image"]`) }, ""},
		{"modality unknown rejected", func(m *ModelMeta) { m.Modalities = []byte(`["smell"]`) }, "modalities"},
		{"parameters ok", func(m *ModelMeta) {
			m.Parameters = []byte(`[{"name":"temperature","type":"number"},{"name":"stream","type":"boolean"}]`)
		}, ""},
		{"parameters not array rejected", func(m *ModelMeta) { m.Parameters = []byte(`{}`) }, "parameters"},
		{"parameter not object rejected", func(m *ModelMeta) { m.Parameters = []byte(`[1]`) }, "parameters"},
		{"parameter empty name rejected", func(m *ModelMeta) { m.Parameters = []byte(`[{"name":" ","type":"string"}]`) }, "parameters"},
		{"parameter unknown type rejected", func(m *ModelMeta) { m.Parameters = []byte(`[{"name":"x","type":"date"}]`) }, "parameters"},
		{"knowledge_cutoff ok", func(m *ModelMeta) { m.KnowledgeCutoff = strPtr("2025-01") }, ""},
		{"knowledge_cutoff month 00 rejected", func(m *ModelMeta) { m.KnowledgeCutoff = strPtr("2025-00") }, "knowledge_cutoff"},
		{"knowledge_cutoff month 13 rejected", func(m *ModelMeta) { m.KnowledgeCutoff = strPtr("2025-13") }, "knowledge_cutoff"},
		{"knowledge_cutoff format rejected", func(m *ModelMeta) { m.KnowledgeCutoff = strPtr("2025-1") }, "knowledge_cutoff"},
		{"context_length zero ok", func(m *ModelMeta) { m.ContextLength = i64Ptr(0) }, ""},
		{"context_length negative rejected", func(m *ModelMeta) { m.ContextLength = i64Ptr(-1) }, "context_length"},
		{"max_output negative rejected", func(m *ModelMeta) { m.MaxOutput = i64Ptr(-1) }, "max_output"},
		{"unset fields skipped", func(m *ModelMeta) {}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &ModelMeta{}
			tc.mutate(m)
			err := ValidateModelMetaInput(m)
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("want 通过，got %v", err)
				}
				return
			}
			var invalid ErrModelMetaInvalid
			if !errors.As(err, &invalid) {
				t.Fatalf("want ErrModelMetaInvalid(%s)，got %v", tc.wantField, err)
			}
			if invalid.Field != tc.wantField {
				t.Fatalf("Field = %q，want %q（err=%v）", invalid.Field, tc.wantField, err)
			}
		})
	}
}
