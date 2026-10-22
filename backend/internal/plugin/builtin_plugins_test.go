package plugin

// 内置任务插件源的验收（§14 G-M6D-01 / relay-06 §2.1、§12 AC-F-01）。
//
// 本文件刻意区分两件事：
//  1. **源可编译、已编入二进制**：10 个 key 的目录与 manifest 必须存在，
//     且通过 T/D 加载期校验——这是注册表 registered 徽章的含义；
//  2. **厂商实现是否完备**：只有仓库内存在足够契约的 key 才实现生命周期
//     钩子；其余源是显式 unsupported 占位，builtinStatus 自述
//     not_registered，生命周期钩子一律抛错。
//
// 为什么两者必须分开：注册表没有“源级退出”语义——只要 plugin.js 可编译，
// Manager 就会给出 registered。若测试把徽章当成交付证据，就会把 9 个占位
// 源伪报为“已实现”。因此本测试不以徽章判定实现完备度，而是以源内
// builtinStatus + 钩子行为为准，并断言未实现项绝不伪装成可出站实现。

import (
	"context"
	"slices"
	"strings"
	"testing"

	"newtg/gateway/plugins"
)

// builtinPluginSpec 是每个内置 key 的**验收期望**。
//
// status 使用 PluginStatus 的字面量，但含义是“实现完备度”，不是 Manager
// 快照徽章：registered = 已按仓库内契约实现；not_registered = 显式
// unsupported 占位（缺厂商契约）。
type builtinPluginSpec struct {
	key             string
	status          PluginStatus
	missingContract string
	fetchMode       string
	channelTypes    []int
	models          []string
	// units 断言 usageSchema 的字段名 → 规范化单位（relay-06 §9.3）。
	units map[string]string
	// lifecycleHooks 是该 fetchMode 下必须导出的生命周期钩子。
	lifecycleHooks []string
}

var builtinPluginSpecs = []builtinPluginSpec{
	{
		key:             "alibaba",
		status:          StatusNotRegistered,
		missingContract: "DashScope/Wan",
		fetchMode:       "per_task",
		channelTypes:    []int{17},
		models:          []string{"wan", "qwen-image", "z-image"},
		units:           map[string]string{"seconds": "second", "image_count": "count"},
		lifecycleHooks:  perTaskHooks(),
	},
	{
		key:             "doubao",
		status:          StatusNotRegistered,
		missingContract: "Volcengine Ark",
		fetchMode:       "per_task",
		channelTypes:    []int{54, 45},
		models:          []string{"seedance", "seedream"},
		units:           map[string]string{"seconds": "second", "image_count": "count"},
		lifecycleHooks:  perTaskHooks(),
	},
	{
		key:             "google",
		status:          StatusNotRegistered,
		missingContract: "Gemini API Veo",
		fetchMode:       "per_task",
		channelTypes:    []int{24},
		models:          []string{"veo"},
		units:           map[string]string{"seconds": "second"},
		lifecycleHooks:  perTaskHooks(),
	},
	{
		key:             "hailuo",
		status:          StatusNotRegistered,
		missingContract: "MiniMax Hailuo",
		fetchMode:       "per_task",
		channelTypes:    []int{35},
		models:          []string{"hailuo"},
		units:           map[string]string{"seconds": "second"},
		lifecycleHooks:  perTaskHooks(),
	},
	{
		key:             "jimeng",
		status:          StatusNotRegistered,
		missingContract: "Jimeng",
		fetchMode:       "per_task",
		channelTypes:    []int{51},
		models:          []string{"jimeng"},
		units:           map[string]string{"seconds": "second"},
		lifecycleHooks:  perTaskHooks(),
	},
	{
		key:             "kling",
		status:          StatusNotRegistered,
		missingContract: "Kling",
		fetchMode:       "per_task",
		channelTypes:    []int{50},
		models:          []string{"kling"},
		units:           map[string]string{"seconds": "second", "units": "credit"},
		lifecycleHooks:  perTaskHooks(),
	},
	{
		key:          "sora",
		status:       StatusRegistered,
		fetchMode:    "per_task",
		channelTypes: []int{55, 1},
		models:       []string{"sora"},
		units:        map[string]string{"seconds": "second"},
		// Sora 走仓库冻结的 OpenAI Videos 契约（relay-06 §3.8），
		// 因此是唯一按真实请求/轮询/产物契约实现的 key。
		lifecycleHooks: perTaskHooks(),
	},
	{
		key:             "sunoapi",
		status:          StatusNotRegistered,
		missingContract: "SunoAPI",
		fetchMode:       "batch",
		channelTypes:    []int{36},
		models:          []string{"suno"},
		units:           map[string]string{"clips": "count"},
		lifecycleHooks: []string{
			"buildSubmitRequest", "parseSubmitResponse", "parseTaskResult",
			"buildBatchQueryRequest", "parseBatchResult",
		},
	},
	{
		key:             "vertex-ai",
		status:          StatusNotRegistered,
		missingContract: "Vertex AI Veo",
		fetchMode:       "per_task",
		channelTypes:    []int{41},
		models:          []string{"veo"},
		units:           map[string]string{"seconds": "second"},
		lifecycleHooks:  perTaskHooks(),
	},
	{
		key:             "vidu",
		status:          StatusNotRegistered,
		missingContract: "Vidu",
		fetchMode:       "per_task",
		channelTypes:    []int{52},
		models:          []string{"vidu"},
		units:           map[string]string{"seconds": "second", "credits": "credit"},
		lifecycleHooks:  perTaskHooks(),
	},
}

