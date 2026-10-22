package billing

// 每日签到入账（modules-03 F-03-12；流程侧 modules-01 §3.19 F-01-30）。
//
// 本文件只负责「一次签到 = 一行 checkins + 至多一行 checkin_bonus 流水」的
// 原子实现，以及月历/连签所需的只读聚合。风控（限流、Turnstile）与 HTTP
// 语义在 httpapi/checkin handler；配置面在 settings 注册表。
//
// 冻结口径（W15-C 裁决）：
//   - 默认关闭：checkin.enabled 注册表默认 false；未启用时 **不进入事务**，
//     调用方据此返回固定的 403（零副作用）。
//   - 一天一次：`checkins (user_id, checkin_date)` 唯一约束是唯一兜底，
//     不靠「先查后写」；并发双击由数据库收敛为「恰好一次成功」。
//   - 同事务：签到记录、余额变更、checkin_bonus 流水在同一个数据库事务内。
//     任一步失败整体回滚——绝不留下「签到了但没发额度」或反向状态
//     （参照侧 SQLite 分支的手动补偿正是 T-05 要改掉的做法）。
//   - 自然日：以站点时区（site.timezone）判定「今天」，非法时区回落 UTC
//     并由调用方的 location provider 记录 warn（I-19、R-W15-15）。
//   - 随机额：闭区间 [min, max]；min = 0 时停发（只写签到行、不写流水）。
//     **随机值在插入前固定并随签到行落库**，重放（同日第二次）读取已存金额，
//     不会重新抽取，也不会产生第二笔入账。
//   - 钱包上界：余额加法沿用账本的 2^53−1 谓词；超界返回 ErrWalletLimit，
//     整个事务回滚（包括签到行），HTTP 层映射 409，不做部分入账。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"newtg/gateway/internal/settings"
)

// 签到设置键（modules-08 §3.8、system-config-mapping §4.7 #32–#34）。
const (
	ConfigCheckinEnabled  = "checkin.enabled"
	ConfigCheckinMinQuota = "checkin.min_quota"
	ConfigCheckinMaxQuota = "checkin.max_quota"
)

// 签到领域的哨兵错误。HTTP 层用 errors.Is 映射状态码，绝不回显内部细节。
var (
	// ErrCheckinDisabled 表示站点总开关关闭。调用方必须在任何写入/限流/
	// 人机校验之前返回固定拒绝（W15-C 裁决 1）。
	ErrCheckinDisabled = errors.New("billing: checkin disabled")
	// ErrCheckinAlreadyDone 表示当日已签到（唯一约束或时区切换守卫命中）。
	// 这仍是一次可重放的查询结果，不是内部错误。
	ErrCheckinAlreadyDone = errors.New("billing: already checked in today")
)

// checkinDateLayout 是 checkins.checkin_date 的规范字符串形态（date 列）。
const checkinDateLayout = "2006-01-02"

// CheckinSettings 是一次签到判定所需的设置快照。
type CheckinSettings struct {
	Enabled  bool
	MinQuota int64
	MaxQuota int64
	// Location 是「今天」的判定时区（site.timezone 解析结果，非法回落 UTC）。
	Location *time.Location
}

// CheckinResult 是一次签到（或重放）的结果投影。
type CheckinResult struct {
	// Date 是本次（或当日已存）签到对应的自然日，YYYY-MM-DD。
	Date string
	// Awarded 是本次发放的积分；同日重放返回首次落库的金额。
	Awarded int64
	// BalanceAfter 是入账后的余额快照（min=0 时为当前余额）。
	BalanceAfter int64
	// AlreadyCheckedIn 为 true 表示当日已签到（零写入，重放金额）。
	AlreadyCheckedIn bool
	// Streak 是截至今天的连续签到天数；MonthTotal / TotalEarned 用于卡片。
	Streak      int32
	MonthTotal  int64
	TotalEarned int64
}

// CheckinDay 是月历中的一天。
type CheckinDay struct {
	Date    string `json:"date"`
	Awarded int64  `json:"awarded"`
}

// CheckinCalendar 是月历 + 统计的只读投影。
type CheckinCalendar struct {
	Month          string
	Today          string
	Days           []CheckinDay
	Streak         int32
	MonthTotal     int64
	TotalEarned    int64
	CheckedInToday bool
}

