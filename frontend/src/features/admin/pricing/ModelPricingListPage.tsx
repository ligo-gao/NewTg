import { useEffect, useMemo, useState } from 'react'
import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components, operations } from '@/api/schema'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { formatCredits } from '@/shared/lib/format'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { StatusBadge } from '@/shared/ui/status-badge'
import { Tag } from '@/shared/ui/tag'
import { TextField } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

import { ModelPricingMutateSheet } from './ModelPricingMutateSheet'
import { PricingPagination } from './PricingPagination'
import {
  DEFAULT_QUOTA_PER_UNIT,
  pricingModeKey,
  type PricingMode,
  type PricingStatus,
} from './pricing-utils'
import { usePricingCapabilities } from './use-pricing-capabilities'
import { fetchSettingsSection } from '../settings/api'

type ModelPricing = components['schemas']['ModelPricing']
type ListQuery = NonNullable<operations['listModelPricing']['parameters']['query']>
type SortOption = NonNullable<ListQuery['sort']>

interface Filters {
  keyword: string
  mode: '' | PricingMode
  status: '' | 'enabled' | 'disabled'
  sort: SortOption
}

const EMPTY_FILTERS: Filters = { keyword: '', mode: '', status: '', sort: 'model' }

interface EditTarget {
  mode: 'create' | 'edit'
  pricing?: ModelPricing
}

export interface ModelPricingListPageProps {
  /**
   * F-08-37 深链（任务插件 → 定价 → 查看定价）携带的插件 key。
   * 过滤只在前端做（R-50）：插件计价变体的唯一真源是
   * `billing_setting.plugin_billing_expr`（键 `<pluginKey>::<model>`），
   * 通过既有 GET /api/settings 读取后按模型名收敛列表；不新增后端端点，
   * 也不给 /api/model-pricing 增加参数。
   */
  plugin?: string
  /** 清除插件过滤（路由回写，去掉 ?plugin=）。 */
  onClearPlugin?: () => void
}

/** 插件过滤时最多向后翻的页数（100 行/页）：防止一个异常大的目录把
 * 浏览器拖死在串行请求上；超出时在页面明示只覆盖前 N 行。 */
const PLUGIN_FILTER_MAX_PAGES = 10

/** F-08-37 深链的插件计价表达式设置键（与后端 pricing.PluginBillingExprSettingKey 同值）。 */
const PLUGIN_BILLING_EXPR_KEY = 'billing_setting.plugin_billing_expr'

/**
 * F-06-13 模型定价列表。
 *
 * 服务端仅在该模型全部计费维度都为 0 时标记免费；单项 0 仍渲染数字 0
 * （99 §3.6 / TC-F-06-13-03），因此用服务端的 free 标志挂 Tag，而不是
 * 从单元格推断。
 *
 * 启停走 PATCH {status} 而不是 POST /{model}:enable 动作路由：模型名允许
 * 含 `:`，动作端点按最后一个冒号切分，PATCH 没有这个歧义。
 */
