// Package expr 实现计费引擎的白名单表达式求值器（M3 Task 1）。
//
// 设计依据：
//   - .superpowers/plans/2026-10-18-m3-计费引擎.md Task 1（冻结接口）；
//   - docs/billing-00-计费引擎接口契约.md §4（语言、内省、版本、请求规则、时间函数）；
//   - docs/billing-01-表达式引擎-设计.md（本包只承载其中的算术子集）。
//
// 为什么自研而不引通用表达式库：表达式字符串来自管理端配置，属于不可信
// 输入。通用求值库（模板、反射、eval）会引入任意代码执行与资源耗尽面；
// 本包只接受显式白名单：数字字面量、标识符 [a-zA-Z_][a-zA-Z0-9_]*、
// `+ - * /`、一元负号、括号、逗号，以及 min/max/ceil/floor/round 五个
// 函数。白名单之外的字符（`;` `#` `$` `{` `}` `.` `"` `'` `|` `%` `=`
// `<` `>` `&` 等）在词法阶段即被拒绝 —— 因此不存在语句分隔符、注释、
// 属性访问、模板/注入语法的解析路径。
//
// 规格 §4.5 的 `|||` 不实现：它是前端文档记号，运行时表达式里不存在
// （前端直接输出普通乘法因子）；`|` 不在白名单内，出现即语法错误。
//
// 规格 §4.6 的时间函数（now/hour/minute/weekday/month/day）不在本任务
// 白名单内：它们必须读取冻结的 RequestInput.RequestTime，而 Task 1 冻结的
// `Eval(vars map[string]float64)` 没有该入参。为避免「静默改用 wall clock
// 导致预扣与结算跨小时匹配到不同档位」的基线缺陷，这些名字一律按未知
// 函数在编译期拒绝（EvalUnknownFunction），而不是求值。
package expr

import (
	"fmt"
	"math"
	"strconv"
)

// EvalErrorKind 是表达式错误的语义种类。
//
// 【为什么区分种类而不只回一个 error】计费侧（订单/HTTP 层）要按种类
// 决定处置：语法/未知函数属于配置错误（保存期 400），除零/溢出属于运行
// 期数据问题（保留预扣、记 warn）。错误文案由 API 层映射，本包不硬编码
// 用户文案（billing-01 §6.4）。
type EvalErrorKind int

const (
	// EvalSyntax 表示词法或语法错误、元数（参数个数）错误、空表达式。
	EvalSyntax EvalErrorKind = iota
	// EvalUnknownVariable 表示求值时变量未绑定。
	EvalUnknownVariable
	// EvalDivideByZero 表示除数为 0（含 -0，Go 中 -0 == 0）。
	// 【为什么显式判零】Task 1 冻结契约要求除零返回该种类；若放任 IEEE
	// 754 语义会产生 ±Inf/NaN，需要调用方再做一次判别，且错误无法定位。
	EvalDivideByZero
	// EvalOverflow 表示结果 NaN 或 ±Inf，或数字字面量超出 float64 范围。
	EvalOverflow
	// EvalUnknownFunction 表示调用了白名单之外的函数。
	EvalUnknownFunction
)

// String 返回种类的稳定英文名，供错误文案与日志使用。
func (k EvalErrorKind) String() string {
	switch k {
	case EvalSyntax:
		return "syntax"
	case EvalUnknownVariable:
		return "unknown variable"
	case EvalDivideByZero:
		return "divide by zero"
	case EvalOverflow:
		return "overflow"
	case EvalUnknownFunction:
		return "unknown function"
	default:
		return "unknown"
	}
}

// EvalError 是编译/求值错误。Kind 供调用方分支处理，Message 只用于诊断。
type EvalError struct {
	Kind    EvalErrorKind
	Message string
}

// Error 实现 error。
func (e *EvalError) Error() string {
	if e == nil {
		// 防御性：nil 接收者不应产生二次 panic（错误路径永不再制造错误）。
		return "expr: <nil>"
	}
	if e.Message == "" {
		return "expr: " + e.Kind.String()
	}
	return "expr: " + e.Kind.String() + ": " + e.Message
}

func syntaxf(format string, args ...any) *EvalError {
	return &EvalError{Kind: EvalSyntax, Message: fmt.Sprintf(format, args...)}
}

// ---------------------------------------------------------------------------
// 词法
// ---------------------------------------------------------------------------

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokNumber
	tokIdent
	tokPlus
	tokMinus
	tokStar
	tokSlash
	tokLParen
	tokRParen
	tokComma
)

func (k tokenKind) String() string {
	switch k {
	case tokEOF:
		return "end of expression"
	case tokNumber:
		return "number"
	case tokIdent:
		return "identifier"
	case tokPlus:
		return "+"
	case tokMinus:
		return "-"
	case tokStar:
		return "*"
	case tokSlash:
		return "/"
	case tokLParen:
		return "("
	case tokRParen:
		return ")"
	case tokComma:
		return ","
	default:
		return "token"
	}
}

