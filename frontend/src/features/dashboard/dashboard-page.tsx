import { useState } from 'react'

import { PageHeader } from '@/features/shell/page-header'
import { usePublicStatus } from '@/lib/public-status'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { TextField } from '@/shared/ui/form'

import type { UsageRange } from './api'
import { CheckinCard } from './checkin-card'
import { ConsumptionTrend } from './consumption-trend'
import {
  formatCreditsValue,
  formatCurrencyValue,
  formatInteger,
  formatRpm,
  formatSuccessRate,
  formatTpm,
  formatTokensValue,
} from './format'
import { ModelDistributionDonut } from './model-distribution-donut'
import { ModelPerformance } from './model-performance'
import { QuickActions } from './quick-actions'
import { RangeSelector } from './range-selector'
import { StatCard } from './stat-card'
import { useDashboardCapabilities } from './use-dashboard-capabilities'
import { useDashboardStats } from './use-dashboard-stats'

/**
 * `/dashboard` overview (modules-05 F-05-03 + F-05-04, R-M6-12, spec §7).
 *
 * One range state feeds every query (cards, ring, trend, rates) so the three
 * surfaces can never disagree about their window. The admin username filter is
 * rendered only after `logs:read` is confirmed, and the applied value — not
 * the draft input — is what enters the query keys; ordinary users therefore
 * never issue an admin-only parameter.
 *
 * The page is a 12-column-style stack: a shared page header, the KPI grid, the
 * rates strip, trend + distribution, quick actions + check-in, and the
 * performance board full width. Every block owns `mt-4` so only the first one
 * needs the page header to sit above it.
 */
