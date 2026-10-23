// store.go 是 holiday_calendar 的持久化面（spec §5.2.1）。
//
// 【整年替换】数据源每年一个 JSON，同步语义是「这一年的全部事实」。ReplaceYear
// 在单个事务里先删该年、再插入新行：中途失败回滚后旧数据原样保留，不会出现
// 「删了旧的、没写新的」的空窗。只碰 (country, year) 一个切片，多国日历
// （第一阶段只有 CN）与其它年份互不影响。
//
// 【查询失败不等于没有数据】LookupHoliday 的 (Holiday, false, nil) 表示
// 「该日期没有记录」，调用方按星期规则继续；error 表示数据库不可达。
// 定价侧对两者都不终止请求，只是把 error 标记成 HolidayLookupFailed。
package holiday

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"newtg/gateway/internal/pricingstrategy"
)

// LookupCacheTTL 是节假日行的进程内缓存时长（spec §5.2.1「TTL 30s」）。
//
// 定价解析在预扣与结算时都会调用 MultiplierAt；没有缓存时每个请求都会多
// 一次 holiday_calendar 查询，而绝大多数日期根本没有行（负缓存同样重要）。
// 缓存只在本进程内，同步成功后的其它副本最多晚 TTL 看到新数据——这正是
// spec 选择 30s 的原因（节假日数据一天只变一次）。
const LookupCacheTTL = 30 * time.Second

// Store 是 holiday_calendar 仓储。
type Store struct {
	DB *sql.DB

	mu    sync.Mutex
	cache map[string]lookupEntry
	// now 是测试注入的时钟；nil 用 time.Now。
	now func() time.Time
}

type lookupEntry struct {
	holiday   pricingstrategy.Holiday
	found     bool
	expiresAt time.Time
}

// NewStore 构造仓储。nil DB 在调用时返回错误（而不是 panic）：装配层可以
// 用同一个构造函数，把「未配置数据库」表达成一次可记录的任务失败。
func NewStore(db *sql.DB) *Store {
	return &Store{DB: db}
}

// ErrInvalidDays 表示要写入的年度数据本身不合法（空集合、日期越界/重复）。
// 这类错误绝不会触达数据库，因此不会清空已有的好数据。
var ErrInvalidDays = errors.New("holiday: 年度数据不合法")

