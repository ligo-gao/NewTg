// Package perfsettings 读取定时性能探测（`perf.schedule.*`）的生效配置。
//
// 配置面分两层：
//   - system_settings（configcenter）：运维在控制台保存的值，键可能从未落库；
//   - 环境变量（由调用方解析成 Defaults）：部署时的基线默认值。
//
// 读取规则与平台其它设置一致：键未落库、JSON null、类型不符、负值都回落
// 到 env 默认；Source 报告本次结果来自哪一层，便于控制台提示「未配置」
// 与「已配置」的差异。包内不依赖 platform/httpapi，避免装配环。
package perfsettings

import (
	"log/slog"
	"time"

	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/modelperf"
	"newtg/gateway/internal/settingread"
)

// 配置中心里的键名。与 settings 注册表的 `perf` section 一一对应；
// 本包不假设注册表已存在，直接读取原始 JSON。
const (
	settingPerfEnabled       = "perf.schedule.enabled"
	settingPerfIntervalHours = "perf.schedule.interval_hours"
	settingPerfRuns          = "perf.schedule.runs"
	settingPerfConcurrency   = "perf.schedule.concurrency"
)

// perfSettingKeys 是参与 Source 判定的四个键：只有四键的值都可用，
// Source 才是 SourceSettings。
var perfSettingKeys = [...]string{
	settingPerfEnabled,
	settingPerfIntervalHours,
	settingPerfRuns,
	settingPerfConcurrency,
}

// Source 的取值：四键的值全部可用 / 全部不可用（未落库或脏值回落）/ 部分可用。
const (
	SourceSettings = "settings"
	SourceEnv      = "env"
	SourceMixed    = "mixed"
)

// legacyTimes 是时刻表解析失败时的兜底（每日 00:00 / 12:00）。
// 装配不完整不等于调度停摆：宁可沿用历史节奏，也不要整点探测全部消失。
const legacyTimes = "00:00,12:00"

// Defaults 是环境变量层面的基线值（config.Config 的 PerfSchedule* 字段）。
type Defaults struct {
	Enabled     bool
	Times       string // 逗号分隔的 HH:MM 列表
	Timezone    string // IANA 时区名
	Runs        int
	Concurrency int
}

// Settings 是一次读取的生效值。IntervalHours>0 时按固定间隔调度，
// 否则使用 LegacyTimes（每日时刻表）。
type Settings struct {
	Enabled       bool
	IntervalHours int
	Runs          int
	Concurrency   int
	// Source ∈ SourceSettings | SourceEnv | SourceMixed：只统计「值真正可用、
	// 因而生效自 settings」的键；脏值按回落 env 计。
	Source string
	// Timezone 只来自 Defaults；LegacyTimes 同样来自 Defaults，但在间隔模式下
	// （IntervalHours > 0）会被置空——那时旧时刻表不再参与调度。
	Timezone    string
	LegacyTimes string
}

// Reader 读取并解释 `perf.schedule.*`。零值不可用，请通过 New 构造。
type Reader struct {
	reader   settingread.Reader
	defaults Defaults
	log      *slog.Logger
}

// New 构造读取器。center 为 nil（未接配置中心）时等价于所有键都未落库，
// 全部按 defaults 生效。
func New(center *configcenter.Center, defaults Defaults, log *slog.Logger) *Reader {
	// 注意：不能把 (*configcenter.Center)(nil) 直接装进接口——接口值非 nil，
	// 调用 Get 会解引用空指针。显式判断后再赋给接口，nil 才是真正的 nil。
	var reader settingread.Reader
	if center != nil {
		reader = center
	}
	return newWithReader(reader, defaults, log)
}

// newWithReader 是测试接缝：接受任意最小读取面（单测用 map），
// 避免为了读设置去构造 PG/Redis 支持的 *configcenter.Center。
func newWithReader(reader settingread.Reader, defaults Defaults, log *slog.Logger) *Reader {
	if log == nil {
		log = slog.Default()
	}
	return &Reader{reader: reader, defaults: defaults, log: log}
}

