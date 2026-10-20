// 系统状态端点（modules-08 §3.7 F-08-21）。
//
//	GET /api/system/status   admin
//
// 响应冻结为 {version, uptime, instances[], dependencies{db,redis,worker}}。
// 版本号只在管理面出现，绝不进公开 /api/status（§1.3 第 5 条 / §3.15 Q6）。
package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/version"
)

// systemProbeTimeout 与 /readyz 的单依赖探测上限一致。
const systemProbeTimeout = time.Second

// SystemStatusConfig 是系统状态端点的装配输入。
type SystemStatusConfig struct {
	DB       *sql.DB
	Redis    *redis.Client
	NodeName string
	// InstanceID 是逐副本心跳的标识（INSTANCE_ID → hostname）。空时回退
	// NodeName，保证最小装配仍能给出一个稳定名称。
	InstanceID string
	Role       string
	StartedAt  time.Time
	Logger     *slog.Logger
}

// SystemStatusHandlers 持有系统状态依赖。
type SystemStatusHandlers struct {
	db         *sql.DB
	rdb        *redis.Client
	instanceID string
	role       string
	startedAt  time.Time
	log        *slog.Logger
	now        func() time.Time
}

// NewSystemStatusHandlers 构造端点。依赖缺失时探测一律记 fail（fail-closed），
// 而不是让 nil 解引用把整个管理面拖垮。
func NewSystemStatusHandlers(cfg SystemStatusConfig) *SystemStatusHandlers {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	startedAt := cfg.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	role := cfg.Role
	if role == "" {
		role = "gateway"
	}
	instanceID := cfg.InstanceID
	if instanceID == "" {
		instanceID = cfg.NodeName
	}
	return &SystemStatusHandlers{
		db: cfg.DB, rdb: cfg.Redis, instanceID: instanceID, role: role,
		startedAt: startedAt, log: log, now: time.Now,
	}
}

// Register 挂载到受保护子树；权限为 admin 只读（与设置 GET 同门）。
func (h *SystemStatusHandlers) Register(mux *http.ServeMux) {
	mux.Handle("GET /api/system/status", requireAdminRole(http.HandlerFunc(h.handle)))
}

type systemStatusResponse struct {
	Version   string             `json:"version"`
	BuildTime string             `json:"build_time"`
	GitCommit string             `json:"git_commit"`
	Uptime    int64              `json:"uptime"`
	Instances []systemInstance   `json:"instances"`
	Deps      systemDependencies `json:"dependencies"`
}

type systemInstance struct {
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	Version   string    `json:"version"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	LastSeen  time.Time `json:"last_seen"`
}

type systemDependencies struct {
	DB     string `json:"db"`
	Redis  string `json:"redis"`
	Worker string `json:"worker"`
}

func (h *SystemStatusHandlers) handle(w http.ResponseWriter, r *http.Request) {
	now := h.now().UTC()
	uptime := int64(now.Sub(h.startedAt).Seconds())
	if uptime < 0 {
		uptime = 0
	}
	instance := systemInstance{
		Name:      h.instanceID,
		Role:      h.role,
		Version:   version.Version,
		Status:    "active",
		StartedAt: h.startedAt.UTC(),
		LastSeen:  now,
	}
	instances := h.instances(r.Context(), instance)
	writeJSON(w, http.StatusOK, systemStatusResponse{
		Version:   version.Version,
		BuildTime: version.BuildTime,
		GitCommit: version.Commit,
		Uptime:    uptime,
		Instances: instances,
		Deps: systemDependencies{
			DB:    h.probeDB(r.Context()),
			Redis: h.probeRedis(r.Context()),
			// worker 心跳表不在本波范围：没有观测面时如实返回 unknown，
			// 绝不把「没查」伪装成「健康」。
			Worker: "unknown",
		},
	})
}

// instances 与 GET /api/instances 读同一 Redis 心跳源（F-08-21）。
//
// 当前实例即使没有心跳（Redis 故障 / 首次写失败）也必须保留：系统状态页
// 的「版本与就绪」卡依赖它；缺失会让管理员误以为当前进程不存在。Redis
// 故障时降级为「只有当前实例」，不把整个状态端点变成 503——版本/就绪信息
// 仍应可读。
func (h *SystemStatusHandlers) instances(ctx context.Context, current systemInstance) []systemInstance {
	records, err := ListInstanceHeartbeats(ctx, h.rdb)
	if err != nil {
		h.log.Warn("system status: instance heartbeat read failed", "error", err)
		return []systemInstance{current}
	}
	out := make([]systemInstance, 0, len(records)+1)
	found := false
	for _, rec := range records {
		if rec.InstanceID == current.Name {
			found = true
		}
		out = append(out, systemInstance{
			Name:      rec.InstanceID,
			Role:      rec.Role,
			Version:   rec.Version,
			Status:    rec.Status,
			StartedAt: rec.StartedAt,
			LastSeen:  rec.LastSeen,
		})
	}
	if !found {
		out = append(out, current)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (h *SystemStatusHandlers) probeDB(ctx context.Context) string {
	if h.db == nil {
		return "failed"
	}
	ctx, cancel := context.WithTimeout(ctx, systemProbeTimeout)
	defer cancel()
	var one int
	if err := h.db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			h.log.Warn("system status: db probe failed", "err", err)
		}
		return "failed"
	}
	return "ok"
}

func (h *SystemStatusHandlers) probeRedis(ctx context.Context) string {
	if h.rdb == nil {
		return "failed"
	}
	ctx, cancel := context.WithTimeout(ctx, systemProbeTimeout)
	defer cancel()
	if err := h.rdb.Ping(ctx).Err(); err != nil {
		h.log.Warn("system status: redis probe failed", "err", err)
		return "failed"
	}
	return "ok"
}
