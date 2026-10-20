// 性能管理端点（modules-08 §3.19 / F-08-48～F-08-50）。
//
//	GET    /api/performance/stats               settings:manage（只读）
//	DELETE /api/performance/disk-cache          settings:manage + X-Confirm-Action
//	POST   /api/performance/gc                  settings:manage + X-Confirm-Action
//	GET    /api/performance/logs                settings:manage（只读）
//	DELETE /api/performance/logs                settings:manage + X-Confirm-Action
//	GET    /api/performance/recent-operations   settings:manage（只读）
//
// 三个对象（磁盘缓存文件 / 运行时 GC / 应用日志文件）都是**进程本地**的：
// 端点只作用于当前实例，请求里的 instance_id 与当前实例不一致时返回
// 409 INSTANCE_MISMATCH 且零副作用（§3.19 Q4）；跨实例批量运维不在本期。
//
// 配置全部来自部署环境变量，**不落 settings 键**（§3.19 Q1 / G-CFG-015）。
// 因此这里直接 os.Getenv，而不是改 config 冻结集或给 settings 注册表加键。
//
// 安全顺序（D3-08）：三个写操作先校验 X-Confirm-Action，再校验实例、最后才
// 读配置/列目录/删除——缺确认头的请求在任何文件系统访问之前就被拒绝。
package httpapi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/rbac"
)

const (
	// diskCacheInactiveWindow 是「不活跃」阈值：mtime 早于 now-10min 才删
	// （§3.19 Q7「与参照侧同口径」）。为什么是 10 分钟：正在写入/仍可能被
	// 进行中的请求引用的缓存文件会持续刷新 mtime，10 分钟足以覆盖一次慢
	// 请求的落盘尾期；阈值更短会把活跃文件误判为陈旧，清理反过来伤到请求。
	diskCacheInactiveWindow = 10 * time.Minute

	// defaultLogFilePattern 是应用日志文件名白名单（§3.19 Q6 的【假设】，
	// D3-01 冻结）。与参照侧 oneapi-*.log 同构；可用 LOG_FILE_PATTERN 覆盖。
	defaultLogFilePattern = "newtg-*.log"

	// performanceWriteRateLimit 是三个写操作的管理员维度限流。设置写端点
	// 现状没有可复用的限流中间件（D3-09 的兜底口径），故按 10/min/admin。
	performanceWriteRateLimit  = 10
	performanceWriteRateWindow = time.Minute
	performanceRateMark        = "PF"

	performanceDefaultRecentLimit = 20
	performanceMaxRecentLimit     = 100

	// performanceMaxCleanupValue 是日志清理 value 的上限（100 年）。
	//
	// by_days 用 time.Duration(value) * 24 * time.Hour 计算截止时间，而
	// int64 纳秒在约 106752 天（≈292 年）时溢出：更大的 value 会让截止时间
	// 绕回未来，删除集合与意图完全相反。100 年既覆盖任何真实运维窗口，也把
	// 乘法留在安全区间。by_count 同样套用该上限：万级文件数的保留集没有
	// 实际意义，统一上限让 query 参数的契约只有一条边界。
	performanceMaxCleanupValue = 36500
)

// performanceProblemType* 是 §3.19.2 错误码的 RFC 7807 type。detail 同时以
// 「CODE:」前缀携带大写下划线码，前端可按 type 或 detail 稳定映射。
const (
	problemTypeConfirmRequired         = "https://newtg.dev/problems/confirm_required"
	problemTypeInstanceMismatch        = "https://newtg.dev/problems/instance_mismatch"
	problemTypeInvalidMode             = "https://newtg.dev/problems/invalid_mode"
	problemTypePerformanceInvalidValue = "https://newtg.dev/problems/invalid_value"
	problemTypeDiskCacheNotConfigured  = "https://newtg.dev/problems/disk_cache_not_configured"
	problemTypeLogDirNotConfigured     = "https://newtg.dev/problems/log_dir_not_configured"
)

// 性能面的 RBAC 动作。ResourceKind 固定 performance；ActionName 区分读与
// 三个写操作——字段策略按 (ResourceKind, ActionName) 登记，写动作必须登记
// 否则 authz 失败关闭（security-02 §2.6.1 规则 6）。
var (
	performanceReadAction       = rbac.AuthzAction{ResourceKind: "performance", ActionName: "read"}
	performanceDiskClearAction  = rbac.AuthzAction{ResourceKind: "performance", ActionName: "clear_disk_cache"}
	performanceGCAction         = rbac.AuthzAction{ResourceKind: "performance", ActionName: "run_gc"}
	performanceLogCleanupAction = rbac.AuthzAction{ResourceKind: "performance", ActionName: "cleanup_logs"}
)

