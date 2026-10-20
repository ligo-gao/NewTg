package store

// 模型目录与供应商目录的管理面仓储（modules-06 F-06-22/23/24/26；
// platform-01 §22.7.1/§22.7.2；D1）。
//
// 【为什么放在 store 而不是 httpapi】目录是管理面 CRUD、公开模型广场（D1b）
// 与模型缺失检测的共同数据源。把读写收敛到 store 后，处理器只依赖一个接缝；
// 引用保护、批量事务与乐观锁都是**数据层不变量**，不能依赖每个 handler 自行
// 记得执行。
//
// 【为什么 vendors 没有 version 列】版本是 7 列规范化后的 sha256 派生值
// （Vendor.Version）。落库会形成第二真源：绕过应用的写入会让存储版本与内容
// 脱节，乐观锁从此失真（D1-01）。
//
// 【为什么引用保护不设 FK】见迁移文件头：级联删除会连带删掉模型档案；正确
// 语义是「有引用 ⇒ 409 + 引用计数」，由 merge 先改挂再删源（D1-07/D1-08）。
//
// 【为什么批量操作必须单事务】F-06-24「事务内任一失败 ⇒ 整体回滚」：一半
// 模型改挂、源供应商已删的中间态比失败本身更难修复。所有多行写入都在同一
// `sql.Tx` 内完成，任一步报错即 defer Rollback。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// 目录仓储的哨兵错误，供 httpapi 映射 404/409/422。
var (
	// ErrVendorNotFound：供应商行不存在。
	//
	// 【handler 映射口径（L-1）】同一个哨兵在两种上下文里映射不同状态码：
	//   - 供应商资源上下文（GET/PUT/DELETE /vendors/{id}）→ 404：被寻址的
	//     资源本身不存在；
	//   - 模型档案上下文（CreateModelMeta/UpdateModelMeta 的 vendor_id）→ 422：
	//     请求体里的引用无效，属于字段校验失败，不是「被寻址的资源不存在」。
	// 哨兵本身不携带上下文，由 handler 按调用点决定映射。
	ErrVendorNotFound = errors.New("store: vendor not found")
	// ErrVendorDuplicate：供应商名（忽略大小写）已存在 → 409。
	ErrVendorDuplicate = errors.New("store: vendor name already exists")
	// ErrVendorConflict：派生 version 与期望值不符（乐观锁）→ 409。
	ErrVendorConflict = errors.New("store: vendor version conflict")
	// ErrVendorInvalid：供应商字段不合法（空名/状态越界等）→ 422。
	ErrVendorInvalid = errors.New("store: vendor input is invalid")
	// ErrVendorOperationInvalid：批量操作的 action/参数组合不合法 → 422。
	ErrVendorOperationInvalid = errors.New("store: vendor operation is invalid")

	// ErrModelMetaNotFound：模型档案不存在。
	ErrModelMetaNotFound = errors.New("store: model metadata not found")
	// ErrModelMetaDuplicate：model_name 已存在 → 409。
	ErrModelMetaDuplicate = errors.New("store: model metadata already exists")
	// ErrModelMetaConflict：updated_at 与期望值不符（乐观锁）→ 409。
	ErrModelMetaConflict = errors.New("store: model metadata was modified concurrently")

	// ErrCatalogEmptySelection：去重后选择为空 → 422。
	ErrCatalogEmptySelection = errors.New("store: selection is empty after deduplication")
	// ErrCatalogTooLarge：单次批量超过 1000 条 → 422（F-06-22/F-06-24）。
	ErrCatalogTooLarge = errors.New("store: selection exceeds 1000 rows")
)

// ErrModelMetaInvalid 是模型档案字段校验失败（JSON 形态/长度/取值域）→ 400。
//
// Field 用列名（description/tags/endpoints/...），Reason 是可直接回填到字段级
// 错误的可读原因；handler 无需再猜是哪个字段不合法。空 Field 表示「未细分的
// 输入非法」（例如 DB CHECK 兜底命中）。
type ErrModelMetaInvalid struct {
	Field  string
	Reason string
}

func (e ErrModelMetaInvalid) Error() string {
	if e.Field == "" {
		return "store: model metadata input is invalid"
	}
	return fmt.Sprintf("store: model metadata input is invalid: %s: %s", e.Field, e.Reason)
}

// Is 让 errors.Is(err, ErrModelMetaInvalid{}) 可用于类型判断。
func (e ErrModelMetaInvalid) Is(target error) bool {
	_, ok := target.(ErrModelMetaInvalid)
	return ok
}

// ErrChannelModelsEmpty 表示「从渠道移除模型」会让渠道的 supported_models 变空。
//
// supported_models 上有 channels_supported_models_chk（至少 1 个元素），空数组
// 既不可表示也违反渠道契约：没有任何模型的渠道无法路由，应当由管理员显式
// 停用/删除，而不是被批量删除档案的副作用悄悄掏空。
//
// ChannelIDs 是预先算出的「移除后会变空」的启用渠道；DB CHECK 兜底（例如停用
// 渠道也命中）时该列表为空。
type ErrChannelModelsEmpty struct {
	ChannelIDs []int64
}

func (e ErrChannelModelsEmpty) Error() string {
	if len(e.ChannelIDs) == 0 {
		return "store: removing models would leave a channel with no supported models"
	}
	return fmt.Sprintf("store: removing models would leave channels %v with no supported models", e.ChannelIDs)
}

// Is 让 errors.Is(err, ErrChannelModelsEmpty{}) 可用于类型判断；
// 调用方用 errors.As 读取 ChannelIDs。
func (e ErrChannelModelsEmpty) Is(target error) bool {
	_, ok := target.(ErrChannelModelsEmpty)
	return ok
}

// VendorVersionConflict 描述一个在批量操作版本校验中「当前版本」的供应商。
//
// 【为什么不是「仅冲突供应商」】expected_version 是整个操作集的 sha256 指纹
// （见 VendorOperation 的冻结语义），指纹不可分解，无法定位是哪一行漂移；
// 因此不一致时返回全部涉及供应商的当前版本，由客户端对比后重新 preview。
type VendorVersionConflict struct {
	VendorID       int64
	CurrentVersion string
}

// ErrVendorVersionConflict 表示批量操作的 expected_version 缺失或已过期 → 409。
type ErrVendorVersionConflict struct {
	Conflicts []VendorVersionConflict
}

func (e ErrVendorVersionConflict) Error() string {
	if len(e.Conflicts) == 0 {
		return "store: vendor version conflict"
	}
	parts := make([]string, 0, len(e.Conflicts))
	for _, c := range e.Conflicts {
		parts = append(parts, fmt.Sprintf("%d=%s", c.VendorID, c.CurrentVersion))
	}
	return "store: vendor version conflict: " + strings.Join(parts, ", ")
}

// Is 让 errors.Is(err, ErrVendorVersionConflict{}) 可用于类型判断。
func (e ErrVendorVersionConflict) Is(target error) bool {
	_, ok := target.(ErrVendorVersionConflict)
	return ok
}

// ErrVendorReferenced 表示供应商仍被模型档案引用，删除被拒。
//
// ReferenceCount 是引用它的 model_meta 行数（**含停用模型**，D1-07）：
// 只数启用行会让「先停用模型再删供应商」留下悬挂的逻辑引用。
type ErrVendorReferenced struct {
	ReferenceCount int64
}

func (e ErrVendorReferenced) Error() string {
	return fmt.Sprintf("store: vendor is referenced by %d model(s)", e.ReferenceCount)
}

// Is 让 errors.Is(err, ErrVendorReferenced{}) 可用于「是否引用类错误」的判断；
// 调用方仍应用 errors.As 读取 ReferenceCount。
func (e ErrVendorReferenced) Is(target error) bool {
	_, ok := target.(ErrVendorReferenced)
	return ok
}

// maxCatalogBatch 是批量选择的上限（F-06-22「批量删除 1–1000」、
// F-06-24「条目数 >1000 ⇒ 422」）。在仓储层兜底而不是只信 handler：
// 任何新的调用方都不应能把一次事务放大到无界行数。
const maxCatalogBatch = 1000

// 供应商批量操作动作（F-06-24）。
const (
	VendorActionAssign = "assign"
	VendorActionMerge  = "merge"
	VendorActionDelete = "delete"
)

// VendorInput 是创建供应商的写入值。Status 为 nil 时取默认 1（启用）。
type VendorInput struct {
	Name        string
	Description *string
	Icon        *string
	Status      *int16
}

// VendorUpdate 是全量更新供应商的写入值（名称/描述/图标/状态）。
type VendorUpdate struct {
	Name        string
	Description *string
	Icon        *string
	Status      int16
}

