// Package perfquery 把「命名查询白名单」翻译成 Prometheus HTTP 调用，
// 并负责超时、15 秒缓存与未接入时的降级。调用方（性能页 overview 端点）
// 只传查询名，PromQL 永远来自本文件的冻结目录，从不拼接用户输入。
//
// 为什么不用 Prometheus 客户端 SDK：它的查询部分是薄封装，本包只需要
// GET /api/v1/query_range + encoding/json 两件事，多一个依赖面不划算
// （计划 Global Constraints：不新增 Go 依赖）。
package perfquery

import "sort"

// 查询名常量。它们是 API 的稳定键（前端只传这些字符串），不得改名；
// 改名等于破坏 /api/performance/overview 契约。
const (
	NameGatewayRPS       = "gateway.rps"
	NameGatewayP95       = "gateway.p95"
	NameGatewayErrorRate = "gateway.error_rate"
	NameGatewayInflight  = "gateway.inflight"

	NameDBPool = "db.pool"
	NameDBWait = "db.wait"

	NameRedisUp      = "redis.up"
	NameRedisHitRate = "redis.hit_rate"
	NameRedisClients = "redis.clients"
	NameRedisMem     = "redis.mem"
	NameRedisCmdRate = "redis.cmd_rate"

	NameHostCPU    = "host.cpu"
	NameHostMem    = "host.mem"
	NameHostDisk   = "host.disk"
	NameHostNetIn  = "host.net_in"
	NameHostNetOut = "host.net_out"

	NamePGBackends    = "pg.backends"
	NamePGMaxBackends = "pg.max_backends"
	NamePGTPS         = "pg.tps"
	NamePGSize        = "pg.size"

	NameTargetUp = "target.up"

	// 按 instance 标签分组的逐副本查询（overview 的 gateways 区）。
	// 与上面的全局查询分开登记：它们的 PromQL 带 `by (instance)`，返回多条
	// 序列，调用方必须走 QueryRangeLabeled 而不是 QueryRange（后者会把多条
	// 序列摊平成一条，时间点交错）。
	NameGatewayByInstanceRPS        = "gateway.by_instance.rps"
	NameGatewayByInstanceP95        = "gateway.by_instance.p95"
	NameGatewayByInstanceInflight   = "gateway.by_instance.inflight"
	NameGatewayByInstanceGoroutines = "gateway.by_instance.goroutines"
	NameGatewayByInstanceHeapMB     = "gateway.by_instance.heap_mb"
	NameGatewayByInstanceErrorRate  = "gateway.by_instance.error_rate"
)

// entry 是一条命名查询：PromQL 与展示单位。
type entry struct {
	promQL string
	unit   string
}

