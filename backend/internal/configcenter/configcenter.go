// Package configcenter 实现 BE-6 配置中心机制：system_settings 全量快照 +
// 乐观锁写入 + Redis pub/sub 秒级广播（≤1s 全副本生效）。
//
// 设计权威：docs/modules-08 §1.3（保存与生效机制）、§1.4（键命名）；
// 任务拆分：docs/modules-99 BE-6。M0 只落地机制面，设置注册表/密钥清单/UI 属后续里程碑。
package configcenter

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ChangedChannel 是配置变更广播频道名（本计划冻结，M0 不得改动）。
const ChangedChannel = "newtg:cfg:changed"

// snapshotTTL 是整表快照的兜底重载周期。pub/sub 单键重载是 ≤1s 的主动路径，
// 整表重载只用于兜底：漏消息、订阅建立前的变更、Redis 短暂不可用等。
const snapshotTTL = 5 * time.Minute

var (
	// ErrConflict 表示乐观锁失败：行不存在或 updated_at 与期望值不匹配（API 层映射 409）。
	ErrConflict = errors.New("configcenter: 版本冲突（updated_at 不匹配）")
	// ErrSecretSealerRequired 表示密钥类键在未配置 Sealer 时被拒绝写入。
	ErrSecretSealerRequired = errors.New("configcenter: 密钥类键必须配置 SecretSealer")
)

// SecretSealer 是密钥类配置值的加解密封口（AES-256-GCM 由 M1 按 security-03 实现，
// M0 只留接缝）。Seal 返回可落库的密文，Open 反向还原。
type SecretSealer interface {
	Seal(plain string) (string, error)
	Open(sealed string) (string, error)
}

// secretKeys 是密钥类键清单（security-03 §2.4：写入前 AES-256-GCM 密封）。
// R-09 的 M1 落地：Turnstile 私钥必须密封落库（modules-08 F-08-07）；
// site_key / enabled 是公开下发字段，不登记。
//
// M5 充值（Task 3）追加支付渠道凭据：Stripe secret/webhook secret 与
// 支付宝应用私钥/公钥都来自 modules-08 §3.20 的密钥类键。登记后 Save
// 会先密封再落库，TopupService 用同一个 SecretSealer 解封；不登记会让
// 生产环境读取到明文/解封失败，渠道被判定为「配置不完整」。
// `pay.stripe.publishable_key` 是公开下发字段（P-17），刻意不登记。
var secretKeys = map[string]struct{}{
	"auth.turnstile.secret_key": {},
	"pay.stripe.secret_key":     {},
	"pay.stripe.webhook_secret": {},
	"pay.alipay.private_key":    {},
	"pay.alipay.public_key":     {},
	// Epay 签名密钥（modules-04 F-04-21）：与其它支付凭据同等级密封。
	"pay.epay.key":         {},
	"notify.smtp.password": {},
	// F-08-20：Webhook 签名密钥必须与 SMTP 口令同等对待（加密落库、
	// 读接口永不回明文）；漏登记会让投递进程读到密文而验签全失败。
	"notify.webhook.secret": {},
	// M6d 部署控制面凭据（modules-08 §3.20.5）：io.net API Key 只经设置键
	// 录入，任何走 Center.Save 的路径都必须先密封；部署侧读取依赖这份清单
	// 与 settings 注册表一致，否则会出现「设置页加密、部署页读到密文」。
	"deploy.ionet.api_key": {},
}

// rowScanner 抽象 *sql.Row 的 Scan，便于单测以缝隙替代真实数据库（GC-8）。
type rowScanner interface {
	Scan(dest ...any) error
}

// dbQuerier 是 Center 需要的数据库能力。*sql.DB 经 sqlDB 适配；单测使用 fake。
type dbQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) rowScanner
}

// sqlDB 把 *sql.DB 适配为 dbQuerier：*sql.DB.QueryRowContext 返回具体类型
// *sql.Row，接口方法需要 rowScanner，故显式包一层。
type sqlDB struct{ *sql.DB }

func (s sqlDB) QueryRowContext(ctx context.Context, query string, args ...any) rowScanner {
	return s.DB.QueryRowContext(ctx, query, args...)
}

// redisClient 是 Center 需要的 Redis 能力（Publish/Subscribe 缝隙，便于单测）。
type redisClient interface {
	Publish(ctx context.Context, channel string, message any) *redis.IntCmd
	Subscribe(ctx context.Context, channels ...string) *redis.PubSub
}

