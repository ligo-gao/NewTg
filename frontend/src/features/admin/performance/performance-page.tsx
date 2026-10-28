import { useQuery } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'

import { ApiError } from '@/api/errors'
import { meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'
import { handleRadioGroupKeyDown } from '@/shared/lib/roving-focus'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

import {
  fetchPerfConfig,
  fetchPerfModels,
  fetchPerfOverview,
  fetchPerfSettings,
  fetchPerformanceLogs,
  fetchPerformanceStats,
  fetchRecentOperations,
  type OverviewRange,
  type PerfModels,
} from './api'
import { useOnlineStatus } from './use-online'
import { formatClock } from './utils'
import { DegradedBanner } from './zones/degraded-banner'
import { GatewayZone } from './zones/gateway-zone'
import { MaintenanceZone } from './zones/maintenance-zone'
import { ModelPerfZone } from './zones/model-perf-zone'
import { describePerformanceError } from './zones/primitives'
import { RecentOpsZone } from './zones/recent-ops-zone'
import { ResourcesZone } from './zones/resources-zone'
import { SettingsZone } from './zones/settings-zone'
import { VitalsStrip, type ModelProbe } from './zones/vitals-strip'

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

/**
 * 页面是否处于后台标签页。轮询必须随 `visibilitychange` 立即停表——只在
 * `refetchInterval` 函数里读 `document.hidden` 不够：已经排定的定时器仍会
 * 在隐藏后触发一次请求。状态变化会让 observer 收到新的 interval（`false`）
 * 并清掉定时器；恢复可见后由 TanStack Query 默认的 `refetchOnWindowFocus`
 * 补一次，不再另造恢复逻辑。
 */
function useDocumentHidden(): boolean {
  const [hidden, setHidden] = useState(
    () => typeof document !== 'undefined' && document.hidden,
  )
  useEffect(() => {
    if (typeof document === 'undefined') return
    const update = () => setHidden(document.hidden)
    document.addEventListener('visibilitychange', update)
    return () => document.removeEventListener('visibilitychange', update)
  }, [])
  return hidden
}

const RANGES: readonly OverviewRange[] = ['15m', '1h', '6h', '24h']

/** 时间窗按钮的文案键：API 值不进 UI，按钮显示本地化标签。 */
const RANGE_LABEL_KEYS: Record<OverviewRange, string> = {
  '15m': 'admin.performance.page.range.m15',
  '1h': 'admin.performance.page.range.h1',
  '6h': 'admin.performance.page.range.h6',
  '24h': 'admin.performance.page.range.h24',
}

/** 轮询节奏：均 ≥ 后端 Prometheus 15s 缓存 TTL，不会打出穿透缓存的高频请求。 */
const OVERVIEW_INTERVAL = 30_000
const MODELS_INTERVAL = 60_000
const CONFIG_INTERVAL = 60_000
const STATS_INTERVAL = 10_000

function probeTime(at: string | null | undefined): number {
  if (at === null || at === undefined) return Number.NEGATIVE_INFINITY
  const parsed = Date.parse(at)
  return Number.isNaN(parsed) ? Number.NEGATIVE_INFINITY : parsed
}

/**
 * 第五格副标题用的「最值得注意的模型」：incident 优先于 degraded，同级取
 * 最近一次探针时间最新的那行。只做选择与比较，不在页面里格式化日期。
 */
function pickModelProbe(models: PerfModels | undefined): ModelProbe | undefined {
  if (models === undefined) return undefined
  const candidates = models.rows.filter(
    (row) => row.status === 'incident' || row.status === 'degraded',
  )
  let best = candidates[0]
  if (best === undefined) return undefined
  for (const row of candidates) {
    const rowRank = row.status === 'incident' ? 0 : 1
    const bestRank = best.status === 'incident' ? 0 : 1
    if (rowRank < bestRank) {
      best = row
      continue
    }
    if (rowRank > bestRank) continue
    if (
      probeTime(row.probe?.last_tested_at) > probeTime(best.probe?.last_tested_at)
    ) {
      best = row
    }
  }
  return { name: best.model, at: best.probe?.last_tested_at ?? null }
}

/**
 * 性能管理页（UI §6.12，F-08-48～F-08-50）。
 *
 * 独立页，不进 Settings Shell：三个对象都是**进程/节点本地**的实时观测与
 * 即时生效的写操作，没有「一组键值 + 整段保存」的语义。
 *
 * 权限 `settings:manage`（modules-07，不新增运维权限键）：入口不渲染、直链
 * 由路由 beforeLoad 送 `/403`；服务端仍逐请求强制。
 *
 * 本文件只做编排：查询、范围状态、派生值与七区顺序；各区的渲染与交互
 * 分别在 `zones/` 下。
 */
export function PerformancePage() {
  const { t, locale } = useI18n()
  const push = useToastStore((state) => state.push)
  const online = useOnlineStatus()
  const wideEnough = useMinWidth(1024)
  const pageHidden = useDocumentHidden()
  const me = useQuery(meQueryOptions())
  const canManage =
    me.data?.permissions?.some(
      (permission) => permission === '*' || permission === 'settings:manage',
    ) ?? false
  const queriesEnabled = canManage && wideEnough

  const [range, setRange] = useState<OverviewRange>('1h')

  /**
   * 失焦或无权时不排任何轮询：`refetchInterval` 直接是 `false`（而不是
   * 函数），这样可见性变化会立刻清掉已排定的定时器。
   */
  const interval = (ms: number): number | false =>
    queriesEnabled && !pageHidden ? ms : false

  const overviewQuery = useQuery({
    queryKey: ['performance', 'overview', range],
    queryFn: ({ signal }) => fetchPerfOverview(range, signal),
    enabled: queriesEnabled,
    // 管理观测页不自动重试：429 要原地提示而不是退避后无限重发。
    retry: false,
    refetchInterval: interval(OVERVIEW_INTERVAL),
    // 切窗时保留上一窗的读数，避免整条体检条/网关/资源区瞬间消失。
    placeholderData: (previousData) => previousData,
  })
  const modelsQuery = useQuery({
    queryKey: ['performance', 'models'],
    queryFn: ({ signal }) => fetchPerfModels(signal),
    enabled: queriesEnabled,
    retry: false,
    refetchInterval: interval(MODELS_INTERVAL),
  })
  const configQuery = useQuery({
    queryKey: ['performance', 'config'],
    queryFn: ({ signal }) => fetchPerfConfig(signal),
    enabled: queriesEnabled,
    retry: false,
    refetchInterval: interval(CONFIG_INTERVAL),
  })
  // 测速四项的落库值与 `updated_at`（乐观锁令牌）来自既有设置端点；
  // 与 config 同节奏刷新，保存后由 onSaved 立即重拉。
  const sectionQuery = useQuery({
    queryKey: ['performance', 'settings'],
    queryFn: ({ signal }) => fetchPerfSettings(signal),
    enabled: queriesEnabled,
    retry: false,
    refetchInterval: interval(CONFIG_INTERVAL),
  })
  const statsQuery = useQuery({
    queryKey: ['performance', 'stats'],
    queryFn: ({ signal }) => fetchPerformanceStats(signal),
    enabled: queriesEnabled,
    retry: false,
    refetchInterval: interval(STATS_INTERVAL),
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

  // 写操作传输失败与 GET 的 offline 必须同等对待：否则页面仍显示「已连接」，
  // 用户会反复点击一个注定失败的按钮。写成功后清除，浏览器 online 事件也清除
  // （网络恢复后 GET 未必立刻重发，不能只依赖查询错误）。
  const [writeOffline, setWriteOffline] = useState(false)

  // 服务端已断（ApiError.kind='offline'）与浏览器 offline 事件任一成立即视为
  // 离线：两者互不替代，但用户看到的结果都是「写操作不可用 + 离线快照」。
  const offline =
    !online ||
    writeOffline ||
    [statsQuery.error, logsQuery.error, recentQuery.error].some(
      (error) => error instanceof ApiError && error.kind === 'offline',
    )

  useEffect(() => {
    if (online) setWriteOffline(false)
  }, [online])

  useEffect(() => {
    const clear = (): void => setWriteOffline(false)
    window.addEventListener('online', clear)
    return () => window.removeEventListener('online', clear)
  }, [])

  // 写失败后另外两条恢复路径都可能不可达：「写成功」只有在写按钮可用时才会
  // 发生，而三个写按钮已因离线被禁用；服务端不可达但 `navigator.onLine` 仍为
  // true 时浏览器也不会派发 online 事件。若只依赖它们，页面会永久锁在离线态，
  // 即便点「刷新」后 GET 全部成功。任一 GET 成功即证明服务端可达，因此
  // 刷新与卡片「重试」成功后同样清除写离线态（dataUpdatedAt 只在成功取数时
  // 前进，失败不会误清除）。
  useEffect(() => {
    if (
      statsQuery.dataUpdatedAt > 0 ||
      logsQuery.dataUpdatedAt > 0 ||
      recentQuery.dataUpdatedAt > 0
    ) {
      setWriteOffline(false)
    }
  }, [
    statsQuery.dataUpdatedAt,
    logsQuery.dataUpdatedAt,
    recentQuery.dataUpdatedAt,
  ])

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

  const sourceQueries = [overviewQuery, modelsQuery, configQuery, sectionQuery]
  const sourceError = sourceQueries.find((query) => query.error !== null)?.error
  const sourceRefetching = sourceQueries.some((query) => query.isFetching)

  const refreshing =
    sourceRefetching ||
    statsQuery.isFetching ||
    logsQuery.isFetching ||
    recentQuery.isFetching

  const refreshAll = (): void => {
    void Promise.all([
      overviewQuery.refetch(),
      modelsQuery.refetch(),
      configQuery.refetch(),
      sectionQuery.refetch(),
      statsQuery.refetch(),
      logsQuery.refetch(),
      recentQuery.refetch(),
    ])
  }

  const retrySources = (): void => {
    void Promise.all(sourceQueries.map((query) => query.refetch()))
  }

  const overview = overviewQuery.data
  const models = modelsQuery.data
  const config = configQuery.data
  const section = sectionQuery.data
  const stats = statsQuery.data
  const logs = logsQuery.data

  // 找不到 prometheus 那条也视为断线（不臆造数据源）；overview 未加载时
  // 不提前弹横幅。
  const prometheus = overview?.sources.find(
    (source) => source.name === 'prometheus',
  )
  const sourceDown =
    overview !== undefined &&
    (prometheus === undefined || prometheus.status !== 'ok')

  const modelProbe = pickModelProbe(models)

  if (!wideEnough) {
    return (
      <div className="mx-auto max-w-[1520px] rounded-xl border border-border-hairline bg-surface-card p-8">
        <h1 className="text-page-title font-semibold text-ink-1">
          {t('admin.performance.page.title')}
        </h1>
        <p className="mt-2 text-body text-ink-2">
          {t('admin.performance.page.desktop_only')}
        </p>
      </div>
    )
  }

  const readOnlyNotice =
    !canManage && !me.isPending ? (
      <FormAlert tone="info">
        {t('admin.performance.common.read_only')}
      </FormAlert>
    ) : null

  return (
    <div
      className="mx-auto flex max-w-[1520px] flex-col gap-4"
      data-testid="performance-page"
    >
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-page-title font-semibold text-ink-1">
            {t('admin.performance.page.title')}
          </h1>
        </div>
        <div className="flex flex-wrap items-center gap-3">
          {/* 时间窗只驱动 overview（含 sparkline）；模型表固定 24h。 */}
          <div
            role="radiogroup"
            aria-label={t('common.dashboard.range.label')}
            onKeyDown={handleRadioGroupKeyDown}
            className="inline-flex rounded-lg border border-border-default bg-surface-card p-0.5"
          >
            {RANGES.map((option) => {
              const active = option === range
              return (
                <button
                  key={option}
                  type="button"
                  role="radio"
                  aria-checked={active}
                  tabIndex={active ? 0 : -1}
                  data-testid={`performance-range-${option}`}
                  onClick={() => setRange(option)}
                  className={[
                    'rounded-md px-2.5 py-1 text-meta focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                    active
                      ? 'bg-surface-hover font-medium text-ink-1'
                      : 'text-ink-2 hover:text-ink-1',
                  ].join(' ')}
                >
                  {t(RANGE_LABEL_KEYS[option])}
                </button>
              )
            })}
          </div>
          <Button
            variant="secondary"
            disabled={refreshing || !canManage}
            onClick={refreshAll}
          >
            {t('admin.performance.page.refresh')}
          </Button>
          <span
            data-testid="performance-last-updated"
            className="font-mono text-meta text-ink-3"
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
          className="text-body text-warning"
          role="status"
        >
          {t('admin.performance.page.offline_snapshot')}
        </p>
      ) : null}

      {sourceError !== null && sourceError !== undefined ? (
        <div
          data-testid="performance-source-error"
          className="flex flex-wrap items-center gap-3 rounded-lg border border-danger-line bg-danger-bg px-3 py-2 text-body text-danger"
          role="alert"
        >
          <span>{describePerformanceError(sourceError, t)}</span>
          <Button
            variant="ghost"
            className="h-7 px-2 text-body"
            disabled={sourceRefetching}
            onClick={retrySources}
          >
            {t('admin.performance.common.retry')}
          </Button>
        </div>
      ) : null}

      <DegradedBanner visible={sourceDown} />

      {overview !== undefined ? (
        <VitalsStrip
          vitals={overview.vitals}
          modelSummary={models?.summary}
          modelProbe={modelProbe}
          sourceDown={sourceDown}
        />
      ) : null}

      {models !== undefined ? (
        <ModelPerfZone models={models} sourceDown={sourceDown} />
      ) : null}

      {overview !== undefined ? (
        <GatewayZone
          gateways={overview.gateways}
          sourceDown={sourceDown}
          localStats={stats?.memory_stats}
        />
      ) : null}

      {overview !== undefined ? (
        <ResourcesZone resources={overview.resources} sourceDown={sourceDown} />
      ) : null}

      {config !== undefined && section !== undefined ? (
        <SettingsZone
          section={section}
          config={config}
          canManage={canManage}
          onSaved={() => {
            void Promise.all([configQuery.refetch(), sectionQuery.refetch()])
          }}
        />
      ) : null}

      <MaintenanceZone
        stats={stats}
        statsPending={statsQuery.isPending}
        statsError={statsQuery.error}
        statsStale={statsQuery.isError && stats !== undefined}
        logs={logs}
        logsPending={logsQuery.isPending}
        logsError={logsQuery.error}
        logsStale={logsQuery.isError && logs !== undefined}
        offline={offline}
        canManage={canManage}
        refetchStats={() => statsQuery.refetch()}
        refetchLogs={() => logsQuery.refetch()}
        refetchRecent={() => recentQuery.refetch()}
        onWriteOffline={setWriteOffline}
      />

      <RecentOpsZone
        recent={recentQuery.data}
        pending={recentQuery.isPending}
        error={recentQuery.error}
        retry={() => {
          void recentQuery.refetch()
        }}
      />
    </div>
  )
}