type token struct {
	kind tokenKind
	text string  // 标识符名或数字原文（诊断用）
	num  float64 // 仅 tokNumber 有效
	pos  int     // 字节偏移，错误定位用
}

type lexer struct {
	src string
	pos int
}

func (l *lexer) peek() byte {
	if l.pos >= len(l.src) {
		return 0
	}
	return l.src[l.pos]
}

func (l *lexer) peekAt(off int) byte {
	p := l.pos + off
	if p >= len(l.src) {
		return 0
	}
	return l.src[p]
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isIdentStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isIdentPart(b byte) bool { return isIdentStart(b) || isDigit(b) }

func isSpace(b byte) bool {
	// 只接受 ASCII 空白。全角/Unicode 空白不在白名单内，出现即报错，
	// 避免「看起来合法、实际被静默忽略」的混淆面。
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

// next 返回下一个 token。白名单外的任何字符都返回 EvalSyntax。
func (l *lexer) next() (token, error) {
	for l.pos < len(l.src) && isSpace(l.src[l.pos]) {
		l.pos++
	}
	if l.pos >= len(l.src) {
		return token{kind: tokEOF, pos: l.pos}, nil
	}

	start := l.pos
	c := l.src[l.pos]

	if isDigit(c) {
		return l.scanNumber()
	}
	if isIdentStart(c) {
		l.pos++
		for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
			l.pos++
		}
		return token{kind: tokIdent, text: l.src[start:l.pos], pos: start}, nil
	}

	l.pos++
	switch c {
	case '+':
		return token{kind: tokPlus, pos: start}, nil
	case '-':
		return token{kind: tokMinus, pos: start}, nil
	case '*':
		return token{kind: tokStar, pos: start}, nil
	case '/':
		return token{kind: tokSlash, pos: start}, nil
	case '(':
		return token{kind: tokLParen, pos: start}, nil
	case ')':
		return token{kind: tokRParen, pos: start}, nil
	case ',':
		return token{kind: tokComma, pos: start}, nil
	default:
		return token{}, syntaxf("unexpected character %q at offset %d", c, start)
	}
}

// scanNumber 读取十进制字面量：整数、小数、科学计数法。
//
// 指数部分只有在「e/E [+-]? 数字」完整成立时才吞入；否则回退到 e 之前，
// 让 e 作为普通标识符继续词法分析，由语法层报「意外 token」—— 这条路径
// 保证 `2e`、`1e+` 这类半截载荷不会绕过检查，也不会产生歧义错误。
func (l *lexer) scanNumber() (token, error) {
	start := l.pos
	for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
		l.pos++
	}
	if l.peek() == '.' {
		l.pos++
		for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			l.pos++
		}
	}
	if l.peek() == 'e' || l.peek() == 'E' {
		save := l.pos
		l.pos++
		if l.peek() == '+' || l.peek() == '-' {
			l.pos++
		}
		if !isDigit(l.peek()) {
			l.pos = save // 回退：指数不完整，不吞入 e
		} else {
			for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
				l.pos++
			}
		}
	}

	text := l.src[start:l.pos]
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		// 形状已由上面的扫描保证，唯一可能的失败是超出 float64 范围
		// （如 1e999 → +Inf + ErrRange）。字面量溢出属于配置错误，
		// 在编译期就拒绝，避免把 Inf 带进求值。
		return token{}, &EvalError{
			Kind:    EvalOverflow,
			Message: fmt.Sprintf("numeric literal %q is out of float64 range", text),
		}
	}
	return token{kind: tokNumber, text: text, num: v, pos: start}, nil
}

// ---------------------------------------------------------------------------
// 语法（递归下降）
// ---------------------------------------------------------------------------
//
// 文法（全部产出 float64）：
//
//	expr   := term (('+' | '-') term)*
//	term   := unary (('*' | '/') unary)*
//	unary  := '-' unary | primary
//	primary:= NUMBER | IDENT | IDENT '(' args ')' | '(' expr ')'
//	args   := expr (',' expr)*
//
// 只实现上述文法：没有比较、逻辑、三元、字符串、属性访问、赋值或任何
// 语句形式。这样每个节点都必然是纯数值运算，注入载荷无处落脚。

type node interface {
	eval(vars map[string]float64) (float64, error)
}

type numberNode struct{ value float64 }

func (n *numberNode) eval(map[string]float64) (float64, error) { return n.value, nil }

type varNode struct{ name string }

