package metrics

import (
	"database/sql"
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// dbPoolCollector 把 database/sql 连接池的 Stats 转成指标。
//
// 为什么自己写 collector 而不采样到 gauge：Stats() 是纯内存快照，Collect
// 时读取零 SQL 往返；交给 Prometheus 每次抓取取值最准确，也不会引入后台
// goroutine 生命周期问题。
//
// db 标签区分 main/logs（platform-05 #37/#38 修订）：两库分治后只看总数
// 无法发现「日志库池耗尽拖垮 usage_logs 落库」。
type dbPoolCollector struct {
	mu    sync.RWMutex
	pools map[string]*sql.DB

	connections *prometheus.Desc
	waits       *prometheus.Desc
}

func newDBPoolCollector() *dbPoolCollector {
	return &dbPoolCollector{
		pools: make(map[string]*sql.DB),
		connections: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, "db", "pool_connections"),
			"Database connection pool connections by db and state.",
			[]string{"db", "state"}, nil,
		),
		waits: prometheus.NewDesc(
			prometheus.BuildFQName(Namespace, "db", "pool_wait_total"),
			"Total number of waits for a database connection by db.",
			[]string{"db"}, nil,
		),
	}
}

// attach 绑定连接池；nil 表示该库未装配（例如纯探针测试），跳过。
func (c *dbPoolCollector) attach(name string, db *sql.DB) {
	if db == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pools[name] = db
}

func (c *dbPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.connections
	ch <- c.waits
}

func (c *dbPoolCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for name, db := range c.pools {
		stats := db.Stats()
		ch <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue,
			float64(stats.OpenConnections), name, "open")
		ch <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue,
			float64(stats.InUse), name, "in_use")
		ch <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue,
			float64(stats.Idle), name, "idle")
		ch <- prometheus.MustNewConstMetric(c.waits, prometheus.CounterValue,
			float64(stats.WaitCount), name)
	}
}

// AttachDBPools 绑定主库/日志库连接池。
func (s *Set) AttachDBPools(main, logs *sql.DB) {
	s.dbPool.attach("main", main)
	s.dbPool.attach("logs", logs)
}

// itoa 是不引入 fmt 的小工具（热路径调用，避免反射式格式化开销）。
func itoa(v int) string { return strconv.Itoa(v) }

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }
