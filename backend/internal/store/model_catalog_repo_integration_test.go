//go:build integration

package store

import (
	"context"
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

func TestModelCatalogVendorCRUD(t *testing.T) {
	db := openTestDB(t)
	repo := NewModelCatalogRepo(db)
	ctx := catalogCtx()

	name := catalogUnique("vnd")
	v, err := repo.CreateVendor(ctx, VendorInput{Name: name})
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
	res, err := repo.ApplyVendorOperations(ctx, VendorOperation{
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
	if _, err := repo.ApplyVendorOperations(ctx, VendorOperation{
		Action: VendorActionAssign, ModelIDs: ids,
	}); err != nil {
		t.Fatalf("assign clear: %v", err)
	}
	if got, _ := repo.GetModelMeta(ctx, ids[0]); got.VendorID != nil {
		t.Fatalf("assign target=0 后 vendor_id = %v，want NULL", got.VendorID)
	}

	// merge：把 A 作为源合并进 B；A 删除、模型改挂 B 或保持 NULL 不受影响。
	if _, err := repo.ApplyVendorOperations(ctx, VendorOperation{
		Action: VendorActionAssign, ModelIDs: []int64{ids[0]}, TargetVendorID: a.ID,
	}); err != nil {
		t.Fatalf("reassign to A: %v", err)
	}
	res, err = repo.ApplyVendorOperations(ctx, VendorOperation{
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
	if _, err := repo.ApplyVendorOperations(ctx, VendorOperation{
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
	for _, m := range missing {
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
	if res.Deleted != 1 || res.ChannelsUpdated != 1 || res.PricingDeleted != 1 || res.GroupPricingDeleted != 1 {
		t.Fatalf("结果 = %+v，want deleted/channels/pricing/group=1", res)
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
