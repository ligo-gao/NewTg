package ratelimit

import "strconv"

// 键形状逐字冻结（security-07 §2.2.2；relay-01 §4.1；M5 简报第 2 项）。
// 不得自创前缀或拆分维度：B10 只有 user 维（R-M5-03 裁决「每用户每模型」
// 是沿用的 UI 旧名，键里没有 model 段），B12 的 GR 标记沿用基线标记位风格。
const (
	// B10：per-user 模型桶（阈值 rl.user_model_rpm，可被 rl.group_overrides 覆盖）。
	UserKeyPrefix = "rateLimit:"
	// B11：per-token 桶（阈值 rl.token_rpm；/pg 无令牌时由调用方跳过）。
	TokenKeyPrefix = "rateLimit:token:"
	// B12：per-user 全站桶（阈值 rl.user_global_rpm，仅中继栈）。
	UserGlobalKeyPrefix = "rateLimit:v2:user:GR:"
)

// UserKey 构造 B10 桶键：rateLimit:<userID>。
func UserKey(userID int64) string {
	return UserKeyPrefix + strconv.FormatInt(userID, 10)
}

// TokenKey 构造 B11 桶键：rateLimit:token:<tokenID>。
func TokenKey(tokenID int64) string {
	return TokenKeyPrefix + strconv.FormatInt(tokenID, 10)
}

// UserGlobalKey 构造 B12 桶键：rateLimit:v2:user:GR:<userID>。
func UserGlobalKey(userID int64) string {
	return UserGlobalKeyPrefix + strconv.FormatInt(userID, 10)
}
