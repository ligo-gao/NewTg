package expr

import (
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
)

// mustCompile 编译测试表达式；失败直接终止当前测试。
func mustCompile(t *testing.T, src string) *Evaluator {
	t.Helper()
	e, err := Compile(src)
	if err != nil {
		t.Fatalf("Compile(%q) 意外失败: %v", src, err)
	}
	return e
}

// wantKind 断言 err 是 *EvalError 且种类匹配。
func wantKind(t *testing.T, err error, want EvalErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误种类 %s，实际 err=nil", want)
	}
	var ee *EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("err 类型 = %T（%v），期望 *EvalError", err, err)
	}
	if ee.Kind != want {
		t.Fatalf("错误种类 = %s（%v），期望 %s", ee.Kind, err, want)
	}
}

// TestEvalArithmetic 覆盖计划 Step 1(a)（组合函数求值）与四则/一元负号/
// 括号/科学计数法/取整边界（含 .5 负数边界）。
func TestEvalArithmetic(t *testing.T) {
	tests := []struct {
		name string
		src  string
		vars map[string]float64
		want float64
	}{
		{
			name: "计划样例 min(max)+round",
			src:  "min(max(a,2),3)+round(b)",
			vars: map[string]float64{"a": 5, "b": 2.4},
			want: 5, // min(max(5,2),3)=3 + round(2.4)=2
		},
		{
			name: "典型 token 计价 p*2.5+c*15",
			src:  "p*2.5 + c*15",
			vars: map[string]float64{"p": 100, "c": 10},
			want: 400,
		},
		{
			name: "科学计数法负指数",
			src:  "2e-3",
			want: 0.002,
		},
		{
			name: "科学计数法大写正指数",
			src:  "1E+1",
			want: 10,
		},
		{
			name: "括号改变优先级",
			src:  "(1+2)*3",
			want: 9,
		},
		{
			name: "一元负号",
			src:  "-a",
			vars: map[string]float64{"a": 2},
			want: -2,
		},
		{
			name: "一元负号作用于括号",
			src:  "-(a-b)",
			vars: map[string]float64{"a": 2, "b": 5},
			want: 3,
		},
		{
			name: "连续一元负号",
			src:  "--a",
			vars: map[string]float64{"a": 2},
			want: 2,
		},
		{
			name: "浮点除法",
			src:  "7/2",
			want: 3.5,
		},
		{
			name: "整数除法保持浮点语义",
			src:  "5/2",
			want: 2.5,
		},
		{
			name: "min 变参（3 个）",
			src:  "min(a,b,c)",
			vars: map[string]float64{"a": 3, "b": 1, "c": 2},
			want: 1,
		},
		{
			name: "max 变参（3 个）",
			src:  "max(a,b,c)",
			vars: map[string]float64{"a": 3, "b": 1, "c": 2},
			want: 3,
		},
		{
			name: "ceil 正数",
			src:  "ceil(2.0)",
			want: 2,
		},
		{
			name: "ceil 负数向零",
			src:  "ceil(-2.3)",
			want: -2,
		},
		{
			name: "floor 正数",
			src:  "floor(2.9)",
			want: 2,
		},
		{
			name: "floor 负数远离零",
			src:  "floor(-2.3)",
			want: -3,
		},
		{
			name: "round 正 .5 远离零",
			src:  "round(2.5)",
			want: 3,
		},
		{
			name: "round 负 .5 远离零",
			src:  "round(-2.5)",
			want: -3,
		},
		{
			name: "round 正不足 .5 舍去",
			src:  "round(2.4)",
			want: 2,
		},
		{
			name: "round 负不足 .5 舍去",
			src:  "round(-2.4)",
			want: -2,
		},
		{
			name: "round -0.5",
			src:  "round(-0.5)",
			want: -1,
		},
		{
			name: "round 0.5",
			src:  "round(0.5)",
			want: 1,
		},
		{
			name: "裸变量",
			src:  "a",
			vars: map[string]float64{"a": 42},
			want: 42,
		},
		{
			name: "空白与换行作为分隔",
			src:  "  1 +\t2\n",
			want: 3,
		},
		{
			name: "减号左结合",
			src:  "1-2-3",
			want: -4, // (1-2)-3，而非 1-(2-3)
		},
		{
			name: "除号左结合",
			src:  "8/4/2",
			want: 1, // (8/4)/2，而非 8/(4/2)
		},
		{
			name: "加减混合左结合",
			src:  "1-2+3",
			want: 2,
		},
		{
			name: "乘加优先级",
			src:  "2*3+4",
			want: 10,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := mustCompile(t, tc.src)
			got, err := e.Eval(tc.vars)
			if err != nil {
				t.Fatalf("Eval(%q) 意外失败: %v", tc.src, err)
			}
			if got != tc.want {
				t.Fatalf("Eval(%q) = %v，期望 %v", tc.src, got, tc.want)
			}
		})
	}
}

