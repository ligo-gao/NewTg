package rankings

// 排行榜窗口/周期/粒度计算（D6a；modules-09 §C1.7、platform-01 §13–§17）。
//
// 【为什么窗口在 Go 里算而不是 SQL 里算】窗口边界是「站点时区自然日/周/月/年
// 的起点」，而 usage_logs.created_at 是 timestamptz（UTC 时刻）。在 SQL 里
// 用 AT TIME ZONE 也能算，但那样每次聚合都要把时区名拼进 SQL，边界无法单测；
// 放在 Go 里可以用 time.LoadLocation + time.Date 得到确定性的 UTC 边界，
// 再由 SQL 只做 [start, end) 的区间扫描（可用 created_at 索引）。
//
// 【DST】time.Date 在目标时区构造「墙上时间」：春季跳过的 00:00（若某时区
// 当天确实没有 00:00）会被规范化到跳变后的第一个合法时刻；秋季重复的
// 00:00 由 Go 选择其一。窗口仍是「该自然日的全部真实时刻」——起止都来自
// 同一 location，差值自然等于 23h/24h/25h，不需要额外补偿。

import (
	"encoding/json"
	"log/slog"
	"time"
)

// Period 是排行榜周期（rankings_snapshot_runs.period 的四值）。
type Period string

const (
	PeriodToday Period = "today"
	PeriodWeek  Period = "week"
	PeriodMonth Period = "month"
	PeriodYear  Period = "year"
)

// allPeriods 是 worker 需要维护的全部周期，顺序固定（测试与日志稳定）。
var allPeriods = []Period{PeriodToday, PeriodWeek, PeriodMonth, PeriodYear}

// ParsePeriod 把外部字符串解析为周期；非法值返回 ok=false（handler 映射 400）。
func ParsePeriod(s string) (Period, bool) {
	p := Period(s)
	for _, candidate := range allPeriods {
		if p == candidate {
			return p, true
		}
	}
	return "", false
}

// Valid 报告 p 是否是四周期之一。
func (p Period) Valid() bool {
	_, ok := ParsePeriod(string(p))
	return ok
}

// RefreshInterval 是 worker 的刷新间隔（R-D6-02 冻结值）：
// today 5m、week/month 30m、year 6h。它也同时用作 Redis 锁的 TTL。
func (p Period) RefreshInterval() time.Duration {
	switch p {
	case PeriodToday:
		return 5 * time.Minute
	case PeriodWeek, PeriodMonth:
		return 30 * time.Minute
	case PeriodYear:
		return 6 * time.Hour
	}
	return 0
}

// Granularity 是趋势桶的粒度（rankings_timeseries.granularity）。
type Granularity string

const (
	GranularityHour Granularity = "hour"
	GranularityDay  Granularity = "day"
	GranularityWeek Granularity = "week"
)

// Granularity 返回周期对应的趋势粒度（R-D6-04）：
// today→hour、week/month→day、year→week。
//
// 【为什么 year 用 week】year 的天桶跨度可达 366 天，超过 F-09-32 的
// 「>90 天降采样」阈值；周桶把序列压到 ~53 点，图表可读且存储有界。
func (p Period) Granularity() Granularity {
	switch p {
	case PeriodToday:
		return GranularityHour
	case PeriodWeek, PeriodMonth:
		return GranularityDay
	case PeriodYear:
		return GranularityWeek
	}
	return GranularityDay
}

// Window 是左闭右开的 UTC 时间窗 [Start, End)。
type Window struct {
	Start time.Time
	End   time.Time
}

// Duration 返回窗口长度。
func (w Window) Duration() time.Duration { return w.End.Sub(w.Start) }

// Valid 报告窗口是否非空。
func (w Window) Valid() bool { return !w.Start.IsZero() && w.End.After(w.Start) }