// Settings 返回当前生效值。now 预留给调用方对齐读取时刻，本实现不依赖它。
func (r *Reader) Settings(_ time.Time) Settings {
	log := r.logger()
	settings := Settings{
		Enabled:     r.defaults.Enabled,
		Runs:        r.defaults.Runs,
		Concurrency: r.defaults.Concurrency,
		Timezone:    r.defaults.Timezone,
		LegacyTimes: r.defaults.Times,
	}

	// usable 只统计「值真正来自 system_settings」的键：脏值虽然在库里，
	// 生效值其实来自 env，Source 不能声称 settings（控制台据此显示
	// 「已按页面设置运行，PERF_SCHEDULE_TIMES 不再生效」，说错就是误导）。
	usable := 0

	if enabled, ok := settingread.BoolOK(r.reader, settingPerfEnabled, r.defaults.Enabled, log); ok {
		settings.Enabled = enabled
		usable++
	}
	if interval, ok := settingread.Int(r.reader, settingPerfIntervalHours, log); ok {
		// settingread.Int 把 0 视为合法值（其它设置里 0 是「禁用」），但
		// interval_hours=0 在本包等价于「继续用每日时刻表」。若照常计入
		// usable，Source 会声称 settings，而 worker 实际跑的是 legacy times。
		// 因此 0 按不可用处理：回落 env（Settings 的 IntervalHours 保持默认 0）。
		if interval > 0 {
			settings.IntervalHours = int(interval)
			usable++
		} else {
			log.Warn("worker: perf schedule interval_hours must be positive; falling back to env times",
				"value", interval)
		}
	}
	if runs, ok := settingread.Int(r.reader, settingPerfRuns, log); ok {
		settings.Runs = int(runs)
		usable++
	}
	if concurrency, ok := settingread.Int(r.reader, settingPerfConcurrency, log); ok {
		settings.Concurrency = int(concurrency)
		usable++
	}

	switch usable {
	case 0:
		settings.Source = SourceEnv
	case len(perfSettingKeys):
		settings.Source = SourceSettings
	default:
		settings.Source = SourceMixed
	}

	// 间隔模式没有「历史时刻表」：页面保存间隔后该字段应消失，调度器也不会
	// 再读它（见 Schedule）。
	if settings.IntervalHours > 0 {
		settings.LegacyTimes = ""
	}

	return settings
}

// Schedule 返回当前生效的时刻表与对应设置。
//
// interval_hours 落库且 >0 时使用固定间隔；否则回落到每日时刻表。
// 时区加载失败回落 UTC、时刻表解析失败回落 00:00/12:00，均告警但不 panic：
// 配置脏了最多是节奏回到默认，不能让调度器起不来。
func (r *Reader) Schedule(now time.Time) (modelperf.Schedule, Settings) {
	settings := r.Settings(now)
	loc := r.location(settings.Timezone)

	if settings.IntervalHours > 0 {
		return modelperf.IntervalSchedule{Hours: settings.IntervalHours, Loc: loc}, settings
	}

	schedule, err := modelperf.ParseTimes(settings.LegacyTimes, loc)
	if err != nil {
		r.logger().Warn("worker: perf schedule times are invalid; falling back to 00:00/12:00",
			"times", settings.LegacyTimes, "error", err)
		// legacyTimes 是常量且必然可解析；忽略错误是安全的。
		schedule, _ = modelperf.ParseTimes(legacyTimes, loc)
	}
	return schedule, settings
}

// NextRun 返回严格晚于 now 的下一次探测时刻；时刻表没有下次时返回零时间。
func (r *Reader) NextRun(now time.Time) time.Time {
	schedule, _ := r.Schedule(now)
	return schedule.Next(now)
}

// logger 返回可用的 logger：零值 Reader（未经 New 构造，字段全空）也能安全
// 走到告警路径，而不是解引用 nil 指针。
func (r *Reader) logger() *slog.Logger {
	if r.log == nil {
		return slog.Default()
	}
	return r.log
}

// location 把 IANA 时区名解析成 *time.Location；空值/未知时区回落 UTC。
// nil 时区会让 ParseTimes/IntervalSchedule 内部再兜一次底，但这里显式回落
// 并告警，运维才能发现时区名拼错了。
func (r *Reader) location(name string) *time.Location {
	if name == "" {
		r.logger().Warn("worker: perf schedule timezone is empty; falling back to UTC")
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil || loc == nil {
		r.logger().Warn("worker: perf schedule timezone is invalid; falling back to UTC",
			"timezone", name, "error", err)
		return time.UTC
	}
	return loc
}
