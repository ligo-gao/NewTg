package store

// 公开定价目录的只读仓储（modules-09 F-09-14~F-09-22；M6c Task 7）。
//
// 【为什么单独一个仓储】公开目录只读 model_pricing/group_pricing/user_groups，
// 不返回任何内部字段（id/author/status/billing_expr/channel 等），也不做写入与
// 乐观锁。把它塞进 PricingRepo 会迫使管理面的每个测试假实现多实现三个只读方法；
// 公开面与计费热路径（pricing.DBSource）也保持互不影响的接缝。
//
// 【为什么整表装载而不是分页 SQL】
//  1. 计数、facet 计数、排序与分页必须来自**同一快照**（brief 的「计数与 items
//     同一快照」）：一次 REPEATABLE READ 事务内取回可见行最直接，也避免
//     COUNT(*) 与 OFFSET 查询在 READ COMMITTED 下看到不同的提交点。
//  2. facet 计数需要「忽略本组筛选后」的多个子集，逐 facet 下 SQL 会把一次
//     公开请求放大成 5~6 次往返；目录规模是「已上架模型数」（百级），内存计算
//     成本可接受。
//  3. 公开目录的分页/搜索/排序语义与 UI 强绑定，放在 HTTP 层可以完全确定性
//     地测试；SQL 层只负责「可见集合」这一条准入规则。
//
// 可见集合（R-PUB-01 + B6-11）：status = 1 且 billing_expr IS NULL。
// 表达式计价（tiered_expr）在 MVP 不公示：表达式自包含计价，目录没有可比较的
// 整数价；这与管理面 F-06-14「未登记模型不自动兜底」同口径——公示页只展示
// 可调用且价格可读的模型。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// PublicModelPricing 是公示页需要的模型定价投影。
//
// 四个价列保留指针：NULL=未设置（R-PUB-03：按 null 展示，不伪造兜底价），
// 0=管理员显式免费。外键/内部字段一律不取。
type PublicModelPricing struct {
	Model           string
	Mode            int16 // 0=per_token / 1=per_request（billing_expr 已在 WHERE 排除）
	PromptPrice     *int64
	CompletionPrice *int64
	CacheReadPrice  *int64
	CacheWritePrice *int64
	// Vendor 是 model_pricing.vendor（可空）。它是卡片供应商的**第二级**
	// 回退：档案关联的 vendors.name → 本列 → model_id 首段前缀（D1-16）。
	Vendor *string
	// Strategy 是结构化时段策略的 JSONB 原文；nil 表示没有策略。handler 用
	// pricingstrategy.Parse 归一后决定是否下发 pricing_schedule（启用时段
	// 才下发，未启用/无策略为 null，与旧响应形状一致）。
	Strategy []byte
}

// PublicVendor 是公开面需要的供应商投影。
//
// Status 一并取出（1=启用 / 2=停用）：档案 vendor_id 指向停用供应商时，
// 供应商名仍是模型的展示名——停用只影响管理面的可选性，不影响历史档案
// 的公开展示。这里不决定取舍，由 handler 按 D1-16 的三级回退使用。
type PublicVendor struct {
	ID     int64
	Name   string
	Status int16
}

// PublicGroup 是公开分组目录的投影（值/倍率/启用状态已由 SQL 过滤）。
type PublicGroup struct {
	Name  string
	Ratio int32 // 整数百分比，120 = 1.20x（modules-06 F-06-15）
}

// PublicGroupPricing 是分组覆盖价（绝对整数价，NOT NULL）。
type PublicGroupPricing struct {
	Group           string
	Model           string
	PromptPrice     int64
	CompletionPrice int64
	CacheReadPrice  int64
	CacheWritePrice int64
}