// Center 持有 system_settings 的内存快照。Get/Version 无锁读（RWMutex），
// Save/Load/pub-sub 重载在写锁内替换快照条目。Center 可并发使用。
type Center struct {
	db  dbQuerier
	rdb redisClient

	// Sealer 为密钥类键的加解密封口；nil 时密钥类键拒绝写入。
	Sealer SecretSealer

	// secretKeys 默认指向包级清单；单测可注入自定义清单。
	secretKeys map[string]struct{}

	mu       sync.RWMutex
	values   map[string]json.RawMessage
	versions map[string]time.Time
	lastLoad time.Time
}

// New 构造配置中心。db 为 PG 主库连接池，rdb 为 go-redis 客户端。
func New(db *sql.DB, rdb *redis.Client) *Center {
	return newCenter(sqlDB{db}, rdb)
}

func newCenter(db dbQuerier, rdb redisClient) *Center {
	return &Center{
		db:         db,
		rdb:        rdb,
		secretKeys: secretKeys,
		values:     make(map[string]json.RawMessage),
		versions:   make(map[string]time.Time),
	}
}

// Load 全量读取 system_settings 并原子替换内存快照与版本表。
func (c *Center) Load(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx, `SELECT "key", value, updated_at FROM system_settings`)
	if err != nil {
		return fmt.Errorf("configcenter: load snapshot: %w", err)
	}
	// 只读查询；关闭失败不影响已读取的快照，主错误优先。
	defer func() { _ = rows.Close() }()

	values := make(map[string]json.RawMessage)
	versions := make(map[string]time.Time)
	for rows.Next() {
		var (
			key       string
			value     []byte
			updatedAt time.Time
		)
		if err := rows.Scan(&key, &value, &updatedAt); err != nil {
			return fmt.Errorf("configcenter: load snapshot scan: %w", err)
		}
		values[key] = json.RawMessage(bytes.Clone(value))
		versions[key] = updatedAt
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("configcenter: load snapshot rows: %w", err)
	}

	c.mu.Lock()
	c.values = values
	c.versions = versions
	c.lastLoad = time.Now()
	c.mu.Unlock()
	return nil
}

// Get 返回 key 的当前 JSON 值（独立副本）；未知键返回 ok=false。
func (c *Center) Get(key string) (json.RawMessage, bool) {
	c.mu.RLock()
	value, ok := c.values[key]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return bytes.Clone(value), true
}

// Version 返回 key 的 updated_at；未知键返回 ok=false。
func (c *Center) Version(key string) (time.Time, bool) {
	c.mu.RLock()
	version, ok := c.versions[key]
	c.mu.RUnlock()
	return version, ok
}

// Save 写入单个配置键。
//
//   - expectUpdatedAt 非 nil：乐观锁 UPDATE，行不存在或不匹配时 affected==0 → ErrConflict。
//   - expectUpdatedAt 为 nil：INSERT ... ON CONFLICT DO UPDATE（upsert）。
//
// 密钥类键（secretKeys 命中）必须配置 Sealer，且 value 必须是 string。
// 写入成功后单键重载本地快照，并 PUBLISH newtg:cfg:changed <key>。
func (c *Center) Save(ctx context.Context, key string, value any, expectUpdatedAt *time.Time) error {
	if key == "" {
		return errors.New("configcenter: key 不能为空")
	}

	if _, secret := c.secretKeys[key]; secret {
		if c.Sealer == nil {
			return ErrSecretSealerRequired
		}
		plain, ok := value.(string)
		if !ok {
			return fmt.Errorf("configcenter: 密钥类键 %q 的值必须是 string，实际 %T", key, value)
		}
		sealed, err := c.Sealer.Seal(plain)
		if err != nil {
			return fmt.Errorf("configcenter: 加密 %q: %w", key, err)
		}
		value = sealed
	}

	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("configcenter: 序列化 %q: %w", key, err)
	}

	// M0 没有调用者上下文（admin 用户 ID 由 API 层在 M6 接入），updated_by 记 NULL。
	var updatedBy any

	if expectUpdatedAt != nil {
		res, err := c.db.ExecContext(ctx,
			`UPDATE system_settings SET value = $1, updated_by = $2, updated_at = now() WHERE "key" = $3 AND updated_at = $4`,
			[]byte(raw), updatedBy, key, *expectUpdatedAt)
		if err != nil {
			return fmt.Errorf("configcenter: update %q: %w", key, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("configcenter: update %q rows affected: %w", key, err)
		}
		if affected == 0 {
			return ErrConflict
		}
		if err := c.reloadKey(ctx, key); err != nil {
			return err
		}
	} else {
		var updatedAt time.Time
		err := c.db.QueryRowContext(ctx,
			`INSERT INTO system_settings ("key", value, updated_by) VALUES ($1, $2, $3)
			 ON CONFLICT ("key") DO UPDATE
			 SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = now()
			 RETURNING updated_at`,
			key, []byte(raw), updatedBy).Scan(&updatedAt)
		if err != nil {
			return fmt.Errorf("configcenter: upsert %q: %w", key, err)
		}
		c.setSnapshot(key, json.RawMessage(bytes.Clone(raw)), updatedAt)
	}

	if err := c.rdb.Publish(ctx, ChangedChannel, key).Err(); err != nil {
		return fmt.Errorf("configcenter: publish %q: %w", key, err)
	}
	return nil
}

