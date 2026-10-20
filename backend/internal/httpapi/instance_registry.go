// 逐副本实例心跳（modules-08 §3.7 F-08-21 / platform-05 §4）。
//
// 每个 gateway/worker 进程启动后每 30s 向 Redis 写一条心跳：
//
//	newtg:instance:<instance_id>  Hash{status,started_at,last_seen,version,role}
//	TTL 90s
//
// 为什么是 Redis 而不是数据库：实例列表是「谁还活着」的瞬时观测面，不是
// 业务事实；Redis key 的 TTL 让进程被杀（来不及 DELETE）时条目在 90s 内
// 自动消失，读侧不需要额外的清理任务。TTL = 3× 心跳间隔，容忍一次丢写。
//
// 为什么读侧按 SCAN 而不是维护一个实例集合：集合成员不会随 key 过期自动
// 移除（Redis 不提供过期回调的可靠语义），需要一个清理者；SCAN + TTL 是
// 无维护者的最终一致方案。实例数是个位数到几十，SCAN 成本可忽略。
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// InstanceHeartbeatKeyPrefix 是心跳 key 的固定前缀（platform-05 观测面）。
	InstanceHeartbeatKeyPrefix = "newtg:instance:"
	// InstanceHeartbeatInterval 是写入周期（F-08-21：30s）。
	InstanceHeartbeatInterval = 30 * time.Second
	// InstanceHeartbeatTTL 是 key 的存活时间。90s = 3 个周期：一次网络
	// 抖动/偶发写失败不会让实例从列表消失；连续 3 次失败则视为失联。
	InstanceHeartbeatTTL = 90 * time.Second

	instanceRoleGateway = "gateway"
	instanceRoleWorker  = "worker"
	instanceStatusLive  = "active"

	// instanceReadTimeout 限制一次列表读取（SCAN + HGETALL）。
	instanceReadTimeout = 2 * time.Second
	// instanceHeartbeatWarnInterval 避免 Redis 长时间不可用时每 30s 刷屏。
	instanceHeartbeatWarnInterval = 5 * time.Minute
)

// ResolveInstanceID 解析当前进程的实例标识：INSTANCE_ID 非空时优先，
// 否则回退主机名，主机名不可用再回退固定占位值 "unknown"。
//
// 这是 D3 性能端点与逐副本心跳的**唯一**实现——两处若各写一份，运维改变
// 环境变量后会出现「性能页显示 A、实例表显示 B」的漂移。getenv/hostname
// 作为接缝传入，便于单测不依赖进程环境。
func ResolveInstanceID(getenv func(string) string, hostname func() (string, error)) string {
	if getenv != nil {
		if id := strings.TrimSpace(getenv("INSTANCE_ID")); id != "" {
			return id
		}
	}
	if hostname != nil {
		if name, err := hostname(); err == nil {
			if id := strings.TrimSpace(name); id != "" {
				return id
			}
		}
	}
	return "unknown"
}

// InstanceRecord 是 GET /api/instances 的单行投影。
//
// 不含地址、端口、密钥或任何连接信息：实例表只回答「谁、什么角色、什么
// 版本、最后一次心跳」。
type InstanceRecord struct {
	InstanceID string    `json:"instance_id"`
	Status     string    `json:"status"`
	Role       string    `json:"role"`
	Version    string    `json:"version"`
	StartedAt  time.Time `json:"started_at"`
	LastSeen   time.Time `json:"last_seen"`
}

// InstanceHeartbeatKey 返回实例心跳的 Redis key。
func InstanceHeartbeatKey(instanceID string) string {
	return InstanceHeartbeatKeyPrefix + instanceID
}

// InstanceHeartbeatConfig 是心跳循环的装配输入。
type InstanceHeartbeatConfig struct {
	Redis      *redis.Client
	InstanceID string
	Role       string
	Version    string
	StartedAt  time.Time
	Interval   time.Duration
	TTL        time.Duration
	Logger     *slog.Logger
	// Now 是时间接缝；nil 时 time.Now。
	Now func() time.Time
}

// InstanceHeartbeat 是运行中的心跳循环。Stop 取消循环、删除本实例 key 并
// 等待退出；重复调用安全。
type InstanceHeartbeat struct {
	rdb       *redis.Client
	key       string
	role      string
	version   string
	startedAt time.Time
	interval  time.Duration
	ttl       time.Duration
	now       func() time.Time
	log       *slog.Logger

	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once

	// warnMu 保护 lastWarn，避免循环内多条告警刷屏。
	warnMu   sync.Mutex
	lastWarn time.Time
}

