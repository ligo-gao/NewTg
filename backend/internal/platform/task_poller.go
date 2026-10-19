package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"

	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/config"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/httpapi"
	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/store"
	"newtg/gateway/internal/tasks"
	"newtg/gateway/plugins"
)

// 本文件把 `async_task_poll` 装配进 worker 角色（relay-06 §8.1/§9.6）。
//
// 为什么放 worker 而不是 gateway：轮询是后台驱动，与请求生命周期无关；
// worker 已经持有计费引擎（ByRequestID 结算/退款）与配置中心，gateway 只
// 需要处理客户端请求。单副本约束与计费兜底循环一致（M3 R-04）。

// startTaskPoller 构造插件运行时与轮询器并在后台运行，返回停止函数。
//
// 停止函数会取消上下文并等待循环退出：必须在关闭 DB 连接池之前调用，
// 否则下一轮轮询会拿到「连接已关闭」错误，把正常停机噪声打进告警。
func startTaskPoller(parent context.Context, cfg *config.Config, db *sql.DB,
	center *configcenter.Center, engine *billing.Engine, log *slog.Logger) (func(), error) {

	sources, err := plugins.TaskPluginSources()
	if err != nil {
		return nil, fmt.Errorf("worker: task plugin sources: %w", err)
	}
	manager, err := plugin.NewManager(sources, plugin.NewSQLVersionLoader(db), plugin.Options{
		Node:                cfg.NodeName,
		MasterEnabled:       configBool(center, "plugin.enabled", true),
		DisabledFactoryKeys: configStringList(center, "plugin.disabled_factory_keys"),
	})
	if err != nil {
		// 内置插件编译失败 = 构建期缺陷；带残缺注册表轮询会让任务解析
		// 到错误插件，必须阻止 worker 上线。
		return nil, fmt.Errorf("worker: load built-in task plugins: %w", err)
	}
	// 与 gateway 装配同口径：B-11 里被管理员停用的内置插件也不得在 worker
	// 侧参与轮询，否则「管理面已停用」与「仍在解析任务」两处状态分裂。
	if pluginsList, listErr := store.NewTaskPluginRepo(db).List(parent); listErr == nil {
		disabled := configStringList(center, "plugin.disabled_factory_keys")
		for _, p := range pluginsList {
			if p.Builtin && !p.Enabled {
				disabled = append(disabled, p.Key)
			}
		}
		manager.SetDisabledFactoryKeys(disabled)
	}
	if err := manager.Reload(parent); err != nil {
		// 与 gateway 装配同口径：一次瞬时的 DB 抖动不阻断 worker 启动，
		// 快照为空时轮询只会累计失败/超时退款，且 30s 后自动重载。
		log.Warn("worker: initial task plugin reload failed", "error", err)
	}

	sealer := httpapi.NewSecretSealer(cfg.CredentialEncryptionKey)
	poller, err := tasks.NewPoller(tasks.PollerConfig{
		Repo:    tasks.NewRepo(db),
		Plugins: manager,
		Billing: engine,
		Decrypt: func(sealed []byte) (string, error) {
			return decryptTaskChannelCredential(sealer, sealed)
		},
		Logger: log,
	})
	if err != nil {
		return nil, fmt.Errorf("worker: task poller: %w", err)
	}

	pollCtx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := poller.Run(pollCtx); err != nil {
			log.Error("worker: task poller exited with error", "error", err)
		}
	}()
	return func() {
		cancel()
		<-done
	}, nil
}

// decryptTaskChannelCredential 解开 channels.credentials 并取出 api_key。
// 空密文表示未配置凭据（auth=none 的插件），返回空串。
func decryptTaskChannelCredential(sealer *httpapi.SecretSealer, sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	plain, err := sealer.Open(sealed)
	if err != nil {
		return "", err
	}
	var creds httpapi.ChannelCredentials
	if err := json.Unmarshal(plain, &creds); err != nil {
		return "", err
	}
	return creds.APIKey, nil
}

// configBool 读取布尔型设置（缺省/解析失败用 fallback）。
func configBool(reader *configcenter.Center, key string, fallback bool) bool {
	raw, ok := reader.Get(key)
	if !ok {
		return fallback
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return fallback
	}
	return v
}

// configStringList 读取字符串数组型设置（缺省 → nil）。
func configStringList(reader *configcenter.Center, key string) []string {
	raw, ok := reader.Get(key)
	if !ok {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}
