package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/grafana/sobek"
)

// DefaultTimeout 是单次插件调用（编译执行或钩子调用）的超时（relay-06 §5.1）。
//
// 冻结 5s 的依据：Sobek 无 async、宿主同步调用，一次钩子执行必须自带上界，
// 否则一个 `while(true){}` 就能永久占住请求 goroutine。宿主函数无法被抢占，
// 所以中断只能靠 `Runtime.Interrupt` 从计时器 goroutine 注入（见 runWithTimeout）。
const DefaultTimeout = 5 * time.Second

// exportGlobal 是冻结的导出入口名（t3-brief 冻结约定 1）。
//
// 用经典脚本 + 显式 globalThis 赋值，而不是 ESM：Sobek 的模块加载能力会把
// 宿主暴露成 `require`/import 面，沙箱边界会从「脚本」扩大到「模块图」。
const exportGlobal = "__NEWTG_PLUGIN__"

// Layer 标识插件来源层（relay-06 §2.3 三层模型）。
type Layer string

const (
	// LayerFactory 是内置编译层（`plugins/tasks/<key>/plugin.js`，编入二进制）。
	LayerFactory Layer = "factory"
	// LayerOverride 是数据库上传/覆盖层（DB 中 active 且 enabled 的版本行）。
	LayerOverride Layer = "override"
)

// errTimeout 是注入给 Runtime.Interrupt 的哨兵。
//
// 用哨兵而不是 context.DeadlineExceeded：调用方的 context 可能先到期，
// 二者都要映射为「timeout」规则；区分只为诊断。
var errTimeout = errors.New("plugin: 执行超时")

// CompiledPlugin 是「编译 + 校验」通过后的插件实例。
//
// 不可变：Program 与 Manifest 在发布后只读，可被多个请求 goroutine 共享；
// 每次调用都新建 Runtime，避免插件顶层状态在请求间泄漏（Sobek Runtime
// 不是并发安全的，也不能跨请求复用可变全局）。
type CompiledPlugin struct {
	Key      string
	Version  string
	Layer    Layer
	Source   string
	hash     string
	program  *sobek.Program
	manifest *Manifest
	hooks    map[string]struct{}
	warnings Issues
	timeout  time.Duration
}

// Manifest 返回只读元数据。
func (p *CompiledPlugin) Manifest() *Manifest { return p.manifest }

// SourceHash 返回 `source_text` 字节的 SHA-256 小写 hex（t3-brief 冻结约定 4）。
//
// 与 `task_plugin_versions.source_hash` 同口径：字节哈希，不做换行/编码归一，
// 否则「同源校验」会在 CRLF/LF 混用时失效。
func (p *CompiledPlugin) SourceHash() string { return p.hash }

// Warnings 返回加载期告警（D7 等），不阻止加载。
func (p *CompiledPlugin) Warnings() Issues { return p.warnings }

// Hooks 返回已导出的钩子名（升序，诊断与测试用）。
func (p *CompiledPlugin) Hooks() []string {
	out := make([]string, 0, len(p.hooks))
	for name := range p.hooks {
		out = append(out, name)
	}
	sortStrings(out)
	return out
}

// HasHook 判断钩子是否导出。
func (p *CompiledPlugin) HasHook(name string) bool {
	_, ok := p.hooks[name]
	return ok
}

// CompileOptions 是编译入口参数。
type CompileOptions struct {
	// Timeout 覆盖单次执行上限；0 走 DefaultTimeout。
	Timeout time.Duration
	// ExpectedKey 非空时强制 manifest.key 与来源 key（目录名 / DB 行 key）一致。
	ExpectedKey string
	// Layer 标记来源层，仅用于状态展示。
	Layer Layer
}

