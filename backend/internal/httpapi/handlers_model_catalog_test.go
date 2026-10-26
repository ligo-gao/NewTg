package httpapi

// D1b 模型目录 / 供应商目录端点的单元验收。
//
// 依赖全部是内存假实现（fakeCatalogRepo + fakeCatalogDerived + miniredis），
// 未打 integration 标签时不触碰外部 PG/Redis。真实 SQL 的乐观锁、大小写唯一
// 索引与引用保护由 handlers_model_catalog_integration_test.go 覆盖。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/egress"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

// ---------------------------------------------------------------------------
// 假仓储
// ---------------------------------------------------------------------------

type fakeCatalogRepo struct {
	mu          sync.Mutex
	models      map[int64]*store.ModelMeta
	vendors     map[int64]*store.Vendor
	nextModelID int64
	nextVendor  int64
	clock       time.Time
	// missing 是 MissingModels 的固定返回。
	missing store.MissingResult
	// emptiedChannels 模拟「移除后会变空的渠道」：模型名 → 渠道 id。
	emptiedChannels map[string][]int64
	// vendorOpsCalls 记录 apply 调用次数（幂等/事务测试观察）。
	applyCalls int
}

func newFakeCatalogRepo() *fakeCatalogRepo {
	return &fakeCatalogRepo{
		models:          map[int64]*store.ModelMeta{},
		vendors:         map[int64]*store.Vendor{},
		clock:           time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC),
		emptiedChannels: map[string][]int64{},
	}
}

func (r *fakeCatalogRepo) tick() time.Time {
	r.clock = r.clock.Add(time.Second)
	return r.clock
}

func cloneModel(rec store.ModelMeta) store.ModelMeta { return rec }

func (r *fakeCatalogRepo) cloneVendor(rec store.Vendor) store.Vendor { return rec }

// ---- vendors ----

func (r *fakeCatalogRepo) CreateVendor(_ context.Context, in store.VendorInput) (store.Vendor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return store.Vendor{}, store.ErrVendorInvalid
	}
	for _, vendor := range r.vendors {
		if strings.EqualFold(vendor.Name, name) {
			return store.Vendor{}, store.ErrVendorDuplicate
		}
	}
	status := int16(1)
	if in.Status != nil {
		status = *in.Status
	}
	now := r.tick()
	r.nextVendor++
	rec := &store.Vendor{
		ID: r.nextVendor, Name: name, Description: in.Description, Icon: in.Icon,
		Status: status, CreatedAt: now, UpdatedAt: now,
	}
	r.vendors[rec.ID] = rec
	return *rec, nil
}

func (r *fakeCatalogRepo) GetVendor(_ context.Context, id int64) (store.Vendor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.vendors[id]
	if !ok {
		return store.Vendor{}, store.ErrVendorNotFound
	}
	out := *rec
	out.ModelCount = r.modelCountLocked(id)
	return out, nil
}

func (r *fakeCatalogRepo) modelCountLocked(vendorID int64) int64 {
	var count int64
	for _, model := range r.models {
		if model.VendorID != nil && *model.VendorID == vendorID {
			count++
		}
	}
	return count
}

func (r *fakeCatalogRepo) UpdateVendor(_ context.Context, id int64, in store.VendorUpdate, expectVersion string) (store.Vendor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.vendors[id]
	if !ok {
		return store.Vendor{}, store.ErrVendorNotFound
	}
	if expectVersion == "" || rec.Version() != expectVersion {
		return store.Vendor{}, store.ErrVendorConflict
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return store.Vendor{}, store.ErrVendorInvalid
	}
	for otherID, vendor := range r.vendors {
		if otherID != id && strings.EqualFold(vendor.Name, name) {
			return store.Vendor{}, store.ErrVendorDuplicate
		}
	}
	rec.Name, rec.Description, rec.Icon, rec.Status = name, in.Description, in.Icon, in.Status
	rec.UpdatedAt = r.tick()
	return *rec, nil
}

func (r *fakeCatalogRepo) DeleteVendor(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.vendors[id]; !ok {
		return store.ErrVendorNotFound
	}
	if refs := r.modelCountLocked(id); refs > 0 {
		return store.ErrVendorReferenced{ReferenceCount: refs}
	}
	delete(r.vendors, id)
	return nil
}

func (r *fakeCatalogRepo) ListVendors(_ context.Context, filter store.VendorFilter) ([]store.Vendor, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]store.Vendor, 0, len(r.vendors))
	for _, rec := range r.vendors {
		if filter.Keyword != "" && !strings.Contains(strings.ToLower(rec.Name), strings.ToLower(filter.Keyword)) {
			continue
		}
		if filter.Status != nil && rec.Status != *filter.Status {
			continue
		}
		out := *rec
		out.ModelCount = r.modelCountLocked(rec.ID)
		if filter.Attached != nil {
			attached := out.ModelCount > 0
			if attached != *filter.Attached {
				continue
			}
		}
		items = append(items, out)
	}
	sort.Slice(items, func(i, j int) bool { return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name) })
	total := int64(len(items))
	return fakePaginate(items, filter.Page, filter.PageSize), total, nil
}

