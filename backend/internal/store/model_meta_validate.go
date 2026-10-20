package store

// 模型档案的应用层校验（platform-01 §22.7.1；L-3）。
//
// 【为什么下沉到 store】字段规则是**数据层不变量**，不是某个 handler 的输入
// 格式：D1b 的管理面、未来的上游同步、导入工具都会写 model_meta。规则留在
// handler 里，任何新写入路径都要重新实现一遍，漏一处就会写出 DB CHECK 只能
// 部分兜住的数据（description 长度、endpoints 形态、枚举取值都没有 DB CHECK）。
// 放在 store 的 Create/Update 入口，所有路径共享同一份判定。
//
// 判定结果用 ErrModelMetaInvalid{Field, Reason}：Field 是列名，handler 可以
// 直接回填字段级错误，不需要再解析错误文本。

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// description 是展示用长文本：512 字符足够一段产品简介，再长会撑破
	// 公开页卡片与列表行高（D1-04：只做应用层限制，不落 DB CHECK）。
	maxModelMetaDescriptionRunes = 512
	// display_name 对应公开卡片的标题行（列宽 varchar(64)）。
	maxModelMetaDisplayNameRunes = 64
	// tags 面向筛选与 facets：超过 10 个标签在 UI 上不可用，也没有检索价值。
	maxModelMetaTags = 10
	// 单个标签 32 字符（trim 后），空标签没有检索意义。
	maxModelMetaTagRunes = 32
)

// modelMetaCapabilities 是 capabilities 的合法取值（F-09-15 的 facets 枚举）。
var modelMetaCapabilities = map[string]struct{}{
	"text": {}, "vision": {}, "audio": {}, "video": {},
	"function_calling": {}, "tool_use": {}, "reasoning": {},
	"embedding": {}, "rerank": {}, "image_generation": {},
	"speech": {}, "transcription": {},
}

// modelMetaModalities 是输入/输出模态的合法取值。
var modelMetaModalities = map[string]struct{}{
	"text": {}, "image": {}, "audio": {}, "video": {},
}

// modelMetaEndpointMethods 是 endpoints 值数组的合法 HTTP 方法（大小写归一）。
var modelMetaEndpointMethods = map[string]struct{}{
	"GET": {}, "POST": {}, "PUT": {}, "PATCH": {}, "DELETE": {},
}

// modelMetaParameterTypes 是 parameters[].type 的合法取值（JSON Schema 的
// 基础类型子集；公开页只渲染这几种控件）。
var modelMetaParameterTypes = map[string]struct{}{
	"string": {}, "number": {}, "integer": {}, "boolean": {}, "array": {}, "object": {},
}

// ValidateModelMetaInput 校验模型档案的可写字段。
//
// 只校验「提交了的」字段：nil 指针 / nil 或空 jsonb 切片表示未提交（Create 时
// 取列默认值、Update 时保留原值），不会被当作非法值。调用方必须把实际要写库
// 的默认值（"[]"/"{}"）先填入再调用，保证校验看到的与落库的一致。
//
// 规则（L-3）：
//   - description ≤512 字符；display_name ≤64 字符（空允许）；
//   - tags：≤10 项、每项 trim 后 1–32 字符、大小写敏感去重、非字符串拒绝；
//   - endpoints：仅对象形态，键以 "/" 开头，值为非空数组且元素 ∈
//     GET/POST/PUT/PATCH/DELETE（大小写不敏感）；
//   - capabilities / modalities / parameters[].type 为固定枚举；
//   - parameters 元素必须是对象且 name 非空；
//   - knowledge_cutoff 为 YYYY-MM 且月份 01–12（DB 正则之外的早期反馈）；
//   - context_length / max_output 非负。
func ValidateModelMetaInput(m *ModelMeta) error {
	if m == nil {
		return nil
	}
	if err := validateRuneLength("description", m.Description, maxModelMetaDescriptionRunes); err != nil {
		return err
	}
	if err := validateRuneLength("display_name", m.DisplayName, maxModelMetaDisplayNameRunes); err != nil {
		return err
	}
	if err := validateModelMetaTags(m.Tags); err != nil {
		return err
	}
	if err := validateModelMetaEndpoints(m.Endpoints); err != nil {
		return err
	}
	if err := validateEnumArray("capabilities", m.Capabilities, modelMetaCapabilities); err != nil {
		return err
	}
	if err := validateEnumArray("modalities", m.Modalities, modelMetaModalities); err != nil {
		return err
	}
	if err := validateModelMetaParameters(m.Parameters); err != nil {
		return err
	}
	if m.KnowledgeCutoff != nil {
		if !validKnowledgeCutoff(*m.KnowledgeCutoff) {
			return ErrModelMetaInvalid{
				Field:  "knowledge_cutoff",
				Reason: `must be "YYYY-MM" with month 01-12`,
			}
		}
	}
	if m.ContextLength != nil && *m.ContextLength < 0 {
		return ErrModelMetaInvalid{Field: "context_length", Reason: "must be >= 0"}
	}
	if m.MaxOutput != nil && *m.MaxOutput < 0 {
		return ErrModelMetaInvalid{Field: "max_output", Reason: "must be >= 0"}
	}
	return nil
}

func validateRuneLength(field string, value *string, max int) error {
	if value == nil {
		return nil
	}
	if utf8.RuneCountInString(*value) > max {
		return ErrModelMetaInvalid{
			Field:  field,
			Reason: fmt.Sprintf("must be at most %d characters", max),
		}
	}
	return nil
}