// TestEvalErrors 覆盖计划 Step 1(b)(c)(f)：除零返回 EvalError（非 panic）、
// 未知变量、溢出；并锁定非有限输入的兜底行为。
func TestEvalErrors(t *testing.T) {
	tests := []struct {
		name string
		src  string
		vars map[string]float64
		want EvalErrorKind
	}{
		{
			name: "除零",
			src:  "a / b",
			vars: map[string]float64{"a": 1, "b": 0},
			want: EvalDivideByZero,
		},
		{
			name: "表达式算出的除数为零",
			src:  "a / (b - c)",
			vars: map[string]float64{"a": 1, "b": 1, "c": 1},
			want: EvalDivideByZero,
		},
		{
			name: "0/0 也报除零而非 NaN",
			src:  "0 / 0",
			want: EvalDivideByZero,
		},
		{
			name: "负零除数",
			src:  "1 / -0",
			want: EvalDivideByZero,
		},
		{
			name: "未知变量",
			src:  "a + missing",
			vars: map[string]float64{"a": 1},
			want: EvalUnknownVariable,
		},
		{
			name: "空变量表下的裸变量",
			src:  "p",
			want: EvalUnknownVariable,
		},
		{
			name: "乘法溢出 1e308*10",
			src:  "1e308 * 10",
			want: EvalOverflow,
		},
		{
			name: "加法溢出",
			src:  "1e308 + 1e308",
			want: EvalOverflow,
		},
		{
			name: "变量为 +Inf",
			src:  "a",
			vars: map[string]float64{"a": math.Inf(1)},
			want: EvalOverflow,
		},
		{
			name: "变量为 NaN",
			src:  "a + 1",
			vars: map[string]float64{"a": math.NaN()},
			want: EvalOverflow,
		},
		{
			name: "一元负号作用于 Inf",
			src:  "-a",
			vars: map[string]float64{"a": math.Inf(1)},
			want: EvalOverflow,
		},
		{
			name: "min 传播 NaN",
			src:  "min(a, 5)",
			vars: map[string]float64{"a": math.NaN()},
			want: EvalOverflow,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := mustCompile(t, tc.src)
			// 不 recover：任何 panic 都会让测试直接失败，即证明「绝不 panic」。
			_, err := e.Eval(tc.vars)
			wantKind(t, err, tc.want)
		})
	}
}

