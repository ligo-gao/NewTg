// 性能页「运行参数」视图：GET /api/performance/config（spec §7.6）。
//
//	GET /api/performance/config   settings:manage（只读）
//
// 返回四块内容，全部是「实际生效值」而不是前端硬编码：
//   - schedule：测速四项的生效值。解释逻辑与 worker 的调度器共用同一构造
//     函数 newPerfSettingsReader 和同一份配置中心快照（worker 在另一进程另有
//     实例，但两边读的是同一张设置表）。next_run_at 由它在服务端算出——浏览器
//     时区与 PERF_SCHEDULE_TIMEZONE 可能不同，客户端重算会给出一个与真实运行
//     不一致的时间（spec §7.6 / Review Focus 4）。
//   - env：部署环境变量快照（B 类）。秘钥类只给「已设置」布尔，DSN 整行不
//     出现；口令值绝不回显。
//   - fixed：代码内置、改部署配置也不生效的参数（C 类）。
//   - settings_groups：A 类分组（渠道健康 / 限流 / 保留清理）的当前值与设置页
//     导航锚点。设置页只有 site/auth/billing/payment/security/notify/policy
//     七个 tab，audit/redemption/task/mj/log 没有入口，因此这些组只给值、
//     不给 link。
//
// 响应里没有任何中文标题或展示文案：组只给稳定 id/tab/link，标签由前端
// i18n 负责（服务端不能替 7 个 locale 选语言）。env[] 的 key 是环境变量名
// （本身稳定），fixed[] 的 key 是稳定英文 slug。
package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/settings"
)

const (
	// effect 取值冻结为 restart / code（spec §7.6）：前端直接映射为
	// 「需重启」/「需改代码」徽标，不得自创同义词。
	perfConfigEffectRestart = "restart"
	perfConfigEffectCode    = "code"

	// settings_groups 的 source：值来自设置中心 / 注册表默认。
	perfConfigSourceSettings = "settings"
	perfConfigSourceDefault  = "default"

	// perfConfigSettingsLinkPrefix 是设置页 tab 的跳转前缀；组没有 tab 时
	// link 为空串（前端渲染为纯文本而不是死链）。
	perfConfigSettingsLinkPrefix = "/admin/settings?tab="
)

// perfConfigResponse 是 GET /api/performance/config 的响应体。
//
// Schedule 为 nil 时 JSON 输出 null：未装配 perfsettings.Reader 的部署/单测
// 仍然要 200，页面按「节奏未知」降级，而不是整块报错。
type perfConfigResponse struct {
	GeneratedAt    time.Time                 `json:"generated_at"`
	Schedule       *perfConfigSchedule       `json:"schedule"`
	Env            []perfConfigEnvRow        `json:"env"`
	Fixed          []perfConfigFixedRow      `json:"fixed"`
	SettingsGroups []perfConfigSettingsGroup `json:"settings_groups"`
}

// perfConfigSchedule 是测速四项的生效值。Source / LegacyTimes 原样透传
// perfsettings.Settings——handler 不做二次解释，否则页面与 worker 会对
// 「当前按什么跑」给出两个答案。
type perfConfigSchedule struct {
	Enabled       bool       `json:"enabled"`
	IntervalHours int        `json:"interval_hours"`
	Runs          int        `json:"runs"`
	Concurrency   int        `json:"concurrency"`
	Source        string     `json:"source"`
	Timezone      string     `json:"timezone"`
	NextRunAt     *time.Time `json:"next_run_at"`
	LegacyTimes   string     `json:"legacy_times"`
}

// perfConfigEnvRow 是一行部署环境参数。
//
// Value 用指针：秘钥类行（METRICS_TOKEN）只给 configured，JSON 里不出现
// value 键；普通行的 value 恒存在（未配置时是空串，前端显示「未配置」）。
//
// configured 与 set 是两个不同的问题：
//   - configured：渲染出来的生效值是否非空（有默认值的变量恒为 true）；
//   - set：部署者是否真的设置了该环境变量（原文非空），用于区分
//     「按默认值跑」与「显式配置」。
type perfConfigEnvRow struct {
	Key        string  `json:"key"`
	Value      *string `json:"value,omitempty"`
	Configured bool    `json:"configured"`
	Set        bool    `json:"set"`
	Effect     string  `json:"effect"`
}