// VendorFilter 是供应商列表的筛选条件。
type VendorFilter struct {
	Page     int
	PageSize int
	Keyword  string
	Status   *int16
	// Attached：true=只显示已挂模型的供应商；false=只显示未挂模型的；
	// nil=不筛选（F-06-23「已挂模型 / 未挂模型」分区）。
	Attached *bool
}

// VendorOperation 是 assign/merge/delete 的入参（F-06-24）。
//
// 【expected_version 的冻结语义（M-1）】ExpectedVersion 是本次操作涉及的全部
// 供应商的**操作指纹**：
//
//	operation fingerprint = sha256(
//	    "<id>=<Vendor.Version()>" 依次以 "\n" 连接，id 升序
//	)，小写 hex
//
// 「涉及的全部供应商」= target（assign/merge，assign 的 target=0 时无）∪
// 全部 sources（merge/delete 的 VendorIDs），按 id 升序去重。空集合的指纹是
// sha256("")（不是空串）：客户端必须先 preview 才能拿到指纹。
//
// 【为什么用整体指纹而不是逐行版本】preview 与 apply 必须共享同一份「读版本
// → 比对 → 写入」的原子视图。把它压成一个值后，apply 只需在事务内（已按 id
// 升序 FOR UPDATE 锁行）重算并做一次相等比较，不存在「先比 A 后比 B、中间 B
// 被改」的窗口；也避免把 per-row 版本冒泡到 API 契约。
type VendorOperation struct {
	Action          string
	ModelIDs        []int64
	VendorIDs       []int64
	TargetVendorID  int64
	ExpectedVersion string
}

// VendorOperationResult 是批量操作的结果（F-06-24 出参的最小投影）。
type VendorOperationResult struct {
	// Affected = 改挂/删除的模型行数（merge 含全部改挂行；delete 为删除的
	// 供应商数）。handler 负责把它与 preview 的影响集合比对。
	Affected int64
}

// VendorVersionRef 是 preview 返回的单个供应商当前版本。
type VendorVersionRef struct {
	VendorID int64
	Version  string
}

// VendorPreview 是 apply 之前的只读影响预览（D1b 的 preview 端点直接调用）。
type VendorPreview struct {
	// Affected 与 ApplyVendorOperations 返回的 Affected 同口径：
	// assign/merge 为受影响的模型行数，delete 为将被删除的供应商数。
	Affected int64
	// Conflicts 只在调用方传了非空 ExpectedVersion 且与当前指纹不一致时非空。
	Conflicts []VendorVersionConflict
	// ExpectedVersion 是当前操作集的指纹；调用方应原样回传给 apply。
	ExpectedVersion string
	// Vendors 是参与指纹计算的供应商及其当前版本（id 升序）。
	Vendors []VendorVersionRef
}

// ModelMetaInput 是创建模型档案的写入值。
//
// Status/SyncOfficial 用指针表达「未提交 ⇒ DB 默认值」（1 / true）；
// NameRule 的合法零值 0 就是默认值，故用普通值。
// 四个 jsonb 字段为 nil 时按列默认值落库（[] 或 {}），不会写成 SQL NULL。
type ModelMetaInput struct {
	ModelName       string
	DisplayName     *string
	Description     *string
	Icon            *string
	Tags            []byte
	VendorID        *int64
	Endpoints       []byte
	Capabilities    []byte
	Modalities      []byte
	Parameters      []byte
	ContextLength   *int64
	MaxOutput       *int64
	KnowledgeCutoff *string
	ReleasedAt      *time.Time
	Status          *int16
	SyncOfficial    *bool
	NameRule        int16
}

// ModelMetaPatch 是模型档案的部分更新。Set 标记区分「未提交」与「显式置空」。
type ModelMetaPatch struct {
	ModelName *string

	DisplayName    *string
	DisplayNameSet bool

	Description    *string
	DescriptionSet bool

	Icon    *string
	IconSet bool

	Tags    []byte
	TagsSet bool

	VendorID    *int64
	VendorIDSet bool

	Endpoints    []byte
	EndpointsSet bool

	Capabilities    []byte
	CapabilitiesSet bool

	Modalities    []byte
	ModalitiesSet bool

	Parameters    []byte
	ParametersSet bool

	ContextLength    *int64
	ContextLengthSet bool

	MaxOutput    *int64
	MaxOutputSet bool

	KnowledgeCutoff    *string
	KnowledgeCutoffSet bool

	ReleasedAt    *time.Time
	ReleasedAtSet bool

	Status       *int16
	SyncOfficial *bool
	NameRule     *int16
}

// ModelMetaFilter 是模型档案列表的筛选条件。
type ModelMetaFilter struct {
	Page     int
	PageSize int
	Keyword  string
	VendorID *int64
	Status   *int16
}

// MissingModel 是「渠道在跑、目录无档」的一行（F-06-26）。
type MissingModel struct {
	ModelName    string
	ChannelCount int64
	// HasPricing = model_pricing 中存在同名行（不区分启停：有价即说明管理员
	// 配过价，缺失检测只关心「是否已有定价」这一事实）。
	HasPricing bool
}

// BatchDeleteResult 是批量删除的副作用计数（F-06-22 的两个可选项）。
//
// Requested 是去重后的选择集大小，Deleted 是实际命中的档案数：部分 id 不存在
// 不算错误，两个计数如实相减即可得出「未命中」条数（L-5）。
type BatchDeleteResult struct {
	Requested           int64
	Deleted             int64
	ChannelsUpdated     int64
	PricingDeleted      int64
	GroupPricingDeleted int64
}

// MissingResult 是「渠道在跑、目录无档」的检测结果（F-06-26）。
type MissingResult struct {
	Items []MissingModel
	// SkippedChannels 是因 supported_models 含非法元素而整渠道跳过的启用渠道数。
	// 暴露计数而不是静默丢弃：跳过意味着「这个渠道的模型集合不可信」，管理员
	// 需要知道缺失清单不完整的原因（M-3）。
	SkippedChannels int64
}

// ModelCatalogRepo 是目录管理端点的存储接缝（单测注入内存假实现）。
type ModelCatalogRepo interface {
	CreateVendor(ctx context.Context, in VendorInput) (Vendor, error)
	GetVendor(ctx context.Context, id int64) (Vendor, error)
	UpdateVendor(ctx context.Context, id int64, in VendorUpdate, expectVersion string) (Vendor, error)
	DeleteVendor(ctx context.Context, id int64) error
	ListVendors(ctx context.Context, filter VendorFilter) ([]Vendor, int64, error)
	SearchVendors(ctx context.Context, keyword string, limit int) ([]Vendor, error)
	ApplyVendorOperations(ctx context.Context, op VendorOperation) (VendorOperationResult, error)
	PreviewVendorOperations(ctx context.Context, op VendorOperation) (VendorPreview, error)

	CreateModelMeta(ctx context.Context, in ModelMetaInput) (ModelMeta, error)
	GetModelMeta(ctx context.Context, id int64) (ModelMeta, error)
	UpdateModelMeta(ctx context.Context, id int64, patch ModelMetaPatch, expectUpdatedAt time.Time) (ModelMeta, error)
	DeleteModelMeta(ctx context.Context, id int64) error
	ListModelMeta(ctx context.Context, filter ModelMetaFilter) ([]ModelMeta, int64, error)
	SearchModelMeta(ctx context.Context, keyword string, limit int) ([]ModelMeta, error)
	MissingModels(ctx context.Context) (MissingResult, error)
	BatchDeleteModelMeta(ctx context.Context, ids []int64, removeFromChannels, clearPricing bool) (BatchDeleteResult, error)
	ResolveMeta(ctx context.Context, modelNames []string) (map[string]ModelMeta, error)
}

// SQLModelCatalogRepo 是 ModelCatalogRepo 的 PostgreSQL 实现。
type SQLModelCatalogRepo struct {
	db *sql.DB
}

// NewModelCatalogRepo 构造目录仓储。db 必须非 nil：目录没有内存兜底，
// 静默返回空列表会把「数据库故障」伪装成「暂无档案」。
func NewModelCatalogRepo(db *sql.DB) *SQLModelCatalogRepo {
	return &SQLModelCatalogRepo{db: db}
}

// ---------------------------------------------------------------------------
// vendors
// ---------------------------------------------------------------------------

const vendorColumns = `id, name, description, icon, status, created_at, updated_at`

