/**
 * 定价公示页（F-09-14 ~ F-09-22）。
 *
 * URL 是唯一状态源：路由用 `validatePricingSearch` 降级并回写，本组件只
 * 负责把用户操作转换成一个新的 `PricingSearch`。这样浏览器后退/前进、
 * 刷新和分享天然一致，也不需要维护第二份筛选状态。
 *
 * 数据只走一个查询：facets 与条目同响应返回（F-09-15），翻页用
 * `keepPreviousData` 保留上一页内容。
 */

import { keepPreviousData, useQuery } from '@tanstack/react-query'
import { useEffect, useMemo, useState } from 'react'

import { ApiError } from '@/api/errors'
import { usePublicStatus } from '@/lib/public-status'
import { ME_QUERY_KEY, meQueryOptions } from '@/lib/session'
import { useI18n } from '@/shared/lib/i18n-context'
import { Breadcrumb } from '@/features/public/breadcrumb'

import { pricingListQueryOptions } from './api'
import { PricingFacetPanel, PricingFilterSheet } from './facet-panel'
import { formatTotal } from './format'
import { capabilityLabel, contextBucketLabel, endpointLabel } from './metadata'
import { ModelCard } from './model-card'
import {
  activeFilterCount,
  capabilityList,
  clearedFilters,
  contextList,
  endpointTypeList,
  queryParamsOf,
  retainFacetValues,
  vendorList,
  withCapabilities,
  withContexts,
  withEndpointTypes,
  withVendors,
  CONTEXT_BUCKETS,
  ENDPOINT_TYPES,
  type PricingSearch,
  type PricingView,
} from './params'
import { Pagination } from './pagination'
import { PricingTable } from './pricing-table'
import { PricingToolbar } from './pricing-toolbar'
import { EmptyState, InlineError, PricingSkeleton } from './states'

export interface PricingPageProps {
  search: PricingSearch
  onSearchChange: (
    next: PricingSearch,
    options?: { replace?: boolean },
  ) => void
}

/** `<768` 强制卡片视图（F-09-23）；没有 matchMedia 的环境按桌面处理。 */
function useMinWidth(px: number): boolean {
  const [matches, setMatches] = useState(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
      return true
    }
    return window.matchMedia(`(min-width: ${px}px)`).matches
  })
  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
      return
    }
    const query = window.matchMedia(`(min-width: ${px}px)`)
    const update = () => setMatches(query.matches)
    update()
    query.addEventListener('change', update)
    return () => query.removeEventListener('change', update)
  }, [px])
  return matches
}

function errorMessage(error: unknown, t: (key: string) => string): string {
  if (error instanceof ApiError && error.kind === 'offline') {
    return t('public.pricing.offline')
  }
  if (error instanceof ApiError && error.kind === 'rate_limited') {
    return t('public.pricing.rate_limited')
  }
  if (error instanceof ApiError && error.detail !== undefined) return error.detail
  return t('public.pricing.error')
}

