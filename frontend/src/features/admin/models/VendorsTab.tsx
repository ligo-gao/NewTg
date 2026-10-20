import { useEffect, useState } from 'react'
import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components, operations } from '@/api/schema'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert, TextField } from '@/shared/ui/form'
import { StatusBadge } from '@/shared/ui/status-badge'
import { useToastStore } from '@/shared/ui/toast'

import { CatalogPagination } from './catalog-pagination'
import {
  canManageCatalog,
  canManageCatalogSensitive,
  hasVendorFilters,
  type CatalogStatusFilter,
  type ModelCatalogSearch,
  type VendorAttachedFilter,
  type VendorSort,
} from './model-catalog-search'
import { VendorMutateSheet, vendorActionError } from './VendorMutateSheet'
import {
  VendorOperationsWizard,
  type VendorOperationAction,
} from './VendorOperationsWizard'

type Vendor = components['schemas']['Vendor']
type VendorListQuery = NonNullable<
  operations['listVendors']['parameters']['query']
>

interface VendorFilterDraft {
  keyword: string
  status: '' | CatalogStatusFilter
  attached: '' | VendorAttachedFilter
  sort: VendorSort
}

function draftOf(search: ModelCatalogSearch): VendorFilterDraft {
  return {
    keyword: search.vv_keyword ?? '',
    status: search.vv_status ?? '',
    attached: search.vv_attached ?? '',
    sort: search.vv_sort,
  }
}

const VENDOR_SORT_OPTIONS: readonly { value: VendorSort; key: string }[] = [
  { value: 'name', key: 'admin.vendors.sort_name' },
  { value: '-name', key: 'admin.vendors.sort_name_desc' },
  { value: 'model_count', key: 'admin.vendors.sort_model_count' },
  { value: '-model_count', key: 'admin.vendors.sort_model_count_desc' },
  { value: 'updated_at', key: 'admin.vendors.sort_updated' },
  { value: '-updated_at', key: 'admin.vendors.sort_updated_desc' },
  { value: 'created_at', key: 'admin.vendors.sort_created' },
  { value: '-created_at', key: 'admin.vendors.sort_created_desc' },
]

export interface VendorsTabProps {
  search: ModelCatalogSearch
  onSearchChange: (next: ModelCatalogSearch, options?: { replace?: boolean }) => void
  permissions: readonly string[] | undefined
}

/**
 * F-06-23 供应商目录 Tab。
 *
 * 供应商的乐观锁是派生 `version`（7 列规范化 sha256），不是 `updated_at`；
 * 因此编辑与批量操作都以「读到什么就回传什么」为准，列表刷新后旧值立即
 * 作废（409 后必须重查）。
 */
