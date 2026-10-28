import { useState, type ReactNode } from 'react'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { StatusBadge } from '@/shared/ui/status-badge'

import {
  cleanupLogs,
  clearDiskCache,
  runGarbageCollection,
  type CleanupMode,
  type PerformanceDiskCacheClearResult,
  type PerformanceGCResult,
  type PerformanceLogCleanupResult,
  type PerformanceLogs,
  type PerformanceStats,
} from '../api'
import { DangerConfirm } from '../danger-confirm'
import {
  diskUsagePercent,
  formatBytes,
  formatCount,
  formatDateTime,
  formatDecimal,
  previewLogCleanup,
  usageBarClass,
  usageTone,
  validateCleanupValue,
} from '../utils'
import {
  InlineError,
  SkeletonLines,
  describePerformanceError,
} from './primitives'

/**
 * 维护动作区（Task 18 / adjudications §5）。
 *
 * 三张卡（磁盘缓存 / 运行时 GC / 应用日志清理）连同它们的 state、handlers
 * 与确认弹窗从旧 `performance-page.tsx` **整段搬出**，行为不变：
 * 写请求回传实例、422/409/429 原地提示、传输失败并入离线态。
 *
 * 页面只注入查询数据、查询状态与三个 refetch 回调；本区不持有查询本身。
 * `data-testid`（`performance-disk-card` / `performance-gc-card` /
 * `performance-logs-card`）原样保留，既有测试与外部脚本依赖它们。
 */

type DiskOutcome =
  | { kind: 'success'; data: PerformanceDiskCacheClearResult }
  | { kind: 'error'; message: string }

type GcOutcome =
  | { kind: 'success'; data: PerformanceGCResult }
  | { kind: 'error'; message: string }

type LogsOutcome =
  | { kind: 'success'; data: PerformanceLogCleanupResult }
  | { kind: 'error'; message: string }

function MaintenanceCard({
  title,
  testId,
  children,
}: {
  title: string
  testId: string
  children: ReactNode
}) {
  return (
    <section
      data-testid={testId}
      className="rounded-xl border border-border-default bg-surface-card p-5"
    >
      <h2 className="mb-3 text-card-title font-semibold leading-[22px] text-ink-1">
        {title}
      </h2>
      {children}
    </section>
  )
}

/** 结果行：成功/部分失败/失败三色，`aria-live=polite` 只播报一次。 */
function ResultLine({
  tone,
  testId,
  children,
}: {
  tone: 'success' | 'warning' | 'danger'
  testId: string
  children: ReactNode
}) {
  const toneClass =
    tone === 'danger'
      ? 'border-danger-line bg-danger-bg text-danger'
      : tone === 'warning'
        ? 'border-warning-line bg-warning-bg text-warning'
        : 'border-success-line bg-success-bg text-success'
  return (
    <p
      data-testid={testId}
      data-tone={tone}
      aria-live="polite"
      className={`mt-3 rounded-lg border px-3 py-2 text-body ${toneClass}`}
    >
      {children}
    </p>
  )
}

export interface MaintenanceZoneProps {
  stats: PerformanceStats | undefined
  statsPending: boolean
  statsError: unknown
  /** 有旧数据、但最近一次取数失败（展示旧值 + 内联错误）。 */
  statsStale: boolean
  logs: PerformanceLogs | undefined
  logsPending: boolean
  logsError: unknown
  logsStale: boolean
  offline: boolean
  canManage: boolean
  refetchStats: () => Promise<unknown>
  refetchLogs: () => Promise<unknown>
  /** 写成功后重拉「最近操作」，结果行与历史必须反映同一次清理。 */
  refetchRecent: () => Promise<unknown>
  /**
   * 写操作的离线判定回调：只有传输失败（`ApiError.kind === 'offline'`）
   * 才把页面推入离线态；409/422/429 都是服务端可达的业务结果。
   */
  onWriteOffline: (isOffline: boolean) => void
}

