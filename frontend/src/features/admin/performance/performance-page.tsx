import { useQuery } from '@tanstack/react-query'
import { useEffect, useRef, useState, type ReactNode } from 'react'

import { ApiError } from '@/api/errors'
import { meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'
import { StatusBadge } from '@/shared/ui/status-badge'
import { useToastStore } from '@/shared/ui/toast'

import {
  cleanupLogs,
  clearDiskCache,
  fetchPerformanceLogs,
  fetchPerformanceStats,
  fetchRecentOperations,
  runGarbageCollection,
  type CleanupMode,
  type PerformanceDiskCacheClearResult,
  type PerformanceGCResult,
  type PerformanceLogCleanupResult,
  type PerformanceRecentOperation,
} from './api'
import { DangerConfirm } from './danger-confirm'
import { useOnlineStatus } from './use-online'
import {
  KNOWN_PROBLEM_CODES,
  diskUsagePercent,
  formatBytes,
  formatClock,
  formatCount,
  formatDateTime,
  formatDecimal,
  formatMilliseconds,
  previewLogCleanup,
  problemCode,
  usageBarClass,
  usageTone,
  validateCleanupValue,
} from './utils'

type DiskOutcome =
  | { kind: 'success'; data: PerformanceDiskCacheClearResult }
  | { kind: 'error'; message: string }

type GcOutcome =
  | { kind: 'success'; data: PerformanceGCResult }
  | { kind: 'error'; message: string }

type LogsOutcome =
  | { kind: 'success'; data: PerformanceLogCleanupResult }
  | { kind: 'error'; message: string }

/** `<1024` 管理页闸门（UI §6.12 响应式）；无 matchMedia 的环境按桌面处理。 */
function useMinWidth(px: number): boolean {
  const [matches, setMatches] = useState(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
      return true
    }
    return window.matchMedia(`(min-width: ${px}px)`).matches
  })
  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
      return
    }
    const query = window.matchMedia(`(min-width: ${px}px)`)
    const update = () => setMatches(query.matches)
    update()
    query.addEventListener('change', update)
    return () => query.removeEventListener('change', update)
  }, [px])
  return matches
}

function PerformanceCard({
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
      <h2 className="mb-3 text-[15px] font-semibold leading-[22px] text-ink-1">
        {title}
      </h2>
      {children}
    </section>
  )
}

function SkeletonLines({ rows, testId }: { rows: number; testId: string }) {
  return (
    <div data-testid={testId} aria-hidden="true" className="space-y-2">
      {Array.from({ length: rows }, (_, index) => (
        <div
          key={index}
          className="h-4 w-full rounded bg-surface-subtle"
        />
      ))}
    </div>
  )
}

function InlineError({
  message,
  retryLabel,
  onRetry,
  testId,
}: {
  message: string
  retryLabel: string
  onRetry: () => void
  testId: string
}) {
  return (
    <div
      data-testid={testId}
      role="alert"
      className="flex flex-wrap items-center gap-3 rounded-lg border border-danger-line bg-danger-bg px-3 py-2 text-sm text-danger"
    >
      <span>{message}</span>
      <Button variant="ghost" className="h-7 px-2 text-xs" onClick={onRetry}>
        {retryLabel}
      </Button>
    </div>
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
      className={`mt-3 rounded-lg border px-3 py-2 text-sm ${toneClass}`}
    >
      {children}
    </p>
  )
}

function actionKey(action: PerformanceRecentOperation['action']): string {
  return `admin.performance.recent.action_${action.replace(/\./g, '_')}`
}

function modeKey(mode: CleanupMode): string {
  return `admin.performance.logs.mode_${mode}`
}

/**
 * 性能管理页（UI §6.12，F-08-48～F-08-50）。
 *
 * 独立页，不进 Settings Shell：三个对象都是**进程/节点本地**的实时观测与
 * 即时生效的写操作，没有「一组键值 + 整段保存」的语义。
 *
 * 权限 `settings:manage`（modules-07，不新增运维权限键）：入口不渲染、直链
 * 由路由 beforeLoad 送 `/403`；服务端仍逐请求强制。
 */
