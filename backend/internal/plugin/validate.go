package plugin

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// 校验规则编号直接采用设计编号（relay-06 §4.2）：T1–T10 是结构/语义规则，
// D1–D7 是「描述 = 计费主语 + 单价」的可机检代理。错误里带编号，测试断言
// 与 UI 聚合都以编号为准，而不是错误文案。

const (
	ruleT7  = "T7"
	ruleT10 = "T10"
)

// 必需生命周期钩子（relay-06 §3.6）。
var alwaysRequiredHooks = []string{
	"buildSubmitRequest",
	"parseSubmitResponse",
	"parseTaskResult",
}

// validateManifest 在结构解码之后执行跨字段语义校验。
//
// hooks 是导出对象上的函数名集合；manifest 的结构错误已在解码阶段收集。
func validateManifest(key string, m *Manifest, hooks map[string]struct{}) (Issues, Issues) {
	d := &decoder{key: key}

	validateRequiredHooks(d, m, hooks)
	validateArtifactPair(d, hooks)
	validateProtocolHooks(d, m, hooks)
	validateRouteHooks(d, m, hooks)
	validateCapabilities(d, m)
	validateUsage(d, m)

	return d.issues, d.warnings
}

// warn 记录 D7 告警：告警不阻止加载，但必须暴露给管理端
// （relay-06 §4.2 D7、§16 未闭合项 1「语义规范不可机检」）。
func (d *decoder) warn(fieldPath, locale, rule, format string, args ...any) {
	d.warnings = append(d.warnings, issuef(d.key, fieldPath, locale, rule, format, args...))
}

func validateRequiredHooks(d *decoder, m *Manifest, hooks map[string]struct{}) {
	required := alwaysRequiredHooks
	switch m.FetchMode {
	case "per_task":
		// per_task：一次查一条任务，必须能构造单任务查询请求。
		required = append(append([]string{}, required...), "buildQueryRequest")
	case "batch":
		// batch：一次拉多条任务；批量构造与批量解析必须成对出现。
		required = append(append([]string{}, required...), "buildBatchQueryRequest", "parseBatchResult")
	}
	for _, hook := range required {
		if _, ok := hooks[hook]; !ok {
			d.add("hooks."+hook, "", "hook_missing", "fetchMode=%s 必须导出 %s", m.FetchMode, hook)
		}
	}
}

func validateArtifactPair(d *decoder, hooks map[string]struct{}) {
	_, list := hooks["listArtifacts"]
	_, content := hooks["buildContentRequest"]
	if list != content {
		d.add("hooks.listArtifacts", "", "hook_pair", "listArtifacts 与 buildContentRequest 必须成对导出")
	}
}

func validateProtocolHooks(d *decoder, m *Manifest, hooks map[string]struct{}) {
	for protocol := range m.Protocols {
		_, decode := hooks["decodeRequest"]
		switch protocol {
		case "openai_responses":
			// Responses 协议接管 create/retrieve：请求解码必备，
			// 事件渲染与终态渲染至少实现一个（relay-06 §3.6 末行）。
			_, events := hooks["renderEvents"]
			_, final := hooks["renderFinal"]
			if !decode {
				d.add("hooks.decodeRequest", "", "hook_missing", "protocols.openai_responses 需要 decodeRequest")
			}
			if !events && !final {
				d.add("hooks.renderEvents", "", "hook_missing",
					"protocols.openai_responses 需要 renderEvents 或 renderFinal")
			}
		case "openai_video", "openai_image":
			if !decode {
				d.add("hooks.decodeRequest", "", "hook_missing", "protocols.%s 需要 decodeRequest", protocol)
			}
			if _, ok := hooks["render"]; !ok {
				d.add("hooks.render", "", "hook_missing", "protocols.%s 需要 render", protocol)
			}
		}
	}
}

func validateRouteHooks(d *decoder, m *Manifest, hooks map[string]struct{}) {
	for idx, route := range m.Routes {
		path := routeFieldPath(idx)
		for _, name := range []string{route.Decode, route.Render} {
			if name == "" {
				continue
			}
			if _, ok := hooks[name]; !ok {
				d.add(path, "", "route_hook_missing", "路由引用的钩子 %q 未导出", name)
			}
		}
	}
}

func routeFieldPath(idx int) string { return "meta.routes[" + itoa(idx) + "]" }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

func validateCapabilities(d *decoder, m *Manifest) {
	for _, capability := range m.RequiredCapabilities {
		// submit-sse-delta@1 要求插件同时声明 sse 提交响应（relay-06 §3.5）；
		// 否则宿主会走「增量累积」路径却永远收不到 SSE 帧。
		if capability == "submit-sse-delta@1" && !containsString(m.SubmitResponseTypes, "sse") {
			d.add("meta.requiredCapabilities", "", "capability_mismatch",
				"submit-sse-delta@1 要求 submitResponseTypes 含 \"sse\"")
		}
	}
}