// PerformanceHandlerConfig 是性能端点的装配输入。
type PerformanceHandlerConfig struct {
	// LogDB 是 audit_logs 所在日志库（recent-operations 的数据源）。
	// nil 时该端点返回空列表（单测/未装配日志库的部署）。
	LogDB  *sql.DB
	Audit  *audit.Logger
	Logger *slog.Logger
	Redis  *redis.Client
	Authz  *rbac.AuthzMiddleware
	// Now 是 10 分钟阈值、mtime 截止与耗时测量的时钟接缝；nil 时 time.Now。
	Now func() time.Time
	// Getenv 是环境变量读取接缝（D3-01 直接 os.Getenv）；nil 时 os.Getenv。
	Getenv func(string) string
	// Hostname 是 INSTANCE_ID 缺省值的来源；nil 时 os.Hostname。
	Hostname func() (string, error)
	// Remove 是单文件删除接缝（测试注入失败以覆盖 failed_files）；nil 时 os.Remove。
	Remove func(string) error
	// RuntimeGC 是运行时 GC 接缝；nil 时 runtime.GC。
	//
	// 为什么需要接缝：实例不匹配的 409 用例要证明请求停在 GC 之前，而进程
	// 全局的 MemStats.NumGC 会被循环自身分配触发的自动 GC 推进，用它做断言
	// 会随机 flake（D3a 复核 F1）。注入 stub 计数才是确定性的观测。
	RuntimeGC func()
}

// PerformanceHandlers 持有性能端点依赖，可并发使用。
type PerformanceHandlers struct {
	logDB *sql.DB
	audit *audit.Logger
	log   *slog.Logger
	rdb   *redis.Client
	authz *rbac.AuthzMiddleware
	now   func() time.Time
	geten func(string) string
	host  func() (string, error)
	rm    func(string) error
	gc    func()
	// lastClearedAt 是本进程最近一次磁盘缓存清理的完成时间（UnixNano）。
	// 0 = 从未清理。它是进程本地观测值：重启后归零，与三个对象本身一样
	// 不跨副本、不落库（§3.19 Q4）。
	lastClearedAt atomic.Int64
}

// NewPerformanceHandlers 构造端点。
func NewPerformanceHandlers(cfg PerformanceHandlerConfig) *PerformanceHandlers {
	authz := cfg.Authz
	if authz == nil {
		authz = rbac.NewAuthzMiddleware()
	}
	// 写侧字段白名单：DELETE 端点无请求体；GC 接受可选的 instance_id。
	// 空 Fields 也必须登记（authz 对未登记写动作失败关闭）。
	authz.SetWritePolicy(performanceDiskClearAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{}})
	authz.SetWritePolicy(performanceGCAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"instance_id": nil,
	}})
	authz.SetWritePolicy(performanceLogCleanupAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{}})

	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	getenv := cfg.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	hostname := cfg.Hostname
	if hostname == nil {
		hostname = os.Hostname
	}
	remove := cfg.Remove
	if remove == nil {
		remove = os.Remove
	}
	runGC := cfg.RuntimeGC
	if runGC == nil {
		runGC = runtime.GC
	}
	return &PerformanceHandlers{
		logDB: cfg.LogDB, audit: cfg.Audit, log: log, rdb: cfg.Redis,
		authz: authz, now: now, geten: getenv, host: hostname, rm: remove, gc: runGC,
	}
}

// Register 把六个端点挂到受保护管理子树。权限一律 settings:manage
// （§3.19 Q2：不新增 ops:*/performance:* 键）。
func (h *PerformanceHandlers) Register(mux *http.ServeMux) {
	mux.Handle("GET /api/performance/stats",
		h.authz.Handler(rbac.PermSettingsManage, performanceReadAction)(http.HandlerFunc(h.handleStats)))
	mux.Handle("GET /api/performance/logs",
		h.authz.Handler(rbac.PermSettingsManage, performanceReadAction)(http.HandlerFunc(h.handleLogs)))
	mux.Handle("GET /api/performance/recent-operations",
		h.authz.Handler(rbac.PermSettingsManage, performanceReadAction)(http.HandlerFunc(h.handleRecentOperations)))
	mux.Handle("DELETE /api/performance/disk-cache",
		h.authz.Handler(rbac.PermSettingsManage, performanceDiskClearAction)(http.HandlerFunc(h.handleDiskCacheClear)))
	mux.Handle("POST /api/performance/gc",
		h.authz.Handler(rbac.PermSettingsManage, performanceGCAction)(http.HandlerFunc(h.handleGC)))
	mux.Handle("DELETE /api/performance/logs",
		h.authz.Handler(rbac.PermSettingsManage, performanceLogCleanupAction)(http.HandlerFunc(h.handleLogCleanup)))
}

// performanceConfig 是一次请求的环境变量快照。
type performanceConfig struct {
	diskCacheEnabled bool
	diskCachePath    string
	diskCacheMaxMB   int64
	diskCacheThresh  int64
	logDir           string
	logFilePattern   string
	instanceID       string
}

