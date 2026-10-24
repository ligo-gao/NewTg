import { useEffect, useState } from 'react'
import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components, operations } from '@/api/schema'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { useI18n, type TranslateFn } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert, TextField } from '@/shared/ui/form'
import { StatusBadge } from '@/shared/ui/status-badge'
import { Tag } from '@/shared/ui/tag'
import { useToastStore } from '@/shared/ui/toast'

import { CatalogPagination } from './catalog-pagination'
import { invalidateCatalog, ModelMutateSheet } from './ModelMutateSheet'
import {
  canManageCatalog,
  canManageCatalogSensitive,
  canManagePricing,
  hasModelFilters,
  MODEL_SQUARE_STATES,
  type CatalogStatusFilter,
  type ModelCatalogSearch,
  type ModelSort,
  type ModelSquareState,
} from './model-catalog-search'
import {
  MissingModelsDrawer,
  missingModelsQueryOptions,
} from './MissingModelsDrawer'
import { SyncUpstreamDialog } from './SyncUpstreamDialog'

type ModelMeta = components['schemas']['ModelMeta']
type ModelListQuery = NonNullable<
  operations['listModelCatalog']['parameters']['query']
>

interface EditTarget {
  mode: 'create' | 'edit'
  model?: ModelMeta
  prefillModelName?: string
}

interface ModelFilterDraft {
  keyword: string
  vendorId: string
  status: '' | CatalogStatusFilter
  squareState: '' | ModelSquareState
  sort: ModelSort
}

function draftOf(search: ModelCatalogSearch): ModelFilterDraft {
  return {
    keyword: search.mv_keyword ?? '',
    vendorId: search.mv_vendor_id === undefined ? '' : String(search.mv_vendor_id),
    status: search.mv_status ?? '',
    squareState: search.mv_square_state ?? '',
    sort: search.mv_sort,
  }
}

const SQUARE_VARIANTS = {
  visible: 'success',
  partial: 'warning',
  unavailable: 'neutral',
  hidden: 'neutral-outline',
} as const

/** 排序值 → 词典键；负数前缀表示降序（与服务端 `sort` 语法一致）。 */
const MODEL_SORT_OPTIONS: readonly { value: ModelSort; key: string }[] = [
  { value: 'model_name', key: 'admin.models.sort_model_name' },
  { value: '-model_name', key: 'admin.models.sort_model_name_desc' },
  { value: 'status', key: 'admin.models.sort_status' },
  { value: '-status', key: 'admin.models.sort_status_desc' },
  { value: 'updated_at', key: 'admin.models.sort_updated' },
  { value: '-updated_at', key: 'admin.models.sort_updated_desc' },
  { value: 'created_at', key: 'admin.models.sort_created' },
  { value: '-created_at', key: 'admin.models.sort_created_desc' },
]

export interface ModelsTabProps {
  search: ModelCatalogSearch
  onSearchChange: (next: ModelCatalogSearch, options?: { replace?: boolean }) => void
  permissions: readonly string[] | undefined
}

/**
 * F-06-22 模型目录 Tab。
 *
 * 列表的四个派生字段（广场态 / 启用渠道数 / 分组 / 供应商名）由服务端在
 * 读取时计算，前端不做二次推断——公开广场与这里必须共用同一口径。
 *
 * 选择集按 `id → 行` 保存：批量删除需要区分「所选是否全部为精确匹配」
 * 来决定是否允许「同时从渠道移除」，只留 id 会丢失 `name_rule`。
 */