// perfConfigFixedRow 是一行代码内置参数；value 已带单位（"30s"、"10/min"），
// unit 只用于前端选格式化方式。
type perfConfigFixedRow struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Unit   string `json:"unit"`
	Effect string `json:"effect"`
}

// perfConfigSettingsGroup 是一个 A 类分组。不含标题：id/tab/link 是稳定
// 契约，中文/多语言标题由前端按 id 映射。
type perfConfigSettingsGroup struct {
	ID    string                  `json:"id"`
	Tab   string                  `json:"tab"`
	Link  string                  `json:"link"`
	Items []perfConfigSettingItem `json:"items"`
}

// perfConfigSettingItem 是 A 类键的当前值。Value 保持注册表/设置中心的
// 原生 JSON 类型（数字是数字、布尔是布尔），前端据此选控件。
type perfConfigSettingItem struct {
	Key    string `json:"key"`
	Value  any    `json:"value"`
	Type   string `json:"type"`
	Unit   string `json:"unit"`
	Source string `json:"source"`
}

// perfConfigSettingSpec 是分组表的一行：Key 必须在 settings 注册表里，
// Unit 是 brief 冻结的单位词。
type perfConfigSettingSpec struct {
	Key  string
	Unit string
}

// perfConfigGroupSpec 是 A 类分组表的一行。Tab 为空表示设置页没有对应
// 入口（保留与清理族），此时 Link 也为空。
type perfConfigGroupSpec struct {
	ID    string
	Tab   string
	Items []perfConfigSettingSpec
}

// perfConfigGroupSpecs 是 A 类的唯一清单（brief Step 2 冻结）。已核对 14
// 个键全部在注册表里；新增/改名必须同步 docs/system-config-mapping.md。
var perfConfigGroupSpecs = []perfConfigGroupSpec{
	{
		ID:  "channel_health",
		Tab: "policy",
		Items: []perfConfigSettingSpec{
			{Key: "rl.health.test_interval_minutes", Unit: "minutes"},
			{Key: "rl.health.test_concurrency", Unit: "count"},
			{Key: "rl.health.response_time_threshold_seconds", Unit: "seconds"},
			{Key: "rl.health.auto_disable_enabled", Unit: "onoff"},
			{Key: "rl.health.auto_enable_enabled", Unit: "onoff"},
		},
	},
	{
		ID:  "rate_limit",
		Tab: "security",
		Items: []perfConfigSettingSpec{
			{Key: "rl.user_global_rpm", Unit: "rpm"},
			{Key: "rl.token_rpm", Unit: "rpm"},
			{Key: "rl.user_model_rpm", Unit: "rpm"},
		},
	},
	{
		// 无 tab：audit/redemption/task/mj/log 在设置页没有渲染入口。
		ID: "retention",
		Items: []perfConfigSettingSpec{
			{Key: "audit.retention_months", Unit: "months"},
			{Key: "redemption.retention_days", Unit: "days"},
			{Key: "task.retention_terminal_days", Unit: "days"},
			{Key: "mj.retention_terminal_days", Unit: "days"},
			{Key: "log.partition_lookahead_months", Unit: "months"},
			{Key: "log.partition_drop_enabled", Unit: "onoff"},
		},
	},
}

// handleConfig 实现 GET /api/performance/config。
func (h *PerformanceHandlers) handleConfig(w http.ResponseWriter, r *http.Request) {
	now := h.now()
	// 环境快照只读一次：env[] 与 fixed[] 的 probe_timeout 必须来自同一份
	// 观测，避免两次 os.Getenv 之间被外部改动造成同一响应内的自相矛盾。
	snapshot := h.perfEnvSnapshot()
	resp := perfConfigResponse{
		GeneratedAt:    now,
		Env:            h.perfConfigEnvRows(snapshot),
		Fixed:          perfConfigFixedRows(snapshot),
		SettingsGroups: h.perfConfigSettingsGroups(),
	}
	if h.schedule != nil {
		resp.Schedule = h.perfConfigSchedule(now)
	}
	writeJSON(w, http.StatusOK, resp)
}

// perfEnvSnapshot 返回本次请求使用的环境快照：装配方注入的静态快照优先
// （测试/自定义部署），否则按当前环境读一次。
func (h *PerformanceHandlers) perfEnvSnapshot() PerfEnvSnapshot {
	if h.env != nil {
		return *h.env
	}
	return h.envSnapshot()
}

