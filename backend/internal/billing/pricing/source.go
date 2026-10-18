package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"newtg/gateway/internal/contract"
)

// ============================================================================
// 数据库定价源（M3 的 canonical 价源）
//
// 承载（platform-01 §7/§8，R9/R10）：
//   - model_pricing：mode(0=per_token / 1=per_request) + billing_expr(NULL/非 NULL)
//     判别三模式；四个价列是**积分/1M tokens 的整数绝对值**，NULL=未设置→兜底，
//     0=显式免费；
//   - group_pricing：分组 × 模型的绝对覆盖价；存在启用行时**替代** model_pricing
//     价 × 分组倍率（modules-06 F-06-16）；
//   - user_groups.ratio：分组倍率，整数百分比（120 = 1.20x）。
//
// 优先级（billing-02 §2.2/§2.4 在 R9 形态下的落点）：
//  1. model_pricing.billing_expr 非 NULL → 表达式（显式模式最高优先）；
//  2. 已配置的整数价 → per_request（mode=1）或 per_token（mode=0）；
//  3. 无行且命中内置表达式表 → 表达式（已配置旧价优先于内置表达式）；
//  4. 未知模型 → 默认价（本包默认常量，绝不静默免费）。
//
// 查询失败（连接错误、上下文取消等）一律返回 error，**不得**回退到免费价：
// 把数据库故障当成「免费」会让一次故障变成全站白送。
// ============================================================================

// ConfigReader 是配置中心的最小读取缝（任务 3 的 Engine 使用同一形状）。
type ConfigReader interface {
	Get(key string) (json.RawMessage, bool)
}

// Source 解析一次请求的定价快照。
type Source interface {
	Resolve(ctx context.Context, model, group string) (contract.PriceSnapshot, error)
}

const (
	// quotaPerUnitKey 是模块 08 登记的兑换比键（modules-99 B-1 的唯一键名）。
	quotaPerUnitKey = "billing.quota_per_unit"

	// sourceCacheTTL 是定价行 / 分组倍率的内存缓存时长。
	//
	// 【为什么需要缓存】定价查询在每次预扣与结算都会发生，属于请求热路径；
	// 但定价变更由管理面广播（pricing.changed）后本进程没有订阅位点，
	// 30s 与公开目录的 exposed 快照 TTL（billing-02 §8.4）一致，作为
	// 「变更后最迟 30s 生效」的保守上界。NewSource 的调用方可调用
	// Invalidate 立即清空。
	sourceCacheTTL = 30 * time.Second

	// maxExactFloatInt 是 float64 能精确表示的整数上界（2^53−1）。
	// PriceData 的价格字段是 float64（契约冻结）；DB 价列是 int64 且允许
	// 到 9.2e18，超过该界会静默丢精度，故在进入快照前显式拒绝。
	maxExactFloatInt = int64(1)<<53 - 1
)

// modelPricingRow 是 model_pricing 的读取投影（只取定价相关列）。
type modelPricingRow struct {
	ID              int64
	Model           string
	Mode            int16
	BillingExpr     *string
	PromptPrice     *int64
	CompletionPrice *int64
	CacheReadPrice  *int64
	CacheWritePrice *int64
	Status          int16
}

// TableName 固定表名，避免 GORM 复数化推断随配置漂移。
func (modelPricingRow) TableName() string { return "model_pricing" }

// groupPricingRow 是 group_pricing 的读取投影。
type groupPricingRow struct {
	ID              int64
	Group           string `gorm:"column:group"`
	Model           string
	PromptPrice     int64
	CompletionPrice int64
	CacheReadPrice  int64
	CacheWritePrice int64
	Status          int16
}

// TableName 固定表名（表名含保留字列 "group"，故列名显式标注）。
func (groupPricingRow) TableName() string { return "group_pricing" }

// userGroupRow 是 user_groups 的读取投影。
type userGroupRow struct {
	Ratio  int
	Status int16
}

