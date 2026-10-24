package billing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

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
