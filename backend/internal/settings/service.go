package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"time"

	"newtg/gateway/internal/configcenter"
)

var (
	// ErrUnknownSection 表示 ?section= 不在注册表（API 映射 400）。
	ErrUnknownSection = errors.New("settings: unknown section")
	// ErrConflict 表示 section 的 updated_at 与 expect_updated_at 不一致（API 映射 409）。
	ErrConflict = errors.New("settings: section version conflict")
	// ErrSecretSealerRequired 表示密钥类键在没有 SecretSealer 时被写（装配错误）。
	ErrSecretSealerRequired = errors.New("settings: secret key requires SecretSealer")
)

// Broadcaster 是配置中心「快照刷新 + 广播」的接缝（生产为 *configcenter.Center）。
type Broadcaster interface {
	Refresh(ctx context.Context, keys []string) error
}

// ConfigReader 是公开下发的读取面（与 httpapi.ConfigReader 同方法集，
// 避免 settings → httpapi 的反向依赖）。
type ConfigReader interface {
	Get(key string) (json.RawMessage, bool)
}

// ServiceConfig 是 Service 的装配输入。DB 必填：设置的真源是 PostgreSQL
// system_settings，缺库时任何写操作都不应伪成功。
type ServiceConfig struct {
	DB          *sql.DB
	Broadcaster Broadcaster
	Sealer      configcenter.SecretSealer
	Logger      *slog.Logger
}

// Service 实现按 section 的读写。并发安全（无进程内可变状态）。
type Service struct {
	db     *sql.DB
	bcast  Broadcaster
	sealer configcenter.SecretSealer
	log    *slog.Logger
}

