import { useI18n } from '@/shared/lib/i18n-context'

/**
 * Models column for the channel table.
 *
 * Only a folded count is rendered here — the dossier in `ChannelDetailPanel`
 * is the single place that lists every model. The button owns the expansion
 * state so the count and the detail row stay in sync.
 */
export interface ChannelModelCellProps {
  channelId: number
  channelName: string
  models: string[]
  expanded: boolean
  onToggle: () => void
}

export function ChannelModelCell({
  channelId,
  channelName,
  models,
  expanded,
  onToggle,
}: ChannelModelCellProps) {
  const { t } = useI18n()

  if (models.length === 0) {
    return <span className="text-meta text-ink-3">—</span>
  }

  return (
    <button
      type="button"
      aria-expanded={expanded}
      // The detail row only exists while expanded; pointing at it when
      // collapsed would reference a missing element.
      aria-controls={expanded ? `channel-detail-${channelId}` : undefined}
      aria-label={t('common.channels.table.modelsCountFor', {
        count: models.length,
        name: channelName,
      })}
      onClick={onToggle}
      className="inline-flex h-7 items-center gap-1 rounded-md border border-border-default bg-surface-card px-2 text-badge text-ink-1 transition-colors hover:border-brand-line hover:bg-brand-subtle hover:text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
    >
      <svg
        data-testid={`channel-model-chevron-${channelId}`}
        viewBox="0 0 20 20"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
        className={[
          'size-3.5 shrink-0 transition-transform',
          expanded ? 'rotate-90' : '',
        ].join(' ')}
      >
        <path d="m8 5 5 5-5 5" />
      </svg>
      {t('common.channels.table.modelsCount', { count: models.length })}
    </button>
  )
}
