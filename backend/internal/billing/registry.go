package billing

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/contract"
)

// 本文件实现在飞请求租约的 Redis 版 Registry（M3 决议 A3 / billing-00 §6.2）。
//
// 为什么放在 Redis 而不是 request_registry 表：租约只在请求生命周期内高频
// 读写（每 30s 一次心跳），且崩溃后丢失一行租约的后果由「持久化 hold 流水
// + 判定窗口」兜底；Redis 天然带 TTL，最适合承载这种短命、可重建的在飞态。
// request_registry 表保留给后续里程碑的跨节点审计（M3 不写它，见报告）。
//
// 键布局（两组，职责不同，不得混用 —— R-17）：
//
//	newtg:billing:inflight:<requestID>  Hash：user_id / started_at_ms /
//	                                    heartbeat_at_ms；TTL = 租约 TTL（300s）
//	newtg:billing:inflight-index        ZSET：member=requestID，score=started_at_ms
//
// 索引键刻意**不带** `inflight:` 前缀（用连字符分隔）：租约键是
// `inflight:` + requestID，若索引命名为 `inflight:index`，一个字面量为
// "index" 的 requestID 会与索引 ZSET 撞键（Hash/ZSET 类型冲突，读写报
// WRONGTYPE）。`inflight-index` 不属于任何 leaseKey 的值域，二者永不重叠。
//
// 为什么需要 ZSET 索引：Hash 的 TTL 是**存活租约**（心跳超时 300s），而未
// 续约的会话在 300s 后 Hash 就被 Redis 删除；但「悬挂」判定窗口是 600s
// （AbandonedWindowSeconds > TTL，R-17 的硬约束）。只扫 Hash 永远看不到
// 「已经开始超过 600s」的死亡会话——它在 300s 时就已经消失了。ZSET 不设
// TTL，保留开始时间索引直到 worker 成功回收后 Remove；扫描时 Hash 缺失
// 本身就是「心跳已停满一个 TTL」的证据（或 Redis 淘汰，同样不可信）。
//
// 清理依赖 Recovery 的幂等性：Remove 失败留下的索引项下一轮会被再次扫到，
// RecoverAbandoned 按 hold 流水 CAS 幂等，重复扫描不产生第二笔退款。
const (
	registryKeyPrefix = "newtg:billing:inflight:"
	registryIndexKey  = "newtg:billing:inflight-index"

	registryFieldUserID      = "user_id"
	registryFieldStartedAt   = "started_at_ms"
	registryFieldHeartbeatAt = "heartbeat_at_ms"

	// registryScanBatch 是单次 ListAbandoned 返回的**回收**上限（billing-06
	// §5.2 的每轮批量 500）。注意候选集里还包含「开始超过 600s 但仍在发
	// 心跳」的长请求：因此不能只取 ZSET 的前 500 个就停（否则大量活跃长
	// 请求会长期占据队头，饿死真正死亡的条目），而是分页检查直到凑满
	// 500 个可回收项或候选集耗尽。
	registryScanBatch = 500
)

// errRegistryUnavailable 表示 Registry 没有可用的 Redis 客户端。
//
// 会话侧把 Register 失败当作 fail-closed（回滚 hold）；worker 侧把
// ListAbandoned 失败当作「本轮扫描失败」而不是「无悬挂」。
var errRegistryUnavailable = errors.New("billing: request registry unavailable")

// RequestRegistry 是 billing.Registry 的 Redis 实现（写入方为计费会话）。
//
// 所有方法可并发调用。now 只用于测试注入可控时钟；生产为 time.Now。
type RequestRegistry struct {
	rdb *redis.Client
	now func() time.Time
}

// NewRegistry 构造 Redis 租约注册表。rdb 为 nil 时所有方法返回错误
// （装配错误必须显式暴露，绝不能静默退化成「无租约」）。
func NewRegistry(rdb *redis.Client) *RequestRegistry {
	return &RequestRegistry{rdb: rdb, now: time.Now}
}

// 编译期断言：RequestRegistry 必须满足会话使用的写侧接口。
var _ Registry = (*RequestRegistry)(nil)

// leaseKey 返回某个请求的租约 Hash 键。
func leaseKey(requestID string) string { return registryKeyPrefix + requestID }

// Register 写入在飞租约：userID + 开始时间 + 心跳，TTL 为会话传入的租约时长。
//
// 三个命令放在 TxPipeline（MULTI/EXEC）中：Hash 与索引必须同时可见，
// 否则 worker 可能看到「索引有、Hash 无」并立即按过期处理（虽然窗口
// 600s 通常兜住，但不应依赖时序巧合）。
func (r *RequestRegistry) Register(ctx context.Context, requestID string, userID int64, lease time.Duration) error {
	if r.rdb == nil {
		return errRegistryUnavailable
	}
	if requestID == "" {
		return ErrMissingRequestID
	}
	if lease <= 0 {
		// 调用方未给出 TTL 时按契约冻结值，绝不写成「永不过期」。
		lease = time.Duration(contract.InflightLeaseTTLSeconds) * time.Second
	}
	nowMs := r.now().UnixMilli()

	// 时间戳由 gateway 本地时钟写入、worker 本地时钟比较（ListAbandoned 的
	// now）。窗口 600s 与 TTL 300s 之间留出了 300s 的时钟裕量；生产环境
	// 必须保持 NTP 同步，否则偏差超过裕量会让活跃长请求被提前判定为悬挂。
	pipe := r.rdb.TxPipeline()
	pipe.HSet(ctx, leaseKey(requestID), map[string]any{
		registryFieldUserID:      userID,
		registryFieldStartedAt:   nowMs,
		registryFieldHeartbeatAt: nowMs,
	})
	pipe.PExpire(ctx, leaseKey(requestID), lease)
	pipe.ZAdd(ctx, registryIndexKey, redis.Z{Score: float64(nowMs), Member: requestID})
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("billing: register inflight lease %s: %w", requestID, err)
	}
	return nil
}

