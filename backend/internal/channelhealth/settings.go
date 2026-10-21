// Package channelhealth 实现渠道自动禁用/自动恢复的判定与执行面
// （W7/M4；设计 relay-01 §10、modules-06 F-06-09/12、modules-08 F-08-45）。
//
// 本包只依赖 store/audit/notify/settings 这些「副作用端」，不依赖 httpapi：
// relay 执行层通过回调把失败事实送进来，手工测试与健康 worker（M4-b）通过
// 导出的纯判定函数复用同一套规则，保证三个入口对「什么算渠道故障」只有
// 一个答案（R-M4-01 / R-M4-15）。
package channelhealth

import (
	"encoding/json"
	"math"
	"strings"
	"time"

	"newtg/gateway/internal/settings"
)

// rl.health 设置键名（modules-08 §3.16 F-08-45；键已注册，无新键）。
const (
	// KeyAutoDisableEnabled 是全局自动禁用总开关。默认 false：开启会让
	// 网关依据上游响应主动改渠道状态，属于危险方向（R-M4-01 C）。
	KeyAutoDisableEnabled = "rl.health.auto_disable_enabled"
	// KeyDisableKeywords 是响应体关键词表。M4-a 兼容三种落库形态：
	//   1. 旧 TypeString 行（换行分隔的纯文本）；
	//   2. 新 TypeList 行（JSON 字符串数组，M4-b 切换后的规范形态）；
	//   3. 缺键/脏值 → 注册表默认（R-M4-11 / R-M4-12）。
	KeyDisableKeywords = "rl.health.disable_keywords"
	// KeyAutoEnableEnabled 是自动恢复开关。默认 false（R-M4-09）：自动把
	// auto_disabled 渠道拉回流量是危险方向，必须由管理员显式开启。
	KeyAutoEnableEnabled = "rl.health.auto_enable_enabled"
	// KeyResponseTimeThresholdSeconds 是体检/手工测试的响应时间阈值
	// （R-M4-02：只在这两条路径应用，中继逐请求不计时）。
	KeyResponseTimeThresholdSeconds = "rl.health.response_time_threshold_seconds"
	// KeyTestIntervalMinutes 是体检周期（1–1440，默认 10）。每轮重读，
	// 改配置无需重启（R-M4-13）。
	KeyTestIntervalMinutes = "rl.health.test_interval_minutes"
	// KeyTestConcurrency 是每轮探测并发度（1–50，默认 5）。
	KeyTestConcurrency = "rl.health.test_concurrency"
)

// WorkerSettings 是健康 worker / 手工测试所需的一次设置快照。
//
// 内嵌 DecisionSettings：禁用判定（开关、关键词）与中继上报共用同一套
// 语义；自动恢复、阈值、调度与并发只属于体检路径（R-M4-02/09/13）。
type WorkerSettings struct {
	DecisionSettings
	// AutoEnableEnabled 默认 false；只有周期健康 worker 读取它
	// （R-M4-15：手工测试成功绝不触发恢复）。
	AutoEnableEnabled bool
	// ResponseTimeThreshold 是「响应头耗时 ≥ 阈值即记失败」的边界
	// （modules-06 F-06-09；R-M4-02/14）。比较用 >=，0 值按注册表默认。
	ResponseTimeThreshold time.Duration
	// Interval 是两轮体检之间的间隔；启动后的首次运行也等待一个 Interval
	// 再加 jitter（R-M4-13，避免重启风暴）。
	Interval time.Duration
	// Concurrency 是单轮并发探测上限（1–50）。
	Concurrency int
}

