import { useQuery } from '@tanstack/react-query'

import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'
import { Button } from '@/shared/ui/base/button'
import { useI18n } from '@/shared/lib/i18n-context'

import { fetchReadyz } from './api'

/** Probe cache key; one card per mounted surface shares the same entry. */
export const READYZ_QUERY_KEY = ['public', 'readyz'] as const

/**
 * F-09-13's recovery probe (`GET /readyz` every 30s). The public shell itself
 * must not embed this (modules-09 裁决一: `/healthz` `/readyz` are
 * infrastructure endpoints), so this card is a reusable building block for the
 * 503 recovery surface and for operators' status views — not a footer badge.
 *
 * Failure policy is frozen: a failed probe renders "status unavailable", never
 * "operational". A 503 with the probe envelope still counts as data (degraded),
 * because that is exactly what the card exists to show.
 */
export interface SiteStatusCardProps {
  /** Poll interval in ms; the 503 page spec freezes 30s. */
  pollIntervalMs?: number
}

type StatusState = 'checking' | 'operational' | 'degraded' | 'unavailable'

const STATE_VARIANT: Record<Exclude<StatusState, 'checking'>, StatusBadgeVariant> = {
  operational: 'success',
  degraded: 'warning',
  unavailable: 'danger',
}

export function SiteStatusCard({ pollIntervalMs = 30_000 }: SiteStatusCardProps) {
  const { t } = useI18n()
  const query = useQuery({
    queryKey: READYZ_QUERY_KEY,
    queryFn: ({ signal }) => fetchReadyz(signal),
    retry: false,
    refetchInterval: pollIntervalMs,
    refetchIntervalInBackground: false,
  })

  const state: StatusState = query.isPending
    ? 'checking'
    : query.isError
      ? 'unavailable'
      : query.data.status === 'ok'
        ? 'operational'
        : 'degraded'

  return (
    <section
      aria-labelledby="site-status-title"
      className="flex flex-col gap-3 rounded-xl border border-border-hairline bg-surface-card p-5"
    >
      <h2 id="site-status-title" className="text-public-body-strong text-ink-1">
        {t('public.status.title')}
      </h2>
      <div role="status" aria-live="polite" className="flex items-center gap-3">
        {state === 'checking' ? (
          <span className="text-public-body text-ink-2">{t('public.status.checking')}</span>
        ) : (
          <StatusBadge variant={STATE_VARIANT[state]}>{t(`public.status.${state}`)}</StatusBadge>
        )}
      </div>
      {state === 'unavailable' ? (
        <div className="flex items-center gap-3">
          <p className="text-public-body text-ink-2">{t('public.status.unavailable_hint')}</p>
          <Button variant="secondary" onClick={() => void query.refetch()} disabled={query.isFetching}>
            {t('public.status.retry')}
          </Button>
        </div>
      ) : null}
    </section>
  )
}