export function PerformancePage() {
  const { t, locale } = useI18n()
  const push = useToastStore((state) => state.push)
  const online = useOnlineStatus()
  const wideEnough = useMinWidth(1024)
  const me = useQuery(meQueryOptions())
  const canManage =
    me.data?.permissions?.some(
      (permission) => permission === '*' || permission === 'settings:manage',
    ) ?? false
  const queriesEnabled = canManage && wideEnough

  const statsQuery = useQuery({
    queryKey: ['performance', 'stats'],
    queryFn: ({ signal }) => fetchPerformanceStats(signal),
    enabled: queriesEnabled,
    // 管理观测页不自动重试：429 要原地提示而不是退避后无限重发。
    retry: false,
  })
  const logsQuery = useQuery({
    queryKey: ['performance', 'logs'],
    queryFn: ({ signal }) => fetchPerformanceLogs(signal),
    enabled: queriesEnabled,
    retry: false,
  })
  const recentQuery = useQuery({
    queryKey: ['performance', 'recent-operations'],
    queryFn: ({ signal }) => fetchRecentOperations(signal),
    enabled: queriesEnabled,
    retry: false,
  })

  const [diskConfirmOpen, setDiskConfirmOpen] = useState(false)
  const [diskPending, setDiskPending] = useState(false)
  const [diskOutcome, setDiskOutcome] = useState<DiskOutcome | null>(null)

  const [gcConfirmOpen, setGcConfirmOpen] = useState(false)
  const [gcPending, setGcPending] = useState(false)
  const [gcOutcome, setGcOutcome] = useState<GcOutcome | null>(null)

  const [logsConfirmOpen, setLogsConfirmOpen] = useState(false)
  const [logsPending, setLogsPending] = useState(false)
  const [logsOutcome, setLogsOutcome] = useState<LogsOutcome | null>(null)
  const [mode, setMode] = useState<CleanupMode>('by_days')
  const [rawValues, setRawValues] = useState<Record<CleanupMode, string>>({
    by_days: '30',
    by_count: '20',
  })

  // 服务端已断（ApiError.kind='offline'）与浏览器 offline 事件任一成立即视为
  // 离线：两者互不替代，但用户看到的结果都是「写操作不可用 + 离线快照」。
  const offline =
    !online ||
    [statsQuery.error, logsQuery.error, recentQuery.error].some(
      (error) => error instanceof ApiError && error.kind === 'offline',
    )

  // 离线 Toast 只在「在线 → 离线」的跳变时播报一次；重复渲染不重复提醒。
  const wasOfflineRef = useRef(!online)
  useEffect(() => {
    if (online) {
      wasOfflineRef.current = false
      return
    }
    if (wasOfflineRef.current) return
    wasOfflineRef.current = true
    push({
      tone: 'error',
      message: t('admin.performance.page.offline_toast'),
    })
  }, [online, push, t])

  const stats = statsQuery.data
  const logs = logsQuery.data

  const rawValue = rawValues[mode]
  const valueValidation = validateCleanupValue(rawValue)
  const cleanupPreview = previewLogCleanup(
    logs?.files ?? [],
    mode,
    valueValidation.value ?? 0,
    Date.now(),
  )

  const refreshing =
    statsQuery.isFetching || logsQuery.isFetching || recentQuery.isFetching
  const refreshAll = (): void => {
    void Promise.all([
      statsQuery.refetch(),
      logsQuery.refetch(),
      recentQuery.refetch(),
    ])
  }

  /** 统一错误文案：已知 Problem 码本地化，429 原地提示，其余通用。 */
  const describeError = (error: unknown): string => {
    if (error instanceof ApiError) {
      if (error.status === 429) {
        return error.retry_after !== undefined
          ? t('admin.performance.errors.rate_limited_retry', {
              seconds: error.retry_after,
            })
          : t('admin.performance.errors.rate_limited')
      }
      const code = problemCode(error)
      if (KNOWN_PROBLEM_CODES.has(code)) {
        return t(`admin.performance.errors.${code}`)
      }
    }
    return t('admin.performance.errors.generic')
  }

  const handleDiskClear = async (): Promise<void> => {
    setDiskPending(true)
    setDiskOutcome(null)
    try {
      const data = await clearDiskCache()
      setDiskOutcome({ kind: 'success', data })
      // 成功后刷新统计与最近操作：结果行与卡片数字必须反映同一次清理。
      await Promise.all([statsQuery.refetch(), recentQuery.refetch()])
    } catch (error) {
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
      const data = await runGarbageCollection()
      setGcOutcome({ kind: 'success', data })
      await Promise.all([statsQuery.refetch(), recentQuery.refetch()])
    } catch (error) {
      setGcOutcome({ kind: 'error', message: describeError(error) })
    } finally {
      setGcPending(false)
      setGcConfirmOpen(false)
    }
  }

  const handleLogCleanup = async (): Promise<void> => {
    const parsed = validateCleanupValue(rawValue)
    if (parsed.value === undefined) return
    setLogsPending(true)
    setLogsOutcome(null)
    try {
      const data = await cleanupLogs(mode, parsed.value)
      setLogsOutcome({ kind: 'success', data })
      await Promise.all([logsQuery.refetch(), recentQuery.refetch()])
    } catch (error) {
      setLogsOutcome({ kind: 'error', message: describeError(error) })
    } finally {
      setLogsPending(false)
      setLogsConfirmOpen(false)
    }
  }

  if (!wideEnough) {
    return (
      <div className="mx-auto max-w-[1440px] rounded-xl border border-border-hairline bg-surface-card p-8">
        <h1 className="text-xl font-semibold text-ink-1">
          {t('admin.performance.page.title')}
        </h1>
        <p className="mt-2 text-sm text-ink-2">
          {t('admin.performance.page.desktop_only')}
        </p>
      </div>
    )
  }

  const instanceId = stats?.instance_id
  const diskEnabled = stats?.cache_stats.enabled === true
  const diskPath = stats?.disk_cache_info.path || stats?.config.path || ''
  const diskMaxMb = stats?.config.max_mb ?? 0
  const diskPercent = diskUsagePercent(
    stats?.cache_stats.total_bytes ?? 0,
    diskMaxMb,
  )
  const diskTone = usageTone(diskPercent)

  const readOnlyNotice =
    !canManage && !me.isPending ? (
      <FormAlert tone="info">
        {t('admin.performance.common.read_only')}
      </FormAlert>
    ) : null

  return (
    <div
      className="mx-auto flex max-w-[1440px] flex-col gap-4"
      data-testid="performance-page"
    >
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold text-ink-1">
            {t('admin.performance.page.title')}
          </h1>
        </div>
        <div className="flex flex-wrap items-center gap-3">
          {instanceId !== undefined && instanceId !== '' ? (
            <StatusBadge variant="neutral">
              {t('admin.performance.page.instance_label', { id: instanceId })}
            </StatusBadge>
          ) : null}
          <Button
            variant="secondary"
            disabled={refreshing || !canManage}
            onClick={refreshAll}
          >
            {t('admin.performance.page.refresh')}
          </Button>
          <span
            data-testid="performance-last-updated"
            className="font-mono text-xs text-ink-3"
          >
            {t('admin.performance.page.last_updated', {
              time: formatClock(statsQuery.dataUpdatedAt, locale),
            })}
          </span>
        </div>
      </header>

      <FormAlert tone="info">
        {t('admin.performance.page.scope_notice')}
      </FormAlert>

      {readOnlyNotice}

      {offline ? (
        <p
          data-testid="performance-offline-snapshot"
          className="text-sm text-warning"
          role="status"
        >
          {t('admin.performance.page.offline_snapshot')}
        </p>
      ) : null}

      {/* ---------------------------------------------------------------- */}
      {/* 磁盘缓存（P1）                                                    */}
      {/* ---------------------------------------------------------------- */}
      <PerformanceCard
        title={t('admin.performance.disk.title')}
        testId="performance-disk-card"
      >
        {statsQuery.isPending ? (
          <SkeletonLines rows={3} testId="performance-disk-skeleton" />
        ) : stats === undefined ? (
          <InlineError
            testId="performance-disk-error"
            message={describeError(statsQuery.error)}
            retryLabel={t('admin.performance.common.retry')}
            onRetry={() => {
              void statsQuery.refetch()
            }}
          />
        ) : (
          <>
            {statsQuery.isError ? (
              <InlineError
                testId="performance-disk-stale"
                message={describeError(statsQuery.error)}
                retryLabel={t('admin.performance.common.retry')}
                onRetry={() => {
                  void statsQuery.refetch()
                }}
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
                <span className="font-mono text-xs text-ink-2">
                  {diskPath === ''
                    ? '—'
                    : t('admin.performance.disk.path', { path: diskPath })}
                </span>
              </div>
              <p className="font-mono text-sm text-ink-2">
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
                <span className="font-mono text-xs text-ink-3">
                  {formatDecimal(diskPercent, locale)}%
                </span>
              </div>
              <p className="font-mono text-sm text-ink-2">
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

            {!diskEnabled ? (
              <p className="mt-3 text-sm text-ink-3">
                {t('admin.performance.disk.empty')}
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
              <p id="performance-disk-consequence" className="text-xs text-ink-3">
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
      </PerformanceCard>

      {/* ---------------------------------------------------------------- */}
      {/* 运行时 GC（P1）                                                   */}
      {/* ---------------------------------------------------------------- */}
      <PerformanceCard
        title={t('admin.performance.gc.title')}
        testId="performance-gc-card"
      >
        {statsQuery.isPending ? (
          <SkeletonLines rows={2} testId="performance-gc-skeleton" />
        ) : stats === undefined ? (
          <InlineError
            testId="performance-gc-error"
            message={describeError(statsQuery.error)}
            retryLabel={t('admin.performance.common.retry')}
            onRetry={() => {
              void statsQuery.refetch()
            }}
          />
        ) : (
          <>
            <div className="grid grid-cols-2 gap-x-6 gap-y-1 text-sm text-ink-2 md:grid-cols-4">
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
              <p id="performance-gc-consequence" className="text-xs text-ink-3">
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
                    after: formatBytes(
                      gcOutcome.data.heap_alloc_after,
                      locale,
                    ),
                  })}
                </ResultLine>
              )
            ) : null}
          </>
        )}
      </PerformanceCard>

      {/* ---------------------------------------------------------------- */}
      {/* 应用日志文件清理（P2）                                             */}
      {/* ---------------------------------------------------------------- */}
      <PerformanceCard
        title={t('admin.performance.logs.title')}
        testId="performance-logs-card"
      >
        {logsQuery.isPending ? (
          <SkeletonLines rows={3} testId="performance-logs-skeleton" />
        ) : logs === undefined ? (
          <InlineError
            testId="performance-logs-error"
            message={describeError(logsQuery.error)}
            retryLabel={t('admin.performance.common.retry')}
            onRetry={() => {
              void logsQuery.refetch()
            }}
          />
        ) : (
          <>
            {logsQuery.isError ? (
              <InlineError
                testId="performance-logs-stale"
                message={describeError(logsQuery.error)}
                retryLabel={t('admin.performance.common.retry')}
                onRetry={() => {
                  void logsQuery.refetch()
                }}
              />
            ) : null}
            <div className="flex flex-col gap-2">
              <p className="font-mono text-xs text-ink-2">
                {logs.log_dir === ''
                  ? '—'
                  : t('admin.performance.logs.dir', { dir: logs.log_dir })}
              </p>
              <p className="font-mono text-sm text-ink-2">
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
                    <legend className="mb-1 text-[13px] font-medium text-ink-1">
                      {t('admin.performance.logs.mode_label')}
                    </legend>
                    <div className="flex flex-wrap gap-3 text-sm text-ink-2">
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
                      className="text-[13px] font-medium text-ink-1"
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
                        className="h-9 w-28 rounded-lg border border-border-default bg-surface-card px-3 font-mono text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                      />
                      <span className="text-sm text-ink-3">
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
                        className="text-xs text-danger"
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
                  className="mt-3 text-sm text-ink-2"
                >
                  {cleanupPreview.count === 0
                    ? t('admin.performance.logs.preview_none')
                    : t('admin.performance.logs.preview', {
                        count: formatCount(cleanupPreview.count, locale),
                        size: formatBytes(cleanupPreview.bytes, locale),
                      })}
                </p>
                <p className="text-xs text-ink-3">
                  {t('admin.performance.logs.active_note')}
                </p>

                <div className="mt-3 flex flex-wrap items-center gap-3">
                  <Button
                    disabled={
                      offline ||
                      logsPending ||
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
                    className="text-xs text-ink-3"
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
                  className="mt-3 text-sm text-ink-3"
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
      </PerformanceCard>

      {/* ---------------------------------------------------------------- */}
      {/* 最近操作（只读）                                                   */}
      {/* ---------------------------------------------------------------- */}
      <PerformanceCard
        title={t('admin.performance.recent.title')}
        testId="performance-recent-card"
      >
        {recentQuery.isPending ? (
          <SkeletonLines rows={3} testId="performance-recent-skeleton" />
        ) : recentQuery.data === undefined ? (
          <InlineError
            testId="performance-recent-error"
            message={describeError(recentQuery.error)}
            retryLabel={t('admin.performance.common.retry')}
            onRetry={() => {
              void recentQuery.refetch()
            }}
          />
        ) : recentQuery.data.items.length === 0 ? (
          <p className="text-sm text-ink-3">
            {t('admin.performance.recent.empty')}
          </p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[760px] border-collapse text-left text-sm">
              <thead>
                <tr className="border-b border-border-hairline text-xs text-ink-3">
                  <th className="px-2 py-2 font-medium">
                    {t('admin.performance.recent.time')}
                  </th>
                  <th className="px-2 py-2 font-medium">
                    {t('admin.performance.recent.actor')}
                  </th>
                  <th className="px-2 py-2 font-medium">
                    {t('admin.performance.recent.action')}
                  </th>
                  <th className="px-2 py-2 font-medium">
                    {t('admin.performance.recent.params')}
                  </th>
                  <th className="px-2 py-2 font-medium">
                    {t('admin.performance.recent.result')}
                  </th>
                  <th className="px-2 py-2 font-medium">
                    {t('admin.performance.recent.amount')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {recentQuery.data.items.map((item, index) => (
                  <tr
                    key={`${item.time}-${index}`}
                    data-testid="performance-recent-row"
                    className="border-b border-border-hairline last:border-0"
                  >
                    <td className="whitespace-nowrap px-2 py-2 font-mono text-xs text-ink-2">
                      {formatDateTime(item.time, locale)}
                    </td>
                    <td className="px-2 py-2 text-ink-2">{item.actor}</td>
                    <td className="px-2 py-2 text-ink-2">
                      {t(actionKey(item.action))}
                    </td>
                    <td className="px-2 py-2 text-ink-2">
                      {item.mode === undefined
                        ? '—'
                        : t('admin.performance.recent.params_summary', {
                            mode: t(modeKey(item.mode)),
                            value: item.value ?? '—',
                          })}
                    </td>
                    <td className="px-2 py-2">
                      <StatusBadge
                        variant={item.result === 'success' ? 'success' : 'danger'}
                      >
                        {t(
                          item.result === 'success'
                            ? 'admin.performance.recent.success'
                            : 'admin.performance.recent.failed',
                        )}
                      </StatusBadge>
                    </td>
                    <td className="px-2 py-2 font-mono text-xs text-ink-2">
                      <span>
                        {item.action === 'system.gc.run'
                          ? formatMilliseconds(item.duration_ms, locale)
                          : t('admin.performance.recent.deleted', {
                              count: formatCount(item.deleted_count, locale),
                              size: formatBytes(item.freed_bytes, locale),
                            })}
                      </span>
                      {item.failed_count > 0 ? (
                        <span className="ml-2 text-warning">
                          {t('admin.performance.recent.failed_count', {
                            count: formatCount(item.failed_count, locale),
                          })}
                        </span>
                      ) : null}
                      {item.skipped_active ? (
                        <span className="ml-2 text-ink-3">
                          {t('admin.performance.recent.skipped_active')}
                        </span>
                      ) : null}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </PerformanceCard>

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
        pending={logsPending}
        onCancel={() => setLogsConfirmOpen(false)}
        onConfirm={() => {
          void handleLogCleanup()
        }}
      />
    </div>
  )
}
