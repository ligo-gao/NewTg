// Package billing 实现 NewTG 的计费引擎（M3）。
//
// 本包在请求生命周期中的位置：
//
//	中继 → PreConsume（按估算预算原子预扣，写 hold 流水）
//	     → [Reserve 上浮 | ChargeIncrement 增量实扣]
//	     → Terminate（按真实用量多退少补）或 Refund（失败全退）
//
// 设计权威：docs/billing-00-计费引擎接口契约.md §5–§9、
// docs/billing-03-预扣结算退款与归一化-设计.md、docs/billing-05-计费量结算-设计.md。
// 净室实现：仅依据规格与冻结契约编写，不复制参照实现源码。
package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"newtg/gateway/internal/billing/pricing"
	platformconfig "newtg/gateway/internal/config"
	"newtg/gateway/internal/contract"
)

// ConfigReader 是计费引擎读取配置中心快照的最小接口。
//
// configcenter.Center.Get 天然满足该形状；单测用内存 map 替代。
type ConfigReader interface {
	Get(key string) (json.RawMessage, bool)
}

// Registry 是在飞请求租约的写侧接口（M3 决议 A3）。
//
// 生产实现见 Task 5 的 Redis registry；单测/未接线时允许为 nil（跳过副作用）。
// 租约参数由会话（本包）拥有：InflightLeaseTTLSeconds 是写入时的 TTL，
// InflightHeartbeatSeconds 是续约间隔。
//
// 【tokenID · R-88】Register 必须携带令牌身份：worker 的崩溃兜底
// （RecoverAbandonedByID）在退回钱包 hold 的同时要把同一金额回补到
// api_tokens.remain_quota；worker 只有 requestID，因此租约里必须存 tokenID。
// 0 表示该请求未绑定令牌（跳过令牌回补）。
type Registry interface {
	Register(ctx context.Context, requestID string, userID, tokenID int64, lease time.Duration) error
	Heartbeat(ctx context.Context, requestID string) error
	Remove(ctx context.Context, requestID string) error
}

// SettlementOutcome 是 Terminate 交给日志层（Saturator）的结算结果。
//
// Status 取 usage_logs.status 的整型枚举（0=refused / 1=success /
// 2=error / 3=refunded）。Cost/PreDeducted/Refunded 均为整数积分。
type SettlementOutcome struct {
	Cost        int64 // 实际应扣（结算后）
	PreDeducted int64 // 预扣总量
	Refunded    int64 // 退还量（≥0）
	// RetainedCost 是 status=refunded 时**保留不退**的 ChargeIncrement 消耗
	// （R-39/R-59）：崩溃兜底只退剩余 hold，已交付用量的增量扣款不随崩溃退。
	// 其它状态忽略该字段（success 用 Cost，refused/error 强制 0）。
	RetainedCost int64
	Status       int16 // usage_logs.status（0..3）
	// FundingSource 是本请求冻结的资金来源，用于 usage_logs.other 的
	// billing_source 键（M4）。零值 = FundingWallet，保持既有日志口径。
	FundingSource contract.FundingKind

	// 订阅结算快照（S4b；仅 FundingSource=subscription 时由 Terminate 填充）。
	// 字段零值一律不写日志键（billing-08 §5.3 的「>0 / ≠0 才写」口径）。
	SubscriptionID          int64  // subscription_id
	SubscriptionPreConsumed int64  // subscription_pre_consumed（冻结的预扣额）
	SubscriptionPostDelta   int64  // subscription_post_delta（可负）
	SubscriptionPlanID      int64  // subscription_plan_id
	SubscriptionPlanTitle   string // subscription_plan_title
	SubscriptionTotal       int64  // subscription_total（amount_total>0 才写）
	SubscriptionUsed        int64  // subscription_used
	// BillingPreference 是用户偏好的落库文本（subscription_first 等）。
	// 只在订阅来源时写 billing_preference；钱包路径保持既有日志口径不变。
	BillingPreference string

	ErrorClass   string
	ErrorMessage string
}

// usage_logs.status 的整型枚举（backend/migrations-logs/20261018061805_init.up.sql）。
const (
	UsageLogStatusRefused  int16 = 0
	UsageLogStatusSuccess  int16 = 1
	UsageLogStatusError    int16 = 2
	UsageLogStatusRefunded int16 = 3
)

