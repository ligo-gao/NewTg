package perfquery

import (
	"reflect"
	"strings"
	"testing"
)

// wantNames 是白名单的冻结集合（按字母序）。顺序即 Names() 的契约：
// 调用方（overview 端点、目录测试）按稳定顺序遍历，避免 map 迭代顺序漂移。
var wantNames = []string{
	"db.pool",
	"db.wait",
	"gateway.by_instance.error_rate",
	"gateway.by_instance.goroutines",
	"gateway.by_instance.heap_mb",
	"gateway.by_instance.inflight",
	"gateway.by_instance.p95",
	"gateway.by_instance.rps",
	"gateway.error_rate",
	"gateway.inflight",
	"gateway.p95",
	"gateway.rps",
	"host.cpu",
	"host.disk",
	"host.mem",
	"host.net_in",
	"host.net_out",
	"pg.backends",
	"pg.max_backends",
	"pg.size",
	"pg.tps",
	"redis.clients",
	"redis.cmd_rate",
	"redis.hit_rate",
	"redis.mem",
	"redis.up",
	"target.up",
}

func TestNamesExactSet(t *testing.T) {
	got := Names()
	if !reflect.DeepEqual(got, wantNames) {
		t.Fatalf("Names() = %v, want %v", got, wantNames)
	}
}

func TestNamesReturnsCopy(t *testing.T) {
	first := Names()
	if len(first) == 0 {
		t.Fatal("Names() 为空")
	}
	first[0] = "mutated"
	second := Names()
	if second[0] != wantNames[0] {
		t.Fatalf("Names() 返回了内部切片：第二次首项 = %q", second[0])
	}
}

// catalogGolden 冻结每条查询的 PromQL 原文（27 条，与 wantNames 一一对应）。
// 改这里的任何一行都必须同时更新设计说明/实施计划里的查询表：本表是
// review 的回归闸门——host.*/pg.max_backends 的标量聚合、网络设备过滤、
// 5xx 口径都靠它钉住，避免实现与测试互相抄写后一起悄悄漂移。
var catalogGolden = map[string]string{
	"db.pool":                        `sum(newtg_db_pool_connections{db="main",state="in_use"})`,
	"db.wait":                        `sum(rate(newtg_db_pool_wait_total{db="main"}[5m]))`,
	"gateway.by_instance.error_rate": `sum by (instance) (rate(newtg_http_requests_total{service="gateway",status_class="5xx"}[5m])) / clamp_min(sum by (instance) (rate(newtg_http_requests_total{service="gateway"}[5m])), 1e-9)`,
	"gateway.by_instance.goroutines": `max by (instance) (go_goroutines)`,
	"gateway.by_instance.heap_mb":    `max by (instance) (go_memstats_heap_alloc_bytes) / 1048576`,
	"gateway.by_instance.inflight":   `sum by (instance) (newtg_http_in_flight_requests{service="gateway"})`,
	"gateway.by_instance.p95":        `histogram_quantile(0.95, sum by (instance, le) (rate(newtg_http_request_duration_seconds_bucket{service="gateway"}[5m])))`,
	"gateway.by_instance.rps":        `sum by (instance) (rate(newtg_http_requests_total{service="gateway"}[1m]))`,
	"gateway.error_rate":             `sum(rate(newtg_http_requests_total{service="gateway",status_class="5xx"}[5m])) / clamp_min(sum(rate(newtg_http_requests_total{service="gateway"}[5m])), 1e-9)`,
	"gateway.inflight":               `sum(newtg_http_in_flight_requests{service="gateway"})`,
	"gateway.p95":                    `histogram_quantile(0.95, sum by (le) (rate(newtg_http_request_duration_seconds_bucket{service="gateway"}[5m])))`,
	"gateway.rps":                    `sum(rate(newtg_http_requests_total{service="gateway"}[1m]))`,
	"host.cpu":                       `max(1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])))`,
	"host.disk":                      `max(1 - node_filesystem_avail_bytes{fstype!~"tmpfs|overlay",mountpoint="/"} / node_filesystem_size_bytes{fstype!~"tmpfs|overlay",mountpoint="/"})`,
	"host.mem":                       `max(1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)`,
	"host.net_in":                    `sum(rate(node_network_receive_bytes_total{device!~"lo|veth.*|docker.*|br-.*|virbr.*"}[5m]))`,
	"host.net_out":                   `sum(rate(node_network_transmit_bytes_total{device!~"lo|veth.*|docker.*|br-.*|virbr.*"}[5m]))`,
	"pg.backends":                    `sum(pg_stat_database_numbackends)`,
	"pg.max_backends":                `max(pg_settings_max_connections)`,
	"pg.size":                        `sum(pg_database_size_bytes)`,
	"pg.tps":                         `sum(rate(pg_stat_database_xact_commit[5m])) + sum(rate(pg_stat_database_xact_rollback[5m]))`,
	"redis.clients":                  `redis_connected_clients`,
	"redis.cmd_rate":                 `sum(rate(redis_commands_processed_total[5m]))`,
	"redis.hit_rate":                 `sum(rate(redis_keyspace_hits_total[5m])) / clamp_min(sum(rate(redis_keyspace_hits_total[5m])) + sum(rate(redis_keyspace_misses_total[5m])), 1e-9)`,
	"redis.mem":                      `redis_memory_used_bytes / (redis_memory_max_bytes > 0)`,
	"redis.up":                       `min(newtg_redis_up)`,
	"target.up":                      `up{job=~"newtg-.*"}`,
}