// TestCompileRejects 覆盖计划 Step 1(d)：注入载荷与白名单外语法必须编译失败。
// 后半部分锁定「未列入 Task 1 白名单的函数在编译期报错」。
func TestCompileRejects(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want EvalErrorKind
	}{
		// 注入载荷
		{name: "分号命令注入", src: `a; rm -rf /`, want: EvalSyntax},
		{name: "分号后接数字", src: `1;2`, want: EvalSyntax},
		{name: "Python 导入调用", src: `__import__()`, want: EvalUnknownFunction},
		{name: "Python 导入带参数", src: `__import__(1)`, want: EvalUnknownFunction},
		{name: "模板插值", src: `${a}`, want: EvalSyntax},
		{name: "属性访问", src: `a.b`, want: EvalSyntax},
		{name: "line 注释", src: `a // comment`, want: EvalSyntax},
		{name: "block 注释", src: `a /* comment */`, want: EvalSyntax},
		{name: "井号注释", src: `a # comment`, want: EvalSyntax},
		{name: "反引号", src: "`a`", want: EvalSyntax},
		{name: "字符串字面量", src: `"a"`, want: EvalSyntax},
		{name: "非 ASCII 标识符", src: `变量 + 1`, want: EvalSyntax},

		// 空表达式/空白
		{name: "空表达式", src: ``, want: EvalSyntax},
		{name: "纯空白", src: " \t\r\n ", want: EvalSyntax},

		// 白名单外运算符/语法
		{name: "幂运算不存在", src: `a ** 2`, want: EvalSyntax},
		{name: "取模不在白名单", src: `a % b`, want: EvalSyntax},
		{name: "比较不在白名单", src: `a == b`, want: EvalSyntax},
		{name: "逻辑与不在白名单", src: `a && b`, want: EvalSyntax},
		{name: "三元不在白名单", src: `a ? b : c`, want: EvalSyntax},
		{name: "||| 不实现", src: `a ||| b`, want: EvalSyntax},
		{name: "赋值不在白名单", src: `a = 1`, want: EvalSyntax},
		{name: "方括号", src: `[a]`, want: EvalSyntax},
		{name: "一元正号不在白名单", src: `+1`, want: EvalSyntax},
		{name: "相邻表达式", src: `1 2`, want: EvalSyntax},
		{name: "两个标识符相邻", src: "a\nb", want: EvalSyntax},
		{name: "多个小数点", src: `1.2.3`, want: EvalSyntax},
		{name: "括号未闭合", src: `(1 + 2`, want: EvalSyntax},
		{name: "多余右括号", src: `1)`, want: EvalSyntax},
		{name: "空参数表", src: `()`, want: EvalSyntax},

		// 元数检查
		{name: "min 少于 2 参", src: `min(a)`, want: EvalSyntax},
		{name: "max 零参", src: `max()`, want: EvalSyntax},
		{name: "ceil 两参", src: `ceil(a, b)`, want: EvalSyntax},
		{name: "round 零参", src: `round()`, want: EvalSyntax},
		{name: "尾逗号", src: `min(a, b,)`, want: EvalSyntax},

		// 字面量溢出与不完整指数（指数不完整时回退，e 成为标识符）
		{name: "字面量超 float64", src: `1e999`, want: EvalOverflow},
		{name: "不完整指数 2e", src: `2e`, want: EvalSyntax},
		{name: "不完整指数 1e+", src: `1e+`, want: EvalSyntax},
		{name: "不完整指数 1e-", src: `1e-`, want: EvalSyntax},

		// 非法 UTF-8 / Unicode 空白（白名单只认 ASCII）
		{name: "非法 UTF-8 字节", src: "\xff\xfe", want: EvalSyntax},
		{name: "非法 UTF-8 截断序列", src: "\xc3\x28", want: EvalSyntax},
		{name: "Unicode 不换行空格", src: "  1", want: EvalSyntax},
		{name: "全角空格", src: "　 1", want: EvalSyntax},

		// 白名单外函数（含规格 §4.6 时间函数与完整引擎函数）
		{name: "abs 不在 Task 1 白名单", src: `abs(-1)`, want: EvalUnknownFunction},
		{name: "now 时间函数未开放", src: `now()`, want: EvalUnknownFunction},
		{name: "hour 时间函数未开放", src: `hour(x)`, want: EvalUnknownFunction},
		{name: "tier 未开放", src: `tier(a, 1)`, want: EvalUnknownFunction},
		{name: "fixed 未开放", src: `fixed(1)`, want: EvalUnknownFunction},
		{name: "u 任务用量未开放", src: `u(x)`, want: EvalUnknownFunction},
		{name: "未知函数", src: `frobnicate(a)`, want: EvalUnknownFunction},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, err := Compile(tc.src)
			if e != nil {
				t.Fatalf("Compile(%q) 意外成功", tc.src)
			}
			wantKind(t, err, tc.want)
		})
	}
}

// TestCompileAcceptsWhitelistedLiterals 锁定合法字面量形态（整数/浮点/
// 科学计数）与不依赖变量的常量表达式。
func TestCompileAcceptsWhitelistedLiterals(t *testing.T) {
	tests := []struct {
		src  string
		vars map[string]float64
		want float64
	}{
		{src: `0`, want: 0},
		{src: `123`, want: 123},
		{src: `1.5`, want: 1.5},
		{src: `2e-3`, want: 0.002},
		{src: `1E+1`, want: 10},
		{src: `3.5e2`, want: 350},
	}
	for _, tc := range tests {
		t.Run(tc.src, func(t *testing.T) {
			got, err := mustCompile(t, tc.src).Eval(tc.vars)
			if err != nil {
				t.Fatalf("Eval(%q) 意外失败: %v", tc.src, err)
			}
			if got != tc.want {
				t.Fatalf("Eval(%q) = %v，期望 %v", tc.src, got, tc.want)
			}
		})
	}
}