// Refresh 是批量写入（单事务）后的快照刷新与广播入口。
//
// settings 服务在一个事务里写入多个键，无法逐键走 Save（那会在事务外重复写库）。
// 提交成功后调用本方法：先从数据库重读每个键到本地快照，再逐键 PUBLISH
// newtg:cfg:changed，保证其它副本走同一失效路径（≤1s 生效）。
//
// 失败策略（F6/F7）：使用 context.WithoutCancel——配置已在 DB 提交，客户端
// 断开不能阻止其它副本失效；单个键失败继续处理其余键并聚合错误，避免一个
// 坏键让后面的键永久停留在旧快照。Redis 未装配时只刷新本地快照，不 Publish。
func (c *Center) Refresh(ctx context.Context, keys []string) error {
	ctx = context.WithoutCancel(ctx)
	var errs []error
	for _, key := range keys {
		if err := c.reloadKey(ctx, key); err != nil {
			errs = append(errs, err)
			continue
		}
		if c.rdb == nil {
			continue
		}
		if err := c.rdb.Publish(ctx, ChangedChannel, key).Err(); err != nil {
			errs = append(errs, fmt.Errorf("configcenter: publish %q: %w", key, err))
		}
	}
	return errors.Join(errs...)
}

// Subscribe 订阅冻结频道并启动后台监听；ctx 结束时停止并释放订阅。
// 收到 key 后单键重载（≤1s 生效路径）；每 snapshotTTL 做一次全量重载兜底。
func (c *Center) Subscribe(ctx context.Context) error {
	pubsub := c.rdb.Subscribe(ctx, ChangedChannel)
	// Receive 等待服务端 SUBSCRIBE 确认，确保 Subscribe 返回后消息不会漏收。
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return fmt.Errorf("configcenter: subscribe %s: %w", ChangedChannel, err)
	}
	go c.watch(ctx, pubsub)
	return nil
}

func (c *Center) watch(ctx context.Context, pubsub *redis.PubSub) {
	// watch 退出路径；关闭 pubsub 的错误已无补救动作。
	defer func() { _ = pubsub.Close() }()

	messages := pubsub.Channel()
	ticker := time.NewTicker(snapshotTTL)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-messages:
			if !ok {
				return
			}
			key := changedKey(msg.Payload)
			if key == "" {
				continue
			}
			if err := c.reloadKey(ctx, key); err != nil && ctx.Err() == nil {
				// 单键重载失败不退出监听；snapshotTTL 兜底会最终修正快照。
				slog.Warn("configcenter: 单键重载失败", "key", key, "err", err)
			}
		case <-ticker.C:
			if err := c.Load(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("configcenter: 快照兜底重载失败", "err", err)
			}
		}
	}
}

// reloadKey 从 PG 重读单个键并更新快照；行已不存在则从快照移除。
func (c *Center) reloadKey(ctx context.Context, key string) error {
	var (
		value     []byte
		updatedAt time.Time
	)
	err := c.db.QueryRowContext(ctx,
		`SELECT value, updated_at FROM system_settings WHERE "key" = $1`, key).
		Scan(&value, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		c.mu.Lock()
		delete(c.values, key)
		delete(c.versions, key)
		c.mu.Unlock()
		return nil
	}
	if err != nil {
		return fmt.Errorf("configcenter: reload %q: %w", key, err)
	}
	c.setSnapshot(key, json.RawMessage(bytes.Clone(value)), updatedAt)
	return nil
}

func (c *Center) setSnapshot(key string, value json.RawMessage, version time.Time) {
	c.mu.Lock()
	c.values[key] = value
	c.versions[key] = version
	c.mu.Unlock()
}

// changedKey 解析广播 payload。本计划（Task 16）冻结 Save 发布纯 key；
// modules-08 §1.3 描述的 `{key, ts}` 对象形态同样接受，便于 M1+ 扩展载荷。
func changedKey(payload string) string {
	payload = strings.TrimSpace(payload)
	if strings.HasPrefix(payload, "{") {
		var msg struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal([]byte(payload), &msg); err == nil && msg.Key != "" {
			return msg.Key
		}
	}
	return payload
}
