// Package version 保存构建期注入的版本信息（Task 1 LDFLAGS 的注入目标）。
package version

// 以下三个变量由构建系统通过 -X newtg/gateway/internal/version.<Name>=<value> 注入。
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)