func (n *varNode) eval(vars map[string]float64) (float64, error) {
	v, ok := vars[n.name]
	if !ok {
		return 0, &EvalError{
			Kind:    EvalUnknownVariable,
			Message: fmt.Sprintf("variable %q is not bound", n.name),
		}
	}
	return v, nil
}

type unaryNode struct{ operand node }

func (n *unaryNode) eval(vars map[string]float64) (float64, error) {
	v, err := n.operand.eval(vars)
	if err != nil {
		return 0, err
	}
	return finiteResult(-v)
}

type binaryNode struct {
	op          tokenKind // tokPlus / tokMinus / tokStar / tokSlash
	left, right node
}

func (n *binaryNode) eval(vars map[string]float64) (float64, error) {
	l, err := n.left.eval(vars)
	if err != nil {
		return 0, err
	}
	r, err := n.right.eval(vars)
	if err != nil {
		return 0, err
	}

	switch n.op {
	case tokPlus:
		return finiteResult(l + r)
	case tokMinus:
		return finiteResult(l - r)
	case tokStar:
		return finiteResult(l * r)
	case tokSlash:
		if r == 0 {
			// 显式判零（含 -0）：契约要求可分类的 EvalDivideByZero，
			// 而不是让 IEEE 754 的 ±Inf/NaN 泄漏到配额换算层。
			return 0, &EvalError{Kind: EvalDivideByZero, Message: "division by zero"}
		}
		return finiteResult(l / r)
	default:
		// 不可达：parser 只构造以上四种二元节点。保留防御分支，
		// 保证任何未来改动都不会 panic。
		return 0, syntaxf("unsupported binary operator")
	}
}

type callNode struct {
	name string
	args []node
}

func (n *callNode) eval(vars map[string]float64) (float64, error) {
	args := make([]float64, len(n.args))
	for i, arg := range n.args {
		v, err := arg.eval(vars)
		if err != nil {
			return 0, err
		}
		args[i] = v
	}

	var out float64
	switch n.name {
	case "min":
		out = args[0]
		for _, v := range args[1:] {
			out = math.Min(out, v)
		}
	case "max":
		out = args[0]
		for _, v := range args[1:] {
			out = math.Max(out, v)
		}
	case "ceil":
		out = math.Ceil(args[0])
	case "floor":
		out = math.Floor(args[0])
	case "round":
		// math.Round = 四舍五入、.5 远离零（round(2.5)=3、round(-2.5)=-3）。
		// 与计费取整的通行语义一致；这是 Task 1 冻结的实现选择。
		out = math.Round(args[0])
	default:
		// 不可达：parser 已拒绝白名单外函数。
		return 0, &EvalError{Kind: EvalUnknownFunction, Message: n.name}
	}
	return finiteResult(out)
}

// finiteResult 拒绝 NaN 与 ±Inf。
//
// 【为什么每次运算都检查】一旦 Inf/NaN 进入后续运算就会「粘住」或产生
// 更隐蔽的 NaN，既无法定位溢出点，也违反「Eval 绝不 panic、错误可分类」
// 的契约。逐节点检查让溢出在发生处即返回 EvalOverflow。
func finiteResult(v float64) (float64, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, &EvalError{
			Kind:    EvalOverflow,
			Message: fmt.Sprintf("non-finite result (%v)", v),
		}
	}
	return v, nil
}

// parser 是递归下降解析器，同时收集 UsedVars（内省）。
type parser struct {
	lex  lexer
	tok  token
	used map[string]bool
}

func (p *parser) advance() error {
	tok, err := p.lex.next()
	if err != nil {
		return err
	}
	p.tok = tok
	return nil
}

