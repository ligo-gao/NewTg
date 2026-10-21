// Package sensitive 实现 modules-08 F-08-18（`sec.sensitive.*`）的敏感词过滤。
//
// 设计权威：docs/modules-08-系统管理与设置-子功能设计.md §3.5 F-08-18：
//   - `sec.sensitive.enabled` bool，默认 false；
//   - `sec.sensitive.scope` jsonb `{prompt, system}`，默认 `{prompt:true, system:false}`；
//   - `sec.sensitive.words` string[]（≤5000 词、单词 ≤64 字符），支持 `*` 通配【假设】；
//   - `sec.sensitive.action` enum(block, log)，默认 block。
//
// 本包只负责「读取设置 + 文本匹配」。拒绝/放行的 HTTP 语义与**计费时序**
// 由调用点负责：命中拒绝必须发生在 `PreConsume` 与任何上游调用之前
// （F-08-18 主流程第 2 条；任务书 W9-sensitive 第 3 条）。
//
// 设计没有定义词边界，因此匹配采用子串语义（Contains）；`*` 按 glob 处理。
// 大小写与全半角归一化来自 TC-F-08-18-02（设计标注【假设】）。
package sensitive

import (
	"encoding/json"
	"hash/fnv"
	"strings"
	"sync"

	"newtg/gateway/internal/contract"
)

// 设置键名与注册表（backend/internal/settings/registry.go）逐字一致；
// 键名不得改写（modules-08 §3.5.2 冻结）。
const (
	keyEnabled = "sec.sensitive.enabled"
	keyScope   = "sec.sensitive.scope"
	keyWords   = "sec.sensitive.words"
	keyAction  = "sec.sensitive.action"
)

// Reader 是 configcenter.Center.Get 的最小读取面（与 httpapi.ConfigReader 同形）。
// 只读当前快照，不负责加载/广播。
type Reader interface {
	Get(key string) (json.RawMessage, bool)
}

// Surface 是设计里的检测面：prompt（用户提示词）/ system（系统提示词）。
type Surface string

const (
	SurfacePrompt Surface = "prompt"
	SurfaceSystem Surface = "system"
)

// Action 是命中后的处理方式。设计只登记 block|log；没有 replace 取值，
// 因此本包不实现掩码替换（待裁决项见 W9-sensitive 报告）。
type Action string

const (
	ActionBlock Action = "block"
	ActionLog   Action = "log"
)

// Scope 对应 `sec.sensitive.scope`；只有被勾选的面才会被扫描。
type Scope struct {
	Prompt bool
	System bool
}

// Config 是一次设置快照的不可变视图。words 已归一化/去重，构造后不得修改。
type Config struct {
	Enabled bool
	Scope   Scope
	Action  Action
	words   []string
}

// Result 是一次请求级匹配结果。Surface 仅用于日志（不落任何命中词）。
type Result struct {
	Matched bool
	Surface Surface
}

// Active 表示过滤器当前是否可能命中：关闭、词表为空、两个面都未勾选
// 时都视为不启用（modules-08 §表：「词表为空且开关开启 → 不拦截任何内容」）。
func (c Config) Active() bool {
	return c.Enabled && len(c.words) > 0 && (c.Scope.Prompt || c.Scope.System)
}

// CheckRequest 扫描一次 IR 请求中的**用户可控文本**。
//
// 只扫描 PartText；图片/音频/工具调用参数（JSON）与工具结果不进入匹配，
// 避免把模型名、代码/JSON 结构误判为命中（任务书第 2 条）。
// assistant 消息是模型产出，不属于 prompt/system 面，恒不扫描。
func (c Config) CheckRequest(req *contract.Request) Result {
	if !c.Active() || req == nil {
		return Result{}
	}
	for _, msg := range req.Messages {
		surface, ok := c.surfaceForRole(msg.Role)
		if !ok {
			continue
		}
		for _, part := range msg.Content {
			if part.Kind != contract.PartText || part.Text == "" || isReasoningPart(part) {
				continue
			}
			if c.matches(part.Text) {
				return Result{Matched: true, Surface: surface}
			}
		}
	}
	return Result{}
}

// CheckText 扫描一段独立文本（公告 title/body 等非 IR 文本）。
//
// 调用方必须显式给出作用面：公告正文是用户可见内容，调用点按 prompt 面处理
// （设计没有为公告单列作用面，见 W9-sensitive 报告的待裁决项）。
func (c Config) CheckText(surface Surface, text string) bool {
	if !c.Active() || text == "" {
		return false
	}
	switch surface {
	case SurfacePrompt:
		if !c.Scope.Prompt {
			return false
		}
	case SurfaceSystem:
		if !c.Scope.System {
			return false
		}
	default:
		return false
	}
	return c.matches(text)
}