func perTaskHooks() []string {
	return []string{"buildSubmitRequest", "parseSubmitResponse", "buildQueryRequest", "parseTaskResult"}
}

// TestBuiltinTaskPluginsExistAndCompile 覆盖 AC-F-01 的前半：
// 10 个 key 的源存在，manifest 字段/渠道类型/用量单位符合设计，且能通过
// Compile（含 T1–T10 / D1–D7）——不因缺厂商契约而跳过加载期校验。
func TestBuiltinTaskPluginsExistAndCompile(t *testing.T) {
	if len(builtinPluginSpecs) != 10 {
		t.Fatalf("内置插件期望 10 个 key，实际 %d", len(builtinPluginSpecs))
	}
	sources, err := plugins.TaskPluginSources()
	if err != nil {
		t.Fatalf("读取内置插件源: %v", err)
	}
	for _, spec := range builtinPluginSpecs {
		spec := spec
		t.Run(spec.key, func(t *testing.T) {
			source, ok := sources[spec.key]
			if !ok {
				t.Fatalf("缺少内置插件源 %q（现有 %v）", spec.key, sortedKeys(sources))
			}
			compiled, err := Compile(context.Background(), source, CompileOptions{
				ExpectedKey: spec.key,
				Layer:       LayerFactory,
			})
			if err != nil {
				t.Fatalf("%s 编译/校验失败: %v", spec.key, err)
			}
			manifest := compiled.Manifest()
			if manifest.Key != spec.key {
				t.Fatalf("manifest.key=%q，期望 %q", manifest.Key, spec.key)
			}
			if manifest.FetchMode != spec.fetchMode {
				t.Fatalf("%s fetchMode=%q，期望 %q", spec.key, manifest.FetchMode, spec.fetchMode)
			}
			if !slices.Equal(manifest.ChannelTypes, spec.channelTypes) {
				t.Fatalf("%s channelTypes=%v，期望 %v", spec.key, manifest.ChannelTypes, spec.channelTypes)
			}
			if !slices.Equal(manifest.Models, spec.models) {
				t.Fatalf("%s models=%v，期望 %v", spec.key, manifest.Models, spec.models)
			}
			for name, unit := range spec.units {
				field, ok := manifest.UsageSchema[name]
				if !ok {
					t.Fatalf("%s usageSchema 缺少字段 %q", spec.key, name)
				}
				if field.Unit != unit {
					t.Fatalf("%s usageSchema.%s unit=%q，期望 %q", spec.key, name, field.Unit, unit)
				}
				if len(field.Description) == 0 {
					t.Fatalf("%s usageSchema.%s 缺 description（T7）", spec.key, name)
				}
			}
			for _, hook := range spec.lifecycleHooks {
				if !compiled.HasHook(hook) {
					t.Fatalf("%s 缺少必需钩子 %s（现有 %v）", spec.key, hook, compiled.Hooks())
				}
			}
			if !compiled.HasHook("builtinStatus") {
				t.Fatalf("%s 缺少 builtinStatus 实现完备度自述", spec.key)
			}
		})
	}
}