// TestCatalogPromQLFrozen 按名字逐条比对 PromQL 原文：任何查询改动都必须
// 显式改 golden 并同步文档，不允许「顺手优化」悄悄上线。
func TestCatalogPromQLFrozen(t *testing.T) {
	if len(catalogGolden) != len(wantNames) {
		t.Fatalf("golden 表 %d 条，白名单 %d 条", len(catalogGolden), len(wantNames))
	}
	for _, name := range wantNames {
		want, ok := catalogGolden[name]
		if !ok {
			t.Errorf("golden 缺少查询 %q", name)
			continue
		}
		got, _, ok := Lookup(name)
		if !ok {
			t.Errorf("Lookup(%q) 未登记", name)
			continue
		}
		if got != want {
			t.Errorf("%s: PromQL = %q, want %q", name, got, want)
		}
	}
}

// TestScalarQueriesAggregateAcrossTargets 钉住 2 号评审发现：host.cpu/mem/disk
// 与 pg.max_backends 会按 target 返回多条序列，而调用方走标量 QueryRange，
// 必须在 PromQL 里用外层 max() 收成一条，否则时间轴会被摊平交错。
func TestScalarQueriesAggregateAcrossTargets(t *testing.T) {
	for _, name := range []string{NameHostCPU, NameHostMem, NameHostDisk, NamePGMaxBackends} {
		promQL, _, ok := Lookup(name)
		if !ok {
			t.Errorf("Lookup(%q) 未登记", name)
			continue
		}
		if !strings.HasPrefix(promQL, "max(") || !strings.HasSuffix(promQL, ")") {
			t.Errorf("%s: 标量查询必须被外层 max() 包裹: %q", name, promQL)
		}
	}

	// 外层聚合不能以丢过滤条件为代价：磁盘仍限根分区与真实文件系统。
	disk, _, _ := Lookup(NameHostDisk)
	for _, want := range []string{`fstype!~"tmpfs|overlay"`, `mountpoint="/"`} {
		if !strings.Contains(disk, want) {
			t.Errorf("%s: 缺少过滤条件 %s: %q", NameHostDisk, want, disk)
		}
	}
	cpu, _, _ := Lookup(NameHostCPU)
	if !strings.Contains(cpu, "avg by (instance)") {
		t.Errorf("%s: 外层 max 之前应保留按 instance 求平均: %q", NameHostCPU, cpu)
	}
}

// TestNetworkCountersExcludeVirtualDevices 钉住 3 号评审发现：Docker 主机上
// veth/docker0/br-* 会与物理网卡重复计数，两边都必须排除。
func TestNetworkCountersExcludeVirtualDevices(t *testing.T) {
	const want = `device!~"lo|veth.*|docker.*|br-.*|virbr.*"`
	for _, name := range []string{NameHostNetIn, NameHostNetOut} {
		promQL, _, _ := Lookup(name)
		if !strings.Contains(promQL, want) {
			t.Errorf("%s: 未排除虚拟网卡 %s: %q", name, want, promQL)
		}
	}
}