// Heartbeat 刷新心跳并续期 Hash TTL（billing-00 §6.2 租约参数）。
//
// TTL 固定取契约冻结的 InflightLeaseTTLSeconds（会话拥有该参数，Registry
// 只执行续约）：心跳间隔 30s 远小于 TTL 300s，单次丢心跳不会误回收。
//
// 若 Hash 已被 Redis 淘汰，HSET 会重建一个只含心跳字段的残缺 Hash：
// 这不影响判定——开始时间仍由 ZSET 的 score 提供，而缺失的
// started_at 只用于观测；下一次 Register 会补全，Remove 会一并清理。
func (r *RequestRegistry) Heartbeat(ctx context.Context, requestID string) error {
	if r.rdb == nil {
		return errRegistryUnavailable
	}
	if requestID == "" {
		return ErrMissingRequestID
	}
	pipe := r.rdb.TxPipeline()
	pipe.HSet(ctx, leaseKey(requestID), registryFieldHeartbeatAt, r.now().UnixMilli())
	pipe.PExpire(ctx, leaseKey(requestID), time.Duration(contract.InflightLeaseTTLSeconds)*time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("billing: heartbeat inflight lease %s: %w", requestID, err)
	}
	return nil
}

// Remove 删除租约 Hash 与索引项（会话终结或 worker 回收成功后调用）。
//
// 删除不存在的键/成员不是错误：重复终结（Terminate/Refund/Recover 的
// 幂等路径）都会走到这里，必须保持无副作用。
func (r *RequestRegistry) Remove(ctx context.Context, requestID string) error {
	if r.rdb == nil {
		return errRegistryUnavailable
	}
	if requestID == "" {
		return ErrMissingRequestID
	}
	pipe := r.rdb.TxPipeline()
	pipe.Del(ctx, leaseKey(requestID))
	pipe.ZRem(ctx, registryIndexKey, requestID)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("billing: remove inflight lease %s: %w", requestID, err)
	}
	return nil
}

// ListAbandoned 返回同时满足两个条件的 requestID：
//
//  1. 开始时间超过 AbandonedWindowSeconds（600s，扫描参数 —— worker 拥有）；
//  2. 心跳超过 InflightLeaseTTLSeconds（300s，租约参数 —— 会话拥有）
//     或 Hash 已不存在（心跳必然已停满一个 TTL）。
//
// 【两组参数不得混用】（R-17）：用扫描窗口当心跳阈值会把仍在发心跳的
// 活跃长请求误退；用 TTL 当扫描窗口会让刚预扣的请求在 5 分钟内被扫。
// 硬约束 AbandonedWindowSeconds > InflightLeaseTTLSeconds 在 contract 冻结。
//
// now 由调用方传入（worker 用 time.Now；测试注入），所有比较都在同一
// 时间基准上完成，不依赖 Redis 服务器时间。
func (r *RequestRegistry) ListAbandoned(ctx context.Context, now time.Time) ([]string, error) {
	if r.rdb == nil {
		return nil, errRegistryUnavailable
	}

	window := time.Duration(contract.AbandonedWindowSeconds) * time.Second
	leaseTTL := time.Duration(contract.InflightLeaseTTLSeconds) * time.Second
	// 严格「超过」：score/heartbeat 必须早于 cutoff 才纳入。
	// 毫秒精度下 Max 取 cutoff-1 等价于 started_at_ms < cutoff。
	windowCutoffMs := now.Add(-window).UnixMilli() - 1
	heartbeatCutoffMs := now.Add(-leaseTTL).UnixMilli()

	out := make([]string, 0, registryScanBatch)
	offset := 0
	for len(out) < registryScanBatch {
		ids, err := r.rdb.ZRangeByScore(ctx, registryIndexKey, &redis.ZRangeBy{
			Min:    "-inf",
			Max:    strconv.FormatInt(windowCutoffMs, 10),
			Offset: int64(offset),
			Count:  registryScanBatch,
		}).Result()
		if err != nil {
			// Redis 故障必须上抛：worker 绝不能把「扫描失败」当成「无悬挂」。
			return nil, fmt.Errorf("billing: list inflight candidates: %w", err)
		}
		if len(ids) == 0 {
			break
		}
		offset += len(ids)

		for _, id := range ids {
			hbMs, err := r.rdb.HGet(ctx, leaseKey(id), registryFieldHeartbeatAt).Int64()
			if errors.Is(err, redis.Nil) {
				// Hash 不存在或字段缺失：心跳已停止至少一个 TTL（Hash 的 TTL
				// 就是租约），按超时处理。窗口条件已由 ZSET score 满足。
				out = append(out, id)
			} else if err != nil {
				return nil, fmt.Errorf("billing: read heartbeat for %s: %w", id, err)
			} else if hbMs < heartbeatCutoffMs {
				out = append(out, id)
			}
			if len(out) >= registryScanBatch {
				break
			}
		}
		// 候选页不足一页说明 ZSET 已到末尾；否则继续下一页（前面可能全是
		// 仍在心跳的活跃长请求）。
		if len(ids) < registryScanBatch {
			break
		}
	}
	return out, nil
}