// Saturator 是 usage_logs 的落库接缝（Task 6 实现）。
//
// 为什么日志写在主库结算之前：usage_logs 在**独立日志库**，与主库余额不是
// 同一个事务。跨库原子性不可用，所以固定顺序为「先日志（按 request_id
// 幂等）、后主库结算」：
//   - 日志失败 → 不结算，保留 hold，返回 error（请求方可重试/worker 兜底）；
//   - 结算失败 → 已写的日志由 worker 兜底退款时置为 refunded。
//
// 绝不在代码或文档里声称两库有事务一致性。
type Saturator interface {
	Saturate(ctx context.Context, fact *contract.UsageFact, rc *contract.RelayContext, outcome SettlementOutcome) error
}

// 计费错误。
var (
	// ErrInsufficientQuota 是额度不足的哨兵错误；T9 的 relay 映射把它转成
	// HTTP 402（不 import relay/billingseam，避免反向依赖）。
	ErrInsufficientQuota = errors.New("billing: insufficient quota")

	// ErrBudgetExceeded 表示二次预扣超出预算语义（C-10/D12：整请求 400，
	// 不是 402）。当前触发点是「同一会话重复 PreConsume」。
	ErrBudgetExceeded = errors.New("billing: reservation exceeds estimated budget")

	// ErrNoPricing 表示既没有冻结快照，也没有可用的定价源。
	ErrNoPricing = errors.New("billing: no price snapshot and no pricing source")

	// ErrInvalidConfig 表示计费相关配置值非法（非正、NaN/Inf 等）。
	ErrInvalidConfig = errors.New("billing: invalid config")

	// ErrUserNotFound 表示账本操作的目标用户不存在。
	ErrUserNotFound = errors.New("billing: user not found")

	// ErrWalletLimit 表示余额变更会越过钱包安全整数上界（2^53−1）。
	ErrWalletLimit = errors.New("billing: wallet balance limit exceeded")

	// ErrMissingRequestID 表示会话没有绑定计费幂等键。
	ErrMissingRequestID = errors.New("billing: missing request id")

	// ErrSessionClosed 表示会话已结算/已退款，不允许继续扣减。
	ErrSessionClosed = errors.New("billing: session already closed")

	// ErrSettlementInProgress 表示同一 requestID 的结算已被另一个执行者持有
	// （Redis requestID 锁未获取到）。调用方应视为并发冲突并稍后重试/记录，
	// 绝不能退化成「再结算一次」。
	ErrSettlementInProgress = errors.New("billing: settlement already in progress")

	// ErrRequestClosed 表示该 requestID 已被结算/退款/兜底关闭（存在结案标记
	// 或已关闭的预扣行）。会话不得再创建/扩展 hold，也不得再增量扣减。
	// 这是 Important-1 的 fail-closed 出口：避免「已退款请求又产生新 hold」。
	ErrRequestClosed = errors.New("billing: request already closed")

	// ErrPriceError 表示定价/配额钳制类错误（HTTP 400，不是 402）。
	// 当前触发点：PreConsume 前 rc 上已存在 QuotaClamp（billing-03 §5.5）。
	ErrPriceError = errors.New("billing: price error")

	// ErrTokenNotFound 表示请求绑定的 API 令牌在库中不存在（数据不一致）。
	ErrTokenNotFound = errors.New("billing: token not found")
)

// QuotaError 承载一次额度不足的结构化信息（Required/Available 供响应体使用）。
//
// Unwrap 返回 ErrInsufficientQuota，使 errors.Is(err, ErrInsufficientQuota) 成立；
// ErrorClass 供中继错误分类器读取（402 不重试）。
type QuotaError struct {
	Class     contract.ErrorClass
	Required  int64
	Available int64
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("billing: insufficient quota: required %d, available %d", e.Required, e.Available)
}

// Unwrap 让 QuotaError 命中 errors.Is(ErrInsufficientQuota)。
func (e *QuotaError) Unwrap() error { return ErrInsufficientQuota }

// ErrorClass 返回契约错误分类；零值按额度不足处理（本类型只表达该语义）。
func (e *QuotaError) ErrorClass() contract.ErrorClass {
	if e.Class == contract.ErrUnknown {
		return contract.ErrInsufficientQuota
	}
	return e.Class
}