// Compile 编译并校验插件源。
//
// 顺序刻意是「沙箱扫描 → 语法编译 → 执行导出 → 元数据校验」：
//   - 沙箱扫描先于执行，避免在确认「无 I/O 入口」前就跑任意代码；
//   - 导出对象缺失（插件忘了赋值）归为 compile_failed（冻结约定 2）；
//   - 校验失败返回 Issues（结构化），不是普通 error。
func Compile(ctx context.Context, source string, opts CompileOptions) (*CompiledPlugin, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	key := opts.ExpectedKey

	if name, line, ok := findForbiddenIdentifier(source); ok {
		return nil, Issues{issuef(key, "source", "", "sandbox_forbidden",
			"第 %d 行: 插件源禁止使用 %q（无 fetch/require/import/文件/异步）", line, name)}
	}

	program, err := sobek.Compile("plugin.js", source, false)
	if err != nil {
		return nil, Issues{issuef(key, "source", "", "syntax_error", "%s", err.Error())}
	}

	// 执行与导出解析合并为一次：顶层的运行期异常、缺失导出对象与元数据
	// 提取都发生在同一个全新 Runtime 中，避免插件顶层逻辑被执行两次。
	plugin, issues := compileExport(ctx, key, source, program, timeout, opts.Layer)
	if issues.HasErrors() {
		return nil, issues
	}
	return plugin, nil
}

// compileExport 在全新 Runtime 中执行程序、取出导出对象并完成元数据校验。
//
// 一次执行同时承担「顶层运行期错误」与「导出对象提取」：插件顶层逻辑
// 只执行一次，避免任何顶层副作用被执行两遍。
func compileExport(ctx context.Context, key, source string, program *sobek.Program, timeout time.Duration, layer Layer) (*CompiledPlugin, Issues) {
	var (
		metaRaw map[string]any
		hooks   = map[string]struct{}{}
	)

	_, err := runWithTimeout(ctx, timeout, func(rt *sobek.Runtime) (any, error) {
		installHostUtils(rt)
		if _, err := rt.RunProgram(program); err != nil {
			return nil, err
		}
		exportValue := rt.GlobalObject().Get(exportGlobal)
		if exportValue == nil || sobek.IsUndefined(exportValue) || sobek.IsNull(exportValue) {
			return nil, errMissingExport
		}
		obj, ok := exportValue.(*sobek.Object)
		if !ok {
			return nil, errExportNotObject
		}
		raw := make(map[string]any, len(obj.Keys()))
		for _, name := range obj.Keys() {
			member := obj.Get(name)
			if _, ok := sobek.AssertFunction(member); ok {
				hooks[name] = struct{}{}
				continue
			}
			raw[name] = safeExport(key, member)
		}
		metaRaw = raw
		return nil, nil
	})
	if err != nil {
		if errors.Is(err, errMissingExport) {
			return nil, Issues{issuef(key, exportGlobal, "", "compile_failed", "执行后必须存在导出对象 %s", exportGlobal)}
		}
		if errors.Is(err, errExportNotObject) {
			return nil, Issues{issuef(key, exportGlobal, "", "compile_failed", "%s 必须是对象", exportGlobal)}
		}
		return nil, Issues{issuef(key, "source", "", runtimeRule(err), "%s", describeRuntimeError(err))}
	}

	manifest, decodeIssues := decodeManifest(key, metaRaw)
	if key != "" && manifest.Key != "" && manifest.Key != key {
		// 来源 key（目录名 / DB 行）与 meta.key 必须一致：任务行的
		// platform 会被改写为 meta.key（relay-06 §1.1），二义会让渠道
		// 绑定与任务归属对不上。
		decodeIssues = append(decodeIssues, issuef(key, "meta.key", "", "key_mismatch",
			"meta.key=%q 与来源 key %q 不一致", manifest.Key, key))
	}

	validateIssues, warnings := validateManifest(key, manifest, hooks)
	all := append(decodeIssues, validateIssues...)
	if all.HasErrors() {
		return nil, all
	}

	return &CompiledPlugin{
		Key:      manifest.Key,
		Version:  manifest.Version,
		Layer:    layer,
		Source:   source,
		hash:     hashSource(source),
		program:  program,
		manifest: manifest,
		hooks:    hooks,
		warnings: warnings,
		timeout:  timeout,
	}, nil
}