// perfConfigSchedule 把 perfsettings.Settings 原样映射成响应，
// next_run_at 只在启用时计算（停用没有「下次运行」）。
func (h *PerformanceHandlers) perfConfigSchedule(now time.Time) *perfConfigSchedule {
	// Schedule 一次调用同时返回时刻表与生效设置：Settings 只读一遍配置中心
	// 快照，响应内的 enabled/runs 与 next_run_at 因此出自同一次快照，不会
	// 因为两次读取之间的广播刷新而互相矛盾。
	upcoming, effective := h.schedule.Schedule(now)
	schedule := &perfConfigSchedule{
		Enabled:       effective.Enabled,
		IntervalHours: effective.IntervalHours,
		Runs:          effective.Runs,
		Concurrency:   effective.Concurrency,
		Source:        effective.Source,
		Timezone:      effective.Timezone,
		LegacyTimes:   effective.LegacyTimes,
	}
	if !effective.Enabled {
		return schedule
	}
	// Next 可能返回零时间（时刻表为空/间隔非法）：此时给 null，而不是
	// 0001-01-01 这种会让页面显示荒谬日期的时间戳。
	if next := upcoming.Next(now); !next.IsZero() {
		schedule.NextRunAt = &next
	}
	return schedule
}

// perfConfigEnvRows 把一次环境快照摊平成行。除 PerfMetricsDSNSet 外每个
// 字段恰好一行：DSN 里可能带口令，整行不渲染（采集管道是否接入由
// PERF_METRICS_ENABLED 表达）；MetricsTokenSet 只给「已设置」布尔。
//
// ProbeTimeout 是 C 类代码常量（不是环境变量），只在 fixed[] 出现，
// 因此不在这里占用一行。
func (h *PerformanceHandlers) perfConfigEnvRows(snapshot PerfEnvSnapshot) []perfConfigEnvRow {
	rows := make([]perfConfigEnvRow, 0, 16)
	addString := func(key, value string) {
		rows = append(rows, perfConfigEnvRow{
			Key:        key,
			Value:      &value,
			Configured: value != "",
			Set:        h.geten(key) != "",
			Effect:     perfConfigEffectRestart,
		})
	}
	addInt := func(key string, value int) { addString(key, strconv.Itoa(value)) }
	addBool := func(key string, value bool) { addString(key, strconv.FormatBool(value)) }
	addDuration := func(key string, value time.Duration) { addString(key, value.String()) }

	addInt("DB_MAX_OPEN_CONNS", snapshot.DBMaxOpenConns)
	addInt("REDIS_POOL_SIZE", snapshot.RedisPoolSize)
	addInt("SHUTDOWN_GRACE_SECONDS", snapshot.ShutdownGraceSeconds)
	addString("METRICS_ADDR", snapshot.MetricsAddr)
	// 秘钥行：Value 保持 nil，JSON 里没有 value 键。
	rows = append(rows, perfConfigEnvRow{
		Key:        "METRICS_TOKEN",
		Configured: snapshot.MetricsTokenSet,
		Set:        h.geten("METRICS_TOKEN") != "",
		Effect:     perfConfigEffectRestart,
	})
	addBool("PERF_METRICS_ENABLED", snapshot.PerfMetricsEnabled)
	addDuration("PERF_METRICS_FLUSH_INTERVAL", snapshot.PerfMetricsFlushInterval)
	addInt("PERF_METRICS_RETENTION_DAYS", snapshot.PerfMetricsRetentionDays)
	addString("PROMETHEUS_URL", snapshot.PrometheusURL)
	addString("PROMETHEUS_TIMEOUT", snapshot.PrometheusTimeout)
	addString("PERF_SCHEDULE_TIMEZONE", snapshot.PerfScheduleTimezone)
	addBool("DISK_CACHE_ENABLED", snapshot.DiskCacheEnabled)
	addString("DISK_CACHE_PATH", snapshot.DiskCachePath)
	addString("LOG_DIR", snapshot.LogDir)
	addInt("DISK_CACHE_MAX_MB", snapshot.DiskCacheMaxMB)
	addInt("DISK_CACHE_THRESHOLD_MB", snapshot.DiskCacheThresholdMB)
	return rows
}