// 计费配置键（configcenter 快照）。
const (
	// ConfigQuotaPerUnit 是「1 元 = 多少积分」的唯一换算基准；GC-2 冻结默认 100。
	ConfigQuotaPerUnit = "billing.quota_per_unit"
	// ConfigPreConsumeRatio 是预扣比例（默认 1.0 = 全额预扣；≤0 拒绝）。
	ConfigPreConsumeRatio = "billing.pre_consume_ratio"
	// ConfigTrustQuotaUSD 是信任旁路阈值（USD）；G7-R51 冻结默认 10 USD，
	// 0 = 禁用旁路。
	ConfigTrustQuotaUSD = "billing.trust_quota_usd"

	// ConfigFreeModelPreconsumeEnabled 是免费模型预扣开关（modules-08 F-08-12，
	// 默认 true）。它与 billing-00 §6.4 的信任旁路是**两套独立策略**：
	// 前者防「零价模型被 max_tokens 放大滥用」，后者是余额充足用户的免预扣。
	ConfigFreeModelPreconsumeEnabled = "billing.free_model_preconsume_enabled"

	// ConfigFreeModelPreconsumeMultiplier 是免费模型预扣上浮系数（decimal，
	// 默认 1.00；有限且 > 0，≤0/非有限按 G7-R05 拒绝）。
	ConfigFreeModelPreconsumeMultiplier = "billing.free_model_preconsume_multiplier"

	// ConfigFreeModelPreconsumeTrustCredits 是免费模型的可信用户阈值（历史累计
	// 消费积分，默认 0 = 一律预扣）。
	ConfigFreeModelPreconsumeTrustCredits = "billing.free_model_preconsume_trust_credits"
)

const (
	defaultQuotaPerUnit    = 100.0
	defaultPreConsumeRatio = 1.0
	defaultTrustQuotaUSD   = 10.0

	defaultFreeModelPreconsumeEnabled    = true
	defaultFreeModelPreconsumeMultiplier = "1.00"
	defaultFreeModelPreconsumeTrust      = int64(0)

	// historyConsumptionTTL 是「历史累计消费」的缓存时长。
	//
	// 为什么缓存：该值只用于免费模型的**跳过预扣**判定（安全方向：多查一次
	// 只会多预扣一笔最终会全额退回的 hold），而 SUM 会随钱包流水增长。
	// 30s 的新鲜度意味着刚跨过阈值的用户最多再被预扣 30s；反向（消费被冲正
	// 后短时间仍被跳过）只会少冻结一小笔零价模型的 hold，不产生少收费。
	historyConsumptionTTL = 30 * time.Second
)

// Engine 装配计费引擎的依赖。所有字段在构造后只读，可被并发使用。
type Engine struct {
	db     *gorm.DB
	logsDB *sql.DB
	rdb    *redis.Client
	cfg    ConfigReader

	source    pricing.Source
	registry  Registry
	saturator Saturator
	ledger    *Ledger
	heartbeat time.Duration // 单测可缩短；零值按契约默认 30s
	// subHeartbeat 是订阅 in-flight 租约的续租间隔（R-W9B-02）。
	//
	// 与 heartbeat 分开：heartbeat 是钱包 Redis 租约的测试注入旋钮，
	// 钱包用例把它压到 10ms；若订阅心跳复用同一字段，每个订阅会话每 10ms
	// 打一次数据库，集成测试会耗尽连接池。零值按 TTL/3 与契约心跳取小
	// （见 subscriptionLeaseHeartbeatInterval）。
	subHeartbeat time.Duration
	// subLeaseTTL 是订阅 in-flight 租约 TTL 的**测试注入**值（R-W9B-02）。
	//
	// 生产路径只能走配置键 billing.subscription_inflight_lease_ttl_seconds，
	// 并有 120s 安全下界（I-1）；测试需要毫秒级 TTL 才能在不等待数分钟的
	// 情况下验证续租/过期/扫描语义，因此保留这个旋钮。非零时完全覆盖配置。
	subLeaseTTL time.Duration
	// subscriptionFunding 是订阅资金来源接缝（S4a）。nil 表示未装配，
	// 资金分派保持纯钱包行为（与 S4a 之前一致）。
	subscriptionFunding SubscriptionFundingSource

	// requestPricing 是冻结请求计价事实的接缝（spec §5.2）：预扣成功后
	// 写入 request_time 与命中倍率，handoff/任务轮询重建会话时读回。
	// nil 表示未装配（单测/无异步结算的部署），此时行为与旧版一致。
	requestPricing RequestPricingStore

	// historyCache 是免费模型可信阈值用的「历史累计消费」短 TTL 缓存。
	historyMu    sync.Mutex
	historyCache map[int64]historyEntry
	// historyTTL：零值用 historyConsumptionTTL（30s）；**负值关闭缓存**
	// （historyTTL > 0 才读写缓存），供需要严格实时的测试/部署使用。
	historyTTL time.Duration
}

