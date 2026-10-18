package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/version"
)

// 探测超时（本计划冻结）：R1 SELECT 1 / R2 PING 各 1s。
const probeTimeout = time.Second

// errProbeDown：依赖缺失（NewChecker 收到 nil）时的恒失败原因。
var errProbeDown = errors.New("probe dependency not configured")

// dbProbe / redisProbe 是 Checker 字段的窄接口（R-03 裁决：NewChecker 收具体类型，
// 内部字段为窄接口，测试直注 stub 即可替换）。
type dbProbe interface {
	// selectOne 对应 R1：SELECT 1。
	selectOne(ctx context.Context) error
	// migrationState 对应 R3：读 schema_migrations 的 (version, dirty)。
	migrationState(ctx context.Context) (version int, dirty bool, err error)
}

type redisProbe interface {
	// ping 对应 R2：PING。
	ping(ctx context.Context) error
}

// Checker 是 /readyz 五项判定的执行者。每次请求实时判定，不缓存（防误判优先，本计划冻结）。
type Checker struct {
	db                dbProbe
	rdb               redisProbe
	migrationExpected int // M0 冻结期望值=1
	log               *slog.Logger
	node              string // cfg.NodeName（装配时由 RunGateway 写入）

	configLoaded atomic.Bool // R4：配置/装配完成标志
	startupDone  atomic.Bool // R5：HTTP server 开始服务标志
}

// NewChecker 收具体类型（*sql.DB / *redis.Client），内部以适配器包装为窄接口；
// nil 依赖由适配器降级为恒失败，调用方仍可安全挂路由。log 为 nil 时退化为 slog.Default()。
func NewChecker(db *sql.DB, rdb *redis.Client, migrationExpected int, log *slog.Logger) *Checker {
	if log == nil {
		log = slog.Default()
	}
	var dp dbProbe = sqlDBProbe{db: db}
	if db == nil {
		dp = downDBProbe{}
	}
	var rp redisProbe = redisClientProbe{rdb: rdb}
	if rdb == nil {
		rp = downRedisProbe{}
	}
	return &Checker{
		db:                dp,
		rdb:               rp,
		migrationExpected: migrationExpected,
		log:               log,
	}
}

// sqlDBProbe 让 *sql.DB 满足 dbProbe（读主库，不使用从库）。
type sqlDBProbe struct{ db *sql.DB }

func (p sqlDBProbe) selectOne(ctx context.Context) error {
	var one int
	return p.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

func (p sqlDBProbe) migrationState(ctx context.Context) (int, bool, error) {
	var (
		v     int
		dirty bool
	)
	err := p.db.QueryRowContext(ctx, "SELECT version, dirty FROM schema_migrations").
		Scan(&v, &dirty)
	return v, dirty, err
}

// redisClientProbe 让 *redis.Client 满足 redisProbe。
type redisClientProbe struct{ rdb *redis.Client }

func (p redisClientProbe) ping(ctx context.Context) error { return p.rdb.Ping(ctx).Err() }

// downDBProbe / downRedisProbe：依赖缺失时的恒失败实现，避免 nil 解引用。
type downDBProbe struct{}

func (downDBProbe) selectOne(ctx context.Context) error { return errProbeDown }
func (downDBProbe) migrationState(ctx context.Context) (int, bool, error) {
	return 0, false, errProbeDown
}

type downRedisProbe struct{}

func (downRedisProbe) ping(ctx context.Context) error { return errProbeDown }

// MarkConfigLoaded 置位 R4（装配完成即调）。
func (c *Checker) MarkConfigLoaded() { c.configLoaded.Store(true) }

// MarkStartupDone 置位 R5（HTTP server 开始服务后调）。
func (c *Checker) MarkStartupDone() { c.startupDone.Store(true) }

type readyzChecks struct {
	Postgres  string `json:"postgres"`
	Redis     string `json:"redis"`
	Migration string `json:"migration"`
	Config    string `json:"config"`
	Startup   string `json:"startup"`
}

// readyzBody 信封键集固定：status|checks|version|node。
type readyzBody struct {
	Status  string       `json:"status"`
	Checks  readyzChecks `json:"checks"`
	Version string       `json:"version"`
	Node    string       `json:"node"`
}

// HandlerHealthz：不触碰 PG/Redis，恒 200 {"status":"ok"}。
func (c *Checker) HandlerHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// HandlerReadyz：五项实时判定。任一失败 → 503 + status=degraded + 失败键 "fail"，
// 其余键仍 "ok"（值集合冻结：ok|fail）。
func (c *Checker) HandlerReadyz(w http.ResponseWriter, r *http.Request) {
	var checks readyzChecks
	allOK := true
	mark := func(ok bool, slot *string) {
		if ok {
			*slot = "ok"
		} else {
			*slot = "fail"
			allOK = false
		}
	}

	// R1：SELECT 1（1s 超时）。
	ctx1, cancel1 := context.WithTimeout(r.Context(), probeTimeout)
	err := c.db.selectOne(ctx1)
	cancel1()
	mark(err == nil, &checks.Postgres)

	// R2：PING（1s 超时）。
	ctx2, cancel2 := context.WithTimeout(r.Context(), probeTimeout)
	err = c.rdb.ping(ctx2)
	cancel2()
	mark(err == nil, &checks.Redis)

	// R3：schema_migrations 版本==migrationExpected 且 dirty=false（读主库）。
	v, dirty, err := c.db.migrationState(r.Context())
	mark(err == nil && v == c.migrationExpected && !dirty, &checks.Migration)

	// R4 / R5：装配与 startup 完成标志。
	mark(c.configLoaded.Load(), &checks.Config)
	mark(c.startupDone.Load(), &checks.Startup)

	body := readyzBody{
		Status:  "ok",
		Checks:  checks,
		Version: version.Version,
		Node:    c.node,
	}
	code := http.StatusOK
	if !allOK {
		body.Status = "degraded"
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, body)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
