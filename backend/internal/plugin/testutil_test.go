package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// fixture 是测试用的插件源生成器。
//
// 为什么不在测试里直接读 sample 源再字符串裁剪：裁剪对缩进/换行敏感，
// 一条规则改动就会让十几个用例一起失败；生成器让每个用例只声明它关心的
// 那一个字段（其余保持合规），失败信号因此是局部的。
type fixture struct {
	Key        string
	Name       string
	APIVersion string
	Version    string
	FetchMode  string
	EnDesc     string
	ZhDesc     string
	// DescriptionJS 覆盖整个 description 表达式（用于多 locale / 总长上限用例）。
	DescriptionJS       string
	UsageSchema         string
	UsageProfiles       string
	UsageExamples       string
	Routes              string
	Protocols           string
	Auth                string
	ChannelTypes        string
	Capabilities        string
	SubmitResponseTypes string
	// Exports 覆盖默认钩子：值为 JS 函数表达式；空字符串表示不导出该钩子。
	Exports map[string]string
	// ExtraMeta 追加顶层字段（key → 原始 JS 表达式）。
	ExtraMeta map[string]string
	Prepend   string
	Append    string
	NoExport  bool
}

const defaultUsageSchema = `{
    seconds: {
      type: "number",
      unit: "second",
      description: { en: "Video generation unit price", zh: "视频生成单价" }
    }
  }`

const defaultUsageProfiles = `[
    {
      models: ["sample-model"],
      schema: {
        seconds: {
          type: "number",
          unit: "second",
          description: { en: "Video generation unit price", zh: "视频生成单价" }
        }
      },
      examples: [{ seconds: 5 }]
    }
  ]`

var defaultExports = map[string]string{
	"buildSubmitRequest": `function (ctx) {
    return { url: (ctx.baseUrl || "https://example.invalid") + "/tasks", method: "POST", body: { model: ctx.model } };
  }`,
	"parseSubmitResponse": `function (resp) {
    return { taskId: "t1", state: "SUBMITTED" };
  }`,
	"buildQueryRequest": `function (ctx) {
    return { url: "https://example.invalid/tasks/" + ctx.taskId, method: "GET" };
  }`,
	"parseTaskResult": `function (ctx, body) {
    return { state: "SUCCESS" };
  }`,
}

func (f fixture) withDefaults() fixture {
	if f.Key == "" {
		f.Key = "sample"
	}
	if f.APIVersion == "" {
		f.APIVersion = "1"
	}
	if f.Version == "" {
		f.Version = "1.0.0"
	}
	if f.FetchMode == "" {
		f.FetchMode = "per_task"
	}
	if f.Name == "" {
		f.Name = "Sample"
	}
	if f.EnDesc == "" {
		f.EnDesc = "Sample task plugin"
	}
	if f.ZhDesc == "" {
		f.ZhDesc = "示例任务插件"
	}
	if f.UsageSchema == "" {
		f.UsageSchema = defaultUsageSchema
	}
	if f.UsageProfiles == "" {
		f.UsageProfiles = defaultUsageProfiles
	}
	if f.UsageExamples == "" {
		f.UsageExamples = `[{ seconds: 5 }]`
	}
	if f.Routes == "" {
		f.Routes = "[]"
	}
	if f.Protocols == "" {
		f.Protocols = "{}"
	}
	if f.Auth == "" {
		f.Auth = `{ type: "api_key" }`
	}
	if f.ChannelTypes == "" {
		f.ChannelTypes = "[]"
	}
	if f.Capabilities == "" {
		f.Capabilities = "[]"
	}
	return f
}

// Source 生成经典脚本源（冻结形态：赋给 globalThis.__NEWTG_PLUGIN__）。
func (f fixture) Source() string {
	f = f.withDefaults()
	if f.NoExport {
		return f.Prepend + "\n" + f.Append
	}
	exports := make(map[string]string, len(defaultExports)+len(f.Exports))
	for name, body := range defaultExports {
		exports[name] = body
	}
	for name, body := range f.Exports {
		if body == "" {
			delete(exports, name)
			continue
		}
		exports[name] = body
	}

	var b strings.Builder
	if f.Prepend != "" {
		b.WriteString(f.Prepend)
		b.WriteString("\n")
	}
	b.WriteString("globalThis.__NEWTG_PLUGIN__ = {\n")
	fmt.Fprintf(&b, "  apiVersion: %s,\n", f.APIVersion)
	fmt.Fprintf(&b, "  key: %q,\n", f.Key)
	fmt.Fprintf(&b, "  name: %q,\n", f.Name)
	fmt.Fprintf(&b, "  version: %q,\n", f.Version)
	b.WriteString("  author: { name: \"NewTG\" },\n")
	if f.DescriptionJS != "" {
		fmt.Fprintf(&b, "  description: %s,\n", f.DescriptionJS)
	} else {
		fmt.Fprintf(&b, "  description: { en: %q, zh: %q },\n", f.EnDesc, f.ZhDesc)
	}
	b.WriteString("  models: [\"sample-model\"],\n")
	fmt.Fprintf(&b, "  fetchMode: %q,\n", f.FetchMode)
	fmt.Fprintf(&b, "  auth: %s,\n", f.Auth)
	fmt.Fprintf(&b, "  channelTypes: %s,\n", f.ChannelTypes)
	b.WriteString("  routes: " + f.Routes + ",\n")
	fmt.Fprintf(&b, "  protocols: %s,\n", f.Protocols)
	fmt.Fprintf(&b, "  usageSchema: %s,\n", f.UsageSchema)
	fmt.Fprintf(&b, "  usageProfiles: %s,\n", f.UsageProfiles)
	fmt.Fprintf(&b, "  usageExamples: %s,\n", f.UsageExamples)
	fmt.Fprintf(&b, "  requiredCapabilities: %s,\n", f.Capabilities)
	if f.SubmitResponseTypes != "" {
		fmt.Fprintf(&b, "  submitResponseTypes: %s,\n", f.SubmitResponseTypes)
	}
	for name, value := range f.ExtraMeta {
		fmt.Fprintf(&b, "  %s: %s,\n", name, value)
	}
	for _, name := range sortedKeys(exports) {
		fmt.Fprintf(&b, "  %s: %s,\n", name, exports[name])
	}
	b.WriteString("};\n")
	if f.Append != "" {
		b.WriteString(f.Append)
		b.WriteString("\n")
	}
	return b.String()
}

func sourceHash(source string) string {
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:])
}

// fixtureWithKey 生成一个只改 key 的合规插件。
func fixtureWithKey(key string) string {
	return fixture{Key: key}.Source()
}

// routeJS 生成一条路由声明。
func routeJS(method, path, kind, decode, render string) string {
	return fmt.Sprintf(`[{ method: %q, path: %q, kind: %q, decode: %q, render: %q }]`,
		method, path, kind, decode, render)
}