func fakePaginate[T any](items []T, page, pageSize int) []T {
	start := (page - 1) * pageSize
	if start >= len(items) {
		return nil
	}
	end := start + pageSize
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

func (r *fakeCatalogRepo) SearchVendors(_ context.Context, keyword string, limit int) ([]store.Vendor, error) {
	items, _, err := r.ListVendors(context.Background(), store.VendorFilter{
		Page: 1, PageSize: limit, Keyword: keyword,
	})
	return items, err
}

func fakeVendorFingerprint(vendors []store.Vendor) string {
	sorted := append([]store.Vendor(nil), vendors...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	var b strings.Builder
	for i, vendor := range sorted {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d=%s", vendor.ID, vendor.Version())
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func (r *fakeCatalogRepo) involvedVendors(op store.VendorOperation) ([]int64, error) {
	dedupe := func(values []int64) []int64 { return dedupeInt64(values) }
	switch op.Action {
	case store.VendorActionAssign:
		ids := dedupe(op.ModelIDs)
		if len(ids) == 0 {
			return nil, store.ErrCatalogEmptySelection
		}
		if len(ids) > 1000 {
			return nil, store.ErrCatalogTooLarge
		}
		if op.TargetVendorID == 0 {
			return nil, nil
		}
		return []int64{op.TargetVendorID}, nil
	case store.VendorActionMerge:
		ids := dedupe(op.VendorIDs)
		if len(ids) == 0 {
			return nil, store.ErrCatalogEmptySelection
		}
		if len(ids) > 1000 {
			return nil, store.ErrCatalogTooLarge
		}
		if op.TargetVendorID == 0 {
			return nil, store.ErrVendorOperationInvalid
		}
		for _, id := range ids {
			if id == op.TargetVendorID {
				return nil, store.ErrVendorOperationInvalid
			}
		}
		return append(ids, op.TargetVendorID), nil
	case store.VendorActionDelete:
		ids := dedupe(op.VendorIDs)
		if len(ids) == 0 {
			return nil, store.ErrCatalogEmptySelection
		}
		if len(ids) > 1000 {
			return nil, store.ErrCatalogTooLarge
		}
		return ids, nil
	default:
		return nil, store.ErrVendorOperationInvalid
	}
}

func (r *fakeCatalogRepo) loadVendorsLocked(ids []int64) ([]store.Vendor, error) {
	out := make([]store.Vendor, 0, len(ids))
	for _, id := range ids {
		rec, ok := r.vendors[id]
		if !ok {
			return nil, store.ErrVendorNotFound
		}
		out = append(out, *rec)
	}
	return out, nil
}

func (r *fakeCatalogRepo) PreviewVendorOperations(_ context.Context, op store.VendorOperation) (store.VendorPreview, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids, err := r.involvedVendors(op)
	if err != nil {
		return store.VendorPreview{}, err
	}
	vendors, err := r.loadVendorsLocked(ids)
	if err != nil {
		return store.VendorPreview{}, err
	}
	preview := store.VendorPreview{ExpectedVersion: fakeVendorFingerprint(vendors)}
	for _, vendor := range vendors {
		preview.Vendors = append(preview.Vendors, store.VendorVersionRef{VendorID: vendor.ID, Version: vendor.Version()})
	}
	if op.ExpectedVersion != "" && op.ExpectedVersion != preview.ExpectedVersion {
		preview.Conflict = true
		for _, vendor := range vendors {
			preview.Conflicts = append(preview.Conflicts,
				store.VendorVersionConflict{VendorID: vendor.ID, CurrentVersion: vendor.Version()})
		}
	}
	switch op.Action {
	case store.VendorActionAssign:
		for _, id := range op.ModelIDs {
			if _, ok := r.models[id]; ok {
				preview.Affected++
			}
		}
	case store.VendorActionMerge:
		for _, model := range r.models {
			if model.VendorID == nil {
				continue
			}
			for _, id := range op.VendorIDs {
				if *model.VendorID == id {
					preview.Affected++
					break
				}
			}
		}
	case store.VendorActionDelete:
		preview.Affected = int64(len(vendors))
	}
	return preview, nil
}

func (r *fakeCatalogRepo) ApplyVendorOperations(_ context.Context, op store.VendorOperation) (store.VendorOperationResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applyCalls++
	ids, err := r.involvedVendors(op)
	if err != nil {
		return store.VendorOperationResult{}, err
	}
	vendors, err := r.loadVendorsLocked(ids)
	if err != nil {
		return store.VendorOperationResult{}, err
	}
	if op.ExpectedVersion == "" || op.ExpectedVersion != fakeVendorFingerprint(vendors) {
		conflicts := make([]store.VendorVersionConflict, 0, len(vendors))
		for _, vendor := range vendors {
			conflicts = append(conflicts, store.VendorVersionConflict{VendorID: vendor.ID, CurrentVersion: vendor.Version()})
		}
		return store.VendorOperationResult{}, store.ErrVendorVersionConflict{Conflicts: conflicts}
	}
	var affected int64
	switch op.Action {
	case store.VendorActionAssign:
		for _, id := range dedupeInt64(op.ModelIDs) {
			model, ok := r.models[id]
			if !ok {
				return store.VendorOperationResult{}, store.ErrModelMetaNotFound
			}
			if op.TargetVendorID == 0 {
				model.VendorID = nil
			} else {
				target := op.TargetVendorID
				model.VendorID = &target
			}
			model.UpdatedAt = r.tick()
			affected++
		}
	case store.VendorActionMerge:
		for _, model := range r.models {
			if model.VendorID == nil {
				continue
			}
			for _, source := range op.VendorIDs {
				if *model.VendorID == source {
					target := op.TargetVendorID
					model.VendorID = &target
					model.UpdatedAt = r.tick()
					affected++
					break
				}
			}
		}
		for _, source := range dedupeInt64(op.VendorIDs) {
			delete(r.vendors, source)
		}
	case store.VendorActionDelete:
		for _, id := range dedupeInt64(op.VendorIDs) {
			if refs := r.modelCountLocked(id); refs > 0 {
				return store.VendorOperationResult{}, store.ErrVendorReferenced{ReferenceCount: refs}
			}
		}
		for _, id := range dedupeInt64(op.VendorIDs) {
			delete(r.vendors, id)
			affected++
		}
	}
	return store.VendorOperationResult{Affected: affected}, nil
}

// ---- model_meta ----

func normalizeJSON(raw []byte, fallback string) []byte {
	if len(raw) == 0 {
		return []byte(fallback)
	}
	return raw
}

func (r *fakeCatalogRepo) CreateModelMeta(_ context.Context, in store.ModelMetaInput) (store.ModelMeta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := strings.TrimSpace(in.ModelName)
	if name == "" {
		return store.ModelMeta{}, store.ErrModelMetaInvalid{Field: "model_name", Reason: "must not be blank"}
	}
	for _, model := range r.models {
		if model.ModelName == name {
			return store.ModelMeta{}, store.ErrModelMetaDuplicate
		}
	}
	if in.VendorID != nil {
		if _, ok := r.vendors[*in.VendorID]; !ok {
			return store.ModelMeta{}, store.ErrVendorNotFound
		}
	}
	candidate := store.ModelMeta{
		ModelName: name, DisplayName: in.DisplayName, Description: in.Description, Icon: in.Icon,
		Tags: normalizeJSON(in.Tags, "[]"), Endpoints: normalizeJSON(in.Endpoints, "{}"),
		Capabilities: normalizeJSON(in.Capabilities, "[]"), Modalities: normalizeJSON(in.Modalities, "[]"),
		Parameters:    normalizeJSON(in.Parameters, "[]"),
		ContextLength: in.ContextLength, MaxOutput: in.MaxOutput,
		KnowledgeCutoff: in.KnowledgeCutoff,
	}
	if err := store.ValidateModelMetaInput(&candidate); err != nil {
		return store.ModelMeta{}, err
	}
	status := int16(1)
	if in.Status != nil {
		status = *in.Status
	}
	syncOfficial := true
	if in.SyncOfficial != nil {
		syncOfficial = *in.SyncOfficial
	}
	now := r.tick()
	r.nextModelID++
	candidate.ID = r.nextModelID
	candidate.VendorID = in.VendorID
	candidate.ReleasedAt = in.ReleasedAt
	candidate.Status = status
	candidate.SyncOfficial = syncOfficial
	candidate.NameRule = in.NameRule
	candidate.CreatedAt, candidate.UpdatedAt = now, now
	r.models[candidate.ID] = &candidate
	return candidate, nil
}

func (r *fakeCatalogRepo) GetModelMeta(_ context.Context, id int64) (store.ModelMeta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.models[id]
	if !ok {
		return store.ModelMeta{}, store.ErrModelMetaNotFound
	}
	return cloneModel(*rec), nil
}

func (r *fakeCatalogRepo) UpdateModelMeta(_ context.Context, id int64, patch store.ModelMetaPatch, expectUpdatedAt time.Time) (store.ModelMeta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.models[id]
	if !ok {
		return store.ModelMeta{}, store.ErrModelMetaNotFound
	}
	if !rec.UpdatedAt.Equal(expectUpdatedAt) {
		return store.ModelMeta{}, store.ErrModelMetaConflict
	}
	updated := cloneModel(*rec)
	if patch.ModelName != nil {
		name := strings.TrimSpace(*patch.ModelName)
		if name == "" {
			return store.ModelMeta{}, store.ErrModelMetaInvalid{Field: "model_name", Reason: "must not be blank"}
		}
		for otherID, model := range r.models {
			if otherID != id && model.ModelName == name {
				return store.ModelMeta{}, store.ErrModelMetaDuplicate
			}
		}
		updated.ModelName = name
	}
	if patch.DisplayNameSet {
		updated.DisplayName = patch.DisplayName
	}
	if patch.DescriptionSet {
		updated.Description = patch.Description
	}
	if patch.IconSet {
		updated.Icon = patch.Icon
	}
	if patch.TagsSet {
		updated.Tags = normalizeJSON(patch.Tags, "[]")
	}
	if patch.VendorIDSet {
		if patch.VendorID != nil {
			if _, ok := r.vendors[*patch.VendorID]; !ok {
				return store.ModelMeta{}, store.ErrVendorNotFound
			}
		}
		updated.VendorID = patch.VendorID
	}
	if patch.EndpointsSet {
		updated.Endpoints = normalizeJSON(patch.Endpoints, "{}")
	}
	if patch.CapabilitiesSet {
		updated.Capabilities = normalizeJSON(patch.Capabilities, "[]")
	}
	if patch.ModalitiesSet {
		updated.Modalities = normalizeJSON(patch.Modalities, "[]")
	}
	if patch.ParametersSet {
		updated.Parameters = normalizeJSON(patch.Parameters, "[]")
	}
	if patch.ContextLengthSet {
		updated.ContextLength = patch.ContextLength
	}
	if patch.MaxOutputSet {
		updated.MaxOutput = patch.MaxOutput
	}
	if patch.KnowledgeCutoffSet {
		updated.KnowledgeCutoff = patch.KnowledgeCutoff
	}
	if patch.ReleasedAtSet {
		updated.ReleasedAt = patch.ReleasedAt
	}
	if patch.Status != nil {
		updated.Status = *patch.Status
	}
	if patch.SyncOfficial != nil {
		updated.SyncOfficial = *patch.SyncOfficial
	}
	if patch.NameRule != nil {
		updated.NameRule = *patch.NameRule
	}
	if err := store.ValidateModelMetaInput(&updated); err != nil {
		return store.ModelMeta{}, err
	}
	updated.UpdatedAt = r.tick()
	r.models[id] = &updated
	return updated, nil
}

func (r *fakeCatalogRepo) DeleteModelMeta(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.models[id]; !ok {
		return store.ErrModelMetaNotFound
	}
	delete(r.models, id)
	return nil
}

func (r *fakeCatalogRepo) DeleteModelMetaVersioned(_ context.Context, id int64, expectUpdatedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.models[id]
	if !ok {
		return store.ErrModelMetaNotFound
	}
	if !rec.UpdatedAt.Equal(expectUpdatedAt) {
		return store.ErrModelMetaConflict
	}
	delete(r.models, id)
	return nil
}

func (r *fakeCatalogRepo) ListModelMeta(_ context.Context, filter store.ModelMetaFilter) ([]store.ModelMeta, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]store.ModelMeta, 0, len(r.models))
	for _, rec := range r.models {
		if filter.Keyword != "" && !strings.Contains(strings.ToLower(rec.ModelName), strings.ToLower(filter.Keyword)) {
			continue
		}
		if filter.Status != nil && rec.Status != *filter.Status {
			continue
		}
		if filter.VendorID != nil && (rec.VendorID == nil || *rec.VendorID != *filter.VendorID) {
			continue
		}
		items = append(items, cloneModel(*rec))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ModelName < items[j].ModelName })
	total := int64(len(items))
	return fakePaginate(items, filter.Page, filter.PageSize), total, nil
}

func (r *fakeCatalogRepo) SearchModelMeta(_ context.Context, keyword string, status *int16, limit int) ([]store.ModelMeta, error) {
	items, _, err := r.ListModelMeta(context.Background(), store.ModelMetaFilter{
		Page: 1, PageSize: limit, Keyword: keyword, Status: status,
	})
	return items, err
}

// ApplyModelSync 在单个锁内模拟一次事务：全部写入先落到临时副本，任何一条
// 失败就丢弃副本（= 回滚），成功才整体替换。
func (r *fakeCatalogRepo) ApplyModelSync(_ context.Context, vendors []store.ModelSyncVendorEntry, models []store.ModelSyncModelEntry) (store.ModelSyncResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := store.ModelSyncResult{
		CreatedVendors: []string{}, UpdatedVendors: []string{},
		CreatedModels: []string{}, UpdatedModels: []string{},
	}
	modelsCopy := make(map[int64]*store.ModelMeta, len(r.models))
	for id, rec := range r.models {
		clone := *rec
		modelsCopy[id] = &clone
	}
	vendorsCopy := make(map[int64]*store.Vendor, len(r.vendors))
	for id, rec := range r.vendors {
		clone := *rec
		vendorsCopy[id] = &clone
	}
	nextModelID, nextVendor, clock := r.nextModelID, r.nextVendor, r.clock
	tick := func() time.Time { clock = clock.Add(time.Second); return clock }

	for _, entry := range vendors {
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			return store.ModelSyncResult{}, store.ErrVendorInvalid
		}
		var current *store.Vendor
		for _, vendor := range vendorsCopy {
			if strings.EqualFold(vendor.Name, name) {
				current = vendor
				break
			}
		}
		if current == nil {
			nextVendor++
			now := tick()
			rec := &store.Vendor{
				ID: nextVendor, Name: name, Description: entry.Description, Icon: entry.Icon,
				Status: 1, CreatedAt: now, UpdatedAt: now,
			}
			vendorsCopy[rec.ID] = rec
			result.CreatedVendors = append(result.CreatedVendors, rec.Name)
			continue
		}
		if entry.Description != nil {
			current.Description = entry.Description
		}
		if entry.Icon != nil {
			current.Icon = entry.Icon
		}
		current.UpdatedAt = tick()
		result.UpdatedVendors = append(result.UpdatedVendors, current.Name)
	}

	for _, entry := range models {
		name := strings.TrimSpace(entry.ModelName)
		if name == "" {
			return store.ModelSyncResult{}, store.ErrModelMetaInvalid{Field: "model_name", Reason: "must not be blank"}
		}
		var currentID int64
		var current *store.ModelMeta
		for id, rec := range modelsCopy {
			if rec.ModelName == name {
				currentID, current = id, rec
				break
			}
		}
		vendorID, vendorErr := fakeResolveVendor(vendorsCopy, entry.VendorName)
		if vendorErr != nil {
			return store.ModelSyncResult{}, vendorErr
		}
		if current == nil {
			candidate := store.ModelMeta{
				ModelName: name, DisplayName: entry.DisplayName, Description: entry.Description,
				Icon: entry.Icon, Tags: normalizeJSON(entry.Tags, "[]"),
				VendorID:      vendorID,
				Endpoints:     normalizeJSON(entry.Endpoints, "{}"),
				Capabilities:  normalizeJSON(entry.Capabilities, "[]"),
				Modalities:    normalizeJSON(entry.Modalities, "[]"),
				Parameters:    normalizeJSON(entry.Parameters, "[]"),
				ContextLength: entry.ContextLength, MaxOutput: entry.MaxOutput,
				KnowledgeCutoff: entry.KnowledgeCutoff, ReleasedAt: entry.ReleasedAt,
			}
			if err := store.ValidateModelMetaInput(&candidate); err != nil {
				return store.ModelSyncResult{}, err
			}
			nextModelID++
			now := tick()
			candidate.ID = nextModelID
			candidate.Status = 1
			candidate.SyncOfficial = true
			candidate.CreatedAt, candidate.UpdatedAt = now, now
			modelsCopy[candidate.ID] = &candidate
			result.CreatedModels = append(result.CreatedModels, candidate.ModelName)
			continue
		}
		if !current.SyncOfficial {
			return store.ModelSyncResult{}, store.ErrSyncBlocked{ModelName: current.ModelName}
		}
		updated := *current
		if entry.DisplayName != nil {
			updated.DisplayName = entry.DisplayName
		}
		if entry.Description != nil {
			updated.Description = entry.Description
		}
		if entry.Icon != nil {
			updated.Icon = entry.Icon
		}
		if entry.Tags != nil {
			updated.Tags = normalizeJSON(entry.Tags, "[]")
		}
		if entry.Capabilities != nil {
			updated.Capabilities = normalizeJSON(entry.Capabilities, "[]")
		}
		if entry.ContextLength != nil {
			updated.ContextLength = entry.ContextLength
		}
		if entry.MaxOutput != nil {
			updated.MaxOutput = entry.MaxOutput
		}
		if entry.VendorName != nil && vendorID != nil {
			updated.VendorID = vendorID
		}
		if entry.Endpoints != nil {
			updated.Endpoints = normalizeJSON(entry.Endpoints, "{}")
		}
		if entry.Modalities != nil {
			updated.Modalities = normalizeJSON(entry.Modalities, "[]")
		}
		if entry.Parameters != nil {
			updated.Parameters = normalizeJSON(entry.Parameters, "[]")
		}
		if entry.KnowledgeCutoff != nil {
			updated.KnowledgeCutoff = entry.KnowledgeCutoff
		}
		if entry.ReleasedAt != nil {
			updated.ReleasedAt = entry.ReleasedAt
		}
		if err := store.ValidateModelMetaInput(&updated); err != nil {
			return store.ModelSyncResult{}, err
		}
		updated.UpdatedAt = tick()
		modelsCopy[currentID] = &updated
		result.UpdatedModels = append(result.UpdatedModels, updated.ModelName)
	}

	r.models, r.vendors = modelsCopy, vendorsCopy
	r.nextModelID, r.nextVendor, r.clock = nextModelID, nextVendor, clock
	return result, nil
}

