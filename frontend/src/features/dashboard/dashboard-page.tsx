import { useState } from 'react'

import { Button } from '@/shared/ui/button'
import { TextField } from '@/shared/ui/form'
import { useI18n } from '@/shared/lib/i18n-context'

import type { UsageRange } from './api'
import { ConsumptionTrend } from './consumption-trend'
import {
  formatCreditsValue,
  formatInteger,
  formatRpm,
  formatSuccessRate,
  formatTpm,
  formatTokensValue,
} from './format'
import { ModelDistributionDonut } from './model-distribution-donut'
import { RangeSelector } from './range-selector'
import { StatCard } from './stat-card'
import { useDashboardCapabilities } from './use-dashboard-capabilities'
import { useDashboardStats } from './use-dashboard-stats'

/**
 * `/dashboard` overview (modules-05 F-05-03 + F-05-04, R-M6-12).
 *
 * One range state feeds every query (cards, ring, trend, rates) so the three
 * surfaces can never disagree about their window. The admin username filter is
 * rendered only after `logs:read` is confirmed, and the applied value — not
 * the draft input — is what enters the query keys; ordinary users therefore
 * never issue an admin-only parameter.
 */
export function DashboardPage() {
  const { t, locale } = useI18n()
  const [range, setRange] = useState<UsageRange>('24h')
  const [usernameDraft, setUsernameDraft] = useState('')
  const [username, setUsername] = useState<string | undefined>(undefined)
  const [selectedModel, setSelectedModel] = useState<string | undefined>(undefined)

  const capabilities = useDashboardCapabilities()
  const { summary, byModel, timeseries, rates, granularity } = useDashboardStats({
    range,
    username,
    model: selectedModel,
  })

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
    <section className="mt-6" aria-labelledby="dashboard-title">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <h2 id="dashboard-title" className="text-lg font-semibold text-ink-1">
          {t('common.dashboard.title')}
        </h2>

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
      </div>

      {username !== undefined &&
      summaryData !== undefined &&
      summaryData.request_count === 0 ? (
        <p data-testid="dashboard-user-empty" className="mt-3 text-sm text-ink-2">
          {t('common.dashboard.filter.no_usage', { username })}
        </p>
      ) : null}

      {/* Rates strip (F-05-04). aria-live announces settled values once the
          concurrent requests resolve, without moving focus. */}
      <div
        aria-live="polite"
        className="mt-4 grid grid-cols-1 gap-4 sm:grid-cols-3"
      >
        <StatCard
          testId="rate-usage"
          label={t('common.dashboard.rates.usage')}
          unit={t('common.dashboard.rates.credits')}
          value={
            ratesData === undefined
              ? ''
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

      {/* Overview cards (F-05-03). */}
      <div
        aria-live="polite"
        className="mt-4 grid grid-cols-2 gap-4 lg:grid-cols-4"
      >
        <StatCard
          testId="card-total-spent"
          label={t('common.dashboard.cards.total_spent')}
          unit={t('common.dashboard.rates.credits')}
          value={
            summaryData === undefined
              ? ''
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
          subline={t('common.dashboard.cards.success_rate', {
            rate:
              summaryData === undefined
                ? '--'
                : formatSuccessRate(summaryData.success_rate, summaryData.request_count),
          })}
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

      <div className="mt-4 grid gap-4 lg:grid-cols-3">
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
      </div>
    </section>
  )
}