export function VendorsTab({ search, onSearchChange, permissions }: VendorsTabProps) {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)

  const canManage = canManageCatalog(permissions)
  const canSensitive = canManageCatalogSensitive(permissions)

  const [draft, setDraft] = useState<VendorFilterDraft>(() => draftOf(search))
  const [editTarget, setEditTarget] = useState<
    { mode: 'create' } | { mode: 'edit'; vendor: Vendor } | null
  >(null)
  const [deleteTarget, setDeleteTarget] = useState<Vendor | null>(null)
  const [selection, setSelection] = useState<ReadonlyMap<number, Vendor>>(
    () => new Map(),
  )
  const [wizardOpen, setWizardOpen] = useState(false)
  const [wizardAction, setWizardAction] = useState<VendorOperationAction>('assign')
  const [wizardVendorIds, setWizardVendorIds] = useState<readonly number[]>([])

  const listQuery = useQuery({
    queryKey: [
      'admin-vendors',
      search.vv_p,
      search.vv_page_size,
      search.vv_keyword ?? '',
      search.vv_status ?? '',
      search.vv_attached ?? '',
      search.vv_sort,
    ],
    placeholderData: keepPreviousData,
    queryFn: async () => {
      const query: VendorListQuery = {
        p: search.vv_p,
        page_size: search.vv_page_size,
        sort: search.vv_sort,
      }
      if (search.vv_keyword !== undefined) query.keyword = search.vv_keyword
      if (search.vv_status !== undefined) query.status = search.vv_status
      if (search.vv_attached !== undefined) {
        query.attached = search.vv_attached === 'attached'
      }
      return requireData(await apiClient.GET('/api/vendors', { params: { query } }))
    },
  })

  const remove = useMutation({
    mutationFn: async (vendor: Vendor) => {
      await apiClient.DELETE('/api/vendors/{id}', {
        params: { path: { id: vendor.id } },
      })
    },
    retry: false,
    onSuccess: async () => {
      push({ tone: 'success', message: t('admin.vendors.delete_success') })
      setDeleteTarget(null)
      await invalidateVendors(queryClient)
    },
    onError: (error) => {
      push({ tone: 'error', message: vendorActionError(error, t) })
      // 409 的常见原因是删除前一刻新增了引用（引用计数在列表里已过期）。
      if (error instanceof ApiError && error.kind === 'conflict') {
        setDeleteTarget(null)
        void invalidateVendors(queryClient)
      }
    },
  })

  const data = listQuery.data
  const items = data?.items ?? []
  const total = data?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / search.vv_page_size))

  useEffect(() => {
    if (!listQuery.isFetching && search.vv_p > pages) {
      onSearchChange({ ...search, vv_p: pages }, { replace: true })
    }
  }, [listQuery.isFetching, search, pages, onSearchChange])

  const selectionKey = [
    search.vv_p,
    search.vv_page_size,
    search.vv_keyword ?? '',
    search.vv_status ?? '',
    search.vv_attached ?? '',
    search.vv_sort,
  ].join('|')
  useEffect(() => {
    setSelection(new Map())
  }, [selectionKey])

  function applyFilters(): void {
    const next: ModelCatalogSearch = { ...search, vv_p: 1 }
    if (draft.keyword.trim() === '') delete next.vv_keyword
    else next.vv_keyword = draft.keyword.trim().slice(0, 128)
    if (draft.status === '') delete next.vv_status
    else next.vv_status = draft.status
    if (draft.attached === '') delete next.vv_attached
    else next.vv_attached = draft.attached
    next.vv_sort = draft.sort
    onSearchChange(next, { replace: true })
  }

  function resetFilters(): void {
    const next: ModelCatalogSearch = { ...search, vv_p: 1, vv_sort: 'name' }
    delete next.vv_keyword
    delete next.vv_status
    delete next.vv_attached
    setDraft(draftOf(next))
    onSearchChange(next, { replace: true })
  }

  const selectedRows = [...selection.values()]
  const allOnPageSelected =
    items.length > 0 && items.every((vendor) => selection.has(vendor.id))

  function openWizard(
    action: VendorOperationAction,
    vendorIds: readonly number[],
  ): void {
    setWizardAction(action)
    setWizardVendorIds(vendorIds)
    setWizardOpen(true)
  }

  return (
    <div className="flex flex-col gap-3">
      <section className="rounded-xl border border-border-hairline bg-surface-subtle p-4">
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
          <TextField
            label={t('admin.models.keyword')}
            value={draft.keyword}
            maxLength={128}
            onChange={(event) => {
              setDraft((current) => ({ ...current, keyword: event.target.value }))
            }}
          />
          <div className="flex flex-col gap-1.5">
            <label htmlFor="vendors-filter-status" className="text-[13px] font-medium text-ink-1">
              {t('admin.vendors.field_status')}
            </label>
            <select
              id="vendors-filter-status"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1"
              value={draft.status}
              onChange={(event) => {
                setDraft((current) => ({
                  ...current,
                  status: event.target.value as VendorFilterDraft['status'],
                }))
              }}
            >
              <option value="">{t('admin.models.all')}</option>
              <option value="enabled">{t('admin.models.status_enabled')}</option>
              <option value="disabled">{t('admin.models.status_disabled')}</option>
            </select>
          </div>
          <div className="flex flex-col gap-1.5">
            <label htmlFor="vendors-filter-attached" className="text-[13px] font-medium text-ink-1">
              {t('admin.vendors.filter_attached')}
            </label>
            <select
              id="vendors-filter-attached"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1"
              value={draft.attached}
              onChange={(event) => {
                setDraft((current) => ({
                  ...current,
                  attached: event.target.value as VendorFilterDraft['attached'],
                }))
              }}
            >
              <option value="">{t('admin.models.all')}</option>
              <option value="attached">{t('admin.vendors.attached_yes')}</option>
              <option value="unattached">{t('admin.vendors.attached_no')}</option>
            </select>
          </div>
          <div className="flex flex-col gap-1.5">
            <label htmlFor="vendors-filter-sort" className="text-[13px] font-medium text-ink-1">
              {t('admin.models.sort')}
            </label>
            <select
              id="vendors-filter-sort"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1"
              value={draft.sort}
              onChange={(event) => {
                setDraft((current) => ({
                  ...current,
                  sort: event.target.value as VendorSort,
                }))
              }}
            >
              {VENDOR_SORT_OPTIONS.map((option) => (
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
                {t('admin.vendors.new_vendor')}
              </Button>
            ) : null}
            {canSensitive ? (
              <Button
                variant="secondary"
                onClick={() => openWizard('assign', [...selection.keys()])}
              >
                {t('admin.vendors.ops_title')}
              </Button>
            ) : null}
          </div>
          <span className="text-[13px] text-ink-2" aria-live="polite">
            {selectedRows.length > 0
              ? `${t('admin.vendors.selected', { count: selectedRows.length })} · `
              : ''}
            {t('admin.vendors.total', { count: total })}
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
            <span>{t('admin.vendors.error')}</span>
            <Button variant="secondary" onClick={() => void listQuery.refetch()}>
              {t('common.action.retry')}
            </Button>
          </div>
        ) : items.length === 0 ? (
          <div className="flex flex-col items-start gap-2 p-5">
            <p className="text-sm text-ink-2">
              {hasVendorFilters(search)
                ? t('admin.vendors.empty_filtered')
                : t('admin.vendors.empty')}
            </p>
            {hasVendorFilters(search) ? (
              <Button variant="secondary" onClick={resetFilters}>
                {t('admin.models.clear_filters')}
              </Button>
            ) : canManage ? (
              <Button onClick={() => setEditTarget({ mode: 'create' })}>
                {t('admin.vendors.new_vendor')}
              </Button>
            ) : null}
          </div>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[960px] text-sm">
              <thead>
                <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
                  <th scope="col" className="px-4 py-2">
                    <input
                      type="checkbox"
                      aria-label={t('admin.vendors.select_all')}
                      checked={allOnPageSelected}
                      onChange={(event) => {
                        setSelection(
                          event.target.checked
                            ? new Map(items.map((vendor) => [vendor.id, vendor]))
                            : new Map(),
                        )
                      }}
                    />
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.vendors.column_name')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.vendors.column_description')}
                  </th>
                  <th scope="col" className="px-4 py-2 text-right font-medium">
                    {t('admin.vendors.column_model_count')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.vendors.column_status')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.vendors.column_updated')}
                  </th>
                  <th scope="col" className="px-4 py-2 text-right font-medium">
                    {t('admin.models.actions')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {items.map((vendor) => (
                  <tr
                    key={vendor.id}
                    className="border-b border-border-hairline last:border-0 hover:bg-surface-hover"
                  >
                    <td className="px-4 py-3">
                      <input
                        type="checkbox"
                        aria-label={t('admin.vendors.select_row', { name: vendor.name })}
                        checked={selection.has(vendor.id)}
                        onChange={(event) => {
                          const checked = event.target.checked
                          setSelection((current) => {
                            const next = new Map(current)
                            if (checked) next.set(vendor.id, vendor)
                            else next.delete(vendor.id)
                            return next
                          })
                        }}
                      />
                    </td>
                    <td className="px-4 py-3 font-medium text-ink-1">{vendor.name}</td>
                    <td className="max-w-[320px] truncate px-4 py-3 text-ink-2" title={vendor.description ?? undefined}>
                      {vendor.description ?? '—'}
                    </td>
                    <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                      {vendor.model_count}
                    </td>
                    <td className="px-4 py-3">
                      <StatusBadge variant={vendor.status === 1 ? 'success' : 'neutral'}>
                        {vendor.status === 1
                          ? t('admin.models.status_enabled')
                          : t('admin.models.status_disabled')}
                      </StatusBadge>
                    </td>
                    <td className="px-4 py-3 text-xs text-ink-2" title={vendor.updated_at}>
                      {formatRelativeTime(vendor.updated_at, locale)}
                    </td>
                    <td className="px-4 py-3">
                      {canManage ? (
                        <div className="flex justify-end gap-1">
                          <Button
                            variant="ghost"
                            className="h-8 px-2 text-[13px]"
                            onClick={() => setEditTarget({ mode: 'edit', vendor })}
                          >
                            {t('admin.models.edit')}
                          </Button>
                          <Button
                            variant="ghost"
                            className="h-8 px-2 text-[13px] text-danger hover:bg-danger-bg"
                            onClick={() => setDeleteTarget(vendor)}
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
          page={search.vv_p}
          pages={pages}
          pageSize={search.vv_page_size}
          disabled={listQuery.isFetching}
          onPageChange={(page) => {
            onSearchChange({ ...search, vv_p: page }, { replace: true })
          }}
          onPageSizeChange={(size) => {
            onSearchChange({ ...search, vv_page_size: size, vv_p: 1 }, { replace: true })
          }}
        />
      </section>

      <VendorMutateSheet
        open={editTarget !== null}
        mode={editTarget?.mode ?? 'create'}
        vendor={editTarget?.mode === 'edit' ? editTarget.vendor : undefined}
        onOpenChange={(open) => {
          if (!open) setEditTarget(null)
        }}
        onSaved={() => setEditTarget(null)}
      />

      <VendorOperationsWizard
        open={wizardOpen}
        onOpenChange={setWizardOpen}
        selectedVendorIds={wizardVendorIds}
        initialAction={wizardAction}
        onApplied={() => {
          setSelection(new Map())
        }}
      />

      <Dialog
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (!open) setDeleteTarget(null)
        }}
        role="alertdialog"
        title={t('admin.vendors.delete_title')}
        description={
          deleteTarget === null ? undefined : t('admin.vendors.delete_body')
        }
        footer={
          <>
            <Button variant="secondary" onClick={() => setDeleteTarget(null)}>
              {t('common.action.cancel')}
            </Button>
            <Button
              variant="danger"
              disabled={remove.isPending || (deleteTarget?.model_count ?? 0) > 0}
              onClick={() => {
                if (deleteTarget !== null) remove.mutate(deleteTarget)
              }}
            >
              {t('admin.models.delete')}
            </Button>
          </>
        }
      >
        {deleteTarget !== null && deleteTarget.model_count > 0 ? (
          <div className="flex flex-col items-start gap-2">
            <FormAlert tone="warning">
              {t('admin.vendors.delete_referenced', { count: deleteTarget.model_count })}
            </FormAlert>
            <Button
              variant="secondary"
              onClick={() => {
                openWizard('merge', [deleteTarget.id])
                setDeleteTarget(null)
              }}
            >
              {t('admin.vendors.delete_merge_instead')}
            </Button>
          </div>
        ) : null}
      </Dialog>
    </div>
  )
}

async function invalidateVendors(
  queryClient: ReturnType<typeof useQueryClient>,
): Promise<void> {
  await Promise.all([
    queryClient.invalidateQueries({ queryKey: ['admin-vendors'] }),
    queryClient.invalidateQueries({ queryKey: ['admin-vendor-options'] }),
    queryClient.invalidateQueries({ queryKey: ['admin-models'] }),
  ])
}