// historyEntry 是累计消费缓存项。
type historyEntry struct {
	credits int64
	expires time.Time
}

// NewEngine 构造计费引擎。
//
// registry / saturator 允许为 nil（单测或尚未接线时跳过对应副作用）；
// source 在 rc.PriceSnapshot 为 nil 时使用。
func NewEngine(db *gorm.DB, logsDB *sql.DB, rdb *redis.Client, cfg ConfigReader,
	source pricing.Source, registry Registry, saturator Saturator) *Engine {
	return &Engine{
		db:           db,
		logsDB:       logsDB,
		rdb:          rdb,
		cfg:          cfg,
		source:       source,
		registry:     registry,
		saturator:    saturator,
		ledger:       NewLedger(db),
		historyCache: make(map[int64]historyEntry),
	}
}

// SetSubscriptionFunding 装配订阅资金来源（S4a）。
//
// 必须在会话创建前调用一次（装配期），构造后不再修改：引擎字段对并发
// 只读是安全的前提。未装配时资金分派保持纯钱包行为。
func (e *Engine) SetSubscriptionFunding(f SubscriptionFundingSource) {
	e.subscriptionFunding = f
}

// SetRequestPricingStore 装配冻结请求计价仓储（spec §5.2）。
//
// 与 SetSubscriptionFunding 同一约定：装配期调用一次，构造后不再修改。
// 未装配时 PreConsume 跳过持久化、handoff 结算保持旧行为（按当前定价
// 重解析），并在日志里给出 WARN。
func (e *Engine) SetRequestPricingStore(store RequestPricingStore) {
	e.requestPricing = store
}

// EffectiveConfig 返回构造时注入的配置读取面。
//
// 它只服务于装配可观测性（控制端 W13 裁决 I2）：platform 装配测试必须能
// 断言计费引擎拿到的是 WithDemoOverride 包装后的**有效值面**，而不是原始
// 配置中心——否则「引擎直接使用 center」的回归会让 gateway/worker 与守卫、
// 额度读到不同的演示开关，而行为测试未必覆盖到。业务代码不得用它读取
// 配置：所有计费键都必须走 configBool/configInt64 等带默认值与校验的路径。
func (e *Engine) EffectiveConfig() ConfigReader {
	if e == nil {
		return nil
	}
	return e.cfg
}

// NewSession 为一次请求绑定一个计费会话（relay 每请求调用工厂）。
func (e *Engine) NewSession(rc *contract.RelayContext) contract.BillingSession {
	s := &Session{engine: e, rc: rc, requestTime: time.Now()}
	if rc != nil {
		s.requestID = rc.RequestID
		s.userID = int64(rc.UserID)
		s.tokenID = int64(rc.TokenID)
	}
	return s
}

