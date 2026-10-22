package updatecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/egress"
	"newtg/gateway/internal/settings"
)

// 设置键（与 settings 注册表逐字一致）。
const (
	KeyEnabled             = "site.update_check.enabled"
	KeySourceURL           = "site.update_check.source_url"
	KeyAllowPrivateAddress = "site.update_check.allow_private_address"
	KeyIntervalSeconds     = "site.update_check.interval_seconds"
	KeyChannel             = "update.channel"
	defaultChannel         = "stable"
	channelBeta            = "beta"
	MinCheckInterval       = 60 * time.Second
	fetchTimeout           = 10 * time.Second
	maxManifestBytes       = 1 << 20
	maxNotesRunes          = 4096
	checkLockTTL           = 25 * time.Second
	waitForLeaderTimeout   = 15 * time.Second
	cacheKey               = "newtg:update_check:latest"
	checkLockKey           = "newtg:update_check:lock"
	forceTimestampKey      = "newtg:update_check:forced_at"
	updateCheckUserAgent   = "newtg-update-check/1"
)

// 检查状态（modules-08 §3.15.3）。
const (
	StateDisabled        = "disabled"
	StateSourceUnset     = "source_unset"
	StateUpToDate        = "up_to_date"
	StateUpdateAvailable = "update_available"
	StateIncomparable    = "incomparable"
	StateError           = "error"
)

// 错误分类（前端按 kind 选本地化文案，不解析 error 文本）。
const (
	ErrorNetwork     = "network"
	ErrorTimeout     = "timeout"
	ErrorRateLimit   = "rate_limit"
	ErrorPayload     = "payload"
	ErrorSSRFBlocked = "ssrf_blocked"
	ErrorInProgress  = "in_progress"
)

// ConfigReader 与 configcenter.Center / settings 读取面的方法集一致。
type ConfigReader interface {
	Get(key string) (json.RawMessage, bool)
}

// Config 是 Checker 的装配输入。
type Config struct {
	Reader ConfigReader
	Redis  *redis.Client
	// CurrentVersion 是构建注入的当前版本（version.Version）。
	CurrentVersion string
	Now            func() time.Time
	Logger         *slog.Logger
	// ClientFactory 是出站客户端接缝（测试注入 loopback 客户端）。
	// 生产留 nil：使用 egress.Guard.NewClient，所有请求都经 SSRF 校验拨号。
	ClientFactory func(egress.Policy) *http.Client
}

// Checker 执行一次更新检查并共享 Redis 缓存。可并发使用。
type Checker struct {
	reader         ConfigReader
	rdb            *redis.Client
	currentVersion string
	now            func() time.Time
	log            *slog.Logger
	clientFactory  func(egress.Policy) *http.Client
}

// New 构造 Checker。Redis 必填：缓存共享与「立即检查」最小间隔都依赖它；
// 无缓存会让每个管理员浏览器各出站一次，违背 Q5。
func New(cfg Config) (*Checker, error) {
	if cfg.Redis == nil {
		return nil, errors.New("updatecheck: redis is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Checker{
		reader:         cfg.Reader,
		rdb:            cfg.Redis,
		currentVersion: cfg.CurrentVersion,
		now:            now,
		log:            logger,
		clientFactory:  cfg.ClientFactory,
	}, nil
}

// Manifest 是 NEW TG 自定的更新清单契约（§3.15.2）。
type Manifest struct {
	Latest      string     `json:"latest"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	NotesURL    string     `json:"notes_url,omitempty"`
	Prerelease  bool       `json:"prerelease,omitempty"`
	Notes       string     `json:"notes,omitempty"`
}

// Success 是「上次成功」快照；失败时随缓存保留并在界面并行展示。
type Success struct {
	Latest      string     `json:"latest"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	NotesURL    string     `json:"notes_url,omitempty"`
	Prerelease  bool       `json:"prerelease,omitempty"`
	Notes       string     `json:"notes,omitempty"`
	CheckedAt   time.Time  `json:"checked_at"`
}

// Result 是 GET/POST /api/system/update-check 的响应体。
//
// 刻意不包含 source_url：更新源可能携带内网拓扑或查询串凭据，响应只回
// 「是否已配置」这一布尔事实（任务约束「不泄露凭据」）。
type Result struct {
	Enabled          bool       `json:"enabled"`
	SourceConfigured bool       `json:"source_configured"`
	Channel          string     `json:"channel"`
	CurrentVersion   string     `json:"current_version"`
	State            string     `json:"state"`
	Cached           bool       `json:"cached"`
	CacheState       string     `json:"cache_state"`
	CheckedAt        *time.Time `json:"checked_at,omitempty"`
	Latest           string     `json:"latest,omitempty"`
	PublishedAt      *time.Time `json:"published_at,omitempty"`
	NotesURL         string     `json:"notes_url,omitempty"`
	Prerelease       bool       `json:"prerelease,omitempty"`
	Notes            string     `json:"notes,omitempty"`

	// PrereleaseSkipped 表示清单是预发布但站点通道为 stable（Q8：呈「已是最新」
	// 并只记跳过）。
	PrereleaseSkipped bool `json:"prerelease_skipped,omitempty"`

	Error     string `json:"error,omitempty"`
	ErrorKind string `json:"error_kind,omitempty"`

	LastSuccess *Success `json:"last_success,omitempty"`

	MinCheckIntervalSeconds int `json:"min_check_interval_seconds"`
}

// RateLimitError 表示「立即检查」未满 60s 最小间隔；HTTP 层映射 429。
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("updatecheck: rate limited, retry after %s", e.RetryAfter)
}