// TableName 固定表名。
func (userGroupRow) TableName() string { return "user_groups" }

type cacheEntry[T any] struct {
	value   T
	expires time.Time
}

// DBSource 是 Source 的 PostgreSQL 实现。
//
// 并发安全：三张缓存表由 mu 保护；查询在锁外执行，允许并发重复查询
// （最坏只是多做一次 DB 读取，换来锁内不做 I/O 的简单性）。缓存值是不可变
// 结构体，读路径返回副本，无共享可变状态。
type DBSource struct {
	db  *gorm.DB
	ttl time.Duration
	now func() time.Time

	mu          sync.RWMutex
	models      map[string]cacheEntry[modelPricingRow]
	groupPrices map[string]cacheEntry[groupPricingRow]
	groupRatios map[string]cacheEntry[float64]
}

// NewSource 构造数据库定价源。
//
// cfg 可为 nil（测试 / 未接配置中心时用默认兑换比）。配置里的
// billing.quota_per_unit 接受 JSON 数字或数值字符串（与 Engine 同口径），
// 但必须是整数且落在 1–1,000,000；非法值让构造失败而不是静默改用默认值——
// 错误的兑换比会让全站金额错位。构造成功时会把 QPU 写入 ComputeQuota 的
// 进程级原子配置（见 readQuotaPerUnit）。
func NewSource(db *gorm.DB, cfg ConfigReader) (*DBSource, error) {
	if db == nil {
		return nil, errors.New("pricing: NewSource 需要非 nil 的 *gorm.DB")
	}
	qpu, err := readQuotaPerUnit(cfg)
	if err != nil {
		return nil, err
	}
	if err := SetQuotaPerUnit(qpu); err != nil {
		return nil, err
	}
	return &DBSource{
		db:          db,
		ttl:         sourceCacheTTL,
		now:         time.Now,
		models:      make(map[string]cacheEntry[modelPricingRow]),
		groupPrices: make(map[string]cacheEntry[groupPricingRow]),
		groupRatios: make(map[string]cacheEntry[float64]),
	}, nil
}

// Invalidate 清空全部缓存（定价变更广播的进程内接缝）。
func (s *DBSource) Invalidate() {
	s.mu.Lock()
	s.models = make(map[string]cacheEntry[modelPricingRow])
	s.groupPrices = make(map[string]cacheEntry[groupPricingRow])
	s.groupRatios = make(map[string]cacheEntry[float64])
	s.mu.Unlock()
}

// readQuotaPerUnit 读取并校验 billing.quota_per_unit（默认 100）。
//
// 【解析口径与 Engine 对齐】配置中心的值可能是 JSON 数字（`100`）或数值
// 字符串（`"100"`）——Task 3 的 Engine.configFloat 两种都接受，本函数保持
// 同一契约，避免同一配置键在两处得到不同结果。设计上该键是 bigint
// （modules-08），故非整数值（100.5）一律拒绝。`null` 与缺键等价（默认 100）。
//
// 【进程级副作用】QPU 只有 ComputeQuota 需要，而它的冻结签名没有该入参；
// 本函数在 NewSource 装配时调用 SetQuotaPerUnit 写入进程级原子值。装配方
// 必须先构造 Source 再对外提供计费服务（T9 装配顺序）。
func readQuotaPerUnit(cfg ConfigReader) (int64, error) {
	if cfg == nil {
		return DefaultQuotaPerUnit, nil
	}
	raw, ok := cfg.Get(quotaPerUnitKey)
	if !ok || len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return DefaultQuotaPerUnit, nil
	}
	f, err := parseConfigNumber(raw)
	if err != nil {
		return 0, fmt.Errorf("pricing: 解析 %s: %w", quotaPerUnitKey, err)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, fmt.Errorf("pricing: %s 必须是整数（got %v）", quotaPerUnitKey, f)
	}
	if f < 1 || f > 1_000_000 {
		return 0, fmt.Errorf("pricing: %s 越界（%v，允许 1–1000000）", quotaPerUnitKey, f)
	}
	return int64(f), nil
}