// LoadWorkerSettings 读取 rl.health 的六键，缺失/脏值回落注册表默认。
//
// configcenter.Get 只返回已存行（不合并默认），所以每个键都要显式回落；
// 数值越界一律钳制到注册表范围而不是拒绝启动——体检是后台任务，错误的
// 设置不应让 worker 沉没，但也不得把并发/间隔放大到设计之外。
func LoadWorkerSettings(src SettingsSource) WorkerSettings {
	base := LoadDecisionSettings(src)
	out := WorkerSettings{
		DecisionSettings:      base,
		AutoEnableEnabled:     registryBoolDefault(KeyAutoEnableEnabled, false),
		ResponseTimeThreshold: time.Duration(registryFloatDefault(KeyResponseTimeThresholdSeconds, 5.0) * float64(time.Second)),
		Interval:              time.Duration(clampInt(registryIntDefault(KeyTestIntervalMinutes, 10), 1, 1440)) * time.Minute,
		Concurrency:           clampInt(registryIntDefault(KeyTestConcurrency, 5), 1, 50),
	}
	if src == nil {
		return out
	}
	if raw, ok := src.Get(KeyAutoEnableEnabled); ok {
		var enabled bool
		// 与自动禁用开关同口径：只接受 JSON bool，脏值保持 false。
		if json.Unmarshal(raw, &enabled) == nil {
			out.AutoEnableEnabled = enabled
		}
	}
	if raw, ok := src.Get(KeyResponseTimeThresholdSeconds); ok && !isNullRaw(raw) {
		var seconds float64
		// 只接受 (0, MaxResponseTimeThresholdSeconds] 的有限数。上限是对
		// 复核 M-9 的纵深防御：写入路径已由注册表 maxFloat 拒绝越界值，但
		// 历史行/手工改库可能出现 1e18 一类值——float64→time.Duration 会
		// 溢出成负数，让超时判定静默失效；这里回落注册表默认（5s）。
		if json.Unmarshal(raw, &seconds) == nil && seconds > 0 &&
			seconds <= settings.MaxResponseTimeThresholdSeconds &&
			!math.IsInf(seconds, 0) && !math.IsNaN(seconds) {
			out.ResponseTimeThreshold = time.Duration(seconds * float64(time.Second))
		}
	}
	if raw, ok := src.Get(KeyTestIntervalMinutes); ok && !isNullRaw(raw) {
		var minutes int64
		if json.Unmarshal(raw, &minutes) == nil {
			out.Interval = time.Duration(clampInt(int(minutes), 1, 1440)) * time.Minute
		}
	}
	if raw, ok := src.Get(KeyTestConcurrency); ok && !isNullRaw(raw) {
		var concurrency int64
		if json.Unmarshal(raw, &concurrency) == nil {
			out.Concurrency = clampInt(int(concurrency), 1, 50)
		}
	}
	return out
}

// isNullRaw 报告配置值是否为 JSON null。
//
// json.Unmarshal 对 null 不报错（保持零值），若不显式排除，「null」会被
// 当成 0 再钳制到下限（并发 1、间隔 1 分钟），与「脏值回落注册表默认」
// 的约定不一致。缺键/清空（settings 的 CLEAR 删除整行）才是回落默认。
func isNullRaw(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// SettingsSource 是配置快照的最小读取面（configcenter.Center 实现）。
// 与 httpapi.ConfigReader 同构，但保留独立声明，避免本包反向依赖 HTTP 层。
type SettingsSource interface {
	Get(key string) (json.RawMessage, bool)
}

// DecisionSettings 是一次判定所需的设置快照。
//
// 不持有整个设置服务：ReportAttemptFailure 会在中继热路径被调用，只读取
// 两个键（configcenter 的 Get 是内存快照 + 短锁），绝不做 DB/Redis 往返。
type DecisionSettings struct {
	// AutoDisableEnabled 是全局开关；缺键/脏值一律 false（fail-closed：
	// 配置损坏时宁可不自动禁用，也不能误禁全部渠道）。
	AutoDisableEnabled bool
	// DisableKeywords 已按行 trim + lowercase，空行已剔除。
	DisableKeywords []string
}

// LoadDecisionSettings 从配置快照读取判定设置，缺键/脏值回落注册表默认。
//
// configcenter.Get 只返回「已存行」，不合并注册表默认（M4 调研 §4）；因此
// 默认值必须由消费方显式回落，不能假设键一定存在。
func LoadDecisionSettings(src SettingsSource) DecisionSettings {
	out := DecisionSettings{
		AutoDisableEnabled: registryBoolDefault(KeyAutoDisableEnabled, false),
		DisableKeywords:    registryKeywordDefaults(),
	}
	if src == nil {
		return out
	}
	if raw, ok := src.Get(KeyAutoDisableEnabled); ok {
		var enabled bool
		// 只接受 JSON bool。字符串 "true" / 数字 / null 都视为脏值并保持
		// 默认 false——不做宽松解析，避免「看起来开启了」的静默歧义。
		if json.Unmarshal(raw, &enabled) == nil {
			out.AutoDisableEnabled = enabled
		}
	}
	if raw, ok := src.Get(KeyDisableKeywords); ok {
		if keywords, ok := ParseDisableKeywords(raw); ok {
			out.DisableKeywords = keywords
		}
	}
	return out
}

// ParseDisableKeywords 解析关键词表，兼容旧 string 行与 TypeList 数组。
//
// 返回 ok=false 表示值的形态无法识别（调用方应回落注册表默认）；空字符串
// 或空数组是**合法值**，表示管理员显式清空了关键词表。
func ParseDisableKeywords(raw json.RawMessage) ([]string, bool) {
	// JSON null 是「脏值」而不是「空表」：空表用 `[]`/空串显式表达。
	// 若把 null 当成空关键词表，会静默关闭关键词触发面（fail-open），
	// 因此回落注册表默认。
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, false
	}
	var asList []string
	if err := json.Unmarshal(raw, &asList); err == nil {
		return normalizeKeywords(asList), true
	}
	var asText string
	if err := json.Unmarshal(raw, &asText); err == nil {
		return splitKeywordLines(asText), true
	}
	return nil, false
}