// config 读取 D3-01 冻结的环境变量。每次请求重读，使部署侧变更无需重启
// （与 settings 不同，这些值不在配置中心，没有广播通道）。
func (h *PerformanceHandlers) config() performanceConfig {
	cfg := performanceConfig{
		diskCacheEnabled: parseEnvBool(h.geten("DISK_CACHE_ENABLED")),
		diskCachePath:    strings.TrimSpace(h.geten("DISK_CACHE_PATH")),
		diskCacheMaxMB:   parseEnvInt64(h.geten("DISK_CACHE_MAX_MB")),
		diskCacheThresh:  parseEnvInt64(h.geten("DISK_CACHE_THRESHOLD_MB")),
		logDir:           strings.TrimSpace(h.geten("LOG_DIR")),
		logFilePattern:   strings.TrimSpace(h.geten("LOG_FILE_PATTERN")),
		instanceID:       strings.TrimSpace(h.geten("INSTANCE_ID")),
	}
	if cfg.logFilePattern == "" {
		cfg.logFilePattern = defaultLogFilePattern
	}
	if cfg.instanceID == "" {
		// 未显式配置时用主机名（D3-01）。宿主机名的失败不影响端点：回退到
		// 固定占位值，让 instance_id 仍然是一个可比较的稳定字符串。
		if name, err := h.host(); err == nil && strings.TrimSpace(name) != "" {
			cfg.instanceID = strings.TrimSpace(name)
		} else {
			cfg.instanceID = "unknown"
		}
	}
	return cfg
}

func parseEnvBool(raw string) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	return err == nil && value
}

func parseEnvInt64(raw string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

// requireConfirm 是三个写操作的第一道门（§3.19 Q3 / D3-08）：大小写不敏感
// 的 X-Confirm-Action: true，缺失或其他值 422 CONFIRM_REQUIRED。必须在任何
// 副作用（读配置、列目录、删文件、触发 GC、消耗限流名额）之前调用。
func (h *PerformanceHandlers) requireConfirm(w http.ResponseWriter, r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Confirm-Action")), "true") {
		return true
	}
	writeProblemDetail(w, http.StatusUnprocessableEntity, problemTypeConfirmRequired,
		"Confirmation required",
		"CONFIRM_REQUIRED: the X-Confirm-Action: true header is required for this operation")
	return false
}

// requireInstance 校验请求携带的 instance_id 是否就是当前实例（§3.19 Q4）。
// 不匹配 409 且调用方必须在任何副作用之前返回；空值表示「未指定」。
func (h *PerformanceHandlers) requireInstance(w http.ResponseWriter, requested, current string) bool {
	requested = strings.TrimSpace(requested)
	if requested == "" || requested == current {
		return true
	}
	writeProblemDetail(w, http.StatusConflict, problemTypeInstanceMismatch,
		"Instance mismatch",
		"INSTANCE_MISMATCH: this operation only applies to the current instance")
	return false
}

// allowWrite 消耗管理员维度的固定窗口名额（10/min/admin）。Redis 不可用时
// fail-open：与其它管理面探测一致，限流不是授权边界。
func (h *PerformanceHandlers) allowWrite(r *http.Request) (bool, time.Duration) {
	if h.rdb == nil {
		return false, 0
	}
	key := RateLimitKeyPrefix + performanceRateMark + ":admin:" + strconv.FormatInt(actorID(r), 10)
	limited, retry, err := allowRequestRate(r.Context(), h.rdb, key, performanceWriteRateLimit, performanceWriteRateWindow)
	if err != nil {
		h.log.Warn("performance: rate limiter unavailable; allowing operation", "err", err)
		return false, 0
	}
	return limited, retry
}

// ---------------------------------------------------------------------------
// GET /api/performance/stats
// ---------------------------------------------------------------------------

type performanceCacheStats struct {
	Enabled       bool       `json:"enabled"`
	FileCount     int        `json:"file_count"`
	TotalBytes    int64      `json:"total_bytes"`
	LastClearedAt *time.Time `json:"last_cleared_at"`
}

type performanceMemoryStats struct {
	AllocBytes   uint64 `json:"alloc_bytes"`
	SysBytes     uint64 `json:"sys_bytes"`
	NumGC        uint32 `json:"num_gc"`
	NumGoroutine int    `json:"num_goroutine"`
}

type performanceDiskCacheInfo struct {
	Path         string     `json:"path"`
	FileCount    int        `json:"file_count"`
	TotalBytes   int64      `json:"total_bytes"`
	LastModified *time.Time `json:"last_modified"`
}

type performanceDiskSpaceInfo struct {
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
	UsedBytes  uint64 `json:"used_bytes"`
}

type performanceConfigView struct {
	Enabled     bool   `json:"enabled"`
	Path        string `json:"path"`
	MaxMB       int64  `json:"max_mb"`
	ThresholdMB int64  `json:"threshold_mb"`
}