// perfConfigFixedRows 是四条代码内置参数（brief Step 2 冻结）。value 尽量
// 从真正的常量/快照派生，避免页面展示值与实际行为漂移。
func perfConfigFixedRows(snapshot PerfEnvSnapshot) []perfConfigFixedRow {
	return []perfConfigFixedRow{
		// probe_timeout 取快照字段（Task 8 已在快照里预留了它），而不是直接
		// 读包级常量：注入的自定义快照（测试/未来可配置化）才能真正生效。
		{Key: "probe_timeout", Value: snapshot.ProbeTimeout.String(), Unit: "duration", Effect: perfConfigEffectCode},
		{Key: "write_rate_limit", Value: strconv.Itoa(performanceWriteRateLimit) + "/min", Unit: "rate", Effect: perfConfigEffectCode},
		{Key: "disk_cache_idle", Value: strconv.Itoa(int(diskCacheInactiveWindow/time.Minute)) + "m", Unit: "duration", Effect: perfConfigEffectCode},
		// 用量日志分区保留期（billing.DefaultPartitionRetentionDays，
		// G7-R47 冻结为 90 天）。
		{Key: "usage_log_retention", Value: strconv.Itoa(billing.DefaultPartitionRetentionDays) + "d", Unit: "duration", Effect: perfConfigEffectCode},
	}
}

// perfConfigSettingsGroups 读出全部 A 类键的当前值。
func (h *PerformanceHandlers) perfConfigSettingsGroups() []perfConfigSettingsGroup {
	groups := make([]perfConfigSettingsGroup, 0, len(perfConfigGroupSpecs))
	for _, spec := range perfConfigGroupSpecs {
		group := perfConfigSettingsGroup{
			ID:    spec.ID,
			Tab:   spec.Tab,
			Link:  "",
			Items: make([]perfConfigSettingItem, 0, len(spec.Items)),
		}
		if spec.Tab != "" {
			group.Link = perfConfigSettingsLinkPrefix + spec.Tab
		}
		for _, item := range spec.Items {
			setting, ok := settings.Lookup(item.Key)
			if !ok {
				// 注册表是编译期常量；查不到只可能是本文件与注册表脱节。
				// 跳过该键并告警，不让端点 500（页面少一行比整块不可用强）。
				h.log.Warn("performance: config setting key is not registered; skipping",
					"key", item.Key)
				continue
			}
			value, source := h.perfConfigSettingValue(setting)
			group.Items = append(group.Items, perfConfigSettingItem{
				Key:    setting.Key,
				Value:  value,
				Type:   string(setting.Type),
				Unit:   item.Unit,
				Source: source,
			})
		}
		groups = append(groups, group)
	}
	return groups
}

// perfConfigSettingValue 返回一个 A 类键的当前值与来源。
//
// 有效设置值优先；未落库、JSON null、类型不符、越界或键级校验失败一律
// 回落注册表默认。脏值必须告警（否则页面会安静地展示一个与设置页不同的
// 值），但绝不让端点失败。
func (h *PerformanceHandlers) perfConfigSettingValue(setting settings.Setting) (any, string) {
	if h.settingGet != nil {
		if raw, ok := h.settingGet(setting.Key); ok {
			value, err := decodePerfConfigSettingValue(setting.Type, raw)
			switch {
			case err == nil:
				rangeErr := validatePerfConfigSettingValue(setting, value)
				if rangeErr == nil {
					return value, perfConfigSourceSettings
				}
				h.warnPerfConfigSetting(setting, raw,
					"performance: setting value is out of range; using registry default", rangeErr)
			case errors.Is(err, errPerfConfigNullValue):
				// null 是「显式清空」，仍然值得告警：写路径不会产生 null，
				// 出现它说明有别的写入者/历史脏行（与 settingread 的口径一致）。
				h.warnPerfConfigSetting(setting, raw,
					"performance: setting is null; using registry default", nil)
			default:
				h.warnPerfConfigSetting(setting, raw,
					"performance: setting value is invalid; using registry default", err)
			}
		}
	}
	return settings.DefaultValue(setting), perfConfigSourceDefault
}

