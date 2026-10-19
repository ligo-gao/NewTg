package plugin

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxKeyLen 是插件 key 的长度上限（relay-06 §2.2 表 + T1）。
//
// 冻结为 30 有两处依据：manifest 的 `key` 上限（T1）与持久化列
// `tasks.plugin_key varchar(30)` / `task_plugins.key` 的 CHECK
// （M6d R-M6D-01 把 DB 上限统一到 30）。两处必须同值，否则会出现
// 「能加载但写不进任务行」的分裂。
const MaxKeyLen = 30

// keyPattern 限制 key 可用于 URL 路径与表索引（relay-06 §2.2 只写了长度，
// 这里补最小字符集约束：空白/控制字符会让 `/v1/tasks/:key` 路由不可达）。
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// semverPattern 是对 §2.2「version: semver」的宽松实现：接受可选的 v 前缀
// 与 prerelease/build 后缀，拒绝空串与任意文本。刻意不引第三方 semver 库：
// 校验只需判形，版本比较由管理端的「版本行创建时间倒序」承担。
var semverPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$`)

// Author 是 manifest.author（净室 v1 契约：`{name, url?}`）。
type Author struct {
	Name string
	URL  string
}

// Route 是 manifest.routes[] 的一条原生路由声明（relay-06 §2.2/§3.1）。
//
// 语义要点：
//   - `submit` / `dynamic` 必须同时声明 decode 与 render；
//   - `query` 只声明 render，**禁** decode，并用 taskIdParam（默认 task_id）；
//   - retainResult 默认 true，仅对即时终态生效（relay-06 §2.2）。
type Route struct {
	Method       string
	Path         string
	Kind         string
	Decode       string
	Render       string
	TaskIDParam  string
	RetainResult *bool
	Models       []string
}

// UsageField 是 usageSchema / usageProfiles[].schema 的一个字段声明。
//
// 数值字段（Type=="number"）是**计费乘数**，其 description 必须通过
// D1–D7（relay-06 §4.2）；这是 T3 任务把「撰写规范」升级为加载期闸门的核心。
type UsageField struct {
	Type        string // number | boolean | enum | string
	Unit        string // second | count | token | credit（仅 number）
	UnitLabel   map[string]string
	Enum        []string
	EnumLabels  map[string]map[string]string
	Description map[string]string
}

// UsageProfile 是 usageProfiles[] 的一项：模型 → 用量 schema 变体。
type UsageProfile struct {
	Models   []string
	Schema   map[string]UsageField
	Examples []any
}

// AuthSpec 是 manifest.auth 的规范化形态。
//
// 取值集合由 T3 冻结：字符串 "none"/"api_key"/"vertex_oauth"，或对象
// `{type: "none"|"api_key"|"oauth2_jwt"}`。未知形态在加载期拒绝，避免
// 运行期以「默认 api_key」隐式兜底。
type AuthSpec struct {
	Type string
	Raw  any
}

// Manifest 是插件导出对象的**元数据**部分（函数钩子单独记录）。
type Manifest struct {
	APIVersion           int
	Key                  string
	Name                 string
	Version              string
	Author               Author
	Description          map[string]string
	Models               []string
	FetchMode            string
	ChannelTypes         []int
	Routes               []Route
	Protocols            map[string]any
	UsageSchema          map[string]UsageField
	UsageProfiles        []UsageProfile
	UsageExamples        []any
	BaseURL              string
	AllowedHosts         []string
	Upstreams            []string
	RequiredCapabilities []string
	SubmitResponseTypes  []string
	Auth                 AuthSpec
}

// knownMetaKeys 是 v1 manifest 的顶层字段全集。
//
// 严格拒绝未知字段（relay-06 §4.5「扩展的兼容位」末段：严格宿主拒绝未知
// 字段，安装前先升级宿主）。这同时落实 T6：filePlaceholder 不是 manifest
// 字段，任何把 `$defs.filePlaceholder` 抄进 meta 的源都会被拒。
var knownMetaKeys = map[string]struct{}{
	"apiVersion": {}, "key": {}, "name": {}, "version": {}, "author": {},
	"description": {}, "models": {}, "fetchMode": {}, "channelTypes": {},
	"routes": {}, "protocols": {}, "usageSchema": {}, "usageProfiles": {},
	"usageExamples": {}, "baseUrl": {}, "allowedHosts": {}, "upstreams": {},
	"requiredCapabilities": {}, "submitResponseTypes": {}, "auth": {},
}

var knownRouteKeys = map[string]struct{}{
	"method": {}, "path": {}, "kind": {}, "decode": {}, "render": {},
	"taskIdParam": {}, "retainResult": {}, "models": {},
}

var knownUsageFieldKeys = map[string]struct{}{
	"type": {}, "unit": {}, "unitLabel": {}, "enum": {}, "enumLabels": {},
	"description": {},
}

var knownUsageProfileKeys = map[string]struct{}{
	"models": {}, "schema": {}, "examples": {},
}

var knownAuthorKeys = map[string]struct{}{"name": {}, "url": {}}

// supportedCapabilities 是宿主已实现的版本化能力（relay-06 §3.5）。
//
// `json-clone@1` 由本任务的最小 utils 提供；`submit-sse-delta@1` 只做声明
// 校验（要求 submitResponseTypes 含 sse），运行期钩子由后续 SSE 任务接线。
var supportedCapabilities = map[string]struct{}{
	"json-clone@1":       {},
	"submit-sse-delta@1": {},
}

// decoder 累积结构化错误，避免「遇到第一条错误就返回」导致 UI 只能看到一条。
//
// warnings 是告警侧（D7）：与 issues 分开收集，因为告警不阻止加载。
type decoder struct {
	key      string
	issues   Issues
	warnings Issues
}

func (d *decoder) add(fieldPath, locale, rule, format string, args ...any) {
	d.issues = append(d.issues, issuef(d.key, fieldPath, locale, rule, format, args...))
}

// typeError 记录类型不匹配。字段路径与期望类型都进消息，规则统一为
// `field_type`，使「缺字段」与「类型错」在 UI 上可区分。
func (d *decoder) typeError(fieldPath, want string, got any) {
	d.add(fieldPath, "", "field_type", "期望 %s，实际 %T", want, got)
}

// checkUnknown 落实「严格拒绝未知字段」。
func (d *decoder) checkUnknown(obj map[string]any, path string, allowed map[string]struct{}, rule string) {
	for name := range obj {
		if _, ok := allowed[name]; !ok {
			d.add(path+name, "", rule, "未知字段 %q（v1 严格拒绝未知字段）", name)
		}
	}
}

func asObject(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func asArray(v any) ([]any, bool) {
	a, ok := v.([]any)
	return a, ok
}

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func asBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// asNumber 接受 Sobek Export 可能产出的所有数值形状。
//
// Sobek 对整数通常导出 int64，对浮点导出 float64；管理端上传路径还可能出现
// json.Number（未来直接从 JSON 解析时）。三者都归一为 float64 以便比较；
// 需要整数的字段再单独判「无小数」。
func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	default:
		return 0, false
	}
}

func asInt(v any) (int, bool) {
	f, ok := asNumber(v)
	if !ok || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}

// stringArray 解码去重字符串数组；空元素与重复元素都拒绝。
// 返回的去重语义是「首见保留」：manifest 去重是硬要求（§2.2 models 非空、去重）。
func (d *decoder) stringArray(v any, path string, allowEmpty bool) ([]string, bool) {
	arr, ok := asArray(v)
	if !ok {
		d.typeError(path, "字符串数组", v)
		return nil, false
	}
	if !allowEmpty && len(arr) == 0 {
		d.add(path, "", "array_empty", "数组不得为空")
		return nil, false
	}
	out := make([]string, 0, len(arr))
	seen := make(map[string]struct{}, len(arr))
	for idx, item := range arr {
		s, ok := asString(item)
		if !ok {
			d.typeError(fmt.Sprintf("%s[%d]", path, idx), "字符串", item)
			continue
		}
		if strings.TrimSpace(s) == "" {
			d.add(fmt.Sprintf("%s[%d]", path, idx), "", "array_item_empty", "元素不得为空")
			continue
		}
		if _, dup := seen[s]; dup {
			d.add(fmt.Sprintf("%s[%d]", path, idx), "", "array_duplicate", "重复元素 %q", s)
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out, true
}

// decodeManifest 把导出对象的元数据部分解码为 Manifest 并执行结构校验。
//
// 传进来的 raw 只含非函数属性（函数由 compile 阶段单独记录为钩子）。
func decodeManifest(key string, raw map[string]any) (*Manifest, Issues) {
	d := &decoder{key: key}
	m := &Manifest{SubmitResponseTypes: []string{"json"}}

	d.checkUnknown(raw, "meta.", knownMetaKeys, "meta_unknown_field")

	// —— apiVersion：恒为数字 1（冻结约定 2；字符串 "1" 不归一，fail-closed）——
	if v, ok := raw["apiVersion"]; ok {
		n, ok := asInt(v)
		if !ok {
			d.typeError("meta.apiVersion", "数字 1", v)
		} else if n != 1 {
			d.add("meta.apiVersion", "", "api_version", "apiVersion 必须为 1，实际 %d", n)
		} else {
			m.APIVersion = n
		}
	} else {
		d.add("meta.apiVersion", "", "api_version", "缺少 apiVersion")
	}

	// —— key：T1 ——
	if v, ok := raw["key"]; ok {
		s, ok := asString(v)
		if !ok {
			d.typeError("meta.key", "字符串", v)
		} else {
			m.Key = s
			validateKey(d, s)
		}
	} else {
		d.add("meta.key", "", "T1", "缺少 key")
	}

	// —— name / version：§2.2 必需字段 ——
	if v, ok := raw["name"]; ok {
		s, ok := asString(v)
		if !ok {
			d.typeError("meta.name", "字符串", v)
		} else if strings.TrimSpace(s) == "" {
			d.add("meta.name", "", "meta_required", "name 不得为空")
		} else {
			m.Name = s
		}
	} else {
		d.add("meta.name", "", "meta_required", "缺少 name")
	}

	if v, ok := raw["version"]; ok {
		s, ok := asString(v)
		if !ok {
			d.typeError("meta.version", "字符串", v)
		} else if !semverPattern.MatchString(s) {
			d.add("meta.version", "", "semver", "version %q 不是 semver", s)
		} else {
			m.Version = s
		}
	} else {
		d.add("meta.version", "", "meta_required", "缺少 version")
	}

	m.Author = d.decodeAuthor(raw["author"])

	// —— description：T2 ——
	if v, ok := raw["description"]; ok {
		m.Description = d.localizedText(v, "meta.description", 512, "T2", true)
	} else {
		d.add("meta.description", "", "T2", "缺少 description")
	}

	// —— models：非空、去重 ——
	if v, ok := raw["models"]; ok {
		if models, ok := d.stringArray(v, "meta.models", false); ok {
			m.Models = models
		}
	} else {
		d.add("meta.models", "", "meta_required", "缺少 models")
	}

	// —— fetchMode ——
	if v, ok := raw["fetchMode"]; ok {
		s, ok := asString(v)
		if !ok {
			d.typeError("meta.fetchMode", "字符串", v)
		} else if s != "per_task" && s != "batch" {
			d.add("meta.fetchMode", "", "fetch_mode", "fetchMode 仅允许 per_task | batch，实际 %q", s)
		} else {
			m.FetchMode = s
		}
	} else {
		d.add("meta.fetchMode", "", "meta_required", "缺少 fetchMode")
	}

	m.ChannelTypes = d.decodeChannelTypes(raw["channelTypes"])
	m.Routes = d.decodeRoutes(raw["routes"])
	m.Protocols = d.decodeProtocols(raw["protocols"])

	if v, ok := raw["usageSchema"]; ok {
		m.UsageSchema = d.decodeUsageFields(v, "meta.usageSchema")
	}
	if v, ok := raw["usageProfiles"]; ok {
		m.UsageProfiles = d.decodeUsageProfiles(v)
	}
	if v, ok := raw["usageExamples"]; ok {
		arr, ok := asArray(v)
		if !ok {
			d.typeError("meta.usageExamples", "数组", v)
		} else {
			if len(arr) > 16 {
				d.add("meta.usageExamples", "", "T4", "usageExamples 最多 16 条，实际 %d", len(arr))
			}
			m.UsageExamples = arr
		}
	}

	m.BaseURL = d.decodeBaseURL(raw["baseUrl"])
	m.AllowedHosts = d.decodeAllowedHosts(raw["allowedHosts"])
	m.Upstreams = d.decodeUpstreams(raw["upstreams"])
	m.RequiredCapabilities = d.decodeCapabilities(raw["requiredCapabilities"])
	m.SubmitResponseTypes = d.decodeSubmitResponseTypes(raw["submitResponseTypes"])
	m.Auth = d.decodeAuth(raw["auth"])

	return m, d.issues
}

func validateKey(d *decoder, key string) {
	if utf8.RuneCountInString(key) > MaxKeyLen {
		d.add("meta.key", "", "T1", "key 最长 %d 字符，实际 %d", MaxKeyLen, utf8.RuneCountInString(key))
	}
	if !keyPattern.MatchString(key) {
		d.add("meta.key", "", "T1", "key %q 含非法字符（仅允许字母/数字/._-，首字符为字母或数字）", key)
	}
}

func (d *decoder) decodeAuthor(v any) Author {
	obj, ok := asObject(v)
	if !ok {
		d.typeError("meta.author", "对象 {name, url?}", v)
		return Author{}
	}
	d.checkUnknown(obj, "meta.author.", knownAuthorKeys, "meta_unknown_field")
	var out Author
	if name, ok := asString(obj["name"]); ok && strings.TrimSpace(name) != "" {
		out.Name = name
	} else {
		d.add("meta.author.name", "", "meta_required", "author.name 必须为非空字符串")
	}
	if u, ok := obj["url"]; ok {
		s, ok := asString(u)
		if !ok {
			d.typeError("meta.author.url", "字符串", u)
		} else if !isAbsoluteHTTP(s) {
			d.add("meta.author.url", "", "author_url", "author.url 必须是绝对 http(s) URL")
		} else {
			out.URL = s
		}
	}
	return out
}

func isAbsoluteHTTP(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host != ""
}

// localizedText 解码 localizedText 并施加长度/必需 locale/控制字符/去重校验。
//
// T2 与 T5 共用同一实现：设计把「描述」与「enum label」的 localizedText
// 约束写成同一组规则（en 必需、去重、非空、无控制字符），只是上限不同
// （512 vs 256）。共用避免两套规则的漂移。
func (d *decoder) localizedText(v any, path string, maxRunes int, rule string, requireEN bool) map[string]string {
	obj, ok := asObject(v)
	if !ok {
		d.typeError(path, "本地化文本对象 {locale: string}", v)
		return nil
	}
	out := make(map[string]string, len(obj))
	seen := make(map[string]string, len(obj))
	for locale, raw := range obj {
		normalized := strings.ToLower(locale)
		if _, dup := seen[normalized]; dup {
			d.add(path+"."+locale, locale, rule, "locale %q 重复（大小写归一后冲突）", locale)
			continue
		}
		seen[normalized] = locale

		s, ok := asString(raw)
		if !ok {
			d.typeError(path+"."+locale, "字符串", raw)
			continue
		}
		if strings.TrimSpace(s) == "" {
			d.add(path+"."+locale, locale, rule, "文本不得为空")
			continue
		}
		if utf8.RuneCountInString(s) > maxRunes {
			d.add(path+"."+locale, locale, rule, "文本最长 %d runes，实际 %d", maxRunes, utf8.RuneCountInString(s))
			continue
		}
		if hasControlChar(s) {
			d.add(path+"."+locale, locale, rule, "文本不得含控制字符")
			continue
		}
		out[locale] = s
	}
	// en 是否必需由调用方决定：meta.description（T2）与 enumLabels（T5）
	// 明确要求 en；数值字段描述因 D5 以 en 为判定对象也要求 en；
	// 非数值字段描述只做 D1/D2/D4（relay-06 §4.2 末段），不强制 en。
	if requireEN {
		if _, ok := seen["en"]; !ok {
			d.add(path+".en", "en", rule, "必须提供 en 文本")
		}
	}
	return out
}

func hasControlChar(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func (d *decoder) decodeChannelTypes(v any) []int {
	if v == nil {
		return nil
	}
	arr, ok := asArray(v)
	if !ok {
		d.typeError("meta.channelTypes", "整数数组", v)
		return nil
	}
	out := make([]int, 0, len(arr))
	seen := make(map[int]struct{}, len(arr))
	for idx, item := range arr {
		n, ok := asInt(item)
		if !ok || n <= 0 {
			d.add(fmt.Sprintf("meta.channelTypes[%d]", idx), "", "channel_type", "渠道类型必须为正整数")
			continue
		}
		// 60/61 是「多渠道绑定 / 任务插件单绑」的特殊类型，不是内置插件的
		// 静态映射值（relay-06 §1.3、§2.2）；声明即拒绝。
		if n == 60 || n == 61 {
			d.add(fmt.Sprintf("meta.channelTypes[%d]", idx), "", "channel_type", "channelTypes 禁止 60/61")
			continue
		}
		if _, dup := seen[n]; dup {
			d.add(fmt.Sprintf("meta.channelTypes[%d]", idx), "", "channel_type", "重复渠道类型 %d", n)
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

func (d *decoder) decodeRoutes(v any) []Route {
	if v == nil {
		return nil
	}
	arr, ok := asArray(v)
	if !ok {
		d.typeError("meta.routes", "数组", v)
		return nil
	}
	out := make([]Route, 0, len(arr))
	for idx, item := range arr {
		path := fmt.Sprintf("meta.routes[%d]", idx)
		obj, ok := asObject(item)
		if !ok {
			d.typeError(path, "对象", item)
			continue
		}
		d.checkUnknown(obj, path+".", knownRouteKeys, "meta_unknown_field")
		var route Route
		if s, ok := asString(obj["method"]); ok && strings.TrimSpace(s) != "" {
			route.Method = strings.ToUpper(strings.TrimSpace(s))
		} else {
			d.add(path+".method", "", "route_method", "method 必须为非空字符串")
		}
		if s, ok := asString(obj["path"]); ok && strings.HasPrefix(s, "/") {
			route.Path = s
		} else {
			d.add(path+".path", "", "route_path", "path 必须以 / 开头")
		}
		if s, ok := asString(obj["kind"]); ok {
			route.Kind = s
		} else {
			d.add(path+".kind", "", "route_kind", "kind 必须为 submit | query | dynamic")
		}
		route.Decode, _ = asString(obj["decode"])
		route.Render, _ = asString(obj["render"])
		if s, ok := asString(obj["taskIdParam"]); ok {
			route.TaskIDParam = s
		}
		if b, ok := asBool(obj["retainResult"]); ok {
			route.RetainResult = &b
		}
		if models, ok := obj["models"]; ok {
			route.Models, _ = d.stringArray(models, path+".models", false)
		}

		switch route.Kind {
		case "submit", "dynamic":
			if route.Decode == "" {
				d.add(path+".decode", "", "route_hook_missing", "%s 路由必须声明 decode", route.Kind)
			}
			if route.Render == "" {
				d.add(path+".render", "", "route_hook_missing", "%s 路由必须声明 render", route.Kind)
			}
			if route.TaskIDParam != "" {
				d.add(path+".taskIdParam", "", "route_task_id_param", "仅 query 路由允许 taskIdParam")
			}
		case "query":
			if route.Render == "" {
				d.add(path+".render", "", "route_hook_missing", "query 路由必须声明 render")
			}
			if route.Decode != "" {
				d.add(path+".decode", "", "route_decode_forbidden", "query 路由禁止 decode")
			}
		case "":
			// 已在上方报 missing kind
		default:
			d.add(path+".kind", "", "route_kind", "未知路由类型 %q", route.Kind)
		}
		out = append(out, route)
	}
	return out
}

func (d *decoder) decodeProtocols(v any) map[string]any {
	if v == nil {
		return nil
	}
	obj, ok := asObject(v)
	if !ok {
		d.typeError("meta.protocols", "对象", v)
		return nil
	}
	known := map[string]struct{}{
		"openai_responses": {}, "openai_video": {}, "openai_image": {},
	}
	for name, value := range obj {
		if _, ok := known[name]; !ok {
			d.add("meta.protocols."+name, "", "protocol_unknown", "未知宿主协议 %q", name)
			continue
		}
		if name == "openai_responses" {
			// openai_responses 必须是对象且带 supports（relay-06 §3.1）；
			// 裸字符串会让「接管哪些操作」变成隐式推断，fail-closed。
			spec, ok := asObject(value)
			if !ok {
				d.add("meta.protocols."+name, "", "protocol_shape", "openai_responses 必须是带 supports 的对象")
			} else if spec["supports"] == nil {
				d.add("meta.protocols."+name+".supports", "", "protocol_shape", "openai_responses 必须声明 supports")
			}
		}
	}
	return obj
}

func (d *decoder) decodeUsageFields(v any, path string) map[string]UsageField {
	obj, ok := asObject(v)
	if !ok {
		d.typeError(path, "对象", v)
		return nil
	}
	out := make(map[string]UsageField, len(obj))
	for name, raw := range obj {
		fieldPath := path + "." + name
		field, ok := d.decodeUsageField(raw, fieldPath)
		if ok {
			out[name] = field
		}
	}
	return out
}

func (d *decoder) decodeUsageField(v any, path string) (UsageField, bool) {
	obj, ok := asObject(v)
	if !ok {
		d.typeError(path, "对象", v)
		return UsageField{}, false
	}
	d.checkUnknown(obj, path+".", knownUsageFieldKeys, "usage_field_unknown")
	var field UsageField

	typ, _ := asString(obj["type"])
	if typ == "" && obj["enum"] != nil {
		typ = "enum"
	}
	switch typ {
	case "number", "boolean", "enum", "string":
		field.Type = typ
	case "":
		d.add(path+".type", "", "T8", "缺少 type")
		return UsageField{}, false
	default:
		d.add(path+".type", "", "T8", "未知用量字段类型 %q", typ)
		return UsageField{}, false
	}

	if desc, ok := obj["description"]; ok {
		field.Description = d.localizedText(desc, path+".description", 256, "D1", typ == "number")
	}

	if unit, ok := obj["unit"]; ok {
		s, ok := asString(unit)
		if !ok {
			d.typeError(path+".unit", "字符串", unit)
		} else {
			field.Unit = s
		}
	}
	if label, ok := obj["unitLabel"]; ok {
		field.UnitLabel = d.localizedText(label, path+".unitLabel", 256, "T5", true)
	}
	if enum, ok := obj["enum"]; ok {
		field.Enum, _ = d.stringArray(enum, path+".enum", false)
	}
	if labels, ok := obj["enumLabels"]; ok {
		field.EnumLabels = d.decodeEnumLabels(labels, path+".enumLabels")
	}

	// —— T8 / T9：单位与字段类型的组合约束 ——
	switch field.Type {
	case "number":
		if field.Unit != "" && !validUnit(field.Unit) {
			d.add(path+".unit", "", "T8", "unit %q 不在 {second,count,token,credit} 内", field.Unit)
		}
		if field.Enum != nil || field.EnumLabels != nil {
			d.add(path, "", "T9", "数值字段不得声明 enum/enumLabels")
		}
		if field.UnitLabel != nil && field.Unit != "count" {
			d.add(path+".unitLabel", "", "T9", "unitLabel 仅 unit:\"count\" 可声明")
		}
	case "boolean":
		if field.Unit != "" || field.UnitLabel != nil {
			d.add(path, "", "T8", "布尔字段不得声明 unit/unitLabel")
		}
		if field.Enum != nil || field.EnumLabels != nil {
			d.add(path, "", "T9", "布尔字段不得声明 enum/enumLabels")
		}
	case "enum", "string":
		if field.Unit != "" || field.UnitLabel != nil {
			d.add(path, "", "T8", "枚举字段不得声明 unit/unitLabel")
		}
		if len(field.Enum) == 0 {
			d.add(path+".enum", "", "T9", "枚举字段必须声明非空 enum")
		}
		for value := range field.EnumLabels {
			if !containsString(field.Enum, value) {
				d.add(path+".enumLabels."+value, "", "T9", "enumLabels 的 key %q 未在 enum 中声明", value)
			}
		}
	}
	return field, true
}

func (d *decoder) decodeEnumLabels(v any, path string) map[string]map[string]string {
	obj, ok := asObject(v)
	if !ok {
		d.typeError(path, "对象", v)
		return nil
	}
	out := make(map[string]map[string]string, len(obj))
	for value, raw := range obj {
		out[value] = d.localizedText(raw, path+"."+value, 256, "T5", true)
	}
	return out
}

func validUnit(unit string) bool {
	switch unit {
	case "second", "count", "token", "credit":
		return true
	default:
		return false
	}
}

func (d *decoder) decodeUsageProfiles(v any) []UsageProfile {
	arr, ok := asArray(v)
	if !ok {
		d.typeError("meta.usageProfiles", "数组", v)
		return nil
	}
	out := make([]UsageProfile, 0, len(arr))
	for idx, item := range arr {
		path := fmt.Sprintf("meta.usageProfiles[%d]", idx)
		obj, ok := asObject(item)
		if !ok {
			d.typeError(path, "对象", item)
			continue
		}
		d.checkUnknown(obj, path+".", knownUsageProfileKeys, "meta_unknown_field")
		var profile UsageProfile
		if models, ok := obj["models"]; ok {
			profile.Models, _ = d.stringArray(models, path+".models", false)
		} else {
			d.add(path+".models", "", "T10", "usageProfiles[].models 必填")
		}
		schema, present := obj["schema"]
		if !present || schema == nil {
			d.add(path+".schema", "", "T10", "usageProfiles[].schema 不得缺失或为 null（空对象合法）")
		} else if _, ok := asObject(schema); !ok {
			d.typeError(path+".schema", "对象", schema)
		} else {
			profile.Schema = d.decodeUsageFields(schema, path+".schema")
		}
		if examples, ok := obj["examples"]; ok {
			exArr, ok := asArray(examples)
			if !ok {
				d.typeError(path+".examples", "数组", examples)
			} else {
				if len(exArr) > 16 {
					d.add(path+".examples", "", "T4", "examples 最多 16 条，实际 %d", len(exArr))
				}
				profile.Examples = exArr
			}
		}
		out = append(out, profile)
	}
	return out
}

func (d *decoder) decodeBaseURL(v any) string {
	if v == nil {
		return ""
	}
	s, ok := asString(v)
	if !ok {
		d.typeError("meta.baseUrl", "字符串", v)
		return ""
	}
	if len(s) > 191 {
		d.add("meta.baseUrl", "", "base_url", "baseUrl 最长 191 字符")
	}
	u, err := url.Parse(s)
	switch {
	case err != nil || u.Host == "":
		d.add("meta.baseUrl", "", "base_url", "baseUrl 必须是绝对 URL")
	case u.Scheme != "http" && u.Scheme != "https":
		d.add("meta.baseUrl", "", "base_url", "baseUrl 仅允许 http(s)")
	case u.User != nil:
		d.add("meta.baseUrl", "", "base_url", "baseUrl 不得含凭据")
	case u.RawQuery != "":
		d.add("meta.baseUrl", "", "base_url", "baseUrl 不得含查询串")
	case u.Fragment != "":
		d.add("meta.baseUrl", "", "base_url", "baseUrl 不得含片段")
	case strings.HasSuffix(s, "/"):
		d.add("meta.baseUrl", "", "base_url", "baseUrl 不得以 / 结尾")
	case !isASCII(u.Hostname()):
		d.add("meta.baseUrl", "", "base_url", "baseUrl 主机必须为 ASCII")
	}
	return s
}

func isASCII(s string) bool {
	for idx := 0; idx < len(s); idx++ {
		if s[idx] >= 0x80 {
			return false
		}
	}
	return true
}

func (d *decoder) decodeAllowedHosts(v any) []string {
	if v == nil {
		return nil
	}
	hosts, ok := d.stringArray(v, "meta.allowedHosts", true)
	if !ok {
		return nil
	}
	for idx, host := range hosts {
		// host 或 host:port；出现 scheme/路径说明作者把 URL 填进了 host 字段，
		// 出站守卫会按 host 比对而永久不匹配，故加载期拒绝。
		if strings.ContainsAny(host, "/?#@") || strings.Contains(host, "://") {
			d.add(fmt.Sprintf("meta.allowedHosts[%d]", idx), "", "allowed_host", "allowedHosts 只接受 host 或 host:port")
		}
	}
	return hosts
}

func (d *decoder) decodeUpstreams(v any) []string {
	if v == nil {
		return nil
	}
	upstreams, ok := d.stringArray(v, "meta.upstreams", true)
	if !ok {
		return nil
	}
	for idx, upstream := range upstreams {
		if upstream != "vendor" && upstream != "new_api" {
			d.add(fmt.Sprintf("meta.upstreams[%d]", idx), "", "upstream", "upstream %q 仅允许 vendor | new_api", upstream)
		}
	}
	return upstreams
}

func (d *decoder) decodeCapabilities(v any) []string {
	if v == nil {
		return nil
	}
	caps, ok := d.stringArray(v, "meta.requiredCapabilities", true)
	if !ok {
		return nil
	}
	for idx, capability := range caps {
		if _, ok := supportedCapabilities[capability]; !ok {
			// 未知 / 重复 / 不支持的版本一律拒绝（relay-06 §3.5）：安装前必须先升级宿主。
			d.add(fmt.Sprintf("meta.requiredCapabilities[%d]", idx), "", "capability_unsupported", "不支持的能力 %q", capability)
		}
	}
	return caps
}

func (d *decoder) decodeSubmitResponseTypes(v any) []string {
	if v == nil {
		return []string{"json"}
	}
	types, ok := d.stringArray(v, "meta.submitResponseTypes", false)
	if !ok {
		return []string{"json"}
	}
	for idx, typ := range types {
		if typ != "json" && typ != "sse" {
			d.add(fmt.Sprintf("meta.submitResponseTypes[%d]", idx), "", "submit_response_type", "仅允许 json | sse，实际 %q", typ)
		}
	}
	return types
}

func (d *decoder) decodeAuth(v any) AuthSpec {
	if v == nil {
		// 缺省按 none 处理是安全的：宿主不会注入任何凭据，插件拿不到 apiKey。
		return AuthSpec{Type: "none"}
	}
	if s, ok := asString(v); ok {
		switch s {
		case "none", "api_key", "vertex_oauth":
			return AuthSpec{Type: s, Raw: v}
		default:
			d.add("meta.auth", "", "T3", "auth 取值 %q 不合法", s)
			return AuthSpec{Raw: v}
		}
	}
	obj, ok := asObject(v)
	if !ok {
		d.add("meta.auth", "", "T3", "auth 必须是字符串或 {type} 对象")
		return AuthSpec{Raw: v}
	}
	typ, ok := asString(obj["type"])
	if !ok {
		d.add("meta.auth.type", "", "T3", "auth.type 必须为字符串")
		return AuthSpec{Raw: v}
	}
	switch typ {
	case "none", "api_key", "oauth2_jwt":
		return AuthSpec{Type: typ, Raw: v}
	default:
		d.add("meta.auth.type", "", "T3", "auth.type 取值 %q 不合法", typ)
		return AuthSpec{Raw: v}
	}
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
