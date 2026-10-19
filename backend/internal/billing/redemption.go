package billing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// 本文件是 M5 兑换码的计费核心（modules-04 §4.F F-04-14/15/16；裁决
// P-4/P-5/P-6/P-15、GC）。
//
// 数据口径（platform-01 §9，11 列）：
//   - `redemption_codes` 只有 `code_hash`，明文码仅在创建响应中出现一次；
//     归一化 = trim → 大写 → Crockford 别名（I/L→1、O→0）→ SHA-256 小写 hex。
//   - 状态只有 unused/disabled/used；**expired 是虚拟态**
//     （status='unused' AND expires_at <= now()），绝不落库、没有过期 worker。
//   - 核销 = 一条条件 UPDATE（`status='unused' AND (expires_at IS NULL OR
//     expires_at > now())`）与入账在**同一事务**内完成（P-15）。并发双花
//     由 PostgreSQL 行锁 + 谓词重求值收敛为「恰一次成功」。
//
// 金额口径（P-6 / GC）：
//
//	到账积分 = ComputeCredits(code.credits, quota_per_unit, ratio)
//	         = floor(code.credits × quota_per_unit / 100 × ratio)
//
// ratio = GroupRatio(pay.topup_group_ratio, users.group)。全程整数/十进制，
// 任何 ≤0 倍率、非法 QPU 与 int64 溢出都拒绝，绝不饱和钳制。