// errMissingExport / errExportNotObject 是编译导出的内部哨兵。
var (
	errMissingExport   = errors.New("plugin: 缺少导出对象")
	errExportNotObject = errors.New("plugin: 导出对象不是对象")
)

// safeExport 把 Sobek Value 转成 Go 值，并把 Export 的 panic 收敛为字符串。
//
// Export 可能因循环引用/宿主对象 panic；元数据校验不应因此崩溃整个启动链，
// 故 fail-closed 成「不可导出」标记，后续类型校验会把它报出来。
func safeExport(key string, v sobek.Value) (out any) {
	defer func() {
		if r := recover(); r != nil {
			out = fmt.Sprintf("<unexportable: %v>", r)
		}
	}()
	return v.Export()
}

func hashSource(source string) string {
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:])
}

// runWithTimeout 在给定 Runtime 上执行 fn，并用 Interrupt 施加上限。
//
// 为什么必须用 Interrupt 而不是只靠 context：钩子是同步 Go→JS 调用，
// 无法从外部抢占；Sobek 的 Interrupt 是唯一能中断长循环的机制。
// 计时器在独立 goroutine 触发 Interrupt 是 Sobek 明确支持的用法。
func runWithTimeout(ctx context.Context, timeout time.Duration, fn func(*sobek.Runtime) (any, error)) (result any, err error) {
	rt := sobek.New()
	defer func() {
		if panicked := recover(); panicked != nil {
			// fail-closed：JS 引擎的 panic（含栈溢出）统一转成错误，
			// 不允许一个坏插件拖垮安装/请求 goroutine。
			result, err = nil, fmt.Errorf("plugin: 运行时 panic: %v", panicked)
		}
	}()

	timer := time.AfterFunc(timeout, func() { rt.Interrupt(errTimeout) })
	defer timer.Stop()

	stop := context.AfterFunc(ctx, func() {
		rt.Interrupt(ctx.Err())
	})
	defer stop()

	return fn(rt)
}

// runtimeRule 把运行期错误映射到规则名。
func runtimeRule(err error) string {
	switch {
	case errors.Is(err, errTimeout), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	default:
		return "runtime_error"
	}
}

// describeRuntimeError 生成带行号的错误文案（t3-brief 冻结约定 2）。
func describeRuntimeError(err error) string {
	var interrupted *sobek.InterruptedError
	if errors.As(err, &interrupted) {
		if errors.Is(err, errTimeout) {
			return "执行超过时限，已中断"
		}
		return "执行被中断: " + interrupted.Error()
	}
	var exception *sobek.Exception
	if errors.As(err, &exception) {
		if line := firstLine(exception); line > 0 {
			return fmt.Sprintf("第 %d 行: %s", line, exception.Error())
		}
		return exception.Error()
	}
	return err.Error()
}

// firstLine 取异常栈第一帧的行号；原生帧没有位置信息。
func firstLine(exception *sobek.Exception) int {
	stack := exception.Stack()
	if len(stack) == 0 {
		return 0
	}
	return stack[0].Position().Line
}