export function PricingPage({ search, onSearchChange }: PricingPageProps) {
  const { t, locale } = useI18n()
  const [sheetOpen, setSheetOpen] = useState(false)
  const [bannerDismissed, setBannerDismissed] = useState(false)
  const wideEnoughForTable = useMinWidth(768)

  const status = usePublicStatus()
  const me = useQuery({ ...meQueryOptions(), queryKey: ME_QUERY_KEY })

  /**
   * `billing.display_unit=cny` publishes prices in CNY as the primary unit, so
   * the list must always show the currency estimate. This is a site-level
   * decision, not a personal display preference: the URL may say
   * `dualCurrency=false` (an older shared link), but it is never allowed to
   * turn the forced estimate off; the toolbar switch is disabled with an
   * explanation instead of silently accepting a value it cannot honour.
   */
  const dualCurrencyForced = status.data?.billing?.display_unit === 'cny'
  const dualCurrency = search.dualCurrency || dualCurrencyForced

  const params = queryParamsOf(search)
  const query = useQuery({
    ...pricingListQueryOptions(params),
    placeholderData: keepPreviousData,
    retry: false,
  })
  const data = query.data

  /** 任何筛选变化都把页码重置到 1（F-09-21 主流程 3）。 */
  const updateFilters = (patch: Partial<PricingSearch>) => {
    onSearchChange({ ...search, ...patch, p: 1 })
  }
  /** 展示开关不重置页码、不触发请求（F-09-19）。 */
  const updateDisplay = (patch: Partial<PricingSearch>) => {
    onSearchChange({ ...search, ...patch })
  }

  const meta = data?.meta
  const currencyAvailable =
    meta !== undefined &&
    Number.isFinite(meta.quota_per_unit) &&
    meta.quota_per_unit > 0 &&
    Number.isFinite(meta.usd_exchange_rate) &&
    meta.usd_exchange_rate > 0

  /**
   * 服务端会丢弃不存在的 facet 值并把越界页收敛到最后一页（R-PUB-04）。
   * 前端拿到响应后把同一结果写回 URL（`replace`，不新增历史），否则用户
   * 复制出去的链接会一直带着无效参数。
   */
  useEffect(() => {
    if (data === undefined) return
    // 翻页/改筛选进行中时 `data` 是 keepPreviousData 保留的上一份结果；
    // 用它去「修正」新 URL 会把刚翻到的页码打回上一页。等本次请求落定
    // 再比较（服务端已经在此刻把越界页收敛好了）。
    if (query.isFetching) return
    const next: Partial<PricingSearch> = {}
    // facet 只在本次筛选有结果时才能当作合法域（见 retainFacetValues 的
    // 注释）：零结果时 facet 缺失也可能只是「合法值 + 其它筛选无交集」。
    const hasResults = data.items.length > 0
    const knownVendors = new Set(data.facets.vendors.map((entry) => entry.value))
    const current = vendorList(search)
    const valid = retainFacetValues(current, knownVendors, hasResults)
    if (valid.length !== current.length) next.vendor = withVendors(valid)

    // capability 的合法域来自目录数据，只能用返回的 facet 候选集判断：
    // 不在候选集里的值要么拼错、要么与其它筛选叠加后已无候选。零结果时
    // 后者无法排除（例如 vendor 没有该能力），因此保留 URL 值走空态。
    const knownCapabilities = new Set(
      data.facets.capabilities.map((entry) => entry.value),
    )
    const capabilities = capabilityList(search)
    const validCapabilities = retainFacetValues(
      capabilities,
      knownCapabilities,
      hasResults,
    )
    if (validCapabilities.length !== capabilities.length) {
      next.capability = withCapabilities(validCapabilities)
    }

    // context / endpointType 是**冻结枚举**：当前目录没有命中时后端仍返回
    // 空集并保留筛选语义，facet 却只列出有命中的值。因此这里用常量白名单，
    // 而不是 facet 候选集，只剔除真正未知的拼写。
    const contexts = contextList(search)
    const validContexts = contexts.filter((entry) =>
      (CONTEXT_BUCKETS as readonly string[]).includes(entry),
    )
    if (validContexts.length !== contexts.length) {
      next.context = withContexts(validContexts)
    }
    const endpointTypes = endpointTypeList(search)
    const validEndpointTypes = endpointTypes.filter((entry) =>
      (ENDPOINT_TYPES as readonly string[]).includes(entry),
    )
    if (validEndpointTypes.length !== endpointTypes.length) {
      next.endpointType = withEndpointTypes(validEndpointTypes)
    }

    if (
      search.group !== '' &&
      !data.facets.groups.some((group) => group.value === search.group)
    ) {
      next.group = ''
    }
    if (data.page !== search.p) next.p = data.page
    if (Object.keys(next).length > 0) {
      onSearchChange({ ...search, ...next }, { replace: true })
    }
    // 只在响应/URL 变化时运行；`search` 整体作为依赖会让每次渲染都进入。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [
    data,
    query.isFetching,
    search.vendor,
    search.capability,
    search.context,
    search.endpointType,
    search.group,
    search.p,
  ])

  const effectiveView: PricingView =
    search.view === 'table' && wideEnoughForTable ? 'table' : 'card'
  // 未显式选择分组时，服务端用会话用户分组计算 group_price；那就是「我的分组」。
  const ownGroupPrice = me.isSuccess && search.group === ''

  const vendorCount = useMemo(() => {
    if (data === undefined) return null
    if (data.facets.vendors.length > 0) return data.facets.vendors.length
    return new Set(data.items.map((card) => card.vendor).filter((v) => v !== '')).size
  }, [data])

  const totalLabel = data === undefined ? '—' : formatTotal(data.total, locale)
  const active = activeFilterCount(search)
  const hasResults = data !== undefined && data.items.length > 0
  const isFiltered =
    search.search !== '' ||
    search.vendor !== '' ||
    search.capability !== '' ||
    search.context !== '' ||
    search.endpointType !== '' ||
    search.group !== '' ||
    search.quotaType !== 'all'

  const removeFilter = (
    kind:
      | 'search'
      | 'vendor'
      | 'capability'
      | 'context'
      | 'endpointType'
      | 'group'
      | 'quotaType',
    value?: string,
  ) => {
    switch (kind) {
      case 'search':
        updateFilters({ search: '' })
        break
      case 'vendor':
        updateFilters({
          vendor: withVendors(vendorList(search).filter((vendor) => vendor !== value)),
        })
        break
      case 'capability':
        updateFilters({
          capability: withCapabilities(
            capabilityList(search).filter((entry) => entry !== value),
          ),
        })
        break
      case 'context':
        updateFilters({
          context: withContexts(
            contextList(search).filter((entry) => entry !== value),
          ),
        })
        break
      case 'endpointType':
        updateFilters({
          endpointType: withEndpointTypes(
            endpointTypeList(search).filter((entry) => entry !== value),
          ),
        })
        break
      case 'group':
        updateFilters({ group: '' })
        break
      case 'quotaType':
        updateFilters({ quotaType: 'all' })
        break
    }
  }

  return (
    <div className="flex flex-col gap-3">
      <Breadcrumb
        items={[
          { label: t('public.nav.home'), path: '/' },
          { label: t('public.pricing.title'), current: true },
        ]}
      />

      <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1">
        <h1 className="text-xl font-semibold leading-7 text-ink-1">
          {t('public.pricing.title')}
        </h1>
        <p aria-live="polite" className="text-[13px] leading-5 tabular-nums text-ink-2">
          {data === undefined || vendorCount === null
            ? '—'
            : t('public.pricing.header_stats', {
                total: totalLabel,
                vendors: vendorCount,
              })}
        </p>
      </div>

      {status.isError && status.data === undefined && !bannerDismissed ? (
        <div
          role="region"
          aria-label={t('public.pricing.config_banner_label')}
          className="flex items-center justify-between gap-3 rounded-lg border border-warning-line bg-warning-bg px-3 py-2 text-[13px] text-warning"
        >
          <span>{t('public.pricing.config_fallback')}</span>
          <button
            type="button"
            aria-label={t('public.pricing.dismiss')}
            onClick={() => setBannerDismissed(true)}
            className="rounded px-1 hover:bg-surface-card focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            ×
          </button>
        </div>
      ) : null}

      <div className="grid gap-6 xl:grid-cols-[264px_minmax(0,1fr)]">
        <aside className="hidden xl:block">
          <div className="sticky top-[72px] max-h-[calc(100vh-96px)] overflow-y-auto pr-2">
            {data === undefined ? (
              <FacetSkeleton />
            ) : (
              <PricingFacetPanel
                facets={data.facets}
                value={search}
                onChange={(next) => onSearchChange({ ...next, p: 1 })}
              />
            )}
          </div>
        </aside>

        <div className="flex min-w-0 flex-col gap-3">
          <PricingToolbar
            value={{ ...search, view: effectiveView, dualCurrency }}
            onChange={updateFilters}
            onDisplayChange={updateDisplay}
            onViewChange={(view) =>
              // 视图切换复用同一份数据与同一页（F-09-17：无额外请求）。
              // 表格的 20/50/100 密度只在用户随后显式改「每页」时生效。
              onSearchChange({ ...search, view })
            }
            currencyAvailable={currencyAvailable}
            dualCurrencyForced={dualCurrencyForced}
            activeFilters={active}
            onOpenFilters={() => setSheetOpen(true)}
          />

          {data === undefined || data.items.length === 0 ? null : data.facets.vendors.length === 0 &&
            data.facets.capabilities.length === 0 &&
            data.facets.context.length === 0 &&
            data.facets.groups.length === 0 &&
            data.facets.endpoints.length === 0 &&
            data.facets.pricing_types.length === 0 ? (
            <p className="text-[13px] leading-5 text-ink-3">
              {t('public.pricing.filters_unavailable')}
            </p>
          ) : null}

          {active > 0 ? (
            <div aria-label={t('public.pricing.active_filters')} className="flex flex-wrap items-center gap-1.5">
              {search.search !== '' ? (
                <FilterChip
                  label={t('public.pricing.filter_search', { value: search.search })}
                  removeLabel={t('public.pricing.remove_filter', {
                    label: t('public.pricing.filter_search', { value: search.search }),
                  })}
                  onRemove={() => removeFilter('search')}
                />
              ) : null}
              {vendorList(search).map((vendor) => (
                <FilterChip
                  key={vendor}
                  label={t('public.pricing.filter_vendor', { value: vendor })}
                  removeLabel={t('public.pricing.remove_filter', {
                    label: t('public.pricing.filter_vendor', { value: vendor }),
                  })}
                  onRemove={() => removeFilter('vendor', vendor)}
                />
              ))}
              {capabilityList(search).map((entry) => (
                <FilterChip
                  key={`capability-${entry}`}
                  label={t('public.pricing.filter_capability', {
                    value: capabilityLabel(entry, t),
                  })}
                  removeLabel={t('public.pricing.remove_filter', {
                    label: t('public.pricing.filter_capability', {
                      value: capabilityLabel(entry, t),
                    }),
                  })}
                  onRemove={() => removeFilter('capability', entry)}
                />
              ))}
              {contextList(search).map((entry) => (
                <FilterChip
                  key={`context-${entry}`}
                  label={t('public.pricing.filter_context', {
                    value: contextBucketLabel(entry, t),
                  })}
                  removeLabel={t('public.pricing.remove_filter', {
                    label: t('public.pricing.filter_context', {
                      value: contextBucketLabel(entry, t),
                    }),
                  })}
                  onRemove={() => removeFilter('context', entry)}
                />
              ))}
              {endpointTypeList(search).map((entry) => (
                <FilterChip
                  key={`endpoint-${entry}`}
                  label={t('public.pricing.filter_endpoint', {
                    value: endpointLabel(entry, t),
                  })}
                  removeLabel={t('public.pricing.remove_filter', {
                    label: t('public.pricing.filter_endpoint', {
                      value: endpointLabel(entry, t),
                    }),
                  })}
                  onRemove={() => removeFilter('endpointType', entry)}
                />
              ))}
              {search.group !== '' ? (
                <FilterChip
                  label={t('public.pricing.filter_group', { value: search.group })}
                  removeLabel={t('public.pricing.remove_filter', {
                    label: t('public.pricing.filter_group', { value: search.group }),
                  })}
                  onRemove={() => removeFilter('group')}
                />
              ) : null}
              {search.quotaType !== 'all' ? (
                <FilterChip
                  label={t('public.pricing.filter_quota', {
                    value:
                      search.quotaType === 'token'
                        ? t('public.pricing.type_token')
                        : t('public.pricing.type_request'),
                  })}
                  removeLabel={t('public.pricing.remove_filter', {
                    label: t('public.pricing.filter_quota', {
                      value:
                        search.quotaType === 'token'
                          ? t('public.pricing.type_token')
                          : t('public.pricing.type_request'),
                    }),
                  })}
                  onRemove={() => removeFilter('quotaType')}
                />
              ) : null}
              <button
                type="button"
                onClick={() => onSearchChange(clearedFilters(search))}
                className="rounded px-1.5 text-[13px] text-brand hover:underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              >
                {t('public.pricing.reset_all')}
              </button>
            </div>
          ) : null}

          {query.isError && data !== undefined ? (
            <InlineError
              message={errorMessage(query.error, t)}
              onRetry={() => void query.refetch()}
            />
          ) : null}

          <div aria-busy={query.isFetching}>
            {query.isError && data === undefined ? (
              <InlineError
                message={errorMessage(query.error, t)}
                onRetry={() => void query.refetch()}
              />
            ) : data === undefined ? (
              <PricingSkeleton view={effectiveView} />
            ) : data.items.length === 0 ? (
              <EmptyState
                filtered={isFiltered}
                onClearFilters={() => onSearchChange(clearedFilters(search))}
              />
            ) : effectiveView === 'table' ? (
              <PricingTable
                items={data.items}
                tokenUnit={search.tokenUnit}
                search={search.search}
                sort={search.sort}
              />
            ) : (
              <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
                {data.items.map((card) => (
                  <ModelCard
                    key={card.model_id}
                    card={card}
                    meta={meta}
                    tokenUnit={search.tokenUnit}
                    dualCurrency={dualCurrency}
                    rechargePrice={search.rechargePrice}
                    search={search.search}
                    isOwnGroup={card.group_price !== null && ownGroupPrice}
                  />
                ))}
              </div>
            )}
          </div>

          {hasResults ? (
            <Pagination
              page={search.p}
              pageSize={search.page_size}
              total={data === undefined ? '0' : data.total}
              view={effectiveView}
              pending={query.isFetching}
              onPageChange={(p) => onSearchChange({ ...search, p }, { replace: false })}
              onPageSizeChange={(page_size) =>
                updateFilters({ page_size })
              }
            />
          ) : null}
        </div>
      </div>

      <PricingFilterSheet
        open={sheetOpen}
        onOpenChange={setSheetOpen}
        facets={data?.facets}
        value={search}
        onApply={(next) => onSearchChange({ ...next, p: 1 })}
      />
    </div>
  )
}

function FilterChip({
  label,
  removeLabel,
  onRemove,
}: {
  label: string
  removeLabel: string
  onRemove: () => void
}) {
  return (
    <button
      type="button"
      aria-label={removeLabel}
      onClick={onRemove}
      className="inline-flex h-6 items-center gap-1 rounded-full bg-surface-subtle px-2 text-xs leading-5 text-ink-1 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
    >
      <span>{label}</span>
      <span aria-hidden="true" className="text-ink-3">×</span>
    </button>
  )
}

/** 侧栏 4 组骨架（F-09-14 六态：禁止整页 spinner）。 */
function FacetSkeleton() {
  return (
    <div aria-busy="true" className="flex flex-col gap-5">
      {[0, 1, 2, 3].map((group) => (
        <div key={group} className="flex flex-col gap-2">
          <div className="h-4 w-1/2 rounded bg-surface-subtle" />
          <div className="h-3 w-full rounded bg-surface-subtle" />
          <div className="h-3 w-5/6 rounded bg-surface-subtle" />
          <div className="h-3 w-2/3 rounded bg-surface-subtle" />
        </div>
      ))}
    </div>
  )
}