// TestCompileResourceLimits 覆盖评审 C-1：不可信输入不得通过无界递归触发
// fatal stack overflow。长度/深度两个上限都必须返回可恢复的 EvalSyntax。
func TestCompileResourceLimits(t *testing.T) {
	t.Run("超长源码被拒绝", func(t *testing.T) {
		src := strings.Repeat("1+", maxSourceLen/2+1) + "1"
		if len(src) <= maxSourceLen {
			t.Fatalf("测试构造错误：源码长度 %d 未超过上限 %d", len(src), maxSourceLen)
		}
		e, err := Compile(src)
		if e != nil {
			t.Fatalf("Compile(超长源码) 意外成功")
		}
		wantKind(t, err, EvalSyntax)
		var ee *EvalError
		if !errors.As(err, &ee) || !strings.Contains(ee.Message, "too long") {
			t.Fatalf("错误信息未提示超长: %v", err)
		}
	})

	t.Run("百万层括号（原崩溃向量）被长度上限拦截", func(t *testing.T) {
		src := strings.Repeat("(", 1_000_000) + "1" + strings.Repeat(")", 1_000_000)
		e, err := Compile(src)
		if e != nil {
			t.Fatalf("Compile(百万层括号) 意外成功")
		}
		wantKind(t, err, EvalSyntax)
	})

	t.Run("括号嵌套超过深度上限", func(t *testing.T) {
		src := strings.Repeat("(", 600) + "1" + strings.Repeat(")", 600)
		if len(src) > maxSourceLen {
			t.Fatalf("测试构造错误：深度用例不应触发长度上限")
		}
		e, err := Compile(src)
		if e != nil {
			t.Fatalf("Compile(600 层括号) 意外成功")
		}
		wantKind(t, err, EvalSyntax)
		var ee *EvalError
		if !errors.As(err, &ee) || !strings.Contains(ee.Message, "nesting") {
			t.Fatalf("错误信息未提示嵌套过深: %v", err)
		}
	})

	t.Run("一元负号超过深度上限", func(t *testing.T) {
		src := strings.Repeat("-", 600) + "1"
		e, err := Compile(src)
		if e != nil {
			t.Fatalf("Compile(600 个一元负号) 意外成功")
		}
		wantKind(t, err, EvalSyntax)
	})

	t.Run("函数调用嵌套超过深度上限", func(t *testing.T) {
		src := strings.Repeat("min(", 600) + "1,1" + strings.Repeat(")", 600)
		e, err := Compile(src)
		if e != nil {
			t.Fatalf("Compile(600 层函数嵌套) 意外成功")
		}
		wantKind(t, err, EvalSyntax)
	})

	t.Run("深度上限之下的括号仍可编译求值", func(t *testing.T) {
		src := strings.Repeat("(", 500) + "1" + strings.Repeat(")", 500)
		got, err := mustCompile(t, src).Eval(nil)
		if err != nil {
			t.Fatalf("500 层括号求值意外失败: %v", err)
		}
		if got != 1 {
			t.Fatalf("500 层括号 = %v，期望 1", got)
		}
	})

	t.Run("接近长度上限的合法加法链可求值", func(t *testing.T) {
		// 60 KB 源码 → 约 3 万层左深二元树：验证长度上限同时把求值递归
		// 深度约束在安全范围（动态栈可承载）。
		src := strings.Repeat("1+", 30_000) + "1"
		if len(src) > maxSourceLen {
			t.Fatalf("测试构造错误：合法加法链不应超过长度上限")
		}
		got, err := mustCompile(t, src).Eval(nil)
		if err != nil {
			t.Fatalf("长加法链求值意外失败: %v", err)
		}
		if got != 30_001 {
			t.Fatalf("长加法链 = %v，期望 30001", got)
		}
	})
}

