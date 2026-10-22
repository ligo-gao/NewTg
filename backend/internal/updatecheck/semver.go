// Package updatecheck 实现 modules-08 §3.15 F-08-42 的服务端更新检查。
//
// 设计要点（本包只实现后端抓取与比较，不含 HTTP 外观）：
//   - 版本真源是构建注入的 version.Version；比较按 semver（含 - 预发布
//     后缀的优先级规则），不可解析或 0.0.0 一律判「无法比较」，绝不报有更新。
//   - 更新源由管理员配置，服务端出站必须先过 egress（sec.ssrf.*）；本功能
//     的 allow_private_address 只抬高本次出站的 AllowPrivateIP，不改变全局。
//   - 结果只进 Redis 缓存（TTL = interval_seconds），不写 PG、不写审计；
//     「立即检查」绕缓存但受固定 60s 最小间隔限制。
//   - 响应与错误绝不回显更新源 URL（可能含内网拓扑或查询串凭据）。
package updatecheck

import (
	"regexp"
	"strconv"
	"strings"
)

// Semver 是用于比较的最小 semver 结构。Build 元数据不参与优先级比较
// （semver §10：build metadata MUST be ignored when determining precedence），
// 因此解析后直接丢弃。
type Semver struct {
	Major uint64
	Minor uint64
	Patch uint64
	// Pre 是点分隔的预发布标识；空表示正式版（正式版优先级高于任何预发布）。
	Pre []string
}

var semverIdentPattern = regexp.MustCompile(`^[0-9A-Za-z-]+$`)

// ParseSemver 解析可带 v/V 前缀的版本串。
//
// 刻意不接受两位或简写版本（如 1.2）：设计 Q4 冻结为 semver；把非 semver
// 当版本比较会让 `0.2` 与 `0.2.0` 的关系变成猜测，宁可判「无法比较」。
// 数字前导零同样按 semver 拒绝（`01.0.0` 不可解析）。
func ParseSemver(raw string) (Semver, bool) {
	text := strings.TrimSpace(raw)
	if len(text) > 0 && (text[0] == 'v' || text[0] == 'V') {
		text = text[1:]
	}
	if text == "" {
		return Semver{}, false
	}

	// 先剥离 build metadata，再按第一个 '-' 分出预发布。
	if plus := strings.IndexByte(text, '+'); plus >= 0 {
		build := text[plus+1:]
		if build == "" || !validIdentifiers(build, false) {
			return Semver{}, false
		}
		text = text[:plus]
	}
	prePart := ""
	if dash := strings.IndexByte(text, '-'); dash >= 0 {
		prePart = text[dash+1:]
		text = text[:dash]
		if prePart == "" || !validIdentifiers(prePart, true) {
			return Semver{}, false
		}
	}

	parts := strings.Split(text, ".")
	if len(parts) != 3 {
		return Semver{}, false
	}
	major, ok := parseCoreNumber(parts[0])
	if !ok {
		return Semver{}, false
	}
	minor, ok := parseCoreNumber(parts[1])
	if !ok {
		return Semver{}, false
	}
	patch, ok := parseCoreNumber(parts[2])
	if !ok {
		return Semver{}, false
	}

	version := Semver{Major: major, Minor: minor, Patch: patch}
	if prePart != "" {
		version.Pre = strings.Split(prePart, ".")
	}
	return version, true
}

func parseCoreNumber(raw string) (uint64, bool) {
	// semver 核心数字禁止前导零：`01` 不是合法版本段（`0` 本身合法）。
	if raw == "" || (len(raw) > 1 && raw[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func validIdentifiers(raw string, numericRules bool) bool {
	for _, ident := range strings.Split(raw, ".") {
		if ident == "" || !semverIdentPattern.MatchString(ident) {
			return false
		}
		if numericRules && isNumericIdentifier(ident) && len(ident) > 1 && ident[0] == '0' {
			return false
		}
	}
	return true
}

func isNumericIdentifier(raw string) bool {
	if raw == "" {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return false
		}
	}
	return true
}

// IsZero 报告是否为 0.0.0（构建未注入版本时的默认值）。
//
// 设计 Q4 明确：v0.0.0 判「无法比较」而不是「有更新」。0.0.0 在语义上
// 也可能是一个真实版本，但对本产品而言它只代表「构建期没注入 VERSION」。
func (v Semver) IsZero() bool {
	return v.Major == 0 && v.Minor == 0 && v.Patch == 0
}

// CompareSemver 按 semver §11 返回 -1 / 0 / 1。
func CompareSemver(a, b Semver) int {
	if cmp := compareUint(a.Major, b.Major); cmp != 0 {
		return cmp
	}
	if cmp := compareUint(a.Minor, b.Minor); cmp != 0 {
		return cmp
	}
	if cmp := compareUint(a.Patch, b.Patch); cmp != 0 {
		return cmp
	}
	// 正式版 > 预发布版。
	if len(a.Pre) == 0 && len(b.Pre) == 0 {
		return 0
	}
	if len(a.Pre) == 0 {
		return 1
	}
	if len(b.Pre) == 0 {
		return -1
	}
	for i := 0; i < len(a.Pre) && i < len(b.Pre); i++ {
		if cmp := comparePrereleaseIdentifier(a.Pre[i], b.Pre[i]); cmp != 0 {
			return cmp
		}
	}
	return compareInt(len(a.Pre), len(b.Pre))
}

func comparePrereleaseIdentifier(a, b string) int {
	aNumeric, bNumeric := isNumericIdentifier(a), isNumericIdentifier(b)
	switch {
	case aNumeric && bNumeric:
		aValue, _ := strconv.ParseUint(a, 10, 64)
		bValue, _ := strconv.ParseUint(b, 10, 64)
		return compareUint(aValue, bValue)
	case aNumeric:
		// 数字标识优先级低于字母数字标识。
		return -1
	case bNumeric:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func compareUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