// CheckinServiceConfig 是签到服务的装配输入。
type CheckinServiceConfig struct {
	// DB 必须是主库 GORM 连接：checkins 与 wallet_transactions 同库同事务。
	DB *gorm.DB
	// Config 是配置中心读取面；nil 时全部使用注册表默认值（测试缝隙）。
	Config ConfigReader
	// Logger 用于非法配置/时区的告警；nil 时使用 slog.Default()。
	Logger *slog.Logger
	// Now 是时钟接缝（站点时区边界测试）。nil 时 time.Now。
	Now func() time.Time
	// DrawInt64n 返回 [0, n) 的均匀随机整数（随机额接缝）。nil 时使用
	// crypto/rand 的拒绝采样实现。
	DrawInt64n func(n int64) (int64, error)
	// Location 返回站点时区。生产由 httpapi 复用 demoQuotaLocation 的
	// 同款解析（site.timezone → 注册表默认 → UTC，非法时 warn）；nil 时 UTC。
	Location func() *time.Location
}

// CheckinService 是签到与入账的应用服务，可并发使用。
type CheckinService struct {
	db         *gorm.DB
	cfg        ConfigReader
	log        *slog.Logger
	now        func() time.Time
	drawInt64n func(n int64) (int64, error)
	location   func() *time.Location
}

