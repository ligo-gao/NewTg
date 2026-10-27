import { useI18n } from '@/shared/lib/i18n-context'
import type { TranslateFn } from '@/shared/lib/i18n-context'
import { StatusBadge } from '@/shared/ui/status-badge'

import type { PerformanceStats, PerfOverview } from '../api'
import {
  formatBytes,
  formatCount,
  formatDateTime,
  formatMs,
  formatPercent,
} from '../utils'
import { codePhrase } from './degraded-banner'

/**
 * 网关实例表（adjudications §4）。心跳来自数据库，所以监控断线时表格照常
 * 渲染；只有逐副本指标列会退化成「指标不可用」，并在表下补一行本机进程
 * 数字（`GET /api/performance/stats`）——那是唯一在断线时仍可取到的数据。
 */

type Gateway = PerfOverview['gateways'][number]
type MemoryStats = PerformanceStats['memory_stats']

/** 本机行说明里的端点标识是代码，不是可翻译文案。 */
const LOCAL_STATS_ENDPOINT = 'GET /api/performance/stats'

function integerOrDash(
  value: number | null,
  locale: string,
): string {
  if (value === null || !Number.isFinite(value)) return '—'
  return formatCount(value, locale)
}

/**
 * 指标列的完整内容；`null` 表示整列应显示 `metrics_missing`。
 * 单个子字段缺失时保留该条并显示 `—`，不让整列消失。
 */
function metricsText(
  gateway: Gateway,
  locale: string,
  t: TranslateFn,
): string | null {
  if (gateway.metrics === null || gateway.metrics_status === 'unknown') {
    return null
  }

  const metrics = gateway.metrics
  const heap =
    metrics.heap_mb === null || !Number.isFinite(metrics.heap_mb)
      ? '—'
      : formatBytes(metrics.heap_mb * 1024 * 1024, locale)

  return [
    `${t('admin.performance.gateways.metric_p95')} ${formatMs(metrics.p95_ms)}`,
    `${t('admin.performance.gateways.metric_5xx')} ${formatPercent(metrics.error_rate)}`,
    `${t('admin.performance.gateways.metric_inflight')} ${integerOrDash(
      metrics.inflight,
      locale,
    )}`,
    `${t('admin.performance.gateways.metric_goroutines')} ${integerOrDash(
      metrics.goroutines,
      locale,
    )}`,
    `${t('admin.performance.gateways.metric_heap')} ${heap}`,
  ].join(' · ')
}

function LocalStatsNote() {
  const { t } = useI18n()

  return (
    <p className="mt-1 text-badge text-ink-3">
      {codePhrase(
        t('admin.performance.gateways.local_note'),
        LOCAL_STATS_ENDPOINT,
      )}
    </p>
  )
}

export interface GatewayZoneProps {
  gateways: PerfOverview['gateways']
  sourceDown: boolean
  localStats?: MemoryStats | undefined
}

export function GatewayZone({
  gateways,
  sourceDown,
  localStats,
}: GatewayZoneProps) {
  const { t, locale } = useI18n()

  // 按 instance_id 升序：服务端的发现顺序随 Prometheus 返回顺序变化，
  // 排序让同一组副本在每次刷新后保持稳定位置。
  const rows = [...gateways].sort((left, right) =>
    left.instance_id.localeCompare(right.instance_id),
  )

  return (
    <section aria-label={t('admin.performance.gateways.title')}>
      <div className="overflow-x-auto rounded-xl border border-border-hairline bg-surface-card">
        <table className="w-full min-w-[760px] text-body">
          <caption className="sr-only">
            {t('admin.performance.gateways.title')}
          </caption>
          <thead>
            <tr className="border-b border-border-hairline text-left text-badge text-ink-3">
              <th scope="col" className="px-4 py-2.5 font-medium">
                {t('admin.performance.gateways.col_id')}
              </th>
              <th scope="col" className="px-4 py-2.5 font-medium">
                {t('admin.performance.gateways.col_role')}
              </th>
              <th scope="col" className="px-4 py-2.5 font-medium">
                {t('admin.performance.gateways.col_version')}
              </th>
              <th scope="col" className="px-4 py-2.5 font-medium">
                {t('admin.performance.gateways.col_last_seen')}
              </th>
              <th scope="col" className="px-4 py-2.5 font-medium">
                {t('admin.performance.gateways.col_metrics')}
              </th>
            </tr>
          </thead>
          <tbody>
            {rows.map((gateway) => {
              const metrics = metricsText(gateway, locale, t)

              return (
                <tr
                  key={gateway.instance_id}
                  data-testid="gateway-row"
                  className="border-b border-border-hairline last:border-b-0"
                >
                  <th scope="row" className="px-4 py-2.5 text-left font-normal">
                    <code className="font-mono text-meta text-ink-1">
                      {gateway.instance_id}
                    </code>
                  </th>
                  <td className="px-4 py-2.5 text-meta text-ink-2">
                    {gateway.role === '' ? '—' : gateway.role}
                  </td>
                  <td className="px-4 py-2.5 font-mono text-badge text-ink-2">
                    {gateway.version === '' ? '—' : gateway.version}
                  </td>
                  <td className="px-4 py-2.5">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="font-mono text-badge text-ink-2">
                        {formatDateTime(gateway.last_seen, locale)}
                      </span>
                      {gateway.heartbeat_fresh ? null : (
                        <StatusBadge variant="warning">
                          {t('admin.performance.gateways.heartbeat_stale')}
                        </StatusBadge>
                      )}
                    </div>
                  </td>
                  <td className="px-4 py-2.5 text-badge text-ink-2">
                    {metrics === null ? (
                      <span className="text-ink-3">
                        {t('admin.performance.gateways.metrics_missing')}
                      </span>
                    ) : (
                      <span className="font-mono tabular-nums">{metrics}</span>
                    )}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>

      {sourceDown ? (
        <div
          data-testid="gateway-local"
          className="mt-3 rounded-lg border border-dashed border-border-default bg-surface-subtle px-4 py-3.5"
        >
          <h3 className="text-meta font-semibold text-ink-1">
            {t('admin.performance.gateways.local_title')}
          </h3>
          <LocalStatsNote />
          {localStats === undefined ? null : (
            <dl className="mt-3 flex flex-wrap gap-x-6 gap-y-2 text-badge">
              <div className="flex items-baseline gap-2">
                <dt className="text-ink-3">
                  {t('admin.performance.gateways.metric_goroutines')}
                </dt>
                <dd className="font-mono tabular-nums text-ink-2">
                  {formatCount(localStats.num_goroutine, locale)}
                </dd>
              </div>
              <div className="flex items-baseline gap-2">
                <dt className="text-ink-3">
                  {t('admin.performance.gateways.metric_heap')}
                </dt>
                <dd className="font-mono tabular-nums text-ink-2">
                  {formatBytes(localStats.alloc_bytes, locale)}
                </dd>
              </div>
            </dl>
          )}
        </div>
      ) : null}
    </section>
  )
}