// demoExemptToken 判定「演示模式 + 不限额度令牌」的计费免单口径
// （modules-08 §3.17.2 / 控制端裁决 I1）。
//
// 为什么只认 unlimited_quota：设计原文把演示账号的识别方式冻结为
// 「其令牌置 unlimited_quota=true」。它不是 unlimited_quota 的通用语义——
// 非演示模式下该标志仍然只解除令牌余额门，users.balance 照常扣减。
//
// 为什么这里直接查库：任务/崩溃恢复路径会按 requestID 重建会话，重建的
// RelayContext 没有鉴权层的 TokenUnlimitedQuota 投影；演示模式是稀有部署，
// 仅在演示模式下才多这一次查询。查询失败按「应计费」处理（fail-closed 到
// 不扩大豁免面），只告警。
func (e *Engine) demoExemptToken(ctx context.Context, tokenID int64) bool {
	if e == nil || e.db == nil || tokenID <= 0 {
		return false
	}
	if !platformconfig.DemoEnabled(e.cfg) {
		return false
	}
	var unlimited bool
	err := e.db.WithContext(ctx).Raw(
		`SELECT remain_quota IS NULL FROM api_tokens WHERE id = ?`, tokenID).
		Row().Scan(&unlimited)
	if err != nil {
		slog.Warn("billing: read token unlimited flag failed; treating request as billable",
			"token_id", tokenID, "error", err)
		return false
	}
	return unlimited
}

// heartbeatInterval 返回心跳间隔（测试可注入更短值）。
func (e *Engine) heartbeatInterval() time.Duration {
	if e.heartbeat > 0 {
		return e.heartbeat
	}
	return time.Duration(contract.InflightHeartbeatSeconds) * time.Second
}

// settlementLockTTL 是 requestID 结算锁的存活时间：必须大于一次结算事务
// （billingOpTimeout=10s）并留出时钟裕量；锁内只做 DB 事务，不做网络日志。
const settlementLockTTL = 30 * time.Second

// settlementLockSeq 保证同一纳秒内多个锁 token 唯一。
var settlementLockSeq atomic.Uint64

// acquireSettlementLock 获取 requestID 作用域的结算锁（R-41）。
//
// 为什么需要：无 hold 的会话没有 consume_pre 行可做 CAS 锚点，两个 Session
// 对象对同一 requestID 结算时可能各自通过事务内复核（READ COMMITTED 下
// 彼此看不到未提交的标记行），造成双扣。Redis SETNX 把并发收窄为串行；
// 事务内的 settlementExists 再处理「顺序重放」。
//
// 失败语义（fail closed）：Redis 不可用或锁已被持有 → 返回错误，不结算。
// 把「锁不可用」当成「可以结算」会直接打开双扣窗口。
//
// rdb 为 nil（单测/未接线）时返回 no-op 释放函数：此时只剩 DB 复核，
// 并发双扣的残余窗口记入 task-3-report；生产装配必须传 rdb（T5/T9）。
func (e *Engine) acquireSettlementLock(ctx context.Context, requestID string) (func(), error) {
	if e.rdb == nil {
		return func() {}, nil
	}
	key := "newtg:billing:settle:" + requestID
	token := fmt.Sprintf("%d-%d", time.Now().UnixNano(), settlementLockSeq.Add(1))
	ok, err := e.rdb.SetNX(ctx, key, token, settlementLockTTL).Result()
	if err != nil {
		return nil, fmt.Errorf("billing: acquire settlement lock for %s: %w", requestID, err)
	}
	if !ok {
		return nil, fmt.Errorf("%w: request %s", ErrSettlementInProgress, requestID)
	}
	return func() {
		// compare-and-delete：锁 TTL 到期后键可能已被别的执行者重新持有，
		// 盲删会破坏对方的锁。释放失败不影响资金一致性（TTL 兜底）。
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = e.rdb.Eval(releaseCtx,
			`if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("del", KEYS[1]) else return 0 end`,
			[]string{key}, token).Err()
	}, nil
}

// configFloat 读取一个数值配置；未设置时返回 def。
//
// configcenter 的 jsonb 值既可能是 JSON number，也可能是 JSON string
// （管理面保存时统一走字符串是基线行为），两种都要接受。NaN/Inf 一律
// 视为配置错误：把 NaN 带进金额计算会产生不可对账的配额。
func (e *Engine) configFloat(key string, def float64) (float64, error) {
	if e.cfg == nil {
		return def, nil
	}
	raw, ok := e.cfg.Get(key)
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return def, nil
	}

	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		var s string
		if err2 := json.Unmarshal(raw, &s); err2 != nil {
			return 0, fmt.Errorf("%w: %s is not numeric", ErrInvalidConfig, key)
		}
		parsed, err3 := strconv.ParseFloat(s, 64)
		if err3 != nil {
			return 0, fmt.Errorf("%w: %s is not numeric", ErrInvalidConfig, key)
		}
		f = parsed
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%w: %s must be finite", ErrInvalidConfig, key)
	}
	return f, nil
}

