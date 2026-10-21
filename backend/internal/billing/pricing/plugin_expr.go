package pricing

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/shopspring/decimal"

	"newtg/gateway/internal/billing/expr"
	"newtg/gateway/internal/contract"
)

// 本文件是 T5 的任务计价表达式面（billing-02 §2.6、billing-05 §3.1/§3.5、
// billing-00 §3.1）。
//
// 【与模型表达式的关系】任务表达式的解析顺序与模型级不同（billing-02
// §2.6）：插件键优先，模型级表达式只作为回退；插件覆盖总是任务用量语义
// （`u("字段名")`），与模型的 billing_mode 无关。模型表达式若引用 token
// 变量（p/c/...），它就不是任务表达式，回退时跳过而不是报错（见
// taskExprOrNil）——否则所有按 tiered token 表达式定价的模型都会让成功
// 任务卡在 settlement_pending。
//
// 【表达式能力边界】本仓库的表达式引擎（billing/expr）只实现算术子集；
// 任务表达式需要的 `u("key")` 用词法改写承载：`u("seconds")` → 绑定变量
// `__task_u_N`，求值时从 TaskUsageFact.Facts 取值。这样不改动共享求值器
// 的白名单（它仍然只认识数字/标识符/四则/min/max/ceil/floor/round），
// 也不引入第二个表达式实现。tier()/fixed()/比较/三元等完整语言特性尚
// 未实现：出现时编译失败 → 结算保留预扣（fail-closed），不会静默按错误
// 金额结算。

const (
	// PluginBillingExprSettingKey 是插件任务计价表达式的设置键。
	//
	// 命名空间 bucketing 依 R-06：仓库原本没有 `billing_setting` 命名空间，
	// 本任务新建该 section 并把键挂在其下（不是平铺顶层键）。settings 包
	// 用同名字面量注册；两处必须保持一致。
	PluginBillingExprSettingKey = "billing_setting.plugin_billing_expr"

	// taskExprVarPrefix 是改写后 u() 变量的前缀。下划线开头保证不会与
	// 插件的真实字段名/表达式变量冲突。
	taskExprVarPrefix = "__task_u_"
)

// ErrEmptyTaskExpression 表示表达式键存在但内容为空/空白。
//
// R-37 同口径的 fail-closed：绝不回退 ratio/per_call 或插件键的下一级。
// 静默回退会让管理员以为覆盖生效，实际按另一套价格结算。
var ErrEmptyTaskExpression = fmt.Errorf("%w: 任务计价表达式为空，拒绝回退其它价格来源", ErrInvalidSpec)

// ErrTaskUsageMissing 表示表达式引用的用量字段在事实中缺失或不是数值。
//
// 差额不可信时保留预扣（billing-05 §3.2 的乙类怪癖），因此这里必须报错，
// 不能用 0 补齐——0 会把「上游没报用量」变成免费。
var ErrTaskUsageMissing = fmt.Errorf("%w: 任务用量事实缺少数值字段", ErrInvalidSpec)

// ExprSource 标识任务表达式来自解析链的哪一级（落审计/日志用）。
type ExprSource string

const (
	// ExprSourcePluginClient：① plugin_billing_expr["<pluginKey>::<clientModel>"]
	ExprSourcePluginClient ExprSource = "plugin_client"
	// ExprSourcePluginMapped：② plugin_billing_expr["<pluginKey>::<mappedModel>"]
	ExprSourcePluginMapped ExprSource = "plugin_mapped"
	// ExprSourceClientTiered：③ 客户端模型自身的 tiered 表达式
	ExprSourceClientTiered ExprSource = "client_tiered"
	// ExprSourceMappedExpr：④ 映射模型自身的 tiered 表达式
	ExprSourceMappedExpr ExprSource = "mapped_expr"
	// ExprSourceHeldSkip：模型配置了 tiered 表达式，但它不是任务表达式
	// （不引用 u()）。这是**显式跳过**而不是「未配置」：调用方必须在
	// private_data 落标记，避免与「根本没有表达式」混淆（复核 I2）。
	ExprSourceHeldSkip ExprSource = "held_skip"
)