// TestErrorMessagesBounded 覆盖评审 M-1：诊断信息中的字面量/标识符必须
// 截断到有界前缀，不能把用户输入原样放大进 EvalError.Message。
func TestErrorMessagesBounded(t *testing.T) {
	assertBounded := func(t *testing.T, err error) {
		t.Helper()
		var ee *EvalError
		if !errors.As(err, &ee) {
			t.Fatalf("err 类型 = %T（%v），期望 *EvalError", err, err)
		}
		if len(ee.Message) > 200 {
			t.Fatalf("错误信息长度 = %d，未被截断", len(ee.Message))
		}
		if !strings.Contains(ee.Message, "...") {
			t.Fatalf("错误信息未包含截断标记: %q", ee.Message)
		}
	}

	t.Run("1MB 数字字面量按长度上限拒绝", func(t *testing.T) {
		e, err := Compile(strings.Repeat("9", 1<<20))
		if e != nil {
			t.Fatalf("Compile(1MB 字面量) 意外成功")
		}
		wantKind(t, err, EvalSyntax)
		var ee *EvalError
		if !errors.As(err, &ee) || len(ee.Message) > 200 {
			t.Fatalf("1MB 字面量产生了未受限的错误信息: %v", err)
		}
	})

	t.Run("超大字面量的溢出信息被截断", func(t *testing.T) {
		e, err := Compile(strings.Repeat("9", 20_000))
		if e != nil {
			t.Fatalf("Compile(2 万位字面量) 意外成功")
		}
		wantKind(t, err, EvalOverflow)
		assertBounded(t, err)
	})

	t.Run("超大标识符的未知变量信息被截断", func(t *testing.T) {
		e := mustCompile(t, strings.Repeat("a", 20_000))
		_, err := e.Eval(nil)
		wantKind(t, err, EvalUnknownVariable)
		assertBounded(t, err)
	})

	t.Run("超大函数名的未知函数信息被截断", func(t *testing.T) {
		e, err := Compile(strings.Repeat("f", 20_000) + "(1)")
		if e != nil {
			t.Fatalf("Compile(超大函数名) 意外成功")
		}
		wantKind(t, err, EvalUnknownFunction)
		assertBounded(t, err)
	})
}

// TestUsedVars 覆盖计划 Step 1 补充项：内省必须含函数名，且返回副本。
func TestUsedVars(t *testing.T) {
	t.Run("变量与函数名都收录", func(t *testing.T) {
		e := mustCompile(t, `min(max(a,2),3)+round(b)`)
		got := e.UsedVars()
		want := map[string]bool{"min": true, "max": true, "a": true, "round": true, "b": true}
		if len(got) != len(want) {
			t.Fatalf("UsedVars() = %v，期望 %v", got, want)
		}
		for name := range want {
			if !got[name] {
				t.Fatalf("UsedVars() = %v，缺少 %q", got, name)
			}
		}
	})

	t.Run("纯 token 变量", func(t *testing.T) {
		got := mustCompile(t, `p*2.5 + c*15`).UsedVars()
		if len(got) != 2 || !got["p"] || !got["c"] {
			t.Fatalf("UsedVars() = %v，期望 {p,c}", got)
		}
	})

	t.Run("无标识符的常量表达式", func(t *testing.T) {
		got := mustCompile(t, `1+2`).UsedVars()
		if len(got) != 0 {
			t.Fatalf("UsedVars() = %v，期望空集", got)
		}
	})

	t.Run("返回副本不可污染内部状态", func(t *testing.T) {
		e := mustCompile(t, `ceil(x)`)
		first := e.UsedVars()
		first["injected"] = true
		second := e.UsedVars()
		if second["injected"] {
			t.Fatalf("UsedVars() 返回了内部 map，修改后污染了后续调用: %v", second)
		}
	})
}

// TestEvalConcurrent 是计划 Step 1 补充的并发冒烟：同一 Evaluator 被多个
// goroutine 并发求值，结果必须一致且无错误。若在 -race 下运行，也会覆盖
// 只读状态的数据竞争检查（环境不保证 -race 时本测试仍有价值）。
func TestEvalConcurrent(t *testing.T) {
	e := mustCompile(t, `min(max(a,b),c)+round(d)`)
	const (
		goroutines = 32
		iterations = 500
	)
	want := 6.0 // min(max(5,2),3)=3 + round(2.5)=3

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vars := map[string]float64{"a": 5, "b": 2, "c": 3, "d": 2.5}
			for i := 0; i < iterations; i++ {
				got, err := e.Eval(vars)
				if err != nil {
					errCh <- err
					return
				}
				if got != want {
					errCh <- errors.New("并发求值结果不一致")
					return
				}
				_ = e.UsedVars()
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("并发求值失败: %v", err)
	}
}