// TestBuiltinPluginImplementationStatusIsHonest 覆盖“不得把未实现项伪报为
// registered”：每个源自述的状态必须与验收表一致；占位源必须
//   - 给出缺失的厂商契约；
//   - 不包含任何伪装的出站 URL（example.invalid / http(s)://）；
//   - 每个生命周期钩子都显式抛 unsupported，而不是返回空描述符或假响应。
func TestBuiltinPluginImplementationStatusIsHonest(t *testing.T) {
	sources, err := plugins.TaskPluginSources()
	if err != nil {
		t.Fatalf("读取内置插件源: %v", err)
	}
	registered := 0
	for _, spec := range builtinPluginSpecs {
		spec := spec
		t.Run(spec.key, func(t *testing.T) {
			source := sources[spec.key]
			compiled := mustCompile(t, source, CompileOptions{ExpectedKey: spec.key, Layer: LayerFactory})

			raw, err := compiled.Call(context.Background(), "builtinStatus")
			if err != nil {
				t.Fatalf("builtinStatus 调用失败: %v", err)
			}
			status, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("builtinStatus 必须返回对象，实际 %T", raw)
			}
			got := builtinString(status["status"])
			if got != string(spec.status) {
				t.Fatalf("%s builtinStatus=%q，期望 %q（不得把占位源伪报为 registered）",
					spec.key, got, spec.status)
			}

			if spec.status == StatusRegistered {
				registered++
				return
			}
			if spec.missingContract == "" {
				t.Fatalf("%s 标注 not_registered 但未说明缺失的厂商契约", spec.key)
			}
			missing := builtinString(status["missingContract"])
			if !strings.Contains(missing, spec.missingContract) {
				t.Fatalf("%s missingContract=%q，未包含 %q", spec.key, missing, spec.missingContract)
			}
			// 未实现源不得出现可执行端点：注释里可以引用渠道 baseUrl，
			// 但代码里既不能有 url 描述符，也不能有 example.invalid 之类的
			// 占位域名（那会把占位源伪装成“可注册实现”）。
			for _, needle := range []string{"example.invalid", "url:"} {
				if strings.Contains(source, needle) {
					t.Fatalf("%s 占位源含疑似伪造端点 %q；未实现时不得写 URL", spec.key, needle)
				}
			}
			for _, hook := range spec.lifecycleHooks {
				if _, callErr := compiled.Call(context.Background(), hook, map[string]any{}); callErr == nil {
					t.Fatalf("%s.%s 不得在契约缺失时成功返回", spec.key, hook)
				} else if !strings.Contains(callErr.Error(), "unsupported") {
					t.Fatalf("%s.%s 必须显式报 unsupported，实际 %v", spec.key, hook, callErr)
				}
			}
		})
	}
	if registered != 1 {
		t.Fatalf("按仓库内契约实现的 key 应恰为 1（sora），实际 %d", registered)
	}
}

// TestBuiltinRegisteredKeysAreRunnable 断言“已实现”集合的判定不是文字游戏：
// 只有 builtinStatus=registered 的 key 在 Manager 快照中可执行；占位源即使
// 因可编译而获得 registered 徽章，也必须继续自述 not_registered 且钩子不可用。
func TestBuiltinRegisteredKeysAreRunnable(t *testing.T) {
	sources, err := plugins.TaskPluginSources()
	if err != nil {
		t.Fatalf("读取内置插件源: %v", err)
	}
	manager := MustNewManager(sources, nil, Options{MasterEnabled: true})
	if err := manager.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	for _, spec := range builtinPluginSpecs {
		entry := manager.Snapshot().Lookup(spec.key)
		if spec.status == StatusRegistered {
			if entry.Status != StatusRegistered || entry.Plugin == nil {
				t.Fatalf("%s 已实现，应可执行: %+v", spec.key, entry)
			}
			continue
		}
		// 占位源是合法 JavaScript，注册表会给出 registered；该徽章只证明
		// “源可编译并已装”，不证明厂商契约已实现。这里以 builtinStatus +
		// 钩子失败证明它没有被当作真实实现。
		if entry.Plugin == nil {
			t.Fatalf("%s 占位源也应可编译（否则内置加载失败）: %+v", spec.key, entry)
		}
		raw, err := entry.Plugin.Call(context.Background(), "builtinStatus")
		if err != nil {
			t.Fatalf("%s builtinStatus: %v", spec.key, err)
		}
		status, _ := raw.(map[string]any)
		if builtinString(status["status"]) != string(StatusNotRegistered) {
			t.Fatalf("%s 占位源自述状态必须为 not_registered: %v", spec.key, status)
		}
	}
}