type performanceStatsResponse struct {
	InstanceID    string                   `json:"instance_id"`
	CacheStats    performanceCacheStats    `json:"cache_stats"`
	MemoryStats   performanceMemoryStats   `json:"memory_stats"`
	DiskCacheInfo performanceDiskCacheInfo `json:"disk_cache_info"`
	DiskSpaceInfo performanceDiskSpaceInfo `json:"disk_space_info"`
	Config        performanceConfigView    `json:"config"`
}

func (h *PerformanceHandlers) handleStats(w http.ResponseWriter, r *http.Request) {
	cfg := h.config()
	resp := performanceStatsResponse{
		InstanceID: cfg.instanceID,
		Config: performanceConfigView{
			Enabled:     cfg.diskCacheEnabled,
			Path:        cfg.diskCachePath,
			MaxMB:       cfg.diskCacheMaxMB,
			ThresholdMB: cfg.diskCacheThresh,
		},
	}

	// 目录不存在/不可读不报 500（D3-03）：cache_stats.enabled=false 且
	// disk_cache_info 保持零值，让 UI 进入说明性空态而不是整页错误。
	if cfg.diskCacheEnabled && cfg.diskCachePath != "" {
		files, err := scanRegularFiles(cfg.diskCachePath, nil)
		if err == nil {
			resp.CacheStats.Enabled = true
			resp.DiskCacheInfo = performanceDiskCacheInfo{Path: cfg.diskCachePath}
			for _, file := range files {
				resp.CacheStats.FileCount++
				resp.CacheStats.TotalBytes += file.size
				resp.DiskCacheInfo.FileCount++
				resp.DiskCacheInfo.TotalBytes += file.size
				if resp.DiskCacheInfo.LastModified == nil || file.modTime.After(*resp.DiskCacheInfo.LastModified) {
					modified := file.modTime
					resp.DiskCacheInfo.LastModified = &modified
				}
			}
		}
	}
	if nanos := h.lastClearedAt.Load(); nanos > 0 {
		cleared := time.Unix(0, nanos).UTC()
		resp.CacheStats.LastClearedAt = &cleared
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	resp.MemoryStats = performanceMemoryStats{
		AllocBytes:   mem.Alloc,
		SysBytes:     mem.Sys,
		NumGC:        mem.NumGC,
		NumGoroutine: runtime.NumGoroutine(),
	}

	// 磁盘空间按缓存目录所在的文件系统统计；缓存未配置时退回根文件系统，
	// 保证 dashboard 仍有可展示的容量（字段恒存在，避免前端分支）。
	spacePath := cfg.diskCachePath
	if spacePath == "" {
		spacePath = "/"
	}
	resp.DiskSpaceInfo = diskSpace(spacePath)

	writeJSON(w, http.StatusOK, resp)
}

// diskSpace 用 statfs 读取挂载点容量。失败时返回零值而不是 500：容量是
// 展示性信息，不应拖垮整张统计卡（D3-03 的空态口径）。
func diskSpace(path string) performanceDiskSpaceInfo {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return performanceDiskSpaceInfo{}
	}
	blockSize := uint64(stat.Bsize)
	total := stat.Blocks * blockSize
	// Bfree 是文件系统空闲块，Bavail 是非特权进程可用块；面向运维的
	// free_bytes 取 Bavail（真正可写入的空间），used 仍按 Bfree 计算。
	free := stat.Bavail * blockSize
	used := (stat.Blocks - stat.Bfree) * blockSize
	return performanceDiskSpaceInfo{TotalBytes: total, FreeBytes: free, UsedBytes: used}
}

// ---------------------------------------------------------------------------
// DELETE /api/performance/disk-cache
// ---------------------------------------------------------------------------

type performanceDiskClearResponse struct {
	InstanceID   string   `json:"instance_id"`
	DeletedCount int      `json:"deleted_count"`
	FreedBytes   int64    `json:"freed_bytes"`
	FailedFiles  []string `json:"failed_files"`
}