// ReplaceYear 用 days 整体替换 country 在 year 年的日历。
//
// 写入前先在内存里完整校验所有日期（YYYY-MM-DD、属于 year、不重复），
// 因此一次非法输入不会产生「已删除旧行」的副作用。days 为空时返回错误，
// 避免把数据源异常当成「今年没有假日」而清空日历。
func (s *Store) ReplaceYear(ctx context.Context, country string, year int, days []Day, source string) error {
	if s == nil || s.DB == nil {
		return errors.New("holiday: Store 未绑定数据库")
	}
	yearStart, yearEnd, err := yearRange(year)
	if err != nil {
		return err
	}
	if err := validateCountry(country); err != nil {
		return err
	}
	if source == "" {
		source = SourceName
	}
	dates, err := normalizeForReplace(year, days)
	if err != nil {
		return err
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("holiday: 开启替换事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 边界用 YYYY-MM-DD 字符串 + ::date，避免把 time.Time 绑成 timestamptz
	// 后按会话时区把 2030-01-01 折成 2029-12-31。
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM holiday_calendar WHERE country = $1 AND date >= $2::date AND date < $3::date`,
		country, yearStart, yearEnd); err != nil {
		return fmt.Errorf("holiday: 清理 %s %d 年旧数据失败: %w", country, year, err)
	}
	for i, day := range days {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO holiday_calendar (country, date, is_off_day, name, source, synced_at)
			 VALUES ($1, $2, $3, $4, $5, now())`,
			country, dates[i], day.OffDay, day.Name, source); err != nil {
			return fmt.Errorf("holiday: 写入 %s %d 年数据失败: %w", country, year, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("holiday: 提交 %s %d 年数据失败: %w", country, year, err)
	}
	// 同步成功后清空缓存（spec §5.2.1）：下一个请求立刻读到新日历，
	// 不必等 TTL 过期。
	s.Invalidate()
	return nil
}

// LookupHoliday 实现 pricingstrategy.HolidayLookup。
//
// date 由调用方按策略自己的时区本地化（pricingstrategy.MultiplierAt 传进来的
// 值已经是 Asia/Shanghai 等策略时区），这里按 **date 自己的 Location** 取
// YYYY-MM-DD。刻意不用 date.In(time.Local)：宿主进程 TZ=UTC 时，
// 2026-10-01 02:00 +08:00 会被折成 09-30，节假日判断整体错一天。
func (s *Store) LookupHoliday(ctx context.Context, country string, date time.Time) (pricingstrategy.Holiday, bool, error) {
	if s == nil || s.DB == nil {
		return pricingstrategy.Holiday{}, false, errors.New("holiday: Store 未绑定数据库")
	}
	if err := validateCountry(country); err != nil {
		return pricingstrategy.Holiday{}, false, err
	}
	key := country + "\x00" + date.Format("2006-01-02")
	if holiday, found, ok := s.cacheGet(key); ok {
		return holiday, found, nil
	}
	day := date.Format("2006-01-02")
	var (
		name   string
		offDay bool
	)
	err := s.DB.QueryRowContext(ctx,
		`SELECT name, is_off_day FROM holiday_calendar WHERE country = $1 AND date = $2::date`,
		country, day).Scan(&name, &offDay)
	if errors.Is(err, sql.ErrNoRows) {
		s.cachePut(key, pricingstrategy.Holiday{}, false)
		return pricingstrategy.Holiday{}, false, nil
	}
	if err != nil {
		return pricingstrategy.Holiday{}, false, fmt.Errorf("holiday: 查询 %s %s 失败: %w", country, day, err)
	}
	holiday := pricingstrategy.Holiday{Name: name, OffDay: offDay}
	s.cachePut(key, holiday, true)
	return holiday, true, nil
}

// Invalidate 丢弃全部缓存条目（同步写库后调用）。
func (s *Store) Invalidate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cache = nil
	s.mu.Unlock()
}

// cacheGet 返回命中值；第三个返回值为 false 表示未命中或已过期。
func (s *Store) cacheGet(key string) (pricingstrategy.Holiday, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.cache[key]
	if !ok || !s.clock().Before(entry.expiresAt) {
		return pricingstrategy.Holiday{}, false, false
	}
	return entry.holiday, entry.found, true
}

func (s *Store) cachePut(key string, holiday pricingstrategy.Holiday, found bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		s.cache = make(map[string]lookupEntry)
	}
	s.cache[key] = lookupEntry{
		holiday:   holiday,
		found:     found,
		expiresAt: s.clock().Add(LookupCacheTTL),
	}
}

func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// yearRange 返回 [year-01-01, next-01-01) 两个日历日期字符串。
//
// 用字符串而不是 time.Time：date 列不带时区，绑定 timestamptz 后 PostgreSQL
// 会按会话时区折算边界日，跨年时可能少删/多删一天。
func yearRange(year int) (string, string, error) {
	if year < 1900 || year > 9999 {
		return "", "", fmt.Errorf("%w: 年份 %d 超出范围", ErrInvalidDays, year)
	}
	return fmt.Sprintf("%04d-01-01", year), fmt.Sprintf("%04d-01-01", year+1), nil
}

// normalizeForReplace 校验待写入日期并返回对应的 date 参数（YYYY-MM-DD 字符串）。
func normalizeForReplace(year int, days []Day) ([]string, error) {
	if len(days) == 0 {
		return nil, fmt.Errorf("%w: 空集合（拒绝清空该年日历）", ErrInvalidDays)
	}
	dates := make([]string, len(days))
	seen := make(map[string]struct{}, len(days))
	for i, day := range days {
		t, err := time.Parse("2006-01-02", day.Date)
		if err != nil {
			return nil, fmt.Errorf("%w: days[%d].date=%q 不是 YYYY-MM-DD", ErrInvalidDays, i, day.Date)
		}
		if t.Year() != year {
			return nil, fmt.Errorf("%w: days[%d].date=%q 不属于 %d 年", ErrInvalidDays, i, day.Date, year)
		}
		if _, dup := seen[day.Date]; dup {
			return nil, fmt.Errorf("%w: days[%d].date=%q 重复", ErrInvalidDays, i, day.Date)
		}
		seen[day.Date] = struct{}{}
		dates[i] = day.Date
	}
	return dates, nil
}

// validateCountry 校验 ISO 3166-1 alpha-2 国家代码的形状（列是 char(2)）。
func validateCountry(country string) error {
	if len(country) != 2 {
		return fmt.Errorf("%w: country=%q 必须是两位国家代码", ErrInvalidDays, country)
	}
	return nil
}