// TestBuiltinSoraHooksUseFrozenOpenAIVideosContract 覆盖已实现 key 的真实
// 变换行为：URL 由渠道 baseUrl 派生（不硬编码厂商域名），请求/响应/状态/
// 产物/用量均落在 relay-06 §3.8 冻结的 OpenAI Videos 形状内。
func TestBuiltinSoraHooksUseFrozenOpenAIVideosContract(t *testing.T) {
	sources, err := plugins.TaskPluginSources()
	if err != nil {
		t.Fatalf("读取内置插件源: %v", err)
	}
	compiled := mustCompile(t, sources["sora"], CompileOptions{ExpectedKey: "sora", Layer: LayerFactory})
	if _, ok := compiled.Manifest().Protocols["openai_video"]; !ok {
		t.Fatal("sora 必须声明 openai_video，/v1/videos 才会把请求交给它（R-15）")
	}

	const baseURL = "https://api.openai.com"
	ctxWithAuth := map[string]any{
		"model":         "sora",
		"upstreamModel": "sora-2",
		"baseUrl":       baseURL,
		"apiKey":        "sk-test",
		"authHeader":    "Bearer sk-test",
	}

	t.Run("submit", func(t *testing.T) {
		desc := callBuiltinObject(t, compiled, "buildSubmitRequest", map[string]any{
			"model":         "sora",
			"upstreamModel": "sora-2",
			"baseUrl":       baseURL,
			"apiKey":        "sk-test",
			"authHeader":    "Bearer sk-test",
			"prompt":        "a cat surfing",
			"seconds":       8,
			"size":          "1280x720",
			"body": map[string]any{
				"prompt":  "a cat surfing",
				"seconds": 8,
				"size":    "1280x720",
			},
		})
		if got := builtinString(desc["url"]); got != baseURL+"/v1/videos" {
			t.Fatalf("submit url=%q，期望 %q", got, baseURL+"/v1/videos")
		}
		if got := builtinString(desc["method"]); got != "POST" {
			t.Fatalf("submit method=%q", got)
		}
		headers, _ := desc["headers"].(map[string]any)
		if got := builtinString(headers["Authorization"]); got != "Bearer sk-test" {
			t.Fatalf("submit Authorization=%q", got)
		}
		body, _ := desc["body"].(map[string]any)
		if got := builtinString(body["model"]); got != "sora-2" {
			t.Fatalf("submit body.model=%q，期望映射后的 sora-2", got)
		}
		if got := builtinString(body["prompt"]); got != "a cat surfing" {
			t.Fatalf("submit body.prompt=%q", got)
		}
		if got, ok := builtinNumber(body["seconds"]); !ok || got != 8 {
			t.Fatalf("submit body.seconds=%v，期望 8", body["seconds"])
		}
		if got := builtinString(body["size"]); got != "1280x720" {
			t.Fatalf("submit body.size=%q", got)
		}
	})

	t.Run("submit_response", func(t *testing.T) {
		parsed := callBuiltinObject(t, compiled, "parseSubmitResponse", map[string]any{
			"statusCode": 200,
			"headers":    map[string]any{"Content-Type": "application/json"},
			"body":       `{"id":"video_123","object":"video","status":"queued","seconds":"8"}`,
		})
		if got := builtinString(parsed["taskId"]); got != "video_123" {
			t.Fatalf("taskId=%q", got)
		}
		if got := builtinString(parsed["state"]); got != "QUEUED" {
			t.Fatalf("state=%q，期望 QUEUED", got)
		}
		if _, ok := parsed["immediate"]; ok {
			t.Fatal("queued 任务不得返回 immediate 终态")
		}
	})

	t.Run("submit_immediate_failure", func(t *testing.T) {
		parsed := callBuiltinObject(t, compiled, "parseSubmitResponse", map[string]any{
			"statusCode": 200,
			"body":       `{"id":"video_456","status":"failed"}`,
		})
		immediate, _ := parsed["immediate"].(map[string]any)
		if got := builtinString(immediate["status"]); got != "FAILURE" {
			t.Fatalf("immediate.status=%q，期望 FAILURE", got)
		}
	})

	t.Run("query", func(t *testing.T) {
		desc := callBuiltinObject(t, compiled, "buildQueryRequest", map[string]any{
			"baseUrl":    baseURL,
			"taskId":     "video_123",
			"authHeader": "Bearer sk-test",
		})
		if got := builtinString(desc["url"]); got != baseURL+"/v1/videos/video_123" {
			t.Fatalf("query url=%q", got)
		}
		if got := builtinString(desc["method"]); got != "GET" {
			t.Fatalf("query method=%q", got)
		}
	})

	t.Run("result_success", func(t *testing.T) {
		parsed := callBuiltinObject(t, compiled, "parseTaskResult",
			ctxWithAuth,
			`{"id":"video_123","status":"completed","seconds":"8","progress":100}`,
			map[string]any{"status": 200},
		)
		if got := builtinString(parsed["state"]); got != "SUCCESS" {
			t.Fatalf("state=%q，期望 SUCCESS", got)
		}
		if got, ok := builtinNumber(parsed["progress"]); !ok || got != 100 {
			t.Fatalf("progress=%v，期望 100", parsed["progress"])
		}
		usage, _ := parsed["usage"].(map[string]any)
		if got, ok := builtinNumber(usage["seconds"]); !ok || got != 8 {
			t.Fatalf("usage.seconds=%v，期望 8", usage["seconds"])
		}
		taskData, _ := parsed["taskData"].(map[string]any)
		artifacts, _ := taskData["artifacts"].([]any)
		if len(artifacts) != 1 {
			t.Fatalf("artifacts=%v，期望 1 项", taskData["artifacts"])
		}
		artifact, _ := artifacts[0].(map[string]any)
		if builtinString(artifact["key"]) != "video" || builtinString(artifact["type"]) != "video" {
			t.Fatalf("artifact=%v，期望 {key:video,type:video}", artifacts[0])
		}
	})

	t.Run("result_failure_and_unknown", func(t *testing.T) {
		failed := callBuiltinObject(t, compiled, "parseTaskResult", ctxWithAuth,
			`{"id":"video_123","status":"failed","error":{"message":"bad prompt"}}`,
			map[string]any{"status": 200})
		if got := builtinString(failed["state"]); got != "FAILURE" {
			t.Fatalf("state=%q，期望 FAILURE", got)
		}
		if got := builtinString(failed["failReason"]); got != "bad prompt" {
			t.Fatalf("failReason=%q", got)
		}

		unknown := callBuiltinObject(t, compiled, "parseTaskResult", ctxWithAuth,
			`{"id":"video_123","status":"a-state-we-do-not-know"}`, map[string]any{"status": 200})
		if got := builtinString(unknown["state"]); got != "UNKNOWN" {
			t.Fatalf("未识别状态必须原样返回 UNKNOWN，实际 %q", got)
		}
	})

	t.Run("usage_and_artifacts", func(t *testing.T) {
		usage := callBuiltinObject(t, compiled, "extractUsageOnComplete",
			ctxWithAuth,
			map[string]any{"taskData": map[string]any{"seconds": "8"}},
			map[string]any{},
		)
		if got, ok := builtinNumber(usage["seconds"]); !ok || got != 8 {
			t.Fatalf("extractUsageOnComplete=%v", usage)
		}
		estimated := callBuiltinObject(t, compiled, "extractUsage", map[string]any{
			"body": map[string]any{"seconds": 5},
		})
		if got, ok := builtinNumber(estimated["seconds"]); !ok || got != 5 {
			t.Fatalf("extractUsage=%v", estimated)
		}

		raw, err := compiled.Call(context.Background(), "listArtifacts")
		if err != nil {
			t.Fatalf("listArtifacts: %v", err)
		}
		items, ok := raw.([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("listArtifacts=%v", raw)
		}

		content := callBuiltinObject(t, compiled, "buildContentRequest", map[string]any{
			"baseUrl":     baseURL,
			"taskId":      "video_123",
			"artifactKey": "video",
			"authHeader":  "Bearer sk-test",
		})
		if got := builtinString(content["url"]); got != baseURL+"/v1/videos/video_123/content" {
			t.Fatalf("content url=%q", got)
		}
	})
}

// callBuiltinObject 调用一个返回对象的钩子；失败即终止子测试。
func callBuiltinObject(t *testing.T, compiled *CompiledPlugin, hook string, args ...any) map[string]any {
	t.Helper()
	raw, err := compiled.Call(context.Background(), hook, args...)
	if err != nil {
		t.Fatalf("%s.%s 调用失败: %v", compiled.Key, hook, err)
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%s.%s 必须返回对象，实际 %T", compiled.Key, hook, raw)
	}
	return obj
}

func builtinString(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func builtinNumber(value any) (float64, bool) {
	switch n := value.(type) {
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