func (h *PerformanceHandlers) handleDiskCacheClear(w http.ResponseWriter, r *http.Request) {
	// 顺序冻结：确认头 → 实例 → 配置 → 列目录 → 删除。确认头在读取任何
	// 配置之前判定，缺头请求零副作用（D3-08）。
	if !h.requireConfirm(w, r) {
		return
	}
	cfg := h.config()
	if !h.requireInstance(w, r.URL.Query().Get("instance_id"), cfg.instanceID) {
		return
	}
	if !cfg.diskCacheEnabled || cfg.diskCachePath == "" {
		writeProblemDetail(w, http.StatusUnprocessableEntity, problemTypeDiskCacheNotConfigured,
			"Disk cache is not configured",
			"DISK_CACHE_NOT_CONFIGURED: set DISK_CACHE_ENABLED=true and DISK_CACHE_PATH to manage the disk cache")
		return
	}
	if limited, retry := h.allowWrite(r); limited {
		w.Header().Set("Retry-After", retrySeconds(retry))
		writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests, "Too many requests")
		return
	}

	files, err := scanRegularFiles(cfg.diskCachePath, nil)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// 目录被配置为启用却不可读：属于部署/权限故障，要给管理员一个
			// 可诊断的 500（而 cache_stats 的展示面仍按空态收敛）。
			h.log.Error("performance: read disk cache directory failed", "path", cfg.diskCachePath, "err", err)
			writeProblemDetail(w, http.StatusInternalServerError, authProblemInternal,
				"Internal Server Error", "")
			return
		}
		files = nil
	}

	// 10 分钟阈值只用于「不活跃」判定（Q7）；mtime 严格早于 cutoff 才删，
	// 恰好在边界上的文件保留（测试固定了该边界语义）。
	cutoff := h.now().Add(-diskCacheInactiveWindow)
	resp := performanceDiskClearResponse{
		InstanceID:  cfg.instanceID,
		FailedFiles: []string{},
	}
	for _, file := range files {
		if !file.modTime.Before(cutoff) {
			continue
		}
		if err := h.rm(file.path); err != nil {
			// 扫描后文件已被别的清理者/轮转删除（os.ErrNotExist）不是操作
			// 失败：清理意图已经达成。按 deleted 计数并计入扫描时的字节数，
			// 只有真实错误（权限、IO）才进 failed_files。
			if errors.Is(err, os.ErrNotExist) {
				resp.DeletedCount++
				resp.FreedBytes += file.size
				continue
			}
			// 单个删除失败不吞掉、也不让整次清理失败（Q7）：列进 failed_files，
			// 已成功的删除保持生效，UI 展示部分失败。
			resp.FailedFiles = append(resp.FailedFiles, file.name)
			continue
		}
		resp.DeletedCount++
		resp.FreedBytes += file.size
	}
	h.lastClearedAt.Store(h.now().UnixNano())

	h.writePerformanceAudit(r, audit.ActionSystemDiskCacheClear, http.StatusOK, map[string]any{
		"instance_id":   cfg.instanceID,
		"deleted_count": resp.DeletedCount,
		"freed_bytes":   resp.FreedBytes,
		"failed_count":  len(resp.FailedFiles),
	})
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// POST /api/performance/gc
// ---------------------------------------------------------------------------

type performanceGCResponse struct {
	InstanceID      string `json:"instance_id"`
	DurationMS      int64  `json:"duration_ms"`
	HeapAllocBefore uint64 `json:"heap_alloc_before"`
	HeapAllocAfter  uint64 `json:"heap_alloc_after"`
	NumGCBefore     uint32 `json:"num_gc_before"`
	NumGCAfter      uint32 `json:"num_gc_after"`
}