// NewCheckinService 构造签到服务。DB 必填：签到与入账的原子性依赖主库事务。
func NewCheckinService(cfg CheckinServiceConfig) (*CheckinService, error) {
	if cfg.DB == nil {
		return nil, errors.New("billing: CheckinServiceConfig.DB is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	draw := cfg.DrawInt64n
	if draw == nil {
		draw = secureRandInt64n
	}
	return &CheckinService{
		db:         cfg.DB,
		cfg:        cfg.Config,
		log:        logger,
		now:        now,
		drawInt64n: draw,
		location:   cfg.Location,
	}, nil
}

// Settings 读取当前签到设置快照。缺失/脏值回落注册表默认值并告警，
// 绝不让一次配置读失败变成「按 0 元发放」或「意外开启」。
func (s *CheckinService) Settings() CheckinSettings {
	enabled := s.boolSetting(ConfigCheckinEnabled, registryBoolDefault(ConfigCheckinEnabled, false))
	minQuota := s.int64Setting(ConfigCheckinMinQuota, registryInt64Default(ConfigCheckinMinQuota, 0))
	maxQuota := s.int64Setting(ConfigCheckinMaxQuota, registryInt64Default(ConfigCheckinMaxQuota, 0))
	// 注册表 section 校验保证保存期 min ≤ max；这里再兜底一次，避免历史
	// 脏行/直改 DB 造成「负数跨度」把随机抽取变成 panic 或越界发放。
	if maxQuota < minQuota {
		s.log.Warn("checkin: max_quota below min_quota; clamping to min_quota",
			"min_quota", minQuota, "max_quota", maxQuota)
		maxQuota = minQuota
	}
	loc := time.UTC
	if s.location != nil {
		if resolved := s.location(); resolved != nil {
			loc = resolved
		}
	}
	return CheckinSettings{
		Enabled:  enabled,
		MinQuota: minQuota,
		MaxQuota: maxQuota,
		Location: loc,
	}
}

// Enabled 是 handler 在限流/人机校验之前判定「是否还有签到功能」的入口。
func (s *CheckinService) Enabled() bool {
	return s.boolSetting(ConfigCheckinEnabled, registryBoolDefault(ConfigCheckinEnabled, false))
}

// CheckIn 执行一次签到。返回 AlreadyCheckedIn=true 是**成功重放**（零写入），
// 调用方按产品语义映射 409；其他错误按哨兵类型映射。
func (s *CheckinService) CheckIn(ctx context.Context, userID int64) (CheckinResult, error) {
	if userID <= 0 {
		return CheckinResult{}, fmt.Errorf("%w: user id must be > 0", ErrUserNotFound)
	}
	cfg := s.Settings()
	if !cfg.Enabled {
		return CheckinResult{}, ErrCheckinDisabled
	}

	now := s.now()
	today := now.In(cfg.Location).Format(checkinDateLayout)
	// 关闭型守卫：站点时区被管理员改动后，同一 UTC 日内可能出现「更早的
	// 本地日」。唯一约束只能拦住相同 checkin_date，拦不住「倒退一天」的
	// 二次领取；本地零点起点（R-W15-15 的 EXISTS 守卫）覆盖这一边界。
	dayStart := startOfDay(now, cfg.Location)

	award, err := drawCheckinAward(cfg.MinQuota, cfg.MaxQuota, s.drawInt64n)
	if err != nil {
		return CheckinResult{}, err
	}

	var out CheckinResult
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var blocked bool
		if err := tx.WithContext(ctx).Raw(
			`SELECT EXISTS(SELECT 1 FROM checkins WHERE user_id = ? AND created_at >= ?)`,
			userID, dayStart).Row().Scan(&blocked); err != nil {
			return fmt.Errorf("billing: check same-day guard: %w", err)
		}
		if blocked {
			return replayCheckinTx(ctx, tx, userID, cfg.Location, &out)
		}

		var checkinID int64
		// 唯一约束是「一天一次」的唯一兜底：并发双击的后者在这里得到 0 行，
		// 事务保持可用（DO NOTHING 不会让 PostgreSQL 中止整个事务）。
		err := tx.WithContext(ctx).Raw(`
			INSERT INTO checkins (user_id, checkin_date, quota_awarded)
			VALUES (?, CAST(? AS date), ?)
			ON CONFLICT (user_id, checkin_date) DO NOTHING
			RETURNING id`, userID, today, award).Row().Scan(&checkinID)
		if errors.Is(err, sql.ErrNoRows) {
			return replayCheckinTx(ctx, tx, userID, cfg.Location, &out)
		}
		if err != nil {
			return fmt.Errorf("billing: insert checkin: %w", err)
		}

		out = CheckinResult{Date: today, Awarded: award}
		if award > 0 {
			// 账本行与签到行同事务；reference_id=checkins.id 让
			// uq_wallet_tx_checkin 部分唯一索引成为「一次签到恰一行流水」的
			// DB 级锚点（迁移 20261024090000）。
			balanceAfter, _, err := applyDeltaTxNote(ctx, tx, userID, award,
				"checkin_bonus", "confirmed", "checkin", strconv.FormatInt(checkinID, 10), "")
			if err != nil {
				return err
			}
			out.BalanceAfter = balanceAfter
		} else {
			// min=0：不发额度、不写账本行，但仍返回余额快照供卡片显示。
			if err := tx.WithContext(ctx).Raw(
				`SELECT balance FROM users WHERE id = ?`, userID).Row().Scan(&out.BalanceAfter); err != nil {
				return fmt.Errorf("billing: read balance for user %d: %w", userID, err)
			}
		}
		return fillCheckinStatsTx(ctx, tx, userID, today, cfg.Location, &out)
	})
	if err != nil {
		return CheckinResult{}, err
	}
	return out, nil
}