// parseConfigNumber 接受 JSON 数字或数值字符串（与 Engine.configFloat 同口径）。
func parseConfigNumber(raw json.RawMessage) (float64, error) {
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, errors.New("不是数值或数值字符串")
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("数值字符串非法: %w", err)
	}
	return v, nil
}

// Resolve 实现 Source。
func (s *DBSource) Resolve(ctx context.Context, model, group string) (contract.PriceSnapshot, error) {
	candidates := ResolveBillingModelCandidates(model)
	group = strings.TrimSpace(group)
	if group == "" {
		// group 缺省用 default（brief 冻结；user_groups.default 由迁移保证存在）。
		group = "default"
	}

	// 按候选顺序查价：精确名优先，别名/基名只在前者未登记时兜底
	// （billing-02 §2.3；见 ResolveBillingModelCandidates）。
	var (
		row     modelPricingRow
		matched string
		found   bool
	)
	for _, cand := range candidates {
		r, ok, err := s.lookupModel(ctx, cand)
		if err != nil {
			return contract.PriceSnapshot{}, err
		}
		if ok {
			row, matched, found = r, cand, true
			break
		}
	}
	if !found || row.Status != 1 {
		return s.defaultSnapshot(ctx, candidates, group)
	}

	price, mode, exprString, err := priceFromModelRow(row, matched)
	if err != nil {
		return contract.PriceSnapshot{}, err
	}

	info, err := s.groupInfo(ctx, group)
	if err != nil {
		return contract.PriceSnapshot{}, err
	}
	price.GroupRatioInfo = info

	// 表达式自包含全部计价（billing-02 §3.1）：group_pricing 的绝对覆盖价
	// 只覆盖价列，不覆盖表达式，故表达式模式跳过覆盖查询。
	if mode != contract.BillingModeTieredExpr {
		override, ok, err := s.lookupGroupPricing(ctx, group, matched)
		if err != nil {
			return contract.PriceSnapshot{}, err
		}
		if ok {
			if err := applyGroupOverride(&price, mode, override); err != nil {
				return contract.PriceSnapshot{}, err
			}
			// F-06-16：覆盖价「替代 model_pricing 价 × 分组倍率」——覆盖价是
			// 该分组的最终绝对价，故生效倍率为 1；原始分组倍率保留在
			// GroupRatioInfo 里供日志/诊断（HasSpecialRatio 标注覆盖命中）。
			price.GroupRatioInfo.GroupRatio = info.GroupRatio
			price.GroupRatioInfo.GroupSpecialRatio = 1
			price.GroupRatioInfo.HasSpecialRatio = true
		}
	}

	return ResolveExpression(&price, matched, mode, exprString)
}

// defaultSnapshot 处理「无定价行 / 停用行」：内置表达式优先，其次默认价。
func (s *DBSource) defaultSnapshot(ctx context.Context, candidates []string, group string) (contract.PriceSnapshot, error) {
	info, err := s.groupInfo(ctx, group)
	if err != nil {
		return contract.PriceSnapshot{}, err
	}

	for _, cand := range candidates {
		if e, ok := BuiltinBillingExpression(cand); ok {
			price := contract.PriceData{GroupRatioInfo: info}
			return ResolveExpression(&price, cand, contract.BillingModeTieredExpr, e)
		}
	}

	model := ""
	if len(candidates) > 0 {
		model = candidates[0]
	}
	price := defaultPriceData()
	price.GroupRatioInfo = info
	return ResolveExpression(&price, model, contract.BillingModeRatio, "")
}