func (h *PerformanceHandlers) handleGC(w http.ResponseWriter, r *http.Request) {
	if !h.requireConfirm(w, r) {
		return
	}
	cfg := h.config()

	// body 可省略；只接受 instance_id（其余字段由 authz 字段策略拒绝）。
	var req struct {
		InstanceID string `json:"instance_id"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuthBodyBytes))
	if err != nil {
		writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest,
			"Invalid request", "unable to read request body")
		return
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest,
				"Invalid request", "invalid JSON body")
			return
		}
	}
	if !h.requireInstance(w, req.InstanceID, cfg.instanceID) {
		return
	}
	if limited, retry := h.allowWrite(r); limited {
		w.Header().Set("Retry-After", retrySeconds(retry))
		writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests, "Too many requests")
		return
	}

	// runtime.GC() 同步执行（Q8）；不调用 debug.FreeOSMemory()——它会把
	// 页归还给操作系统，语义超出 §3.19 定义的「运行时 GC」且可能造成更大的
	// 停顿（D3-05 冻结不调用）。
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := h.now()
	h.gc()
	duration := h.now().Sub(started)
	runtime.ReadMemStats(&after)

	resp := performanceGCResponse{
		InstanceID:      cfg.instanceID,
		DurationMS:      duration.Milliseconds(),
		HeapAllocBefore: before.HeapAlloc,
		HeapAllocAfter:  after.HeapAlloc,
		NumGCBefore:     before.NumGC,
		NumGCAfter:      after.NumGC,
	}
	h.writePerformanceAudit(r, audit.ActionSystemGCRun, http.StatusOK, map[string]any{
		"instance_id":       cfg.instanceID,
		"duration_ms":       resp.DurationMS,
		"heap_alloc_before": resp.HeapAllocBefore,
		"heap_alloc_after":  resp.HeapAllocAfter,
		"num_gc_before":     resp.NumGCBefore,
		"num_gc_after":      resp.NumGCAfter,
	})
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// GET /api/performance/logs
// ---------------------------------------------------------------------------

type performanceLogFile struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

type performanceLogsResponse struct {
	Enabled    bool                 `json:"enabled"`
	LogDir     string               `json:"log_dir"`
	FileCount  int                  `json:"file_count"`
	TotalSize  int64                `json:"total_size"`
	OldestTime *time.Time           `json:"oldest_time"`
	NewestTime *time.Time           `json:"newest_time"`
	Files      []performanceLogFile `json:"files"`
}

func (h *PerformanceHandlers) handleLogs(w http.ResponseWriter, r *http.Request) {
	cfg := h.config()
	resp := performanceLogsResponse{Files: []performanceLogFile{}}
	if cfg.logDir == "" {
		// LOG_DIR 未配置 = 生产默认 stdout（Q10）：说明性空态，仍返回 200。
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Enabled = true
	resp.LogDir = cfg.logDir

	files, err := scanLogFiles(cfg.logDir, cfg.logFilePattern)
	if err != nil {
		// 目录不可读同样不伪造数据：返回空列表 + 日志，供 UI 显示空态。
		if !errors.Is(err, os.ErrNotExist) {
			h.log.Warn("performance: read log directory failed", "path", cfg.logDir, "err", err)
		}
	}
	for _, file := range files {
		resp.Files = append(resp.Files, performanceLogFile{
			Name: file.name, Size: file.size, ModTime: file.modTime,
		})
		resp.FileCount++
		resp.TotalSize += file.size
		if resp.OldestTime == nil || file.modTime.Before(*resp.OldestTime) {
			oldest := file.modTime
			resp.OldestTime = &oldest
		}
		if resp.NewestTime == nil || file.modTime.After(*resp.NewestTime) {
			newest := file.modTime
			resp.NewestTime = &newest
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// DELETE /api/performance/logs
// ---------------------------------------------------------------------------

type performanceLogCleanupResponse struct {
	InstanceID    string   `json:"instance_id"`
	Mode          string   `json:"mode"`
	Value         int      `json:"value"`
	DeletedCount  int      `json:"deleted_count"`
	FreedBytes    int64    `json:"freed_bytes"`
	SkippedActive bool     `json:"skipped_active"`
	FailedFiles   []string `json:"failed_files"`
}

func (h *PerformanceHandlers) handleLogCleanup(w http.ResponseWriter, r *http.Request) {
	if !h.requireConfirm(w, r) {
		return
	}
	cfg := h.config()
	query := r.URL.Query()
	if !h.requireInstance(w, query.Get("instance_id"), cfg.instanceID) {
		return
	}

	// mode/value 属于请求参数，先于目录配置校验：非法请求不应因为部署恰好
	// 未开文件日志而得到另一个错误码（前端按码分流）。
	mode := strings.TrimSpace(query.Get("mode"))
	if mode != "by_count" && mode != "by_days" {
		writeProblemDetail(w, http.StatusUnprocessableEntity, problemTypeInvalidMode,
			"Invalid cleanup mode", "INVALID_MODE: mode must be by_count or by_days")
		return
	}
	value, err := strconv.Atoi(strings.TrimSpace(query.Get("value")))
	if err != nil || value < 1 || value > performanceMaxCleanupValue {
		writeProblemDetail(w, http.StatusUnprocessableEntity, problemTypePerformanceInvalidValue,
			"Invalid value",
			"INVALID_VALUE: value must be an integer between 1 and "+strconv.Itoa(performanceMaxCleanupValue))
		return
	}
	if cfg.logDir == "" {
		// Q10：未配置时必须显式 422，而不是静默成功（「无文件可删」会掩盖
		// 部署未启用文件日志的事实）。
		writeProblemDetail(w, http.StatusUnprocessableEntity, problemTypeLogDirNotConfigured,
			"Log directory is not configured",
			"LOG_DIR_NOT_CONFIGURED: file logging is disabled (production default is stdout)")
		return
	}
	if limited, retry := h.allowWrite(r); limited {
		w.Header().Set("Retry-After", retrySeconds(retry))
		writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests, "Too many requests")
		return
	}

	files, err := scanLogFiles(cfg.logDir, cfg.logFilePattern)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			h.log.Error("performance: read log directory failed", "path", cfg.logDir, "err", err)
			writeProblemDetail(w, http.StatusInternalServerError, authProblemInternal,
				"Internal Server Error", "")
			return
		}
		files = nil
	}

	resp := performanceLogCleanupResponse{
		InstanceID:  cfg.instanceID,
		Mode:        mode,
		Value:       value,
		FailedFiles: []string{},
	}
	active := activeLogFile(files)
	candidates := logCleanupCandidates(files, active, mode, value, h.now())

	for _, file := range candidates {
		if active != nil && file.path == active.path {
			// 活动文件永不删除（Q6）。按最新 mtime 判定活动文件是冻结口径
			// （D3-07）：打开句柄的判定不可移植，mtime 是唯一跨平台可用的
			// 近似——正在写入的日志文件恒有最新 mtime。
			resp.SkippedActive = true
			continue
		}
		if err := h.rm(file.path); err != nil {
			// 与磁盘缓存清理同一语义：扫描后消失的文件视为已清理，不算失败。
			if errors.Is(err, os.ErrNotExist) {
				resp.DeletedCount++
				resp.FreedBytes += file.size
				continue
			}
			resp.FailedFiles = append(resp.FailedFiles, file.name)
			continue
		}
		resp.DeletedCount++
		resp.FreedBytes += file.size
	}

	h.writePerformanceAudit(r, audit.ActionSystemLogCleanupRun, http.StatusOK, map[string]any{
		"instance_id":    cfg.instanceID,
		"mode":           resp.Mode,
		"value":          resp.Value,
		"deleted_count":  resp.DeletedCount,
		"freed_bytes":    resp.FreedBytes,
		"failed_count":   len(resp.FailedFiles),
		"skipped_active": resp.SkippedActive,
	})
	writeJSON(w, http.StatusOK, resp)
}

// logCleanupCandidates 计算删除候选集。
//
// by_count：按 mtime 倒序（同 mtime 按文件名倒序，保证确定性），保留最新 N 个；
// by_days：删除 mtime 早于 now-N*24h 的文件。两者都只在白名单普通文件集合内
// 计算（scanLogFiles 已完成过滤）。
func logCleanupCandidates(files []perfFile, active *perfFile, mode string, value int, now time.Time) []perfFile {
	if len(files) == 0 {
		return nil
	}
	switch mode {
	case "by_count":
		sorted := append([]perfFile(nil), files...)
		sortPerfFilesDesc(sorted)
		if value >= len(sorted) {
			return nil
		}
		return sorted[value:]
	case "by_days":
		cutoff := now.Add(-time.Duration(value) * 24 * time.Hour)
		var out []perfFile
		for _, file := range files {
			if file.modTime.Before(cutoff) {
				out = append(out, file)
			}
		}
		return out
	default:
		return nil
	}
}

// activeLogFile 返回最新 mtime 的日志文件（同 mtime 取文件名较大者）。
// nil 表示目录里没有白名单日志文件。
func activeLogFile(files []perfFile) *perfFile {
	if len(files) == 0 {
		return nil
	}
	sorted := append([]perfFile(nil), files...)
	sortPerfFilesDesc(sorted)
	return &sorted[0]
}

func sortPerfFilesDesc(files []perfFile) {
	// 简单插入/标准库排序：文件数是个位到百位量级，无需优化。
	sort.Slice(files, func(i, j int) bool {
		if files[i].modTime.Equal(files[j].modTime) {
			return files[i].name > files[j].name
		}
		return files[i].modTime.After(files[j].modTime)
	})
}

// ---------------------------------------------------------------------------
// GET /api/performance/recent-operations
// ---------------------------------------------------------------------------

type recentOperation struct {
	Time       time.Time `json:"time"`
	Actor      string    `json:"actor"`
	Action     string    `json:"action"`
	Route      string    `json:"route"`
	Result     string    `json:"result"`
	InstanceID string    `json:"instance_id"`
	// Mode/Value 只对日志清理有值，供 D3b「参数摘要」列展示；其余两类操作
	// 的审计 detail 没有这两个键，omitempty 让响应形状保持可选。
	Mode          string `json:"mode,omitempty"`
	Value         int64  `json:"value,omitempty"`
	DeletedCount  int64  `json:"deleted_count"`
	FreedBytes    int64  `json:"freed_bytes"`
	FailedCount   int64  `json:"failed_count"`
	DurationMS    int64  `json:"duration_ms"`
	SkippedActive bool   `json:"skipped_active"`
}

type recentOperationsResponse struct {
	Items []recentOperation `json:"items"`
}

func (h *PerformanceHandlers) handleRecentOperations(w http.ResponseWriter, r *http.Request) {
	limit := performanceDefaultRecentLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > performanceMaxRecentLimit {
			writeProblemDetail(w, http.StatusUnprocessableEntity, problemTypePerformanceInvalidValue,
				"Invalid limit", "INVALID_VALUE: limit must be between 1 and 100")
			return
		}
		limit = parsed
	}

	resp := recentOperationsResponse{Items: []recentOperation{}}
	if h.logDB == nil {
		// 未装配日志库（单测/无日志库的开发装配）：审计面本就不存在，
		// 返回空列表而不是 503——「暂无操作记录」是真实状态。
		writeJSON(w, http.StatusOK, resp)
		return
	}

	const query = `SELECT created_at, username, detail->>'action',
		COALESCE(route, ''), result, detail
		FROM audit_logs
		WHERE category = 'operation' AND detail->>'action' IN ($1, $2, $3)
		ORDER BY created_at DESC, id DESC
		LIMIT $4`
	rows, err := h.logDB.QueryContext(r.Context(), query,
		audit.ActionSystemDiskCacheClear, audit.ActionSystemGCRun, audit.ActionSystemLogCleanupRun, limit)
	if err != nil {
		h.log.Error("performance: query recent operations failed", "err", err)
		writeProblemDetail(w, http.StatusInternalServerError, authProblemInternal,
			"Internal Server Error", "")
		return
	}
	defer rows.Close()

	for rows.Next() {
		var (
			item   recentOperation
			detail []byte
		)
		if err := rows.Scan(&item.Time, &item.Actor, &item.Action, &item.Route,
			&item.Result, &detail); err != nil {
			h.log.Error("performance: scan recent operation failed", "err", err)
			writeProblemDetail(w, http.StatusInternalServerError, authProblemInternal,
				"Internal Server Error", "")
			return
		}
		applyRecentOperationDetail(&item, detail)
		resp.Items = append(resp.Items, item)
	}
	if err := rows.Err(); err != nil {
		h.log.Error("performance: iterate recent operations failed", "err", err)
		writeProblemDetail(w, http.StatusInternalServerError, authProblemInternal,
			"Internal Server Error", "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// applyRecentOperationDetail 从 detail 提取展示字段。detail 由本包写入，
// 但仍按外部数据解析：缺失或类型不符时保留零值，不让一行脏数据打挂列表。
func applyRecentOperationDetail(item *recentOperation, raw []byte) {
	if len(raw) == 0 {
		return
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		return
	}
	if value, ok := detail["instance_id"].(string); ok {
		item.InstanceID = value
	}
	if value, ok := detail["mode"].(string); ok {
		item.Mode = value
	}
	item.Value = jsonInt64(detail["value"])
	item.DeletedCount = jsonInt64(detail["deleted_count"])
	item.FreedBytes = jsonInt64(detail["freed_bytes"])
	item.FailedCount = jsonInt64(detail["failed_count"])
	item.DurationMS = jsonInt64(detail["duration_ms"])
	if value, ok := detail["skipped_active"].(bool); ok {
		item.SkippedActive = value
	}
}

func jsonInt64(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	case string:
		parsed, _ := strconv.ParseInt(typed, 10, 64)
		return parsed
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// 公共辅助
// ---------------------------------------------------------------------------

// perfFile 是目录扫描结果的统一形态。
type perfFile struct {
	name    string
	path    string
	size    int64
	modTime time.Time
}

// scanRegularFiles 列出 dir 下（不递归）的普通文件。match 为 nil 时不过滤。
// 目录、符号链接与其他非常规文件一律不返回——Q6 的白名单语义在扫描层
// 完成，删除路径因此不可能碰到它们。
func scanRegularFiles(dir string, match func(string) bool) ([]perfFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]perfFile, 0, len(entries))
	for _, entry := range entries {
		if match != nil && !match(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			// 列目录后条目消失是正常竞态（日志轮转/别的清理者）：跳过。
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		// 用 Info 的 Mode 而不是 DirEntry.Type：部分文件系统返回 DT_UNKNOWN，
		// Type 会退化成 0（看似普通文件）；Info 的 Lstat 结果是权威判定。
		if !info.Mode().IsRegular() {
			continue
		}
		out = append(out, perfFile{
			name: entry.Name(), path: filepath.Join(dir, entry.Name()),
			size: info.Size(), modTime: info.ModTime(),
		})
	}
	return out, nil
}

// scanLogFiles 只保留匹配 LOG_FILE_PATTERN 的普通文件（Q6）。模式按**文件名**
// 匹配（filepath.Match），不允许模式里带路径分隔符去越出 LOG_DIR。
func scanLogFiles(dir, pattern string) ([]perfFile, error) {
	return scanRegularFiles(dir, func(name string) bool {
		if strings.ContainsAny(pattern, `/\`) {
			return false
		}
		matched, err := filepath.Match(pattern, name)
		return err == nil && matched
	})
}

// writePerformanceAudit 写 §3.19.2 的三类运维审计。before/after 恒为 null
// （运维动作不是字段级变更）；detail 只含计数/字节/耗时/实例标识。
func (h *PerformanceHandlers) writePerformanceAudit(r *http.Request, action string, status int, detail map[string]any) {
	if h.audit == nil {
		return
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return
	}
	_ = writeAuditEvent(r.Context(), h.audit, audit.Event{
		Action:     action,
		Actor:      actorID(r),
		Method:     r.Method,
		Route:      r.URL.Path,
		StatusCode: status,
		IP:         clientIP(r),
		UserAgent:  r.UserAgent(),
		Detail:     raw,
	})
}