export function ModelsTab({ search, onSearchChange, permissions }: ModelsTabProps) {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)

  const canManage = canManageCatalog(permissions)
  const canSensitive = canManageCatalogSensitive(permissions)
  const canPrice = canManagePricing(permissions)

  const [draft, setDraft] = useState<ModelFilterDraft>(() => draftOf(search))
  const [editTarget, setEditTarget] = useState<EditTarget | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<ModelMeta | null>(null)
  const [missingOpen, setMissingOpen] = useState(false)
  const [syncOpen, setSyncOpen] = useState(false)
  const [batchOpen, setBatchOpen] = useState(false)
  const [selection, setSelection] = useState<ReadonlyMap<number, ModelMeta>>(
    () => new Map(),
  )
  const [removeFromChannels, setRemoveFromChannels] = useState(false)
  const [clearPricing, setClearPricing] = useState(false)
  const [batchError, setBatchError] = useState<string | undefined>(undefined)

  const listQuery = useQuery({
    queryKey: [
      'admin-models',
      search.mv_p,
      search.mv_page_size,
      search.mv_keyword ?? '',
      search.mv_vendor_id ?? '',
      search.mv_status ?? '',
      search.mv_square_state ?? '',
      search.mv_sort,
    ],
    placeholderData: keepPreviousData,
    queryFn: async () => {
      const query: ModelListQuery = {
        p: search.mv_p,
        page_size: search.mv_page_size,
        sort: search.mv_sort,
      }
      if (search.mv_keyword !== undefined) query.keyword = search.mv_keyword
      if (search.mv_vendor_id !== undefined) query.vendor_id = search.mv_vendor_id
      if (search.mv_status !== undefined) query.status = search.mv_status
      if (search.mv_square_state !== undefined) {
        query.square_state = search.mv_square_state
      }
      return requireData(await apiClient.GET('/api/models', { params: { query } }))
    },
  })

  // 工具栏徽章与缺失抽屉共用一个查询键：打开抽屉不会重复请求。
  const missingQuery = useQuery(missingModelsQueryOptions())
  const vendorOptions = useQuery({
    queryKey: ['admin-vendor-options'],
    staleTime: 60_000,
    queryFn: async () => {
      return requireData(
        await apiClient.GET('/api/vendors/search', {
          params: { query: { limit: 100 } },
        }),
      )
    },
  })

  const remove = useMutation({
    mutationFn: async (row: ModelMeta) => {
      // `updated_at` 带时区偏移，必须交给 openapi-fetch 的查询序列化器
      // 编码（字面 `+` 会被解码成空格），不要手工拼 URL。
      await apiClient.DELETE('/api/models/{id}', {
        params: {
          path: { id: row.id },
          query: { updated_at: row.updated_at },
        },
      })
    },
    retry: false,
    onSuccess: async () => {
      push({ tone: 'success', message: t('admin.models.delete_success') })
      setDeleteTarget(null)
      await invalidateCatalog(queryClient)
    },
    onError: (error) => {
      push({ tone: 'error', message: catalogActionError(error, t) })
      // 409/404 表示行已被别处改变；刷新让幽灵行消失。
      if (
        error instanceof ApiError &&
        (error.kind === 'conflict' || error.kind === 'not_found')
      ) {
        setDeleteTarget(null)
        void invalidateCatalog(queryClient)
      }
    },
  })

  const batchRemove = useMutation({
    mutationFn: async (ids: number[]) => {
      return requireData(
        await apiClient.POST('/api/models/batch-delete', {
          body: {
            ids,
            remove_from_channels: removeFromChannels,
            clear_pricing: clearPricing,
          },
        }),
      )
    },
    retry: false,
    onSuccess: async (result) => {
      push({
        tone: 'success',
        message: t('admin.models.batch_success', {
          deleted: result.deleted,
          channels: result.channels_updated,
          pricing: result.pricing_deleted + result.group_pricing_deleted,
        }),
      })
      setBatchOpen(false)
      setSelection(new Map())
      await invalidateCatalog(queryClient)
    },
    onError: (error) => {
      const message = batchDeleteError(error, t)
      setBatchError(message)
      push({ tone: 'error', message })
      // 404/409/422 之后列表与派生字段可能已经不一致；刷新但不自动重试
      // （web-00 §5.6：确定性拒绝不重放）。
      if (
        error instanceof ApiError &&
        (error.kind === 'conflict' ||
          error.kind === 'not_found' ||
          error.kind === 'validation')
      ) {
        setSelection(new Map())
        void invalidateCatalog(queryClient)
      }
    },
  })

  const data = listQuery.data
  const items = data?.items ?? []
  const total = data?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / search.mv_page_size))

  // offset 分页在删除后可能越过末页；回退到最后一页（web-00 §5.5）。
  useEffect(() => {
    if (!listQuery.isFetching && search.mv_p > pages) {
      onSearchChange({ ...search, mv_p: pages }, { replace: true })
    }
  }, [listQuery.isFetching, search, pages, onSearchChange])

  // 换页/换筛选后上一批选中项已不可见，继续保留会让批量删除作用到看不见的
  // 行上。依赖用序列化值而不是 search 对象（路由 hook 每次渲染可能返回新
  // 对象，直接依赖会反复清空选择）。
  const selectionKey = [
    search.mv_p,
    search.mv_page_size,
    search.mv_keyword ?? '',
    search.mv_vendor_id ?? '',
    search.mv_status ?? '',
    search.mv_square_state ?? '',
    search.mv_sort,
  ].join('|')
  useEffect(() => {
    setSelection(new Map())
  }, [selectionKey])

  function applyFilters(): void {
    const next: ModelCatalogSearch = { ...search, mv_p: 1 }
    if (draft.keyword.trim() === '') delete next.mv_keyword
    else next.mv_keyword = draft.keyword.trim().slice(0, 128)
    if (draft.vendorId === '') delete next.mv_vendor_id
    else next.mv_vendor_id = Number(draft.vendorId)
    if (draft.status === '') delete next.mv_status
    else next.mv_status = draft.status
    if (draft.squareState === '') delete next.mv_square_state
    else next.mv_square_state = draft.squareState
    next.mv_sort = draft.sort
    onSearchChange(next, { replace: true })
  }

  function resetFilters(): void {
    const next: ModelCatalogSearch = { ...search, mv_p: 1, mv_sort: 'model_name' }
    delete next.mv_keyword
    delete next.mv_vendor_id
    delete next.mv_status
    delete next.mv_square_state
    setDraft(draftOf(next))
    onSearchChange(next, { replace: true })
  }

  function toggleSelected(row: ModelMeta, checked: boolean): void {
    setSelection((current) => {
      const next = new Map(current)
      if (checked) next.set(row.id, row)
      else next.delete(row.id)
      return next
    })
  }

  const selectedRows = [...selection.values()]
  const anyInexactRule = selectedRows.some((row) => row.name_rule !== 0)
  const canRemoveFromChannels = selectedRows.length > 0 && !anyInexactRule
  const allOnPageSelected =
    items.length > 0 && items.every((row) => selection.has(row.id))
  const missingCount = missingQuery.data?.total ?? 0

  return (
    <div className="flex flex-col gap-3">
      <section className="rounded-xl border border-border-hairline bg-surface-subtle p-4">
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-5">
          <TextField
            label={t('admin.models.keyword')}
            value={draft.keyword}
            maxLength={128}
            onChange={(event) => {
              setDraft((current) => ({ ...current, keyword: event.target.value }))
            }}
          />
          <div className="flex flex-col gap-1.5">
            <label htmlFor="models-filter-vendor" className="text-meta font-medium text-ink-1">
              {t('admin.models.field_vendor')}
            </label>
            <select
              id="models-filter-vendor"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1"
              value={draft.vendorId}
              onChange={(event) => {
                setDraft((current) => ({ ...current, vendorId: event.target.value }))
              }}
            >
              <option value="">{t('admin.models.all_vendors')}</option>
              {(vendorOptions.data?.items ?? []).map((vendor) => (
                <option key={vendor.id} value={vendor.id}>
                  {vendor.name}
                </option>
              ))}
            </select>
          </div>
          <div className="flex flex-col gap-1.5">
            <label htmlFor="models-filter-status" className="text-meta font-medium text-ink-1">
              {t('admin.models.field_status')}
            </label>
            <select
              id="models-filter-status"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1"
              value={draft.status}
              onChange={(event) => {
                setDraft((current) => ({
                  ...current,
                  status: event.target.value as ModelFilterDraft['status'],
                }))
              }}
            >
              <option value="">{t('admin.models.all')}</option>
              <option value="enabled">{t('admin.models.status_enabled')}</option>
              <option value="disabled">{t('admin.models.status_disabled')}</option>
            </select>
          </div>
          <div className="flex flex-col gap-1.5">
            <label htmlFor="models-filter-square" className="text-meta font-medium text-ink-1">
              {t('admin.models.column_square')}
            </label>
            <select
              id="models-filter-square"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1"
              value={draft.squareState}
              onChange={(event) => {
                setDraft((current) => ({
                  ...current,
                  squareState: event.target.value as ModelFilterDraft['squareState'],
                }))
              }}
            >
              <option value="">{t('admin.models.all')}</option>
              {MODEL_SQUARE_STATES.map((state) => (
                <option key={state} value={state}>
                  {t(`admin.models.square_${state}`)}
                </option>
              ))}
            </select>
          </div>
          <div className="flex flex-col gap-1.5">
            <label htmlFor="models-filter-sort" className="text-meta font-medium text-ink-1">
              {t('admin.models.sort')}
            </label>
            <select
              id="models-filter-sort"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1"
              value={draft.sort}
              onChange={(event) => {
                setDraft((current) => ({
                  ...current,
                  sort: event.target.value as ModelSort,
                }))
              }}
            >
              {MODEL_SORT_OPTIONS.map((option) => (
                <option key={option.value} value={option.value}>
                  {t(option.key)}
                </option>
              ))}
            </select>
          </div>
        </div>
        <div className="mt-3 flex justify-end gap-2">
          <Button variant="secondary" onClick={resetFilters}>
            {t('admin.models.reset')}
          </Button>
          <Button onClick={applyFilters}>{t('admin.models.search')}</Button>
        </div>
      </section>

      <section className="rounded-xl border border-border-hairline bg-surface-card">
        <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border-hairline px-5 py-3">
          <div className="flex flex-wrap items-center gap-2">
            {canManage ? (
              <Button onClick={() => setEditTarget({ mode: 'create' })}>
                {t('admin.models.new_model')}
              </Button>
            ) : null}
            {canSensitive ? (
              <Button variant="secondary" onClick={() => setSyncOpen(true)}>
                {t('admin.models.sync_upstream')}
              </Button>
            ) : null}
            <Button variant="secondary" onClick={() => setMissingOpen(true)}>
              {t('admin.models.missing_models')}
              {missingCount > 0 ? (
                <span className="ml-1 rounded-full bg-surface-subtle px-1.5 font-mono text-badge tabular-nums">
                  {missingCount}
                </span>
              ) : null}
            </Button>
            {canSensitive ? (
              <Button
                variant="danger"
                className="h-8 px-3 text-meta"
                disabled={selectedRows.length === 0}
                onClick={() => {
                  setRemoveFromChannels(false)
                  setClearPricing(false)
                  setBatchError(undefined)
                  setBatchOpen(true)
                }}
              >
                {t('admin.models.delete_selected', { count: selectedRows.length })}
              </Button>
            ) : null}
          </div>
          <span className="text-meta text-ink-2" aria-live="polite">
            {selectedRows.length > 0
              ? `${t('admin.models.selected', { count: selectedRows.length })} · `
              : ''}
            {t('admin.models.total', { count: total })}
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
            <span>{t('admin.models.error')}</span>
            <Button variant="secondary" onClick={() => void listQuery.refetch()}>
              {t('common.action.retry')}
            </Button>
          </div>
        ) : items.length === 0 ? (
          <div className="flex flex-col items-start gap-2 p-5">
            <p className="text-body text-ink-2">
              {hasModelFilters(search)
                ? t('admin.models.empty_filtered')
                : t('admin.models.empty')}
            </p>
            {hasModelFilters(search) ? (
              <Button variant="secondary" onClick={resetFilters}>
                {t('admin.models.clear_filters')}
              </Button>
            ) : canManage ? (
              <Button onClick={() => setEditTarget({ mode: 'create' })}>
                {t('admin.models.new_model')}
              </Button>
            ) : null}
          </div>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[1160px] text-body">
              <thead>
                <tr className="border-b border-border-hairline text-left text-meta text-ink-2">
                  <th scope="col" className="px-4 py-2">
                    <input
                      type="checkbox"
                      aria-label={t('admin.models.select_all')}
                      checked={allOnPageSelected}
                      onChange={(event) => {
                        setSelection(
                          event.target.checked
                            ? new Map(items.map((row) => [row.id, row]))
                            : new Map(),
                        )
                      }}
                    />
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.models.column_model')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.models.column_vendor')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.models.column_tags')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.models.column_status')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.models.column_square')}
                  </th>
                  <th scope="col" className="px-4 py-2 text-right font-medium">
                    {t('admin.models.column_channels')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.models.column_updated')}
                  </th>
                  <th scope="col" className="px-4 py-2 text-right font-medium">
                    {t('admin.models.actions')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {items.map((row) => (
                  <tr
                    key={row.id}
                    className="border-b border-border-hairline last:border-0 hover:bg-surface-hover"
                  >
                    <td className="px-4 py-3">
                      <input
                        type="checkbox"
                        aria-label={t('admin.models.select_row', { name: row.model_name })}
                        checked={selection.has(row.id)}
                        onChange={(event) => {
                          toggleSelected(row, event.target.checked)
                        }}
                      />
                    </td>
                    <td className="px-4 py-3">
                      <span className="font-mono text-ink-1">{row.model_name}</span>
                      {row.display_name !== null && row.display_name !== undefined ? (
                        <span className="block text-meta text-ink-2">{row.display_name}</span>
                      ) : null}
                    </td>
                    <td className="px-4 py-3 text-ink-1">{row.vendor_name ?? '—'}</td>
                    <td className="px-4 py-3">
                      <div className="flex flex-wrap gap-1">
                        {row.tags.slice(0, 2).map((tag) => (
                          <Tag key={tag}>{tag}</Tag>
                        ))}
                        {row.tags.length > 2 ? <Tag>+{row.tags.length - 2}</Tag> : null}
                      </div>
                    </td>
                    <td className="px-4 py-3">
                      <StatusBadge variant={row.status === 1 ? 'success' : 'neutral'}>
                        {row.status === 1
                          ? t('admin.models.status_enabled')
                          : t('admin.models.status_disabled')}
                      </StatusBadge>
                    </td>
                    <td className="px-4 py-3">
                      <StatusBadge variant={SQUARE_VARIANTS[row.square_state]}>
                        {t(`admin.models.square_${row.square_state}`)}
                      </StatusBadge>
                    </td>
                    <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                      {row.configured_channel_count}
                    </td>
                    <td className="px-4 py-3 text-meta text-ink-2" title={row.updated_at}>
                      {formatRelativeTime(row.updated_at, locale)}
                    </td>
                    <td className="px-4 py-3">
                      {canManage ? (
                        <div className="flex justify-end gap-1">
                          <Button
                            variant="ghost"
                            className="h-8 px-2 text-meta"
                            onClick={() => setEditTarget({ mode: 'edit', model: row })}
                          >
                            {t('admin.models.edit')}
                          </Button>
                          <Button
                            variant="ghost"
                            className="h-8 px-2 text-meta text-danger hover:bg-danger-bg"
                            onClick={() => setDeleteTarget(row)}
                          >
                            {t('admin.models.delete')}
                          </Button>
                        </div>
                      ) : null}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}

        <CatalogPagination
          page={search.mv_p}
          pages={pages}
          pageSize={search.mv_page_size}
          disabled={listQuery.isFetching}
          onPageChange={(page) => {
            onSearchChange({ ...search, mv_p: page }, { replace: true })
          }}
          onPageSizeChange={(size) => {
            onSearchChange({ ...search, mv_page_size: size, mv_p: 1 }, { replace: true })
          }}
        />
      </section>

      <ModelMutateSheet
        open={editTarget !== null}
        mode={editTarget?.mode ?? 'create'}
        model={editTarget?.model}
        prefillModelName={editTarget?.prefillModelName}
        onOpenChange={(open) => {
          if (!open) setEditTarget(null)
        }}
        onSaved={() => setEditTarget(null)}
      />

      <MissingModelsDrawer
        open={missingOpen}
        onOpenChange={setMissingOpen}
        canManage={canManage}
        onCreateRecord={(modelName) => {
          setMissingOpen(false)
          setEditTarget({ mode: 'create', prefillModelName: modelName })
        }}
      />

      <SyncUpstreamDialog open={syncOpen} onOpenChange={setSyncOpen} />

      <Dialog
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (!open) setDeleteTarget(null)
        }}
        role="alertdialog"
        title={t('admin.models.delete_title')}
        description={
          deleteTarget === null
            ? undefined
            : t('admin.models.delete_body', { name: deleteTarget.model_name })
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
              {t('admin.models.delete')}
            </Button>
          </>
        }
      />

      <Dialog
        open={batchOpen}
        onOpenChange={setBatchOpen}
        variant="sheet"
        panelClassName="!max-w-[480px]"
        title={t('admin.models.batch_title', { count: selectedRows.length })}
        description={t('admin.models.batch_body')}
      >
        <div className="flex flex-col gap-4">
          {batchError !== undefined ? <FormAlert tone="danger">{batchError}</FormAlert> : null}

          <label className="flex items-start gap-2 text-body text-ink-1">
            <input
              type="checkbox"
              className="mt-0.5"
              checked={removeFromChannels}
              disabled={!canRemoveFromChannels}
              onChange={(event) => {
                setRemoveFromChannels(event.target.checked)
              }}
            />
            <span>
              {t('admin.models.batch_remove_channels')}
              <span className="block text-meta text-ink-3">
                {anyInexactRule
                  ? t('admin.models.batch_remove_channels_blocked')
                  : t('admin.models.batch_remove_channels_hint')}
              </span>
            </span>
          </label>

          <label className="flex items-start gap-2 text-body text-ink-1">
            <input
              type="checkbox"
              className="mt-0.5"
              checked={clearPricing}
              disabled={!canPrice}
              onChange={(event) => {
                setClearPricing(event.target.checked)
              }}
            />
            <span>
              {t('admin.models.batch_clear_pricing')}
              <span className="block text-meta text-ink-3">
                {canPrice
                  ? t('admin.models.batch_clear_pricing_hint')
                  : t('admin.models.batch_clear_pricing_blocked')}
              </span>
            </span>
          </label>

          <div className="flex justify-end gap-2">
            <Button variant="secondary" onClick={() => setBatchOpen(false)}>
              {t('common.action.cancel')}
            </Button>
            <Button
              variant="danger"
              disabled={batchRemove.isPending || selectedRows.length === 0}
              onClick={() => {
                batchRemove.mutate(selectedRows.map((row) => row.id))
              }}
            >
              {t('admin.models.batch_confirm', { count: selectedRows.length })}
            </Button>
          </div>
        </div>
      </Dialog>
    </div>
  )
}

function catalogActionError(error: unknown, t: TranslateFn): string {
  if (error instanceof ApiError) {
    if (error.kind === 'conflict') return t('admin.models.conflict')
    if (error.kind === 'not_found') return t('admin.models.not_found')
    if (error.kind === 'validation') return t('admin.models.validation_error')
    if (error.kind === 'forbidden') return t('admin.models.forbidden')
  }
  return t('admin.models.action_error')
}

/**
 * 批量删除的 422 有两种：空选择/超限（无扩展字段）与「渠道将没有可用模型」
 * （`channel_ids`）。后者必须把渠道号列出来，否则管理员只知道被拒绝，不知道
 * 该先解除哪个渠道的最后一个模型。
 */
function batchDeleteError(error: unknown, t: TranslateFn): string {
  if (
    error instanceof ApiError &&
    error.kind === 'validation' &&
    error.channel_ids !== undefined &&
    error.channel_ids.length > 0
  ) {
    const channels = error.channel_ids.map((id) => `#${id}`).join(', ')
    return t('admin.models.batch_channels_empty', { channels })
  }
  return catalogActionError(error, t)
}