// surfaceForRole 把 IR 角色映射到设计的两条作用面。
//
// developer 是 OpenAI 的系统指令角色（与 system 同义），并入 system 面；
// tool/assistant 内容不是用户提示词，保持原样不扫描。
func (c Config) surfaceForRole(role contract.Role) (Surface, bool) {
	switch role {
	case contract.RoleUser:
		if c.Scope.Prompt {
			return SurfacePrompt, true
		}
	case contract.RoleSystem, contract.RoleDeveloper:
		if c.Scope.System {
			return SurfaceSystem, true
		}
	}
	return "", false
}

// isReasoningPart 跳过模型推理载体（Gemini thought / Anthropic thinking）。
// 这些文本不是用户提示词；扫描它们会让推理内容里的词造成误拦截。
func isReasoningPart(part contract.ContentPart) bool {
	if part.Meta == nil {
		return false
	}
	if thought, ok := part.Meta["thought"].(bool); ok && thought {
		return true
	}
	_, thinking := part.Meta["anthropic_thinking"]
	return thinking
}

// matches 对一段原始文本做一次归一化，然后逐词匹配。
//
// 热路径成本是 O(词数 × 文本长度)：设计上限 5000 词，且只在启用时执行；
// 未命中通配符时使用 strings.Contains，比正则更可预测（设计未定义词边界）。
func (c Config) matches(text string) bool {
	normalized := normalize(text)
	for _, word := range c.words {
		if wildcardContains(normalized, word) {
			return true
		}
	}
	return false
}

// NewConfig 构造已归一化的配置（测试与 Cache 共用）。
//
// 直接构造时未知 action 按 block 处理（fail-closed）；从设置读取时未知
// action 属于坏配置，Cache.Load 会整体回落为不启用。
func NewConfig(enabled bool, scope Scope, words []string, action Action) Config {
	if action != ActionLog {
		action = ActionBlock
	}
	cfg := Config{Enabled: enabled, Scope: scope, Action: action}
	seen := make(map[string]struct{}, len(words))
	for _, raw := range words {
		word := normalize(strings.TrimSpace(raw))
		if word == "" {
			continue
		}
		if _, dup := seen[word]; dup {
			continue
		}
		seen[word] = struct{}{}
		cfg.words = append(cfg.words, word)
	}
	return cfg
}

// Cache 缓存「设置原始值 → Config」的解析结果。
//
// 为什么需要缓存：`sec.sensitive.words` 最多 5000 词，每个中继请求都做一次
// JSON 反序列化会成为热路径成本。缓存键是四个设置原始 JSON 的 FNV 哈希，
// 值变化（配置中心快照替换）时自然失效；关闭态只读 enabled 一个键，不碰词表。
//
// Cache 零值可用，可并发使用。
type Cache struct {
	mu    sync.Mutex
	key   uint64
	valid bool
	cfg   Config
}

