import { useI18n } from '@/shared/lib/i18n-context'
import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'

import type { ChannelStatus } from './types'

/**
 * Channel status is a three-state badge (modules-06 §1.3 / §5.1):
 * enabled = success, manual_disabled = neutral, auto_disabled = warning.
 *
 * `manual_disabled` is deliberately **neutral**, never `danger`: modules-00
 * §4.6.4 reserves danger for errors and irreversible confirmations and states
 * "禁用一律 neutral". (The older sub-functional design table still shows
 * `danger` for manual_disabled; the later UI design and the global rule win.)
 */
const STATUS_META: Record<ChannelStatus, { key: string; variant: StatusBadgeVariant }> = {
  1: { key: 'common.channels.status.enabled', variant: 'success' },
  2: { key: 'common.channels.status.manual_disabled', variant: 'neutral' },
  3: { key: 'common.channels.status.auto_disabled', variant: 'warning' },
}

const STATUS_VALUES: readonly ChannelStatus[] = [1, 2, 3]

export interface ChannelStatusBadgeProps {
  status: number
}

export function ChannelStatusBadge({ status }: ChannelStatusBadgeProps) {
  const { t } = useI18n()
  const meta = STATUS_VALUES.includes(status as ChannelStatus)
    ? STATUS_META[status as ChannelStatus]
    : undefined

  if (meta === undefined) {
    // 06-3: unknown values render an em dash, not a fourth "Unknown" state.
    return (
      <span
        data-testid="channel-status-unknown"
        aria-label={t('common.channels.status.unknown')}
        className="text-ink-3"
      >
        —
      </span>
    )
  }

  return (
    <span data-testid="channel-status-badge">
      <StatusBadge variant={meta.variant}>{t(meta.key)}</StatusBadge>
    </span>
  )
}