// validateUsage 实现 T7 / T10 与 D1–D7。
func validateUsage(d *decoder, m *Manifest) {
	seenModels := make(map[string]int, len(m.Models))
	for idx, profile := range m.UsageProfiles {
		path := "meta.usageProfiles[" + itoa(idx) + "]"
		for _, model := range profile.Models {
			if !containsString(m.Models, model) {
				d.add(path+".models", "", ruleT10, "模型 %q 未在 meta.models 中声明（拼写必须完全一致）", model)
				continue
			}
			if owner, dup := seenModels[model]; dup {
				d.add(path+".models", "", ruleT10, "模型 %q 已属于 usageProfiles[%d]", model, owner)
				continue
			}
			seenModels[model] = idx
		}
		// 含 token 单位的 profile 必须自带 examples（relay-06 §4.2 T10；
		// token 计费对示例的依赖最强，缺示例时定价无法在 UI 上自证）。
		if profileHasUnit(profile.Schema, "token") && len(profile.Examples) == 0 {
			d.add(path+".examples", "", ruleT10, "含 unit:\"token\" 字段的 profile 必须自带 examples")
		}
	}

	validateSchemaFields(d, m.UsageSchema, "meta.usageSchema")
	for idx, profile := range m.UsageProfiles {
		validateSchemaFields(d, profile.Schema, "meta.usageProfiles["+itoa(idx)+"].schema")
	}
}

func profileHasUnit(schema map[string]UsageField, unit string) bool {
	for _, field := range schema {
		if field.Unit == unit {
			return true
		}
	}
	return false
}

func validateSchemaFields(d *decoder, schema map[string]UsageField, base string) {
	for name, field := range schema {
		path := base + "." + name
		if field.Type == "number" {
			if len(field.Description) == 0 {
				// T7：数值字段是计费乘数，缺 description 时 UI 只能显示字段名，
				// 无法区分「数量」与「价格」——这是本设计要堵住的错标入口。
				d.add(path+".description", "", ruleT7, "数值字段必须携带 description（数值字段 = 计费乘数）")
				continue
			}
			for locale, text := range field.Description {
				validatePricingDescription(d, path+".description", locale, text, true)
			}
			continue
		}
		// 非数值字段的描述只做可机检的结构部分（D1/D2/D4），
		// 语义部分保持撰写规范（relay-06 §4.2 末段）。
		for locale, text := range field.Description {
			validatePricingDescription(d, path+".description", locale, text, false)
		}
	}
}

// validatePricingDescription 执行 D1–D7。
//
// strict=true 表示该描述属于数值字段（计费乘数），施加 D5/D6 的「主语 + 单价」
// 词根约束；非数值字段只施加 D1/D2/D4。
func validatePricingDescription(d *decoder, path, locale, text string, strict bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		d.add(path, locale, "D1", "描述去首尾空白后不得为空")
		return
	}
	if utf8.RuneCountInString(text) > 256 {
		d.add(path, locale, "D1", "描述最长 256 runes，实际 %d", utf8.RuneCountInString(text))
	}
	if hasControlChar(text) {
		d.add(path, locale, "D1", "描述不得含控制字符")
	}
	for _, r := range text {
		// D2：货币符号会让 UI/读者把用量字段误读成价格。
		if strings.ContainsRune("$¥€£₩₹₽฿元円", r) {
			d.add(path, locale, "D2", "描述不得含货币符号 %q", string(r))
			break
		}
	}
	for _, r := range text {
		// D3：阿拉伯数字（unicode.IsDigit 同时覆盖全角 ０-９ 与阿拉伯-印度数字）。
		if unicode.IsDigit(r) {
			d.add(path, locale, "D3", "描述不得含数字 %q", string(r))
			break
		}
	}
	// D4 只禁止**结尾**标点；中间标点（如 "Image, video unit price"）合法。
	runeText := []rune(trimmed)
	if strings.ContainsRune(".!?,;:。！？，；：", runeText[len(runeText)-1]) {
		d.add(path, locale, "D4", "描述不得以标点 %q 结尾", string(runeText[len(runeText)-1]))
	}
	if !strict {
		return
	}
	lower := strings.ToLower(trimmed)
	switch {
	case locale == "en":
		// D5：英文必须以 unit price 结尾（大小写不敏感）。
		if !strings.HasSuffix(lower, "unit price") {
			d.add(path, locale, "D5", "英文描述必须以 unit price 结尾")
		}
	case strings.HasPrefix(strings.ToLower(locale), "zh"):
		// D6：中文（zh / zh-TW / zh-Hans / zh-Hant）必须含「单价」。
		if !strings.Contains(text, "单价") {
			d.add(path, locale, "D6", "中文描述必须包含 单价")
		}
	default:
		// D7：其它 locale 的语义规范不可机检——加载不拒绝，但必须告警。
		d.warn(path, locale, "D7", "非 en/zh 描述的语义规范不可机检（仅校验结构）")
	}
}
