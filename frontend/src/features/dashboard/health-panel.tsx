import type { SystemStatusResponse } from '@/features/admin/system/system-status-page'
import { useI18n } from '@/shared/lib/i18n-context'

export interface HealthPanelProps {
  system: SystemStatusResponse | undefined
  perfEnabled: boolean | undefined
  loading: boolean
  error: boolean
}

const LABELS: Record<string, string> = {
  postgres: 'PostgreSQL',
  postgresql: 'PostgreSQL',
  database: 'PostgreSQL',
  redis: 'Redis',
}

function stateOf(value: unknown): 'healthy' | 'degraded' | 'unknown' {
  if (typeof value === 'boolean') return value ? 'healthy' : 'degraded'
  if (typeof value === 'string') {
    const token = value.toLowerCase()
    if (['ok', 'up', 'healthy', 'ready', 'alive', 'connected', 'active'].includes(token)) {
      return 'healthy'
    }
    if (['down', 'error', 'unhealthy', 'degraded', 'not ready', 'failed', 'stale', 'offline'].includes(token)) {
      return 'degraded'
    }
  }
  if (typeof value === 'object' && value !== null) {
    const record = value as Record<string, unknown>
    if (typeof record.ok === 'boolean') return record.ok ? 'healthy' : 'degraded'
    if (typeof record.healthy === 'boolean') return record.healthy ? 'healthy' : 'degraded'
    if (record.status !== undefined) return stateOf(record.status)
  }
  return 'unknown'
}

/**
 * 系统健康：只展示有权限拿到的数据。权限不足时页面不会挂载本面板；
 * 单个依赖缺失显示“未知”，不伪造“正常”。
 */
export function HealthPanel({ system, perfEnabled, loading, error }: HealthPanelProps) {
  const { t } = useI18n()
  const dependencies: Array<[string, unknown]> =
    system?.dependencies !== undefined && typeof system.dependencies === 'object'
      ? Object.entries(system.dependencies)
      : []
  const instanceCount = Array.isArray(system?.instances)
    ? system.instances.length
    : undefined

  const rows: Array<{ key: string; label: string; state: 'healthy' | 'degraded' | 'unknown'; value: string }> = []
  if (dependencies.length > 0) {
    for (const [key, value] of dependencies) {
      const state = stateOf(value)
      rows.push({
        key,
        label: LABELS[key] ?? key,
        state,
        value: t(
          state === 'healthy'
            ? 'common.dashboard.overview.health.healthy'
            : state === 'degraded'
              ? 'common.dashboard.overview.health.degraded'
              : 'common.dashboard.overview.health.unknown',
        ),
      })
    }
  }
  if (instanceCount !== undefined) {
    rows.push({
      key: 'instances',
      label: t('common.dashboard.overview.health.instances'),
      state: instanceCount > 0 ? 'healthy' : 'degraded',
      value: t('common.dashboard.overview.health.instances_value', {
        count: instanceCount,
      }),
    })
  }
  rows.push({
    key: 'perf',
    label: t('common.dashboard.overview.health.perf'),
    state: perfEnabled === true ? 'healthy' : perfEnabled === false ? 'degraded' : 'unknown',
    value:
      perfEnabled === true
        ? t('common.dashboard.overview.health.perf_on')
        : perfEnabled === false
          ? t('common.dashboard.overview.health.perf_off')
          : t('common.dashboard.overview.health.unknown'),
  })

  return (
    <section
      aria-label={t('common.dashboard.overview.health.title')}
      data-testid="health-panel"
      className="rounded-xl border border-border-hairline bg-surface-card p-4"
    >
      <div className="flex items-baseline justify-between gap-2">
        <h2 className="text-body font-semibold text-ink-1">
          {t('common.dashboard.overview.health.title')}
        </h2>
        <span className="text-badge text-ink-3">
          {t('common.dashboard.overview.health.subtitle')}
        </span>
      </div>
      {loading ? (
        <div className="mt-3 h-28 animate-pulse rounded bg-surface-hover motion-reduce:animate-none" />
      ) : error ? (
        <p className="mt-3 text-badge text-ink-3">
          {t('common.dashboard.overview.health.error')}
        </p>
      ) : (
        <ul className="mt-1 list-none p-0">
          {rows.map((row) => (
            <li
              key={row.key}
              className="flex items-baseline gap-2 border-t border-border-hairline py-2 text-badge first:border-t-0"
            >
              <span
                aria-hidden="true"
                className={[
                  'size-[6px] shrink-0 rounded-full',
                  row.state === 'healthy'
                    ? 'bg-success'
                    : row.state === 'degraded'
                      ? 'bg-warning'
                      : 'bg-ink-4',
                ].join(' ')}
              />
              <span className="text-ink-2">{row.label}</span>
              <span className="ml-auto font-mono text-badge tabular-nums text-ink-2">
                {row.value}
              </span>
            </li>
          ))}
        </ul>
      )}
      <a
        href="/admin/system/status"
        className="mt-2 inline-block text-badge text-brand hover:underline"
      >
        {t('common.dashboard.overview.health.view')}
      </a>
    </section>
  )
}