// NewService 构造设置服务。
func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.DB == nil {
		return nil, errors.New("settings: DB is required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: cfg.DB, bcast: cfg.Broadcaster, sealer: cfg.Sealer, log: log}, nil
}

// Item 是 GET /api/settings 的单项。Value 为 nil 表示该键从未落库
// （前端应展示 Default）；密钥类一律 MaskedValue。
type Item struct {
	Key       string `json:"key"`
	Value     any    `json:"value"`
	Type      Type   `json:"type"`
	Default   any    `json:"default"`
	Public    bool   `json:"public"`
	Secret    bool   `json:"secret"`
	Dangerous bool   `json:"dangerous"`
	// DangerDirection 是危险项的风险方向（"on"/"off"）；非危险项为空。
	DangerDirection string `json:"danger_direction"`
	Locked          bool   `json:"locked"`
}

// SectionView 是 GET /api/settings 的响应体。UpdatedAt 为 section 内各键
// updated_at 的最大值；无行时为 null（客户端下次写入传 null）。
type SectionView struct {
	Section   string     `json:"section"`
	UpdatedAt *time.Time `json:"updated_at"`
	Items     []Item     `json:"items"`
}

// UpdateResult 是 PUT /api/settings 的服务层结果（审计由 HTTP 层写入）。
type UpdateResult struct {
	Section   string         `json:"section"`
	UpdatedAt *time.Time     `json:"updated_at"`
	Values    map[string]any `json:"values"`
	// Before/After 是审计 diff（密钥掩码）。只包含本次请求提供的键。
	Before map[string]any
	After  map[string]any
	// Changed 是实际写入的键（跳过「留空不更新」的密钥）。
	Changed []string
}

type storedRow struct {
	raw       json.RawMessage
	updatedAt time.Time
}

// Get 读取一个 section 的全部注册键。DB 里没值的键返回 value=null。
func (s *Service) Get(ctx context.Context, section string) (SectionView, error) {
	rows, ok := Section(section)
	if !ok {
		return SectionView{}, ErrUnknownSection
	}
	stored, err := s.readRows(ctx, s.db, rows)
	if err != nil {
		return SectionView{}, err
	}
	view := SectionView{Section: section, Items: make([]Item, 0, len(rows))}
	var latest time.Time
	for _, setting := range rows {
		item := Item{
			Key:             setting.Key,
			Type:            setting.Type,
			Default:         DefaultValue(setting),
			Public:          setting.Public,
			Secret:          setting.Secret,
			Dangerous:       setting.Dangerous,
			DangerDirection: setting.DangerDirection,
			Locked:          setting.Locked,
		}
		if row, exists := stored[setting.Key]; exists {
			if setting.Secret {
				item.Value = MaskedValue
			} else {
				item.Value = decodeStoredValue(row.raw)
			}
			if row.updatedAt.After(latest) {
				latest = row.updatedAt
			}
		}
		view.Items = append(view.Items, item)
	}
	if !latest.IsZero() {
		view.UpdatedAt = &latest
	}
	return view, nil
}

// UpdateInput 是一次 section 写入。
type UpdateInput struct {
	Section string
	// Values 是请求体 values 的原始 JSON（兼容 int64 精度）。
	Values map[string]json.RawMessage
	// ExpectUpdatedAt 为 nil 表示「期望该 section 尚无任何行」。
	ExpectUpdatedAt *time.Time
	// DangerousConfirmed 表示前端已完成危险项二次确认（body 的
	// dangerous_confirmed=true）。危险键在未确认时拒绝写入（§1.3 / §1.5）。
	DangerousConfirmed bool
	Actor              int64
}

// Put 执行「整段校验 → 单事务写 → 提交后广播」。
//
// 并发保护用 section 级 PostgreSQL advisory lock + updated_at 比对：即便两个
// 管理员同时修改同一 section 的不同键，也会串行化并让后到者拿到 409，
// 而不是静默覆盖对方的整段快照（§1.3 乐观锁）。
func (s *Service) Put(ctx context.Context, in UpdateInput) (UpdateResult, error) {
	rows, ok := Section(in.Section)
	if !ok {
		return UpdateResult{}, ErrUnknownSection
	}
	byKey := make(map[string]Setting, len(rows))
	for _, setting := range rows {
		byKey[setting.Key] = setting
	}

	// 1) 请求键必须属于本 section（未注册键/跨 section 一律 400）。
	//
	// values[key]=null 的语义是 CLEAR：删除该行，让注册表默认值重新生效
	// （密钥类因此回到「未配置」）。它与密钥的空串/掩码不同——后者是
	// 「留空不更新」，避免前端回显掩码时把已配置的密钥清掉。
	provided := make(map[string]any, len(in.Values))
	clears := make(map[string]struct{})
	secretPlain := make(map[string]string)
	for key, raw := range in.Values {
		setting, known := byKey[key]
		if !known {
			if _, registered := Lookup(key); registered {
				return UpdateResult{}, invalid(key, "key does not belong to section %q", in.Section)
			}
			return UpdateResult{}, invalid(key, "unregistered setting key")
		}
		isClear := isJSONNull(raw)
		if setting.Locked {
			// 锁定键始终以服务端值生效：clear 等价于回到默认，无实际写入。
			if isClear {
				continue
			}
			decoded, err := normalizeRaw(setting, raw)
			if err != nil {
				return UpdateResult{}, err
			}
			if decoded != setting.Default {
				return UpdateResult{}, &LockedError{Key: key}
			}
			// 锁定键允许提交（前端只读也可整体回传），但始终以服务端值为准。
			continue
		}
		// 危险项按「风险方向」判断：只有关闭型风险被置为 false / 开启型风险
		// 被置为 true（非布尔项：值发生变化）时才要求确认；安全方向放行。
		// 具体判定在读到当前值之后进行，见 enforceDangerous。
		if isClear {
			clears[key] = struct{}{}
			continue
		}
		if setting.Secret {
			plain, skip, err := secretWriteValue(setting, raw)
			if err != nil {
				return UpdateResult{}, err
			}
			if skip {
				continue // 留空 / 掩码 = 不更新
			}
			secretPlain[key] = plain
			provided[key] = plain
			continue
		}
		value, err := normalizeRaw(setting, raw)
		if err != nil {
			return UpdateResult{}, err
		}
		provided[key] = value
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("settings: begin tx: %w", err)
	}
	// 主错误优先：成功路径显式 Commit。
	defer func() { _ = tx.Rollback() }()

	// 2) section 级串行化。事务结束自动释放。
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, in.Section); err != nil {
		return UpdateResult{}, fmt.Errorf("settings: lock section: %w", err)
	}

	current, err := s.readRows(ctx, tx, rows)
	if err != nil {
		return UpdateResult{}, err
	}
	currentMax := maxUpdatedAt(current)
	if !sameTimePointer(in.ExpectUpdatedAt, currentMax) {
		return UpdateResult{}, ErrConflict
	}
	if err := enforceDangerous(in, byKey, provided, clears, current); err != nil {
		return UpdateResult{}, err
	}

	// 3) 合并视图（默认值 → 已存值 → 本次新值）用于 section 级交叉校验。
	// clear 的键不出现在新值里，因此自然回落到默认值/未配置。
	merged := make(map[string]any, len(rows))
	secretConfigured := make(map[string]bool)
	for _, setting := range rows {
		merged[setting.Key] = DefaultValue(setting)
		if _, cleared := clears[setting.Key]; cleared {
			continue
		}
		if row, exists := current[setting.Key]; exists {
			if setting.Secret {
				secretConfigured[setting.Key] = true
			} else {
				merged[setting.Key] = decodeStoredValue(row.raw)
			}
		}
	}
	for key, value := range provided {
		merged[key] = value
		if setting, ok := byKey[key]; ok && setting.Secret {
			secretConfigured[key] = true
		}
	}
	// R2：充值档位的下限是「所有支付方式最低充值取最大」，而各 provider 的
	// min_topup 在各自 section。pay section 的 merged 视图默认看不到它们，
	// 这里在同一事务内补齐（已存值优先，否则默认值）。
	if in.Section == "pay" {
		if err := s.loadProviderMinimums(ctx, tx, merged); err != nil {
			return UpdateResult{}, err
		}
	}
	if validator := sectionValidators[in.Section]; validator != nil {
		if err := validator(merged, secretConfigured); err != nil {
			return UpdateResult{}, err
		}
	}

	// 4) 单事务写。更新时间在 Go 侧统一取值并截断到微秒：PostgreSQL 只存
	// 微秒，若返回纳秒值，客户端下一次带回来的 expect_updated_at 会永远不等。
	updatedAt := nowUTC().Truncate(time.Microsecond)
	changed := make([]string, 0, len(provided)+len(clears))
	for _, key := range sortedKeys(provided) {
		setting := byKey[key]
		value := provided[key]
		payload := value
		if setting.Secret {
			if s.sealer == nil {
				return UpdateResult{}, ErrSecretSealerRequired
			}
			sealed, err := s.sealer.Seal(secretPlain[key])
			if err != nil {
				return UpdateResult{}, fmt.Errorf("settings: seal %q: %w", key, err)
			}
			payload = sealed
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return UpdateResult{}, fmt.Errorf("settings: marshal %q: %w", key, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO system_settings ("key", value, updated_by, updated_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT ("key") DO UPDATE
			SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at`,
			key, raw, nullableActor(in.Actor), updatedAt); err != nil {
			return UpdateResult{}, fmt.Errorf("settings: write %q: %w", key, err)
		}
		changed = append(changed, key)
	}
	for _, key := range sortedSet(clears) {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM system_settings WHERE "key" = $1`, key); err != nil {
			return UpdateResult{}, fmt.Errorf("settings: clear %q: %w", key, err)
		}
		changed = append(changed, key)
	}

	// 写后版本必须重读：clear 可能删掉 section 的最后一行（此时契约要求
	// updated_at=null），不能拿本次写入的 now 冒充。
	post, err := s.readRows(ctx, tx, rows)
	if err != nil {
		return UpdateResult{}, err
	}
	postMax := maxUpdatedAt(post)
	if err := tx.Commit(); err != nil {
		return UpdateResult{}, fmt.Errorf("settings: commit: %w", err)
	}

	// 5) 提交后刷新快照并广播。广播失败不回滚已经提交的写入，也不把成功
	// 伪装成失败；本地快照已由 Refresh 重读，其它副本有 5 分钟兜底。
	if len(changed) > 0 && s.bcast != nil {
		if err := s.bcast.Refresh(ctx, changed); err != nil {
			s.log.Warn("settings: config broadcast failed after commit", "section", in.Section, "err", err)
		}
	}

	result := UpdateResult{
		Section:   in.Section,
		UpdatedAt: postMax,
		Values:    make(map[string]any, len(in.Values)),
		Before:    make(map[string]any, len(in.Values)),
		After:     make(map[string]any, len(in.Values)),
		Changed:   changed,
	}
	if len(changed) == 0 {
		// 空写入或全部是「留空不更新」的密钥：返回当前 section 版本，
		// 让前端不必再次 GET 就能清除脏标记。
		result.UpdatedAt = currentMax
	}
	for key := range in.Values {
		setting := byKey[key]
		row, exists := current[key]
		if exists {
			if setting.Secret {
				result.Before[key] = MaskedValue
			} else {
				result.Before[key] = decodeStoredValue(row.raw)
			}
		} else {
			result.Before[key] = nil
		}
		if _, cleared := clears[key]; cleared {
			// 行已删除：存储层回到「未配置」，默认值由 GET 的 default 字段表达。
			result.Values[key] = nil
			result.After[key] = nil
			continue
		}
		if setting.Secret {
			if secretConfigured[key] {
				result.Values[key] = MaskedValue
				result.After[key] = MaskedValue
			} else {
				result.Values[key] = nil
				result.After[key] = nil
			}
			continue
		}
		if value, ok := provided[key]; ok {
			result.Values[key] = value
			result.After[key] = value
		} else {
			// 锁定键被忽略：对外回传服务端权威默认值。
			result.Values[key] = DefaultValue(setting)
			result.After[key] = DefaultValue(setting)
		}
	}
	return result, nil
}

// enforceDangerous 只对「本次真正提交 + 处于风险方向」的危险键要求确认。
//
// 方向语义（contract v2 / modules-08 §1.5）：
//   - bool + off：目标值为 false 才是风险（如关闭注册/支付）；
//   - bool + on ：目标值为 true 才是风险（如允许私网/开启自动禁用）；
//   - 非 bool   ：任意值变化都按风险处理（兑换比、词表、危险密钥）；
//   - clear     ：回落默认值后再按上述规则判断；值未变化则不算风险。
func enforceDangerous(in UpdateInput, byKey map[string]Setting,
	provided map[string]any, clears map[string]struct{}, current map[string]storedRow) error {
	if in.DangerousConfirmed {
		return nil
	}
	for key := range in.Values {
		setting, ok := byKey[key]
		if !ok || !setting.Dangerous {
			continue
		}
		risky, err := dangerousChange(setting, key, provided, clears, current)
		if err != nil {
			return err
		}
		if risky {
			return invalid(key,
				"dangerous setting requires dangerous_confirmed=true for the %s direction",
				directionLabel(setting))
		}
	}
	return nil
}

func directionLabel(setting Setting) string {
	switch setting.DangerDirection {
	case DangerDirectionOff:
		return "off"
	case DangerDirectionAny:
		return "any"
	default:
		return "on"
	}
}

func dangerousChange(setting Setting, key string,
	provided map[string]any, clears map[string]struct{}, current map[string]storedRow) (bool, error) {
	_, isProvided := provided[key]
	_, isClear := clears[key]
	if !isProvided && !isClear {
		return false, nil // 空串/掩码的密钥写入 = 无变化
	}
	row, hasRow := current[key]

	if setting.Secret {
		// 密钥以密文落库，无法与明文比较；保守地认为「写入或清除已配置值」
		// 都是变化，必须确认。clear 一个从未配置的密钥不算变化。
		if isClear {
			return hasRow, nil
		}
		return true, nil
	}

	before := DefaultValue(setting)
	if hasRow {
		if decoded := decodeStoredValue(row.raw); decoded != nil {
			before = decoded
		}
	}
	after := DefaultValue(setting)
	if isProvided {
		after = provided[key]
	}

	if setting.Type == TypeBool {
		beforeBool, _ := before.(bool)
		afterBool, _ := after.(bool)
		if beforeBool == afterBool {
			return false, nil
		}
		switch setting.DangerDirection {
		case DangerDirectionOff:
			return !afterBool, nil
		case DangerDirectionAny:
			return true, nil
		default: // on
			return afterBool, nil
		}
	}
	// 非布尔危险项：值变化即风险（"on" 与 "any" 同义；off 不用于非布尔项）。
	return !reflect.DeepEqual(before, after), nil
}

// providerMinTopupKeys 是跨 section 参与档位下限计算的键。
var providerMinTopupKeys = map[string]string{
	"pay.stripe":        "pay.stripe.min_topup_cny",
	"pay.waffo":         "pay.waffo.min_topup_cny",
	"pay.waffo_pancake": "pay.waffo_pancake.min_topup_cny",
}

// loadProviderMinimums 把各支付 section 的最低充值并入 pay section 的校验视图。
func (s *Service) loadProviderMinimums(ctx context.Context, q queryer, merged map[string]any) error {
	for section, key := range providerMinTopupKeys {
		rows, ok := Section(section)
		if !ok {
			continue
		}
		setting, ok := Lookup(key)
		if !ok {
			continue
		}
		merged[key] = DefaultValue(setting)
		stored, err := s.readRows(ctx, q, rows)
		if err != nil {
			return err
		}
		if row, exists := stored[key]; exists {
			if value := decodeStoredValue(row.raw); value != nil {
				merged[key] = value
			}
		}
	}
	return nil
}

// PublicSnapshot 聚合注册表中 public:true 的键（§1.4.1）。DB 缺值回落默认值。
func PublicSnapshot(reader ConfigReader) map[string]any {
	out := make(map[string]any)
	for _, setting := range All() {
		if !setting.Public {
			continue
		}
		value := DefaultValue(setting)
		if reader != nil {
			if raw, ok := reader.Get(setting.Key); ok {
				if decoded := decodeStoredValue(raw); decoded != nil {
					value = decoded
				}
			}
		}
		out[setting.Key] = value
	}
	return out
}

// DomainValues 把扁平公开键投影成 `/api/status` 的域对象：
// "site.seo.description" → out["seo"]["description"]。
func DomainValues(snapshot map[string]any, domain string) map[string]any {
	prefix := domain + "."
	out := make(map[string]any)
	for key, value := range snapshot {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		path := strings.Split(strings.TrimPrefix(key, prefix), ".")
		insertNested(out, path, value)
	}
	return out
}

func insertNested(root map[string]any, path []string, value any) {
	current := root
	for i, segment := range path {
		if i == len(path)-1 {
			current[segment] = value
			return
		}
		next, ok := current[segment].(map[string]any)
		if !ok {
			next = make(map[string]any)
			current[segment] = next
		}
		current = next
	}
}

func (s *Service) readRows(ctx context.Context, q queryer, rows []Setting) (map[string]storedRow, error) {
	keys := make([]string, 0, len(rows))
	for _, setting := range rows {
		keys = append(keys, setting.Key)
	}
	if len(keys) == 0 {
		return map[string]storedRow{}, nil
	}
	placeholders := make([]string, len(keys))
	args := make([]any, len(keys))
	for i, key := range keys {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = key
	}
	result, err := q.QueryContext(ctx, fmt.Sprintf(
		`SELECT "key", value, updated_at FROM system_settings WHERE "key" IN (%s)`,
		strings.Join(placeholders, ", ")), args...)
	if err != nil {
		return nil, fmt.Errorf("settings: read section: %w", err)
	}
	defer func() { _ = result.Close() }()

	out := make(map[string]storedRow, len(rows))
	for result.Next() {
		var (
			key       string
			value     []byte
			updatedAt time.Time
		)
		if err := result.Scan(&key, &value, &updatedAt); err != nil {
			return nil, fmt.Errorf("settings: scan section: %w", err)
		}
		out[key] = storedRow{raw: append(json.RawMessage(nil), value...), updatedAt: updatedAt}
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("settings: iterate section: %w", err)
	}
	return out, nil
}

// queryer 让 readRows 同时服务 *sql.DB 与 *sql.Tx。
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func maxUpdatedAt(rows map[string]storedRow) *time.Time {
	var latest time.Time
	for _, row := range rows {
		if row.updatedAt.After(latest) {
			latest = row.updatedAt
		}
	}
	if latest.IsZero() {
		return nil
	}
	return &latest
}

func sameTimePointer(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func nullableActor(actor int64) any {
	if actor == 0 {
		return nil
	}
	return actor
}

func decodeStoredValue(raw json.RawMessage) any {
	value, err := decodeJSON(raw)
	if err != nil {
		return nil
	}
	// 与写入路径同型：数据库里的整数必须还原为 int64，否则 section 级
	// int64 校验（档位/最低充值）会把已存值当成 json.Number 静默跳过。
	return normalizeScalars(value)
}

// secretWriteValue 实现「密钥留空不更新」：空串与掩码视为不修改。
// 注意：null 由调用方先按 CLEAR 处理（删除行），不会走到这里。
func secretWriteValue(setting Setting, raw json.RawMessage) (string, bool, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == `""` || trimmed == `"`+MaskedValue+`"` {
		return "", true, nil
	}
	value, err := normalizeRaw(setting, raw)
	if err != nil {
		return "", false, err
	}
	return value.(string), false, nil
}

// isJSONNull 判定请求值是否为字面量 null（CLEAR 语义）。
func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// sortedSet 返回集合的稳定顺序（审计/响应可复现）。
func sortedSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
