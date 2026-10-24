import type { ReactNode } from 'react'

import { useI18n, type TranslateFn } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { Tag } from '@/shared/ui/tag'

import { channelDisabledReasonKey } from './channel-disabled-reason'
import { ChannelMetricsPanel } from './channel-metrics-panel'
import type { Channel } from './types'

/**
 * Expanded channel dossier (channels-preview.html).
 *
 * The dossier is a stack of cards on the subtle detail-row background: a
 * full-width model list (the only place models are enumerated), a row of
 * routing / channel-info / 24h-health cards, and the full-width metrics card
 * (`ChannelMetricsPanel`). The last-test cell mirrors the list column so the
 * dossier is self-sufficient.
 */

const CARD = 'min-w-0 rounded-lg border border-border-hairline bg-surface-card p-4'
const CARD_HEADING = 'text-badge font-semibold tracking-[0.02em] text-ink-2'
const ROW = 'flex items-start justify-between gap-3'
const ROW_LABEL = 'text-meta text-ink-2'
const ROW_VALUE = 'text-right text-body text-ink-1 tabular-nums'

function formatRelativeTime(iso: string, t: TranslateFn): string {
  const timestamp = Date.parse(iso)
  if (Number.isNaN(timestamp)) return '—'
  const seconds = Math.round((Date.now() - timestamp) / 1000)
  if (seconds < 60) return t('common.channels.time.justNow')
  const minutes = Math.round(seconds / 60)
  if (minutes < 60) return t('common.channels.time.minutesAgo', { count: minutes })
  const hours = Math.round(minutes / 60)
  if (hours < 24) return t('common.channels.time.hoursAgo', { count: hours })
  return t('common.channels.time.daysAgo', { count: Math.round(hours / 24) })
}

/**
 * Last-test value for the dossier. The list page no longer has a last-test
 * column (it was replaced by the 24h health cell), so this is the only place
 * the last-test state is surfaced.
 */
function LastTestCell({ channel }: { channel: Channel }) {
  const { t, locale } = useI18n()
  if (channel.last_test_at === null) {
    return <span className="text-meta text-ink-3">{t('common.channels.table.neverTested')}</span>
  }
  const timestamp = Date.parse(channel.last_test_at)
  const absolute = Number.isNaN(timestamp)
    ? channel.last_test_at
    : new Date(timestamp).toLocaleString(locale)
  const tone =
    channel.last_test_result === true
      ? 'text-success'
      : channel.last_test_result === false
        ? 'text-danger'
        : 'text-ink-2'
  return (
    <span title={absolute} className={`text-meta ${tone}`}>
      {channel.last_test_result === true
        ? '✓'
        : channel.last_test_result === false
          ? '✗'
          : ''}{' '}
      {formatRelativeTime(channel.last_test_at, t)}
    </span>
  )
}

function GroupsValue({ groups }: { groups: string[] | null }) {
  const { t } = useI18n()
  if (groups === null || groups.length === 0) {
    return <span className="text-meta text-ink-2">{t('common.channels.table.allGroups')}</span>
  }
  return (
    <span className="flex flex-wrap justify-end gap-1">
      {groups.map((group) => (
        <Tag key={group}>{group}</Tag>
      ))}
    </span>
  )
}

function FieldRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className={ROW}>
      <dt className={ROW_LABEL}>{label}</dt>
      <dd className={ROW_VALUE}>{children}</dd>
    </div>
  )
}

function formatPercent(value: number | null): string {
  return value === null ? '—' : `${(value * 100).toFixed(1)}%`
}

function formatLatency(value: number | null): string {
  return value === null ? '—' : `${Math.round(value)} ms`
}

export interface ChannelDetailPanelProps {
  channel: Channel
  /**
   * Opens the performance-test dialog. Optional so the panel stays usable
   * standalone (and in older call sites); the action only renders when given.
   */
  onPerformanceTest?: () => void
}