// warnPerfConfigSetting 记录一条 A 类脏值告警。密钥类键（当前清单里没有，
// 但注册表支持）不回显原始值，避免将来把令牌类键加进分组表后明文入日志。
func (h *PerformanceHandlers) warnPerfConfigSetting(setting settings.Setting, raw []byte, msg string, err error) {
	attrs := []any{"key", setting.Key}
	if setting.Secret {
		attrs = append(attrs, "raw", "<redacted>")
	} else {
		attrs = append(attrs, "raw", string(raw))
	}
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	h.log.Warn(msg, attrs...)
}

// validatePerfConfigSettingValue 复用注册表声明的范围与键级校验。
//
// 只做类型解析是不够的：消费方（channelhealth.LoadWorkerSettings、
// platform/retention.go 等）对越界值会 clamp/回落到默认，页面若把它标成
// source:"settings" 就违背了「看到什么就是跑什么」。number 类型额外排除
// NaN/±Inf——JSON 本身不产生它们，但显式校验让将来换用其它解码路径时不会
// 静默把不可编码的值写进响应。
func validatePerfConfigSettingValue(setting settings.Setting, value any) error {
	switch typed := value.(type) {
	case int64:
		if setting.MinInt != nil && typed < *setting.MinInt {
			return fmt.Errorf("value %d is below min %d", typed, *setting.MinInt)
		}
		if setting.MaxInt != nil && typed > *setting.MaxInt {
			return fmt.Errorf("value %d is above max %d", typed, *setting.MaxInt)
		}
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return fmt.Errorf("value %v is not finite", typed)
		}
		if setting.MinFloat != nil && typed < *setting.MinFloat {
			return fmt.Errorf("value %v is below min %v", typed, *setting.MinFloat)
		}
		if setting.MaxFloat != nil && typed > *setting.MaxFloat {
			return fmt.Errorf("value %v is above max %v", typed, *setting.MaxFloat)
		}
	}
	if setting.Validate != nil {
		if err := setting.Validate(value); err != nil {
			return err
		}
	}
	return nil
}

// errPerfConfigUnsupportedType 表示注册表类型不在本函数的解析范围内。
var (
	errPerfConfigUnsupportedType = errors.New("unsupported registry type")
	errPerfConfigNullValue       = errors.New("value is null")
)

// decodePerfConfigSettingValue 按注册表声明的类型解析设置中心的 JSON 值。
// 解到指针以区分「JSON null」与「零值」：null 落回默认，而不是把 false/0
// 当成有效设置（与 settingread 的口径一致）。
func decodePerfConfigSettingValue(typ settings.Type, raw []byte) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, errPerfConfigNullValue
	}
	switch typ {
	case settings.TypeBool:
		var value *bool
		if err := json.Unmarshal(trimmed, &value); err != nil || value == nil {
			return nil, fmt.Errorf("expected bool: %v", errOrNil(err))
		}
		return *value, nil
	case settings.TypeInt:
		var value *int64
		if err := json.Unmarshal(trimmed, &value); err != nil || value == nil {
			return nil, fmt.Errorf("expected integer: %v", errOrNil(err))
		}
		return *value, nil
	case settings.TypeNumber:
		var value *float64
		if err := json.Unmarshal(trimmed, &value); err != nil || value == nil {
			return nil, fmt.Errorf("expected number: %v", errOrNil(err))
		}
		return *value, nil
	case settings.TypeString, settings.TypeEnum:
		var value *string
		if err := json.Unmarshal(trimmed, &value); err != nil || value == nil {
			return nil, fmt.Errorf("expected string: %v", errOrNil(err))
		}
		return *value, nil
	case settings.TypeList:
		var value []any
		if err := json.Unmarshal(trimmed, &value); err != nil || value == nil {
			return nil, fmt.Errorf("expected array: %v", errOrNil(err))
		}
		return value, nil
	case settings.TypeObject:
		var value map[string]any
		if err := json.Unmarshal(trimmed, &value); err != nil || value == nil {
			return nil, fmt.Errorf("expected object: %v", errOrNil(err))
		}
		return value, nil
	default:
		return nil, fmt.Errorf("%w %q", errPerfConfigUnsupportedType, typ)
	}
}

// errOrNil 把 nil error 变成可读错误：指针解出来是 nil（JSON null）时
// json.Unmarshal 不报错，需要一个可展示的原因。
func errOrNil(err error) error {
	if err != nil {
		return err
	}
	return errPerfConfigNullValue
}