// priceFromModelRow 把一行 model_pricing 映射为 PriceData / 模式 / 表达式。
func priceFromModelRow(row modelPricingRow, model string) (contract.PriceData, contract.BillingMode, string, error) {
	if row.BillingExpr != nil {
		e := strings.TrimSpace(*row.BillingExpr)
		if e == "" {
			// R-37（fail-closed）：billing_expr 列存在但为空 → 配置错误。
			// 静默回退 ratio/per_call 会让管理员以为表达式生效，实际按另一套
			// 价格计费——这是比拒绝请求更危险的「静默错价」。
			return contract.PriceData{}, 0, "",
				fmt.Errorf("%w: model_pricing(%s).billing_expr 为空，拒绝回退 ratio/per_call", ErrInvalidSpec, row.Model)
		}
		// R9：billing_expr 非 NULL 即表达式模式（显式模式最高优先）。
		return contract.PriceData{}, contract.BillingModeTieredExpr, e, nil
	}

	switch row.Mode {
	case 1: // per_request：prompt_price = 每次请求价（积分/次）
		price := contract.PriceData{UsePrice: true}
		if row.PromptPrice != nil {
			v, err := exactPrice("prompt_price", *row.PromptPrice)
			if err != nil {
				return contract.PriceData{}, 0, "", err
			}
			price.ModelPrice = v
		} else {
			// R9：NULL=未设置→兜底。
			price.ModelPrice = DefaultPerCallPrice()
		}
		price.FreeModel = price.ModelPrice == 0
		return price, contract.BillingModePerCall, "", nil

	case 0: // per_token：四个价列是积分/1M tokens 的绝对价
		price := contract.PriceData{}
		if row.PromptPrice != nil {
			v, err := exactPrice("prompt_price", *row.PromptPrice)
			if err != nil {
				return contract.PriceData{}, 0, "", err
			}
			price.ModelRatio = v
		} else {
			price.ModelRatio = DefaultPromptPricePer1M()
		}
		if row.CompletionPrice != nil {
			v, err := exactPrice("completion_price", *row.CompletionPrice)
			if err != nil {
				return contract.PriceData{}, 0, "", err
			}
			if v == 0 {
				// 显式零用 -1 哨兵承载；NULL 保持 0（未设置）交给模型族兜底。
				price.CompletionRatio = explicitZeroRatio
			} else {
				price.CompletionRatio = v
			}
		}
		if row.CacheReadPrice != nil {
			v, err := exactPrice("cache_read_price", *row.CacheReadPrice)
			if err != nil {
				return contract.PriceData{}, 0, "", err
			}
			price.CacheRatio = v
			price.CacheRatioSet = true
		}
		if row.CacheWritePrice != nil {
			v, err := exactPrice("cache_write_price", *row.CacheWritePrice)
			if err != nil {
				return contract.PriceData{}, 0, "", err
			}
			price.CacheCreationRatio = v
			price.CacheCreationRatioSet = true
		}
		price.ImageRatio = DefaultImageRatio
		price.AudioRatio = DefaultAudioRatio
		price.AudioCompletionRatio = DefaultAudioCompletionRatio
		// 免费模型：四项有效价全为 0（modules-06 F-06-13 的「免费」判定）。
		price.FreeModel = price.ModelRatio == 0 &&
			effectiveCompletionPrice(row, model, price.ModelRatio) == 0 &&
			effectiveCacheReadPrice(row, price.ModelRatio) == 0 &&
			effectiveCacheWritePrice(row, price.ModelRatio) == 0
		return price, contract.BillingModeRatio, "", nil

	default:
		return contract.PriceData{}, 0, "", fmt.Errorf("pricing: model_pricing.mode=%d 非法（期望 0/1）", row.Mode)
	}
}

// exactPrice 把 DB 的 int64 积分价转成 float64，拒绝超出精确整数域的取值。
func exactPrice(name string, v int64) (float64, error) {
	if v < 0 {
		return 0, fmt.Errorf("%w: %s=%d", ErrInvalidPrice, name, v)
	}
	if v > maxExactFloatInt {
		return 0, fmt.Errorf("%w: %s=%d 超出 float64 精确整数域（>2^53−1）", ErrInvalidPrice, name, v)
	}
	return float64(v), nil
}

