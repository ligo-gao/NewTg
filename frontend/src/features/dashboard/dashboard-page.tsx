import { useState } from 'react'

import type { components } from '@/api/schema'
import { CheckinCard } from '@/features/dashboard/checkin-card'
import { PageHeader } from '@/features/shell/page-header'
import { usePublicStatus } from '@/lib/public-status'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'

import { AttentionPanel, type AttentionItem } from './attention-panel'
import type { UsageRange } from './api'
import { Composition, type CompositionMode } from './composition'
import { CoreMetrics } from './core-metrics'
import { DashboardTrend, type TrendMetric } from './dashboard-trend'
import { FailuresTable } from './failures-table'
import { HealthPanel } from './health-panel'
import { QuickActions } from './quick-actions'
import { RangeSelector } from './range-selector'
import { useDashboardExtra, useDashboardPermissions } from './use-dashboard-extra'
import { useDashboardStats } from './use-dashboard-stats'
import { formatCurrencyValue } from './format'

/**
 * `/dashboard` — 角色化运营总览。
 *
 * 管理员（admin + logs:read）先看消费、请求、成功率、P95 延迟，再看
 * 按模型/按用户构成、待办、系统健康和最近失败请求；其他用户看余额、
 * 个人用量、签到和最近请求。所有数据都来自现有聚合端点，权限不足的
 * 面板直接隐藏，不让整页因为一个 403 失败。
 */