// configBool 读取布尔配置；未设置时返回 def。
func (e *Engine) configBool(key string, def bool) (bool, error) {
	if e.cfg == nil {
		return def, nil
	}
	raw, ok := e.cfg.Get(key)
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return def, nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, fmt.Errorf("%w: %s is not a boolean", ErrInvalidConfig, key)
}

// configInt64 读取整数配置；未设置时返回 def。
func (e *Engine) configInt64(key string, def int64) (int64, error) {
	if e.cfg == nil {
		return def, nil
	}
	raw, ok := e.cfg.Get(key)
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return def, nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		parsed, err2 := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err2 == nil {
			return parsed, nil
		}
	}
	return 0, fmt.Errorf("%w: %s is not an integer", ErrInvalidConfig, key)
}

// configDecimal 读取十进制定点配置；未设置时返回 def。
//
// 免费模型上浮系数必须是 decimal：ceil(max_tokens × multiplier) 若用
// float64，1.1 × 1000 可能得到 1100.0000000000002，ceil 后多预扣 1 积分。
// 从配置原文（JSON number/string）构造 decimal 可保持管理员写入的精度。
func (e *Engine) configDecimal(key, def string) (decimal.Decimal, error) {
	defVal, err := decimal.NewFromString(def)
	if err != nil {
		return decimal.Zero, fmt.Errorf("%w: bad default for %s", ErrInvalidConfig, key)
	}
	if e.cfg == nil {
		return defVal, nil
	}
	raw, ok := e.cfg.Get(key)
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return defVal, nil
	}
	text := strings.TrimSpace(string(raw))
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		text = strings.TrimSpace(s)
	}
	d, err := decimal.NewFromString(text)
	if err != nil {
		return decimal.Zero, fmt.Errorf("%w: %s is not a decimal", ErrInvalidConfig, key)
	}
	return d, nil
}

// freeModelPreconsumeEnabled 返回免费模型预扣开关（默认 true）。
func (e *Engine) freeModelPreconsumeEnabled() (bool, error) {
	return e.configBool(ConfigFreeModelPreconsumeEnabled, defaultFreeModelPreconsumeEnabled)
}

// freeModelPreconsumeMultiplier 返回免费模型上浮系数（默认 1.00；>0 且有限）。
//
// 非正或非有限按 G7-R05 拒绝：配置错误不得静默取 0（会把滥用保护关掉），
// 也不得静默取 1（掩盖管理员写错的值）。
func (e *Engine) freeModelPreconsumeMultiplier() (decimal.Decimal, error) {
	d, err := e.configDecimal(ConfigFreeModelPreconsumeMultiplier, defaultFreeModelPreconsumeMultiplier)
	if err != nil {
		return decimal.Zero, err
	}
	if !d.IsPositive() {
		return decimal.Zero, fmt.Errorf("%w: %s must be > 0", ErrInvalidConfig, ConfigFreeModelPreconsumeMultiplier)
	}
	return d, nil
}

// freeModelPreconsumeTrustCredits 返回可信用户阈值（默认 0 = 一律预扣）。
func (e *Engine) freeModelPreconsumeTrustCredits() (int64, error) {
	n, err := e.configInt64(ConfigFreeModelPreconsumeTrustCredits, defaultFreeModelPreconsumeTrust)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("%w: %s must be >= 0", ErrInvalidConfig, ConfigFreeModelPreconsumeTrustCredits)
	}
	return n, nil
}

// freeModelHold 计算免费模型预扣额：ceil(maxTokens × multiplier)。
//
// 用 decimal 乘法 + 取整，再走 QuotaFromDecimalStrict：越界（int32 上界）
// 返回错误而不是静默饱和——饱和的超大预扣会变成「额度不足/价格错误」的
// 阻断路径（billing-07 §2.3），绝不静默回绕。
func freeModelHold(maxTokens int, multiplier decimal.Decimal) (int64, error) {
	if maxTokens <= 0 {
		return 0, nil
	}
	ceil := decimal.NewFromInt(int64(maxTokens)).Mul(multiplier).Ceil()
	hold, err := pricing.QuotaFromDecimalStrict(ceil)
	if err != nil {
		return 0, fmt.Errorf("billing: free model pre-consume: %w", err)
	}
	return hold, nil
}