func effectiveCompletionPrice(row modelPricingRow, model string, prompt float64) float64 {
	if row.CompletionPrice != nil {
		return float64(*row.CompletionPrice)
	}
	return CompletionPricePer1M(model, prompt, nil)
}

func effectiveCacheReadPrice(row modelPricingRow, prompt float64) float64 {
	if row.CacheReadPrice != nil {
		return float64(*row.CacheReadPrice)
	}
	return prompt * DefaultCacheRatio
}

func effectiveCacheWritePrice(row modelPricingRow, prompt float64) float64 {
	if row.CacheWritePrice != nil {
		return float64(*row.CacheWritePrice)
	}
	return prompt * DefaultCacheCreationRatio
}

// applyGroupOverride 用 group_pricing 的绝对价替换模型价。
func applyGroupOverride(price *contract.PriceData, mode contract.BillingMode, row groupPricingRow) error {
	switch mode {
	case contract.BillingModePerCall:
		v, err := exactPrice("group_pricing.prompt_price", row.PromptPrice)
		if err != nil {
			return err
		}
		price.ModelPrice = v
		price.UsePrice = true
		price.FreeModel = v == 0
	case contract.BillingModeRatio:
		prompt, err := exactPrice("group_pricing.prompt_price", row.PromptPrice)
		if err != nil {
			return err
		}
		completion, err := exactPrice("group_pricing.completion_price", row.CompletionPrice)
		if err != nil {
			return err
		}
		cacheRead, err := exactPrice("group_pricing.cache_read_price", row.CacheReadPrice)
		if err != nil {
			return err
		}
		cacheWrite, err := exactPrice("group_pricing.cache_write_price", row.CacheWritePrice)
		if err != nil {
			return err
		}
		price.ModelRatio = prompt
		// 覆盖价的 completion_price 是**绝对值**：0 表示该分组输出免费，
		// 必须用 -1 哨兵承载，否则 applyRatioDefaults 会把 0 当「未设置」
		// 并按模型族（1–5× 输入价）兜底——静默多收费（review C1）。
		if completion == 0 {
			price.CompletionRatio = explicitZeroRatio
		} else {
			price.CompletionRatio = completion
		}
		price.CacheRatio = cacheRead
		price.CacheRatioSet = true
		price.CacheCreationRatio = cacheWrite
		price.CacheCreationRatioSet = true
		// 5m/1h 随新基准重新派生（覆盖行没有分档列）。
		price.CacheCreation5mRatio = 0
		price.CacheCreationSet = false
		price.CacheCreation1hRatio = 0
		price.FreeModel = prompt == 0 && completion == 0 && cacheRead == 0 && cacheWrite == 0
	default:
		return fmt.Errorf("%w: 覆盖价不适用于模式 %d", ErrInvalidMode, mode)
	}
	return nil
}

// groupInfo 读取分组倍率并包装为 GroupRatioInfo。
func (s *DBSource) groupInfo(ctx context.Context, group string) (contract.GroupRatioInfo, error) {
	ratio, err := s.lookupGroupRatio(ctx, group)
	if err != nil {
		return contract.GroupRatioInfo{}, err
	}
	return contract.GroupRatioInfo{Group: group, GroupRatio: ratio}, nil
}

// ---------------------------------------------------------------------------
// 查询 + 缓存
// ---------------------------------------------------------------------------

func (s *DBSource) lookupModel(ctx context.Context, model string) (modelPricingRow, bool, error) {
	if v, ok := s.getModelCache(model); ok {
		return v, true, nil
	}
	var row modelPricingRow
	err := s.db.WithContext(ctx).Where("model = ?", model).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return modelPricingRow{}, false, nil
	}
	if err != nil {
		return modelPricingRow{}, false, fmt.Errorf("pricing: 查询 model_pricing(%s): %w", model, err)
	}
	s.putModelCache(model, row)
	return row, true, nil
}