func (p *parser) parseAdditive() (node, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}
	for p.tok.kind == tokPlus || p.tok.kind == tokMinus {
		op := p.tok.kind
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.parseMultiplicative()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: op, left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseMultiplicative() (node, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.tok.kind == tokStar || p.tok.kind == tokSlash {
		op := p.tok.kind
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: op, left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseUnary() (node, error) {
	if p.tok.kind == tokMinus {
		if err := p.advance(); err != nil {
			return nil, err
		}
		operand, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &unaryNode{operand: operand}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (node, error) {
	switch p.tok.kind {
	case tokNumber:
		n := &numberNode{value: p.tok.num}
		return n, p.advance()

	case tokIdent:
		name := p.tok.text
		pos := p.tok.pos
		if err := p.advance(); err != nil {
			return nil, err
		}
		if p.tok.kind == tokLParen {
			return p.parseCall(name, pos)
		}
		// 变量引用：所有标识符都登记，求值时由 vars 决定是否绑定。
		p.used[name] = true
		return &varNode{name: name}, nil

	case tokLParen:
		if err := p.advance(); err != nil {
			return nil, err
		}
		inner, err := p.parseAdditive()
		if err != nil {
			return nil, err
		}
		if p.tok.kind != tokRParen {
			return nil, syntaxf("expected ')' at offset %d, got %s", p.tok.pos, p.tok.kind)
		}
		return inner, p.advance()

	default:
		return nil, syntaxf("unexpected %s at offset %d", p.tok.kind, p.tok.pos)
	}
}

// parseCall 解析函数调用并做白名单/元数检查。
//
// 这里同时是「时间函数冻结范围」的落点：Task 1 的冻结接口没有
// RequestInput.RequestTime 入参，因此 now/hour/minute/weekday/month/day
// 以及 tier/fixed/param/header/has/u/abs 全部按未知函数编译失败，
// 而不是静默求值或静默忽略（billing-00 §4.6、billing-01 §2.2）。
func (p *parser) parseCall(name string, pos int) (node, error) {
	if err := p.advance(); err != nil { // 吃掉 '('
		return nil, err
	}

	var args []node
	if p.tok.kind != tokRParen {
		for {
			arg, err := p.parseAdditive()
			if err != nil {
				return nil, err
			}
			args = append(args, arg)
			if p.tok.kind != tokComma {
				break
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
		}
	}
	if p.tok.kind != tokRParen {
		return nil, syntaxf("expected ')' or ',' at offset %d, got %s", p.tok.pos, p.tok.kind)
	}
	if err := p.advance(); err != nil {
		return nil, err
	}

	switch name {
	case "min", "max":
		if len(args) < 2 {
			return nil, syntaxf("function %q expects at least 2 arguments, got %d", name, len(args))
		}
	case "ceil", "floor", "round":
		if len(args) != 1 {
			return nil, syntaxf("function %q expects exactly 1 argument, got %d", name, len(args))
		}
	default:
		return nil, &EvalError{
			Kind:    EvalUnknownFunction,
			Message: fmt.Sprintf("unknown function %q at offset %d", name, pos),
		}
	}

	// 内省约定：usedVars 含函数名（contract.UsedVars 注释、billing-01 §4.1）。
	p.used[name] = true
	return &callNode{name: name, args: args}, nil
}

// ---------------------------------------------------------------------------
// 对外接口
// ---------------------------------------------------------------------------

// Evaluator 是编译后的表达式。零值不可用，必须经 Compile 构造。
//
// 【并发约定】Compile 返回后 Evaluator 完全只读：Eval 只读取 AST 与入参，
// 不写任何共享状态；因此同一个 Evaluator 可被多个 goroutine 并发调用，
// 不需要加锁（billing-01 §3.3 允许编译产物跨请求复用）。
type Evaluator struct {
	root     node
	usedVars map[string]bool
}

// Compile 解析并校验表达式，返回可复用的求值器。
//
// 失败种类：EvalSyntax（语法/元数/空表达式）、EvalUnknownFunction（白名单
// 外函数）、EvalOverflow（字面量超出 float64 范围）。编译失败不产生
// Evaluator，调用方无需清理。
func Compile(src string) (*Evaluator, error) {
	p := &parser{lex: lexer{src: src}, used: make(map[string]bool)}
	if err := p.advance(); err != nil {
		return nil, err
	}
	if p.tok.kind == tokEOF {
		return nil, syntaxf("empty expression")
	}

	root, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	if p.tok.kind != tokEOF {
		// 表达式必须恰好消费完：`a b`、`1,2`、`a?b:c` 之类都在这里失败。
		return nil, syntaxf("unexpected %s at offset %d", p.tok.kind, p.tok.pos)
	}
	return &Evaluator{root: root, usedVars: p.used}, nil
}

// Eval 按变量绑定求值。
//
// 未绑定的变量返回 EvalUnknownVariable；除零返回 EvalDivideByZero；结果
// NaN/±Inf 返回 EvalOverflow。任何输入（包括 NaN/Inf 变量值）都不会导致
// panic。方法本身无副作用，可并发调用。
func (e *Evaluator) Eval(vars map[string]float64) (float64, error) {
	v, err := e.root.eval(vars)
	if err != nil {
		return 0, err
	}
	// 兜底：裸变量或常量路径不经过二元/一元节点，这里统一做有限性检查，
	// 保证「Eval 的结果永远是有限 float64」这一不变量对调用方成立。
	return finiteResult(v)
}

// UsedVars 返回表达式引用到的全部标识符，含函数名（如 min/max/round），
// 与 contract.UsedVars 的注释一致；下游按具体变量名查询（cr/img/...），
// 因此集合里混有函数名是安全的（billing-01 §4.1/§4.2 注记）。
//
// 返回副本：调用方可自由修改结果，不会污染 Evaluator 或并发调用者。
func (e *Evaluator) UsedVars() map[string]bool {
	out := make(map[string]bool, len(e.usedVars))
	for name := range e.usedVars {
		out[name] = true
	}
	return out
}