// cumulativeConsumption 返回用户历史累计消费积分（Σ consume 流水的绝对值）。
//
// 只统计 type='consume'：预扣 hold 不是消费；release/reversal 是退回/冲正，
// 不应计入「历史累计消费」阈值。SUM(-amount) 直接得到正数消耗额。
//
// 使用 historyConsumptionTTL 的短 TTL 缓存（见常量注释）；查询失败不缓存，
// 按错误上抛（免费模型请求宁可失败也不误判可信）。
func (e *Engine) cumulativeConsumption(ctx context.Context, userID int64) (int64, error) {
	ttl := e.historyTTL
	if ttl == 0 {
		ttl = historyConsumptionTTL
	}
	now := time.Now()

	e.historyMu.Lock()
	if ttl > 0 {
		if entry, ok := e.historyCache[userID]; ok && now.Before(entry.expires) {
			e.historyMu.Unlock()
			return entry.credits, nil
		}
	}
	e.historyMu.Unlock()

	var credits int64
	err := e.db.WithContext(ctx).Raw(
		`SELECT COALESCE(SUM(-amount), 0) FROM wallet_transactions
		 WHERE user_id = ? AND type = 'consume'`, userID).Row().Scan(&credits)
	if err != nil {
		return 0, fmt.Errorf("billing: sum historical consumption for user %d: %w", userID, err)
	}

	if ttl > 0 {
		e.historyMu.Lock()
		e.historyCache[userID] = historyEntry{credits: credits, expires: now.Add(ttl)}
		e.historyMu.Unlock()
	}
	return credits, nil
}

// quotaPerUnit 返回积分换算基准（默认 100）。
func (e *Engine) quotaPerUnit() (int64, error) {
	v, err := e.configFloat(ConfigQuotaPerUnit, defaultQuotaPerUnit)
	if err != nil {
		return 0, err
	}
	if v <= 0 {
		return 0, fmt.Errorf("%w: %s must be > 0", ErrInvalidConfig, ConfigQuotaPerUnit)
	}
	q, err := pricing.QuotaFromFloatStrict(v)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %v", ErrInvalidConfig, ConfigQuotaPerUnit, err)
	}
	return q, nil
}

// preConsumeRatio 返回预扣比例（默认 1.0；≤0 拒绝）。
func (e *Engine) preConsumeRatio() (float64, error) {
	v, err := e.configFloat(ConfigPreConsumeRatio, defaultPreConsumeRatio)
	if err != nil {
		return 0, err
	}
	if v <= 0 {
		// 配置错误不得静默取 0：0 会把所有预扣变成 0，等价于关闭保护。
		return 0, fmt.Errorf("%w: %s must be > 0", ErrInvalidConfig, ConfigPreConsumeRatio)
	}
	return v, nil
}

// trustQuotaUSD 返回信任旁路阈值（默认 10 USD；0 = 禁用；负值拒绝）。
func (e *Engine) trustQuotaUSD() (float64, error) {
	v, err := e.configFloat(ConfigTrustQuotaUSD, defaultTrustQuotaUSD)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, fmt.Errorf("%w: %s must be >= 0", ErrInvalidConfig, ConfigTrustQuotaUSD)
	}
	return v, nil
}

// preConsumeMultiplier 校验快照冻结的预扣上浮系数（billing-03 §3.2）。
//
// 旧快照没有该字段（零值）按默认 1 处理（G7-R05 改判，原 1.2 废止）；
// 已配置的负值/非有限值属于配置错误，拒绝而不是静默取 1。
func (e *Engine) preConsumeMultiplier(snap *contract.PriceSnapshot) (float64, error) {
	if snap == nil {
		return 1, nil
	}
	v := snap.PreConsumeMultiplier
	if v == 0 {
		return 1, nil
	}
	if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%w: pre_consume_multiplier must be a finite positive number", ErrInvalidConfig)
	}
	return v, nil
}