func (s *DBSource) lookupGroupPricing(ctx context.Context, group, model string) (groupPricingRow, bool, error) {
	key := group + "\x00" + model
	if v, ok := s.getGroupPriceCache(key); ok {
		return v, true, nil
	}
	var row groupPricingRow
	err := s.db.WithContext(ctx).
		Where(`"group" = ? AND model = ? AND status = 1`, group, model).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return groupPricingRow{}, false, nil
	}
	if err != nil {
		return groupPricingRow{}, false, fmt.Errorf("pricing: 查询 group_pricing(%s,%s): %w", group, model, err)
	}
	s.putGroupPriceCache(key, row)
	return row, true, nil
}

func (s *DBSource) lookupGroupRatio(ctx context.Context, group string) (float64, error) {
	if v, ok := s.getGroupRatioCache(group); ok {
		return v, nil
	}
	var row userGroupRow
	err := s.db.WithContext(ctx).
		Select("ratio, status").
		Where("name = ? AND status = 1", group).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// billing-02 §4.7：未命中记日志返回 1（不是错误、不是 0）。
		s.putGroupRatioCache(group, 1.0)
		return 1.0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("pricing: 查询 user_groups(%s): %w", group, err)
	}
	// ratio 是整数百分比：120 → 1.20x（modules-06 F-06-15）。
	ratio := float64(row.Ratio) / 100
	s.putGroupRatioCache(group, ratio)
	return ratio, nil
}

// ---------------------------------------------------------------------------
// 缓存读写
//
// 【为什么每类缓存有独立方法】缓存字段（s.models 等）会被 Invalidate 整体
// 替换。若把字段本身作为参数传给泛型 helper，参数求值发生在加锁之前，会与
// Invalidate 的赋值构成数据竞争（review I2）。因此字段访问必须发生在锁内：
// 每个方法在持锁期间读取/写入字段并完成 map 操作，Invalidate 只在持锁时
// 替换 map。旧 map 在被替换后不再有写入者（所有 put 都访问当前字段），
// 因此读取旧 map 的副本也是安全的。
// ---------------------------------------------------------------------------

func (s *DBSource) getModelCache(key string) (modelPricingRow, bool) {
	s.mu.RLock()
	e, ok := s.models[key]
	s.mu.RUnlock()
	if !ok || !s.now().Before(e.expires) {
		return modelPricingRow{}, false
	}
	return e.value, true
}

func (s *DBSource) putModelCache(key string, value modelPricingRow) {
	s.mu.Lock()
	s.models[key] = cacheEntry[modelPricingRow]{value: value, expires: s.now().Add(s.ttl)}
	s.mu.Unlock()
}

func (s *DBSource) getGroupPriceCache(key string) (groupPricingRow, bool) {
	s.mu.RLock()
	e, ok := s.groupPrices[key]
	s.mu.RUnlock()
	if !ok || !s.now().Before(e.expires) {
		return groupPricingRow{}, false
	}
	return e.value, true
}

func (s *DBSource) putGroupPriceCache(key string, value groupPricingRow) {
	s.mu.Lock()
	s.groupPrices[key] = cacheEntry[groupPricingRow]{value: value, expires: s.now().Add(s.ttl)}
	s.mu.Unlock()
}

func (s *DBSource) getGroupRatioCache(key string) (float64, bool) {
	s.mu.RLock()
	e, ok := s.groupRatios[key]
	s.mu.RUnlock()
	if !ok || !s.now().Before(e.expires) {
		return 0, false
	}
	return e.value, true
}

func (s *DBSource) putGroupRatioCache(key string, value float64) {
	s.mu.Lock()
	s.groupRatios[key] = cacheEntry[float64]{value: value, expires: s.now().Add(s.ttl)}
	s.mu.Unlock()
}