// Version 派生供应商乐观锁版本（D1-01 / platform-01 §22.7.2）。
//
// 口径：对 id/name/description/icon/status/created_at/updated_at 规范化后取
// sha256 小写 hex。规范化使用「字段前缀 + NUL 分隔」，NULL 与空串用不同标记，
// 避免 description=NULL 与 description="" 派生出同一版本（两者内容不同）。
// 时间统一转 UTC + RFC3339Nano，保证跨进程/跨时区稳定。
func (v Vendor) Version() string {
	var b strings.Builder
	b.WriteString("vendor\x00")
	b.WriteString(strconv.FormatInt(v.ID, 10))
	b.WriteByte(0)
	writeVersionString(&b, v.Name)
	writeVersionOptional(&b, v.Description)
	writeVersionOptional(&b, v.Icon)
	b.WriteString(strconv.FormatInt(int64(v.Status), 10))
	b.WriteByte(0)
	// 时间戳是版本输入：任何 updated_at 变化都必须改变版本，即使业务列未变。
	b.WriteString(v.CreatedAt.UTC().Format(time.RFC3339Nano))
	b.WriteByte(0)
	b.WriteString(v.UpdatedAt.UTC().Format(time.RFC3339Nano))
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func writeVersionString(b *strings.Builder, value string) {
	b.WriteString("s:")
	b.WriteString(value)
	b.WriteByte(0)
}

func writeVersionOptional(b *strings.Builder, value *string) {
	if value == nil {
		b.WriteString("n")
		b.WriteByte(0)
		return
	}
	writeVersionString(b, *value)
}

func scanVendor(row interface{ Scan(...any) error }) (Vendor, error) {
	var v Vendor
	err := row.Scan(&v.ID, &v.Name, &v.Description, &v.Icon, &v.Status, &v.CreatedAt, &v.UpdatedAt)
	return v, err
}

// scanVendorWithCount 读取 vendorColumns 末尾多出的 model_count 列。
func scanVendorWithCount(row interface{ Scan(...any) error }) (Vendor, error) {
	var v Vendor
	err := row.Scan(&v.ID, &v.Name, &v.Description, &v.Icon, &v.Status,
		&v.CreatedAt, &v.UpdatedAt, &v.ModelCount)
	return v, err
}

const vendorModelCountExpr = `(SELECT count(*) FROM model_meta m WHERE m.vendor_id = v.id)`

// CreateVendor 插入供应商。名称先裁剪首尾空白再校验非空（L-4），落库为裁剪
// 后的值：`" Acme "` 与 `"Acme"` 是同一个名字，否则 lower(name) 唯一索引会把
// 它们当成两个供应商。大小写不敏感的唯一性由表达式索引兜底并映射为
// ErrVendorDuplicate。
func (r *SQLModelCatalogRepo) CreateVendor(ctx context.Context, in VendorInput) (Vendor, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return Vendor{}, ErrVendorInvalid
	}
	status := int16(1)
	if in.Status != nil {
		status = *in.Status
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO vendors (name, description, icon, status)
		VALUES ($1, $2, $3, $4)
		RETURNING `+vendorColumns,
		name, in.Description, in.Icon, status)
	v, err := scanVendor(row)
	if err == nil {
		return v, nil
	}
	if isUniqueViolation(err) {
		return Vendor{}, ErrVendorDuplicate
	}
	if isCheckViolation(err) {
		return Vendor{}, ErrVendorInvalid
	}
	return Vendor{}, fmt.Errorf("store: create vendor(%s): %w", name, err)
}

// GetVendor 读取供应商（含模型数）。
func (r *SQLModelCatalogRepo) GetVendor(ctx context.Context, id int64) (Vendor, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT v.id, v.name, v.description, v.icon, v.status, v.created_at, v.updated_at, `+
			vendorModelCountExpr+` FROM vendors v WHERE v.id = $1`, id)
	v, err := scanVendorWithCount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Vendor{}, ErrVendorNotFound
	}
	if err != nil {
		return Vendor{}, fmt.Errorf("store: get vendor(%d): %w", id, err)
	}
	return v, nil
}

// UpdateVendor 全量更新供应商，并以派生 version 做乐观锁。
//
// 【为什么在事务里先 SELECT ... FOR UPDATE】version 不在行上，无法用
// `WHERE version = $n` 一步完成。先锁行再读当前版本，才能保证「比较 → 写入」
// 之间没有并发提交；否则两个并发更新可能都读到旧版本而都成功。
func (r *SQLModelCatalogRepo) UpdateVendor(ctx context.Context, id int64, in VendorUpdate, expectVersion string) (Vendor, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return Vendor{}, ErrVendorInvalid
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Vendor{}, fmt.Errorf("store: begin update vendor(%d): %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := scanVendor(tx.QueryRowContext(ctx,
		`SELECT `+vendorColumns+` FROM vendors WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Vendor{}, ErrVendorNotFound
	}
	if err != nil {
		return Vendor{}, fmt.Errorf("store: lock vendor(%d): %w", id, err)
	}
	// 空版本一律视为冲突（F-06-24 的「空版本亦视为冲突」同口径）：客户端必须
	// 先读取并回传版本，不能省略校验。
	if expectVersion == "" || current.Version() != expectVersion {
		return Vendor{}, ErrVendorConflict
	}

	updated, err := scanVendor(tx.QueryRowContext(ctx, `
		UPDATE vendors SET name = $1, description = $2, icon = $3, status = $4, updated_at = now()
		WHERE id = $5
		RETURNING `+vendorColumns,
		name, in.Description, in.Icon, in.Status, id))
	if err != nil {
		if isUniqueViolation(err) {
			return Vendor{}, ErrVendorDuplicate
		}
		if isCheckViolation(err) {
			return Vendor{}, ErrVendorInvalid
		}
		return Vendor{}, fmt.Errorf("store: update vendor(%d): %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return Vendor{}, fmt.Errorf("store: commit update vendor(%d): %w", id, err)
	}
	return updated, nil
}

// DeleteVendor 删除供应商，先在同一事务内做引用保护（D1-07）。
//
// 【为什么统计全部行】停用的模型档案同样是引用；只数 status=1 会让「先停用
// 模型再删供应商」留下悬挂的 vendor_id，公开面/管理面随后读到不存在的供应商。
func (r *SQLModelCatalogRepo) DeleteVendor(ctx context.Context, id int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin delete vendor(%d): %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	var locked int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM vendors WHERE id = $1 FOR UPDATE`, id).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrVendorNotFound
	}
	if err != nil {
		return fmt.Errorf("store: lock vendor(%d): %w", id, err)
	}
	var refs int64
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM model_meta WHERE vendor_id = $1`, id).Scan(&refs); err != nil {
		return fmt.Errorf("store: count models of vendor(%d): %w", id, err)
	}
	if refs > 0 {
		return ErrVendorReferenced{ReferenceCount: refs}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM vendors WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: delete vendor(%d): %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit delete vendor(%d): %w", id, err)
	}
	return nil
}

func vendorWhere(filter VendorFilter) (string, []any) {
	where := " WHERE 1=1"
	args := make([]any, 0, 4)
	if keyword := strings.TrimSpace(filter.Keyword); keyword != "" {
		args = append(args, "%"+escapeLike(keyword)+"%")
		where += fmt.Sprintf(` AND (v.name ILIKE $%d ESCAPE '!' OR COALESCE(v.description, '') ILIKE $%d ESCAPE '!')`,
			len(args), len(args))
	}
	if filter.Status != nil {
		args = append(args, *filter.Status)
		where += fmt.Sprintf(" AND v.status = $%d", len(args))
	}
	if filter.Attached != nil {
		if *filter.Attached {
			where += " AND EXISTS (SELECT 1 FROM model_meta m WHERE m.vendor_id = v.id)"
		} else {
			where += " AND NOT EXISTS (SELECT 1 FROM model_meta m WHERE m.vendor_id = v.id)"
		}
	}
	return where, args
}

// ListVendors 返回 offset 分页列表；total 与 items 共用同一 WHERE，避免
// 筛选条件在计数/取页两处漂移。排序固定 lower(name)：名称比较大小写不敏感。
//
// 分页参数在入口 clamp（L-2）：page<1→1、page_size<1→20、page_size>100→100，
// 与分页契约一致；负数 offset 会让 SQL 报错，超大 page_size 会把一次列表
// 放大成整表扫描，两者都应是「按契约纠正」而不是 500。
func (r *SQLModelCatalogRepo) ListVendors(ctx context.Context, filter VendorFilter) ([]Vendor, int64, error) {
	filter.Page, filter.PageSize = clampCatalogPage(filter.Page, filter.PageSize)
	where, args := vendorWhere(filter)

	var total int64
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM vendors v`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count vendors: %w", err)
	}

	offset := (filter.Page - 1) * filter.PageSize
	query := `SELECT v.id, v.name, v.description, v.icon, v.status, v.created_at, v.updated_at, ` +
		vendorModelCountExpr + ` FROM vendors v` + where +
		` ORDER BY lower(v.name) ASC, v.id ASC` +
		fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
	rows, err := r.db.QueryContext(ctx, query, append(args, filter.PageSize, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list vendors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]Vendor, 0, filter.PageSize)
	for rows.Next() {
		v, scanErr := scanVendorWithCount(rows)
		if scanErr != nil {
			return nil, 0, fmt.Errorf("store: scan vendor: %w", scanErr)
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: iterate vendors: %w", err)
	}
	return items, total, nil
}