// TestCatalogEntriesUsable 钉「集合 + 可用性」：每条 PromQL 非空、不是
// fmt 占位（不得出现 %）、单位非空且无首尾空白。具体 PromQL 文本由上面的
// catalogGolden 逐条冻结。
func TestCatalogEntriesUsable(t *testing.T) {
	if len(wantNames) != 27 {
		t.Fatalf("冻结白名单应为 27 条，实际 %d", len(wantNames))
	}
	for _, name := range wantNames {
		promQL, unit, ok := Lookup(name)
		if !ok {
			t.Errorf("Lookup(%q) 未登记", name)
			continue
		}
		if strings.TrimSpace(promQL) == "" {
			t.Errorf("%s: PromQL 为空", name)
		}
		if strings.Contains(promQL, "%") {
			t.Errorf("%s: PromQL 含 %% 占位: %q", name, promQL)
		}
		if strings.TrimSpace(unit) == "" {
			t.Errorf("%s: 单位为空", name)
		}
		if unit != strings.TrimSpace(unit) {
			t.Errorf("%s: 单位含首尾空白: %q", name, unit)
		}
	}
}

// TestByInstanceCatalogGrouped 钉住逐副本查询的分类：六条 by_instance 查询
// 必须按 instance 聚合（调用方走 QueryRangeLabeled），而 gateway 的四条全局
// 查询不得带 instance 维度（它们走 QueryRange，会把多条序列摊平）。
func TestByInstanceCatalogGrouped(t *testing.T) {
	byInstance := []string{
		NameGatewayByInstanceRPS,
		NameGatewayByInstanceP95,
		NameGatewayByInstanceInflight,
		NameGatewayByInstanceGoroutines,
		NameGatewayByInstanceHeapMB,
		NameGatewayByInstanceErrorRate,
	}
	for _, name := range byInstance {
		promQL, _, ok := Lookup(name)
		if !ok {
			t.Errorf("Lookup(%q) 未登记", name)
			continue
		}
		if !strings.Contains(promQL, "by (instance") {
			t.Errorf("%s: PromQL 未按 instance 聚合: %q", name, promQL)
		}
	}
	for _, name := range []string{NameGatewayRPS, NameGatewayP95, NameGatewayInflight, NameGatewayErrorRate} {
		promQL, _, ok := Lookup(name)
		if !ok {
			t.Errorf("Lookup(%q) 未登记", name)
			continue
		}
		if strings.Contains(promQL, "by (instance") {
			t.Errorf("%s: 全局查询不应带 instance 维度: %q", name, promQL)
		}
	}
}

func TestLookupUnknown(t *testing.T) {
	if promQL, unit, ok := Lookup("gateway.unknown"); ok || promQL != "" || unit != "" {
		t.Fatalf("Lookup(unknown) = (%q, %q, %v), want (\"\", \"\", false)", promQL, unit, ok)
	}
	if _, _, ok := Lookup(""); ok {
		t.Fatal("Lookup(\"\") 应为 false")
	}
}

// TestNameConstants 钉住常量与查询名的对应，防止改名时只改常量值或只改
// catalog 键。
func TestNameConstants(t *testing.T) {
	constants := map[string]string{
		NameGatewayRPS:       "gateway.rps",
		NameGatewayP95:       "gateway.p95",
		NameGatewayErrorRate: "gateway.error_rate",
		NameGatewayInflight:  "gateway.inflight",
		NameDBPool:           "db.pool",
		NameDBWait:           "db.wait",
		NameRedisUp:          "redis.up",
		NameRedisHitRate:     "redis.hit_rate",
		NameRedisClients:     "redis.clients",
		NameRedisMem:         "redis.mem",
		NameRedisCmdRate:     "redis.cmd_rate",
		NameHostCPU:          "host.cpu",
		NameHostMem:          "host.mem",
		NameHostDisk:         "host.disk",
		NameHostNetIn:        "host.net_in",
		NameHostNetOut:       "host.net_out",
		NamePGBackends:       "pg.backends",
		NamePGMaxBackends:    "pg.max_backends",
		NamePGTPS:            "pg.tps",
		NamePGSize:           "pg.size",
		NameTargetUp:         "target.up",

		NameGatewayByInstanceRPS:        "gateway.by_instance.rps",
		NameGatewayByInstanceP95:        "gateway.by_instance.p95",
		NameGatewayByInstanceInflight:   "gateway.by_instance.inflight",
		NameGatewayByInstanceGoroutines: "gateway.by_instance.goroutines",
		NameGatewayByInstanceHeapMB:     "gateway.by_instance.heap_mb",
		NameGatewayByInstanceErrorRate:  "gateway.by_instance.error_rate",
	}
	if len(constants) != len(wantNames) {
		t.Fatalf("常量数 %d，白名单 %d", len(constants), len(wantNames))
	}
	for constant, name := range constants {
		if constant != name {
			t.Errorf("常量值 = %q, want %q", constant, name)
		}
		if _, _, ok := Lookup(name); !ok {
			t.Errorf("常量 %q 未登记进 catalog", name)
		}
	}
}
