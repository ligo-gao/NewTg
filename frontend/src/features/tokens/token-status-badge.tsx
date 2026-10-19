import { useI18n } from '@/shared/lib/i18n-context'
import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'

import type { TokenStatus } from './api'

const VARIANTS: Record<TokenStatus, StatusBadgeVariant> = {
  enabled: 'success',
  disabled: 'neutral',
  expired: 'warning',
  exhausted: 'danger',
}

const HINT_KEYS: Record<TokenStatus, string> = {
  enabled: 'common.tokens.status.enabled',
  disabled: 'common.tokens.status.disabledHint',
  expired: 'common.tokens.status.expiredHint',
  exhausted: 'common.tokens.status.exhaustedHint',
}

export interface TokenStatusBadgeProps {
  status: TokenStatus
  /** ISO expiry, used only for the expired tooltip. */
  expiresAt?: string | null
}

/**
 * Server-derived token status (modules-02 §2.10). The projection computes the
 * value; the client never re-derives priority here. Colour is never the only
 * encoding: the badge always carries a dot and the status word, and the title
 * adds the reason for assistive/colour-blind readers.
 */
export function TokenStatusBadge({ status, expiresAt }: TokenStatusBadgeProps) {
  const { t, locale } = useI18n()
  const hint =
    status === 'expired' && expiresAt
      ? t('common.tokens.status.expiredHint', {
          date: new Date(expiresAt).toLocaleString(locale),
        })
      : t(HINT_KEYS[status])
  return (
    <span title={hint}>
      <StatusBadge variant={VARIANTS[status]}>{t(`common.tokens.status.${status}`)}</StatusBadge>
    </span>
  )
}
