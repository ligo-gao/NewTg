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

// TestCatalogEntriesUsable 只钉「集合 + 可用性」：每条 PromQL 非空、不是
// fmt 占位（不得出现 %）、单位非空且无首尾空白。具体 PromQL 文本由
// catalog.go 与计划表格逐条人工核对，避免测试与实现互相抄写后一起漂移。
func TestCatalogEntriesUsable(t *testing.T) {
	if len(wantNames) != 21 {
		t.Fatalf("冻结白名单应为 21 条，实际 %d", len(wantNames))
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
