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
	"strings"

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
)

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
