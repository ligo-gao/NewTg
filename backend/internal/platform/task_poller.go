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
	"newtg/gateway/internal/mj"
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

	// MJ 族独立的轮询上限（relay-05 §6.1：MJ 卡住兜底 1h，插件 1440m，
	// 两族故意不同）。共用 task.poll_* 会让调 MJ 连带改变插件任务。
	settingMJPollTimeoutMinutes = "mj.poll_timeout_minutes"
	settingMJPollMaxFailures    = "mj.poll_max_failures"

	// settingMJRetentionDays 是终态 MJ 任务的保留天数（#70）。
	settingMJRetentionDays = "mj.retention_terminal_days"
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
	// W8/MJ-b：MJ 族的查询驱动与补偿结算。Handler 负责「查上游 → CAS 回填 →
	// 终态交给 Settler」，Settler 同时作为 MJPoller 的补偿扫描入口——两者
	// 必须是同一个实例，否则「终态结算」与「pending 重试」会走两套资金
	// 语义。
	taskRepo := tasks.NewRepo(db)
	mjDriver, mjSettler, err := buildMJPollDriver(taskRepo, center, engine, sealer, log)
	if err != nil {
		return nil, err
	}
	// W9-B4：两个轮询器由同一构造函数装配，配置上限只在该函数内解析一次
	// 并同时注入两侧；接线本身有单测（buildTaskPollers 的 accessor 断言），
	// 删除任一侧的赋值都会让用例变红。
	poller, mjPoller, err := buildTaskPollers(taskPollerDeps{
		Repo:      taskRepo,
		Plugins:   manager,
		Billing:   engine,
		Decrypt:   func(sealed []byte) (string, error) { return decryptTaskChannelCredential(sealer, sealed) },
		Limits:    center,
		Log:       log,
		MJHandler: mjDriver.PollTask,
		MJSettler: mjSettler,
		MJRetention: func(now time.Time) *time.Time {
			return httpapi.MJRetentionDeadline(now, mjRetentionDays(center, log))
		},
	})
	if err != nil {
		return nil, err
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

// taskPollerDeps 是 buildTaskPollers 的装配输入。
//
// Limits 是设置读取面（生产 *configcenter.Center，测试用 fake）；把依赖
// 收进结构体是为了让接线单测只替换存储/计费/插件，而不复制生产构造路径。
type taskPollerDeps struct {
	Repo    tasks.PendingRepo
	Plugins tasks.PluginSnapshotter
	Billing tasks.BillingHandoff
	Decrypt func([]byte) (string, error)
	Limits  settingReader
	Log     *slog.Logger
	// MJHandler 是 MJ 族的查询/结算驱动（MJ-b 接入点）。nil 时 MJPoller
	// 只领取、不推进/不结算；mj.enabled=true 时装配守卫会拒绝上线
	// （见 mjSettlerGuard），避免普通提交的 hold 永久冻结。
	MJHandler tasks.MJTaskHandler
	// MJSettler 是 MJ 族的补偿结算入口；与 MJHandler 的内部结算共用同一个
	// tasks.Settler 实例。
	MJSettler tasks.MJTaskSettler
	// MJRetention 计算 MJ 终态保留截止（超时/失败上限推进的终态也要写）。
	MJRetention func(now time.Time) *time.Time
}

// mjSettlerGuard 是「绘图族已开启但无人结算」的装配门禁（I2）。
//
// 为什么必须 fail-closed：普通 MJ 提交在落库后 Detach（停心跳、移除 Redis
// 在飞租约），hold 只等 MJ 侧终态结算/退款。MJ-b 之前 MJPoller 的 Handler
// 为 nil：既不轮询也不结算，兜底扫描又只认 Redis 在飞会话与插件族 pending
// 行，于是每一笔成功提交的预扣都会永久冻结。允许 mj.enabled=true 等于
// 静默把用户余额锁死，所以宁可拒绝进程上线。
//
// handler 非 nil = 调用方已接管 MJ 推进/结算；此时才允许开启。
func mjSettlerGuard(reader settingReader, handler tasks.MJTaskHandler) error {
	if handler != nil || !settingBool(reader, "mj.enabled") {
		return nil
	}
	return fmt.Errorf("mj.enabled is true but no MJ settlement handler is wired (MJ-b); refusing to start")
}

// settingBool 读取布尔设置；缺失/解析失败按 false（fail-closed）。
func settingBool(reader settingReader, key string) bool {
	if reader == nil {
		return false
	}
	raw, ok := reader.Get(key)
	if !ok {
		return false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	return value
}

// buildTaskPollers 用一个解析结果装配插件 poller 与 MJ poller（W9-B4）。
//
// 这是「配置 → 两个轮询器」的唯一路径：startTaskPoller 与接线单测都调用
// 它，因此删除任一侧的 Timeout/MaxFailures 赋值都会让接线用例变红，而不是
// 只在集成环境里静默回落默认值。
func buildTaskPollers(deps taskPollerDeps) (*tasks.Poller, *tasks.MJPoller, error) {
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	// I2：mj.enabled=true 但没有 MJ 结算驱动时拒绝上线（hold 会永久冻结）。
	if err := mjSettlerGuard(deps.Limits, deps.MJHandler); err != nil {
		return nil, nil, fmt.Errorf("worker: %w", err)
	}
	// 指针语义保证「未配置」与「显式 0=禁用」不会被混为一谈。
	// 两族分别解析：task.* 只约束插件 poller，mj.* 只约束 MJ poller。
	timeout, maxFailures := taskPollLimits(deps.Limits, log)
	mjTimeout, mjMaxFailures := mjPollLimits(deps.Limits, log)
	log.Info("worker: task poll limits resolved",
		"plugin_timeout", describePollTimeout(timeout, tasks.DefaultTaskTimeout),
		"plugin_max_failures", describeMaxFailures(maxFailures),
		"mj_timeout", describePollTimeout(mjTimeout, tasks.DefaultMJTaskTimeout),
		"mj_max_failures", describeMaxFailures(mjMaxFailures))

	poller, err := tasks.NewPoller(tasks.PollerConfig{
		Repo:    deps.Repo,
		Plugins: deps.Plugins,
		Billing: deps.Billing,
		Decrypt: deps.Decrypt,
		Logger:  log,
		// nil 时 NewPoller 回落设计默认（1440m/20）。
		Timeout:     timeout,
		MaxFailures: maxFailures,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("worker: task poller: %w", err)
	}
	mjPoller, err := tasks.NewMJPoller(tasks.MJPollerConfig{
		Repo:    deps.Repo,
		Handler: deps.MJHandler,
		Settler: deps.MJSettler,
		// B2：MJ 行若来自订阅来源，Detach 后同样需要续租，否则兜底扫描会把
		// 仍在轮询的任务当成崩溃请求退款（与插件 poller 同一份交接面）。
		Billing: deps.Billing,
		Logger:  log,
		// MJ 专属上限（mj.poll_*，默认 60m/20）。Handler 为空时它不会因
		// 超时改状态（A0 冻结语义）；mj.enabled=true 的空 Handler 已被上面的
		// 守卫拒绝。
		Timeout:     mjTimeout,
		MaxFailures: mjMaxFailures,
		// 终态保留期限与 MJ 驱动使用同一份设置（mj.retention_terminal_days）。
		RetentionDeadline: deps.MJRetention,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("worker: mj task poller: %w", err)
	}
	return poller, mjPoller, nil
}

// mjPollRepo 是 buildMJPollDriver 的存储接缝（生产 *tasks.Repo）。
//
// 用接口而不是具体类型，让装配测试可以注入内存 fake 验证「driver/settler
// 真的接上了」；生产仍只有 tasks.Repo 一个实现。
type mjPollRepo interface {
	httpapi.MJPollStore
	httpapi.MJChannelSource
	tasks.BillingFlagStore
}

// buildMJPollDriver 构造 MJ 族的查询/结算驱动（W8/MJ-b）。
//
// gateway 与 worker 都调用同一构造函数：gateway 只用它通过启动守卫
// （证明「mj.enabled 打开时存在结算驱动」），worker 才真正运行它。把构造
// 收进一个函数避免两侧各写一份依赖清单——漏一个依赖会让守卫通过但轮询
// 静默不工作，正是守卫要防的故障。
func buildMJPollDriver(repo mjPollRepo, reader settingReader, engine tasks.BillingSettler,
	sealer *httpapi.SecretSealer, log *slog.Logger) (*httpapi.MJPollDriver, *tasks.Settler, error) {

	settler, err := tasks.NewSettler(tasks.SettlerConfig{
		Billing: engine, Repo: repo, Logger: log,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("worker: mj settler: %w", err)
	}
	driver, err := httpapi.NewMJPollDriver(httpapi.MJPollDriverConfig{
		Store:    repo,
		Channels: repo,
		Sealer:   sealer,
		// 固定形状的 midjourney-proxy 客户端：禁重定向、mj-api-secret、
		// 查询 15s（SubmitClient 的默认超时会按调用类型分别收窄）。
		Client:  mj.NewSubmitClient(nil),
		Settler: settler,
		Logger:  log,
		RetentionDays: func() int64 {
			return mjRetentionDays(reader, log)
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("worker: mj poll driver: %w", err)
	}
	return driver, settler, nil
}

// mjRetentionDays 读取终态保留天数；未配置/脏值回落设计默认 30。
//
// 0 是显式关闭（不写 retention_deadline），与「未配置」必须区分。
func mjRetentionDays(reader settingReader, log *slog.Logger) int64 {
	if days, ok := readSettingInt(reader, settingMJRetentionDays, log); ok {
		return days
	}
	return 30
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

// taskPollLimits 把设置中心的插件族轮询上限映射成轮询器配置（W9-B4）。
//
// 三态冻结（R-W9B-04）：
//   - 键未落库（Center.Get ok=false）→ 返回 nil：交给 NewPoller 回落插件族
//     设计默认（1440 分钟 / 20 次）。**不得**在这里填 0，也不得填默认值，
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
	return pollLimits(reader, settingTaskPollTimeoutMinutes, settingTaskPollMaxFailures, log)
}

// mjPollLimits 解析 MJ 族专属上限；未配置时返回 nil，由 NewMJPoller 回落
// 60m/20（relay-05 §6.1）。与 taskPollLimits 完全独立：改一侧不影响另一侧。
func mjPollLimits(reader settingReader, log *slog.Logger) (timeout *time.Duration, maxFailures *int) {
	return pollLimits(reader, settingMJPollTimeoutMinutes, settingMJPollMaxFailures, log)
}

// pollLimits 是两族上限的共享解析实现（键名与默认值由调用方决定）。
func pollLimits(reader settingReader, timeoutKey, failuresKey string, log *slog.Logger) (timeout *time.Duration, maxFailures *int) {
	if log == nil {
		log = slog.Default()
	}
	if minutes, ok := readSettingInt(reader, timeoutKey, log); ok {
		// 分钟 → Duration 的溢出护栏：注册表本身限制 525600 分钟，但
		// configcenter 里的历史行/手工写入不受注册表校验，仍需防御。
		if minutes > math.MaxInt64/int64(time.Minute) {
			log.Warn("worker: task poll timeout is out of range; falling back to the design default",
				"key", timeoutKey, "minutes", minutes)
		} else {
			d := time.Duration(minutes) * time.Minute
			timeout = &d
		}
	}
	if failures, ok := readSettingInt(reader, failuresKey, log); ok {
		if failures > int64(math.MaxInt) {
			log.Warn("worker: task poll max failures overflows int; falling back to the design default",
				"key", failuresKey, "max_failures", failures)
		} else {
			v := int(failures)
			maxFailures = &v
		}
	}
	return timeout, maxFailures
}

// readSettingInt 读取一个非负整数设置。
//
// 返回 ok=false 覆盖四种情况：未配置、JSON null、解析失败、负值。调用方
// 因此只需区分「有合法值」与「回落默认」；具体的告警在这里发出，便于运维
// 定位脏行。
//
// 为什么用 *int64：json.Unmarshal 把 `null` 解到 int64 时返回 nil error 且
// 保持零值，会被误读成「显式 0 = 禁用」——这是 fail-open（静默关掉兜底）。
// 解到指针后 null → nil 指针，明确落入「回落默认 + warn」。
func readSettingInt(reader settingReader, key string, log *slog.Logger) (int64, bool) {
	if reader == nil {
		return 0, false
	}
	raw, ok := reader.Get(key)
	if !ok {
		return 0, false
	}
	var value *int64
	if err := json.Unmarshal(raw, &value); err != nil {
		log.Warn("worker: setting is not a valid integer; falling back to the design default",
			"key", key, "raw", string(raw), "error", err)
		return 0, false
	}
	if value == nil {
		log.Warn("worker: setting is null; falling back to the design default",
			"key", key, "raw", string(raw))
		return 0, false
	}
	if *value < 0 {
		// 设计只冻结了 `<=0 禁用`：0 是唯一的禁用形式。负值是配置错误，
		// 不是另一种禁用写法（见 taskPollLimits 的注释）。
		log.Warn("worker: setting is negative; falling back to the design default",
			"key", key, "value", *value)
		return 0, false
	}
	return *value, true
}

// describePollTimeout 只服务于启动日志的可观测性：nil 显示为对应族的
// 设计默认（插件 1440m / MJ 60m），避免运维把「未配置」误读成「0=禁用」。
func describePollTimeout(timeout *time.Duration, fallback time.Duration) string {
	if timeout == nil {
		return fmt.Sprintf("default (%s)", fallback)
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