// SearchVendors 是选择器用的轻量搜索（列表语义的关键字，带 limit）。
func (r *SQLModelCatalogRepo) SearchVendors(ctx context.Context, keyword string, limit int) ([]Vendor, error) {
	limit = clampCatalogLimit(limit)
	query := `SELECT v.id, v.name, v.description, v.icon, v.status, v.created_at, v.updated_at, ` +
		vendorModelCountExpr + ` FROM vendors v`
	args := make([]any, 0, 2)
	if kw := strings.TrimSpace(keyword); kw != "" {
		args = append(args, "%"+escapeLike(kw)+"%")
		query += fmt.Sprintf(` WHERE (v.name ILIKE $%d ESCAPE '!' OR COALESCE(v.description, '') ILIKE $%d ESCAPE '!')`,
			len(args), len(args))
	}
	args = append(args, limit)
	query += ` ORDER BY lower(v.name) ASC, v.id ASC` + fmt.Sprintf(" LIMIT $%d", len(args))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search vendors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]Vendor, 0, limit)
	for rows.Next() {
		v, scanErr := scanVendorWithCount(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store: scan vendor: %w", scanErr)
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate vendors: %w", err)
	}
	return items, nil
}

// vendorOperationVendorIDs 校验选择集并返回本次操作涉及的全部供应商 id
// （去重、升序）：target（assign/merge）∪ sources（merge/delete）。
//
// 【哨兵优先级】空选择 → 超限 → 参数组合非法：选择集错误与版本冲突无关，
// 必须在加锁/比对版本之前返回，否则客户端会拿到误导性的 409。
func vendorOperationVendorIDs(op VendorOperation) ([]int64, error) {
	switch op.Action {
	case VendorActionAssign:
		modelIDs := dedupeIDs(op.ModelIDs)
		if len(modelIDs) == 0 {
			return nil, ErrCatalogEmptySelection
		}
		if len(modelIDs) > maxCatalogBatch {
			return nil, ErrCatalogTooLarge
		}
		// target=0 是「清空归属」，没有需要校验版本的供应商行。
		if op.TargetVendorID == 0 {
			return nil, nil
		}
		return []int64{op.TargetVendorID}, nil
	case VendorActionMerge:
		sources := dedupeIDs(op.VendorIDs)
		if len(sources) == 0 {
			return nil, ErrCatalogEmptySelection
		}
		if len(sources) > maxCatalogBatch {
			return nil, ErrCatalogTooLarge
		}
		if op.TargetVendorID == 0 {
			return nil, ErrVendorOperationInvalid
		}
		for _, source := range sources {
			// 源 = 目标会让「改挂后删源」把目标一起删掉；必须先拒绝。
			if source == op.TargetVendorID {
				return nil, ErrVendorOperationInvalid
			}
		}
		return sortedIDs(append(sources, op.TargetVendorID)), nil
	case VendorActionDelete:
		ids := dedupeIDs(op.VendorIDs)
		if len(ids) == 0 {
			return nil, ErrCatalogEmptySelection
		}
		if len(ids) > maxCatalogBatch {
			return nil, ErrCatalogTooLarge
		}
		return sortedIDs(ids), nil
	default:
		return nil, ErrVendorOperationInvalid
	}
}

