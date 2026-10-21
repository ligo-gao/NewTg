package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"time"

	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/config"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/httpapi"
	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/store"
	"newtg/gateway/internal/tasks"
	"newtg/gateway/plugins"
)

// 任务轮询上限的设置键（W9-B4 / G-M6D-27，设计回写见 settings 注册表的
// `task` section）。设计原文的 TASK_TIMEOUT_MINUTES / TASK_POLL_MAX_FAILURES
// 是基线环境变量；新系统以 system_settings 为唯一可运行配置面，故落在
// `task.*` 命名空间，语义一一对应：
//
//	task.poll_timeout_minutes ↔ TASK_TIMEOUT_MINUTES（默认 1440）
//	task.poll_max_failures    ↔ TASK_POLL_MAX_FAILURES（默认 20）
const (
	settingTaskPollTimeoutMinutes = "task.poll_timeout_minutes"
	settingTaskPollMaxFailures    = "task.poll_max_failures"
)

// settingReader 是配置中心读取面的最小接口（生产 *configcenter.Center）。
// 抽出接口让三态映射可以在不依赖 PG/Redis 的单测里验证（R-W9B-08）。
type settingReader interface {
	Get(key string) (json.RawMessage, bool)
}

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
	// W9-B4 / R-W9B-04：把运营配置真正传进两个轮询器。指针语义保证
	// 「未配置」与「显式 0=禁用」不会被混为一谈。
	timeout, maxFailures := taskPollLimits(center, log)
	log.Info("worker: task poll limits resolved",
		"timeout", describeTimeout(timeout), "max_failures", describeMaxFailures(maxFailures))
	poller, err := tasks.NewPoller(tasks.PollerConfig{
		Repo:    tasks.NewRepo(db),
		Plugins: manager,
		Billing: engine,
		Decrypt: func(sealed []byte) (string, error) {
			return decryptTaskChannelCredential(sealer, sealed)
		},
		Logger: log,
		// Timeout/MaxFailures 为 nil 时回落设计默认（1440m/20）。
		Timeout:     timeout,
		MaxFailures: maxFailures,
	})
	if err != nil {
		return nil, fmt.Errorf("worker: task poller: %w", err)
	}

	// W8/MJ-A0：MJ 族有独立的领取入口（family='mj'）。A0 只装配领取骨架
	// （Handler 为 nil：只领取、不推进/不结算）；MJ-b 在这里注入查询驱动，
	// MJ-a2/d 注入计费时机。先装配的价值是让「MJ 行有唯一归属的 poller」
	// 在数据面上成立，而不是只依赖插件 poller 的 SQL 过滤（R-MJ-04 纵深防御）。
	mjPoller, err := tasks.NewMJPoller(tasks.MJPollerConfig{
		Repo:   tasks.NewRepo(db),
		Logger: log,
		// 同一对配置同时约束 MJ 领取器（W9-B4）。Handler 为空时它仍不会
		// 因超时改状态——A0 冻结语义，见 tasks/mj_poller.go。
		Timeout:     timeout,
		MaxFailures: maxFailures,
	})
	if err != nil {
		return nil, fmt.Errorf("worker: mj task poller: %w", err)
	}

	pollCtx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	mjDone := make(chan struct{})
	go func() {
		defer close(done)
		if err := poller.Run(pollCtx); err != nil {
			log.Error("worker: task poller exited with error", "error", err)
		}
	}()
	go func() {
		defer close(mjDone)
		if err := mjPoller.Run(pollCtx); err != nil {
			log.Error("worker: mj task poller exited with error", "error", err)
		}
	}()
	return func() {
		cancel()
		<-done
		<-mjDone
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

// taskPollLimits 把设置中心的两个任务轮询上限映射成轮询器配置（W9-B4）。
//
// 三态冻结（R-W9B-04）：
//   - 键未落库（Center.Get ok=false）→ 返回 nil：交给 NewPoller/NewMJPoller
//     回落设计默认（1440 分钟 / 20 次）。**不得**在这里填 0，也不得填默认值，
//     否则「未配置」与「显式 0」在轮询器里就再也分不开。
//   - 显式 0 → 返回指向 0 的指针：timedOut / maxFailures 判定据此禁用。
//   - 正值 → 按值透传（分钟换算为 time.Duration）。
//
// 坏值/负值不是「禁用」：负的上限没有设计含义，静默当成 0 会让运营以为
// 兜底还在生效。这里统一回落默认（返回 nil）并 warn，绝不 panic——worker
// 不能因为一条脏设置就拒绝上线（否则任务预扣会永久冻结）。
//
// 生效时机：本函数只在 startTaskPoller 启动时读一次快照，改动在 worker
// 重建后生效（与「启动期装配」一致）；运行中热更新需要把读取下沉到轮询
// 循环，本波不做。
func taskPollLimits(reader settingReader, log *slog.Logger) (timeout *time.Duration, maxFailures *int) {
	if log == nil {
		log = slog.Default()
	}
	if minutes, ok := readSettingInt(reader, settingTaskPollTimeoutMinutes, log); ok {
		// 分钟 → Duration 的溢出护栏：注册表本身限制 525600 分钟，但
		// configcenter 里的历史行/手工写入不受注册表校验，仍需防御。
		if minutes > math.MaxInt64/int64(time.Minute) {
			log.Warn("worker: task poll timeout is out of range; falling back to the design default",
				"key", settingTaskPollTimeoutMinutes, "minutes", minutes)
		} else {
			d := time.Duration(minutes) * time.Minute
			timeout = &d
		}
	}
	if failures, ok := readSettingInt(reader, settingTaskPollMaxFailures, log); ok {
		if failures > int64(math.MaxInt) {
			log.Warn("worker: task poll max failures overflows int; falling back to the design default",
				"key", settingTaskPollMaxFailures, "max_failures", failures)
		} else {
			v := int(failures)
			maxFailures = &v
		}
	}
	return timeout, maxFailures
}

// readSettingInt 读取一个非负整数设置。
//
// 返回 ok=false 覆盖三种情况：未配置、解析失败、负值。调用方因此只需区分
// 「有合法值」与「回落默认」；具体的告警在这里发出，便于运维定位脏行。
func readSettingInt(reader settingReader, key string, log *slog.Logger) (int64, bool) {
	if reader == nil {
		return 0, false
	}
	raw, ok := reader.Get(key)
	if !ok {
		return 0, false
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		log.Warn("worker: task poll setting is not a valid integer; falling back to the design default",
			"key", key, "raw", string(raw), "error", err)
		return 0, false
	}
	if value < 0 {
		// 设计只冻结了 `<=0 禁用`：0 是唯一的禁用形式。负值是配置错误，
		// 不是另一种禁用写法（见 taskPollLimits 的注释）。
		log.Warn("worker: task poll setting is negative; falling back to the design default",
			"key", key, "value", value)
		return 0, false
	}
	return value, true
}

// describeTimeout / describeMaxFailures 只服务于启动日志的可观测性：
// nil 明确显示为 default，避免运维把「未配置」误读成「0=禁用」。
func describeTimeout(timeout *time.Duration) string {
	if timeout == nil {
		return fmt.Sprintf("default (%s)", tasks.DefaultTaskTimeout)
	}
	return timeout.String()
}

func describeMaxFailures(maxFailures *int) string {
	if maxFailures == nil {
		return fmt.Sprintf("default (%d)", tasks.DefaultMaxPollFailures)
	}
	return fmt.Sprintf("%d", *maxFailures)
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