// CurrentWindow 计算周期 p 在当前时刻 now 的窗口：
// 站点时区自然周期起点 → now，边界统一转 UTC 落库。
func CurrentWindow(p Period, now time.Time, loc *time.Location) Window {
	if loc == nil {
		loc = time.UTC
	}
	start := periodStart(p, now.In(loc), loc)
	return Window{Start: start.UTC(), End: now.UTC()}
}

// PreviousWindow 返回与当前窗口**等长且紧邻**的上一窗口 [start-len, start)。
//
// 【为什么是等长而不是「上一个自然周期」】today/week/month/year 的当前窗口
// 都是「周期起点 → now」的**未完成**窗口（例如今天 10:00 只有 10 小时数据）。
// 与上一自然周期的**全量**窗口比环比会系统性低估当日增长；取紧邻的等长
// 片段才能让 delta_pct 表达「同长度时间段的相对变化」。这也是
// rankings_model_stats.prev_rank 字段注释「相邻等长上一窗口」的口径。
func PreviousWindow(p Period, now time.Time, loc *time.Location) Window {
	cur := CurrentWindow(p, now, loc)
	length := cur.Duration()
	if length <= 0 {
		return Window{Start: cur.Start, End: cur.Start}
	}
	return Window{Start: cur.Start.Add(-length), End: cur.Start}
}