export function DashboardPage() {
  const { t, locale } = useI18n()
  const [range, setRange] = useState<UsageRange>('24h')
  const [usernameDraft, setUsernameDraft] = useState('')
  const [username, setUsername] = useState<string | undefined>(undefined)
  const [selectedModel, setSelectedModel] = useState<string | undefined>(undefined)

  const capabilities = useDashboardCapabilities()
  const status = usePublicStatus()
  const { summary, byModel, timeseries, rates, granularity } = useDashboardStats({
    range,
    username,
    model: selectedModel,
  })

  // `billing.display_unit` is a site-wide presentation choice already honoured
  // by the public pricing page; the dashboard must not print a different unit.
  // Missing status (older backend, fetch failure) keeps the credit defaults.
  const displayUnit =
    status.data?.billing?.display_unit === 'cny' ? 'cny' : 'credits'
  const quotaPerUnit = status.data?.billing?.quota_per_unit ?? 100
  const currencySymbol = status.data?.billing?.currency_symbol ?? '¥'
  const showCurrency = displayUnit === 'cny'

  const summaryData = summary.data
  const ratesData = rates.data

  const spendTooltip =
    summaryData === undefined
      ? undefined
      : t('common.dashboard.cards.spend_tooltip', {
          pre: formatCreditsValue(summaryData.pre_deducted_total, locale),
          refunded: formatCreditsValue(summaryData.refunded_total, locale),
        })

  const ratesSpendTooltip =
    ratesData === undefined
      ? undefined
      : t('common.dashboard.cards.spend_tooltip', {
          pre: formatCreditsValue(ratesData.pre_deducted_total, locale),
          refunded: formatCreditsValue(ratesData.refunded_total, locale),
        })

  return (
    <section className="mt-6" aria-label={t('common.dashboard.title')}>
      <PageHeader
        crumbs={[
          { label: t('common.console.label') },
          { label: t('common.nav.group.workspace') },
          { label: t('common.nav.dashboard') },
        ]}
        title={t('common.dashboard.title')}
        description={t('common.dashboard.page.description')}
        actions={
          <div className="flex flex-wrap items-end gap-3">
            {capabilities.canFilterByUsername ? (
              <form
                className="flex items-end gap-2"
                onSubmit={(event) => {
                  event.preventDefault()
                  const applied = usernameDraft.trim()
                  setUsername(applied === '' ? undefined : applied)
                }}
              >
                <TextField
                  id="dashboard-username"
                  label={t('common.dashboard.filter.username')}
                  placeholder={t('common.dashboard.filter.username_placeholder')}
                  value={usernameDraft}
                  onChange={(event) => setUsernameDraft(event.target.value)}
                  className="h-9 w-44"
                />
                <Button type="submit" variant="secondary" className="h-9">
                  {t('common.dashboard.filter.apply')}
                </Button>
                {username !== undefined ? (
                  <Button
                    type="button"
                    variant="ghost"
                    className="h-9"
                    onClick={() => {
                      setUsername(undefined)
                      setUsernameDraft('')
                    }}
                  >
                    {t('common.dashboard.filter.clear')}
                  </Button>
                ) : null}
              </form>
            ) : null}

            <RangeSelector value={range} onChange={setRange} />
          </div>
        }
      />

      {username !== undefined &&
      summaryData !== undefined &&
      summaryData.request_count === 0 ? (
        <p data-testid="dashboard-user-empty" className="mt-3 text-sm text-ink-2">
          {t('common.dashboard.filter.no_usage', { username })}
        </p>
      ) : null}

      {/* Overview cards (F-05-03). aria-live announces settled values once the
          concurrent requests resolve, without moving focus. */}
      <div
        aria-live="polite"
        className="mt-4 grid grid-cols-2 gap-4 lg:grid-cols-4"
      >
        <StatCard
          testId="card-total-spent"
          label={t('common.dashboard.cards.total_spent')}
          unit={
            showCurrency ? undefined : t('common.dashboard.rates.credits')
          }
          value={
            summaryData === undefined
              ? ''
              : showCurrency
                ? formatCurrencyValue(
                    summaryData.cost_total,
                    quotaPerUnit,
                    currencySymbol,
                    locale,
                  )
                : formatCreditsValue(summaryData.cost_total, locale)
          }
          tooltip={spendTooltip}
          loading={summary.isPending}
          error={summary.isError}
          onRetry={() => void summary.refetch()}
          retryLabel={t('common.action.retry')}
        />
        <StatCard
          testId="card-requests"
          label={t('common.dashboard.cards.requests')}
          value={
            summaryData === undefined
              ? ''
              : formatInteger(summaryData.request_count, locale)
          }
          subline={
            summaryData !== undefined && summaryData.request_count <= 0
              ? t('common.dashboard.cards.no_data')
              : t('common.dashboard.cards.success_rate', {
                  rate:
                    summaryData === undefined
                      ? '--'
                      : formatSuccessRate(
                          summaryData.success_rate,
                          summaryData.request_count,
                        ),
                })
          }
          loading={summary.isPending}
          error={summary.isError}
          onRetry={() => void summary.refetch()}
          retryLabel={t('common.action.retry')}
        />
        <StatCard
          testId="card-input-tokens"
          label={t('common.dashboard.cards.input_tokens')}
          value={
            summaryData === undefined
              ? ''
              : formatTokensValue(summaryData.prompt_tokens_total, locale)
          }
          loading={summary.isPending}
          error={summary.isError}
          onRetry={() => void summary.refetch()}
          retryLabel={t('common.action.retry')}
        />
        <StatCard
          testId="card-output-tokens"
          label={t('common.dashboard.cards.output_tokens')}
          value={
            summaryData === undefined
              ? ''
              : formatTokensValue(summaryData.completion_tokens_total, locale)
          }
          loading={summary.isPending}
          error={summary.isError}
          onRetry={() => void summary.refetch()}
          retryLabel={t('common.action.retry')}
        />
      </div>

      {/* Rates strip (F-05-04). */}
      <div
        aria-live="polite"
        className="mt-4 grid grid-cols-1 gap-4 sm:grid-cols-3"
      >
        <StatCard
          testId="rate-usage"
          // "Credits used" would contradict a ¥ value; in currency mode the
          // card is a plain spend figure.
          label={
            showCurrency
              ? t('common.dashboard.rates.spend')
              : t('common.dashboard.rates.usage')
          }
          unit={
            showCurrency ? undefined : t('common.dashboard.rates.credits')
          }
          value={
            ratesData === undefined
              ? ''
              : showCurrency
                ? formatCurrencyValue(
                    ratesData.cost_total,
                    quotaPerUnit,
                    currencySymbol,
                    locale,
                  )
                : formatCreditsValue(ratesData.cost_total, locale)
          }
          tooltip={ratesSpendTooltip}
          loading={rates.isPending}
          error={rates.isError}
          onRetry={() => void rates.refetch()}
          retryLabel={t('common.action.retry')}
        />
        <StatCard
          testId="rate-rpm"
          label={t('common.dashboard.rates.rpm')}
          unit={t('common.dashboard.rates.req_per_min')}
          value={ratesData === undefined ? '' : formatRpm(ratesData.rpm_avg, locale)}
          tooltip={
            ratesData === undefined
              ? undefined
              : ratesData.rpm_peak > 0
                ? t('common.dashboard.rates.peak', {
                    value: formatInteger(ratesData.rpm_peak, locale),
                  })
                : t('common.dashboard.rates.peak_none')
          }
          loading={rates.isPending}
          error={rates.isError}
          onRetry={() => void rates.refetch()}
          retryLabel={t('common.action.retry')}
        />
        <StatCard
          testId="rate-tpm"
          label={t('common.dashboard.rates.tpm')}
          unit={t('common.dashboard.rates.tokens_per_min')}
          value={ratesData === undefined ? '' : formatTpm(ratesData.tpm_avg, locale)}
          tooltip={
            ratesData === undefined
              ? undefined
              : t('common.dashboard.rates.tpm_exact', {
                  value: formatInteger(ratesData.tpm_avg, locale),
                })
          }
          loading={rates.isPending}
          error={rates.isError}
          onRetry={() => void rates.refetch()}
          retryLabel={t('common.action.retry')}
        />
      </div>

      {/* Trend on the left, model distribution on the right (spec §7). */}
      <div className="mt-4 grid gap-4 lg:grid-cols-3">
        <div className="lg:col-span-2">
          <ConsumptionTrend
            items={timeseries.data ?? []}
            range={range}
            granularity={granularity}
            loading={timeseries.isPending}
            error={timeseries.isError}
            onRetry={() => void timeseries.refetch()}
            selectedModel={selectedModel}
            onClearModel={() => setSelectedModel(undefined)}
          />
        </div>
        <div className="lg:col-span-1">
          <ModelDistributionDonut
            items={byModel.data ?? []}
            loading={byModel.isPending}
            error={byModel.isError}
            onRetry={() => void byModel.refetch()}
            selectedModel={selectedModel}
            onSelectModel={setSelectedModel}
          />
        </div>
      </div>

      {/* Quick actions + daily check-in (modules-01 §3.19, spec §7). The
          check-in card renders itself only when the public status flag is on,
          so disabled sites see just the quick actions. */}
      <div className="mt-4 grid gap-4 lg:grid-cols-2">
        <QuickActions />
        <CheckinCard className="" />
      </div>

      {/* F-05-08 模型性能看板。看板不跟随概览的 24h/7d/14d/30d 选择，因为
          采集管道只承诺 24h/7d 两个窗口；两者混用会让「口径偏差」提示失真。 */}
      <ModelPerformance
        canViewDimension={capabilities.canFilterByUsername}
      />
    </section>
  )
}