// splitKeywordLines 按行拆分旧 TypeString 关键词表。
func splitKeywordLines(text string) []string {
	return normalizeKeywords(strings.Split(text, "\n"))
}

// normalizeKeywords 逐项 trim + lowercase 并丢弃空项。
//
// lowercase 在加载时完成（判定函数只 lowercase body），这样热路径上每个
// 关键词只做一次大小写归一；R-M4-12 明确用 lowercase strings.Contains，
// 不引入 Aho-Corasick（7 条 × ≤64KiB 的复杂度可忽略，后续可替换）。
func normalizeKeywords(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		value := strings.ToLower(strings.TrimSpace(item))
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	return out
}

// registryBoolDefault 返回注册表布尔默认值；注册表缺键/类型不符时用 fallback。
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

// registryIntDefault 返回注册表整数默认值；缺键/类型不符时用 fallback。
func registryIntDefault(key string, fallback int) int {
	setting, ok := settings.Lookup(key)
	if !ok {
		return fallback
	}
	switch value := settings.DefaultValue(setting).(type) {
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return fallback
	}
}

// registryFloatDefault 返回注册表数值默认值；缺键/类型不符时用 fallback。
func registryFloatDefault(key string, fallback float64) float64 {
	setting, ok := settings.Lookup(key)
	if !ok {
		return fallback
	}
	switch value := settings.DefaultValue(setting).(type) {
	case float64:
		return value
	case int64:
		return float64(value)
	default:
		return fallback
	}
}

// clampInt 把配置值钳制到注册表声明的闭区间（R-M4-13 的 1–1440 / 1–50）。
// 钳制而不是报错：体检是后台任务，越界配置不应让循环停摆，但也不能
// 让并发/间隔超出设计（并发过高会放大出站风暴）。
func clampInt(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

// registryKeywordDefaults 读取注册表的关键词默认值。
//
// 当前注册表是 TypeString（M4-b 才改 TypeList，R-M4-11）；这里同时支持
// string 与 []any/[]string，保证 M4-b 切换后本函数无需再改。
func registryKeywordDefaults() []string {
	setting, ok := settings.Lookup(KeyDisableKeywords)
	if !ok {
		return nil
	}
	switch value := settings.DefaultValue(setting).(type) {
	case string:
		return splitKeywordLines(value)
	case []any:
		items := make([]string, 0, len(value))
		for _, item := range value {
			if text, ok := item.(string); ok {
				items = append(items, text)
			}
		}
		return normalizeKeywords(items)
	case []string:
		return normalizeKeywords(value)
	default:
		return nil
	}
}
