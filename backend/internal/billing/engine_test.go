package billing

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
)

// TestEngineNewSessionBindsRelayContext 验证工厂在缺少请求上下文时安全失败：
// 未绑定 requestID 的会话不得访问账本（返回 ErrMissingRequestID）。
func TestEngineNewSessionBindsRelayContext(t *testing.T) {
	e := NewEngine(nil, nil, nil, nil, nil, nil, nil)
	s := e.NewSession(nil)
	if s == nil {
		t.Fatal("NewSession(nil) = nil")
	}
	if s.Reserved() != 0 {
		t.Fatalf("Reserved = %d, want 0", s.Reserved())
	}
	if s.NeedsRefund() {
		t.Fatal("NeedsRefund = true, want false")
	}
	if err := s.PreConsume(contract.QuotaEstimate{}); !errors.Is(err, ErrMissingRequestID) {
		t.Fatalf("PreConsume = %v, want ErrMissingRequestID", err)
	}
	if err := s.Terminate(contract.UsageFact{}); !errors.Is(err, ErrMissingRequestID) {
		t.Fatalf("Terminate = %v, want ErrMissingRequestID", err)
	}
	if err := s.Refund(); !errors.Is(err, ErrMissingRequestID) {
		t.Fatalf("Refund = %v, want ErrMissingRequestID", err)
	}
	if err := s.RecoverAbandoned(""); !errors.Is(err, ErrMissingRequestID) {
		t.Fatalf("RecoverAbandoned = %v, want ErrMissingRequestID", err)
	}
}

