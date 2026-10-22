package plugin

import (
	"encoding/json"
	"fmt"

	"github.com/grafana/sobek"
)

// 本文件提供本任务范围内的最小宿主工具面（t3-brief 冻结约定 9）。
//
// 已实现 `json-clone@1`：`utils.json.clone(value)` 返回一个独立的 JSON 快照。
// 完整工具集（unixNow / uuid / hmacSHA256 / jwtSignHS256 / base64 / volcSignV4）
// 需要各自的同步上界与密钥语义，留给后续宿主 API 任务；未实现的工具不在
// 全局对象上存在（调用即 ReferenceError/TypeError），而不是返回空值。
//
// `utils.hasCapability(name)` 不授予能力，只让插件探测**宿主支持**的某个
// 版本化能力（v1.d.ts）；真正的能力选择仍在加载期由 requiredCapabilities
// 决定，声明未知/重复版本会被拒绝。
//
// 安全边界：json.clone 无 I/O，唯一风险是内存放大，因此按 relay-06 §3.5
// 冻结 1 MiB / 32 层 / 32768 节点三重上限；超限抛 JS TypeError，绝不截断
// （截断会静默改变请求体）。
const (
	jsonCloneMaxBytes = 1 << 20
	jsonCloneMaxDepth = 32
	jsonCloneMaxNodes = 32768
)

// installHostUtils 把 utils 对象注入 Runtime 全局。
//
// 每次调用都是全新 Runtime，因此宿主对象不会跨请求泄漏；插件改全局
// utils 只影响自己那一次调用。
func installHostUtils(rt *sobek.Runtime) {
	utils := rt.NewObject()
	jsonObject := rt.NewObject()
	jsonObject.Set("clone", func(call sobek.FunctionCall) sobek.Value { return jsonClone(rt, call) })
	utils.Set("json", jsonObject)
	utils.Set("hasCapability", func(call sobek.FunctionCall) sobek.Value {
		name := ""
		if len(call.Arguments) > 0 {
			name = call.Argument(0).String()
		}
		// hasCapability 探测**宿主支持**的能力，与本次插件声明无关；
		// 它不授予任何能力（声明与加载期校验仍由 requiredCapabilities 决定）。
		_, supported := supportedCapabilities[name]
		return rt.ToValue(supported)
	})
	rt.GlobalObject().Set("utils", utils)
}

// jsonClone 实现 utils.json.clone。
//
// 用 JSON 往返而不是结构化遍历克隆：插件返回值本就应是 JSON 可表达的
// 请求/响应形状；往返同时天然拒绝函数、undefined 与循环引用。
func jsonClone(rt *sobek.Runtime, call sobek.FunctionCall) sobek.Value {
	if len(call.Arguments) == 0 {
		panic(rt.NewTypeError("utils.json.clone 需要一个参数"))
	}
	raw, err := json.Marshal(safeExport("", call.Argument(0)))
	if err != nil {
		panic(rt.NewTypeError("utils.json.clone: 值不可 JSON 序列化: " + err.Error()))
	}
	if len(raw) > jsonCloneMaxBytes {
		panic(rt.NewTypeError(fmt.Sprintf("utils.json.clone: 超过 %d 字节上限", jsonCloneMaxBytes)))
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		panic(rt.NewTypeError("utils.json.clone: 值不可 JSON 解析: " + err.Error()))
	}
	nodes := 0
	if err := checkJSONLimits(decoded, 1, &nodes); err != nil {
		panic(rt.NewTypeError("utils.json.clone: " + err.Error()))
	}
	return rt.ToValue(decoded)
}

// checkJSONLimits 遍历解码后的 JSON 值，施加层数与节点数上限。
//
// 层数从 1 开始计（标量为第 1 层）：与「32 层」的直觉一致，
// 第 33 层嵌套即拒绝。
func checkJSONLimits(value any, depth int, nodes *int) error {
	if depth > jsonCloneMaxDepth {
		return fmt.Errorf("超过 %d 层嵌套上限", jsonCloneMaxDepth)
	}
	*nodes++
	if *nodes > jsonCloneMaxNodes {
		return fmt.Errorf("超过 %d 节点上限", jsonCloneMaxNodes)
	}
	switch typed := value.(type) {
	case map[string]any:
		for _, item := range typed {
			if err := checkJSONLimits(item, depth+1, nodes); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range typed {
			if err := checkJSONLimits(item, depth+1, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}
