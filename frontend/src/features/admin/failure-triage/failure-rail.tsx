/**
 * 最近失败索引栏。
 *
 * 只负责列表与归属筛选：数据、选中项与查询状态都由页面持有，点击行只
 * 回传 requestId。行内的判定色与案卷印章使用同一套 token。
 */
import type { TriageOwner } from './verdict-copy'
import type { RecentFailures } from './api'
import { normalizeRequestId } from '@/shared/lib/request-id'
import { useI18n } from '@/shared/lib/i18n-context'
import { ownerDotClass, ownerLabelKey, ownerTextClass } from './verdict-copy'

export type OwnerFilter = 'all' | TriageOwner

const FILTERS: { id: OwnerFilter; key: string }[] = [
  { id: 'all', key: 'admin.failure_triage.rail.tab_all' },
  { id: 'user', key: 'admin.failure_triage.rail.tab_user' },
  { id: 'channel', key: 'admin.failure_triage.rail.tab_channel' },
  { id: 'gateway', key: 'admin.failure_triage.rail.tab_gateway' },
]

function formatClock(value: string, locale: string): string {
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp)) return '—'
  return new Intl.DateTimeFormat(locale, {
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  }).format(timestamp)
}

function formatMoment(value: string, locale: string): string {
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp)) return '—'
  return new Intl.DateTimeFormat(locale, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  }).format(timestamp)
}

export interface FailureRailProps {
  data: RecentFailures | undefined
  isPending: boolean
  isError: boolean
  owner: OwnerFilter
  onOwnerChange: (owner: OwnerFilter) => void
  selectedId: string | null
  onSelect: (requestId: string) => void
  onRetry: () => void
}

export function FailureRail({
  data,
  isPending,
  isError,
  owner,
  onOwnerChange,
  selectedId,
  onSelect,
  onRetry,
}: FailureRailProps) {
  const { t, locale } = useI18n()
  const items = data?.items ?? []
  const window = data?.window

  return (
    <aside
      aria-label={t('admin.failure_triage.rail.heading')}
      className="flex min-w-0 flex-col border-r border-border-hairline bg-surface-subtle max-[1100px]:border-r-0 max-[1100px]:border-b"
      data-testid="failure-rail"
    >
      <div className="flex items-baseline justify-between gap-2.5 px-4 pt-4 pb-2.5">
        <h2 className="text-meta font-semibold text-ink-1">
          {t('admin.failure_triage.rail.heading')}
        </h2>
        <span className="text-badge text-ink-3">
          {t('admin.failure_triage.rail.count', {
            count: data?.counts.total ?? '—',
          })}
        </span>
      </div>

      <div
        role="group"
        aria-label={t('admin.failure_triage.rail.heading')}
        className="flex gap-3.5 border-b border-border-hairline px-4"
      >
        {FILTERS.map((filter) => {
          const active = filter.id === owner
          return (
            <button
              key={filter.id}
              type="button"
              aria-pressed={active}
              onClick={() => onOwnerChange(filter.id)}
              className={[
                'relative pb-2 text-badge',
                active
                  ? 'font-semibold text-ink-1 after:absolute after:inset-x-0 after:-bottom-px after:h-0.5 after:bg-ink-1'
                  : 'text-ink-2 hover:text-ink-1',
              ].join(' ')}
            >
              {t(filter.key)}
            </button>
          )
        })}
      </div>

      <div
        role="listbox"
        aria-label={t('admin.failure_triage.rail.heading')}
        className="flex max-h-[640px] flex-col overflow-auto max-[1100px]:max-h-[260px]"
      >
        {isPending ? (
          <div className="flex flex-col gap-3 p-4" aria-hidden="true">
            {[0, 1, 2, 3].map((row) => (
              <div key={row} className="flex flex-col gap-1.5">
                <span className="h-3 w-24 rounded bg-ink-4/30" />
                <span className="h-3 w-40 rounded bg-ink-4/30" />
              </div>
            ))}
          </div>
        ) : isError ? (
          <div className="flex flex-col items-start gap-2 p-4" data-testid="rail-error">
            <p className="text-meta text-ink-2">
              {t('admin.failure_triage.error.recent')}
            </p>
            <button
              type="button"
              onClick={onRetry}
              className="h-8 rounded-lg border border-border-default bg-surface-card px-3 text-badge text-ink-1 hover:bg-surface-hover"
            >
              {t('admin.failure_triage.error.retry')}
            </button>
          </div>
        ) : items.length === 0 ? (
          <p className="px-4 py-7 text-meta text-ink-2">
            {t('admin.failure_triage.rail.empty')}
          </p>
        ) : (
          items.map((item) => {
            const selected =
              selectedId !== null &&
              normalizeRequestId(selectedId) === normalizeRequestId(item.request_id)
            return (
              <button
                key={item.request_id}
                type="button"
                role="option"
                aria-selected={selected}
                data-testid="failure-row"
                data-owner={item.owner}
                onClick={() => onSelect(item.request_id)}
                className={[
                  'flex w-full flex-col gap-1 border-b border-border-hairline px-4 py-2.5 text-left last:border-b-0',
                  selected
                    ? 'bg-surface-card shadow-[inset_0_0_0_1px_var(--border-hairline)]'
                    : 'bg-transparent hover:bg-surface-hover',
                  'focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]',
                ].join(' ')}
              >
                <span className="flex min-w-0 items-baseline gap-1.5">
                  <span
                    className={[
                      'inline-flex items-center gap-1.5 text-badge font-semibold',
                      ownerTextClass(item.owner),
                    ].join(' ')}
                  >
                    <span
                      aria-hidden="true"
                      className={[
                        'size-1.5 flex-none rounded-full',
                        ownerDotClass(item.owner),
                      ].join(' ')}
                    />
                    {t(ownerLabelKey(item.owner))}
                  </span>
                  <span className="ml-auto text-badge tabular-nums text-ink-3">
                    {formatClock(item.created_at, locale)}
                  </span>
                </span>
                <span className="flex min-w-0 items-baseline gap-2 text-badge text-ink-2">
                  <span className="truncate font-medium text-ink-1">{item.model}</span>
                  <span className="truncate font-mono text-ink-3">
                    {item.request_id}
                  </span>
                </span>
              </button>
            )
          })
        )}
      </div>

      <div className="mt-auto border-t border-border-hairline px-4 py-2.5 text-badge leading-4 text-ink-3">
        {t('admin.failure_triage.rail.footer')}
        {window !== undefined ? (
          <span className="mt-1 block">
            {t('admin.failure_triage.rail.window', {
              start: formatMoment(window.start, locale),
              end: formatMoment(window.end, locale),
            })}
          </span>
        ) : null}
      </div>
    </aside>
  )
}
