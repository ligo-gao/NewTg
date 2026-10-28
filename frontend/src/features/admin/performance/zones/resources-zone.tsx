import { useId } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { StatusBadge } from '@/shared/ui/status-badge'

import type { PerfOverview } from '../api'
import {
  formatBytes,
  formatCount,
  formatDecimal,
  formatPercent,
} from '../utils'
import { codePhrase } from './degraded-banner'
import { ZoneRail } from './primitives'

/**
 * 服务器资源三卡（adjudications §5）。数值全部来自 Prometheus 资源查询；
 * 监控断线时整区替换为虚线空态，契约里可空的字段（null = 无样本）渲染为
 * `—`，绝不能当成真实的 0 展示出来。
 */

type Resources = PerfOverview['resources']

interface ResourceRow {
  label: string
  value: string
}

function ResourceCard({
  resourceKey,
  title,
  badge,
  rows,
}: {
  resourceKey: string
  title: string
  badge?: { variant: 'success' | 'danger'; text: string }
  rows: readonly ResourceRow[]
}) {
  return (
    <article
      data-testid="resource-card"
      data-resource={resourceKey}
      className="min-w-0 bg-surface-card px-4 py-3.5"
    >
      <div className="flex items-center justify-between gap-2">
        <h3 className="text-meta font-semibold text-ink-1">{title}</h3>
        {badge === undefined ? null : (
          <StatusBadge variant={badge.variant}>{badge.text}</StatusBadge>
        )}
      </div>
      <dl className="mt-3 grid grid-cols-2 gap-x-3 gap-y-2 text-badge">
        {rows.map((row) => (
          <div key={row.label} className="min-w-0">
            <dt className="text-ink-3">{row.label}</dt>
            <dd className="mt-0.5 font-mono tabular-nums text-ink-2">
              {row.value}
            </dd>
          </div>
        ))}
      </dl>
    </article>
  )
}

export interface ResourcesZoneProps {
  resources: Resources
  sourceDown: boolean
}

export function ResourcesZone({ resources, sourceDown }: ResourcesZoneProps) {
  const { t, locale } = useI18n()
  const titleId = useId()
  const rail = (
    <ZoneRail titleId={titleId} title={t('admin.performance.resources.title')} />
  )

  if (sourceDown) {
    return (
      <section aria-labelledby={titleId}>
        {rail}
        <div
          data-testid="resources-unavailable"
          className="rounded-lg border border-dashed border-border-default bg-surface-subtle px-5 py-5"
        >
          <p className="font-semibold text-ink-1">
            {t('admin.performance.resources.unavailable_title')}
          </p>
          <p className="mt-1.5 max-w-[72ch] text-meta text-ink-2">
            {codePhrase(
              t('admin.performance.resources.unavailable_note'),
              t('admin.performance.banner.code'),
            )}
          </p>
        </div>
      </section>
    )
  }

  const { host, postgres, redis } = resources
  // null（未知）不是故障：不渲染徽标。只有明确的 1/0 才给出 up/down。
  const redisUp = redis.up === null ? null : redis.up > 0
  // 这三行是「每秒」量：单位来自词典（en `/s`、zh `/秒`），不硬编码。
  // 未知（`—`）不能再挂单位——「带单位的未知」会被读成一个数值。
  const perSecond = t('admin.performance.common.per_second')
  const withPerSecond = (text: string): string =>
    text === '—' ? text : `${text}${perSecond}`

  const hostRows: ResourceRow[] = [
    { label: t('admin.performance.resources.cpu'), value: formatPercent(host.cpu) },
    { label: t('admin.performance.resources.mem'), value: formatPercent(host.mem) },
    {
      label: t('admin.performance.resources.disk'),
      value: formatPercent(host.disk),
    },
    {
      label: t('admin.performance.resources.net_in'),
      value: withPerSecond(formatBytes(host.net_in, locale)),
    },
    {
      label: t('admin.performance.resources.net_out'),
      value: withPerSecond(formatBytes(host.net_out, locale)),
    },
  ]

  const postgresRows: ResourceRow[] = [
    {
      label: t('admin.performance.resources.backends'),
      // 两边都未知时整行只显示 `—`；单边未知自然得到 `11 / —`。
      value:
        postgres.backends === null && postgres.max === null
          ? '—'
          : `${formatCount(postgres.backends, locale)} / ${formatCount(
              postgres.max,
              locale,
            )}`,
    },
    {
      label: t('admin.performance.resources.tps'),
      value: formatCount(postgres.tps, locale),
    },
    {
      label: t('admin.performance.resources.size'),
      value: formatBytes(postgres.size_bytes, locale),
    },
    {
      label: t('admin.performance.resources.lock_wait'),
      value: formatDecimal(postgres.lock_wait, locale),
    },
  ]

  const redisRows: ResourceRow[] = [
    {
      label: t('admin.performance.resources.clients'),
      value: formatCount(redis.clients, locale),
    },
    {
      label: t('admin.performance.resources.hit_rate'),
      value: formatPercent(redis.hit_rate),
    },
    {
      label: t('admin.performance.resources.cmd_rate'),
      value: withPerSecond(formatCount(redis.cmd_rate, locale)),
    },
  ]

  return (
    <section aria-labelledby={titleId}>
      {rail}
      <div className="grid grid-cols-1 gap-px overflow-hidden rounded-xl border border-border-hairline bg-border-hairline md:grid-cols-3">
        <ResourceCard
          resourceKey="host"
          title={t('admin.performance.resources.host')}
          rows={hostRows}
        />
        <ResourceCard
          resourceKey="postgres"
          title={t('admin.performance.resources.postgres')}
          rows={postgresRows}
        />
        <ResourceCard
          resourceKey="redis"
          title={t('admin.performance.resources.redis')}
          badge={
            redisUp === null
              ? undefined
              : {
                  variant: redisUp ? 'success' : 'danger',
                  text: t(
                    redisUp
                      ? 'admin.performance.resources.redis_up'
                      : 'admin.performance.resources.redis_down',
                  ),
                }
          }
          rows={redisRows}
        />
      </div>
    </section>
  )
}