// ApplyVendorOperations 在单事务内执行 assign/merge/delete（D1-08）。
//
// 【锁顺序】先对涉及的全部 vendor 行按 id 升序 `FOR UPDATE`，再重算操作指纹
// 并比对 expected_version。固定升序避免两个并发 merge（A→B 与 B→A）互相等待
// 成死锁；先锁后比保证「读版本 → 比对 → 写入」之间没有并发提交。
// expected_version 缺失或过期时整体回滚，不产生任何写入（M-1）。
//
// 【为什么 assign 也锁 target 行】assign 只引用目标供应商，但目标可能被并发
// 删除或改名：同一把行锁同时保护「存在性」与「版本一致性」，比 FOR SHARE
// 更简单且不损失正确性（assign 之间本来就要串行化同一目标）。
func (r *SQLModelCatalogRepo) ApplyVendorOperations(ctx context.Context, op VendorOperation) (VendorOperationResult, error) {
	involved, err := vendorOperationVendorIDs(op)
	if err != nil {
		return VendorOperationResult{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return VendorOperationResult{}, fmt.Errorf("store: begin vendor operation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	vendors, err := lockVendorRows(ctx, tx, involved)
	if err != nil {
		return VendorOperationResult{}, err
	}
	if err := verifyVendorVersion(op.ExpectedVersion, vendors); err != nil {
		return VendorOperationResult{}, err
	}

	var affected int64
	switch op.Action {
	case VendorActionAssign:
		affected, err = applyVendorAssign(ctx, tx, op)
	case VendorActionMerge:
		affected, err = applyVendorMerge(ctx, tx, op)
	case VendorActionDelete:
		affected, err = applyVendorDelete(ctx, tx, op)
	}
	if err != nil {
		return VendorOperationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return VendorOperationResult{}, fmt.Errorf("store: commit vendor operation(%s): %w", op.Action, err)
	}
	return VendorOperationResult{Affected: affected}, nil
}

// PreviewVendorOperations 只读地返回影响行数、当前操作指纹与（可选）冲突集，
// 供 D1b 的 preview 端点调用。
//
// 不加锁：preview 是建议性的，最终一致性由 apply 在同一事务内重算指纹保证。
// Conflicts 只在调用方主动传了 ExpectedVersion 时计算——首次 preview 的客户端
// 还没有指纹，把「未传」报告成「全部冲突」只会制造噪音。
func (r *SQLModelCatalogRepo) PreviewVendorOperations(ctx context.Context, op VendorOperation) (VendorPreview, error) {
	involved, err := vendorOperationVendorIDs(op)
	if err != nil {
		return VendorPreview{}, err
	}
	vendors, err := r.loadVendorVersions(ctx, involved)
	if err != nil {
		return VendorPreview{}, err
	}

	preview := VendorPreview{
		ExpectedVersion: vendorVersionsFingerprint(vendors),
		Vendors:         make([]VendorVersionRef, 0, len(vendors)),
	}
	for _, v := range vendors {
		preview.Vendors = append(preview.Vendors, VendorVersionRef{VendorID: v.ID, Version: v.Version()})
	}
	if op.ExpectedVersion != "" && op.ExpectedVersion != preview.ExpectedVersion {
		preview.Conflicts = vendorVersionConflicts(vendors)
	}

	switch op.Action {
	case VendorActionAssign:
		err = r.db.QueryRowContext(ctx,
			`SELECT count(*) FROM model_meta WHERE id = ANY($1::bigint[])`,
			dedupeIDs(op.ModelIDs)).Scan(&preview.Affected)
		if err != nil {
			return VendorPreview{}, fmt.Errorf("store: preview assign: %w", err)
		}
	case VendorActionMerge:
		err = r.db.QueryRowContext(ctx,
			`SELECT count(*) FROM model_meta WHERE vendor_id = ANY($1::bigint[])`,
			sortedIDs(dedupeIDs(op.VendorIDs))).Scan(&preview.Affected)
		if err != nil {
			return VendorPreview{}, fmt.Errorf("store: preview merge: %w", err)
		}
	case VendorActionDelete:
		// 供应商行已在上面的加锁读里断言存在，数量即被删行数。
		preview.Affected = int64(len(vendors))
	}
	return preview, nil
}

// loadVendorVersions 只读加载供应商行（id 升序），用于 preview 与指纹重算。
func (r *SQLModelCatalogRepo) loadVendorVersions(ctx context.Context, ids []int64) ([]Vendor, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+vendorColumns+` FROM vendors WHERE id = ANY($1::bigint[]) ORDER BY id`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: load vendor versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanVendors(rows, len(ids))
}

// lockVendorRows 按 id 升序 `FOR UPDATE` 锁定供应商行，并断言全部存在。
func lockVendorRows(ctx context.Context, tx *sql.Tx, ids []int64) ([]Vendor, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT `+vendorColumns+` FROM vendors WHERE id = ANY($1::bigint[]) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: lock vendors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanVendors(rows, len(ids))
}

func scanVendors(rows *sql.Rows, want int) ([]Vendor, error) {
	vendors := make([]Vendor, 0, want)
	for rows.Next() {
		v, err := scanVendor(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan vendor: %w", err)
		}
		vendors = append(vendors, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate vendors: %w", err)
	}
	// 少一行即至少一个 id 不存在：与单行 Get 同口径返回 NotFound。
	if len(vendors) != want {
		return nil, ErrVendorNotFound
	}
	return vendors, nil
}

// vendorVersionsFingerprint 实现冻结的操作指纹（见 VendorOperation 注释）：
// 对 id 升序的 `<id>=<Version()>` 以换行连接后取 sha256 小写 hex。
// 空集合也返回 sha256("")，客户端必须先 preview 才能拿到该值。
func vendorVersionsFingerprint(vendors []Vendor) string {
	sorted := make([]Vendor, len(vendors))
	copy(sorted, vendors)
	slices.SortFunc(sorted, func(a, b Vendor) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		default:
			return 0
		}
	})
	var b strings.Builder
	for i, v := range sorted {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(strconv.FormatInt(v.ID, 10))
		b.WriteByte('=')
		b.WriteString(v.Version())
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// verifyVendorVersion 比对调用方回传的指纹与事务内重算的指纹。
//
// 空 ExpectedVersion 一律视为冲突（客户端必须 preview）；指纹不一致时返回
// 全部涉及供应商的当前版本，供客户端定位并重新 preview。
func verifyVendorVersion(expected string, vendors []Vendor) error {
	if expected != "" && expected == vendorVersionsFingerprint(vendors) {
		return nil
	}
	return ErrVendorVersionConflict{Conflicts: vendorVersionConflicts(vendors)}
}

func vendorVersionConflicts(vendors []Vendor) []VendorVersionConflict {
	conflicts := make([]VendorVersionConflict, 0, len(vendors))
	for _, v := range vendors {
		conflicts = append(conflicts, VendorVersionConflict{VendorID: v.ID, CurrentVersion: v.Version()})
	}
	return conflicts
}

// sortedIDs 返回升序副本：所有多行写入都按同一顺序加锁，消除重叠集合的死锁面。
func sortedIDs(ids []int64) []int64 {
	out := append([]int64(nil), ids...)
	slices.Sort(out)
	return out
}

// setStringPtr：字段未被 Set 时返回 nil，让校验函数按「未提交 ⇒ 跳过」处理。
func setStringPtr(set bool, value *string) *string {
	if !set {
		return nil
	}
	return value
}

func setInt64Ptr(set bool, value *int64) *int64 {
	if !set {
		return nil
	}
	return value
}

func applyVendorAssign(ctx context.Context, tx *sql.Tx, op VendorOperation) (int64, error) {
	// target_vendor_id = 0 表示清空归属（D1-08）；NULL 而不是 0 落库。
	target := nullableInt64Value(op.TargetVendorID)
	var affected int64
	// 逐条按 id 升序 UPDATE（L-8）：单条 UPDATE ... WHERE id = ANY(...) 的行锁
	// 顺序由执行器决定，与并发操作的重叠集合交叉时仍可能形成环；显式升序
	// 让所有写路径共享同一个加锁序。
	for _, id := range sortedIDs(dedupeIDs(op.ModelIDs)) {
		result, err := tx.ExecContext(ctx,
			`UPDATE model_meta SET vendor_id = $1, updated_at = now() WHERE id = $2`, target, id)
		if err != nil {
			return 0, fmt.Errorf("store: assign model(%d) to vendor(%d): %w", id, op.TargetVendorID, err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("store: assign model(%d) rows: %w", id, err)
		}
		// 选中的 id 必须全部存在：preview 与实际影响集合一致是 F-06-24 的验收项，
		// 静默跳过不存在的 id 会让两者产生差异。
		if n == 0 {
			return 0, ErrModelMetaNotFound
		}
		affected += n
	}
	return affected, nil
}

func applyVendorMerge(ctx context.Context, tx *sql.Tx, op VendorOperation) (int64, error) {
	target := op.TargetVendorID
	sources := sortedIDs(dedupeIDs(op.VendorIDs))

	// 先按 id 升序取出全部待改挂模型，再逐条 UPDATE（L-8）：多条源供应商的
	// 集合可能重叠，升序单行写入与 assign 共享同一加锁序。
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM model_meta WHERE vendor_id = ANY($1::bigint[]) ORDER BY id`, sources)
	if err != nil {
		return 0, fmt.Errorf("store: select models of merged vendors: %w", err)
	}
	modelIDs := make([]int64, 0, 64)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("store: scan merged model id: %w", err)
		}
		modelIDs = append(modelIDs, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("store: iterate merged model ids: %w", err)
	}
	_ = rows.Close()

	for _, id := range modelIDs {
		if _, err := tx.ExecContext(ctx,
			`UPDATE model_meta SET vendor_id = $1, updated_at = now() WHERE id = $2`, target, id); err != nil {
			return 0, fmt.Errorf("store: merge model(%d) into vendor(%d): %w", id, target, err)
		}
	}
	for _, source := range sources {
		if _, err := tx.ExecContext(ctx, `DELETE FROM vendors WHERE id = $1`, source); err != nil {
			return 0, fmt.Errorf("store: delete merged vendor(%d): %w", source, err)
		}
	}
	return int64(len(modelIDs)), nil
}

func applyVendorDelete(ctx context.Context, tx *sql.Tx, op VendorOperation) (int64, error) {
	ids := sortedIDs(dedupeIDs(op.VendorIDs))
	// delete 仅允许零引用（D1-08）；有引用时返回计数让前端提示走 merge。
	var refs int64
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM model_meta WHERE vendor_id = ANY($1::bigint[])`, ids).Scan(&refs); err != nil {
		return 0, fmt.Errorf("store: count models of vendors: %w", err)
	}
	if refs > 0 {
		return 0, ErrVendorReferenced{ReferenceCount: refs}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM vendors WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return 0, fmt.Errorf("store: delete vendors: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete vendors rows: %w", err)
	}
	return deleted, nil
}

// ---------------------------------------------------------------------------
// model_meta
// ---------------------------------------------------------------------------

const modelMetaColumns = `id, model_name, display_name, description, icon, tags,
	vendor_id, endpoints, capabilities, modalities, parameters, context_length,
	max_output, knowledge_cutoff, released_at, status, sync_official, name_rule,
	created_at, updated_at`

func scanModelMeta(row interface{ Scan(...any) error }) (ModelMeta, error) {
	var m ModelMeta
	err := row.Scan(
		&m.ID, &m.ModelName, &m.DisplayName, &m.Description, &m.Icon, &m.Tags,
		&m.VendorID, &m.Endpoints, &m.Capabilities, &m.Modalities, &m.Parameters,
		&m.ContextLength, &m.MaxOutput, &m.KnowledgeCutoff, &m.ReleasedAt,
		&m.Status, &m.SyncOfficial, &m.NameRule, &m.CreatedAt, &m.UpdatedAt,
	)
	return m, err
}

// defaultJSONBytes 把 nil 的空容器替换为列默认值。
//
// jsonb 参数为 nil 会被驱动写成 SQL NULL，而四列都是 NOT NULL；用空字节切片
// 也会被当作空 JSON 文本（非法）。这里显式给出 '[]' / '{}'，与 DDL 默认一致。
func defaultJSONBytes(value []byte, fallback string) []byte {
	if len(value) == 0 {
		return []byte(fallback)
	}
	return value
}

// isInvalidTextRepresentation 识别 JSON/数值解析失败（SQLSTATE 22P02）。
// 应用层把这类错误与 CHECK 违反（23514）一并映射为可读的 400。
func isInvalidTextRepresentation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// validateVendorReference 在同一事务内以 FOR SHARE 锁定被引用的供应商。
//
// 与 model_meta 的写入放在同一事务：没有 FK 时，否则会出现「校验存在 →
// 供应商被删 → 档案写入成功」的悬挂引用。
func validateVendorReference(ctx context.Context, tx *sql.Tx, vendorID int64) error {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM vendors WHERE id = $1 FOR SHARE`, vendorID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrVendorNotFound
	}
	if err != nil {
		return fmt.Errorf("store: check vendor(%d): %w", vendorID, err)
	}
	return nil
}

// CreateModelMeta 写入模型档案。model_name 按规范首尾裁剪空白（§22.7.1），
// 其余字段先过 ValidateModelMetaInput（L-3：规则下沉到 store，任何写入路径
// 都绕不过去）。
func (r *SQLModelCatalogRepo) CreateModelMeta(ctx context.Context, in ModelMetaInput) (ModelMeta, error) {
	name := strings.TrimSpace(in.ModelName)
	if name == "" {
		return ModelMeta{}, ErrModelMetaInvalid{Field: "model_name", Reason: "must not be blank"}
	}
	status := int16(1)
	if in.Status != nil {
		status = *in.Status
	}
	syncOfficial := true
	if in.SyncOfficial != nil {
		syncOfficial = *in.SyncOfficial
	}

	// 先落到「实际会写库」的形态再校验：nil 的 jsonb 值在插入时换成列默认值，
	// 校验必须看到同一份内容（否则会放过 nil → '[]' 之外的差异）。
	tags := defaultJSONBytes(in.Tags, "[]")
	endpoints := defaultJSONBytes(in.Endpoints, "{}")
	capabilities := defaultJSONBytes(in.Capabilities, "[]")
	modalities := defaultJSONBytes(in.Modalities, "[]")
	parameters := defaultJSONBytes(in.Parameters, "[]")
	if err := ValidateModelMetaInput(&ModelMeta{
		ModelName:       name,
		DisplayName:     in.DisplayName,
		Description:     in.Description,
		Tags:            tags,
		Endpoints:       endpoints,
		Capabilities:    capabilities,
		Modalities:      modalities,
		Parameters:      parameters,
		ContextLength:   in.ContextLength,
		MaxOutput:       in.MaxOutput,
		KnowledgeCutoff: in.KnowledgeCutoff,
	}); err != nil {
		return ModelMeta{}, err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelMeta{}, fmt.Errorf("store: begin create model_meta(%s): %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if in.VendorID != nil {
		if err := validateVendorReference(ctx, tx, *in.VendorID); err != nil {
			return ModelMeta{}, err
		}
	}

	rec, err := scanModelMeta(tx.QueryRowContext(ctx, `
		INSERT INTO model_meta
			(model_name, display_name, description, icon, tags, vendor_id,
			 endpoints, capabilities, modalities, parameters, context_length,
			 max_output, knowledge_cutoff, released_at, status, sync_official, name_rule)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		RETURNING `+modelMetaColumns,
		name, in.DisplayName, in.Description, in.Icon,
		tags, in.VendorID,
		endpoints, capabilities, modalities, parameters,
		in.ContextLength, in.MaxOutput, in.KnowledgeCutoff, in.ReleasedAt,
		status, syncOfficial, in.NameRule))
	if err != nil {
		switch {
		case isUniqueViolation(err):
			return ModelMeta{}, ErrModelMetaDuplicate
		case isCheckViolation(err), isInvalidTextRepresentation(err):
			return ModelMeta{}, ErrModelMetaInvalid{}
		}
		return ModelMeta{}, fmt.Errorf("store: create model_meta(%s): %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return ModelMeta{}, fmt.Errorf("store: commit create model_meta(%s): %w", name, err)
	}
	return rec, nil
}

// GetModelMeta 按代理 id 读取模型档案。
func (r *SQLModelCatalogRepo) GetModelMeta(ctx context.Context, id int64) (ModelMeta, error) {
	rec, err := scanModelMeta(r.db.QueryRowContext(ctx,
		`SELECT `+modelMetaColumns+` FROM model_meta WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ModelMeta{}, ErrModelMetaNotFound
	}
	if err != nil {
		return ModelMeta{}, fmt.Errorf("store: get model_meta(%d): %w", id, err)
	}
	return rec, nil
}

// UpdateModelMeta 部分更新模型档案，以 updated_at 做乐观锁。
//
// 只校验「本次提交了的」字段：nil/未 Set 表示不修改，交给 DB 保留原值。
func (r *SQLModelCatalogRepo) UpdateModelMeta(ctx context.Context, id int64, patch ModelMetaPatch, expectUpdatedAt time.Time) (ModelMeta, error) {
	var trimmedName *string
	if patch.ModelName != nil {
		name := strings.TrimSpace(*patch.ModelName)
		if name == "" {
			return ModelMeta{}, ErrModelMetaInvalid{Field: "model_name", Reason: "must not be blank"}
		}
		trimmedName = &name
	}
	// 与下面 SET 构造保持同一份取值：TagsSet + nil 会写列默认值 '[]'。
	var (
		tags         []byte
		endpoints    []byte
		capabilities []byte
		modalities   []byte
		parameters   []byte
	)
	if patch.TagsSet {
		tags = defaultJSONBytes(patch.Tags, "[]")
	}
	if patch.EndpointsSet {
		endpoints = defaultJSONBytes(patch.Endpoints, "{}")
	}
	if patch.CapabilitiesSet {
		capabilities = defaultJSONBytes(patch.Capabilities, "[]")
	}
	if patch.ModalitiesSet {
		modalities = defaultJSONBytes(patch.Modalities, "[]")
	}
	if patch.ParametersSet {
		parameters = defaultJSONBytes(patch.Parameters, "[]")
	}
	if err := ValidateModelMetaInput(&ModelMeta{
		DisplayName:     setStringPtr(patch.DisplayNameSet, patch.DisplayName),
		Description:     setStringPtr(patch.DescriptionSet, patch.Description),
		Tags:            tags,
		Endpoints:       endpoints,
		Capabilities:    capabilities,
		Modalities:      modalities,
		Parameters:      parameters,
		ContextLength:   setInt64Ptr(patch.ContextLengthSet, patch.ContextLength),
		MaxOutput:       setInt64Ptr(patch.MaxOutputSet, patch.MaxOutput),
		KnowledgeCutoff: setStringPtr(patch.KnowledgeCutoffSet, patch.KnowledgeCutoff),
	}); err != nil {
		return ModelMeta{}, err
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelMeta{}, fmt.Errorf("store: begin update model_meta(%d): %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	// 改挂供应商时同样用 FOR SHARE 堵住「校验存在 → 供应商被删」的窗口。
	if patch.VendorIDSet && patch.VendorID != nil {
		if err := validateVendorReference(ctx, tx, *patch.VendorID); err != nil {
			return ModelMeta{}, err
		}
	}

	sets := make([]string, 0, 16)
	args := make([]any, 0, 18)
	add := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if patch.ModelName != nil {
		add("model_name", *trimmedName)
	}
	if patch.DisplayNameSet {
		add("display_name", patch.DisplayName)
	}
	if patch.DescriptionSet {
		add("description", patch.Description)
	}
	if patch.IconSet {
		add("icon", patch.Icon)
	}
	if patch.TagsSet {
		add("tags", tags)
	}
	if patch.VendorIDSet {
		add("vendor_id", patch.VendorID)
	}
	if patch.EndpointsSet {
		add("endpoints", endpoints)
	}
	if patch.CapabilitiesSet {
		add("capabilities", capabilities)
	}
	if patch.ModalitiesSet {
		add("modalities", modalities)
	}
	if patch.ParametersSet {
		add("parameters", parameters)
	}
	if patch.ContextLengthSet {
		add("context_length", patch.ContextLength)
	}
	if patch.MaxOutputSet {
		add("max_output", patch.MaxOutput)
	}
	if patch.KnowledgeCutoffSet {
		add("knowledge_cutoff", patch.KnowledgeCutoff)
	}
	if patch.ReleasedAtSet {
		add("released_at", nullableTime(patch.ReleasedAt))
	}
	if patch.Status != nil {
		add("status", *patch.Status)
	}
	if patch.SyncOfficial != nil {
		add("sync_official", *patch.SyncOfficial)
	}
	if patch.NameRule != nil {
		add("name_rule", *patch.NameRule)
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, id, expectUpdatedAt.UTC())

	query := `UPDATE model_meta SET ` + strings.Join(sets, ", ") +
		fmt.Sprintf(` WHERE id = $%d AND updated_at = $%d RETURNING `, len(args)-1, len(args)) +
		modelMetaColumns
	rec, err := scanModelMeta(tx.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		// 二次查询区分「行不存在（404）」与「版本过期（409）」。
		var exists bool
		if checkErr := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM model_meta WHERE id = $1)`, id).Scan(&exists); checkErr != nil {
			return ModelMeta{}, fmt.Errorf("store: check model_meta(%d): %w", id, checkErr)
		}
		if !exists {
			return ModelMeta{}, ErrModelMetaNotFound
		}
		return ModelMeta{}, ErrModelMetaConflict
	}
	if err != nil {
		switch {
		case isUniqueViolation(err):
			return ModelMeta{}, ErrModelMetaDuplicate
		case isCheckViolation(err), isInvalidTextRepresentation(err):
			return ModelMeta{}, ErrModelMetaInvalid{}
		}
		return ModelMeta{}, fmt.Errorf("store: update model_meta(%d): %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return ModelMeta{}, fmt.Errorf("store: commit update model_meta(%d): %w", id, err)
	}
	return rec, nil
}

// DeleteModelMeta 物理删除单条档案（目录不保留软删，§22.7.1）。
func (r *SQLModelCatalogRepo) DeleteModelMeta(ctx context.Context, id int64) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM model_meta WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete model_meta(%d): %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete model_meta(%d) rows: %w", id, err)
	}
	if affected == 0 {
		return ErrModelMetaNotFound
	}
	return nil
}

func modelMetaWhere(filter ModelMetaFilter) (string, []any) {
	where := " WHERE 1=1"
	args := make([]any, 0, 4)
	if keyword := strings.TrimSpace(filter.Keyword); keyword != "" {
		args = append(args, "%"+escapeLike(keyword)+"%")
		where += fmt.Sprintf(` AND (model_name ILIKE $%d ESCAPE '!' OR COALESCE(display_name, '') ILIKE $%d ESCAPE '!')`,
			len(args), len(args))
	}
	if filter.VendorID != nil {
		args = append(args, *filter.VendorID)
		where += fmt.Sprintf(" AND vendor_id = $%d", len(args))
	}
	if filter.Status != nil {
		args = append(args, *filter.Status)
		where += fmt.Sprintf(" AND status = $%d", len(args))
	}
	return where, args
}

// ListModelMeta 返回 offset 分页列表；total 与 items 共用同一 WHERE。
// 分页参数在入口 clamp（L-2），口径同 ListVendors。
func (r *SQLModelCatalogRepo) ListModelMeta(ctx context.Context, filter ModelMetaFilter) ([]ModelMeta, int64, error) {
	filter.Page, filter.PageSize = clampCatalogPage(filter.Page, filter.PageSize)
	where, args := modelMetaWhere(filter)

	var total int64
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM model_meta`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count model_meta: %w", err)
	}

	offset := (filter.Page - 1) * filter.PageSize
	query := `SELECT ` + modelMetaColumns + ` FROM model_meta` + where +
		` ORDER BY model_name ASC, id ASC` +
		fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
	rows, err := r.db.QueryContext(ctx, query, append(args, filter.PageSize, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list model_meta: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]ModelMeta, 0, filter.PageSize)
	for rows.Next() {
		rec, scanErr := scanModelMeta(rows)
		if scanErr != nil {
			return nil, 0, fmt.Errorf("store: scan model_meta: %w", scanErr)
		}
		items = append(items, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: iterate model_meta: %w", err)
	}
	return items, total, nil
}

// SearchModelMeta 是选择器用的轻量搜索。
func (r *SQLModelCatalogRepo) SearchModelMeta(ctx context.Context, keyword string, limit int) ([]ModelMeta, error) {
	limit = clampCatalogLimit(limit)
	query := `SELECT ` + modelMetaColumns + ` FROM model_meta`
	args := make([]any, 0, 2)
	if kw := strings.TrimSpace(keyword); kw != "" {
		args = append(args, "%"+escapeLike(kw)+"%")
		query += fmt.Sprintf(` WHERE (model_name ILIKE $%d ESCAPE '!' OR COALESCE(display_name, '') ILIKE $%d ESCAPE '!')`,
			len(args), len(args))
	}
	args = append(args, limit)
	query += ` ORDER BY model_name ASC, id ASC` + fmt.Sprintf(" LIMIT $%d", len(args))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search model_meta: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]ModelMeta, 0, limit)
	for rows.Next() {
		rec, scanErr := scanModelMeta(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store: scan model_meta: %w", scanErr)
		}
		items = append(items, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate model_meta: %w", err)
	}
	return items, nil
}

// MissingModels 返回「启用渠道在跑、但目录里没有精确档案」的模型（F-06-26）。
//
// 【数据源必须是 channels.supported_models 而不是 abilities】NEW TG 已裁掉
// abilities 表（G7-R34 派生路由）；用渠道的模型清单并集与 relay-01 §5.1 的
// 读面口径一致。
//
// 【缺失检测口径（严格版，M-3）】任一启用渠道的 supported_models 含非法元素
// （非字符串、空串、纯空白）⇒ **整渠道跳过**，其全部模型都不计入任何
// channel_count。理由：supported_models 是自由 JSON 数组，单个坏元素意味着
// 这份清单本身不可信；只跳过坏元素会让同一渠道的计数凭空少一项，用户看到
// 的「N 个渠道在用」会算错。跳过数通过 MissingResult.SkippedChannels 暴露，
// 管理员据此修复渠道配置后再复检。
//
// 纯空白用 POSIX `[[:space:]]` 判定（btrim 只认空格，会把 "\t" 当成合法名）。
func (r *SQLModelCatalogRepo) MissingModels(ctx context.Context) (MissingResult, error) {
	invalidChannel := `EXISTS (
		SELECT 1 FROM jsonb_array_elements(c.supported_models) AS elem
		WHERE jsonb_typeof(elem) <> 'string'
		   OR (elem #>> '{}') ~ '^[[:space:]]*$'
	)`

	var skipped int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM channels c WHERE c.status = 1 AND `+invalidChannel).Scan(&skipped); err != nil {
		return MissingResult{}, fmt.Errorf("store: count skipped channels: %w", err)
	}

	rows, err := r.db.QueryContext(ctx, `
		WITH channel_models AS (
			SELECT elem #>> '{}' AS model_name, count(DISTINCT c.id) AS channel_count
			FROM channels c
			CROSS JOIN LATERAL jsonb_array_elements(c.supported_models) AS elem
			WHERE c.status = 1 AND NOT `+invalidChannel+`
			GROUP BY elem #>> '{}'
		)
		SELECT cm.model_name, cm.channel_count,
		       EXISTS (SELECT 1 FROM model_pricing mp WHERE mp.model = cm.model_name)
		FROM channel_models cm
		WHERE NOT EXISTS (
			SELECT 1 FROM model_meta mm WHERE mm.model_name = cm.model_name
		)
		ORDER BY cm.channel_count DESC, cm.model_name ASC`)
	if err != nil {
		return MissingResult{}, fmt.Errorf("store: list missing models: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := MissingResult{Items: make([]MissingModel, 0, 32), SkippedChannels: skipped}
	for rows.Next() {
		var rec MissingModel
		if err := rows.Scan(&rec.ModelName, &rec.ChannelCount, &rec.HasPricing); err != nil {
			return MissingResult{}, fmt.Errorf("store: scan missing model: %w", err)
		}
		result.Items = append(result.Items, rec)
	}
	if err := rows.Err(); err != nil {
		return MissingResult{}, fmt.Errorf("store: iterate missing models: %w", err)
	}
	return result, nil
}

// BatchDeleteModelMeta 删除 1–1000 条档案，并可选地在同一事务内完成
// 「从渠道移除」与「清定价」（F-06-22）。
//
// 【removeFromChannels 只对 name_rule=0 生效】按前缀/后缀/包含去「移除」会把
// 一批本不属于该档案的模型一起删掉（F-06-22 的顺序即语义条款）。非精确规则
// 的档案即使被选中，也不会改动渠道清单。
//
// 【清定价按 model_name 精确匹配】model_pricing.model 是自然键；非精确规则
// 的档案名往往不是实际被路由的模型名，没有可安全删除的定价行。这是设计
// 语义的直接推论，不是遗漏。
//
// 【空渠道保护（H-1）】supported_models 有 channels_supported_models_chk
// （至少 1 个元素）。若移除后某启用渠道会变空，返回 ErrChannelModelsEmpty
// 并整体回滚——部分渠道被掏空、档案却没删的中间态比直接失败更难修复。
// DB CHECK 的 23514 作为兜底同样映射为该哨兵（覆盖停用渠道等预检未覆盖的
// 情形），绝不让数据层约束以 500 泄露给调用方。
//
// 【部分 id 不存在（L-5）】不报错：Requested 是去重后的选择集，Deleted 是
// 实际命中数；全部不存在才返回 NotFound（与单条删除同口径）。
func (r *SQLModelCatalogRepo) BatchDeleteModelMeta(ctx context.Context, ids []int64, removeFromChannels, clearPricing bool) (BatchDeleteResult, error) {
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return BatchDeleteResult{}, ErrCatalogEmptySelection
	}
	if len(ids) > maxCatalogBatch {
		return BatchDeleteResult{}, ErrCatalogTooLarge
	}
	result := BatchDeleteResult{Requested: int64(len(ids))}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return BatchDeleteResult{}, fmt.Errorf("store: begin batch delete model_meta: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT id, model_name, name_rule FROM model_meta WHERE id = ANY($1::bigint[]) FOR UPDATE`, ids)
	if err != nil {
		return BatchDeleteResult{}, fmt.Errorf("store: lock model_meta batch: %w", err)
	}
	var (
		allNames   []string
		exactNames []string
		lockedIDs  []int64
	)
	for rows.Next() {
		var (
			id       int64
			name     string
			nameRule int16
		)
		if err := rows.Scan(&id, &name, &nameRule); err != nil {
			_ = rows.Close()
			return BatchDeleteResult{}, fmt.Errorf("store: scan model_meta batch: %w", err)
		}
		allNames = append(allNames, name)
		if nameRule == 0 {
			exactNames = append(exactNames, name)
		}
		lockedIDs = append(lockedIDs, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return BatchDeleteResult{}, fmt.Errorf("store: iterate model_meta batch: %w", err)
	}
	_ = rows.Close()
	if len(lockedIDs) == 0 {
		// 全部 id 都不存在：与单条删除同口径返回 404，避免「删除成功但没删到
		// 任何东西」的误导性 204。部分不存在则按计数如实返回（L-5）。
		return BatchDeleteResult{}, ErrModelMetaNotFound
	}

	if removeFromChannels && len(exactNames) > 0 {
		emptyChannels, err := channelsEmptiedByRemoval(ctx, tx, exactNames)
		if err != nil {
			return BatchDeleteResult{}, err
		}
		if len(emptyChannels) > 0 {
			return BatchDeleteResult{}, ErrChannelModelsEmpty{ChannelIDs: emptyChannels}
		}

		// 重建数组而不是使用 jsonb 减法：`-` 删除的是**元素值**，对重复项只删
		// 第一个；重建能一次性去掉全部同名项，并保留非字符串元素。
		upd, err := tx.ExecContext(ctx, `
			UPDATE channels
			SET supported_models = COALESCE((
					SELECT jsonb_agg(elem)
					FROM jsonb_array_elements(supported_models) AS elem
					WHERE NOT (jsonb_typeof(elem) = 'string' AND elem #>> '{}' = ANY($1::text[]))
				), '[]'::jsonb),
				updated_at = now()
			WHERE EXISTS (
				SELECT 1 FROM jsonb_array_elements(supported_models) AS elem
				WHERE jsonb_typeof(elem) = 'string' AND elem #>> '{}' = ANY($1::text[])
			)`, exactNames)
		if err != nil {
			// 预检只覆盖启用渠道；停用渠道同样受 CHECK 约束，命中时按同一哨兵
			// 返回（此时事务已被 PG 标记为 aborted，无法再查出渠道 id）。
			if isCheckViolation(err) {
				return BatchDeleteResult{}, ErrChannelModelsEmpty{}
			}
			return BatchDeleteResult{}, fmt.Errorf("store: remove models from channels: %w", err)
		}
		if result.ChannelsUpdated, err = upd.RowsAffected(); err != nil {
			return BatchDeleteResult{}, fmt.Errorf("store: remove models from channels rows: %w", err)
		}
	}
	if clearPricing && len(allNames) > 0 {
		// 覆盖价先删（逻辑引用），再删源定价；与 DeleteModelPricing 的级联口径一致。
		priced, err := tx.ExecContext(ctx,
			`DELETE FROM model_pricing WHERE model = ANY($1::text[])`, allNames)
		if err != nil {
			return BatchDeleteResult{}, fmt.Errorf("store: clear model_pricing: %w", err)
		}
		if result.PricingDeleted, err = priced.RowsAffected(); err != nil {
			return BatchDeleteResult{}, fmt.Errorf("store: clear model_pricing rows: %w", err)
		}
		groups, err := tx.ExecContext(ctx,
			`DELETE FROM group_pricing WHERE model = ANY($1::text[])`, allNames)
		if err != nil {
			return BatchDeleteResult{}, fmt.Errorf("store: clear group_pricing: %w", err)
		}
		if result.GroupPricingDeleted, err = groups.RowsAffected(); err != nil {
			return BatchDeleteResult{}, fmt.Errorf("store: clear group_pricing rows: %w", err)
		}
	}
	deleted, err := tx.ExecContext(ctx, `DELETE FROM model_meta WHERE id = ANY($1::bigint[])`, lockedIDs)
	if err != nil {
		return BatchDeleteResult{}, fmt.Errorf("store: delete model_meta batch: %w", err)
	}
	if result.Deleted, err = deleted.RowsAffected(); err != nil {
		return BatchDeleteResult{}, fmt.Errorf("store: delete model_meta batch rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return BatchDeleteResult{}, fmt.Errorf("store: commit batch delete model_meta: %w", err)
	}
	return result, nil
}

// channelsEmptiedByRemoval 找出「移除 $1 之后 supported_models 会变空」的
// 启用渠道 id（升序）。
//
// 判定：不存在任何「不在移除集合里的元素」。非字符串元素会被上面的重建 SQL
// 保留，因此也计入「不是空」——与写入逻辑逐项对齐，避免预检与更新口径漂移。
func channelsEmptiedByRemoval(ctx context.Context, tx *sql.Tx, models []string) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id
		FROM channels c
		WHERE c.status = 1
		  AND NOT EXISTS (
			SELECT 1 FROM jsonb_array_elements(c.supported_models) AS elem
			WHERE NOT (jsonb_typeof(elem) = 'string' AND elem #>> '{}' = ANY($1::text[]))
		  )
		ORDER BY c.id`, models)
	if err != nil {
		return nil, fmt.Errorf("store: check emptied channels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan emptied channel: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate emptied channels: %w", err)
	}
	return ids, nil
}

// ResolveMeta 把「实际被路由的模型名」解析到目录档案（D1b 公开面用）。
//
// 命中优先级冻结为 精确(0) → 前缀(1) → 后缀(3) → 包含(2)，见 §22.7.1
// 「命中优先级」。同优先级内 tie-break 为
// `length(model_name) DESC, model_name ASC`：更长的模式更具体（`gpt-4o` 应
// 优先于 `gpt-4`），名字升序保证结果确定。
//
// 【为什么整表装载后在内存匹配】目录规模是百级；一次查询 + 内存匹配可以
// 完全确定性地测试优先级，也避免为每个请求名生成一组 LIKE 模式（模型名含
// `_`，LIKE 的 `_` 是通配符，必须转义，容易出安全/正确性缺口）。
// 只返回 status=1 的档案：停用档案对公开面不存在（广场态恒 hidden）。
//
// 【为什么用 strings.HasPrefix 而不是 SQL LIKE】见上：模型名合法字符集包含
// `_` 与 `%` 之外的 `_`；`_` 在 LIKE 中是单字符通配符，直接用会误命中。
func (r *SQLModelCatalogRepo) ResolveMeta(ctx context.Context, modelNames []string) (map[string]ModelMeta, error) {
	requested := make([]string, 0, len(modelNames))
	seen := make(map[string]struct{}, len(modelNames))
	for _, name := range modelNames {
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		requested = append(requested, name)
	}
	if len(requested) == 0 {
		return map[string]ModelMeta{}, nil
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT `+modelMetaColumns+` FROM model_meta
		 WHERE status = 1
		 ORDER BY length(model_name) DESC, model_name ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: load model_meta for resolve: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 按 name_rule 分桶；桶内保持 SQL 的「长度降序 + 名称升序」。
	byRule := map[int16][]ModelMeta{
		0: {}, 1: {}, 2: {}, 3: {},
	}
	for rows.Next() {
		rec, scanErr := scanModelMeta(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store: scan model_meta for resolve: %w", scanErr)
		}
		if _, ok := byRule[rec.NameRule]; ok {
			byRule[rec.NameRule] = append(byRule[rec.NameRule], rec)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate model_meta for resolve: %w", err)
	}

	// 冻结的优先级顺序：0 精确 → 1 前缀 → 3 后缀 → 2 包含。
	precedence := []int16{0, 1, 3, 2}
	resolved := make(map[string]ModelMeta, len(requested))
	for _, name := range requested {
		for _, rule := range precedence {
			matched := false
			for _, candidate := range byRule[rule] {
				if modelNameMatches(name, candidate.ModelName, rule) {
					resolved[name] = candidate
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
	}
	return resolved, nil
}

// modelNameMatches 判断目录模式是否命中实际路由名（rule 语义见 D1-02）。
func modelNameMatches(requested, pattern string, rule int16) bool {
	switch rule {
	case 0:
		return requested == pattern
	case 1:
		return strings.HasPrefix(requested, pattern)
	case 2:
		return strings.Contains(requested, pattern)
	case 3:
		return strings.HasSuffix(requested, pattern)
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// clampCatalogPage 按分页契约收敛列表参数（L-2）：page<1→1、page_size<1→20、
// page_size>100→100。负数 page 会算出负 offset 直接报错，超大 page_size 会把
// 一次列表放大成整表扫描——两者都应按契约纠正而不是 500。
func clampCatalogPage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}

// clampCatalogLimit 把选择器的 limit 收敛到 1–100（与分页契约的 page_size 上限
// 一致；L-2）：limit<=0 用 20，防止调用方把一次选择器查询放大成整表扫描。
func clampCatalogLimit(limit int) int {
	if limit <= 0 {
		return 20
	}
	if limit > 100 {
		return 100
	}
	return limit
}

// nullableInt64Value 把「0 = 清空归属」翻译为 SQL NULL。
func nullableInt64Value(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}