// shouldTrust 判定是否命中信任额度旁路（billing-00 §6.4 条件全集）。
//
// 全部满足才命中：① 非强制预扣；② 阈值是正有限值；③ 令牌无限额或余额超阈；
// ④ 钱包余额超阈；⑤ 订阅侧不得旁路（M3 只有钱包资金源，订阅实现属 M5）。
//
// budget 是本次预扣的估算额：钱包还必须**先覆盖这笔预留**才考虑旁路
// （billing-03 §3.3「钱包仍需先覆盖估算预留才会考虑旁路」）。否则信任放行
// 会把「余额不足」推迟到结算，违背 402 前置拦截。
//
// 契约 QuotaEstimate 没有 ForcePreConsume 字段（M2 冻结时遗漏），这里以
// 「任务时长 / 固定价键」作为任务路径的判据：异步任务必须预扣，禁用旁路。
// 该替代判据在报告中登记为接口缺口。
func (e *Engine) shouldTrust(ctx context.Context, rc *contract.RelayContext,
	est contract.QuotaEstimate, budget int64) (bool, error) {

	if e.db == nil || rc == nil {
		return false, nil
	}
	// ① 强制预扣（任务路径）禁用旁路。
	if est.TaskDuration > 0 || est.FixedPriceKey != "" {
		return false, nil
	}

	usd, err := e.trustQuotaUSD()
	if err != nil {
		return false, err
	}
	if usd <= 0 {
		return false, nil // 0 = 禁用旁路
	}
	perUnit, err := e.quotaPerUnit()
	if err != nil {
		return false, err
	}
	threshold, err := pricing.QuotaFromFloatStrict(usd * float64(perUnit))
	if err != nil {
		// 阈值越界不是请求的问题，但也不能静默按 0 处理；按配置错误上抛，
		// 让装配期/首个请求暴露问题。
		return false, fmt.Errorf("%w: trust threshold: %v", ErrInvalidConfig, err)
	}
	if threshold <= 0 {
		return false, nil
	}

	// ③ 令牌侧：无限额（remain_quota IS NULL）或余额 > 阈值。
	tokenOK, err := e.tokenAboveThreshold(ctx, int64(rc.TokenID), threshold)
	if err != nil {
		return false, err
	}
	if !tokenOK {
		return false, nil
	}

	// ④ 钱包侧：余额 > 阈值。
	balance, err := e.walletBalance(ctx, int64(rc.UserID))
	if err != nil {
		return false, err
	}
	if balance <= threshold {
		return false, nil
	}
	if budget > 0 && balance < budget {
		// 余额超阈但覆盖不了本次估算预留：不得旁路。正常预扣路径会以
		// *QuotaError 返回 402（billing-03 §3.3、§3.4）。
		return false, nil
	}

	// ⑤ 订阅侧一律 false（订阅永不旁路，billing-00 §6.4）：订阅预扣要求
	// amount>0 并锁定订阅，旁路会与其内部账不一致（billing-03 §3.3）。
	// M3 没有订阅资金源，该条件对钱包请求不构成否决；M5 接入订阅时必须在
	// 此处为订阅资金源显式返回 false，不得默认放行。
	return true, nil
}

// tokenAboveThreshold 判断令牌侧是否满足旁路条件。
//
// remain_quota 为 NULL 表示不限额度（满足）；令牌行不存在按 fail-closed
// 处理（不旁路），因为无法证明其余额超阈。
func (e *Engine) tokenAboveThreshold(ctx context.Context, tokenID, threshold int64) (bool, error) {
	if tokenID <= 0 {
		// 没有绑定 API 令牌（渠道测试/内部调用）：视为无限额。
		return true, nil
	}
	var remain sql.NullInt64
	err := e.db.WithContext(ctx).Raw(`SELECT remain_quota FROM api_tokens WHERE id = ?`, tokenID).
		Row().Scan(&remain)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("billing: read token quota: %w", err)
	}
	if !remain.Valid {
		return true, nil
	}
	return remain.Int64 > threshold, nil
}

// walletBalance 读取用户钱包余额。
func (e *Engine) walletBalance(ctx context.Context, userID int64) (int64, error) {
	var balance int64
	err := e.db.WithContext(ctx).Raw(`SELECT balance FROM users WHERE id = ?`, userID).
		Row().Scan(&balance)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: user %d", ErrUserNotFound, userID)
	}
	if err != nil {
		return 0, fmt.Errorf("billing: read wallet balance: %w", err)
	}
	return balance, nil
}