// 兑换码字符集与长度由 security-03 §2.6 冻结：26 位 Crockford Base32
// （32^26 = 130 bit），去除易混淆的 I/L/O/U，不插入分隔符，统一大写。
const (
	RedemptionCodeLength = 26
	RedemptionAlphabet   = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// ConfigTopupGroupRatio 是充值/兑换的分组倍率键（jsonb `{group: ratio}`）。
// 与 ConfigQuotaPerUnit 同属冻结键名；缺键时默认 1.0（不阻断兑换）。
const ConfigTopupGroupRatio = "pay.topup_group_ratio"

// 生成/管理面的边界（modules-04 F-04-15 / P-9）。
const (
	redemptionCountMin     = 1
	redemptionCountMax     = 100
	redemptionBatchMaxRune = 20
	// redemptionCollisionRetries：哈希碰撞属天文概率，但重试 3 次后必须
	// 报错而不是无限循环（F-04-15 异常流程【假设】）。
	redemptionCollisionRetries = 3
)

// 兑换码错误面。调用方（HTTP）用 errors.Is 映射为不同文案/状态码。
var (
	// ErrRedemptionCodeInvalid 表示归一化后查无此码。
	ErrRedemptionCodeInvalid = errors.New("billing: invalid redemption code")
	// ErrRedemptionCodeUsed 表示码已被核销。
	ErrRedemptionCodeUsed = errors.New("billing: redemption code already used")
	// ErrRedemptionCodeDisabled 表示管理员已禁用该码。
	ErrRedemptionCodeDisabled = errors.New("billing: redemption code disabled")
	// ErrRedemptionCodeExpired 表示虚拟过期态（含 expires_at == now 边界）。
	ErrRedemptionCodeExpired = errors.New("billing: redemption code expired")
	// ErrInvalidRedemptionBatch 覆盖生成请求的 count/credits/batch/expires_at 校验。
	ErrInvalidRedemptionBatch = errors.New("billing: invalid redemption batch")
	// ErrRedemptionCodeCollision 表示连续重试后仍无法生成唯一哈希。
	ErrRedemptionCodeCollision = errors.New("billing: redemption code hash collision")
)

// RedemptionService 是兑换码生成与核销的应用服务。
//
// db 必须是主库 GORM 连接：条件 UPDATE、入账流水与 users 余额同库，
// 跨库事务无法保证「码核销 ↔ 入账」原子性。
type RedemptionService struct {
	db      *gorm.DB
	funding FundingSource
	cfg     ConfigReader
	now     func() time.Time
}

// NewRedemptionService 构造兑换码服务。cfg 可为 nil（测试/未接配置中心），
// 此时 quota_per_unit=100、倍率表为空（所有分组 1.0）——P-6 的冻结默认值。
func NewRedemptionService(db *gorm.DB, funding FundingSource, cfg ConfigReader) *RedemptionService {
	return &RedemptionService{db: db, funding: funding, cfg: cfg, now: time.Now}
}

// NormalizeRedemptionCode 是生成、兑换与迁移共用的唯一归一化实现。
//
// 大写化保证「小写输入可兑」；I/L→1、O→0 是 Crockford 的别名规则，用户
// 抄写时把 1/0 看成字母是最常见的输入错误。TrimSpace 放在最前：前后空白
// 不该改变码的语义。
func NormalizeRedemptionCode(raw string) string {
	normalized := strings.ToUpper(strings.TrimSpace(raw))
	if normalized == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(normalized))
	for _, r := range normalized {
		switch r {
		case 'I', 'L':
			b.WriteByte('1')
		case 'O':
			b.WriteByte('0')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// HashRedemptionCode 返回归一化码的 SHA-256 小写十六进制。
// 调用方必须传入 NormalizeRedemptionCode 的结果；本函数不再二次归一化，
// 避免「生成时已大写、兑换时又改一次」的两套语义。
func HashRedemptionCode(normalized string) string {
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// generateRedemptionCode 用 crypto/rand 生成 26 位 Crockford Base32。
//
// 【为什么取 b&31 而不是取模】字母表恰好 32 个字符，256 是 32 的整数倍，
// 每个字节低 5 位均匀分布；无需拒绝采样，也就没有取模偏置。
// crypto/rand 而非 math/rand：兑换码是可直接换钱的 bearer token。
func generateRedemptionCode() (string, error) {
	buf := make([]byte, RedemptionCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("billing: generate redemption code: %w", err)
	}
	out := make([]byte, RedemptionCodeLength)
	for i, b := range buf {
		out[i] = RedemptionAlphabet[b&31]
	}
	return string(out), nil
}

// GenerateCodes 批量生成兑换码并返回**明文**（仅此一次）。
//
// 同一事务内逐行 INSERT：任一枚失败则整批回滚，避免留下半批码。唯一约束
// `redemption_codes_code_hash_uq` 是碰撞的最终判据；应用层重试只负责换码。
func (s *RedemptionService) GenerateCodes(ctx context.Context, batchName *string,
	credits int64, count int, expiresAt *time.Time, createdBy int64) ([]string, error) {

	if err := validateRedemptionBatch(batchName, credits, count, expiresAt, s.now()); err != nil {
		return nil, err
	}
	if createdBy <= 0 {
		return nil, fmt.Errorf("%w: created_by must be > 0", ErrInvalidRedemptionBatch)
	}
	// 落库前 trim：批次名是人对人的分组标签，前后空白没有语义。
	trimmedBatch := strings.TrimSpace(*batchName)
	batchName = &trimmedBatch

	codes := make([]string, 0, count)
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i := 0; i < count; i++ {
			inserted := false
			for attempt := 0; attempt < redemptionCollisionRetries; attempt++ {
				plain, err := generateRedemptionCode()
				if err != nil {
					return err
				}
				hash := HashRedemptionCode(plain)
				err = tx.Exec(`
					INSERT INTO redemption_codes
						(code_hash, credits, status, batch_name, created_by, expires_at)
					VALUES (?, ?, 'unused', ?, ?, ?)`,
					hash, credits, batchName, createdBy, expiresAt).Error
				if err == nil {
					codes = append(codes, plain)
					inserted = true
					break
				}
				if isUniqueViolation(err) {
					continue
				}
				return fmt.Errorf("billing: insert redemption code: %w", err)
			}
			if !inserted {
				return fmt.Errorf("%w: retried %d times",
					ErrRedemptionCodeCollision, redemptionCollisionRetries)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}

func validateRedemptionBatch(batchName *string, credits int64, count int,
	expiresAt *time.Time, now time.Time) error {

	if count < redemptionCountMin || count > redemptionCountMax {
		return fmt.Errorf("%w: count must be between %d and %d, got %d",
			ErrInvalidRedemptionBatch, redemptionCountMin, redemptionCountMax, count)
	}
	if credits <= 0 {
		return fmt.Errorf("%w: credits must be > 0, got %d", ErrInvalidRedemptionBatch, credits)
	}
	if batchName == nil {
		return fmt.Errorf("%w: batch name is required", ErrInvalidRedemptionBatch)
	}
	name := strings.TrimSpace(*batchName)
	if name == "" || utf8.RuneCountInString(name) > redemptionBatchMaxRune {
		return fmt.Errorf("%w: batch name must be 1-%d characters",
			ErrInvalidRedemptionBatch, redemptionBatchMaxRune)
	}
	// 边界与兑换条件一致：expires_at 必须严格晚于 now，等于 now 即为过期。
	if expiresAt != nil && !expiresAt.After(now) {
		return fmt.Errorf("%w: expires_at must be in the future", ErrInvalidRedemptionBatch)
	}
	return nil
}

// Redeem 原子核销一枚兑换码并把额度记入用户余额，返回入账后的余额。
// 只需要余额的调用方使用本方法；需要展示「本次到账多少积分」时使用
// RedeemDetailed。
func (s *RedemptionService) Redeem(ctx context.Context, userID int64, rawCode string) (int64, error) {
	_, balanceAfter, err := s.RedeemDetailed(ctx, userID, rawCode)
	return balanceAfter, err
}

// RedeemDetailed 是 Redeem 的完整结果版本：credits 是本次实际入账的积分
// （已含 quota_per_unit 与分组倍率），balanceAfter 是入账后的余额快照。
//
// 事务内顺序（不可交换）：
//  1. 条件 UPDATE 抢码（status='unused' 且未过期），RETURNING id/credits；
//     affected=0 时在同一事务内 SELECT 诊断具体原因；
//  2. ComputeCredits 换算（失败整体回滚，码回到 unused）；
//  3. FundingSource.ApplyTx 加余额 + 恰一行 topup 流水。
//
// 比例/配置读取在事务外完成：它们只读配置与 users.group，不参与原子性；
// 放进事务只会延长持锁时间。
func (s *RedemptionService) RedeemDetailed(ctx context.Context, userID int64, rawCode string) (int64, int64, error) {
	if userID <= 0 {
		return 0, 0, fmt.Errorf("%w: user id must be > 0", ErrInvalidFundingCredit)
	}
	normalized := NormalizeRedemptionCode(rawCode)
	if normalized == "" {
		return 0, 0, ErrRedemptionCodeInvalid
	}
	hash := HashRedemptionCode(normalized)

	quotaPerUnit, err := s.quotaPerUnit()
	if err != nil {
		return 0, 0, err
	}
	ratios, err := s.topupGroupRatios()
	if err != nil {
		return 0, 0, err
	}
	group, err := s.userGroup(ctx, userID)
	if err != nil {
		return 0, 0, err
	}
	ratio, err := GroupRatio(ratios, group)
	if err != nil {
		return 0, 0, err
	}

	var (
		credits      int64
		balanceAfter int64
	)
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row struct {
			ID      int64
			Credits int64
		}
		res := tx.Raw(`
			UPDATE redemption_codes
			SET status = 'used', used_by = ?, used_at = now(), updated_at = now()
			WHERE code_hash = ? AND status = 'unused'
			  AND (expires_at IS NULL OR expires_at > now())
			RETURNING id, credits`, userID, hash).Scan(&row)
		if res.Error != nil {
			return fmt.Errorf("billing: redeem code update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return diagnoseRedemption(tx, hash)
		}

		amount, err := ComputeCredits(row.Credits, quotaPerUnit, ratio)
		if err != nil {
			return err
		}
		// P-6：向下取整后为 0 同样是非法入账（极小倍率会把它抹平）。
		// 拒绝而不是写入 0 积分流水——流水金额必须为正，且用户应看到
		// 明确的配置错误而不是「兑换成功但没到账」。
		if amount <= 0 {
			return fmt.Errorf("%w: code %d credits %d floor to zero (qpu=%d ratio=%s)",
				ErrInvalidFundingCredit, row.ID, row.Credits, quotaPerUnit, ratio)
		}
		credited, _, err := s.funding.ApplyTx(ctx, tx, Credit{
			UserID:        userID,
			Amount:        amount,
			Type:          CreditTypeTopup,
			ReferenceType: "redemption",
			ReferenceID:   strconv.FormatInt(row.ID, 10),
			Note:          "redemption code",
		})
		if err != nil {
			return err
		}
		credits = amount
		balanceAfter = credited
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return credits, balanceAfter, nil
}

// diagnoseRedemption 在条件 UPDATE 影响 0 行后区分四种失败原因。
//
// Q：为什么区分原因而不是统一报「无效」？F-04-14 要求四种文案——用户
// 需要知道已使用/已过期/已禁用与拼错的处置完全不同（重试 vs 联系客服）。
// 这里只泄露「该码存在」以外的状态；不返回码值或用户信息。
func diagnoseRedemption(tx *gorm.DB, hash string) error {
	var row struct {
		Status  string
		Expired bool
	}
	res := tx.Raw(`
		SELECT status,
		       (expires_at IS NOT NULL AND expires_at <= now()) AS expired
		FROM redemption_codes WHERE code_hash = ?`, hash).Scan(&row)
	if res.Error != nil {
		return fmt.Errorf("billing: diagnose redemption code: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrRedemptionCodeInvalid
	}
	switch row.Status {
	case "used":
		return ErrRedemptionCodeUsed
	case "disabled":
		return ErrRedemptionCodeDisabled
	case "unused":
		if row.Expired {
			return ErrRedemptionCodeExpired
		}
	}
	// unused 且未过期却更新失败：通常是并发已把状态改走；
	// 按「已使用」返回，保证调用方拿到可重试语义而非内部错误。
	return ErrRedemptionCodeUsed
}

// userGroup 读取用户分组；用户不存在时返回 ErrUserNotFound。
func (s *RedemptionService) userGroup(ctx context.Context, userID int64) (string, error) {
	var group string
	err := s.db.WithContext(ctx).Raw(`SELECT "group" FROM users WHERE id = ?`, userID).
		Row().Scan(&group)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: user %d", ErrUserNotFound, userID)
	}
	if err != nil {
		return "", fmt.Errorf("billing: read user group for %d: %w", userID, err)
	}
	return group, nil
}

// quotaPerUnit 读取 billing.quota_per_unit（P-6 冻结默认 100）。
// 接受 JSON 数字或数值字符串（与 Engine/pricing 同口径）；非整数或越界
// 一律拒绝——错误汇率会直接放大或缩小到账积分。
func (s *RedemptionService) quotaPerUnit() (int64, error) {
	if s.cfg == nil {
		return DefaultQuotaPerUnit, nil
	}
	raw, ok := s.cfg.Get(ConfigQuotaPerUnit)
	if !ok {
		return DefaultQuotaPerUnit, nil
	}
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return DefaultQuotaPerUnit, nil
	}
	if strings.HasPrefix(text, `"`) {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return 0, fmt.Errorf("%w: %v", ErrInvalidQuotaPerUnit, err)
		}
		text = strings.TrimSpace(str)
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value < 1 || value > 1_000_000 {
		return 0, fmt.Errorf("%w: %q must be an integer in 1-1000000", ErrInvalidQuotaPerUnit, text)
	}
	return value, nil
}

// topupGroupRatios 读取 pay.topup_group_ratio；缺键/空值视为 {}（全 1.0）。
func (s *RedemptionService) topupGroupRatios() (map[string]decimal.Decimal, error) {
	if s.cfg == nil {
		return map[string]decimal.Decimal{}, nil
	}
	raw, ok := s.cfg.Get(ConfigTopupGroupRatio)
	if !ok {
		return map[string]decimal.Decimal{}, nil
	}
	return ParseTopupGroupRatios(raw)
}