// PublicPricingCatalog 是一次快照读取的结果。
//
// Version 由五张表的 (max(updated_at), count(*), max(id)) 派生（F-09-14 的
// ETag 口径 + D1-22：元数据/供应商编辑也必须使版本变化，否则 304 会让
// 缓存继续返回旧名称与旧 facet）；它只反映「定价域有没有变化」，不随请求
// 参数变化，因此同一版本下不同筛选条件的条件请求可以命中 304。
type PublicPricingCatalog struct {
	Models    []PublicModelPricing
	Groups    []PublicGroup
	Overrides []PublicGroupPricing
	// Meta 是 model_meta 的**全表**投影（含 status=2）。启用档案用于卡片/
	// 详情/facet；停用档案参与名称匹配，命中即剔除该模型或详情 404（D1-13：
	// 只加载停用档案才能判断「精确命中的是一条停用档案」）。
	//
	// 顺序不保证；解析走 store.ResolveModelMeta，它自行排序，不依赖 SQL
	// ORDER BY。
	Meta []ModelMeta
	// Vendors 是 vendors 的全表投影（含停用行，见 PublicVendor.Status）。
	Vendors     []PublicVendor
	Version     string
	GeneratedAt time.Time
}

// PublicCounts 是首页统计条的两个真实计数（R-PUB-08）。
type PublicCounts struct {
	// Models 与 /api/pricing 的可见集合同口径（status=1 且无表达式）。
	Models int64
	// Upstreams = COUNT(DISTINCT channels.type) WHERE status = 1。
	Upstreams int64
}

// PublicPricingRepo 是公开读面的存储接缝（单测注入内存假实现）。
type PublicPricingRepo interface {
	LoadPublicPricingCatalog(ctx context.Context) (PublicPricingCatalog, error)
	LoadPublicCounts(ctx context.Context) (PublicCounts, error)
}

// SQLPublicPricingRepo 是 PublicPricingRepo 的 PostgreSQL 实现。
type SQLPublicPricingRepo struct {
	db *sql.DB
}

// NewPublicPricingRepo 构造公开定价仓储。db 必须非 nil：公开目录没有可用的
// 内存兜底，静默返回空目录会让「暂无上架模型」的文案掩盖数据库故障。
func NewPublicPricingRepo(db *sql.DB) *SQLPublicPricingRepo {
	return &SQLPublicPricingRepo{db: db}
}

// visibleModelWhere 是公示准入规则的单一定义点。
//
// 【为什么冻结成常量】facets、计数与 items 三条路径必须完全一致；任何一处
// 漏掉 billing_expr IS NULL 都会让表达式模型出现在目录里而没有整数价。
const visibleModelWhere = `status = 1 AND billing_expr IS NULL`