// StartInstanceHeartbeat 启动心跳循环并立即写一次（不等第一个 30s tick，
// 否则新副本在首个窗口内不出现在实例表）。
//
// 首次写失败不会让进程启动失败：Redis 已在 OpenRedis 阶段 Ping 过，这里的
// 失败是瞬时抖动；循环会按周期重试，读侧最多晚 30s 看到该实例。
func StartInstanceHeartbeat(parent context.Context, cfg InstanceHeartbeatConfig) (*InstanceHeartbeat, error) {
	if cfg.Redis == nil {
		return nil, errors.New("httpapi: instance heartbeat requires redis")
	}
	if strings.TrimSpace(cfg.InstanceID) == "" {
		return nil, errors.New("httpapi: instance heartbeat requires instance id")
	}
	role := strings.TrimSpace(cfg.Role)
	if role == "" {
		role = instanceRoleGateway
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = InstanceHeartbeatInterval
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = InstanceHeartbeatTTL
	}
	startedAt := cfg.StartedAt
	if startedAt.IsZero() {
		startedAt = now()
	}

	ctx, cancel := context.WithCancel(parent)
	h := &InstanceHeartbeat{
		rdb:       cfg.Redis,
		key:       InstanceHeartbeatKey(strings.TrimSpace(cfg.InstanceID)),
		role:      role,
		version:   cfg.Version,
		startedAt: startedAt.UTC(),
		interval:  interval,
		ttl:       ttl,
		now:       now,
		log:       logger,
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	if err := h.write(ctx); err != nil {
		h.warn(ctx, "instance heartbeat initial write failed", err)
	}
	go h.loop(ctx)
	return h, nil
}

// Stop 停止循环、删除本实例心跳并等待退出。删除使用独立的短超时上下文：
// 停机时父 ctx 已经取消，若复用父 ctx 会跳过删除，导致实例在 TTL 内仍显示
// 为存活。
func (h *InstanceHeartbeat) Stop() {
	if h == nil {
		return
	}
	h.stopOnce.Do(h.cancel)
	<-h.done
}

func (h *InstanceHeartbeat) loop(ctx context.Context) {
	defer close(h.done)
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			h.remove()
			return
		case <-ticker.C:
			if err := h.write(ctx); err != nil {
				h.warn(ctx, "instance heartbeat write failed", err)
			}
		}
	}
}

func (h *InstanceHeartbeat) write(ctx context.Context) error {
	now := h.now().UTC()
	// HSet + Expire 放在同一 MULTI 里：若两次命令之间进程被杀，只有 HSet
	// 落地的 key 没有 TTL，会永久留在实例表（第一次写时尤其危险）。
	pipe := h.rdb.TxPipeline()
	pipe.HSet(ctx, h.key, map[string]interface{}{
		"status":     instanceStatusLive,
		"started_at": h.startedAt.Format(time.RFC3339Nano),
		"last_seen":  now.Format(time.RFC3339Nano),
		"version":    h.version,
		"role":       h.role,
	})
	pipe.Expire(ctx, h.key, h.ttl)
	_, err := pipe.Exec(ctx)
	return err
}

func (h *InstanceHeartbeat) remove() {
	ctx, cancel := context.WithTimeout(context.Background(), instanceReadTimeout)
	defer cancel()
	if err := h.rdb.Del(ctx, h.key).Err(); err != nil {
		h.log.Warn("instance heartbeat delete on shutdown failed", "error", err)
	}
}

// warn 对同一类故障按 5 分钟节流：Redis 故障期间心跳每 30s 重试一次，
// 不节流会在日志里放大成无信息量的噪声。
func (h *InstanceHeartbeat) warn(ctx context.Context, msg string, err error) {
	if ctx.Err() != nil {
		return
	}
	now := h.now()
	h.warnMu.Lock()
	last := h.lastWarn
	if !last.IsZero() && now.Sub(last) < instanceHeartbeatWarnInterval {
		h.warnMu.Unlock()
		return
	}
	h.lastWarn = now
	h.warnMu.Unlock()
	h.log.Warn(msg, "error", err, "instance", h.key)
}

// ListInstanceHeartbeats 读取全部未过期实例心跳，按 instance_id 排序。
//
// 空值/读不到的 key 直接跳过：key 可能在 SCAN 与 HGETALL 之间过期（或由
// 停机删除），这不是错误。Redis 本身不可用才返回错误，由调用方决定
// 503 还是降级。
func ListInstanceHeartbeats(ctx context.Context, rdb *redis.Client) ([]InstanceRecord, error) {
	if rdb == nil {
		return nil, errors.New("httpapi: instance list requires redis")
	}
	ctx, cancel := context.WithTimeout(ctx, instanceReadTimeout)
	defer cancel()

	seen := make(map[string]struct{})
	var records []InstanceRecord
	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, InstanceHeartbeatKeyPrefix+"*", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			values, err := rdb.HGetAll(ctx, key).Result()
			if err != nil {
				return nil, err
			}
			// 键在 SCAN 后过期：跳过而不是产出空行。
			if len(values) == 0 {
				continue
			}
			id := strings.TrimPrefix(key, InstanceHeartbeatKeyPrefix)
			if id == "" {
				continue
			}
			record := InstanceRecord{
				InstanceID: id,
				Status:     values["status"],
				Role:       values["role"],
				Version:    values["version"],
			}
			if record.Status == "" {
				record.Status = instanceStatusLive
			}
			if started, err := time.Parse(time.RFC3339Nano, values["started_at"]); err == nil {
				record.StartedAt = started.UTC()
			}
			if seenAt, err := time.Parse(time.RFC3339Nano, values["last_seen"]); err == nil {
				record.LastSeen = seenAt.UTC()
			}
			records = append(records, record)
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].InstanceID < records[j].InstanceID
	})
	return records, nil
}

// DefaultInstanceRole 返回当前运行模式对应的角色枚举。RUN_MODE 的合法值
// 由 config 校验为 gateway/worker/migrate；migrate 不常驻、不写心跳。
func DefaultInstanceRole(runMode string) string {
	switch strings.TrimSpace(runMode) {
	case instanceRoleWorker:
		return instanceRoleWorker
	default:
		return instanceRoleGateway
	}
}

// ResolveProcessInstanceID 是生产路径的便捷封装（os.Getenv + os.Hostname），
// 供 platform 装配与 httpapi 组合根共用。
func ResolveProcessInstanceID() string {
	return ResolveInstanceID(os.Getenv, os.Hostname)
}