export function ChannelDetailPanel({
  channel,
  onPerformanceTest,
}: ChannelDetailPanelProps) {
  const { t, locale } = useI18n()
  const updatedAt = new Date(channel.updated_at).toLocaleString(locale)
  const summary = channel.metrics

  const rows: Array<{ label: string; value: string }> = [
    { label: t('common.channels.detail.id'), value: String(channel.id) },
    {
      label: t('common.channels.detail.rateLimit'),
      value:
        channel.rate_limit === null
          ? t('common.channels.detail.unlimited')
          : String(channel.rate_limit),
    },
    {
      label: t('common.channels.detail.autoBan'),
      value: channel.auto_ban
        ? t('common.channels.detail.yes')
        : t('common.channels.detail.no'),
    },
    {
      label: t('common.channels.detail.failCount'),
      value: String(channel.fail_count),
    },
    {
      label: t('common.channels.detail.tag'),
      value: channel.tag ?? '—',
    },
    {
      label: t('common.channels.detail.updated'),
      value: updatedAt,
    },
  ]
  // 自动禁用原因只在 status=3 且 code 非空时显示（空值隐藏）。时间复用
  // updated_at，因此文案是「最近更新」而不是「禁用时刻」（R-M4-05）。
  if (channel.status === 3 && channel.auto_disabled_reason) {
    rows.splice(rows.length - 1, 0, {
      label: t('common.channels.detail.autoDisabledReason'),
      value: `${t(channelDisabledReasonKey(channel.auto_disabled_reason))} · ${t(
        'common.channels.detail.reasonUpdatedAt',
        { at: updatedAt },
      )}`,
    })
  }

  return (
    <div className="flex flex-col gap-3">
      <section className={CARD}>
        <div className="flex items-center justify-between gap-3">
          <h3 className={CARD_HEADING}>
            {t('common.channels.detail.modelsTitle', {
              count: channel.supported_models.length,
            })}
          </h3>
          {onPerformanceTest === undefined ? null : (
            <Button variant="secondary" onClick={onPerformanceTest}>
              {t('common.channels.actions.performance')}
            </Button>
          )}
        </div>
        {channel.supported_models.length === 0 ? (
          <p className="mt-2 text-body text-ink-3">—</p>
        ) : (
          <div className="mt-2 flex flex-wrap gap-1.5">
            {channel.supported_models.map((model) => (
              <span
                key={model}
                className="inline-flex items-center rounded-md border border-border-hairline bg-surface-subtle px-2 py-0.5 font-mono text-badge text-ink-1"
              >
                {model}
              </span>
            ))}
          </div>
        )}
      </section>

      <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-[minmax(0,1fr)_minmax(0,1.5fr)_minmax(0,1fr)]">
        <section className={CARD}>
          <h3 className={CARD_HEADING}>
            {t('common.channels.table.routingGroups')}
          </h3>
          <dl className="mt-2 flex flex-col gap-1.5">
            <FieldRow label={t('common.channels.table.priority')}>
              {channel.priority}
            </FieldRow>
            <FieldRow label={t('common.channels.table.weight')}>
              {channel.weight}
            </FieldRow>
            <FieldRow label={t('common.channels.table.groups')}>
              <GroupsValue groups={channel.group_tags} />
            </FieldRow>
          </dl>
        </section>

        <section className={CARD}>
          <h3 className={CARD_HEADING}>{t('common.channels.detail.infoTitle')}</h3>
          <dl className="mt-2 flex flex-col gap-1.5">
            {rows.map((row) => (
              <FieldRow key={row.label} label={row.label}>
                {row.value}
              </FieldRow>
            ))}
          </dl>
        </section>

        <section className={CARD}>
          <h3 className={CARD_HEADING}>
            {t('common.channels.table.health24h')}
          </h3>
          <dl className="mt-2 flex flex-col gap-1.5">
            <FieldRow label={t('common.channels.detail.lastTest')}>
              <LastTestCell channel={channel} />
            </FieldRow>
            {summary === undefined ? null : (
              <>
                <FieldRow label={t('common.channels.table.successRate24h')}>
                  {formatPercent(summary.success_rate)}
                </FieldRow>
                <FieldRow label={t('common.channels.table.avgLatency24h')}>
                  {formatLatency(summary.avg_latency_ms)}
                </FieldRow>
              </>
            )}
          </dl>
        </section>
      </div>

      {/* The list DTO deliberately omits credentials_masked and credentials_set
          (GC-6); only the detail endpoint returns them. Expanding a list row
          therefore has no credentials cell to render. */}
      <ChannelMetricsPanel channel={channel} />
    </div>
  )
}