// TaskExprResolution 是 ResolveTaskExpr 的详细结果（T5 复核 I2）。
//
// 为什么不把跳过原因塞进 error：跳过是**正常回落**（模型表达式是 token
// 语义），调用方要按 held 结算并在任务行留下可审计的标记；而含 u() 却编译
// 失败是配置错误，必须从 error 返回并在提交期 fail-closed。
type TaskExprResolution struct {
	Expr   *Expr
	Source ExprSource
	// SkipReason 非空表示遇到并跳过了模型级表达式（Expr 为 nil）。
	SkipModel  string
	SkipReason string
}

// BillingSetting 是任务表达式解析所需的配置切片。
//
// 与 billing-02 §2.1 的 BillingSetting 同形（三个 map），但本仓库的模型
// 级表达式权威在 model_pricing 表，调用方按需填入 BillingMode/BillingExpr。
type BillingSetting struct {
	// PluginBillingExpr：键 "<pluginKey>::<model>" → 任务用量表达式。
	PluginBillingExpr map[string]string
	// BillingMode：模型 → "tiered_expr"（其它值/缺键 = 非表达式模式）。
	BillingMode map[string]string
	// BillingExpr：模型 → 表达式源码。
	BillingExpr map[string]string
}

// BillingSettingFromConfig 从配置中心快照读取插件覆盖表。
//
// 畸形 JSON/非字符串值一律忽略（返回空表）而不是让任务提交失败：这个键
// 由设置保存路径校验，运行期快照损坏时保守方向是「按既有 held 结算」，
// 不能把全部任务提交变成 500。空值条目保留在表里，由 ResolveTaskExpr
// 按 R-37 fail-closed 拒绝。
func BillingSettingFromConfig(cfg ConfigReader) BillingSetting {
	out := BillingSetting{PluginBillingExpr: map[string]string{}}
	if cfg == nil {
		return out
	}
	raw, ok := cfg.Get(PluginBillingExprSettingKey)
	if !ok || len(raw) == 0 {
		return out
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return out
	}
	for k, v := range obj {
		if s, ok := v.(string); ok {
			out.PluginBillingExpr[k] = s
		}
	}
	return out
}

// Expr 是编译后的任务计价表达式。
//
// 【并发】与 expr.Evaluator 相同：编译后只读，可被并发求值。
type Expr struct {
	// Source 是原始表达式源码（冻结进任务快照的正是它，不是改写后的文本）。
	Source string
	// groupRatio 是提交期冻结的分组倍率（模型侧快照同源）。
	groupRatio float64
	// quotaPerUnit 是提交期冻结的 QuotaPerUnit（复核 M1）。
	//
	// 站点兑换比是热更配置；若结算期才读当前值，提交与结算之间的一次
	// 改动就会让同一任务的金额前后不一致。快照冻结它、结算只读快照。
	quotaPerUnit int64
	// ev 是改写后编译得到的求值器。
	ev *expr.Evaluator
	// keyByVar：改写变量 → 任务用量字段名。
	keyByVar map[string]string
}

// WithGroupRatio 返回带分组倍率的副本。
//
// 冻结接口 EvalTaskUsage 不接收分组倍率；提交期把模型侧快照的 GroupRatio
// 放进 Expr，结算期只读该值（P-08：不重读配置）。
func (e *Expr) WithGroupRatio(r float64) *Expr {
	if e == nil {
		return nil
	}
	copied := *e
	copied.groupRatio = r
	return &copied
}

// WithQuotaPerUnit 返回带冻结兑换比的副本（复核 M1）。
//
// q <= 0 表示未冻结（旧快照），EvalTaskUsage 回落到进程当前值。
func (e *Expr) WithQuotaPerUnit(q int64) *Expr {
	if e == nil {
		return nil
	}
	copied := *e
	copied.quotaPerUnit = q
	return &copied
}