// TestEngineConfigValidation 验证预扣比例与上浮系数的边界：
// ≤0 / 非有限值一律拒绝，绝不静默取 0 或 1（G7-R05）。
func TestEngineConfigValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  ConfigReader
		snap *contract.PriceSnapshot
	}{
		{
			name: "pre_consume_ratio = 0",
			cfg:  mapConfig{ConfigPreConsumeRatio: json.RawMessage("0")},
			snap: perCallSnapshot(100),
		},
		{
			name: "pre_consume_ratio 非有限",
			cfg:  mapConfig{ConfigPreConsumeRatio: json.RawMessage(`"NaN"`)},
			snap: perCallSnapshot(100),
		},
		{
			name: "pre_consume_multiplier 为负",
			cfg:  billingTestConfig(),
			snap: func() *contract.PriceSnapshot {
				s := perCallSnapshot(100)
				s.PreConsumeMultiplier = -1
				return s
			}(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEngine(nil, nil, nil, tc.cfg, nil, nil, nil)
			s := newBillingTestSession(e, 1, testRequestID("cfg"), tc.snap)
			err := s.PreConsume(contract.QuotaEstimate{Model: "m"})
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("PreConsume = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

// TestEnginePreConsumeRatioApplied 验证 billing.pre_consume_ratio 乘在预算上。
func TestEnginePreConsumeRatioApplied(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	cfg := mapConfig{
		ConfigPreConsumeRatio: json.RawMessage("0.5"),
		ConfigTrustQuotaUSD:   json.RawMessage("0"),
	}
	e := NewEngine(gormDB, nil, nil, cfg, nil, nil, nil)
	req := testRequestID("ratio-applied")
	s := newBillingTestSession(e, userID, req, perCallSnapshot(100))

	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got := s.Reserved(); got != 50 {
		t.Fatalf("Reserved = %d, want 50（100 × 0.5）", got)
	}
	if got := userBalance(t, db, userID); got != 450 {
		t.Fatalf("balance = %d, want 450", got)
	}
}

// TestEnginePreConsumeMultiplierScope 验证上浮系数的作用域：
// 传统倍率生效、按次不生效（billing-03 §3.2）。
func TestEnginePreConsumeMultiplierScope(t *testing.T) {
	t.Run("传统倍率生效", func(t *testing.T) {
		gormDB, db := openBillingTestDB(t)
		userID := createBillingTestUser(t, db, 500)
		e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
		snap := ratioSnapshot(100, 100)
		snap.PreConsumeMultiplier = 2
		req := testRequestID("mult-ratio")
		s := newBillingTestSession(e, userID, req, snap)
		if err := s.PreConsume(contract.QuotaEstimate{Model: "m", PromptTokens: 1_000_000}); err != nil {
			t.Fatalf("PreConsume: %v", err)
		}
		if got := s.Reserved(); got != 200 {
			t.Fatalf("Reserved = %d, want 200（1M tokens × 100 × 2）", got)
		}
	})

	t.Run("按次不生效", func(t *testing.T) {
		gormDB, db := openBillingTestDB(t)
		userID := createBillingTestUser(t, db, 500)
		e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
		snap := perCallSnapshot(100)
		snap.PreConsumeMultiplier = 2
		req := testRequestID("mult-percall")
		s := newBillingTestSession(e, userID, req, snap)
		if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
			t.Fatalf("PreConsume: %v", err)
		}
		if got := s.Reserved(); got != 100 {
			t.Fatalf("Reserved = %d, want 100（按次不乘上浮系数）", got)
		}
	})
}

// TestEngineTokenAboveThreshold 覆盖信任旁路条件③的令牌侧判定：
// remain_quota NULL = 无限额（满足）；有值须 > 阈值；令牌不存在 fail-closed。
func TestEngineTokenAboveThreshold(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 100)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	ctx := context.Background()

	insertToken := func(remain any) int64 {
		t.Helper()
		var id int64
		err := db.QueryRow(
			`INSERT INTO api_tokens (user_id, name, key_hash, "group", remain_quota)
			 VALUES ($1, 'tok', $2, 'default', $3) RETURNING id`,
			userID, testRequestID("keyhash"), remain).Scan(&id)
		if err != nil {
			t.Fatalf("insert token: %v", err)
		}
		return id
	}

	unlimited := insertToken(nil)
	high := insertToken(int64(2000))
	low := insertToken(int64(500))

	tests := []struct {
		name    string
		tokenID int64
		want    bool
	}{
		{"无限额（NULL）满足", unlimited, true},
		{"余额 2000 > 阈值 1000", high, true},
		{"余额 500 <= 阈值 1000", low, false},
		{"未绑定令牌视为无限额", 0, true},
		{"令牌不存在 fail-closed", 9_999_999, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := e.tokenAboveThreshold(ctx, tc.tokenID, 1000)
			if err != nil {
				t.Fatalf("tokenAboveThreshold: %v", err)
			}
			if ok != tc.want {
				t.Fatalf("tokenAboveThreshold = %v, want %v", ok, tc.want)
			}
		})
	}
}

// TestEnginePreConsumeMultiplierDefault 验证旧快照的零值按默认 1 处理。
func TestEnginePreConsumeMultiplierDefault(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	e := NewEngine(gormDB, nil, nil, billingTestConfig(), nil, nil, nil)
	snap := perCallSnapshot(100)
	snap.PreConsumeMultiplier = 0 // 旧快照缺字段
	req := testRequestID("mult-default")
	s := newBillingTestSession(e, userID, req, snap)
	if err := s.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if got := s.Reserved(); got != 100 {
		t.Fatalf("Reserved = %d, want 100（零值按默认 1）", got)
	}
}

// TestEngineSettlementLock 验证 R-41 的 requestID 锁语义：
// 同一键二次获取返回 ErrSettlementInProgress；释放后可重获；Redis 不可用
// 时 fail closed。
func TestEngineSettlementLock(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{
		Addr:        mr.Addr(),
		MaxRetries:  -1, // 关闭重试：Redis 不可用路径要快速返回
		DialTimeout: 200 * time.Millisecond,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	e := NewEngine(nil, nil, rdb, nil, nil, nil, nil)
	ctx := context.Background()

	release1, err := e.acquireSettlementLock(ctx, "req-lock")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := e.acquireSettlementLock(ctx, "req-lock"); !errors.Is(err, ErrSettlementInProgress) {
		t.Fatalf("second acquire = %v, want ErrSettlementInProgress", err)
	}
	release1()
	release2, err := e.acquireSettlementLock(ctx, "req-lock")
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()

	// Redis 不可用：fail closed（不得把锁故障当成可结算）。
	mr.Close()
	if _, err := e.acquireSettlementLock(ctx, "req-lock"); err == nil {
		t.Fatal("acquire with redis down = nil, want error")
	}

	// rdb 为 nil（单测/未接线）：no-op 释放，不报错。
	if release, err := NewEngine(nil, nil, nil, nil, nil, nil, nil).
		acquireSettlementLock(ctx, "req-lock"); err != nil {
		t.Fatalf("nil rdb acquire: %v", err)
	} else {
		release()
	}
}

func TestEngineOutputReserveRejectsDefaultAboveMax(t *testing.T) {
	cfg := mapConfig{
		ConfigPreConsumeOutputReserveDefault: json.RawMessage("4096"),
		ConfigPreConsumeOutputReserveMax:     json.RawMessage("1024"),
	}
	e := NewEngine(nil, nil, nil, cfg, nil, nil, nil)
	if _, err := e.outputReserve(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

// TestScalePreConsumeQuotaInexactFactorDrift 锁定预扣缩放中的 float64 中间态
// 边界（audit P1-11）。
//
// 约定（billing-03 §5.4 / billing-07 §2.1）：预扣的 float 乘积走
// QuotaFromFloatStrict，即向零截断。base 已被 ComputeQuota 钳在 int32 域
// （≤2^31−1），float64 相对误差 <2^-52，故「float64 乘积的截断」与「十进制
// 精确乘积的截断」最多相差 1 积分。预扣只是 hold；结算走
// QuotaFromDecimalChecked（十进制、半离零），这点误差不会进入实收。
func TestScalePreConsumeQuotaInexactFactorDrift(t *testing.T) {
	cases := []struct {
		name   string
		base   int64
		factor float64
		want   int64 // 当前实现（float64 乘积向零截断）的值
		exact  int64 // 十进制精确乘积的向零截断值
	}{
		{"100 × 0.1", 100, 0.1, 10, 10},
		{"100 × 1/3", 100, 1.0 / 3.0, 33, 33},
		{"100000 × 0.1（大用量）", 100_000, 0.1, 10_000, 10_000},
		{"2147483647 × 0.1（int32 上界）", contract.MaxQuota, 0.1, 214_748_364, 214_748_364},
		{"2147483647 × 1/3（int32 上界）", contract.MaxQuota, 1.0 / 3.0, 715_827_882, 715_827_882},
		// 90 × 0.7 = 63，但 float64 乘积为 62.99999999999999，向零截断得
		// 62——P1-11 要钉住的 1 积分下穿（仍在 ≤1 的误差界内）。
		{"90 × 0.7（float64 下穿整数）", 90, 0.7, 62, 63},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scalePreConsumeQuota(tc.base, tc.factor)
			if err != nil {
				t.Fatalf("scalePreConsumeQuota: %v", err)
			}
			if got != tc.want {
				t.Fatalf("quota = %d, want %d（向零截断的 float64 乘积）", got, tc.want)
			}
			if diff := got - tc.exact; diff < -1 || diff > 1 {
				t.Fatalf("与十进制精确乘积差 %d 积分，超出 ≤1 的 float64 误差界", diff)
			}
		})
	}
}

// TestScalePreConsumeQuotaOverflowBlocks 验证缩放越界时由 Strict 阻断，
// 不会静默饱和成 MaxQuota 后继续冻结（billing-07 §2.1 的预扣语义）。
func TestScalePreConsumeQuotaOverflowBlocks(t *testing.T) {
	q, err := scalePreConsumeQuota(90, 1e9) // 9e10 > MaxInt32
	if q != 0 || err == nil {
		t.Fatalf("越界应返回 (0,error)，got q=%d err=%v", q, err)
	}
	clamp, ok := pricing.AsClamp(err)
	if !ok || clamp.Kind != contract.ClampOverflow || clamp.Op != "QuotaFromFloatStrict" {
		t.Fatalf("应为 QuotaFromFloatStrict 溢出标记，got %v", err)
	}

	// 恰好 MaxInt32 界内：不产生标记。
	if q, err := scalePreConsumeQuota(int64(contract.MaxQuota), 1); q != contract.MaxQuota || err != nil {
		t.Fatalf("上界内应通过，got q=%d err=%v", q, err)
	}
}

// TestPreConsumeMultiplierRejectsNonFinite 补齐 pre_consume_multiplier 的
// 非有限值守卫。负值（TestEngineConfigValidation）与零值默认 1
// （TestEnginePreConsumeMultiplierDefault）已有覆盖，这里只补 NaN/±Inf。
func TestPreConsumeMultiplierRejectsNonFinite(t *testing.T) {
	e := NewEngine(nil, nil, nil, nil, nil, nil, nil)
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := e.preConsumeMultiplier(&contract.PriceSnapshot{PreConsumeMultiplier: v}); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("multiplier=%v 应拒绝，got %v", v, err)
		}
	}
	// 0.1/1/3 这类合法非精确小数必须原样冻结，不得被取整或拒绝。
	for _, v := range []float64{0.1, 1.0 / 3.0} {
		got, err := e.preConsumeMultiplier(&contract.PriceSnapshot{PreConsumeMultiplier: v})
		if err != nil || got != v {
			t.Errorf("multiplier=%v 应原样返回，got %v err=%v", v, got, err)
		}
	}
}