// findForbiddenIdentifier 是加载期沙箱扫描（relay-06 §5.1 硬限制）。
//
// 运行时本身不提供 fetch/require/文件/定时器，但「不存在」只在**调用时**
// 才报 ReferenceError；一个未被触发的死代码仍会把危险意图带进二进制。
// 这里在编译前做词法级拒绝，让违规源无法注册。
//
// 扫描跳过字符串与注释（避免描述文案里出现 async 等词被误伤）；
// 正则字面量不单独识别——这是可接受的残留：正则里出现这些标识符
// 只会导致「更严格地拒绝」，不会放过危险代码（fail-closed）。
func findForbiddenIdentifier(source string) (string, int, bool) {
	forbidden := map[string]struct{}{
		"import": {}, "require": {}, "fetch": {},
		"XMLHttpRequest": {}, "WebSocket": {}, "process": {}, "Deno": {}, "Bun": {},
		"async": {}, "await": {}, "eval": {}, "setTimeout": {}, "setInterval": {},
	}
	line := 1
	idx := 0
	for idx < len(source) {
		ch := source[idx]
		switch {
		case ch == '\n':
			line++
			idx++
		case ch == '/' && idx+1 < len(source) && source[idx+1] == '/':
			for idx < len(source) && source[idx] != '\n' {
				idx++
			}
		case ch == '/' && idx+1 < len(source) && source[idx+1] == '*':
			idx += 2
			for idx+1 < len(source) && !(source[idx] == '*' && source[idx+1] == '/') {
				if source[idx] == '\n' {
					line++
				}
				idx++
			}
			idx += 2
		case ch == '\'' || ch == '"' || ch == '`':
			quote := ch
			idx++
			for idx < len(source) {
				if source[idx] == '\\' {
					if idx+1 < len(source) && source[idx+1] == '\n' {
						line++
					}
					idx += 2
					continue
				}
				if source[idx] == '\n' {
					line++
				}
				if source[idx] == quote {
					idx++
					break
				}
				idx++
			}
		case isIdentStart(ch):
			start := idx
			for idx < len(source) && isIdentPart(source[idx]) {
				idx++
			}
			word := source[start:idx]
			if _, bad := forbidden[word]; bad {
				return word, line, true
			}
		default:
			idx++
		}
	}
	return "", 0, false
}

func isIdentStart(ch byte) bool {
	return ch == '_' || ch == '$' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
}

func isIdentPart(ch byte) bool {
	return isIdentStart(ch) || (ch >= '0' && ch <= '9')
}

// Call 在全新 Runtime 中执行一个钩子。
//
// args 依次经 rt.ToValue 转换（map/slice/标量均可）。返回值是 JS 值的
// Go 导出；调用方负责按契约校验形状。
func (p *CompiledPlugin) Call(ctx context.Context, hook string, args ...any) (any, error) {
	if !p.HasHook(hook) {
		return nil, Issues{issuef(p.Key, "hooks."+hook, "", "hook_missing", "插件未导出 %s", hook)}
	}
	value, err := runWithTimeout(ctx, p.timeout, func(rt *sobek.Runtime) (any, error) {
		installHostUtils(rt)
		if _, err := rt.RunProgram(p.program); err != nil {
			return nil, err
		}
		exportValue := rt.GlobalObject().Get(exportGlobal)
		obj, ok := exportValue.(*sobek.Object)
		if !ok {
			return nil, errMissingExport
		}
		fn, ok := sobek.AssertFunction(obj.Get(hook))
		if !ok {
			return nil, fmt.Errorf("plugin: %s 不是函数", hook)
		}
		jsArgs := make([]sobek.Value, len(args))
		for idx, arg := range args {
			jsArgs[idx] = rt.ToValue(arg)
		}
		value, err := fn(sobek.Undefined(), jsArgs...)
		if err != nil {
			return nil, err
		}
		if value == nil || sobek.IsUndefined(value) || sobek.IsNull(value) {
			return nil, nil
		}
		return safeExport(p.Key, value), nil
	})
	if err != nil {
		// 钩子执行失败一律结构化为 Issue：超时/异常必须能被管理端点与
		// 任务路径按规则名分类（超时而非「未知错误」）。
		return nil, Issues{issuef(p.Key, "hooks."+hook, "", runtimeRule(err), "%s", describeRuntimeError(err))}
	}
	return value, nil
}

// sortStrings 是小的插入排序，避免为几元素排序引入 sort 依赖噪音。
func sortStrings(items []string) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j-1] > items[j]; j-- {
			items[j-1], items[j] = items[j], items[j-1]
		}
	}
}