// UsageKeys 返回表达式引用的任务用量字段名（去重、稳定顺序）。
//
// 提交期用它把「字段 → 单位」冻结进任务快照（复核 I1）：结算不再依赖
// 活插件的 usageSchema，插件改名/下架不会让已提交任务求值失败。
func (e *Expr) UsageKeys() []string {
	if e == nil {
		return nil
	}
	seen := make(map[string]bool, len(e.keyByVar))
	out := make([]string, 0, len(e.keyByVar))
	for _, key := range e.keyByVar {
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// CompileTaskExpr 编译一条任务用量表达式。
//
// 空串/空白 → ErrEmptyTaskExpression（fail-closed）。改写只识别字面量调用
// `u("字段名")`；动态参数（u(name)）原样保留，随后因未知标识符被拒绝。
func CompileTaskExpr(src string) (*Expr, error) {
	trimmed := strings.TrimSpace(src)
	if trimmed == "" {
		return nil, ErrEmptyTaskExpression
	}
	rewritten, keyByVar, err := rewriteTaskUsage(trimmed)
	if err != nil {
		return nil, err
	}
	ev, err := expr.Compile(rewritten)
	if err != nil {
		return nil, fmt.Errorf("%w: 编译任务计价表达式: %v", ErrInvalidSpec, err)
	}
	// 白名单：本子集只允许 u() 变量与数学函数。token 变量（p/c/...）、
	// tier/fixed/时间函数都不属于任务表达式，出现即为配置错误——在编译期
	// 拒绝而不是等到结算期，让保存/解析路径尽早暴露。
	for name := range ev.UsedVars() {
		if _, ok := keyByVar[name]; ok {
			continue
		}
		switch name {
		case "min", "max", "ceil", "floor", "round":
			continue
		}
		return nil, fmt.Errorf("%w: 任务表达式引用了不支持的标识符 %q（仅支持 u(\"字段\") 与数学函数）",
			ErrInvalidSpec, name)
	}
	return &Expr{Source: trimmed, ev: ev, keyByVar: keyByVar}, nil
}

// ResolveTaskExpr 按 billing-02 §2.6 的四级顺序解析任务表达式。
//
// 顺序不可调换（billing-05 §3.1）：
//  1. plugin_billing_expr["<pluginKey>::<clientModel>"]
//  2. plugin_billing_expr["<pluginKey>::<mappedModel>"]（mapped ≠ client 时）
//  3. 客户端模型的 tiered 表达式
//  4. 映射模型的 tiered 表达式（客户端模型无表达式模式时）
//
// mappedModel == clientModel 时第 2/4 级不重复求值（P-06）。
// 返回 (nil, "", nil) 表示「未配置任务表达式」，调用方沿用按预扣额结清。
// 需要区分「跳过模型级表达式」时用 ResolveTaskExprDetailed。
func ResolveTaskExpr(cfg BillingSetting, pluginKey, clientModel, mappedModel string) (*Expr, ExprSource, error) {
	res, err := ResolveTaskExprDetailed(cfg, pluginKey, clientModel, mappedModel)
	if err != nil {
		return nil, "", err
	}
	return res.Expr, res.Source, nil
}

// ResolveTaskExprDetailed 是带跳过原因的四级解析（T5 复核 I2）。
//
// 失败口径（复核 I2 之后）：
//   - 插件键存在但空白/不可编译 → error（提交期 400 + 全额退款）；
//   - 模型级表达式**引用 u(** 却不可编译 → error（显式配置了任务覆盖却
//     写坏，静默回落 held 会把「写错」变成免费/多收，必须拒绝）；
//   - 模型级表达式不引用 u( → 不是任务表达式，跳过并在结果里记录原因，
//     由调用方按 held 结算并把 source:"held_skip" 落 private_data。
func ResolveTaskExprDetailed(cfg BillingSetting, pluginKey, clientModel, mappedModel string) (TaskExprResolution, error) {
	pluginKey = strings.TrimSpace(pluginKey)
	clientModel = strings.TrimSpace(clientModel)
	mappedModel = strings.TrimSpace(mappedModel)
	if pluginKey == "" || clientModel == "" {
		return TaskExprResolution{}, fmt.Errorf("%w: 任务表达式解析需要 pluginKey 与 clientModel", ErrInvalidSpec)
	}

	// ① 插件键（客户端模型）。键存在即命中；空值是配置错误，不回退。
	if src, ok := cfg.PluginBillingExpr[pluginKey+"::"+clientModel]; ok {
		e, err := CompileTaskExpr(src)
		if err != nil {
			return TaskExprResolution{}, err
		}
		return TaskExprResolution{Expr: e, Source: ExprSourcePluginClient}, nil
	}

	// ② 插件键（最终映射模型）。与 ① 只有模型名不同，映射相同视为同一级。
	if mappedModel != "" && mappedModel != clientModel {
		if src, ok := cfg.PluginBillingExpr[pluginKey+"::"+mappedModel]; ok {
			e, err := CompileTaskExpr(src)
			if err != nil {
				return TaskExprResolution{}, err
			}
			return TaskExprResolution{Expr: e, Source: ExprSourcePluginMapped}, nil
		}
	}

	var skipped TaskExprResolution

	// ③ 客户端模型的 tiered 表达式。
	if e, skipReason, err := taskExprOrNil(cfg, clientModel); err != nil {
		return TaskExprResolution{}, err
	} else if e != nil {
		return TaskExprResolution{Expr: e, Source: ExprSourceClientTiered}, nil
	} else if skipReason != "" {
		skipped = TaskExprResolution{
			Source: ExprSourceHeldSkip, SkipModel: clientModel, SkipReason: skipReason,
		}
	}

	// ④ 映射模型的 tiered 表达式。
	if mappedModel != "" && mappedModel != clientModel {
		if e, skipReason, err := taskExprOrNil(cfg, mappedModel); err != nil {
			return TaskExprResolution{}, err
		} else if e != nil {
			return TaskExprResolution{Expr: e, Source: ExprSourceMappedExpr}, nil
		} else if skipReason != "" && skipped.SkipReason == "" {
			skipped = TaskExprResolution{
				Source: ExprSourceHeldSkip, SkipModel: mappedModel, SkipReason: skipReason,
			}
		}
	}
	return skipped, nil
}

// taskExprOrNil 在模型配置了 tiered 表达式时编译它。
//
// 模型级表达式可能是 token 语义（p/c），不能当任务表达式用。区分两种失败：
//   - 源码不含 u( → 不是任务表达式，返回 skipReason（正常回落）；
//   - 源码含 u( 但编译失败 → 返回 error（配置错误，提交期 fail-closed）。
//
// 为什么含 u( 不能静默跳过：那是管理员显式写的任务覆盖，写坏后回落 held
// 会让金额与配置意图不一致，且没有任何可审计的“降级”信号。
func taskExprOrNil(cfg BillingSetting, model string) (*Expr, string, error) {
	if !isTieredModel(cfg, model) {
		return nil, "", nil
	}
	src := cfg.BillingExpr[model]
	e, err := CompileTaskExpr(src)
	if err != nil {
		if ContainsTaskUsageCall(src) {
			return nil, "", fmt.Errorf("%w: 模型 %q 的任务表达式不可编译: %v", ErrInvalidSpec, model, err)
		}
		return nil, "tiered expression does not reference u() task usage", nil
	}
	return e, "", nil
}

// ContainsTaskUsageCall 报告表达式中是否出现任务用量调用 u(。
//
// 只用于「编译失败后判断这是不是任务表达式」，不做语法分析：`u (` 也
// 算命中（空格允许），`func_u(`/`units(` 不算（前一个字符是标识符字符）。
func ContainsTaskUsageCall(src string) bool {
	for i := 0; i < len(src); i++ {
		if src[i] != 'u' {
			continue
		}
		if i > 0 && isIdentPartByte(src[i-1]) {
			continue
		}
		k := i + 1
		for k < len(src) && isSpaceByte(src[k]) {
			k++
		}
		if k < len(src) && src[k] == '(' {
			return true
		}
	}
	return false
}

func isTieredModel(cfg BillingSetting, model string) bool {
	switch strings.ToLower(strings.TrimSpace(cfg.BillingMode[model])) {
	case "tiered_expr", "expression", "expr":
		return strings.TrimSpace(cfg.BillingExpr[model]) != ""
	default:
		return false
	}
}

// EvalTaskUsage 求值任务表达式并换算为整数积分。
//
// 公式（billing-05 §3.4 第 5 条）：`额度 = 表达式 USD × QuotaPerUnit × groupRatio`
// ——任务用量表达式**不除 1e6**（那是 token 表达式 `$/1M tokens` 的口径）。
//
// 失败语义（billing-05 §3.2，乙类怪癖照搬）：字段缺失/非数值、负结果、
// 钳制统统返回 error，由调用方保留预扣并置 settlement_pending；绝不把
// 「差额不可信」结算成全额退款或 0。
func EvalTaskUsage(e *Expr, fact contract.TaskUsageFact) (int64, error) {
	if e == nil || e.ev == nil {
		return 0, ErrNoExpression
	}
	vars := make(map[string]float64, len(e.keyByVar))
	for name, key := range e.keyByVar {
		value, ok := fact.Facts[key]
		if !ok || value.Num == nil {
			return 0, fmt.Errorf("%w: %q", ErrTaskUsageMissing, key)
		}
		vars[name] = *value.Num
	}
	out, err := e.ev.Eval(vars)
	if err != nil {
		return 0, fmt.Errorf("%w: 求值任务表达式: %v", ErrInvalidSpec, err)
	}
	if out < 0 {
		// 负成本会成为负配额（抵扣），属配置错误；不得静默按负额入账。
		return 0, fmt.Errorf("%w: %v", ErrNegativeExpression, out)
	}

	group := e.groupRatio
	if group == 0 {
		// 未冻结分组倍率（旧快照/单测）按 1，与模型侧快照缺省口径一致。
		group = 1
	}
	// M1：优先用提交期冻结的兑换比；旧快照/单测回落到进程当前值。
	qpu := e.quotaPerUnit
	if qpu <= 0 {
		qpu = CurrentQuotaPerUnit()
	}
	total := dec(out).Mul(decimal.NewFromInt(qpu)).Mul(dec(group))
	quota, clamp := QuotaFromDecimalChecked(total)
	if clamp != nil {
		return quota, &ClampError{Clamp: clamp}
	}
	if quota == 0 && out > 0 {
		// 与 computeExpressionQuota 一致：正价格被取整为 0 → 最小计费 1。
		return 1, nil
	}
	return quota, nil
}

// rewriteTaskUsage 把字面量 u("字段名") 改写成绑定变量。
//
// 为什么用词法改写而不是扩展共享求值器：expr 包的白名单是「不可信输入的
// 最小语法面」，加入字符串字面量与函数实参类型会扩大所有计费路径的攻击面。
// 这里只识别一种固定形状（u + ( + 字符串 + )），其余原样交给编译器拒绝。
func rewriteTaskUsage(src string) (string, map[string]string, error) {
	var b strings.Builder
	b.Grow(len(src))
	keyByVar := map[string]string{}
	// 记录改写位置：动态/畸形 u(...) 不吞入，交由后续编译报错。
	for i := 0; i < len(src); {
		if !isIdentStartByte(src[i]) {
			b.WriteByte(src[i])
			i++
			continue
		}
		start := i
		for i < len(src) && isIdentPartByte(src[i]) {
			i++
		}
		ident := src[start:i]
		if ident != "u" {
			b.WriteString(ident)
			continue
		}
		k := i
		for k < len(src) && isSpaceByte(src[k]) {
			k++
		}
		if k >= len(src) || src[k] != '(' {
			b.WriteString(ident)
			continue
		}
		k++
		for k < len(src) && isSpaceByte(src[k]) {
			k++
		}
		if k >= len(src) || (src[k] != '"' && src[k] != '\'') {
			b.WriteString(ident)
			continue
		}
		quote := src[k]
		k++
		keyStart := k
		for k < len(src) && src[k] != quote {
			k++
		}
		if k >= len(src) {
			return "", nil, fmt.Errorf("%w: u() 的字符串字面量未闭合", ErrInvalidSpec)
		}
		key := strings.TrimSpace(src[keyStart:k])
		k++ // 闭合引号
		for k < len(src) && isSpaceByte(src[k]) {
			k++
		}
		if k >= len(src) || src[k] != ')' {
			b.WriteString(ident)
			continue
		}
		if key == "" {
			return "", nil, fmt.Errorf("%w: u(\"\") 的字段名为空", ErrInvalidSpec)
		}
		varName := fmt.Sprintf("%s%d", taskExprVarPrefix, len(keyByVar))
		keyByVar[varName] = key
		b.WriteString(varName)
		i = k + 1
	}
	return b.String(), keyByVar, nil
}

func isIdentStartByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPartByte(c byte) bool {
	return isIdentStartByte(c) || (c >= '0' && c <= '9')
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}