export function MaintenanceZone({
  stats,
  statsPending,
  statsError,
  statsStale,
  logs,
  logsPending,
  logsError,
  logsStale,
  offline,
  canManage,
  refetchStats,
  refetchLogs,
  refetchRecent,
  onWriteOffline,
}: MaintenanceZoneProps) {
  const { t, locale } = useI18n()

  const [diskConfirmOpen, setDiskConfirmOpen] = useState(false)
  const [diskPending, setDiskPending] = useState(false)
  const [diskOutcome, setDiskOutcome] = useState<DiskOutcome | null>(null)

  const [gcConfirmOpen, setGcConfirmOpen] = useState(false)
  const [gcPending, setGcPending] = useState(false)
  const [gcOutcome, setGcOutcome] = useState<GcOutcome | null>(null)

  const [logsConfirmOpen, setLogsConfirmOpen] = useState(false)
  const [logsWorking, setLogsWorking] = useState(false)
  const [logsOutcome, setLogsOutcome] = useState<LogsOutcome | null>(null)
  const [mode, setMode] = useState<CleanupMode>('by_days')
  const [rawValues, setRawValues] = useState<Record<CleanupMode, string>>({
    by_days: '30',
    by_count: '20',
  })

  const rawValue = rawValues[mode]
  const valueValidation = validateCleanupValue(rawValue)
  const cleanupPreview = previewLogCleanup(
    logs?.files ?? [],
    mode,
    valueValidation.value ?? 0,
    Date.now(),
  )

  const describeError = (error: unknown): string =>
    describePerformanceError(error, t)

  const handleDiskClear = async (): Promise<void> => {
    setDiskPending(true)
    setDiskOutcome(null)
    try {
      // 回传页面统计所属实例：实例漂移时服务端 409，而不是清错副本。
      const data = await clearDiskCache(stats?.instance_id)
      onWriteOffline(false)
      setDiskOutcome({ kind: 'success', data })
      // 成功后刷新统计与最近操作：结果行与卡片数字必须反映同一次清理。
      await Promise.all([refetchStats(), refetchRecent()])
    } catch (error) {
      onWriteOffline(
        error instanceof ApiError && error.kind === 'offline',
      )
      setDiskOutcome({ kind: 'error', message: describeError(error) })
    } finally {
      setDiskPending(false)
      setDiskConfirmOpen(false)
    }
  }

  const handleRunGc = async (): Promise<void> => {
    setGcPending(true)
    setGcOutcome(null)
    try {
      const data = await runGarbageCollection(stats?.instance_id)
      onWriteOffline(false)
      setGcOutcome({ kind: 'success', data })
      await Promise.all([refetchStats(), refetchRecent()])
    } catch (error) {
      onWriteOffline(
        error instanceof ApiError && error.kind === 'offline',
      )
      setGcOutcome({ kind: 'error', message: describeError(error) })
    } finally {
      setGcPending(false)
      setGcConfirmOpen(false)
    }
  }

  const handleLogCleanup = async (): Promise<void> => {
    const parsed = validateCleanupValue(rawValue)
    if (parsed.value === undefined) return
    setLogsWorking(true)
    setLogsOutcome(null)
    try {
      const data = await cleanupLogs(mode, parsed.value, stats?.instance_id)
      onWriteOffline(false)
      setLogsOutcome({ kind: 'success', data })
      await Promise.all([refetchLogs(), refetchRecent()])
    } catch (error) {
      onWriteOffline(
        error instanceof ApiError && error.kind === 'offline',
      )
      setLogsOutcome({ kind: 'error', message: describeError(error) })
    } finally {
      setLogsWorking(false)
      setLogsConfirmOpen(false)
    }
  }

  const configEnabled = stats?.config.enabled === true
  const diskEnabled = stats?.cache_stats.enabled === true
  const diskPath = stats?.disk_cache_info.path || stats?.config.path || ''
  const diskMaxMb = stats?.config.max_mb ?? 0
  const diskPercent = diskUsagePercent(
    stats?.cache_stats.total_bytes ?? 0,
    diskMaxMb,
  )
  const diskTone = usageTone(diskPercent)

  const retry = (): void => {
    void refetchStats()
  }
  const retryLogs = (): void => {
    void refetchLogs()
  }

  return (
    <>
      {/* ---------------------------------------------------------------- */}
      {/* 磁盘缓存（P1）                                                    */}
      {/* ---------------------------------------------------------------- */}
      <MaintenanceCard
        title={t('admin.performance.disk.title')}
        testId="performance-disk-card"
      >
        {statsPending ? (
          <SkeletonLines rows={3} testId="performance-disk-skeleton" />
        ) : stats === undefined ? (
          <InlineError
            testId="performance-disk-error"
            message={describeError(statsError)}
            retryLabel={t('admin.performance.common.retry')}
            onRetry={retry}
          />
        ) : (
          <>
            {statsStale ? (
              <InlineError
                testId="performance-disk-stale"
                message={describeError(statsError)}
                retryLabel={t('admin.performance.common.retry')}
                onRetry={retry}
              />
            ) : null}
            <div className="flex flex-col gap-2">
              <div className="flex flex-wrap items-center gap-3">
                <StatusBadge variant={diskEnabled ? 'success' : 'neutral'}>
                  {t(
                    diskEnabled
                      ? 'admin.performance.disk.status_enabled'
                      : 'admin.performance.disk.status_disabled',
                  )}
                </StatusBadge>
                <span className="font-mono text-meta text-ink-2">
                  {diskPath === ''
                    ? '—'
                    : t('admin.performance.disk.path', { path: diskPath })}
                </span>
              </div>
              <p className="font-mono text-body text-ink-2">
                {t('admin.performance.disk.files', {
                  count: formatCount(stats.cache_stats.file_count, locale),
                })}
                {' · '}
                {t('admin.performance.disk.usage', {
                  used: formatBytes(stats.cache_stats.total_bytes, locale),
                  max:
                    diskMaxMb > 0
                      ? formatBytes(diskMaxMb * 1024 * 1024, locale)
                      : '—',
                })}
              </p>
              <div className="flex items-center gap-2">
                <div
                  role="progressbar"
                  aria-label={t('admin.performance.disk.usage_label')}
                  aria-valuemin={0}
                  aria-valuemax={100}
                  aria-valuenow={Math.round(diskPercent)}
                  data-testid="performance-disk-usage"
                  data-tone={diskTone}
                  className="h-1 w-40 overflow-hidden rounded-full bg-surface-subtle"
                >
                  <div
                    className={`h-full ${usageBarClass(diskTone)}`}
                    style={{ width: `${diskPercent}%` }}
                  />
                </div>
                <span className="font-mono text-meta text-ink-3">
                  {formatDecimal(diskPercent, locale)}%
                </span>
              </div>
              <p className="font-mono text-body text-ink-2">
                {t('admin.performance.disk.threshold', {
                  size:
                    stats.config.threshold_mb > 0
                      ? formatBytes(
                          stats.config.threshold_mb * 1024 * 1024,
                          locale,
                        )
                      : '—',
                })}
                {' · '}
                {stats.cache_stats.last_cleared_at === null
                  ? t('admin.performance.disk.never_cleared')
                  : t('admin.performance.disk.last_cleared', {
                      time: formatDateTime(
                        stats.cache_stats.last_cleared_at,
                        locale,
                      ),
                    })}
              </p>
            </div>

            {/*
             * 空态原因必须由 config.enabled 区分：环境变量关闭与「已启用但
             * 目录不可访问」是两种不同的运维问题，后者能通过修权限解决。
             * 只用 cache_stats.enabled 会把两者混成同一句「未启用」。
             */}
            {!configEnabled ? (
              <p className="mt-3 text-body text-ink-3">
                {t('admin.performance.disk.empty')}
              </p>
            ) : !diskEnabled ? (
              <p className="mt-3 text-body text-ink-3">
                {t('admin.performance.disk.dir_unavailable')}
              </p>
            ) : null}

            <div className="mt-3 flex flex-wrap items-center gap-3">
              <Button
                disabled={!diskEnabled || offline || diskPending || !canManage}
                aria-describedby="performance-disk-consequence"
                onClick={() => setDiskConfirmOpen(true)}
              >
                {t('admin.performance.disk.clear_button')}
              </Button>
              <p
                id="performance-disk-consequence"
                className="text-meta text-ink-3"
              >
                {t('admin.performance.disk.consequence')}
              </p>
            </div>

            {diskOutcome !== null ? (
              diskOutcome.kind === 'error' ? (
                <ResultLine tone="danger" testId="performance-disk-result">
                  {diskOutcome.message}
                </ResultLine>
              ) : diskOutcome.data.failed_files.length > 0 ? (
                <ResultLine tone="warning" testId="performance-disk-result">
                  {t('admin.performance.disk.result_partial', {
                    count: formatCount(diskOutcome.data.deleted_count, locale),
                    size: formatBytes(diskOutcome.data.freed_bytes, locale),
                    failed: formatCount(
                      diskOutcome.data.failed_files.length,
                      locale,
                    ),
                  })}
                </ResultLine>
              ) : (
                <ResultLine tone="success" testId="performance-disk-result">
                  {t('admin.performance.disk.result_success', {
                    count: formatCount(diskOutcome.data.deleted_count, locale),
                    size: formatBytes(diskOutcome.data.freed_bytes, locale),
                  })}
                </ResultLine>
              )
            ) : null}
          </>
        )}
      </MaintenanceCard>

      {/* ---------------------------------------------------------------- */}
      {/* 运行时 GC（P1）                                                   */}
      {/* ---------------------------------------------------------------- */}
      <MaintenanceCard
        title={t('admin.performance.gc.title')}
        testId="performance-gc-card"
      >
        {statsPending ? (
          <SkeletonLines rows={2} testId="performance-gc-skeleton" />
        ) : stats === undefined ? (
          <InlineError
            testId="performance-gc-error"
            message={describeError(statsError)}
            retryLabel={t('admin.performance.common.retry')}
            onRetry={retry}
          />
        ) : (
          <>
            <div className="grid grid-cols-2 gap-x-6 gap-y-1 text-body text-ink-2 md:grid-cols-4">
              <p className="font-mono">
                {t('admin.performance.gc.alloc', {
                  size: formatBytes(stats.memory_stats.alloc_bytes, locale),
                })}
              </p>
              <p className="font-mono">
                {t('admin.performance.gc.sys', {
                  size: formatBytes(stats.memory_stats.sys_bytes, locale),
                })}
              </p>
              <p className="font-mono">
                {t('admin.performance.gc.num_gc', {
                  count: formatCount(stats.memory_stats.num_gc, locale),
                })}
              </p>
              <p className="font-mono">
                {t('admin.performance.gc.goroutines', {
                  count: formatCount(stats.memory_stats.num_goroutine, locale),
                })}
              </p>
            </div>

            <div className="mt-3 flex flex-wrap items-center gap-3">
              <Button
                disabled={offline || gcPending || !canManage}
                aria-describedby="performance-gc-consequence"
                onClick={() => setGcConfirmOpen(true)}
              >
                {t('admin.performance.gc.run_button')}
              </Button>
              <p id="performance-gc-consequence" className="text-meta text-ink-3">
                {t('admin.performance.gc.note')}
              </p>
            </div>

            {gcOutcome !== null ? (
              gcOutcome.kind === 'error' ? (
                <ResultLine tone="danger" testId="performance-gc-result">
                  {gcOutcome.message}
                </ResultLine>
              ) : (
                <ResultLine tone="success" testId="performance-gc-result">
                  {t('admin.performance.gc.result', {
                    ms: formatCount(gcOutcome.data.duration_ms, locale),
                    before: formatBytes(
                      gcOutcome.data.heap_alloc_before,
                      locale,
                    ),
                    after: formatBytes(gcOutcome.data.heap_alloc_after, locale),
                  })}
                </ResultLine>
              )
            ) : null}
          </>
        )}
      </MaintenanceCard>

      {/* ---------------------------------------------------------------- */}
      {/* 应用日志文件清理（P2）                                             */}
      {/* ---------------------------------------------------------------- */}
      <MaintenanceCard
        title={t('admin.performance.logs.title')}
        testId="performance-logs-card"
      >
        {logsPending ? (
          <SkeletonLines rows={3} testId="performance-logs-skeleton" />
        ) : logs === undefined ? (
          <InlineError
            testId="performance-logs-error"
            message={describeError(logsError)}
            retryLabel={t('admin.performance.common.retry')}
            onRetry={retryLogs}
          />
        ) : (
          <>
            {logsStale ? (
              <InlineError
                testId="performance-logs-stale"
                message={describeError(logsError)}
                retryLabel={t('admin.performance.common.retry')}
                onRetry={retryLogs}
              />
            ) : null}
            <div className="flex flex-col gap-2">
              <p className="font-mono text-meta text-ink-2">
                {logs.log_dir === ''
                  ? '—'
                  : t('admin.performance.logs.dir', { dir: logs.log_dir })}
              </p>
              <p className="font-mono text-body text-ink-2">
                {t('admin.performance.logs.files', {
                  count: formatCount(logs.file_count, locale),
                })}
                {' · '}
                {t('admin.performance.logs.total', {
                  size: formatBytes(logs.total_size, locale),
                })}
                {' · '}
                {t('admin.performance.logs.oldest', {
                  time: formatDateTime(logs.oldest_time, locale),
                })}
                {' · '}
                {t('admin.performance.logs.newest', {
                  time: formatDateTime(logs.newest_time, locale),
                })}
              </p>
            </div>

            {logs.enabled ? (
              <>
                <div className="mt-3 flex flex-col gap-3 xl:flex-row xl:items-end xl:gap-6">
                  <fieldset className="flex flex-col gap-1">
                    <legend className="mb-1 text-meta font-medium text-ink-1">
                      {t('admin.performance.logs.mode_label')}
                    </legend>
                    <div className="flex flex-wrap gap-3 text-body text-ink-2">
                      <label className="flex items-center gap-1.5">
                        <input
                          type="radio"
                          name="performance-log-mode"
                          value="by_days"
                          checked={mode === 'by_days'}
                          onChange={() => setMode('by_days')}
                        />
                        {t('admin.performance.logs.mode_by_days')}
                      </label>
                      <label className="flex items-center gap-1.5">
                        <input
                          type="radio"
                          name="performance-log-mode"
                          value="by_count"
                          checked={mode === 'by_count'}
                          onChange={() => setMode('by_count')}
                        />
                        {t('admin.performance.logs.mode_by_count')}
                      </label>
                    </div>
                  </fieldset>

                  <div className="flex flex-col gap-1">
                    <label
                      htmlFor="performance-log-value"
                      className="text-meta font-medium text-ink-1"
                    >
                      {t('admin.performance.logs.value_label')}
                    </label>
                    <div className="flex items-center gap-2">
                      <input
                        id="performance-log-value"
                        type="number"
                        min={1}
                        max={36500}
                        step={1}
                        value={rawValue}
                        aria-invalid={
                          valueValidation.error !== undefined || undefined
                        }
                        aria-describedby={
                          valueValidation.error !== undefined
                            ? 'performance-log-value-error'
                            : undefined
                        }
                        onChange={(event) => {
                          const next = event.target.value
                          setRawValues((current) => ({
                            ...current,
                            [mode]: next,
                          }))
                        }}
                        className="h-9 w-28 rounded-lg border border-border-default bg-surface-card px-3 font-mono text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                      />
                      <span className="text-body text-ink-3">
                        {t(
                          mode === 'by_days'
                            ? 'admin.performance.logs.days_unit'
                            : 'admin.performance.logs.count_unit',
                        )}
                      </span>
                    </div>
                    {valueValidation.error !== undefined ? (
                      <p
                        id="performance-log-value-error"
                        className="text-meta text-danger"
                      >
                        {t(
                          valueValidation.error === 'required'
                            ? 'admin.performance.validation.value_required'
                            : 'admin.performance.validation.value_range',
                        )}
                      </p>
                    ) : null}
                  </div>
                </div>

                <p
                  data-testid="performance-logs-preview"
                  aria-live="polite"
                  className="mt-3 text-body text-ink-2"
                >
                  {cleanupPreview.count === 0
                    ? t('admin.performance.logs.preview_none')
                    : t('admin.performance.logs.preview', {
                        count: formatCount(cleanupPreview.count, locale),
                        size: formatBytes(cleanupPreview.bytes, locale),
                      })}
                </p>
                <p className="text-meta text-ink-3">
                  {t('admin.performance.logs.active_note')}
                </p>

                <div className="mt-3 flex flex-wrap items-center gap-3">
                  <Button
                    disabled={
                      offline ||
                      logsWorking ||
                      !canManage ||
                      valueValidation.value === undefined
                    }
                    aria-describedby="performance-logs-consequence"
                    onClick={() => setLogsConfirmOpen(true)}
                  >
                    {t('admin.performance.logs.cleanup_button')}
                  </Button>
                  <p
                    id="performance-logs-consequence"
                    className="text-meta text-ink-3"
                  >
                    {t('admin.performance.logs.consequence')}
                  </p>
                </div>

                {logsOutcome !== null ? (
                  logsOutcome.kind === 'error' ? (
                    <ResultLine tone="danger" testId="performance-logs-result">
                      {logsOutcome.message}
                    </ResultLine>
                  ) : logsOutcome.data.failed_files.length > 0 ? (
                    <ResultLine
                      tone="warning"
                      testId="performance-logs-result"
                    >
                      {t('admin.performance.logs.result_partial', {
                        count: formatCount(
                          logsOutcome.data.deleted_count,
                          locale,
                        ),
                        size: formatBytes(logsOutcome.data.freed_bytes, locale),
                        failed: formatCount(
                          logsOutcome.data.failed_files.length,
                          locale,
                        ),
                      })}
                      {logsOutcome.data.skipped_active
                        ? ` ${t('admin.performance.logs.result_skipped')}`
                        : ''}
                    </ResultLine>
                  ) : (
                    <ResultLine tone="success" testId="performance-logs-result">
                      {t('admin.performance.logs.result_success', {
                        count: formatCount(
                          logsOutcome.data.deleted_count,
                          locale,
                        ),
                        size: formatBytes(logsOutcome.data.freed_bytes, locale),
                      })}
                      {logsOutcome.data.skipped_active
                        ? ` ${t('admin.performance.logs.result_skipped')}`
                        : ''}
                    </ResultLine>
                  )
                ) : null}
              </>
            ) : (
              <>
                {/*
                 * 未配置时按钮仍然渲染但禁用（六态「空态」要求）：直接隐藏
                 * 会让运维找不到入口；aria-describedby 直接指向空态说明，
                 * 读屏用户因此能听到禁用原因。
                 */}
                <p
                  id="performance-logs-consequence"
                  className="mt-3 text-body text-ink-3"
                >
                  {t('admin.performance.logs.empty')}
                </p>
                <div className="mt-3 flex flex-wrap items-center gap-3">
                  <Button
                    disabled
                    aria-describedby="performance-logs-consequence"
                  >
                    {t('admin.performance.logs.cleanup_button')}
                  </Button>
                </div>
              </>
            )}
          </>
        )}
      </MaintenanceCard>

      <DangerConfirm
        open={diskConfirmOpen}
        title={t('admin.performance.disk.confirm_title')}
        description={t('admin.performance.disk.confirm_body')}
        confirmLabel={t('admin.performance.disk.clear_button')}
        pending={diskPending}
        onCancel={() => setDiskConfirmOpen(false)}
        onConfirm={() => {
          void handleDiskClear()
        }}
      />
      <DangerConfirm
        open={gcConfirmOpen}
        title={t('admin.performance.gc.confirm_title')}
        description={t('admin.performance.gc.confirm_body')}
        confirmLabel={t('admin.performance.gc.run_button')}
        pending={gcPending}
        onCancel={() => setGcConfirmOpen(false)}
        onConfirm={() => {
          void handleRunGc()
        }}
      />
      <DangerConfirm
        open={logsConfirmOpen}
        title={t('admin.performance.logs.confirm_title')}
        description={t('admin.performance.logs.confirm_body', {
          count: formatCount(cleanupPreview.count, locale),
          size: formatBytes(cleanupPreview.bytes, locale),
          active_note: t('admin.performance.logs.active_note'),
        })}
        confirmLabel={t('admin.performance.logs.cleanup_button')}
        pending={logsWorking}
        onCancel={() => setLogsConfirmOpen(false)}
        onConfirm={() => {
          void handleLogCleanup()
        }}
      />
    </>
  )
}