// fakeResolveVendor 复刻 store.resolveSyncVendorID：名字大小写不敏感，查不到
// 就返回 ErrSyncVendorMissing。假实现必须与真实现同口径，测试才有意义。
func fakeResolveVendor(vendors map[int64]*store.Vendor, name *string) (*int64, error) {
	if name == nil {
		return nil, nil
	}
	key := strings.ToLower(strings.TrimSpace(*name))
	if key == "" {
		return nil, store.ErrSyncVendorMissing{VendorName: *name}
	}
	for _, vendor := range vendors {
		if strings.ToLower(strings.TrimSpace(vendor.Name)) == key {
			id := vendor.ID
			return &id, nil
		}
	}
	return nil, store.ErrSyncVendorMissing{VendorName: *name}
}

func (r *fakeCatalogRepo) MissingModels(_ context.Context) (store.MissingResult, error) {
	return r.missing, nil
}

func (r *fakeCatalogRepo) BatchDeleteModelMeta(_ context.Context, ids []int64, removeFromChannels, clearPricing bool) (store.BatchDeleteResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids = dedupeInt64(ids)
	if len(ids) == 0 {
		return store.BatchDeleteResult{}, store.ErrCatalogEmptySelection
	}
	if len(ids) > 1000 {
		return store.BatchDeleteResult{}, store.ErrCatalogTooLarge
	}
	result := store.BatchDeleteResult{Requested: int64(len(ids))}
	names := make([]string, 0, len(ids))
	locked := make([]int64, 0, len(ids))
	for _, id := range ids {
		rec, ok := r.models[id]
		if !ok {
			continue
		}
		names = append(names, rec.ModelName)
		locked = append(locked, id)
	}
	if len(locked) == 0 {
		return store.BatchDeleteResult{}, store.ErrModelMetaNotFound
	}
	if removeFromChannels {
		for _, name := range names {
			if channelIDs, ok := r.emptiedChannels[name]; ok {
				return store.BatchDeleteResult{}, store.ErrChannelModelsEmpty{ChannelIDs: channelIDs}
			}
		}
		result.ChannelsUpdated = int64(len(names))
	}
	if clearPricing {
		result.PricingDeleted = int64(len(names))
		result.GroupPricingDeleted = int64(len(names))
	}
	for _, id := range locked {
		delete(r.models, id)
	}
	result.Deleted = int64(len(locked))
	return result, nil
}

func (r *fakeCatalogRepo) ResolveMeta(_ context.Context, _ []string) (map[string]store.ModelMeta, error) {
	return map[string]store.ModelMeta{}, nil
}

// ---------------------------------------------------------------------------
// 假派生读取面
// ---------------------------------------------------------------------------

type fakeCatalogDerived struct {
	byName  map[string]ModelDerived
	vendors map[int64]string
}

func (d *fakeCatalogDerived) Derive(_ context.Context, models []store.ModelMeta) (map[int64]ModelDerived, error) {
	out := make(map[int64]ModelDerived, len(models))
	for _, model := range models {
		info, ok := d.byName[model.ModelName]
		if !ok {
			info = ModelDerived{SquareState: squareStateUnavailable, EnableGroups: []string{}}
		}
		out[model.ID] = info
	}
	return out, nil
}

