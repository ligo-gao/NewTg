package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// DryRunRequest 是管理端沙箱试运行请求（relay-06 §2.6 `POST /:key/dryrun`）。
//
// 安全语义（t3-brief 冻结约定 8）：默认**零副作用**——不出站、不写库、
// 不扣费；本包甚至不 import net/http，从构造上保证 dryrun 不可能发请求。
// AllowOutbound 目前不改变行为（出站执行器属后续任务），保留字段是为了
// 将来接线时 API 形状不破坏。
type DryRunRequest struct {
	Key string
	// Version 为空表示使用当前生效层；非空时从 loader 读取该版本源并编译。
	Version string
	// Endpoint 是试运行端点："submit" | "query" | "result" | "submit_response"，
	// 或任意已导出的钩子名（按单参数调用）。
	Endpoint string
	// Payload 是端点参数：submit/query 传 ctx 对象；result 传
	// {ctx, body, status, headers}；submit_response 传 {statusCode, headers, body}。
	Payload any
	// AllowOutbound 预留；本任务不实现出站，true 也不会产生网络调用。
	AllowOutbound bool
}

// DryRunResult 是试运行结果。Result 已做 JSON 往返以保证可序列化
// （插件可能返回函数/循环引用，管理端点不能因此 500）。
type DryRunResult struct {
	Key        string `json:"key"`
	Version    string `json:"version"`
	Endpoint   string `json:"endpoint"`
	Result     any    `json:"result"`
	DurationMS int64  `json:"duration_ms"`
}

// DryRun 在指定 key/version 上执行一次钩子。
func (m *Manager) DryRun(ctx context.Context, req DryRunRequest) (*DryRunResult, error) {
	if req.Key == "" {
		return nil, Issues{issuef("", "key", "", "dryrun_required", "dryrun 必须指定 key")}
	}
	plugin, err := m.resolveForDryRun(ctx, req)
	if err != nil {
		return nil, err
	}

	hook, args, err := dryRunCall(req)
	if err != nil {
		return nil, err
	}
	if !plugin.HasHook(hook) {
		return nil, Issues{issuef(req.Key, "hooks."+hook, "", "hook_missing", "插件未导出 %s", hook)}
	}

	start := time.Now()
	value, err := plugin.Call(ctx, hook, args...)
	duration := time.Since(start).Milliseconds()
	if err != nil {
		return nil, err
	}
	normalized, err := jsonNormalize(value)
	if err != nil {
		return nil, Issues{issuef(req.Key, "result", "", "dryrun_unserializable",
			"钩子返回值不可 JSON 序列化: %v", err)}
	}
	endpoint := req.Endpoint
	if endpoint == "" {
		endpoint = "submit"
	}
	return &DryRunResult{
		Key:        plugin.Key,
		Version:    plugin.Version,
		Endpoint:   endpoint,
		Result:     normalized,
		DurationMS: duration,
	}, nil
}

// resolveForDryRun 选择 dryrun 使用的编译实例。
//
// 指定版本时从 loader 读取源并**重新编译校验**（relay-06 §4.3：dryrun 走
// 同一校验，D 规则违规同样拒绝）；不指定版本时用当前快照的生效层。
func (m *Manager) resolveForDryRun(ctx context.Context, req DryRunRequest) (*CompiledPlugin, error) {
	if req.Version == "" {
		plugin, ok := m.Snapshot().Plugin(req.Key)
		if !ok {
			entry := m.Snapshot().Lookup(req.Key)
			return nil, Issues{issuef(req.Key, "", "", "dryrun_unavailable",
				"插件当前不可执行（status=%s）", entry.Status)}
		}
		return plugin, nil
	}
	if m.loader == nil {
		return nil, Issues{issuef(req.Key, "version", "", "dryrun_unavailable", "未配置版本加载器")}
	}
	version, err := m.loader.LoadVersion(ctx, req.Key, req.Version)
	if err != nil {
		return nil, Issues{issuef(req.Key, "version", "", "dryrun_version_not_found",
			"读取版本 %q 失败: %v", req.Version, err)}
	}
	if version == nil {
		return nil, Issues{issuef(req.Key, "version", "", "dryrun_version_not_found", "版本 %q 不存在", req.Version)}
	}
	plugin, issues := compileOverrideForDryRun(req.Key, *version, m.timeout)
	if issues.HasErrors() {
		return nil, issues
	}
	return plugin, nil
}

func compileOverrideForDryRun(key string, version ActiveVersion, timeout time.Duration) (*CompiledPlugin, Issues) {
	plugin, issues := compileOverride(key, version, timeout)
	if plugin != nil {
		return plugin, nil
	}
	return nil, issues
}

// dryRunCall 把端点名映射到钩子与调用参数。
func dryRunCall(req DryRunRequest) (hook string, args []any, err error) {
	switch req.Endpoint {
	case "", "submit":
		return "buildSubmitRequest", []any{req.Payload}, nil
	case "query":
		return "buildQueryRequest", []any{req.Payload}, nil
	case "submit_response":
		return "parseSubmitResponse", []any{req.Payload}, nil
	case "result":
		// parseTaskResult 的签名是 (ctx, body, {status, headers})；
		// 管理端 payload 用命名对象表达三段，避免调用方依赖位置参数。
		payload, ok := req.Payload.(map[string]any)
		if !ok {
			return "", nil, Issues{issuef(req.Key, "payload", "", "dryrun_payload",
				"result 端点的 payload 必须是 {ctx, body, status, headers} 对象")}
		}
		meta := map[string]any{}
		if v, ok := payload["status"]; ok {
			meta["status"] = v
		}
		if v, ok := payload["headers"]; ok {
			meta["headers"] = v
		}
		return "parseTaskResult", []any{payload["ctx"], payload["body"], meta}, nil
	default:
		// 其它钩子按「单参数」调用，便于沙箱页直接试任意导出钩子。
		return req.Endpoint, []any{req.Payload}, nil
	}
}

// jsonNormalize 通过 JSON 往返把 JS 导出值收敛为纯 JSON 数据。
//
// 意义：Sobek 的 safeExport 可能产出 map/slice 内的宿主对象；管理端点
// （httpapi）必须能直接编码。往返失败明确报错，不静默丢弃字段。
func jsonNormalize(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ErrDryRunUnavailable 预留给装配层做错误映射（当前用 Issues 表达）。
var ErrDryRunUnavailable = errors.New("plugin: dryrun 不可用")