export function DashboardPage() {
  const { t, locale } = useI18n()
  const [range, setRange] = useState<UsageRange>('24h')
  const [metric, setMetric] = useState<TrendMetric>('cost')
  const [compositionMode, setCompositionMode] = useState<CompositionMode>('users')
  const [selectedModel, setSelectedModel] = useState<string | undefined>(undefined)

  const permissions = useDashboardPermissions()
  const stats = useDashboardStats({ range, model: selectedModel })
  const extra = useDashboardExtra({ range, permissions })

  const status = usePublicStatus()
  const showCurrency = status.data?.billing?.display_unit === 'cny'
  const quotaPerUnit = status.data?.billing?.quota_per_unit ?? 100
  const currencySymbol = status.data?.billing?.currency_symbol ?? '¥'

  const adminView = permissions.canLogsRead
  const summary = stats.summary.data
  const rates = stats.rates.data

  const unpriced = (extra.candidates.data?.items ?? []).filter((item) => !item.priced)
  const perfUnavailable =
    extra.perf.isError || extra.perf.data?.pipeline?.enabled === false
  const attention: AttentionItem[] = []
  if (unpriced.length > 0) {
    attention.push({
      id: 'unpriced',
      severity: 'warn',
      title: t('common.dashboard.overview.attention.unpriced', {
        count: unpriced.length,
      }),
      detail: t('common.dashboard.overview.attention.unpriced_detail', {
        models: unpriced
          .slice(0, 3)
          .map((item) => item.model)
          .join('、'),
      }),
      href: '/admin/pricing/model-pricing',
      action: t('common.dashboard.overview.attention.go_pricing'),
    })
  }
  if (adminView && perfUnavailable) {
    attention.push({
      id: 'perf',
      severity: 'info',
      title: t('common.dashboard.overview.attention.perf'),
      detail: t('common.dashboard.overview.attention.perf_detail'),
      href: '/admin/system/performance',
      action: t('common.dashboard.overview.attention.go_perf'),
    })
  }

  const composition =
    compositionMode === 'users'
      ? { data: extra.byUser, label: 'users' }
      : { data: extra.byGroup, label: 'groups' }

  const wallet = extra.wallet.data as components['schemas']['WalletSummary'] | undefined

  function refresh() {
    void stats.summary.refetch()
    void stats.rates.refetch()
    void stats.timeseries.refetch()
    void stats.byModel.refetch()
    if (adminView) {
      void extra.byUser.refetch()
      void extra.byGroup.refetch()
      void extra.failures.refetch()
    } else {
      void extra.recent.refetch()
      void extra.wallet.refetch()
    }
  }

  return (
    <section className="mt-6" aria-label={t('common.dashboard.overview.title')}>
      <PageHeader
        crumbs={[
          { label: t('common.console.label') },
          { label: t('common.nav.group.workspace') },
          { label: t('common.nav.dashboard') },
        ]}
        title={
          adminView
            ? t('common.dashboard.overview.admin_title')
            : t('common.dashboard.overview.user_title')
        }
        description={
          adminView
            ? t('common.dashboard.overview.admin_description')
            : t('common.dashboard.overview.user_description')
        }
        actions={
          <div className="flex flex-wrap items-center gap-2">
            <RangeSelector value={range} onChange={setRange} />
            <Button type="button" variant="secondary" className="h-9" onClick={refresh}>
              {t('common.dashboard.overview.refresh')}
            </Button>
          </div>
        }
      />

      <CoreMetrics
        role={adminView ? 'admin' : 'user'}
        summary={summary}
        rates={rates}
        wallet={wallet}
        timeseries={stats.timeseries.data}
        loading={stats.summary.isPending}
        error={stats.summary.isError}
        showCurrency={showCurrency}
        quotaPerUnit={quotaPerUnit}
        currencySymbol={currencySymbol}
        locale={locale}
        onRetry={() => void stats.summary.refetch()}
      />

      <div className="mt-4 grid grid-cols-1 gap-4 xl:grid-cols-12">
        <div className="xl:col-span-8">
          <DashboardTrend
            items={stats.timeseries.data ?? []}
            range={range}
            granularity={stats.granularity}
            metric={metric}
            onMetricChange={setMetric}
            loading={stats.timeseries.isPending}
            error={stats.timeseries.isError}
            onRetry={() => void stats.timeseries.refetch()}
            selectedModel={selectedModel}
            onClearModel={() => setSelectedModel(undefined)}
          />
        </div>

        <aside className="flex flex-col gap-4 xl:col-span-4" aria-label={t('common.dashboard.overview.side')}>
          {adminView ? (
            <>
              <AttentionPanel items={attention} />
              {permissions.canSettingsManage ? (
                <HealthPanel
                  system={extra.system.data}
                  perfEnabled={
                    extra.perf.isError
                      ? false
                      : extra.perf.data?.pipeline?.enabled
                  }
                  loading={extra.system.isPending}
                  error={extra.system.isError}
                />
              ) : null}
            </>
          ) : (
            <>
              <section className="rounded-xl border border-border-hairline bg-surface-card p-4">
                <div className="flex items-baseline justify-between gap-2">
                  <h2 className="text-body font-semibold text-ink-1">
                    {t('common.dashboard.overview.account.title')}
                  </h2>
                  <span className="text-badge text-ink-3">
                    {t('common.dashboard.overview.account.unit')}
                  </span>
                </div>
                <p className="mt-2 font-mono text-figure-md tabular-nums tracking-[-0.03em] text-ink-1">
                  {wallet === undefined
                    ? '--'
                    : formatCurrencyValue(
                        wallet.balance,
                        quotaPerUnit,
                        currencySymbol,
                        locale,
                      )}
                </p>
                <p className="text-badge text-ink-3">
                  {t('common.dashboard.overview.account.available')}
                  {wallet === undefined
                    ? ''
                    : ` · ${t('common.dashboard.overview.account.held', {
                        value: formatCurrencyValue(
                          wallet.held_credits,
                          quotaPerUnit,
                          currencySymbol,
                          locale,
                        ),
                      })}`}
                </p>
                <dl className="mt-3 grid grid-cols-2 gap-2 border-t border-border-hairline pt-2 text-badge">
                  <div>
                    <dt className="text-ink-3">
                      {t('common.dashboard.overview.account.consumed')}
                    </dt>
                    <dd className="font-mono tabular-nums text-ink-1">
                      {wallet === undefined
                        ? '--'
                        : formatCurrencyValue(
                            wallet.total_consumed,
                            quotaPerUnit,
                            currencySymbol,
                            locale,
                          )}
                    </dd>
                  </div>
                  <div>
                    <dt className="text-ink-3">
                      {t('common.dashboard.overview.account.threshold')}
                    </dt>
                    <dd className="font-mono tabular-nums text-ink-1">
                      {wallet === undefined
                        ? '--'
                        : formatCurrencyValue(
                            wallet.threshold,
                            quotaPerUnit,
                            currencySymbol,
                            locale,
                          )}
                    </dd>
                  </div>
                </dl>
                <a
                  href="/wallet"
                  className="mt-3 inline-flex h-8 items-center rounded-lg bg-brand px-3 text-badge font-medium text-on-brand hover:bg-brand-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                >
                  {t('common.dashboard.overview.account.topup')}
                </a>
              </section>
              <CheckinCard className="" />
            </>
          )}
        </aside>

        {adminView ? (
          <>
            <div className="xl:col-span-6">
              <Composition
                variant="audience"
                role="admin"
                mode={compositionMode}
                onModeChange={setCompositionMode}
                byUser={extra.byUser.data}
                byGroup={extra.byGroup.data}
                byModel={undefined}
                loading={composition.data.isPending}
                error={composition.data.isError}
                onRetry={() => void composition.data.refetch()}
                onSelectModel={setSelectedModel}
                showCurrency={showCurrency}
                quotaPerUnit={quotaPerUnit}
                currencySymbol={currencySymbol}
                locale={locale}
              />
            </div>
            <div className="xl:col-span-6">
              <Composition
                variant="model"
                role="admin"
                mode="users"
                onModeChange={setCompositionMode}
                byUser={undefined}
                byGroup={undefined}
                byModel={stats.byModel.data}
                loading={stats.byModel.isPending}
                error={stats.byModel.isError}
                onRetry={() => void stats.byModel.refetch()}
                selectedModel={selectedModel}
                onSelectModel={setSelectedModel}
                showCurrency={showCurrency}
                quotaPerUnit={quotaPerUnit}
                currencySymbol={currencySymbol}
                locale={locale}
              />
            </div>
          </>
        ) : (
          <div className="xl:col-span-12">
            <Composition
              variant="model"
              role="user"
              mode="users"
              onModeChange={setCompositionMode}
              byUser={undefined}
              byGroup={undefined}
              byModel={stats.byModel.data}
              loading={stats.byModel.isPending}
              error={stats.byModel.isError}
              onRetry={() => void stats.byModel.refetch()}
              selectedModel={selectedModel}
              onSelectModel={setSelectedModel}
              showCurrency={showCurrency}
              quotaPerUnit={quotaPerUnit}
              currencySymbol={currencySymbol}
              locale={locale}
            />
          </div>
        )}

        <div className="xl:col-span-12">
          <FailuresTable
            title={
              adminView
                ? t('common.dashboard.overview.failures.title')
                : t('common.dashboard.overview.recent.title')
            }
            subtitle={
              adminView
                ? t('common.dashboard.overview.failures.subtitle')
                : t('common.dashboard.overview.recent.subtitle')
            }
            items={
              adminView
                ? (extra.failures.data?.items ?? [])
                : (extra.recent.data?.items ?? [])
            }
            loading={
              adminView ? extra.failures.isPending : extra.recent.isPending
            }
            error={adminView ? extra.failures.isError : extra.recent.isError}
            onRetry={() => {
              if (adminView) void extra.failures.refetch()
              else void extra.recent.refetch()
            }}
            showUser={adminView}
            emptyLabel={
              adminView
                ? t('common.dashboard.overview.failures.empty')
                : t('common.dashboard.overview.recent.empty')
            }
            testId={adminView ? 'failures-table' : 'recent-table'}
            showCurrency={showCurrency}
            quotaPerUnit={quotaPerUnit}
            currencySymbol={currencySymbol}
            locale={locale}
          />
        </div>

        {!adminView ? (
          <div className="xl:col-span-12">
            <QuickActions />
          </div>
        ) : null}
      </div>
    </section>
  )
}