// LoadPublicPricingCatalog 在同一 REPEATABLE READ 只读事务内读取三张表。
//
// 【为什么是 REPEATABLE READ】PostgreSQL 的默认 READ COMMITTED 下，count 与
// 分页查询之间若有管理员提交定价变更，可能出现「total 与 items 不属于同一
// 快照」的抖动（F-09-21 的翻页不重复/不遗漏要求）。只读事务把三张表固定到
// 同一快照，代价是一次快照建立（公开目录是低频只读面）。
func (r *SQLPublicPricingRepo) LoadPublicPricingCatalog(ctx context.Context) (PublicPricingCatalog, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return PublicPricingCatalog{}, fmt.Errorf("store: begin public pricing snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	models, err := loadPublicModels(ctx, tx)
	if err != nil {
		return PublicPricingCatalog{}, err
	}
	groups, err := loadPublicGroups(ctx, tx)
	if err != nil {
		return PublicPricingCatalog{}, err
	}
	overrides, err := loadPublicOverrides(ctx, tx)
	if err != nil {
		return PublicPricingCatalog{}, err
	}
	// 元数据与供应商在**同一事务**内读取（D1-22）：若它们来自独立连接，
	// 可能与定价行分属不同提交点，出现「新价格 + 旧展示名」的混合快照；
	// 版本哈希也会与展示内容脱节，让 ETag 无法作为内容指纹。
	meta, err := loadPublicModelMeta(ctx, tx)
	if err != nil {
		return PublicPricingCatalog{}, err
	}
	vendors, err := loadPublicVendors(ctx, tx)
	if err != nil {
		return PublicPricingCatalog{}, err
	}
	version, err := publicPricingVersion(ctx, tx)
	if err != nil {
		return PublicPricingCatalog{}, err
	}
	if err := tx.Commit(); err != nil {
		return PublicPricingCatalog{}, fmt.Errorf("store: commit public pricing snapshot: %w", err)
	}
	return PublicPricingCatalog{
		Models: models, Groups: groups, Overrides: overrides,
		Meta: meta, Vendors: vendors,
		Version: version, GeneratedAt: time.Now().UTC(),
	}, nil
}

func loadPublicModels(ctx context.Context, tx *sql.Tx) ([]PublicModelPricing, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT model, mode, prompt_price, completion_price, cache_read_price, cache_write_price, vendor,
		       strategy
		FROM model_pricing
		WHERE `+visibleModelWhere+`
		ORDER BY model ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list public model_pricing: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// ORDER BY 只是让快照读取顺序确定；HTTP 层会按请求的排序键重排。
	// 这里仍按 model 升序返回，便于测试与差分对比。
	models := make([]PublicModelPricing, 0, 64)
	for rows.Next() {
		var rec PublicModelPricing
		if err := rows.Scan(&rec.Model, &rec.Mode, &rec.PromptPrice,
			&rec.CompletionPrice, &rec.CacheReadPrice, &rec.CacheWritePrice, &rec.Vendor,
			&rec.Strategy); err != nil {
			return nil, fmt.Errorf("store: scan public model_pricing: %w", err)
		}
		models = append(models, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate public model_pricing: %w", err)
	}
	return models, nil
}

// loadPublicModelMeta 读取全部模型档案（含 status=2）。
//
// 为什么不过滤 status：D1-13 要求「停用档案精确命中 ⇒ 剔除模型」。若 SQL
// 直接丢掉停用行，一条 exact 停用档案会被更宽的前缀档案「穿透」，模型反而
// 带着错误的展示名出现在广场上。过滤交给解析后的 Status 判定。
func loadPublicModelMeta(ctx context.Context, tx *sql.Tx) ([]ModelMeta, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+modelMetaColumns+` FROM model_meta`)
	if err != nil {
		return nil, fmt.Errorf("store: list public model_meta: %w", err)
	}
	defer func() { _ = rows.Close() }()

	meta := make([]ModelMeta, 0, 64)
	for rows.Next() {
		rec, scanErr := scanModelMeta(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store: scan public model_meta: %w", scanErr)
		}
		meta = append(meta, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate public model_meta: %w", err)
	}
	return meta, nil
}

// loadPublicVendors 读取供应商目录（含停用行，见 PublicVendor.Status）。
func loadPublicVendors(ctx context.Context, tx *sql.Tx) ([]PublicVendor, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, name, status FROM vendors ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list public vendors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	vendors := make([]PublicVendor, 0, 16)
	for rows.Next() {
		var rec PublicVendor
		if err := rows.Scan(&rec.ID, &rec.Name, &rec.Status); err != nil {
			return nil, fmt.Errorf("store: scan public vendors: %w", err)
		}
		vendors = append(vendors, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate public vendors: %w", err)
	}
	return vendors, nil
}

func loadPublicGroups(ctx context.Context, tx *sql.Tx) ([]PublicGroup, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT name, ratio FROM user_groups
		WHERE status = 1
		ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list public user_groups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	groups := make([]PublicGroup, 0, 8)
	for rows.Next() {
		var group PublicGroup
		if err := rows.Scan(&group.Name, &group.Ratio); err != nil {
			return nil, fmt.Errorf("store: scan public user_groups: %w", err)
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate public user_groups: %w", err)
	}
	return groups, nil
}

func loadPublicOverrides(ctx context.Context, tx *sql.Tx) ([]PublicGroupPricing, error) {
	// JOIN 源模型：覆盖价只对「可见模型」有展示意义。若源模型停用/表达式，
	// 其覆盖价即使自身 status=1 也必须随模型一起消失（否则会出现没有对应
	// 卡片的孤儿价格）。
	rows, err := tx.QueryContext(ctx, `
		SELECT gp."group", gp.model, gp.prompt_price, gp.completion_price,
		       gp.cache_read_price, gp.cache_write_price
		FROM group_pricing gp
		JOIN model_pricing mp ON mp.model = gp.model
		WHERE gp.status = 1 AND mp.status = 1 AND mp.billing_expr IS NULL
		ORDER BY gp."group" ASC, gp.model ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list public group_pricing: %w", err)
	}
	defer func() { _ = rows.Close() }()

	overrides := make([]PublicGroupPricing, 0, 16)
	for rows.Next() {
		var rec PublicGroupPricing
		if err := rows.Scan(&rec.Group, &rec.Model, &rec.PromptPrice,
			&rec.CompletionPrice, &rec.CacheReadPrice, &rec.CacheWritePrice); err != nil {
			return nil, fmt.Errorf("store: scan public group_pricing: %w", err)
		}
		overrides = append(overrides, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate public group_pricing: %w", err)
	}
	return overrides, nil
}

// publicPricingVersion 计算定价域版本号（F-09-14/B3 契约的 ETag 派生口径）。
//
// 取五张表各自的 max(updated_at)+count(*)+max(id)——定价三表之外必须纳入
// model_meta 与 vendors（D1-22）：卡片名称、能力标签、上下文分档与供应商
// 展示名都来自这两张表，若版本不含它们，管理员改完档案后公开页仍会因
// If-None-Match 命中 304 而继续展示旧内容（最长一个缓存周期）。
//
// 只用 count 不够：同一行改价并重新启用时 count 不变；只用 max(updated_at)
// 不够：删除最新一行会让 max 回退，而删除同样必须改变版本。max(id) 覆盖
// 「删除后又插入同内容行」的情形，三者组合在常规管理操作下不会产生版本回退。
func publicPricingVersion(ctx context.Context, tx *sql.Tx) (string, error) {
	var builder []byte
	for _, table := range []string{"model_pricing", "group_pricing", "user_groups", "model_meta", "vendors"} {
		var (
			updated time.Time
			count   int64
			maxID   int64
		)
		// 表名来自本函数内的固定白名单，绝不接受调用方字符串。
		query := `SELECT COALESCE(max(updated_at), '1970-01-01T00:00:00Z'::timestamptz),
		                 count(*), COALESCE(max(id), 0)
		          FROM ` + table
		if err := tx.QueryRowContext(ctx, query).Scan(&updated, &count, &maxID); err != nil {
			return "", fmt.Errorf("store: read %s version: %w", table, err)
		}
		builder = fmt.Appendf(builder, "%s|%d|%d|%d;", table, updated.UTC().UnixNano(), count, maxID)
	}
	// 策略内容指纹（spec §5.4 / 计划 Task 6）：strategy 决定公开面的
	// pricing_schedule。虽然管理面改策略会推进 updated_at，但迁移回填或直接
	// SQL 可能只改内容不改时间戳；把内容本身纳入版本，保证「策略变了 ⇒ 版本
	// 必变」，避免公开页因 If-None-Match 继续返回旧的时段表。
	var strategyFingerprint string
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(md5(string_agg(COALESCE(strategy::text, ''), '|' ORDER BY model)), '')
		FROM model_pricing`).Scan(&strategyFingerprint); err != nil {
		return "", fmt.Errorf("store: read model_pricing strategy version: %w", err)
	}
	builder = fmt.Appendf(builder, "strategy|%s;", strategyFingerprint)
	sum := sha256.Sum256(builder)
	// 取前 16 个 hex 字符（64 bit）作为版本标签：ETag 需要稳定且短，完整
	// 256 bit 对缓存键过重；碰撞概率在「定价变更 + 相同请求」下可忽略。
	return hex.EncodeToString(sum[:])[:16], nil
}

// LoadPublicCounts 读取首页统计条的两个 DB 计数。
//
// 两个计数在同一 REPEATABLE READ 事务内读取：F-09-04 的验收要求「数值与后端
// 公示计数一致（同一快照）」，而 /api/home/stats 与 /api/pricing 是两个请求，
// 不可能跨请求共享快照；但至少本次响应内的 models 与 upstreams 必须来自同一
// 提交点，避免一次刷新里出现半新半旧的一组数字。
func (r *SQLPublicPricingRepo) LoadPublicCounts(ctx context.Context) (PublicCounts, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return PublicCounts{}, fmt.Errorf("store: begin public counts snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var counts PublicCounts
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM model_pricing WHERE `+visibleModelWhere).Scan(&counts.Models); err != nil {
		return PublicCounts{}, fmt.Errorf("store: count public models: %w", err)
	}
	// channels.status 三态（1=enabled / 2=manual_disabled / 3=auto_disabled），
	// 统计条只算真正可调度的渠道（R-PUB-08 冻结口径）。
	if err := tx.QueryRowContext(ctx,
		`SELECT count(DISTINCT type) FROM channels WHERE status = 1`).Scan(&counts.Upstreams); err != nil {
		return PublicCounts{}, fmt.Errorf("store: count public upstreams: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return PublicCounts{}, fmt.Errorf("store: commit public counts snapshot: %w", err)
	}
	return counts, nil
}

// ListPublicChatModels 返回公开目录可见、且具备对话/流式端点的模型名（已排序）。
//
// 计划 Task 3：定时性能探测（后续任务）只需要「公开面真实在售、且能对话」的
// 模型集合。可见集合必须与 HTTP 层完全一致：visibleModelWhere 先筛定价行，
// 再用 ResolveModelMeta 剔除「解析到停用档案」的模型（D1-13）；任何一处漏掉
// 都会让探测结果出现在首页性能表里，而对应的模型在目录中并不存在。
//
// 【为什么端点为空也保留】没有档案或档案没有 endpoints 元数据时，无法证明它
// 不是对话模型；静默丢弃会让这类模型永远得不到实测数据（Review Focus 4）。
// 只有「明确列出了端点、且其中没有对话端点」才剔除。
//
// 与 LoadPublicPricingCatalog 一样在 REPEATABLE READ 只读事务内读取：定价行与
// 档案必须来自同一快照，否则可能出现「模型刚停用、旧档案仍说它是 chat」的
// 混合集合。这里不计算版本号，探测方按自己的调度周期取用即可。
func (r *SQLPublicPricingRepo) ListPublicChatModels(ctx context.Context) ([]string, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("store: begin public chat models snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	models, err := loadPublicModels(ctx, tx)
	if err != nil {
		return nil, err
	}
	meta, err := loadPublicModelMeta(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit public chat models snapshot: %w", err)
	}

	names := make([]string, 0, len(models))
	for _, rec := range models {
		names = append(names, rec.Model)
	}
	return publicChatModelNames(names, meta), nil
}

// publicChatModelNames 从公开可见的模型名集合中挑出对话模型（已排序）。
//
// 纯函数：命中规则与 HTTP 层共用 ResolveModelMeta，端点判定共用
// DeriveEndpointTypes，避免探测侧与展示侧各写一份枚举而漂移。
func publicChatModelNames(names []string, meta []ModelMeta) []string {
	resolved := ResolveModelMeta(meta, names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		record, ok := resolved[name]
		// 命中停用档案 ⇒ 公开面同样剔除（D1-13）。
		if ok && record.Status != 1 {
			continue
		}
		// 只有「明确列出端点、且没有对话端点」才剔除；无端点/无档案保守保留。
		if ok {
			types := DeriveEndpointTypes(record.Endpoints)
			if len(types) > 0 && !hasChatEndpoint(types) {
				continue
			}
		}
		out = append(out, name)
	}
	// 排序让探测任务与测试都有确定顺序；DB 虽已按 model 升序返回，纯函数不依赖它。
	sort.Strings(out)
	return out
}

// hasChatEndpoint 判断公开端点枚举里是否存在对话/流式协议族。
//
// 三个值对应 OpenAI chat、Anthropic messages 与 Gemini generateContent：
// 它们都是可被性能探测直接调用的对话入口。
func hasChatEndpoint(types []string) bool {
	for _, value := range types {
		switch value {
		case "chat.completions", "messages", "generateContent":
			return true
		}
	}
	return false
}