func (d *fakeCatalogDerived) VendorNames(_ context.Context, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	for _, id := range ids {
		if name, ok := d.vendors[id]; ok {
			out[id] = name
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 测试环境
// ---------------------------------------------------------------------------

type catalogTestEnv struct {
	t        *testing.T
	repo     *fakeCatalogRepo
	derived  *fakeCatalogDerived
	handlers *ModelCatalogHandlers
	router   http.Handler
	syncRaw  []byte
	auditLog *captureAuditExecer
}

type captureAuditExecer struct {
	mu   sync.Mutex
	args [][]any
}

func (e *captureAuditExecer) ExecContext(_ context.Context, _ string, args ...any) (sql.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.args = append(e.args, append([]any(nil), args...))
	return fakeSQLResult{}, nil
}
func newCatalogTestEnv(t *testing.T) *catalogTestEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	repo := newFakeCatalogRepo()
	derived := &fakeCatalogDerived{byName: map[string]ModelDerived{}, vendors: map[int64]string{}}
	env := &catalogTestEnv{t: t, repo: repo, derived: derived}
	auditExec := &captureAuditExecer{}
	env.auditLog = auditExec
	handlers, err := NewModelCatalogHandlers(ModelCatalogHandlerConfig{
		Repo: repo, Redis: rdb, Authz: rbac.NewAuthzMiddleware(), Logger: testLogger(),
		Audit: audit.New(auditExec), Derived: derived,
		SyncSource: func(context.Context) ([]byte, error) { return env.syncRaw, nil },
	})
	if err != nil {
		t.Fatalf("NewModelCatalogHandlers: %v", err)
	}
	env.handlers = handlers
	mux := http.NewServeMux()
	handlers.RegisterModelCatalogRoutes(mux, nil)
	env.router = mux
	return env
}

func (e *catalogTestEnv) request(p *auth.Principal, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	}
	return serve(e.router, req)
}

func catalogFullAdmin() *auth.Principal {
	return &auth.Principal{UserID: 77, Role: rbac.RoleAdmin}
}

func catalogAdmin(perms ...string) *auth.Principal {
	if perms == nil {
		// nil 在 RBAC 语义里是 DB NULL = FULL；「无权限」必须是空而非 nil。
		perms = []string{}
	}
	return &auth.Principal{UserID: 77, Role: rbac.RoleAdmin, Permissions: perms}
}

func decodeCatalogProblem(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("problem JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	return body
}

func decodeModelView(t *testing.T, rr *httptest.ResponseRecorder) modelMetaView {
	t.Helper()
	var view modelMetaView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("model view JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	return view
}

func decodeVendorView(t *testing.T, rr *httptest.ResponseRecorder) vendorView {
	t.Helper()
	var view vendorView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("vendor view JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	return view
}

func createTestModel(t *testing.T, env *catalogTestEnv, name string) modelMetaView {
	t.Helper()
	rr := env.request(catalogFullAdmin(), http.MethodPost, "/api/models",
		map[string]any{"model_name": name, "description": "d"}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create model %s = %d（body=%q）", name, rr.Code, rr.Body.String())
	}
	return decodeModelView(t, rr)
}

func createTestVendor(t *testing.T, env *catalogTestEnv, name string) vendorView {
	t.Helper()
	rr := env.request(catalogFullAdmin(), http.MethodPost, "/api/vendors", map[string]any{"name": name}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create vendor %s = %d（body=%q）", name, rr.Code, rr.Body.String())
	}
	return decodeVendorView(t, rr)
}

// ---------------------------------------------------------------------------
// 权限矩阵
// ---------------------------------------------------------------------------

func TestModelCatalogPermissionMatrix(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		perm   string
		body   any
	}{
		{"list models", http.MethodGet, "/api/models", rbac.PermChannelsRead, nil},
		{"create model", http.MethodPost, "/api/models", rbac.PermChannelsManage, map[string]any{"model_name": "m"}},
		{"get model", http.MethodGet, "/api/models/1", rbac.PermChannelsRead, nil},
		{"update model", http.MethodPut, "/api/models/1", rbac.PermChannelsManage, map[string]any{"updated_at": "2026-10-20T08:00:00Z"}},
		{"delete model", http.MethodDelete, "/api/models/1", rbac.PermChannelsManage, nil},
		{"search models", http.MethodGet, "/api/models/search", rbac.PermChannelsRead, nil},
		{"batch delete", http.MethodPost, "/api/models/batch-delete", rbac.PermChannelsManageSensitive, map[string]any{"ids": []int64{1}}},
		{"missing", http.MethodGet, "/api/models/missing", rbac.PermChannelsRead, nil},
		{"sync preview", http.MethodGet, "/api/models/sync/preview", rbac.PermChannelsManageSensitive, nil},
		{"sync apply", http.MethodPost, "/api/models/sync", rbac.PermChannelsManageSensitive, map[string]any{"source_version": "x"}},
		{"list vendors", http.MethodGet, "/api/vendors", rbac.PermChannelsRead, nil},
		{"create vendor", http.MethodPost, "/api/vendors", rbac.PermChannelsManage, map[string]any{"name": "v"}},
		{"get vendor", http.MethodGet, "/api/vendors/1", rbac.PermChannelsRead, nil},
		{"update vendor", http.MethodPut, "/api/vendors/1", rbac.PermChannelsManage, map[string]any{"version": "x"}},
		{"delete vendor", http.MethodDelete, "/api/vendors/1", rbac.PermChannelsManage, nil},
		{"search vendors", http.MethodGet, "/api/vendors/search", rbac.PermChannelsRead, nil},
		{"preview ops", http.MethodPost, "/api/vendors/operations/preview", rbac.PermChannelsManageSensitive, map[string]any{"action": "assign", "model_ids": []int64{1}}},
		{"apply ops", http.MethodPost, "/api/vendors/operations", rbac.PermChannelsManageSensitive, map[string]any{"action": "assign", "model_ids": []int64{1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newCatalogTestEnv(t)
			rr := env.request(catalogAdmin(), tc.method, tc.path, tc.body, nil)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("缺权限 %s = %d，期望 403（body=%q）", tc.perm, rr.Code, rr.Body.String())
			}
			if got := decodeCatalogProblem(t, rr)["status"]; got != float64(http.StatusForbidden) {
				t.Fatalf("problem.status = %v，期望 403", got)
			}
			// 反向：持有该权限时不得因授权失败而 403。
			rr = env.request(catalogAdmin(tc.perm), tc.method, tc.path, tc.body, nil)
			if rr.Code == http.StatusForbidden {
				t.Fatalf("持有 %s 仍 403：%s", tc.perm, rr.Body.String())
			}
		})
	}
}

func TestModelCatalogBatchDeleteClearPricingRequiresPricingManage(t *testing.T) {
	env := newCatalogTestEnv(t)
	view := createTestModel(t, env, "priced-model")

	rr := env.request(catalogAdmin(rbac.PermChannelsManageSensitive), http.MethodPost,
		"/api/models/batch-delete", map[string]any{"ids": []int64{view.ID}, "clear_pricing": true}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("clear_pricing 无 pricing:manage = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
	rr = env.request(catalogAdmin(rbac.PermChannelsManageSensitive, rbac.PermPricingManage), http.MethodPost,
		"/api/models/batch-delete", map[string]any{"ids": []int64{view.ID}, "clear_pricing": true}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("clear_pricing 有 pricing:manage = %d（body=%q）", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 模型 CRUD / 派生字段
// ---------------------------------------------------------------------------

func TestModelCatalogModelCRUDAndDerived(t *testing.T) {
	env := newCatalogTestEnv(t)
	vendor := createTestVendor(t, env, "Acme")
	env.derived.vendors[vendor.ID] = "Acme"
	env.derived.byName["gpt-4o"] = ModelDerived{
		SquareState: squareStateVisible, ConfiguredChannelCount: 3,
		EnableGroups: []string{"default", "vip"},
	}
	env.derived.byName["off-model"] = ModelDerived{SquareState: squareStateUnavailable, EnableGroups: []string{}}

	rr := env.request(catalogFullAdmin(), http.MethodPost, "/api/models", map[string]any{
		"model_name": "gpt-4o", "display_name": "GPT-4o", "description": "desc",
		"tags": []string{"vision"}, "vendor_id": vendor.ID, "name_rule": 0,
		"capabilities": []string{"vision"}, "context_length": 128000,
		"endpoints": map[string]any{"/v1/chat/completions": []string{"POST"}},
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view := decodeModelView(t, rr)
	if view.ModelName != "gpt-4o" || view.VendorName == nil || *view.VendorName != "Acme" {
		t.Fatalf("view = %+v", view)
	}
	if view.SquareState != squareStateVisible || view.ConfiguredChannelCount != 3 ||
		len(view.EnableGroups) != 2 {
		t.Fatalf("派生字段 = %+v", view)
	}
	if string(view.Tags) != `["vision"]` || string(view.Endpoints) != `{"/v1/chat/completions":["POST"]}` {
		t.Fatalf("jsonb 回显 = %s / %s", view.Tags, view.Endpoints)
	}

	// 列表 + 排序 + square_state 筛选。
	createTestModel(t, env, "off-model")
	rr = env.request(catalogFullAdmin(), http.MethodGet, "/api/models?p=1&page_size=10&sort=-model_name", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var list modelListView
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("list JSON: %v", err)
	}
	if list.Total != 2 || list.Items[0].ModelName != "off-model" {
		t.Fatalf("list = %+v", list)
	}
	rr = env.request(catalogFullAdmin(), http.MethodGet, "/api/models?square_state=visible", nil, nil)
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("list JSON: %v", err)
	}
	if list.Total != 1 || list.Items[0].ModelName != "gpt-4o" {
		t.Fatalf("square_state 筛选 = %+v", list)
	}

	// 详情。
	rr = env.request(catalogFullAdmin(), http.MethodGet, fmt.Sprintf("/api/models/%d", view.ID), nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get = %d", rr.Code)
	}

	// 更新：缺 updated_at → 400。
	rr = env.request(catalogFullAdmin(), http.MethodPut, fmt.Sprintf("/api/models/%d", view.ID),
		map[string]any{"description": "new"}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 updated_at = %d，期望 400", rr.Code)
	}
	// 过期 updated_at → 409。
	rr = env.request(catalogFullAdmin(), http.MethodPut, fmt.Sprintf("/api/models/%d", view.ID),
		map[string]any{"description": "new", "updated_at": "2020-01-01T00:00:00Z"}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("过期 updated_at = %d，期望 409", rr.Code)
	}
	rr = env.request(catalogFullAdmin(), http.MethodPut, fmt.Sprintf("/api/models/%d", view.ID),
		map[string]any{"description": "new", "updated_at": view.UpdatedAt.Format(time.RFC3339Nano)}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("update = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view = decodeModelView(t, rr)
	if view.Description == nil || *view.Description != "new" {
		t.Fatalf("更新后 = %+v", view)
	}

	// 删除：缺 updated_at → 400；过期 → 409；正确 → 204。
	rr = env.request(catalogFullAdmin(), http.MethodDelete, fmt.Sprintf("/api/models/%d", view.ID), nil, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("删除缺 updated_at = %d，期望 400", rr.Code)
	}
	rr = env.request(catalogFullAdmin(), http.MethodDelete,
		fmt.Sprintf("/api/models/%d?updated_at=%s", view.ID, url.QueryEscape("2020-01-01T00:00:00Z")), nil, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("删除过期 updated_at = %d，期望 409", rr.Code)
	}
	rr = env.request(catalogFullAdmin(), http.MethodDelete,
		fmt.Sprintf("/api/models/%d?updated_at=%s", view.ID, url.QueryEscape(view.UpdatedAt.Format(time.RFC3339Nano))), nil, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("删除 = %d（body=%q）", rr.Code, rr.Body.String())
	}
	rr = env.request(catalogFullAdmin(), http.MethodGet, fmt.Sprintf("/api/models/%d", view.ID), nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("删除后 get = %d，期望 404", rr.Code)
	}
}

func TestModelCatalogModelValidation(t *testing.T) {
	env := newCatalogTestEnv(t)
	createTestModel(t, env, "dup")
	rr := env.request(catalogFullAdmin(), http.MethodPost, "/api/models", map[string]any{"model_name": "dup"}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("重名 = %d，期望 409", rr.Code)
	}
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models", map[string]any{"model_name": "bad name"}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("非法模型名 = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models", map[string]any{
		"model_name": "with-vendor", "vendor_id": 999,
	}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("vendor_id 不存在 = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	if problem := decodeCatalogProblem(t, rr); problem["type"] != problemTypeValidationFailed {
		t.Fatalf("problem.type = %v", problem["type"])
	}
	// endpoints 键不以 / 开头 → store 校验 → 422。
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models", map[string]any{
		"model_name": "bad-endpoints", "endpoints": map[string]any{"v1/chat": []string{"POST"}},
	}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("endpoints 非法键 = %d，期望 422", rr.Code)
	}
	// 搜索：limit 超上限 → 400。
	rr = env.request(catalogFullAdmin(), http.MethodGet, "/api/models/search?limit=101", nil, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("limit=101 = %d，期望 400", rr.Code)
	}
	// status 在内存过滤（合法枚举必须真实生效）。
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models", map[string]any{
		"model_name": "disabled-search", "status": 2,
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create disabled = %d", rr.Code)
	}
	rr = env.request(catalogFullAdmin(), http.MethodGet, "/api/models/search?status=2", nil, nil)
	var search modelSearchListView
	if err := json.Unmarshal(rr.Body.Bytes(), &search); err != nil {
		t.Fatalf("search JSON: %v", err)
	}
	if len(search.Items) != 1 || search.Items[0].ModelName != "disabled-search" {
		t.Fatalf("search status=2 = %+v", search.Items)
	}
	rr = env.request(catalogFullAdmin(), http.MethodGet, "/api/models?status=bogus", nil, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("非法 status = %d，期望 400", rr.Code)
	}
	rr = env.request(catalogFullAdmin(), http.MethodGet, "/api/models?square_state=bogus", nil, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("非法 square_state = %d，期望 400", rr.Code)
	}
}

func TestModelCatalogBatchDeleteBoundaries(t *testing.T) {
	env := newCatalogTestEnv(t)
	view := createTestModel(t, env, "b")

	rr := env.request(catalogFullAdmin(), http.MethodPost, "/api/models/batch-delete",
		map[string]any{"ids": []int64{}}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("空选择 = %d，期望 422", rr.Code)
	}
	tooLarge := make([]int64, 1001)
	for i := range tooLarge {
		tooLarge[i] = int64(i + 1)
	}
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models/batch-delete",
		map[string]any{"ids": tooLarge}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("1001 条 = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	env.repo.emptiedChannels["b"] = []int64{9, 10}
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models/batch-delete",
		map[string]any{"ids": []int64{view.ID}, "remove_from_channels": true}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("渠道变空 = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	problem := decodeCatalogProblem(t, rr)
	if problem["type"] != catalogProblemChannelModelsEmpty {
		t.Fatalf("problem.type = %v", problem["type"])
	}
	if ids, ok := problem["channel_ids"].([]any); !ok || len(ids) != 2 {
		t.Fatalf("channel_ids = %v", problem["channel_ids"])
	}
	// 正常批量删除。
	delete(env.repo.emptiedChannels, "b")
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models/batch-delete",
		map[string]any{"ids": []int64{view.ID}, "remove_from_channels": true}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("批量删除 = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var result batchDeleteResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &result)
	if result.Deleted != 1 || result.Requested != 1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestModelCatalogMissing(t *testing.T) {
	env := newCatalogTestEnv(t)
	env.repo.missing = store.MissingResult{
		Items: []store.MissingModel{
			{ModelName: "x", ChannelCount: 2, HasPricing: true},
		},
		SkippedChannels: 1,
	}
	rr := env.request(catalogFullAdmin(), http.MethodGet, "/api/models/missing", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("missing = %d", rr.Code)
	}
	var list missingListView
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("missing JSON: %v", err)
	}
	if list.Total != 1 || list.SkippedChannels != 1 || list.Items[0].ChannelCount != 2 || !list.Items[0].HasPricing {
		t.Fatalf("missing = %+v", list)
	}
}

// ---------------------------------------------------------------------------
// 供应商 CRUD / 批量操作
// ---------------------------------------------------------------------------

func TestModelCatalogVendorCRUDAndConflicts(t *testing.T) {
	env := newCatalogTestEnv(t)
	vendor := createTestVendor(t, env, "Acme")

	// 大小写重名 → 409。
	rr := env.request(catalogFullAdmin(), http.MethodPost, "/api/vendors", map[string]any{"name": "ACME"}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("大小写重名 = %d，期望 409", rr.Code)
	}
	// 缺 version → 400；错误 version → 409。
	rr = env.request(catalogFullAdmin(), http.MethodPut, fmt.Sprintf("/api/vendors/%d", vendor.ID),
		map[string]any{"name": "Acme2"}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 version = %d，期望 400", rr.Code)
	}
	rr = env.request(catalogFullAdmin(), http.MethodPut, fmt.Sprintf("/api/vendors/%d", vendor.ID),
		map[string]any{"name": "Acme2", "version": "stale"}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("错误 version = %d，期望 409", rr.Code)
	}
	rr = env.request(catalogFullAdmin(), http.MethodPut, fmt.Sprintf("/api/vendors/%d", vendor.ID),
		map[string]any{"name": "Acme2", "version": vendor.Version}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("update = %d（body=%q）", rr.Code, rr.Body.String())
	}
	updated := decodeVendorView(t, rr)
	if updated.Name != "Acme2" || updated.Version == vendor.Version {
		t.Fatalf("updated = %+v", updated)
	}

	// 引用保护：挂一个模型后删除 → 409 + reference_count。
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models", map[string]any{
		"model_name": "ref-model", "vendor_id": updated.ID,
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create model = %d", rr.Code)
	}
	rr = env.request(catalogFullAdmin(), http.MethodDelete, fmt.Sprintf("/api/vendors/%d", updated.ID), nil, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("引用中删除 = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}
	problem := decodeCatalogProblem(t, rr)
	if problem["type"] != catalogProblemVendorReferenced || problem["reference_count"] != float64(1) {
		t.Fatalf("problem = %+v", problem)
	}

	// 列表 attached 分区。
	rr = env.request(catalogFullAdmin(), http.MethodGet, "/api/vendors?attached=true", nil, nil)
	var list vendorListView
	_ = json.Unmarshal(rr.Body.Bytes(), &list)
	if list.Total != 1 || list.Items[0].ModelCount != 1 {
		t.Fatalf("attached=true = %+v", list)
	}
	rr = env.request(catalogFullAdmin(), http.MethodGet, "/api/vendors?attached=maybe", nil, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("非法 attached = %d，期望 400", rr.Code)
	}
}

func TestModelCatalogVendorOperations(t *testing.T) {
	env := newCatalogTestEnv(t)
	source := createTestVendor(t, env, "Source")
	target := createTestVendor(t, env, "Target")
	rr := env.request(catalogFullAdmin(), http.MethodPost, "/api/models", map[string]any{
		"model_name": "m1", "vendor_id": source.ID,
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create model = %d", rr.Code)
	}
	model := decodeModelView(t, rr)

	// preview 返回指纹。
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/vendors/operations/preview",
		map[string]any{"action": "merge", "vendor_ids": []int64{source.ID}, "target_vendor_id": target.ID}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var preview vendorPreviewResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &preview)
	if preview.ExpectedVersion == "" || preview.Affected != 1 {
		t.Fatalf("preview = %+v", preview)
	}

	// 缺 expected_version → 409 vendor_conflict。
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/vendors/operations",
		map[string]any{"action": "merge", "vendor_ids": []int64{source.ID}, "target_vendor_id": target.ID}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("缺版本 = %d，期望 409", rr.Code)
	}
	if problem := decodeCatalogProblem(t, rr); problem["type"] != catalogProblemVendorConflict {
		t.Fatalf("problem = %+v", problem)
	}
	// 过期指纹 → 409 + conflicts[]。
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/vendors/operations",
		map[string]any{"action": "merge", "vendor_ids": []int64{source.ID}, "target_vendor_id": target.ID,
			"expected_version": "stale"}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("过期版本 = %d，期望 409", rr.Code)
	}
	if conflicts, ok := decodeCatalogProblem(t, rr)["conflicts"].([]any); !ok || len(conflicts) != 2 {
		t.Fatalf("conflicts = %v", decodeCatalogProblem(t, rr)["conflicts"])
	}
	// 正确指纹 → merge；源供应商消失、模型改挂目标。
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/vendors/operations",
		map[string]any{"action": "merge", "vendor_ids": []int64{source.ID}, "target_vendor_id": target.ID,
			"expected_version": preview.ExpectedVersion}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("merge = %d（body=%q）", rr.Code, rr.Body.String())
	}
	rr = env.request(catalogFullAdmin(), http.MethodGet, fmt.Sprintf("/api/vendors/%d", source.ID), nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("merge 后源供应商 = %d，期望 404", rr.Code)
	}
	got, _ := env.repo.GetModelMeta(context.Background(), model.ID)
	if got.VendorID == nil || *got.VendorID != target.ID {
		t.Fatalf("merge 后模型归属 = %+v", got.VendorID)
	}

	// 非法参数组合 → 422。
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/vendors/operations",
		map[string]any{"action": "merge", "vendor_ids": []int64{}, "target_vendor_id": target.ID,
			"expected_version": preview.ExpectedVersion}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("空选择 = %d，期望 422", rr.Code)
	}
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/vendors/operations/preview",
		map[string]any{"action": "explode"}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("非法 action = %d，期望 422", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// 上游同步
// ---------------------------------------------------------------------------

func syncManifestJSON(t *testing.T, extraMap map[string]any) []byte {
	t.Helper()
	manifest := map[string]any{
		"version": "upstream-1",
		"models": []any{
			map[string]any{"model_name": "new-model", "display_name": "New", "capabilities": []string{"text"}},
			map[string]any{"model_name": "changed-model", "description": "upstream desc"},
			map[string]any{"model_name": "locked-model", "description": "should not overwrite"},
			map[string]any{"model_name": "same-model", "description": "same"},
		},
		"vendors": []any{
			map[string]any{"name": "UpVendor", "description": "v"},
		},
	}
	for key, value := range extraMap {
		manifest[key] = value
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return raw
}

func TestModelCatalogSyncPreviewAndApply(t *testing.T) {
	env := newCatalogTestEnv(t)
	// 本地：changed-model 与上游不同、locked-model 锁定、same-model 相同、
	// orphan-model 上游已消失。
	createTestModel(t, env, "changed-model")
	rr := env.request(catalogFullAdmin(), http.MethodPost, "/api/models", map[string]any{
		"model_name": "locked-model", "sync_official": false, "description": "should not overwrite",
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create locked = %d", rr.Code)
	}
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models", map[string]any{
		"model_name": "same-model", "description": "same",
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create same = %d", rr.Code)
	}
	createTestModel(t, env, "orphan-model")

	env.syncRaw = syncManifestJSON(t, nil)
	rr = env.request(catalogFullAdmin(), http.MethodGet, "/api/models/sync/preview", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var preview syncPreviewResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &preview); err != nil {
		t.Fatalf("preview JSON: %v", err)
	}
	sum := sha256.Sum256(env.syncRaw)
	if preview.SourceVersion != hex.EncodeToString(sum[:]) {
		t.Fatalf("source_version = %q", preview.SourceVersion)
	}
	if len(preview.Groups.Create) != 1 || preview.Groups.Create[0].ModelName != "new-model" {
		t.Fatalf("create = %+v", preview.Groups.Create)
	}
	if len(preview.Groups.Update) != 1 || preview.Groups.Update[0].ModelName != "changed-model" {
		t.Fatalf("update = %+v", preview.Groups.Update)
	}
	if len(preview.Groups.Blocked) != 1 || preview.Groups.Blocked[0] != "locked-model" {
		t.Fatalf("blocked = %+v", preview.Groups.Blocked)
	}
	if len(preview.Groups.MissingUpstream) != 1 || preview.Groups.MissingUpstream[0] != "orphan-model" {
		t.Fatalf("missing_upstream = %+v", preview.Groups.MissingUpstream)
	}
	if len(preview.Groups.Unchanged) != 1 || preview.Groups.Unchanged[0] != "same-model" {
		t.Fatalf("unchanged = %+v", preview.Groups.Unchanged)
	}
	if len(preview.Groups.Vendors.Create) != 1 || preview.Groups.Vendors.Create[0] != "UpVendor" {
		t.Fatalf("vendor create = %+v", preview.Groups.Vendors)
	}

	// apply：缺版本 → 409。
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models/sync", map[string]any{
		"models": []any{map[string]any{"model_name": "new-model"}},
	}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("缺 source_version = %d，期望 409", rr.Code)
	}
	// 过期版本 → 409。
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models/sync", map[string]any{
		"source_version": "deadbeef", "models": []any{map[string]any{"model_name": "new-model"}},
	}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("过期 source_version = %d，期望 409", rr.Code)
	}
	// 条目不在清单 → 409。
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models/sync", map[string]any{
		"source_version": preview.SourceVersion, "models": []any{map[string]any{"model_name": "ghost"}},
	}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("条目消失 = %d，期望 409", rr.Code)
	}
	// 勾选 blocked → 400。
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models/sync", map[string]any{
		"source_version": preview.SourceVersion, "models": []any{map[string]any{"model_name": "locked-model"}},
	}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("勾选 blocked = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	// 正常 apply：只写勾选项。
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models/sync", map[string]any{
		"source_version": preview.SourceVersion,
		"models": []any{
			map[string]any{"model_name": "new-model"},
			map[string]any{"model_name": "changed-model"},
		},
		"vendors": []any{map[string]any{"name": "UpVendor"}},
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("apply = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var applied syncApplyResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &applied)
	if len(applied.CreatedModels) != 1 || applied.CreatedModels[0] != "new-model" {
		t.Fatalf("created = %+v", applied)
	}
	if len(applied.UpdatedModels) != 1 || applied.UpdatedModels[0] != "changed-model" {
		t.Fatalf("updated = %+v", applied)
	}
	if len(applied.CreatedVendors) != 1 || applied.CreatedVendors[0] != "UpVendor" {
		t.Fatalf("created vendors = %+v", applied)
	}
	// locked-model 的值未被覆盖；orphan-model 未被动。
	models, _, _ := env.repo.ListModelMeta(context.Background(), store.ModelMetaFilter{Page: 1, PageSize: 100})
	byName := map[string]store.ModelMeta{}
	for _, model := range models {
		byName[model.ModelName] = model
	}
	if byName["locked-model"].Description == nil || *byName["locked-model"].Description != "should not overwrite" {
		t.Fatalf("blocked 被覆盖：%+v", byName["locked-model"].Description)
	}
	if _, ok := byName["orphan-model"]; !ok {
		t.Fatalf("未勾选的本地模型被删除")
	}
	if byName["changed-model"].Description == nil || *byName["changed-model"].Description != "upstream desc" {
		t.Fatalf("changed-model 未更新：%+v", byName["changed-model"].Description)
	}
}

func TestModelCatalogSyncSourceUnavailable(t *testing.T) {
	env := newCatalogTestEnv(t)
	env.handlers.syncSource = func(context.Context) ([]byte, error) { return nil, ErrSyncSourceNotConfigured }
	rr := env.request(catalogFullAdmin(), http.MethodGet, "/api/models/sync/preview", nil, nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("未配置源 = %d，期望 503", rr.Code)
	}
	env.handlers.syncSource = func(context.Context) ([]byte, error) { return nil, ErrSyncSourceUnavailable }
	rr = env.request(catalogFullAdmin(), http.MethodGet, "/api/models/sync/preview", nil, nil)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("源不可用 = %d，期望 502", rr.Code)
	}
	env.handlers.syncSource = func(context.Context) ([]byte, error) { return []byte(`{"models":"bad"}`), nil }
	rr = env.request(catalogFullAdmin(), http.MethodGet, "/api/models/sync/preview", nil, nil)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("清单非法 = %d，期望 502", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// 幂等 / 审计
// ---------------------------------------------------------------------------

func TestModelCatalogCreateIdempotency(t *testing.T) {
	env := newCatalogTestEnv(t)
	headers := map[string]string{"Idempotency-Key": "k-1"}
	body := map[string]any{"model_name": "idem-model"}
	rr := env.request(catalogFullAdmin(), http.MethodPost, "/api/models", body, headers)
	if rr.Code != http.StatusCreated {
		t.Fatalf("首次 = %d（body=%q）", rr.Code, rr.Body.String())
	}
	first := rr.Body.String()
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models", body, headers)
	if rr.Code != http.StatusCreated || rr.Body.String() != first {
		t.Fatalf("重放 = %d body=%q，期望与首次一致", rr.Code, rr.Body.String())
	}
	// 同 key 不同 body → 422 idempotency_key_reuse。
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models",
		map[string]any{"model_name": "other-model"}, headers)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("key 复用不同 body = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	// vendors 幂等独立 scope。
	vendors := map[string]string{"Idempotency-Key": "k-1"}
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/vendors", map[string]any{"name": "V"}, vendors)
	if rr.Code != http.StatusCreated {
		t.Fatalf("vendor 首次 = %d", rr.Code)
	}
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/vendors", map[string]any{"name": "V"}, vendors)
	if rr.Code != http.StatusCreated {
		t.Fatalf("vendor 重放 = %d", rr.Code)
	}
}

func TestModelCatalogAuditHasNoSensitiveContent(t *testing.T) {
	env := newCatalogTestEnv(t)
	rr := env.request(catalogFullAdmin(), http.MethodPost, "/api/models", map[string]any{
		"model_name": "audited", "description": "super-secret-description",
		"icon": "secret-icon-key",
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d", rr.Code)
	}
	view := decodeModelView(t, rr)
	rr = env.request(catalogFullAdmin(), http.MethodDelete,
		fmt.Sprintf("/api/models/%d?updated_at=%s", view.ID, url.QueryEscape(view.UpdatedAt.Format(time.RFC3339Nano))), nil, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rr.Code)
	}
	env.auditLog.mu.Lock()
	entries := append([][]any(nil), env.auditLog.args...)
	env.auditLog.mu.Unlock()
	if len(entries) != 2 {
		t.Fatalf("审计条数 = %d，期望 2", len(entries))
	}
	for _, args := range entries {
		if len(args) < 10 {
			t.Fatalf("审计参数不足：%+v", args)
		}
		detail, _ := args[9].([]byte)
		text := string(detail)
		for _, forbidden := range []string{"super-secret-description", "secret-icon-key", "endpoints"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("审计 detail 含敏感/整行内容 %q：%s", forbidden, text)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// name_rule 匹配（与 store 语义对齐）
// ---------------------------------------------------------------------------

func TestModelCatalogDerivedNameRuleMatch(t *testing.T) {
	cases := []struct {
		name     string
		models   []string
		pattern  string
		rule     int16
		expected bool
	}{
		{"exact hit", []string{"gpt-4o"}, "gpt-4o", 0, true},
		{"exact miss", []string{"gpt-4o"}, "gpt-4", 0, false},
		{"prefix hit", []string{"gpt-4o-mini"}, "gpt-4", 1, true},
		{"prefix miss", []string{"o3-mini"}, "gpt-4", 1, false},
		{"contains hit", []string{"openai/gpt-4o"}, "gpt-4", 2, true},
		{"contains miss", []string{"claude-3"}, "gpt-4", 2, false},
		{"suffix hit", []string{"gpt-4-turbo"}, "turbo", 3, true},
		{"suffix miss", []string{"turbo-gpt-4"}, "turbo", 3, false},
		{"unknown rule", []string{"gpt-4o"}, "gpt-4o", 9, false},
		{"underscore literal", []string{"gpt_4o"}, "gpt_", 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := channelSupportsModel(tc.models, tc.pattern, tc.rule); got != tc.expected {
				t.Fatalf("channelSupportsModel(%v, %q, %d) = %v，期望 %v",
					tc.models, tc.pattern, tc.rule, got, tc.expected)
			}
		})
	}
}

func TestModelCatalogDerivedSquareState(t *testing.T) {
	groups := []string{"default", "vip"}
	channels := []derivedChannel{
		{models: []string{"m-all"}, allGroups: true},
		{models: []string{"m-vip"}, groups: []string{"vip"}},
		{models: []string{"m-none"}, groups: []string{"other"}},
	}
	derive := func(pattern string) (ModelDerived, bool) {
		covered := map[string]struct{}{}
		allGroups := false
		var count int64
		for _, channel := range channels {
			if channelSupportsModel(channel.models, pattern, 0) {
				count++
				if channel.allGroups {
					allGroups = true
				}
				for _, group := range channel.groups {
					covered[group] = struct{}{}
				}
			}
		}
		info := ModelDerived{ConfiguredChannelCount: count, EnableGroups: renderEnableGroups(groups, covered, allGroups)}
		switch {
		case count == 0:
			info.SquareState = squareStateUnavailable
		case allGroups || coversAll(groups, covered):
			info.SquareState = squareStateVisible
		case len(info.EnableGroups) > 0:
			info.SquareState = squareStatePartial
		default:
			info.SquareState = squareStateUnavailable
		}
		return info, count > 0
	}
	all, _ := derive("m-all")
	if all.SquareState != squareStateVisible || all.ConfiguredChannelCount != 1 || len(all.EnableGroups) != 2 {
		t.Fatalf("all groups = %+v", all)
	}
	vip, _ := derive("m-vip")
	if vip.SquareState != squareStatePartial || len(vip.EnableGroups) != 1 || vip.EnableGroups[0] != "vip" {
		t.Fatalf("partial = %+v", vip)
	}
	none, _ := derive("m-none")
	if none.SquareState != squareStateUnavailable || none.ConfiguredChannelCount != 1 {
		t.Fatalf("no group = %+v", none)
	}
	missing, ok := derive("absent")
	if ok || missing.SquareState != squareStateUnavailable || missing.ConfiguredChannelCount != 0 {
		t.Fatalf("absent = %+v", missing)
	}
}

// ---------------------------------------------------------------------------
// D1b 收口：同步去重/回滚、null 口径、清单校验、删除竞争窗口、重定向
// ---------------------------------------------------------------------------

// syncManifestFor 构造任意上游清单原文。
func syncManifestFor(t *testing.T, models, vendors []map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"version": "upstream-x", "models": models, "vendors": vendors})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return raw
}

// previewSourceVersion 跑一次 preview 并返回 source_version（顺带写入 Redis 缓存）。
func previewSourceVersion(t *testing.T, env *catalogTestEnv) string {
	t.Helper()
	rr := env.request(catalogFullAdmin(), http.MethodGet, "/api/models/sync/preview", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var preview syncPreviewResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &preview); err != nil {
		t.Fatalf("preview JSON: %v", err)
	}
	return preview.SourceVersion
}

func TestModelCatalogSyncDuplicateSelections(t *testing.T) {
	env := newCatalogTestEnv(t)
	env.syncRaw = syncManifestFor(t,
		[]map[string]any{{"model_name": "dup-model"}},
		[]map[string]any{{"name": "DupVendor"}},
	)
	version := previewSourceVersion(t, env)

	// 内容完全一致的重复项：幂等合并，只创建一次。
	rr := env.request(catalogFullAdmin(), http.MethodPost, "/api/models/sync", map[string]any{
		"source_version": version,
		"models": []any{
			map[string]any{"model_name": "dup-model"},
			map[string]any{"model_name": "dup-model"},
		},
		"vendors": []any{
			map[string]any{"name": "dupvendor"},
			map[string]any{"name": "DupVendor"},
		},
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("幂等去重 = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var applied syncApplyResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &applied)
	if len(applied.CreatedModels) != 1 || len(applied.CreatedVendors) != 1 {
		t.Fatalf("重复项应只写一次：%+v", applied)
	}

	// 同名但内容不同：422，且不产生写入。
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models/sync", map[string]any{
		"source_version": version,
		"models": []any{
			map[string]any{"model_name": "dup-model", "display_name": "A"},
			map[string]any{"model_name": "dup-model", "display_name": "B"},
		},
	}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("冲突重复项 = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestModelCatalogSyncAppliesAtomically(t *testing.T) {
	env := newCatalogTestEnv(t)
	// bad-model 的 capabilities 非法：供应商写入成功后会在这里失败，用于验证
	// 整体回滚（供应商不能残留）。
	env.syncRaw = syncManifestFor(t,
		[]map[string]any{{"model_name": "bad-model", "capabilities": []string{"not-a-capability"}}},
		[]map[string]any{{"name": "RollbackVendor", "description": "v"}},
	)
	version := previewSourceVersion(t, env)

	rr := env.request(catalogFullAdmin(), http.MethodPost, "/api/models/sync", map[string]any{
		"source_version": version,
		"models":         []any{map[string]any{"model_name": "bad-model"}},
		"vendors":        []any{map[string]any{"name": "RollbackVendor"}},
	}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("非法模型 = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	vendors, _, err := env.repo.ListVendors(context.Background(), store.VendorFilter{Page: 1, PageSize: 100})
	if err != nil {
		t.Fatalf("list vendors: %v", err)
	}
	for _, vendor := range vendors {
		if strings.EqualFold(vendor.Name, "RollbackVendor") {
			t.Fatalf("事务未回滚：供应商 %q 被写入", vendor.Name)
		}
	}
}

func TestModelCatalogSyncMissingOrNullDoesNotDiff(t *testing.T) {
	env := newCatalogTestEnv(t)
	rr := env.request(catalogFullAdmin(), http.MethodPost, "/api/models", map[string]any{
		"model_name": "null-desc", "description": "local-null",
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create null-desc = %d", rr.Code)
	}
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models", map[string]any{
		"model_name": "missing-desc", "description": "local-missing",
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create missing-desc = %d", rr.Code)
	}

	// 显式 null 与字段缺失都必须「不参与 diff/patch」。
	env.syncRaw = syncManifestFor(t, []map[string]any{
		{"model_name": "null-desc", "description": nil},
		{"model_name": "missing-desc"},
	}, nil)
	version := previewSourceVersion(t, env)

	rr = env.request(catalogFullAdmin(), http.MethodGet, "/api/models/sync/preview", nil, nil)
	var preview syncPreviewResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &preview)
	if len(preview.Groups.Update) != 0 || len(preview.Groups.Unchanged) != 2 {
		t.Fatalf("null/缺失不应产生 update：update=%+v unchanged=%+v",
			preview.Groups.Update, preview.Groups.Unchanged)
	}

	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models/sync", map[string]any{
		"source_version": version,
		"models": []any{
			map[string]any{"model_name": "null-desc"},
			map[string]any{"model_name": "missing-desc"},
		},
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("apply = %d（body=%q）", rr.Code, rr.Body.String())
	}
	models, _, _ := env.repo.ListModelMeta(context.Background(), store.ModelMetaFilter{Page: 1, PageSize: 100})
	for _, model := range models {
		switch model.ModelName {
		case "null-desc":
			if model.Description == nil || *model.Description != "local-null" {
				t.Fatalf("null 清空了本地值：%+v", model.Description)
			}
		case "missing-desc":
			if model.Description == nil || *model.Description != "local-missing" {
				t.Fatalf("缺失清空了本地值：%+v", model.Description)
			}
		}
	}
}

func TestModelCatalogSyncWritesProfileFields(t *testing.T) {
	env := newCatalogTestEnv(t)
	createTestModel(t, env, "profile-model")
	env.syncRaw = syncManifestFor(t, []map[string]any{{
		"model_name":       "profile-model",
		"vendor":           "Acme",
		"endpoints":        map[string]any{"/v1/chat/completions": []string{"POST"}},
		"modalities":       []string{"text", "image"},
		"parameters":       []map[string]any{{"name": "temperature", "type": "number"}},
		"knowledge_cutoff": "2025-03",
		"released_at":      "2025-03-14",
	}}, []map[string]any{{"name": "Acme"}})

	rr := env.request(catalogFullAdmin(), http.MethodGet, "/api/models/sync/preview", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var preview syncPreviewResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &preview); err != nil {
		t.Fatalf("preview JSON: %v", err)
	}
	if len(preview.Groups.Update) != 1 {
		t.Fatalf("update = %+v", preview.Groups.Update)
	}
	for _, field := range []string{
		"vendor", "endpoints", "modalities", "parameters", "knowledge_cutoff", "released_at",
	} {
		if _, ok := preview.Groups.Update[0].Changes[field]; !ok {
			t.Fatalf("preview 未报告 %s 差异：%+v", field, preview.Groups.Update[0].Changes)
		}
	}

	// 只勾模型、不勾供应商：名字解析不到，整批 422（store 不替上游建行）。
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models/sync", map[string]any{
		"source_version": preview.SourceVersion,
		"models":         []any{map[string]any{"model_name": "profile-model"}},
	}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("缺供应商 = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	if byName := modelBy(t, env, "profile-model"); byName.VendorID != nil {
		t.Fatalf("失败的 apply 不应写入：%+v", byName.VendorID)
	}

	// 一起勾上供应商：模型与供应商同事务写入。
	rr = env.request(catalogFullAdmin(), http.MethodPost, "/api/models/sync", map[string]any{
		"source_version": preview.SourceVersion,
		"models":         []any{map[string]any{"model_name": "profile-model"}},
		"vendors":        []any{map[string]any{"name": "Acme"}},
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("apply = %d（body=%q）", rr.Code, rr.Body.String())
	}
	model := modelBy(t, env, "profile-model")
	if model.VendorID == nil {
		t.Fatalf("vendor_id 未写入")
	}
	var vendorName string
	for _, vendor := range env.repo.vendors {
		if vendor.ID == *model.VendorID {
			vendorName = vendor.Name
		}
	}
	if vendorName != "Acme" {
		t.Fatalf("vendor_id 指向 %q，期望 Acme", vendorName)
	}
	var endpoints map[string][]string
	if err := json.Unmarshal(model.Endpoints, &endpoints); err != nil {
		t.Fatalf("endpoints 不是对象：%v（%q）", err, model.Endpoints)
	}
	if len(endpoints["/v1/chat/completions"]) != 1 || endpoints["/v1/chat/completions"][0] != "POST" {
		t.Fatalf("endpoints = %+v", endpoints)
	}
	var modalities []string
	if err := json.Unmarshal(model.Modalities, &modalities); err != nil || len(modalities) != 2 {
		t.Fatalf("modalities = %q（%v）", model.Modalities, err)
	}
	var params []map[string]any
	if err := json.Unmarshal(model.Parameters, &params); err != nil || len(params) != 1 {
		t.Fatalf("parameters = %q（%v）", model.Parameters, err)
	}
	if model.KnowledgeCutoff == nil || *model.KnowledgeCutoff != "2025-03" {
		t.Fatalf("knowledge_cutoff = %+v", model.KnowledgeCutoff)
	}
	if model.ReleasedAt == nil || model.ReleasedAt.Format("2006-01-02") != "2025-03-14" {
		t.Fatalf("released_at = %+v", model.ReleasedAt)
	}

	// 再跑一次 preview：档案已与清单一致 ⇒ unchanged（大小写不敏感比较）。
	rr = env.request(catalogFullAdmin(), http.MethodGet, "/api/models/sync/preview", nil, nil)
	var after syncPreviewResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &after)
	if len(after.Groups.Update) != 0 {
		t.Fatalf("同步后仍报 update：%+v", after.Groups.Update)
	}
}

// modelBy 取假仓储里的档案（测试断言用）。
func modelBy(t *testing.T, env *catalogTestEnv, name string) store.ModelMeta {
	t.Helper()
	for _, model := range env.repo.models {
		if model.ModelName == name {
			return *model
		}
	}
	t.Fatalf("模型 %s 不存在", name)
	return store.ModelMeta{}
}

func TestModelCatalogSyncPreviewUnknownVendor(t *testing.T) {
	env := newCatalogTestEnv(t)
	// 上游写了 vendor，但既不在本地、也没在清单 vendors 里声明 ⇒ 解析不了，
	// preview 必须提前暴露（否则勾选后才 422）。
	env.syncRaw = syncManifestFor(t, []map[string]any{
		{"model_name": "ghost-vendor-model", "vendor": "Nowhere"},
	}, nil)
	rr := env.request(catalogFullAdmin(), http.MethodGet, "/api/models/sync/preview", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var preview syncPreviewResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &preview)
	if len(preview.Groups.MissingVendor) != 1 || preview.Groups.MissingVendor[0] != "ghost-vendor-model" {
		t.Fatalf("missing_vendor = %+v", preview.Groups.MissingVendor)
	}
	if len(preview.Groups.Create) != 0 {
		t.Fatalf("解析不了供应商的条目不应进 create：%+v", preview.Groups.Create)
	}
}

func TestModelCatalogSyncProfileFieldValidation(t *testing.T) {
	cases := []struct {
		name  string
		model map[string]any
	}{
		{"endpoints not object", map[string]any{"model_name": "m", "endpoints": []string{"/v1/chat/completions"}}},
		{"endpoints string", map[string]any{"model_name": "m", "endpoints": "/v1/chat/completions"}},
		{"modalities not array", map[string]any{"model_name": "m", "modalities": "text"}},
		{"parameters not array", map[string]any{"model_name": "m", "parameters": map[string]any{"name": "x"}}},
		{"knowledge_cutoff bad month", map[string]any{"model_name": "m", "knowledge_cutoff": "2025-13"}},
		{"knowledge_cutoff bad shape", map[string]any{"model_name": "m", "knowledge_cutoff": "2025/03"}},
		{"released_at not a date", map[string]any{"model_name": "m", "released_at": "yesterday"}},
		{"released_at blank", map[string]any{"model_name": "m", "released_at": ""}},
		{"vendor blank", map[string]any{"model_name": "m", "vendor": "   "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseSyncManifest(syncManifestFor(t, []map[string]any{tc.model}, nil)); err == nil {
				t.Fatalf("非法档案字段未被拒绝")
			}
		})
	}

	// 合法形状：date-time 也可，输出统一成 YYYY-MM-DD。
	manifest, err := parseSyncManifest(syncManifestFor(t, []map[string]any{{
		"model_name": "m", "released_at": "2025-03-14T08:30:00Z", "vendor": "  Acme  ",
	}}, nil))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if manifest.Models[0].ReleasedAt == nil || *manifest.Models[0].ReleasedAt != "2025-03-14" {
		t.Fatalf("released_at 未规范成日期：%+v", manifest.Models[0].ReleasedAt)
	}
	if manifest.Models[0].Vendor == nil || *manifest.Models[0].Vendor != "Acme" {
		t.Fatalf("vendor 未裁剪：%+v", manifest.Models[0].Vendor)
	}
}

func TestDiffSyncModelVendorCaseInsensitive(t *testing.T) {
	upstreamVendor := "acme"
	upstream := syncManifestModel{
		Vendor:  &upstreamVendor,
		Present: map[string]bool{"vendor": true},
	}
	if changes := diffSyncModel(store.ModelMeta{}, "Acme", upstream); len(changes) != 0 {
		t.Fatalf("只差大小写不应产生差异：%+v", changes)
	}
	other := "Other"
	upstream.Vendor = &other
	changes := diffSyncModel(store.ModelMeta{}, "Acme", upstream)
	if _, ok := changes["vendor"]; !ok {
		t.Fatalf("供应商变更未报告：%+v", changes)
	}
}

func TestModelCatalogSyncManifestVendorValidation(t *testing.T) {
	cases := []struct {
		name   string
		vendor map[string]any
	}{
		{"name too long", map[string]any{"name": strings.Repeat("n", catalogVendorNameMaxRunes+1)}},
		{"description too long", map[string]any{"name": "v", "description": strings.Repeat("d", 513)}},
		{"icon too long", map[string]any{"name": "v", "icon": strings.Repeat("i", catalogIconMaxRunes+1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseSyncManifest(syncManifestFor(t, nil, []map[string]any{tc.vendor})); err == nil {
				t.Fatalf("非法供应商清单未被拒绝")
			}
		})
	}

	// 上游数据问题 → preview 502（不是 422）。
	env := newCatalogTestEnv(t)
	env.syncRaw = syncManifestFor(t, nil, []map[string]any{
		{"name": "v", "description": strings.Repeat("d", 513)},
	})
	rr := env.request(catalogFullAdmin(), http.MethodGet, "/api/models/sync/preview", nil, nil)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("非法清单 = %d，期望 502", rr.Code)
	}
	problem := decodeCatalogProblem(t, rr)
	if problem["detail"] != "the upstream manifest does not match the expected schema" {
		t.Fatalf("502 文案必须固定，实际 %v", problem["detail"])
	}
}

func TestModelCatalogSyncManifestRejectsTrailingData(t *testing.T) {
	if _, err := parseSyncManifest([]byte(`{"models":[]}{"models":[]}`)); err == nil {
		t.Fatalf("尾随 JSON 文档未被拒绝")
	}
}

func TestModelCatalogSyncManifestTrimsNames(t *testing.T) {
	manifest, err := parseSyncManifest(syncManifestFor(t,
		[]map[string]any{{"model_name": "  gpt-4  "}},
		[]map[string]any{{"name": "  Acme  "}},
	))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if manifest.Models[0].ModelName != "gpt-4" || manifest.Vendors[0].Name != "Acme" {
		t.Fatalf("名称未统一裁剪：%+v %+v", manifest.Models[0], manifest.Vendors[0])
	}
}

func TestModelCatalogDeleteRejectsConcurrentUpdate(t *testing.T) {
	env := newCatalogTestEnv(t)
	view := createTestModel(t, env, "race-model")
	stale := view.UpdatedAt

	// 模拟「读后、删前」另一次更新已提交。
	rr := env.request(catalogFullAdmin(), http.MethodPut,
		fmt.Sprintf("/api/models/%d", view.ID),
		map[string]any{"description": "updated", "updated_at": stale.Format(time.RFC3339Nano)}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("并发更新 = %d（body=%q）", rr.Code, rr.Body.String())
	}

	rr = env.request(catalogFullAdmin(), http.MethodDelete,
		fmt.Sprintf("/api/models/%d?updated_at=%s", view.ID, url.QueryEscape(stale.Format(time.RFC3339Nano))),
		nil, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("过期版本删除 = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}
	rr = env.request(catalogFullAdmin(), http.MethodGet, fmt.Sprintf("/api/models/%d", view.ID), nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("409 删除不应删行，GET = %d", rr.Code)
	}
}

func TestModelCatalogSearchStatusUsesSQLFilter(t *testing.T) {
	env := newCatalogTestEnv(t)
	disabled := int16(2)
	_, _ = env.repo.CreateModelMeta(context.Background(), store.ModelMetaInput{
		ModelName: "disabled-match", Status: &disabled,
	})
	createTestModel(t, env, "enabled-match")

	rr := env.request(catalogFullAdmin(), http.MethodGet, "/api/models/search?status=disabled", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("search = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var list modelSearchListView
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("search JSON: %v", err)
	}
	for _, item := range list.Items {
		if item.Status != 2 {
			t.Fatalf("status=disabled 返回了启用项：%+v", item)
		}
	}
	if len(list.Items) != 1 || list.Items[0].ModelName != "disabled-match" {
		t.Fatalf("status 过滤结果 = %+v", list.Items)
	}
}

func TestModelCatalogReleasedAtIsDate(t *testing.T) {
	env := newCatalogTestEnv(t)
	rr := env.request(catalogFullAdmin(), http.MethodPost, "/api/models", map[string]any{
		"model_name": "dated-model", "released_at": "2026-01-02",
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if body["released_at"] != "2026-01-02" {
		t.Fatalf("released_at = %v，期望 YYYY-MM-DD（契约 format: date）", body["released_at"])
	}
}

func TestModelCatalogUnknownWriteFieldDenied(t *testing.T) {
	env := newCatalogTestEnv(t)
	rr := env.request(catalogFullAdmin(), http.MethodPost, "/api/models",
		map[string]any{"model_name": "unknown-field", "bogus": 1}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("未知字段 = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestVendorOperationErrorLocatesField(t *testing.T) {
	cases := []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"action": "assign", "target_vendor_id": "not-an-int"}, "target_vendor_id"},
		{map[string]any{"action": "assign", "model_ids": "not-an-array"}, "model_ids"},
		{map[string]any{"action": "nope"}, "action"},
	}
	env := newCatalogTestEnv(t)
	for _, tc := range cases {
		rr := env.request(catalogFullAdmin(), http.MethodPost, "/api/vendors/operations/preview", tc.body, nil)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%+v = %d，期望 422", tc.body, rr.Code)
		}
		problem := decodeCatalogProblem(t, rr)
		fields, _ := problem["fields"].([]any)
		if len(fields) != 1 || fields[0] != tc.field {
			t.Fatalf("fields = %v，期望 [%s]", problem["fields"], tc.field)
		}
	}
}

func TestValidateSyncURL(t *testing.T) {
	// 同步拉取已并入统一出站执行器（B2）；这里验证 basic + 无域名限制下
	// 的形状规则：协议白名单、仅 80/443、拒绝 userinfo。
	guard := egress.NewGuard(egress.Policy{Mode: egress.ModeBasic, DomainMode: egress.DomainAll})
	allowed := []string{"http://example.com/manifest.json", "https://example.com:443/x", "http://example.com:80/x"}
	for _, raw := range allowed {
		if _, err := guard.ValidateURLShape(raw); err != nil {
			t.Fatalf("应允许 %q：%v", raw, err)
		}
	}
	rejected := []string{
		"http://example.com:6379/x",
		"http://example.com:22/x",
		"ftp://example.com/x",
		"http://user:pass@example.com/x",
		"http:///no-host",
	}
	for _, raw := range rejected {
		if _, err := guard.ValidateURLShape(raw); err == nil {
			t.Fatalf("应拒绝目标 %q", raw)
		}
	}
}

// TestSyncSourceUsesEgressGuard 是 B2 接入回归：模型同步拉取必须经过
// 统一出站执行器。伪造解析器返回私网地址时，请求在连接前被拒绝，且
// 解析恰好发生一次（反 rebinding 路径）。
func TestSyncSourceUsesEgressGuard(t *testing.T) {
	var calls int
	syncEgressResolver = func(context.Context, string) ([]net.IP, error) {
		calls++
		return []net.IP{net.ParseIP("10.0.0.8")}, nil
	}
	defer func() { syncEgressResolver = nil }()

	source := NewModelSyncSourceFromConfig(mapConfig{
		"model_sync.source_url": json.RawMessage(`"http://sync.example/manifest.json"`),
		// 放开域名维度，确保拒绝来自地址规则而不是 allowlist。
		"sec.ssrf.domain_mode": json.RawMessage(`"all"`),
	})
	if source == nil {
		t.Fatal("sync source 不应为 nil")
	}
	if _, err := source(context.Background()); !errors.Is(err, ErrSyncSourceUnavailable) {
		t.Fatalf("err = %v，期望 ErrSyncSourceUnavailable", err)
	}
	if calls != 1 {
		t.Fatalf("解析次数 = %d，期望 1", calls)
	}
}

// TestSyncSourceThroughGuardedClient 是 LOW-5 补测：同步拉取的成功分支
// 同样要经 egress 执行器（解析一次 → 校验 → 拨已校验 IP），而不是只在
// 拒绝路径上接入守卫。
func TestSyncSourceThroughGuardedClient(t *testing.T) {
	payload := []byte(`{"models":[]}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/manifest.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	guard := egress.NewGuard(egress.Policy{
		Mode: egress.ModeBasic, DomainMode: egress.DomainAll,
		// AllowLoopback + AllowAnyPort 是仅测试装配的接缝（httptest 随机端口）。
		AllowLoopback: true, AllowAnyPort: true,
	})
	body, err := fetchSyncManifest(context.Background(), server.URL+"/manifest.json", guard)
	if err != nil {
		t.Fatalf("fetchSyncManifest: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("body = %q，期望 %q", body, payload)
	}
}
