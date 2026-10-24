import type { components } from '@/api/schema'
import { useI18n } from '@/shared/lib/i18n-context'

import type { UsageStatsByModelItem } from './api'
import {
  UNAVAILABLE,
  formatCreditsValue,
  formatCurrencyValue,
  formatInteger,
  formatTokensValue,
} from './format'

type ByUser = components['schemas']['UsageStatsByUserResponse']
type ByGroup = components['schemas']['UsageStatsByGroupResponse']

export type CompositionMode = 'users' | 'groups'
export type CompositionVariant = 'audience' | 'model'

export interface CompositionProps {
  role: 'admin' | 'user'
  variant: CompositionVariant
  mode: CompositionMode
  onModeChange: (mode: CompositionMode) => void
  byUser: ByUser | undefined
  byGroup: ByGroup | undefined
  byModel: UsageStatsByModelItem[] | undefined
  loading: boolean
  error: boolean
  onRetry: () => void
  selectedModel?: string
  onSelectModel: (model: string | undefined) => void
  showCurrency: boolean
  quotaPerUnit: number
  currencySymbol: string
  locale: string
}

interface Row {
  key: string
  name: string
  cost: number
  requests: number
  tokens: number
  percent: number
}

/**
 * 仪表台构成面板。`audience` 在按用户/按分组之间切换（管理员）；
 * `model` 展示按模型消费，点击行会把主趋势图筛到该模型。
 */
export function Composition({
  role,
  variant,
  mode,
  onModeChange,
  byUser,
  byGroup,
  byModel,
  loading,
  error,
  onRetry,
  selectedModel,
  onSelectModel,
  showCurrency,
  quotaPerUnit,
  currencySymbol,
  locale,
}: CompositionProps) {
  const { t } = useI18n()

  const credits = (value: number) =>
    showCurrency
      ? formatCurrencyValue(value, quotaPerUnit, currencySymbol, locale)
      : formatCreditsValue(value, locale)

  const isAdmin = role === 'admin'
  const isModel = variant === 'model'

  const rows: Row[] = isModel
    ? (byModel ?? []).map((item) => ({
        key: `model-${item.model}`,
        name: item.model,
        cost: item.cost,
        requests: 0,
        tokens: item.tokens,
        percent: item.percent,
      }))
    : mode === 'users'
      ? (byUser?.items ?? []).map((item) => ({
          key: `user-${item.user_id}-${item.username}`,
          name: item.username || `#${item.user_id}`,
          cost: item.cost,
          requests: item.requests,
          tokens: item.tokens,
          percent: item.percent,
        }))
      : (byGroup?.items ?? []).map((item) => ({
          key: `group-${item.group}`,
          name: item.group,
          cost: item.cost,
          requests: item.requests,
          tokens: item.tokens,
          percent: item.percent,
        }))

  const title = isModel
    ? isAdmin
      ? t('common.dashboard.overview.composition.by_model')
      : t('common.dashboard.overview.composition.my_models')
    : mode === 'users'
      ? t('common.dashboard.overview.composition.by_user')
      : t('common.dashboard.overview.composition.by_group')

  const subtitle = isModel
    ? t('common.dashboard.overview.composition.subtitle_model')
    : t('common.dashboard.overview.composition.subtitle_admin')

  return (
    <section
      aria-label={title}
      data-testid={isModel ? 'composition-models' : 'composition-users'}
      className="rounded-xl border border-border-hairline bg-surface-card p-4"
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 className="text-body font-semibold text-ink-1">{title}</h2>
          <p className="mt-0.5 text-badge text-ink-3">{subtitle}</p>
        </div>
        {isAdmin && !isModel ? (
          <div
            role="group"
            aria-label={t('common.dashboard.overview.composition.mode_label')}
            className="inline-flex overflow-hidden rounded-lg border border-border-default"
          >
            {(['users', 'groups'] as const).map((value) => (
              <button
                key={value}
                type="button"
                aria-pressed={mode === value}
                onClick={() => onModeChange(value)}
                className={[
                  'px-2.5 py-1 text-badge focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                  mode === value
                    ? 'bg-brand text-on-brand'
                    : 'bg-surface-card text-ink-2 hover:bg-surface-hover hover:text-ink-1',
                ].join(' ')}
              >
                {t(
                  value === 'users'
                    ? 'common.dashboard.overview.composition.users'
                    : 'common.dashboard.overview.composition.groups',
                )}
              </button>
            ))}
          </div>
        ) : null}
      </div>

      <div className="mt-3">
        {loading ? (
          <div className="h-32 animate-pulse rounded bg-surface-hover motion-reduce:animate-none" />
        ) : error ? (
          <div className="flex h-32 flex-col items-center justify-center gap-2 text-body text-ink-3">
            <p>{t('common.dashboard.overview.composition.error')}</p>
            <button type="button" onClick={onRetry} className="text-brand hover:underline">
              {t('common.action.retry')}
            </button>
          </div>
        ) : rows.length === 0 ? (
          <div className="flex h-32 items-center justify-center text-body text-ink-3">
            {t('common.dashboard.overview.composition.empty')}
          </div>
        ) : (
          <table className="w-full border-collapse text-badge">
            <thead>
              <tr className="border-b border-border-hairline text-badge text-ink-3">
                <th className="pb-1.5 text-left font-normal">
                  {isModel
                    ? t('common.dashboard.overview.composition.model')
                    : mode === 'users'
                      ? t('common.dashboard.overview.composition.user')
                      : t('common.dashboard.overview.composition.group')}
                </th>
                <th className="pb-1.5 text-right font-normal">
                  {t('common.dashboard.overview.composition.cost')}
                </th>
                <th className="hidden pb-1.5 text-right font-normal sm:table-cell">
                  {isModel
                    ? t('common.dashboard.overview.composition.tokens')
                    : t('common.dashboard.overview.composition.requests')}
                </th>
                <th className="pb-1.5 text-right font-normal">
                  {t('common.dashboard.overview.composition.percent')}
                </th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => {
                const selectable = isModel && row.name !== 'other'
                const active = selectable && selectedModel === row.name
                return (
                  <tr
                    key={row.key}
                    className={[
                      'border-b border-border-hairline last:border-b-0',
                      selectable ? 'cursor-pointer hover:bg-surface-hover' : '',
                      active ? 'bg-brand-subtle' : '',
                    ].join(' ')}
                    onClick={() => {
                      if (selectable) onSelectModel(active ? undefined : row.name)
                    }}
                  >
                    <td className="max-w-[160px] truncate py-2 pr-3 font-mono text-badge text-ink-1">
                      {row.name}
                    </td>
                    <td className="py-2 text-right font-mono text-badge tabular-nums text-ink-2">
                      {error ? UNAVAILABLE : credits(row.cost)}
                    </td>
                    <td className="hidden py-2 text-right font-mono text-badge tabular-nums text-ink-2 sm:table-cell">
                      {isModel
                        ? formatTokensValue(row.tokens, locale)
                        : formatInteger(row.requests, locale)}
                    </td>
                    <td className="w-[90px] py-2 text-right">
                      <div className="flex items-center justify-end gap-2">
                        <span className="h-1 w-10 overflow-hidden rounded-full bg-surface-subtle">
                          <span
                            className="block h-full rounded-full bg-brand"
                            style={{ width: `${Math.max(0, Math.min(100, row.percent))}%` }}
                          />
                        </span>
                        <span className="w-10 font-mono text-badge tabular-nums text-ink-2">
                          {row.percent.toFixed(1)}%
                        </span>
                      </div>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </div>
    </section>
  )
}