// Calendar 返回指定月份（YYYY-MM）的签到日与统计；month 为空时取站点时区的
// 当前月。禁用状态下由 handler 先拒绝，本方法本身不判开关（历史可读）。
func (s *CheckinService) Calendar(ctx context.Context, userID int64, month string) (CheckinCalendar, error) {
	if userID <= 0 {
		return CheckinCalendar{}, fmt.Errorf("%w: user id must be > 0", ErrUserNotFound)
	}
	cfg := s.Settings()
	now := s.now()
	today := now.In(cfg.Location).Format(checkinDateLayout)

	month = strings.TrimSpace(month)
	if month == "" {
		month = today[:7]
	}
	// 月份格式在 handler 已校验；服务层再解析一次，防止内部调用绕过。
	if _, err := time.ParseInLocation("2006-01", month, cfg.Location); err != nil {
		return CheckinCalendar{}, fmt.Errorf("billing: invalid checkin month %q: %w", month, err)
	}

	rows, err := checkinRowsTx(ctx, s.db.WithContext(ctx), userID)
	if err != nil {
		return CheckinCalendar{}, err
	}
	streak, total, checkedToday := summarizeCheckins(rows, today, cfg.Location)

	out := CheckinCalendar{
		Month:          month,
		Today:          today,
		Days:           make([]CheckinDay, 0, 31),
		Streak:         streak,
		TotalEarned:    total,
		CheckedInToday: checkedToday,
	}
	for _, row := range rows {
		if !strings.HasPrefix(row.Date, month+"-") {
			continue
		}
		out.Days = append(out.Days, CheckinDay{Date: row.Date, Awarded: row.Awarded})
		out.MonthTotal += row.Awarded
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 事务内辅助
// ---------------------------------------------------------------------------

// replayCheckinTx 读取当日已存签到并填满结果（零写入）。
//
// 为什么按 user 取最新一行而不是按 today 查询：时区切换守卫可能命中一个
// 比「今天」更早的本地日（管理员把站点时区往西调）；此时最新一行才是用户
// 刚刚领取的那笔，重放必须返回同一金额。
func replayCheckinTx(ctx context.Context, tx *gorm.DB, userID int64,
	loc *time.Location, out *CheckinResult) error {

	var row struct {
		ID      int64
		Date    string
		Awarded int64
	}
	err := tx.WithContext(ctx).Raw(`
		SELECT id, to_char(checkin_date, 'YYYY-MM-DD'), quota_awarded
		FROM checkins WHERE user_id = ?
		ORDER BY checkin_date DESC, id DESC LIMIT 1`, userID).
		Row().Scan(&row.ID, &row.Date, &row.Awarded)
	if errors.Is(err, sql.ErrNoRows) {
		// 守卫命中却说没有行：数据不一致，宁可报错也不能伪造一笔签到。
		return ErrCheckinAlreadyDone
	}
	if err != nil {
		return fmt.Errorf("billing: read existing checkin: %w", err)
	}
	var balance int64
	if err := tx.WithContext(ctx).Raw(
		`SELECT balance FROM users WHERE id = ?`, userID).Row().Scan(&balance); err != nil {
		return fmt.Errorf("billing: read balance for user %d: %w", userID, err)
	}
	*out = CheckinResult{
		Date:             row.Date,
		Awarded:          row.Awarded,
		BalanceAfter:     balance,
		AlreadyCheckedIn: true,
	}
	return fillCheckinStatsTx(ctx, tx, userID, row.Date, loc, out)
}

// fillCheckinStatsTx 填充连续天数/本月累计/累计获得。
func fillCheckinStatsTx(ctx context.Context, tx *gorm.DB, userID int64,
	today string, loc *time.Location, out *CheckinResult) error {

	rows, err := checkinRowsTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	streak, total, _ := summarizeCheckins(rows, today, loc)
	out.Streak = streak
	out.TotalEarned = total
	out.MonthTotal = sumMonth(rows, today[:7])
	return nil
}

type checkinRow struct {
	Date    string
	Awarded int64
}

// checkinRowsTx 读取该用户全部签到行（按日期升序）。行数上限是「用户寿命内
// 每天一行」，远小于分页阈值；一次取回让连签/本月/累计三个口径必然一致。
func checkinRowsTx(ctx context.Context, tx *gorm.DB, userID int64) ([]checkinRow, error) {
	rows, err := tx.WithContext(ctx).Raw(`
		SELECT to_char(checkin_date, 'YYYY-MM-DD') AS day, quota_awarded
		FROM checkins WHERE user_id = ?
		ORDER BY checkin_date ASC`, userID).Rows()
	if err != nil {
		return nil, fmt.Errorf("billing: list checkins for user %d: %w", userID, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]checkinRow, 0, 32)
	for rows.Next() {
		var row checkinRow
		if err := rows.Scan(&row.Date, &row.Awarded); err != nil {
			return nil, fmt.Errorf("billing: scan checkin row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("billing: iterate checkin rows: %w", err)
	}
	return out, nil
}

// summarizeCheckins 计算连续天数、累计获得额与「今天是否已签」。
//
// 连续天数口径：从今天开始；今天未签则从昨天开始（保留「昨天连签、今天还
// 没签」的连续性），一旦遇到缺口立即停止。不实现连签递增奖励（O6 不做）。
func summarizeCheckins(rows []checkinRow, today string, loc *time.Location) (int32, int64, bool) {
	set := make(map[string]int64, len(rows))
	var total int64
	for _, row := range rows {
		set[row.Date] = row.Awarded
		total += row.Awarded
	}
	checkedToday := false
	if _, ok := set[today]; ok {
		checkedToday = true
	}
	cursor, err := time.ParseInLocation(checkinDateLayout, today, loc)
	if err != nil {
		// today 由本包按同一 layout 生成，解析失败只可能是调用方传入脏值；
		// 返回 0 而不是猜测一个日期。
		return 0, total, checkedToday
	}
	if !checkedToday {
		cursor = cursor.AddDate(0, 0, -1)
	}
	var streak int32
	for {
		if _, ok := set[cursor.Format(checkinDateLayout)]; !ok {
			break
		}
		streak++
		cursor = cursor.AddDate(0, 0, -1)
	}
	return streak, total, checkedToday
}

func sumMonth(rows []checkinRow, month string) int64 {
	var total int64
	for _, row := range rows {
		if strings.HasPrefix(row.Date, month+"-") {
			total += row.Awarded
		}
	}
	return total
}

// startOfDay 返回站点时区下的本地零点（转成绝对时间供 timestamptz 比较）。
func startOfDay(now time.Time, loc *time.Location) time.Time {
	local := now.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
}

// ---------------------------------------------------------------------------
// 随机额与设置读取
// ---------------------------------------------------------------------------

// drawCheckinAward 按模块 03 F-03-12 的金额规则抽取：
//   - max ≤ min：恒等于 min（min=max 是「定额」；max<min 已被设置校验拒绝，
//     这里按 min 兜底）；
//   - 否则 min + rand[0, max−min]，闭区间（与参照侧 rand.Intn(max-min+1) 同分布）。
func drawCheckinAward(minQuota, maxQuota int64, draw func(int64) (int64, error)) (int64, error) {
	if maxQuota <= minQuota {
		return minQuota, nil
	}
	span := maxQuota - minQuota + 1
	offset, err := draw(span)
	if err != nil {
		return 0, fmt.Errorf("billing: draw checkin award: %w", err)
	}
	if offset < 0 || offset >= span {
		return 0, fmt.Errorf("billing: checkin draw returned %d outside [0,%d)", offset, span)
	}
	return minQuota + offset, nil
}

// secureRandInt64n 返回 [0, n) 的均匀随机整数（crypto/rand，拒绝采样由
// math/big.Int 内部完成）。签到奖励不需要密码学强度，但也没有理由用可预测
// 的 PRNG 暴露发放节奏。
func secureRandInt64n(n int64) (int64, error) {
	if n <= 0 {
		return 0, errors.New("billing: random bound must be > 0")
	}
	value, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0, err
	}
	return value.Int64(), nil
}

func (s *CheckinService) boolSetting(key string, fallback bool) bool {
	if s.cfg != nil {
		if raw, ok := s.cfg.Get(key); ok {
			var value bool
			if err := json.Unmarshal(raw, &value); err == nil {
				return value
			}
			s.log.Warn("checkin: invalid boolean setting; using default",
				"key", key, "fallback", fallback)
		}
	}
	return fallback
}

func (s *CheckinService) int64Setting(key string, fallback int64) int64 {
	if s.cfg != nil {
		if raw, ok := s.cfg.Get(key); ok {
			// 用 *int64 接收：JSON null 解到 int64 会静默变成 0，
			// 把「未配置」误读成「停发」。
			var value *int64
			if err := json.Unmarshal(raw, &value); err == nil && value != nil {
				return *value
			}
			s.log.Warn("checkin: invalid integer setting; using default",
				"key", key, "fallback", fallback)
		}
	}
	return fallback
}

// registryBoolDefault / registryInt64Default 从注册表读取默认值：默认值的
// 唯一真源在 settings 注册表，本包不复制第二份。
func registryBoolDefault(key string, fallback bool) bool {
	setting, ok := settings.Lookup(key)
	if !ok {
		return fallback
	}
	if value, ok := settings.DefaultValue(setting).(bool); ok {
		return value
	}
	return fallback
}

func registryInt64Default(key string, fallback int64) int64 {
	setting, ok := settings.Lookup(key)
	if !ok {
		return fallback
	}
	if value, ok := settings.DefaultValue(setting).(int64); ok {
		return value
	}
	return fallback
}
