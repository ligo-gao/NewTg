import { useI18n } from '@/shared/lib/i18n-context'
import { StatusBadge } from '@/shared/ui/status-badge'

import type { PerfOverview } from '../api'
import {
  formatBytes,
  formatCount,
  formatPercent,
} from '../utils'
import { codePhrase } from './degraded-banner'

/**
 * 服务器资源三卡（adjudications §5）。数值全部来自 Prometheus 资源查询；
 * 监控断线时整区替换为虚线空态，绝不能把契约里「缺失填 0」的字段当成真实的
 * 0 展示出来。
 */

type Resources = PerfOverview['resources']

interface ResourceRow {
  label: string
  value: string
  /** 值缺失时用弱色，避免 `—` 看起来像读数。 */
  muted?: boolean
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
            <dd
              className={[
                'mt-0.5 font-mono tabular-nums',
                row.muted === true ? 'text-ink-3' : 'text-ink-2',
              ].join(' ')}
            >
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

  if (sourceDown) {
    return (
      <section aria-label={t('admin.performance.resources.title')}>
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
  const redisUp = redis.up > 0

  const hostRows: ResourceRow[] = [
    { label: t('admin.performance.resources.cpu'), value: formatPercent(host.cpu) },
    { label: t('admin.performance.resources.mem'), value: formatPercent(host.mem) },
    {
      label: t('admin.performance.resources.disk'),
      value: formatPercent(host.disk),
    },
    {
      label: t('admin.performance.resources.net_in'),
      value: formatBytes(host.net_in, locale),
    },
    {
      label: t('admin.performance.resources.net_out'),
      value: formatBytes(host.net_out, locale),
    },
  ]

  const postgresRows: ResourceRow[] = [
    {
      label: t('admin.performance.resources.backends'),
      value: `${formatCount(postgres.backends, locale)} / ${formatCount(
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
      value: formatCount(redis.cmd_rate, locale),
    },
  ]

  return (
    <section
      aria-label={t('admin.performance.resources.title')}
      className="grid grid-cols-1 gap-px overflow-hidden rounded-xl border border-border-hairline bg-border-hairline md:grid-cols-3"
    >
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
        badge={{
          variant: redisUp ? 'success' : 'danger',
          text: t(
            redisUp
              ? 'admin.performance.resources.redis_up'
              : 'admin.performance.resources.redis_down',
          ),
        }}
        rows={redisRows}
      />
    </section>
  )
}