// periodStart 计算站点时区下周期起点（墙上时间）。
func periodStart(p Period, t time.Time, loc *time.Location) time.Time {
	y, m, d := t.Date()
	switch p {
	case PeriodToday:
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	case PeriodWeek:
		// ISO 周：周一为起点。Go 的 Sunday=0，故 (Weekday+6)%7 是「距周一的天数」。
		back := (int(t.Weekday()) + 6) % 7
		return time.Date(y, m, d-back, 0, 0, 0, 0, loc)
	case PeriodMonth:
		return time.Date(y, m, 1, 0, 0, 0, 0, loc)
	case PeriodYear:
		return time.Date(y, time.January, 1, 0, 0, 0, 0, loc)
	}
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// ConfigReader 是站点设置的只读面（生产为 *configcenter.Center 的快照）。
// 与 settings.ConfigReader/httpapi.ConfigReader 同方法集，刻意在本包重复声明，
// 避免 rankings 反向依赖 HTTP 层。
type ConfigReader interface {
	Get(key string) (json.RawMessage, bool)
}

// 站点设置键（modules-08 §1.4.1）。
const (
	configKeyTimezone   = "site.timezone"
	configKeyMinSamples = "site.rankings_min_samples"
	configKeyKeepRuns   = "site.rankings_keep_runs"
	configKeyEnabled    = "nav.rankings.enabled"
)

// defaultMinSamples 是阈值缺省值（R-D6-02；与 settings 注册表默认值一致）。
const defaultMinSamples = 100

// defaultKeepRuns 是每 period 保留 run 数的缺省值（R-D6-07 /
// site.rankings_keep_runs 注册表默认值）。设置缺失时 worker 回退到此值。
const defaultKeepRuns = 10

// keepRunsFloor / keepRunsCeil 与 settings 注册表一致（1–100）。
// 下限 1 保证 worker 永远留得下最新 run；上限防止把「保留」配成近乎不清理。
const (
	keepRunsFloor = 1
	keepRunsCeil  = 100
)

// minSamplesFloor / minSamplesCeil 是隐私地板与上界（R-D6-02）：
// 下限 50 不允许配置为 0，避免单用户/少量请求反推行为。
const (
	minSamplesFloor int64 = 50
	minSamplesCeil  int64 = 100000
)

// ResolveLocation 解析 site.timezone。
//
// R-D6-11：键缺失 → UTC；非法 IANA 名 → 回退 UTC 并 Warn。设置写入侧已由
// settings 注册表校验 IANA，这里的回退只兜「历史脏值 / 手工改库 / 旧快照」，
// 不能让一个坏时区名让整个排行榜 worker 每轮都失败。
func ResolveLocation(reader ConfigReader, log *slog.Logger) *time.Location {
	if log == nil {
		log = slog.Default()
	}
	raw, ok := reader.Get(configKeyTimezone)
	if !ok {
		return time.UTC
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil || name == "" {
		log.Warn("rankings: site.timezone 不是合法字符串，回退 UTC", "raw", string(raw))
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		log.Warn("rankings: site.timezone 不是合法 IANA 时区，回退 UTC", "timezone", name, "error", err)
		return time.UTC
	}
	return loc
}

// ResolveMinSamples 读取 site.rankings_min_samples 并钳制到 [50, 100000]。
//
// 缺省/非法/越界都返回 100（而不是让 worker 报错）：阈值是展示口径，
// 保守默认比让排行榜整周期停摆更重要；越界值钳制到边界方向（<50 抬到 50，
// >100000 压到 100000），与 R-D6-02 的「阈值抬高只见效于更保守方向」一致。
func ResolveMinSamples(reader ConfigReader, log *slog.Logger) int64 {
	if log == nil {
		log = slog.Default()
	}
	raw, ok := reader.Get(configKeyMinSamples)
	if !ok {
		return defaultMinSamples
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		log.Warn("rankings: site.rankings_min_samples 不是整数，使用默认阈值", "raw", string(raw))
		return defaultMinSamples
	}
	if v < minSamplesFloor {
		log.Warn("rankings: site.rankings_min_samples 低于隐私地板，钳制到 50", "configured", v)
		return minSamplesFloor
	}
	if v > minSamplesCeil {
		log.Warn("rankings: site.rankings_min_samples 超过上界，钳制到 100000", "configured", v)
		return minSamplesCeil
	}
	return v
}

// ResolveKeepRuns 读取 site.rankings_keep_runs 并钳制到 [1, 100]。
//
// 缺省/非法/越界都回退或钳制而不是让 worker 失败：清理是后台维护，
// 为一条展示性设置让快照写入整轮失败不值得。设置写入侧已由注册表校验，
// 这里兜历史脏值/手工改库。
func ResolveKeepRuns(reader ConfigReader, log *slog.Logger) int {
	if log == nil {
		log = slog.Default()
	}
	raw, ok := reader.Get(configKeyKeepRuns)
	if !ok {
		return defaultKeepRuns
	}
	// 用指针解析：设置快照里显式 null（键存在但值为空）应等价于「未设置」
	// 而回退默认值。直接 Unmarshal 到 int 时 null 会静默留下 0，随后被钳制
	// 到下限 1——保留数从 10 悄悄降到 1，历史 run 被多删。
	var v *int
	if err := json.Unmarshal(raw, &v); err != nil || v == nil {
		log.Warn("rankings: site.rankings_keep_runs 不是整数，使用默认保留数", "raw", string(raw))
		return defaultKeepRuns
	}
	if *v < keepRunsFloor {
		log.Warn("rankings: site.rankings_keep_runs 低于下限，钳制到 1", "configured", *v)
		return keepRunsFloor
	}
	if *v > keepRunsCeil {
		log.Warn("rankings: site.rankings_keep_runs 超过上限，钳制到 100", "configured", *v)
		return keepRunsCeil
	}
	return *v
}

// RankingsEnabled 读取 nav.rankings.enabled。
//
// 键缺失视为开启（与 settings 注册表默认值 true 一致）；显式 false 时 worker
// 跳过聚合——页面被导航开关关闭时没有必要继续消耗日志库扫描。R-D6-08 的
// 「冷启动重开」由此自然成立：开关重新打开后的下一轮 tick 就会生成新快照。
func RankingsEnabled(reader ConfigReader) bool {
	raw, ok := reader.Get(configKeyEnabled)
	if !ok {
		return true
	}
	var enabled bool
	if err := json.Unmarshal(raw, &enabled); err != nil {
		return true
	}
	return enabled
}