// decodeJSONArray 把 jsonb 字节解成 []any，并区分「不是数组」（含字面量 null）。
func decodeJSONArray(field string, raw []byte) ([]any, error) {
	var items []any
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, ErrModelMetaInvalid{Field: field, Reason: "must be a JSON array"}
	}
	return items, nil
}

func validateModelMetaTags(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	items, err := decodeJSONArray("tags", raw)
	if err != nil {
		return err
	}
	if len(items) > maxModelMetaTags {
		return ErrModelMetaInvalid{
			Field:  "tags",
			Reason: fmt.Sprintf("must contain at most %d tags", maxModelMetaTags),
		}
	}
	seen := make(map[string]struct{}, len(items))
	for i, item := range items {
		tag, ok := item.(string)
		if !ok {
			return ErrModelMetaInvalid{
				Field:  "tags",
				Reason: fmt.Sprintf("element %d is not a string", i),
			}
		}
		tag = strings.TrimSpace(tag)
		if tag == "" || utf8.RuneCountInString(tag) > maxModelMetaTagRunes {
			return ErrModelMetaInvalid{
				Field:  "tags",
				Reason: fmt.Sprintf("element %d must be 1-%d characters after trimming", i, maxModelMetaTagRunes),
			}
		}
		// 「去重」在这里取拒绝语义：校验函数只返回 error、不返回规整后的值，
		// 静默去重需要调用方再实现一次同样的逻辑，等于把规则劈成两半。需要
		// 合并重复标签的调用方应在提交前自行去重。
		if _, dup := seen[tag]; dup {
			return ErrModelMetaInvalid{
				Field:  "tags",
				Reason: fmt.Sprintf("duplicate tag %q", tag),
			}
		}
		seen[tag] = struct{}{}
	}
	return nil
}

func validateModelMetaEndpoints(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var endpoints map[string]any
	if err := json.Unmarshal(raw, &endpoints); err != nil || endpoints == nil {
		return ErrModelMetaInvalid{Field: "endpoints", Reason: "must be a JSON object"}
	}
	// 排序后再遍历：多个非法键时错误信息稳定，测试与日志可复现。
	paths := make([]string, 0, len(endpoints))
	for path := range endpoints {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if !strings.HasPrefix(path, "/") {
			return ErrModelMetaInvalid{
				Field:  "endpoints",
				Reason: fmt.Sprintf("path %q must start with /", path),
			}
		}
		methods, ok := endpoints[path].([]any)
		if !ok || len(methods) == 0 {
			return ErrModelMetaInvalid{
				Field:  "endpoints",
				Reason: fmt.Sprintf("methods of %q must be a non-empty array", path),
			}
		}
		for i, item := range methods {
			method, ok := item.(string)
			if !ok {
				return ErrModelMetaInvalid{
					Field:  "endpoints",
					Reason: fmt.Sprintf("method %d of %q is not a string", i, path),
				}
			}
			if _, valid := modelMetaEndpointMethods[strings.ToUpper(method)]; !valid {
				return ErrModelMetaInvalid{
					Field:  "endpoints",
					Reason: fmt.Sprintf("method %q of %q must be one of GET/POST/PUT/PATCH/DELETE", method, path),
				}
			}
		}
	}
	return nil
}

// validateEnumArray 校验「字符串数组且元素属于固定枚举」的 jsonb 字段。
func validateEnumArray(field string, raw []byte, allowed map[string]struct{}) error {
	if len(raw) == 0 {
		return nil
	}
	items, err := decodeJSONArray(field, raw)
	if err != nil {
		return err
	}
	for i, item := range items {
		value, ok := item.(string)
		if !ok {
			return ErrModelMetaInvalid{
				Field:  field,
				Reason: fmt.Sprintf("element %d is not a string", i),
			}
		}
		if _, valid := allowed[value]; !valid {
			return ErrModelMetaInvalid{
				Field:  field,
				Reason: fmt.Sprintf("element %d has unsupported value %q", i, value),
			}
		}
	}
	return nil
}

func validateModelMetaParameters(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	items, err := decodeJSONArray("parameters", raw)
	if err != nil {
		return err
	}
	for i, item := range items {
		param, ok := item.(map[string]any)
		if !ok {
			return ErrModelMetaInvalid{
				Field:  "parameters",
				Reason: fmt.Sprintf("element %d is not an object", i),
			}
		}
		name, ok := param["name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			return ErrModelMetaInvalid{
				Field:  "parameters",
				Reason: fmt.Sprintf("element %d must have a non-empty name", i),
			}
		}
		typ, ok := param["type"].(string)
		if !ok {
			return ErrModelMetaInvalid{
				Field:  "parameters",
				Reason: fmt.Sprintf("element %d must have a string type", i),
			}
		}
		if _, valid := modelMetaParameterTypes[typ]; !valid {
			return ErrModelMetaInvalid{
				Field:  "parameters",
				Reason: fmt.Sprintf("element %d has unsupported type %q", i, typ),
			}
		}
	}
	return nil
}

// validKnowledgeCutoff 校验 YYYY-MM 且月份 01–12（与 model_meta_knowledge_cutoff_chk
// 同口径：四位年 + 两位月）。
func validKnowledgeCutoff(value string) bool {
	if len(value) != 7 || value[4] != '-' {
		return false
	}
	for i, r := range value {
		if i == 4 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	month := (value[5]-'0')*10 + (value[6] - '0')
	return month >= 1 && month <= 12
}