// catalog 是冻结白名单。值里的正则竖线是 PromQL 语法（如 `tmpfs|overlay`），
// 不是 Markdown 表格的转义。
var catalog = map[string]entry{
	NameGatewayRPS: {
		promQL: `sum(rate(newtg_http_requests_total{service="gateway"}[1m]))`,
		unit:   "req/s",
	},
	NameGatewayP95: {
		promQL: `histogram_quantile(0.95, sum by (le) (rate(newtg_http_request_duration_seconds_bucket{service="gateway"}[5m])))`,
		unit:   "s",
	},
	NameGatewayErrorRate: {
		// 页面文案、DTO 注释与告警规则都叫「5xx」，查询就必须只数 5xx；
		// 曾误写成 `4xx|5xx`，让前端把「4xx+5xx 占比」标成 5xx。
		promQL: `sum(rate(newtg_http_requests_total{service="gateway",status_class="5xx"}[5m])) / clamp_min(sum(rate(newtg_http_requests_total{service="gateway"}[5m])), 1e-9)`,
		unit:   "ratio",
	},
	NameGatewayInflight: {
		promQL: `sum(newtg_http_in_flight_requests{service="gateway"})`,
		unit:   "conn",
	},
	NameDBPool: {
		promQL: `sum(newtg_db_pool_connections{db="main",state="in_use"})`,
		unit:   "conn",
	},
	NameDBWait: {
		promQL: `sum(rate(newtg_db_pool_wait_total{db="main"}[5m]))`,
		unit:   "wait/s",
	},
	NameRedisUp: {
		promQL: `min(newtg_redis_up)`,
		unit:   "bool",
	},
	NameRedisHitRate: {
		promQL: `sum(rate(redis_keyspace_hits_total[5m])) / clamp_min(sum(rate(redis_keyspace_hits_total[5m])) + sum(rate(redis_keyspace_misses_total[5m])), 1e-9)`,
		unit:   "ratio",
	},
	NameRedisClients: {
		promQL: `redis_connected_clients`,
		unit:   "conn",
	},
	NameRedisMem: {
		// 分母为 0（compose 默认 maxmemory=0，即不限制）时，比较运算
		// `> 0` 会过滤掉整个样本：页面显示 unknown，而不是把「已用字节数」
		// 当成比例判成 crit。clamp_min 会把 0 变成 1，反而保证误报。
		promQL: `redis_memory_used_bytes / (redis_memory_max_bytes > 0)`,
		unit:   "ratio",
	},
	NameRedisCmdRate: {
		promQL: `sum(rate(redis_commands_processed_total[5m]))`,
		unit:   "ops/s",
	},
	NameHostCPU: {
		// 外层 max 不可省：`avg by (instance)` 每个 target 返回一条序列，
		// 而 QueryRange 会把多条序列摊平成一条交错的时间轴（见文件头警告）。
		// 先按 instance 求平均、再跨实例取最大值，才是一条可消费的标量曲线。
		promQL: `max(1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])))`,
		unit:   "ratio",
	},
	NameHostMem: {
		// 与 host.cpu 同理：内存是逐 target 的序列，必须外层 max 收成标量。
		promQL: `max(1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)`,
		unit:   "ratio",
	},
	NameHostDisk: {
		// 保留 fstype/mountpoint 过滤，外层同样要 max 才能跨 target 收成标量。
		promQL: `max(1 - node_filesystem_avail_bytes{fstype!~"tmpfs|overlay",mountpoint="/"} / node_filesystem_size_bytes{fstype!~"tmpfs|overlay",mountpoint="/"})`,
		unit:   "ratio",
	},
	NameHostNetIn: {
		// 排除虚拟网卡：Docker 主机上同一份流量会同时记在 veth、docker0/br-*
		// 和物理网卡上，只筛 lo 会把流量重复累加数倍。
		promQL: `sum(rate(node_network_receive_bytes_total{device!~"lo|veth.*|docker.*|br-.*|virbr.*"}[5m]))`,
		unit:   "B/s",
	},
	NameHostNetOut: {
		// 与 host.net_in 同口径：必须排除虚拟网卡，否则出口流量被放大。
		promQL: `sum(rate(node_network_transmit_bytes_total{device!~"lo|veth.*|docker.*|br-.*|virbr.*"}[5m]))`,
		unit:   "B/s",
	},
	NamePGBackends: {
		promQL: `sum(pg_stat_database_numbackends)`,
		unit:   "conn",
	},
	NamePGMaxBackends: {
		// 每个 target 各有一条 pg_settings_max_connections，未经聚合会被
		// QueryRange 摊平成多条序列；max 取所有实例里最宽松的上限。
		promQL: `max(pg_settings_max_connections)`,
		unit:   "conn",
	},
	NamePGTPS: {
		promQL: `sum(rate(pg_stat_database_xact_commit[5m])) + sum(rate(pg_stat_database_xact_rollback[5m]))`,
		unit:   "tx/s",
	},
	NamePGSize: {
		promQL: `sum(pg_database_size_bytes)`,
		unit:   "B",
	},
	NameTargetUp: {
		promQL: `up{job=~"newtg-.*"}`,
		unit:   "bool",
	},
	NameGatewayByInstanceRPS: {
		promQL: `sum by (instance) (rate(newtg_http_requests_total{service="gateway"}[1m]))`,
		unit:   "req/s",
	},
	NameGatewayByInstanceP95: {
		promQL: `histogram_quantile(0.95, sum by (instance, le) (rate(newtg_http_request_duration_seconds_bucket{service="gateway"}[5m])))`,
		unit:   "s",
	},
	NameGatewayByInstanceInflight: {
		promQL: `sum by (instance) (newtg_http_in_flight_requests{service="gateway"})`,
		unit:   "conn",
	},
	NameGatewayByInstanceGoroutines: {
		// 不加 service 过滤：go_* 由每个进程的 /metrics 直接暴露，同一
		// instance 标签在所有 job 上一致（§7.0 的常量标签方案）。
		promQL: `max by (instance) (go_goroutines)`,
		unit:   "goroutines",
	},
	NameGatewayByInstanceHeapMB: {
		promQL: `max by (instance) (go_memstats_heap_alloc_bytes) / 1048576`,
		unit:   "MB",
	},
	NameGatewayByInstanceErrorRate: {
		// 与全局 gateway.error_rate 同口径：只数 5xx，否则副本错误率同样是
		// 「4xx+5xx」的冒充值。
		promQL: `sum by (instance) (rate(newtg_http_requests_total{service="gateway",status_class="5xx"}[5m])) / clamp_min(sum by (instance) (rate(newtg_http_requests_total{service="gateway"}[5m])), 1e-9)`,
		unit:   "ratio",
	},
}

// Names 返回白名单查询名的字母序副本。副本保证调用方无法改写目录。
func Names() []string {
	names := make([]string, 0, len(catalog))
	for name := range catalog {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Lookup 按查询名取 PromQL 与单位；未登记的名字返回 ok=false。
func Lookup(name string) (promQL, unit string, ok bool) {
	e, ok := catalog[name]
	if !ok {
		return "", "", false
	}
	return e.promQL, e.unit, true
}