export function ModelPricingListPage({
  plugin,
  onClearPlugin,
}: ModelPricingListPageProps = {}) {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const capabilities = usePricingCapabilities()

  const [draft, setDraft] = useState<Filters>(EMPTY_FILTERS)
  const [applied, setApplied] = useState<Filters>(EMPTY_FILTERS)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [editTarget, setEditTarget] = useState<EditTarget | null>(null)
  const [disableTarget, setDisableTarget] = useState<ModelPricing | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<ModelPricing | null>(null)

  const pluginKey = plugin?.trim() ?? ''
  const pluginFiltered = pluginKey !== ''

  // 插件 → 模型集合来自设置面的 plugin_billing_expr（键 `<plugin>::<model>`）。
  // 读失败/无配置都不臆造数据：列表显示为空态并说明原因，而不是悄悄
  // 展示全部模型让管理员以为过滤生效。唯一的例外是 403（无 settings:manage），
  // 见下方 pluginUnavailable：那不是读取故障，而是功能不可用。
  const pluginSettings = useQuery({
    queryKey: ['settings', 'billing_setting'],
    enabled: pluginFiltered,
    staleTime: 30_000,
    queryFn: ({ signal }) => fetchSettingsSection('billing_setting', signal),
  })

  // R-55：GET /api/settings 在并发线合并后要求 `settings:manage`（R-26），
  // 而 /admin 路由守卫只判 role === 'admin'。因此没有该能力的 admin 仍能
  // 带着 F-08-37 的 ?plugin= 深链进来，读设置时拿到 403。这与「设置读取
  // 失败」语义不同：不是暂时性故障，而是插件过滤对本用户不可用。此时：
  // 隐藏筛选入口、不渲染错误提示，列表退回未过滤的普通分页，页面其余
  // 功能（增删改查）不受影响。其它读取失败仍走原有空态并说明原因。
  const pluginUnavailable =
    pluginFiltered &&
    pluginSettings.isError &&
    pluginSettings.error instanceof ApiError &&
    pluginSettings.error.kind === 'forbidden'
  // 过滤是否真正生效：路由上有 plugin 且过滤功能可用。
  const pluginActive = pluginFiltered && !pluginUnavailable

  const pluginModels = useMemo(() => {
    const out = new Set<string>()
    if (!pluginActive || pluginSettings.data === undefined) return out
    const item = pluginSettings.data.items.find(
      (entry) => entry.key === PLUGIN_BILLING_EXPR_KEY,
    )
    const map = item?.value
    if (typeof map !== 'object' || map === null || Array.isArray(map)) return out
    const prefix = `${pluginKey}::`
    for (const key of Object.keys(map)) {
      if (key.startsWith(prefix)) out.add(key.slice(prefix.length))
    }
    return out
  }, [pluginActive, pluginKey, pluginSettings.data])

  const listQuery = useQuery({
    queryKey: ['admin-model-pricing', applied, page, pageSize, pluginKey],
    // 能力探测完成前不发列表请求：无 pricing:read 时那是注定 403 的请求，
    // 也会避免「先闪 403 再出现表格」。插件过滤还要等设置读取完成，
    // 否则会用空集合把整页过滤掉。
    enabled:
      !capabilities.loading &&
      capabilities.canRead &&
      // 设置读取结束（成功或失败）后再发列表请求：读失败时也要让空态
      // 明确说「无法读取插件配置」，而不是让禁用中的查询永远显示骨架。
      (!pluginActive || !pluginSettings.isPending),
    placeholderData: keepPreviousData,
    queryFn: async () => {
      const base: ListQuery = {
        p: 1,
        page_size: pageSize,
        sort: applied.sort,
      }
      if (applied.keyword.trim() !== '') base.keyword = applied.keyword.trim()
      if (applied.mode !== '') base.mode = applied.mode
      if (applied.status !== '') base.status = applied.status

      if (!pluginActive) {
        const withPage = { ...base, p: page }
        return requireData(
          await apiClient.GET('/api/model-pricing', { params: { query: withPage } }),
        )
      }

      // 插件过滤是纯前端：服务端分页与本地过滤叠加会让「本页没有命中」
      // 伪装成「没有数据」，所以按 100 行/页向后拉全量再过滤，然后本地
      // 分页。上限 PLUGIN_FILTER_MAX_PAGES 保证异常目录不会无限递归。
      const all: ModelPricing[] = []
      let serverPage = 1
      let total = 0
      let quotaPerUnit = DEFAULT_QUOTA_PER_UNIT
      for (; serverPage <= PLUGIN_FILTER_MAX_PAGES; serverPage++) {
        const data = requireData(
          await apiClient.GET('/api/model-pricing', {
            params: { query: { ...base, p: serverPage, page_size: 100 } },
          }),
        )
        all.push(...data.items)
        total = data.total
        quotaPerUnit = data.quota_per_unit ?? DEFAULT_QUOTA_PER_UNIT
        if (data.items.length === 0 || all.length >= total) break
      }
      const filtered = all.filter((row) => pluginModels.has(row.model))
      return {
        items: filtered,
        total: filtered.length,
        page: 1,
        page_size: pageSize,
        quota_per_unit: quotaPerUnit,
      }
    },
  })

  const setStatus = useMutation({
    mutationFn: async ({
      row,
      status,
    }: {
      row: ModelPricing
      status: PricingStatus
    }) => {
      await apiClient.PATCH('/api/model-pricing/{model}', {
        params: { path: { model: row.model } },
        body: { updated_at: row.updated_at, status },
      })
    },
    onSuccess: async (_data, variables) => {
      push({
        tone: 'success',
        message:
          variables.status === 2
            ? t('admin.pricing.disabled_success')
            : t('admin.pricing.enabled_success'),
      })
      setDisableTarget(null)
      await queryClient.invalidateQueries({ queryKey: ['admin-model-pricing'] })
      // 启停会改变分组覆盖价展示的 source_status，也影响模型选择器。
      await queryClient.invalidateQueries({ queryKey: ['admin-pricing-models'] })
      await queryClient.invalidateQueries({ queryKey: ['admin-group-pricing'] })
    },
    onError: (error) => {
      const conflict = error instanceof ApiError && error.kind === 'conflict'
      const gone = error instanceof ApiError && error.kind === 'not_found'
      push({
        tone: 'error',
        message: conflict
          ? t('admin.pricing.conflict')
          : gone
            ? t('admin.pricing.gone')
            : t('admin.pricing.action_error'),
      })
      // 并发删除的 404 与乐观锁 409 一样必须刷新，避免幽灵行与重复 404。
      if (conflict || gone) {
        void queryClient.invalidateQueries({ queryKey: ['admin-model-pricing'] })
      }
    },
  })

  const remove = useMutation({
    mutationFn: async (row: ModelPricing) => {
      // 绝不能手工拼这个 URL：updated_at 常带 +08:00 偏移，查询串里的
      // 字面 `+` 会被解码成空格。把值交给 openapi-fetch 的查询序列化器
      // （encodeURIComponent）会把 `+` 转成 %2B；MSW 测试锁定了原始 URL。
      await apiClient.DELETE('/api/model-pricing/{model}', {
        params: {
          path: { model: row.model },
          query: { updated_at: row.updated_at },
        },
      })
    },
    onSuccess: async () => {
      push({ tone: 'success', message: t('admin.pricing.deleted_success') })
      setDeleteTarget(null)
      await queryClient.invalidateQueries({ queryKey: ['admin-model-pricing'] })
      // 删除模型定价会同事务级联删除分组覆盖价。
      await queryClient.invalidateQueries({ queryKey: ['admin-pricing-models'] })
      await queryClient.invalidateQueries({ queryKey: ['admin-group-pricing'] })
    },
    onError: (error) => {
      const conflict = error instanceof ApiError && error.kind === 'conflict'
      const gone = error instanceof ApiError && error.kind === 'not_found'
      push({
        tone: 'error',
        message: conflict
          ? t('admin.pricing.conflict')
          : gone
            ? t('admin.pricing.gone')
            : t('admin.pricing.action_error'),
      })
      // 并发删除的 404 与乐观锁 409 一样必须刷新，避免幽灵行与重复 404。
      if (conflict || gone) {
        void queryClient.invalidateQueries({ queryKey: ['admin-model-pricing'] })
      }
    },
  })

  const data = listQuery.data
  // 插件过滤返回的是「过滤后的全量」，分页在本地切；普通路径仍由服务端
  // offset 分页直接给出本页数据。
  const allFilteredItems = data?.items ?? []
  const items = pluginActive
    ? allFilteredItems.slice((page - 1) * pageSize, page * pageSize)
    : allFilteredItems
  const total = data?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / pageSize))
  const quotaPerUnit = data?.quota_per_unit ?? DEFAULT_QUOTA_PER_UNIT

  // 深链切换插件时回到第一页；否则换了插件还停在上一个插件过滤后的页码，
  // 会显示越界空页。
  useEffect(() => {
    setPage(1)
  }, [pluginKey])

  // 删除后 offset 分页可能越过末尾；契约要求回退到最后一页
  // （web-00 §5.5）。total 归零时同样回第 1 页，否则会显示
  // 「Page N of 1」并停在越界页。
  useEffect(() => {
    if (!listQuery.isFetching && page > pages) {
      setPage(pages)
    }
  }, [listQuery.isFetching, page, pages])

  function applyFilters(): void {
    setApplied(draft)
    setPage(1)
  }

  function resetFilters(): void {
    setDraft(EMPTY_FILTERS)
    setApplied(EMPTY_FILTERS)
    setPage(1)
  }

  const hasFilters =
    pluginActive ||
    applied.keyword.trim() !== '' ||
    applied.mode !== '' ||
    applied.status !== '' ||
    applied.sort !== 'model'

  // 插件过滤的空态要说明「为什么空」：设置读取失败、该插件没有表达式、
  // 或表达式有模型但当前筛选无命中，三种原因是不同的操作方向。
  const emptyMessage = pluginActive
    ? pluginSettings.isError
      ? t('admin.pricing.plugin_filter_error')
      : pluginModels.size === 0
        ? t('admin.pricing.plugin_filter_empty')
        : t('admin.pricing.empty_filtered')
    : hasFilters
      ? t('admin.pricing.empty_filtered')
      : t('admin.pricing.empty')

  if (capabilities.loading) {
    return (
      <div className="mx-auto flex max-w-[1520px] flex-col gap-3">
        <h1 className="text-page-title font-semibold text-ink-1">{t('admin.pricing.title')}</h1>
        <section className="rounded-xl border border-border-hairline bg-surface-card">
          <div className="space-y-2 p-5" aria-busy="true">
            {[0, 1, 2, 3, 4].map((index) => (
              <div key={index} className="h-10 animate-pulse rounded bg-surface-subtle" />
            ))}
          </div>
        </section>
      </div>
    )
  }

  if (capabilities.known && !capabilities.canRead) {
    return (
      <div className="mx-auto max-w-[1520px] rounded-xl border border-border-hairline bg-surface-card p-8">
        <h1 className="text-page-title font-semibold text-ink-1">{t('common.error403.title')}</h1>
        <p className="mt-2 text-body text-ink-2">{t('common.error403.body')}</p>
      </div>
    )
  }

  return (
    <div className="mx-auto flex max-w-[1520px] flex-col gap-3">
      <h1 className="text-page-title font-semibold text-ink-1">{t('admin.pricing.title')}</h1>

      {pluginActive ? (
        <section className="flex flex-wrap items-center justify-between gap-2 rounded-xl border border-border-hairline bg-surface-subtle px-4 py-3">
          <div className="flex flex-col gap-1">
            <span className="text-body text-ink-1">
              {t('admin.pricing.plugin_filter_active', { plugin: pluginKey })}
            </span>
            <span className="text-meta text-ink-3">{t('admin.pricing.plugin_filter_hint')}</span>
          </div>
          <Button variant="secondary" className="h-8 px-3 text-meta" onClick={onClearPlugin}>
            {t('admin.pricing.plugin_filter_clear')}
          </Button>
        </section>
      ) : null}

      <section className="rounded-xl border border-border-hairline bg-surface-subtle p-4">
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
          <TextField
            label={t('admin.pricing.keyword')}
            value={draft.keyword}
            maxLength={128}
            onChange={(event) => {
              setDraft((current) => ({ ...current, keyword: event.target.value }))
            }}
          />
          <div className="flex flex-col gap-1.5">
            <label htmlFor="pricing-filter-mode" className="text-meta font-medium text-ink-1">
              {t('admin.pricing.mode')}
            </label>
            <select
              id="pricing-filter-mode"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              value={draft.mode}
              onChange={(event) => {
                setDraft((current) => ({
                  ...current,
                  mode: event.target.value as Filters['mode'],
                }))
              }}
            >
              <option value="">{t('admin.pricing.all')}</option>
              <option value="per_token">{t('admin.pricing.mode_per_token')}</option>
              <option value="per_request">{t('admin.pricing.mode_per_request')}</option>
              <option value="tiered_expr">{t('admin.pricing.mode_tiered_expr')}</option>
            </select>
          </div>
          <div className="flex flex-col gap-1.5">
            <label htmlFor="pricing-filter-status" className="text-meta font-medium text-ink-1">
              {t('admin.pricing.status')}
            </label>
            <select
              id="pricing-filter-status"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              value={draft.status}
              onChange={(event) => {
                setDraft((current) => ({
                  ...current,
                  status: event.target.value as Filters['status'],
                }))
              }}
            >
              <option value="">{t('admin.pricing.all')}</option>
              <option value="enabled">{t('admin.pricing.status_enabled')}</option>
              <option value="disabled">{t('admin.pricing.status_disabled')}</option>
            </select>
          </div>
          <div className="flex flex-col gap-1.5">
            <label htmlFor="pricing-filter-sort" className="text-meta font-medium text-ink-1">
              {t('admin.pricing.sort')}
            </label>
            <select
              id="pricing-filter-sort"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              value={draft.sort}
              onChange={(event) => {
                setDraft((current) => ({
                  ...current,
                  sort: event.target.value as SortOption,
                }))
              }}
            >
              <option value="model">{t('admin.pricing.sort_model')}</option>
              <option value="-model">{t('admin.pricing.sort_model_desc')}</option>
              <option value="prompt_price">{t('admin.pricing.sort_prompt_price')}</option>
              <option value="-prompt_price">{t('admin.pricing.sort_prompt_price_desc')}</option>
              <option value="updated_at">{t('admin.pricing.sort_updated_at')}</option>
              <option value="-updated_at">{t('admin.pricing.sort_updated_at_desc')}</option>
            </select>
          </div>
        </div>
        <div className="mt-3 flex justify-end gap-2">
          <Button variant="secondary" onClick={resetFilters}>
            {t('admin.pricing.reset')}
          </Button>
          <Button onClick={applyFilters}>{t('admin.pricing.search')}</Button>
        </div>
      </section>

      <section className="rounded-xl border border-border-hairline bg-surface-card">
        <div className="flex items-center justify-between border-b border-border-hairline px-5 py-3">
          {capabilities.canManage ? (
            <Button onClick={() => setEditTarget({ mode: 'create' })}>
              {t('admin.pricing.new')}
            </Button>
          ) : (
            <span />
          )}
          <span className="text-meta text-ink-2">
            {t('admin.pricing.total', { count: total })}
          </span>
        </div>

        {listQuery.isPending ? (
          <div className="space-y-2 p-5" aria-busy="true">
            {[0, 1, 2, 3, 4].map((index) => (
              <div key={index} className="h-10 animate-pulse rounded bg-surface-subtle" />
            ))}
          </div>
        ) : listQuery.isError ? (
          <div className="flex items-center gap-3 p-5 text-body text-ink-2">
            <span>
              {listQuery.error instanceof ApiError && listQuery.error.kind === 'forbidden'
                ? t('common.error403.body')
                : t('admin.pricing.error')}
            </span>
            <Button variant="secondary" onClick={() => void listQuery.refetch()}>
              {t('common.action.retry')}
            </Button>
          </div>
        ) : items.length === 0 ? (
          <div className="flex flex-col items-start gap-2 p-5">
            <p className="text-body text-ink-2">{emptyMessage}</p>
            {hasFilters ? (
              <Button
                variant="secondary"
                onClick={() => {
                  resetFilters()
                  // 插件过滤不是本地草稿，必须通过路由回写清除，否则
                  // 「清除筛选」之后列表仍然是空的。
                  if (pluginActive) onClearPlugin?.()
                }}
              >
                {t('admin.pricing.clear_filters')}
              </Button>
            ) : (
              <>
                <p className="text-meta text-ink-3">{t('admin.pricing.empty_hint')}</p>
                {capabilities.canManage ? (
                  <Button onClick={() => setEditTarget({ mode: 'create' })}>
                    {t('admin.pricing.new')}
                  </Button>
                ) : null}
              </>
            )}
          </div>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[1080px] text-body">
              <thead>
                <tr className="border-b border-border-hairline text-left text-meta text-ink-2">
                  <th scope="col" rowSpan={2} className="px-4 py-2 font-medium">
                    {t('admin.pricing.column_model')}
                  </th>
                  <th scope="col" rowSpan={2} className="px-4 py-2 font-medium">
                    {t('admin.pricing.column_mode')}
                  </th>
                  <th
                    scope="colgroup"
                    colSpan={4}
                    className="border-l border-border-hairline px-4 py-2 text-center font-medium"
                  >
                    {t('admin.pricing.price_group')}
                  </th>
                  <th scope="col" rowSpan={2} className="px-4 py-2 font-medium">
                    {t('admin.pricing.column_status')}
                  </th>
                  <th scope="col" rowSpan={2} className="px-4 py-2 font-medium">
                    {t('admin.pricing.column_updated')}
                  </th>
                  <th scope="col" rowSpan={2} className="px-4 py-2 text-right font-medium">
                    {t('admin.pricing.actions')}
                  </th>
                </tr>
                <tr className="border-b border-border-hairline text-left text-meta text-ink-2">
                  <th scope="col" className="border-l border-border-hairline px-4 py-2 text-right font-medium">
                    {t('admin.pricing.column_prompt')}
                  </th>
                  <th scope="col" className="px-4 py-2 text-right font-medium">
                    {t('admin.pricing.column_completion')}
                  </th>
                  <th scope="col" className="px-4 py-2 text-right font-medium">
                    {t('admin.pricing.column_cache_read')}
                  </th>
                  <th scope="col" className="px-4 py-2 text-right font-medium">
                    {t('admin.pricing.column_cache_write')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {items.map((row) => {
                  const perRequest = row.mode === 'per_request'
                  return (
                    <tr
                      key={row.model}
                      className="border-b border-border-hairline last:border-0 hover:bg-surface-hover"
                    >
                      <td className="px-4 py-3">
                        <span className="font-mono text-ink-1">{row.model}</span>{' '}
                        {row.strategy_kind === 'peak_offpeak' ? (
                          <Tag
                            variant="info"
                            ariaLabel={t('admin.pricing.strategy_kind_peak_offpeak')}
                          >
                            {t('admin.pricing.strategy_kind_peak_offpeak')}
                          </Tag>
                        ) : null}{' '}
                        {row.free ? (
                          <Tag variant="success" ariaLabel={t('admin.pricing.free')}>
                            {t('admin.pricing.free')}
                          </Tag>
                        ) : null}
                      </td>
                      <td className="px-4 py-3">
                        <Tag ariaLabel={t(pricingModeKey(row.mode))}>
                          {t(pricingModeKey(row.mode))}
                        </Tag>
                      </td>
                      <td className="border-l border-border-hairline px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                        {renderPrice(row.prompt_price)}
                      </td>
                      <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                        {perRequest ? '—' : renderPrice(row.completion_price)}
                      </td>
                      <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                        {perRequest ? '—' : renderPrice(row.cache_read_price)}
                      </td>
                      <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                        {perRequest ? '—' : renderPrice(row.cache_write_price)}
                      </td>
                      <td className="px-4 py-3">
                        <StatusBadge variant={row.status === 1 ? 'success' : 'neutral'}>
                          {row.status === 1
                            ? t('admin.pricing.status_enabled')
                            : t('admin.pricing.status_disabled')}
                        </StatusBadge>
                      </td>
                      <td className="px-4 py-3 text-meta text-ink-2" title={row.updated_at}>
                        {formatRelativeTime(row.updated_at, locale)}
                      </td>
                      <td className="px-4 py-3">
                        {capabilities.canManage ? (
                          <div className="flex justify-end gap-1">
                            <Button
                              variant="ghost"
                              className="h-8 px-2 text-meta"
                              onClick={() => setEditTarget({ mode: 'edit', pricing: row })}
                            >
                              {t('admin.pricing.edit')}
                            </Button>
                            <Button
                              variant="ghost"
                              className="h-8 px-2 text-meta"
                              disabled={setStatus.isPending}
                              onClick={() => {
                                // 停用会让用户无法请求该模型，因此需要确认；
                                // 启用是安全且即时的。
                                if (row.status === 1) {
                                  setDisableTarget(row)
                                  return
                                }
                                setStatus.mutate({ row, status: 1 })
                              }}
                            >
                              {row.status === 1
                                ? t('admin.pricing.disable')
                                : t('admin.pricing.enable')}
                            </Button>
                            <Button
                              variant="ghost"
                              className="h-8 px-2 text-meta text-danger hover:bg-danger-bg"
                              onClick={() => setDeleteTarget(row)}
                            >
                              {t('admin.pricing.delete')}
                            </Button>
                          </div>
                        ) : null}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}

        <PricingPagination
          page={page}
          pages={pages}
          pageSize={pageSize}
          disabled={listQuery.isFetching}
          onPageChange={setPage}
          onPageSizeChange={(size) => {
            setPageSize(size)
            setPage(1)
          }}
        />
      </section>

      <ModelPricingMutateSheet
        open={editTarget !== null}
        mode={editTarget?.mode ?? 'create'}
        pricing={editTarget?.pricing}
        quotaPerUnit={quotaPerUnit}
        onOpenChange={(open) => {
          if (!open) setEditTarget(null)
        }}
        onSaved={() => setEditTarget(null)}
      />

      <Dialog
        open={disableTarget !== null}
        onOpenChange={(open) => {
          if (!open) setDisableTarget(null)
        }}
        title={t('admin.pricing.disable_title')}
        description={t('admin.pricing.disable_body')}
        footer={
          <>
            <Button variant="secondary" onClick={() => setDisableTarget(null)}>
              {t('common.action.cancel')}
            </Button>
            <Button
              disabled={setStatus.isPending}
              onClick={() => {
                if (disableTarget !== null) {
                  setStatus.mutate({ row: disableTarget, status: 2 })
                }
              }}
            >
              {t('admin.pricing.disable')}
            </Button>
          </>
        }
      />

      <Dialog
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (!open) setDeleteTarget(null)
        }}
        title={t('admin.pricing.delete_title')}
        description={
          deleteTarget === null
            ? undefined
            : t('admin.pricing.delete_body', { model: deleteTarget.model })
        }
        footer={
          <>
            <Button variant="secondary" onClick={() => setDeleteTarget(null)}>
              {t('common.action.cancel')}
            </Button>
            <Button
              variant="danger"
              disabled={remove.isPending}
              onClick={() => {
                if (deleteTarget !== null) remove.mutate(deleteTarget)
              }}
            >
              {t('admin.pricing.delete')}
            </Button>
          </>
        }
      />
    </div>
  )
}

function renderPrice(price: number | null | undefined): string {
  if (price === null || price === undefined) return '—'
  return formatCredits(price)
}