// IsRateLimit 报告错误是否为最小间隔限流。
func IsRateLimit(err error) (*RateLimitError, bool) {
	var target *RateLimitError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// ErrInProgress 表示本次读取没有缓存、且另一副本正在抓取；HTTP 层映射 503。
var ErrInProgress = errors.New("updatecheck: another instance is checking")

type cacheEntry struct {
	Manifest    *Manifest `json:"manifest,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
	ErrorKind   string    `json:"error_kind,omitempty"`
	LastSuccess *Success  `json:"last_success,omitempty"`
}

// Check 执行一次读取（force=false，命中缓存直接返回）或立即检查
// （force=true，绕缓存但受 60s 最小间隔限制）。
func (c *Checker) Check(ctx context.Context, force bool) (Result, error) {
	cfg := c.readConfig()
	if !cfg.enabled {
		return c.baseResult(cfg, StateDisabled), nil
	}
	if cfg.source == "" {
		return c.baseResult(cfg, StateSourceUnset), nil
	}

	if force {
		// 最小间隔只约束「立即检查」：自动读取不应把管理员的连续刷新
		// 变成额外出站。先占用时间戳，失败也保持 60s 冷却（避免失败重试
		// 打爆版本源）。
		allowed, retryAfter, err := c.acquireForceSlot(ctx)
		if err != nil {
			return Result{}, err
		}
		if !allowed {
			return Result{}, &RateLimitError{RetryAfter: retryAfter}
		}
	}

	now := c.now().UTC()
	previous, previousFound := c.loadCache(ctx)

	if !force && previousFound {
		if !c.entryStale(previous, now, cfg.interval) {
			return c.resultFromCache(cfg, previous, "fresh"), nil
		}
	}

	// 单飞：两个管理员同时打开页面时只允许一个副本出站；其余等待缓存。
	acquired, err := c.rdb.SetNX(ctx, checkLockKey, "1", checkLockTTL).Result()
	if err != nil {
		return Result{}, fmt.Errorf("updatecheck: acquire check lock: %w", err)
	}
	if !acquired {
		if entry, ok := c.waitForCache(ctx, cfg.interval); ok {
			return c.resultFromCache(cfg, entry, c.cacheState(entry, now, cfg.interval)), nil
		}
		return Result{}, ErrInProgress
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.rdb.Del(releaseCtx, checkLockKey).Err()
	}()

	// 等待锁期间另一副本可能已经写入缓存（GET 路径直接复用，避免第二次出站）。
	if !force {
		if entry, ok := c.loadCache(ctx); ok && !c.entryStale(entry, now, cfg.interval) {
			return c.resultFromCache(cfg, entry, "fresh"), nil
		}
	}

	manifest, errorKind := c.fetch(ctx, cfg)
	entry := cacheEntry{CheckedAt: now, ErrorKind: errorKind}
	if previousFound {
		entry.LastSuccess = previous.lastSuccess()
	}
	if errorKind == "" {
		entry.Manifest = manifest
		entry.LastSuccess = &Success{
			Latest:      manifest.Latest,
			PublishedAt: manifest.PublishedAt,
			NotesURL:    manifest.NotesURL,
			Prerelease:  manifest.Prerelease,
			Notes:       manifest.Notes,
			CheckedAt:   now,
		}
	}
	c.saveCache(ctx, entry, cfg.interval)

	return c.resultFromCache(cfg, entry, "miss"), nil
}

type runtimeConfig struct {
	enabled      bool
	source       string
	allowPrivate bool
	interval     time.Duration
	channel      string
}

func (c *Checker) readConfig() runtimeConfig {
	intervalSeconds := c.settingInt(KeyIntervalSeconds)
	if intervalSeconds < 300 {
		intervalSeconds = 300
	}
	if intervalSeconds > 86400 {
		intervalSeconds = 86400
	}
	channel := c.settingString(KeyChannel)
	if channel != channelBeta {
		channel = defaultChannel
	}
	return runtimeConfig{
		enabled:      c.settingBool(KeyEnabled),
		source:       strings.TrimSpace(c.settingString(KeySourceURL)),
		allowPrivate: c.settingBool(KeyAllowPrivateAddress),
		interval:     time.Duration(intervalSeconds) * time.Second,
		channel:      channel,
	}
}

func (c *Checker) settingBool(key string) bool {
	def, _ := settingDefault(key).(bool)
	if c.reader == nil {
		return def
	}
	raw, ok := c.reader.Get(key)
	if !ok {
		return def
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return def
	}
	return value
}

func (c *Checker) settingString(key string) string {
	def, _ := settingDefault(key).(string)
	if c.reader == nil {
		return def
	}
	raw, ok := c.reader.Get(key)
	if !ok {
		return def
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return def
	}
	return value
}

func (c *Checker) settingInt(key string) int64 {
	def, _ := settingDefault(key).(int64)
	if c.reader == nil {
		return def
	}
	raw, ok := c.reader.Get(key)
	if !ok {
		return def
	}
	var value int64
	if json.Unmarshal(raw, &value) != nil {
		return def
	}
	return value
}

// settingDefault 从注册表取唯一真源的默认值。
func settingDefault(key string) any {
	if setting, ok := settings.Lookup(key); ok {
		return settings.DefaultValue(setting)
	}
	return nil
}

func (c *Checker) baseResult(cfg runtimeConfig, state string) Result {
	return Result{
		Enabled:                 cfg.enabled,
		SourceConfigured:        cfg.source != "",
		Channel:                 cfg.channel,
		CurrentVersion:          c.currentVersion,
		State:                   state,
		CacheState:              "miss",
		MinCheckIntervalSeconds: int(MinCheckInterval / time.Second),
	}
}

func (c *Checker) entryStale(entry cacheEntry, now time.Time, interval time.Duration) bool {
	return entry.CheckedAt.IsZero() || now.Sub(entry.CheckedAt) > interval
}

func (c *Checker) cacheState(entry cacheEntry, now time.Time, interval time.Duration) string {
	if c.entryStale(entry, now, interval) {
		return "stale"
	}
	return "fresh"
}

func (c *Checker) loadCache(ctx context.Context) (cacheEntry, bool) {
	raw, err := c.rdb.Get(ctx, cacheKey).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			c.log.Warn("update check: cache read failed", "error", err)
		}
		return cacheEntry{}, false
	}
	var entry cacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		// 损坏的缓存不能当成有效结果：删掉后按 miss 处理（下一次出站重建）。
		c.log.Warn("update check: cache entry corrupt", "error", err)
		_ = c.rdb.Del(ctx, cacheKey).Err()
		return cacheEntry{}, false
	}
	return entry, true
}

func (c *Checker) saveCache(ctx context.Context, entry cacheEntry, ttl time.Duration) {
	raw, err := json.Marshal(entry)
	if err != nil {
		c.log.Warn("update check: cache marshal failed", "error", err)
		return
	}
	if err := c.rdb.Set(ctx, cacheKey, raw, ttl).Err(); err != nil {
		// 缓存写失败不改变本次响应：结果已经拿到，只是下一次会再出站。
		c.log.Warn("update check: cache write failed", "error", err)
	}
}

func (e cacheEntry) lastSuccess() *Success {
	if e.LastSuccess != nil {
		return e.LastSuccess
	}
	if e.Manifest == nil {
		return nil
	}
	snapshot := &Success{
		Latest:      e.Manifest.Latest,
		PublishedAt: e.Manifest.PublishedAt,
		NotesURL:    e.Manifest.NotesURL,
		Prerelease:  e.Manifest.Prerelease,
		Notes:       e.Manifest.Notes,
		CheckedAt:   e.CheckedAt,
	}
	return snapshot
}

func (c *Checker) acquireForceSlot(ctx context.Context) (bool, time.Duration, error) {
	ok, err := c.rdb.SetNX(ctx, forceTimestampKey, c.now().UTC().Format(time.RFC3339), MinCheckInterval).Result()
	if err != nil {
		return false, 0, fmt.Errorf("updatecheck: force interval lock: %w", err)
	}
	if ok {
		return true, 0, nil
	}
	ttl, err := c.rdb.TTL(ctx, forceTimestampKey).Result()
	if err != nil || ttl <= 0 {
		ttl = MinCheckInterval
	}
	if ttl > MinCheckInterval {
		ttl = MinCheckInterval
	}
	// 向上取整：TTL 经常是 59.8s，显示 59 会让按钮在仍被拒绝时解禁。
	retry := time.Duration(int64(ttl/time.Second)) * time.Second
	if ttl%time.Second != 0 {
		retry += time.Second
	}
	if retry <= 0 {
		retry = time.Second
	}
	return false, retry, nil
}

func (c *Checker) waitForCache(ctx context.Context, interval time.Duration) (cacheEntry, bool) {
	deadline := time.NewTimer(waitForLeaderTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return cacheEntry{}, false
		case <-deadline.C:
			return cacheEntry{}, false
		case <-ticker.C:
			if entry, ok := c.loadCache(ctx); ok {
				return entry, true
			}
		}
	}
}

func (c *Checker) resultFromCache(cfg runtimeConfig, entry cacheEntry, cacheState string) Result {
	result := c.baseResult(cfg, "")
	result.Cached = cacheState != "miss"
	result.CacheState = cacheState
	if !entry.CheckedAt.IsZero() {
		checked := entry.CheckedAt.UTC()
		result.CheckedAt = &checked
	}
	result.LastSuccess = entry.LastSuccess
	if entry.ErrorKind != "" {
		result.State = StateError
		result.ErrorKind = entry.ErrorKind
		result.Error = errorMessage(entry.ErrorKind)
		return result
	}
	if entry.Manifest == nil {
		result.State = StateError
		result.ErrorKind = ErrorPayload
		result.Error = errorMessage(ErrorPayload)
		return result
	}
	result.Latest = entry.Manifest.Latest
	result.PublishedAt = entry.Manifest.PublishedAt
	result.NotesURL = entry.Manifest.NotesURL
	result.Prerelease = entry.Manifest.Prerelease
	result.Notes = entry.Manifest.Notes

	current, ok := ParseSemver(c.currentVersion)
	if !ok || current.IsZero() {
		// Q4：当前版本不可解析或 0.0.0 → 「无法比较」，绝不报有更新。
		result.State = StateIncomparable
		return result
	}
	latest, ok := ParseSemver(entry.Manifest.Latest)
	if !ok {
		result.State = StateError
		result.ErrorKind = ErrorPayload
		result.Error = errorMessage(ErrorPayload)
		return result
	}
	if entry.Manifest.Prerelease && cfg.channel != channelBeta {
		result.State = StateUpToDate
		result.PrereleaseSkipped = true
		return result
	}
	if CompareSemver(current, latest) < 0 {
		result.State = StateUpdateAvailable
		return result
	}
	result.State = StateUpToDate
	return result
}

func errorMessage(kind string) string {
	switch kind {
	case ErrorNetwork:
		return "update source request failed"
	case ErrorTimeout:
		return "update source request timed out"
	case ErrorRateLimit:
		return "update source rate-limited the request"
	case ErrorPayload:
		return "update source returned an invalid manifest"
	case ErrorSSRFBlocked:
		return "update source blocked by outbound security policy"
	case ErrorInProgress:
		return "another instance is checking for updates"
	default:
		return "update check failed"
	}
}

// fetch 通过 egress 守卫抓取清单。返回值 errorKind 为空表示成功；具体底层
// 错误只进日志，绝不写进响应（避免 URL/查询串泄露）。
func (c *Checker) fetch(ctx context.Context, cfg runtimeConfig) (*Manifest, string) {
	policy := c.policy(cfg.allowPrivate, cfg.source)
	guard := egress.NewGuard(policy)

	// ValidateURL 会解析并校验全部候选 IP。生产拨号还会在 DialContext 再次
	// 校验（防 DNS rebinding），这里的提前校验用于尽早给出 ssrf_blocked。
	if _, err := guard.ValidateURL(ctx, cfg.source); err != nil {
		c.log.Warn("update check: source rejected by outbound policy", "error", err)
		if errors.Is(err, egress.ErrBlocked) || errors.Is(err, egress.ErrRedirect) {
			return nil, ErrorSSRFBlocked
		}
		if errors.Is(err, egress.ErrUnresolved) {
			return nil, ErrorNetwork
		}
		if isTimeout(err) {
			return nil, ErrorTimeout
		}
		return nil, ErrorNetwork
	}

	client := c.clientFor(policy)
	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, cfg.source, nil)
	if err != nil {
		return nil, ErrorNetwork
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", updateCheckUserAgent)

	response, err := client.Do(request)
	if err != nil {
		// 不记录 err.Error()：*url.Error 会拼上完整 URL（含查询串），可能把
		// 更新源里的凭据写进日志。只记录分类结果与错误类型。
		c.log.Warn("update check: fetch failed", "error_type", fmt.Sprintf("%T", err))
		switch {
		case isTimeout(err):
			return nil, ErrorTimeout
		case errors.Is(err, egress.ErrBlocked), errors.Is(err, egress.ErrRedirect):
			return nil, ErrorSSRFBlocked
		default:
			return nil, ErrorNetwork
		}
	}
	defer func() { _ = response.Body.Close() }()

	switch {
	case response.StatusCode == http.StatusTooManyRequests, response.StatusCode == http.StatusForbidden:
		return nil, ErrorRateLimit
	case response.StatusCode != http.StatusOK:
		c.log.Warn("update check: unexpected source status", "status", response.StatusCode)
		return nil, ErrorNetwork
	}

	// 多读 1 字节判断溢出；不把整个超大响应读进内存。
	body, err := io.ReadAll(io.LimitReader(response.Body, maxManifestBytes+1))
	if err != nil {
		if isTimeout(err) {
			return nil, ErrorTimeout
		}
		return nil, ErrorNetwork
	}
	if len(body) > maxManifestBytes {
		c.log.Warn("update check: manifest too large", "bytes", len(body))
		return nil, ErrorPayload
	}

	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		c.log.Warn("update check: manifest is not valid JSON", "error", err)
		return nil, ErrorPayload
	}
	if kind := validateManifest(&manifest); kind != "" {
		return nil, kind
	}
	return &manifest, ""
}

// policy 把全局 sec.ssrf.* 与功能级 allow_private_address 合并。
// 后者只影响本次出站的 Policy 副本——绝不写回配置中心或全局缓存（Q2）。
//
// 为什么还要把源主机追加到 allowlist：egress 的 allow_private_ip 语义是
// 「只有显式列入 allowlist 的主机才可访问私网」。默认 domain_mode=allowlist，
// 若只抬高 AllowPrivateIP，管理员开启本卡豁免后仍会被全局域名清单拒绝，
// 与 Q2「开启后仅本功能可达」相悖。这里只把管理员本次配置的那一个主机
// 追加进**本次出站副本**，不放宽其它主机，也不改变全局策略。
func (c *Checker) policy(allowPrivate bool, source string) egress.Policy {
	policy := egress.PolicyFromConfig(c.reader)
	if !allowPrivate {
		return policy
	}
	policy.AllowPrivateIP = true
	if policy.DomainMode == egress.DomainAllowlist {
		if parsed, err := url.Parse(strings.TrimSpace(source)); err == nil {
			host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
			if host != "" {
				policy.DomainList = append(policy.DomainList, host)
			}
		}
	}
	return policy
}

func (c *Checker) clientFor(policy egress.Policy) *http.Client {
	if c.clientFactory != nil {
		return c.clientFactory(policy)
	}
	return egress.NewGuard(policy).NewClient(egress.ClientOptions{Timeout: fetchTimeout})
}

func validateManifest(manifest *Manifest) string {
	manifest.Latest = strings.TrimSpace(manifest.Latest)
	if _, ok := ParseSemver(manifest.Latest); !ok {
		return ErrorPayload
	}
	if manifest.NotesURL != "" {
		notes, err := url.Parse(strings.TrimSpace(manifest.NotesURL))
		if err != nil || !notes.IsAbs() || notes.Host == "" ||
			strings.ToLower(notes.Scheme) != "https" || notes.User != nil || notes.Fragment != "" {
			return ErrorPayload
		}
		manifest.NotesURL = notes.String()
	}
	if utf8.RuneCountInString(manifest.Notes) > maxNotesRunes {
		return ErrorPayload
	}
	return ""
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
