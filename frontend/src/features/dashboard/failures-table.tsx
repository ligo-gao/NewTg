import type { components } from '@/api/schema'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { useI18n } from '@/shared/lib/i18n-context'

import { UNAVAILABLE, formatCurrencyValue, formatCreditsValue } from './format'

type UsageLogItem = components['schemas']['UsageLogItem']

export interface FailuresTableProps {
  title: string
  subtitle: string
  items: UsageLogItem[]
  loading: boolean
  error: boolean
  onRetry: () => void
  showUser: boolean
  emptyLabel: string
  testId: string
  showCurrency: boolean
  quotaPerUnit: number
  currencySymbol: string
  locale: string
}

const STATUS_CLASS: Record<string, string> = {
  success: 'bg-success-bg text-success',
  error: 'bg-danger-bg text-danger',
  refunded: 'bg-warning-bg text-warning',
  refused: 'bg-neutral-bg text-neutral',
}

/**
 * 最近请求 / 最近失败请求。管理员多一列用户；普通用户只看自己的行。
 * 时间使用相对时间，悬停可读原始 ISO（title 属性）。
 */
export function FailuresTable({
  title,
  subtitle,
  items,
  loading,
  error,
  onRetry,
  showUser,
  emptyLabel,
  testId,
  showCurrency,
  quotaPerUnit,
  currencySymbol,
  locale,
}: FailuresTableProps) {
  const { t } = useI18n()

  const cost = (value: number) =>
    showCurrency
      ? formatCurrencyValue(value, quotaPerUnit, currencySymbol, locale)
      : formatCreditsValue(value, locale)

  return (
    <section
      aria-label={title}
      data-testid={testId}
      className="rounded-xl border border-border-hairline bg-surface-card p-4"
    >
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <div>
          <h2 className="text-body font-semibold text-ink-1">{title}</h2>
          <p className="mt-0.5 text-badge text-ink-3">{subtitle}</p>
        </div>
        <a href="/dashboard/usage-logs" className="text-badge text-brand hover:underline">
          {t('common.dashboard.overview.failures.view_all')}
        </a>
      </div>

      {loading ? (
        <div className="mt-3 h-28 animate-pulse rounded bg-surface-hover motion-reduce:animate-none" />
      ) : error ? (
        <div className="flex h-28 flex-col items-center justify-center gap-2 text-body text-ink-3">
          <p>{t('common.dashboard.overview.failures.load_error')}</p>
          <button type="button" onClick={onRetry} className="text-brand hover:underline">
            {t('common.action.retry')}
          </button>
        </div>
      ) : items.length === 0 ? (
        <p className="mt-4 text-badge text-ink-3">{emptyLabel}</p>
      ) : (
        <div className="mt-2 overflow-x-auto">
          <table className="w-full min-w-[680px] border-collapse text-badge">
            <thead>
              <tr className="border-b border-border-hairline text-badge text-ink-3">
                <th className="pb-1.5 text-left font-normal">
                  {t('common.dashboard.overview.failures.time')}
                </th>
                <th className="pb-1.5 text-left font-normal">
                  {t('common.dashboard.overview.failures.model')}
                </th>
                <th className="pb-1.5 text-left font-normal">
                  {t('common.dashboard.overview.failures.status')}
                </th>
                <th className="pb-1.5 text-left font-normal">
                  {t('common.dashboard.overview.failures.error')}
                </th>
                <th className="pb-1.5 text-right font-normal">
                  {t('common.dashboard.overview.failures.latency')}
                </th>
                <th className="pb-1.5 text-right font-normal">
                  {t('common.dashboard.overview.failures.cost')}
                </th>
                {showUser ? (
                  <th className="pb-1.5 text-left font-normal">
                    {t('common.dashboard.overview.failures.user')}
                  </th>
                ) : null}
              </tr>
            </thead>
            <tbody>
              {items.map((item) => (
                <tr
                  key={item.id}
                  className="border-b border-border-hairline last:border-b-0 hover:bg-surface-hover"
                >
                  <td className="py-2 pr-3 font-mono text-badge text-ink-2" title={item.created_at}>
                    {formatRelativeTime(item.created_at, locale)}
                  </td>
                  <td className="max-w-[150px] truncate py-2 pr-3 font-mono text-badge text-ink-1">
                    {item.model}
                  </td>
                  <td className="py-2 pr-3">
                    <span
                      className={[
                        'inline-flex items-center gap-1 rounded-full px-1.5 py-0.5 text-badge',
                        STATUS_CLASS[item.status] ?? STATUS_CLASS.refused,
                      ].join(' ')}
                    >
                      <span aria-hidden="true" className="size-1 rounded-full bg-current" />
                      {t(`common.dashboard.overview.failures.status_${item.status}`)}
                    </span>
                  </td>
                  <td className="max-w-[160px] truncate py-2 pr-3 font-mono text-badge text-danger">
                    {item.error_class ?? '—'}
                  </td>
                  <td className="py-2 pr-3 text-right font-mono text-badge tabular-nums text-ink-2">
                    {item.latency_ms === null || item.latency_ms === undefined
                      ? UNAVAILABLE
                      : `${item.latency_ms}ms`}
                  </td>
                  <td className="py-2 pr-3 text-right font-mono text-badge tabular-nums text-ink-2">
                    {cost(item.cost)}
                  </td>
                  {showUser ? (
                    <td className="py-2 font-mono text-badge text-ink-2">
                      {item.username ?? '—'}
                    </td>
                  ) : null}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
