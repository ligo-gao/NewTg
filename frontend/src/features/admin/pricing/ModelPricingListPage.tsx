import { useEffect, useState } from 'react'
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
export function ModelPricingListPage() {
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

  const listQuery = useQuery({
    queryKey: ['admin-model-pricing', applied, page, pageSize],
    // 能力探测完成前不发列表请求：无 pricing:read 时那是注定 403 的请求，
    // 也会避免「先闪 403 再出现表格」。
    enabled: !capabilities.loading && capabilities.canRead,
    placeholderData: keepPreviousData,
    queryFn: async () => {
      const query: ListQuery = {
        p: page,
        page_size: pageSize,
        sort: applied.sort,
      }
      if (applied.keyword.trim() !== '') query.keyword = applied.keyword.trim()
      if (applied.mode !== '') query.mode = applied.mode
      if (applied.status !== '') query.status = applied.status
      return requireData(
        await apiClient.GET('/api/model-pricing', { params: { query } }),
      )
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
  const items = data?.items ?? []
  const total = data?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / pageSize))
  const quotaPerUnit = data?.quota_per_unit ?? DEFAULT_QUOTA_PER_UNIT

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
    applied.keyword.trim() !== '' ||
    applied.mode !== '' ||
    applied.status !== '' ||
    applied.sort !== 'model'

  if (capabilities.loading) {
    return (
      <div className="mx-auto flex max-w-[1440px] flex-col gap-3">
        <h1 className="text-xl font-semibold text-ink-1">{t('admin.pricing.title')}</h1>
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
      <div className="mx-auto max-w-[1440px] rounded-xl border border-border-hairline bg-surface-card p-8">
        <h1 className="text-xl font-semibold text-ink-1">{t('common.error403.title')}</h1>
        <p className="mt-2 text-sm text-ink-2">{t('common.error403.body')}</p>
      </div>
    )
  }

  return (
    <div className="mx-auto flex max-w-[1440px] flex-col gap-3">
      <h1 className="text-xl font-semibold text-ink-1">{t('admin.pricing.title')}</h1>

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
            <label htmlFor="pricing-filter-mode" className="text-[13px] font-medium text-ink-1">
              {t('admin.pricing.mode')}
            </label>
            <select
              id="pricing-filter-mode"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
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
            <label htmlFor="pricing-filter-status" className="text-[13px] font-medium text-ink-1">
              {t('admin.pricing.status')}
            </label>
            <select
              id="pricing-filter-status"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
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
            <label htmlFor="pricing-filter-sort" className="text-[13px] font-medium text-ink-1">
              {t('admin.pricing.sort')}
            </label>
            <select
              id="pricing-filter-sort"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
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
          <span className="text-[13px] text-ink-2">
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
          <div className="flex items-center gap-3 p-5 text-sm text-ink-2">
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
            <p className="text-sm text-ink-2">
              {hasFilters ? t('admin.pricing.empty_filtered') : t('admin.pricing.empty')}
            </p>
            {hasFilters ? (
              <Button variant="secondary" onClick={resetFilters}>
                {t('admin.pricing.clear_filters')}
              </Button>
            ) : (
              <>
                <p className="text-xs text-ink-3">{t('admin.pricing.empty_hint')}</p>
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
            <table className="w-full min-w-[1080px] text-sm">
              <thead>
                <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
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
                <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
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
                      <td className="px-4 py-3 text-xs text-ink-2" title={row.updated_at}>
                        {formatRelativeTime(row.updated_at, locale)}
                      </td>
                      <td className="px-4 py-3">
                        {capabilities.canManage ? (
                          <div className="flex justify-end gap-1">
                            <Button
                              variant="ghost"
                              className="h-8 px-2 text-[13px]"
                              onClick={() => setEditTarget({ mode: 'edit', pricing: row })}
                            >
                              {t('admin.pricing.edit')}
                            </Button>
                            <Button
                              variant="ghost"
                              className="h-8 px-2 text-[13px]"
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
                              className="h-8 px-2 text-[13px] text-danger hover:bg-danger-bg"
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