// Load 读取当前快照并返回（可能命中缓存的）Config。
//
// 坏配置回落为「不启用」而不是「全词拦截」：任务书要求坏配置不启用，
// 避免一个手改的脏行把全部流量拦死；设置写入侧仍有注册表校验兜底。
func (c *Cache) Load(r Reader) Config {
	if r == nil {
		return Config{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	rawEnabled, hasEnabled := r.Get(keyEnabled)
	hash := fnv.New64a()
	if hasEnabled {
		_, _ = hash.Write(rawEnabled)
	}
	enabled, ok := decodeBool(rawEnabled, hasEnabled)
	if !ok || !enabled {
		// 用一个显式标记区分「关闭态哈希」与「开启态哈希」。
		_, _ = hash.Write([]byte{0})
		return c.remember(hash.Sum64(), Config{})
	}
	_, _ = hash.Write([]byte{1})

	rawScope, hasScope := r.Get(keyScope)
	rawWords, hasWords := r.Get(keyWords)
	rawAction, hasAction := r.Get(keyAction)
	for _, raw := range [][]byte{rawScope, rawWords, rawAction} {
		_, _ = hash.Write(raw)
	}
	sum := hash.Sum64()
	if c.valid && c.key == sum {
		return c.cfg
	}
	cfg, ok := parse(rawScope, hasScope, rawWords, hasWords, rawAction, hasAction)
	if !ok {
		cfg = Config{}
	}
	return c.remember(sum, cfg)
}

func (c *Cache) remember(key uint64, cfg Config) Config {
	c.key, c.valid, c.cfg = key, true, cfg
	return cfg
}

// parse 把开启态的四键解析为 Config；任一键形状非法 → ok=false。
func parse(scopeRaw json.RawMessage, hasScope bool, wordsRaw json.RawMessage, hasWords bool,
	actionRaw json.RawMessage, hasAction bool) (Config, bool) {
	scope, ok := decodeScope(scopeRaw, hasScope)
	if !ok {
		return Config{}, false
	}
	words, ok := decodeWords(wordsRaw, hasWords)
	if !ok {
		return Config{}, false
	}
	action, ok := decodeAction(actionRaw, hasAction)
	if !ok {
		return Config{}, false
	}
	return NewConfig(true, scope, words, action), true
}

func decodeBool(raw json.RawMessage, present bool) (bool, bool) {
	if !present {
		return false, true // 缺省 = 注册表默认 false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, false
	}
	return value, true
}

// decodeScope 解析作用面对象。缺省键沿用注册表默认 {prompt:true, system:false}；
// 未登记字段或非 bool 值视为坏配置。
func decodeScope(raw json.RawMessage, present bool) (Scope, bool) {
	scope := Scope{Prompt: true, System: false}
	if !present || len(raw) == 0 || string(raw) == "null" {
		return scope, true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Scope{}, false
	}
	for key, value := range obj {
		var enabled bool
		if err := json.Unmarshal(value, &enabled); err != nil {
			return Scope{}, false
		}
		switch key {
		case "prompt":
			scope.Prompt = enabled
		case "system":
			scope.System = enabled
		default:
			return Scope{}, false
		}
	}
	return scope, true
}

// decodeWords 兼容三种历史/规范形态：规范 string[]、通用 []any、旧字符串
// （textarea 一行一词）。旧形态按换行切分并保留词内空格。
func decodeWords(raw json.RawMessage, present bool) ([]string, bool) {
	if !present || len(raw) == 0 || string(raw) == "null" {
		return nil, true
	}
	var direct []string
	if err := json.Unmarshal(raw, &direct); err == nil {
		return direct, true
	}
	var generic []any
	if err := json.Unmarshal(raw, &generic); err == nil {
		words := make([]string, 0, len(generic))
		for _, item := range generic {
			word, ok := item.(string)
			if !ok {
				return nil, false
			}
			words = append(words, word)
		}
		return words, true
	}
	var legacy string
	if err := json.Unmarshal(raw, &legacy); err == nil {
		return strings.FieldsFunc(legacy, func(r rune) bool { return r == '\n' || r == '\r' }), true
	}
	return nil, false
}

func decodeAction(raw json.RawMessage, present bool) (Action, bool) {
	if !present {
		return ActionBlock, true // 注册表默认 block
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	switch Action(value) {
	case ActionBlock, ActionLog:
		return Action(value), true
	}
	return "", false
}

// normalize 做大小写与全半角归一化（TC-F-08-18-02【假设】）。
//
// 先做全角 ASCII（U+FF01–U+FF5E）与表意空格到半角的折叠，再 ToLower：
// 这样全角词与半角输入、大小写变体都能命中同一词条。Unicode 大小写使用
// strings.ToLower（简单折叠），不追求 locale 特例。
func normalize(text string) string {
	var folded strings.Builder
	folded.Grow(len(text))
	for _, r := range text {
		switch {
		case r == '　':
			r = ' '
		case r >= '！' && r <= '～':
			r -= 0xFEE0
		}
		folded.WriteRune(r)
	}
	return strings.ToLower(folded.String())
}

// wildcardContains 实现设计里的 `*` 通配（glob 语义：任意长度任意字符）。
//
// 无通配符时走 strings.Contains（设计未定义词边界）；有通配符时按 `*`
// 切段顺序查找，首段可从任意位置开始、末段之后可有多余文本。
// 归一化已把全角 `＊` 折叠为 `*`，因此两种写法等价。
func wildcardContains(text, pattern string) bool {
	if !strings.Contains(pattern, "*") {
		return strings.Contains(text, pattern)
	}
	parts := strings.Split(pattern, "*")
	pos := 0
	if parts[0] != "" {
		index := strings.Index(text, parts[0])
		if index < 0 {
			return false
		}
		pos = index + len(parts[0])
	}
	for _, part := range parts[1:] {
		if part == "" {
			continue
		}
		index := strings.Index(text[pos:], part)
		if index < 0 {
			return false
		}
		pos += index + len(part)
	}
	return true
}
