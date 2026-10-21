package ratelimit

import (
	"encoding/json"
	"log/slog"

	"newtg/gateway/internal/settings"
)

// 阈值键（modules-08 F-08-16 / system-config-mapping §4.7）。
const (
	KeyUserModelRPM   = "rl.user_model_rpm"
	KeyTokenRPM       = "rl.token_rpm"
	KeyUserGlobalRPM  = "rl.user_global_rpm"
	KeyGroupOverrides = "rl.group_overrides"
)

// ConfigReader 是 configcenter.Center.Get 的读取面（与 httpapi.ConfigReader
// 同形，便于单测注入；本包不依赖 httpapi，避免反向依赖）。
type ConfigReader interface {
	Get(key string) (json.RawMessage, bool)
}

// Settings 是一次请求准入所需的阈值快照。
//
// 默认值不在本包硬编码：缺键时回落 settings 注册表（modules-08 F-08-16 的
// 唯一真源 60/0/0）。这样运维改注册表默认只改一处，公开定价页与运行时
// 也不会出现两个「默认值」。
type Settings struct {
	UserModelRPM   int64
	TokenRPM       int64
	UserGlobalRPM  int64
	GroupOverrides map[string]int64
}

// Resolve 读取四个 rl 键并归一为 Settings。
//
// 口径（R-M5-11/R-M5-14）：
//   - 配置中心只返回「已落库」的键；缺键是正常状态，回落注册表默认。
//   - 非整数/负值/JSON null 是手工改库才可能的脏值：回落注册表默认并记 warn，
//     绝不 panic、也不把脏值当 0（0 在契约里是「不限」）。
//   - group_overrides 整体损坏：忽略全部覆盖 + warn；单条值损坏：只忽略该条。
//     配置损坏与 Redis 故障是两回事——后者仍由调用方 fail-closed。
//
// 每次请求调用本函数是刻意的（4 次 map 读 + 一次小 JSON 解析）：阈值必须
// ≤1s 生效，且不得缓存/重建计数（security-07 §2.2.3 第 3 条）。
func Resolve(config ConfigReader, log *slog.Logger) Settings {
	if log == nil {
		log = slog.Default()
	}
	out := Settings{
		UserModelRPM:   registryDefaultInt64(KeyUserModelRPM),
		TokenRPM:       registryDefaultInt64(KeyTokenRPM),
		UserGlobalRPM:  registryDefaultInt64(KeyUserGlobalRPM),
		GroupOverrides: map[string]int64{},
	}
	if config == nil {
		return out
	}
	out.UserModelRPM = readInt64Setting(config, KeyUserModelRPM, out.UserModelRPM, log)
	out.TokenRPM = readInt64Setting(config, KeyTokenRPM, out.TokenRPM, log)
	out.UserGlobalRPM = readInt64Setting(config, KeyUserGlobalRPM, out.UserGlobalRPM, log)
	out.GroupOverrides = readGroupOverrides(config, log)
	return out
}

// UserModelLimit 返回某分组生效的 B10 阈值：分组覆盖优先，否则全局默认。
//
// R-M5-06：group_overrides 只覆盖 B10（公开定价页的「生效值」同一口径），
// 取组由调用方从 rc.UsingGroup 解析（auto=首组、/pg=用户组）。覆盖值 0 原样
// 返回，由 Allow 的「limit<=0 不限」语义把它变成「该组不限」。
func (s Settings) UserModelLimit(group string) int {
	if limit, ok := s.GroupOverrides[group]; ok {
		return int(limit)
	}
	return int(s.UserModelRPM)
}

// TokenLimit 返回 B11 阈值。B11 没有分组覆盖语义（R-M5-06）。
func (s Settings) TokenLimit() int {
	return int(s.TokenRPM)
}

// UserGlobalLimit 返回 B12 阈值。B12 没有分组覆盖语义（R-M5-06）。
func (s Settings) UserGlobalLimit() int {
	return int(s.UserGlobalRPM)
}

// registryDefaultInt64 读注册表默认值。键未登记时返回 0（不得 panic：
// 本包可能在注册表尚未演进的中间版本被调用，退化为「不限」比打死中继安全）。
func registryDefaultInt64(key string) int64 {
	setting, ok := settings.Lookup(key)
	if !ok {
		return 0
	}
	value, ok := settings.DefaultValue(setting).(int64)
	if !ok {
		return 0
	}
	return value
}

// readInt64Setting 读一个整数阈值；缺失/脏值回落 fallback。
//
// 用 *int64 而不是 int64 接收，是为了把 JSON null 也判为脏值：null 解到
// int64 会静默变成 0，那会把「未配置」误读成「不限」。
func readInt64Setting(config ConfigReader, key string, fallback int64, log *slog.Logger) int64 {
	raw, ok := config.Get(key)
	if !ok {
		return fallback
	}
	var value *int64
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		log.Warn("ratelimit: invalid setting; falling back to registry default",
			"key", key, "fallback", fallback)
		return fallback
	}
	if *value < 0 {
		log.Warn("ratelimit: negative setting; falling back to registry default",
			"key", key, "value", *value, "fallback", fallback)
		return fallback
	}
	return *value
}

// readGroupOverrides 解析 {group: rpm}。整体损坏时返回空表（全部走默认），
// 单条非整数/负值/null 时只丢弃该条并 warn（R-M5-14）。
func readGroupOverrides(config ConfigReader, log *slog.Logger) map[string]int64 {
	out := map[string]int64{}
	raw, ok := config.Get(KeyGroupOverrides)
	if !ok {
		return out
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		log.Warn("ratelimit: invalid group_overrides; ignoring all overrides", "error", err)
		return out
	}
	for group, item := range entries {
		var value *int64
		if err := json.Unmarshal(item, &value); err != nil || value == nil {
			log.Warn("ratelimit: invalid group override; ignoring entry", "group", group)
			continue
		}
		if *value < 0 {
			log.Warn("ratelimit: negative group override; ignoring entry", "group", group, "value", *value)
			continue
		}
		out[group] = *value
	}
	return out
}
