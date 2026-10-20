import { useEffect, useState } from 'react'
import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components, operations } from '@/api/schema'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { formatCredits } from '@/shared/lib/format'
import { useI18n, type TranslateFn } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'
import { TextField } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

import { CreateCodesResultDialog, CreateCodesSheet } from './create-codes-sheet'
import {
  hasRedemptionFilters,
  type RedemptionSearch,
  type RedemptionStatusFilter,
} from './redemption-search'

type RedemptionCode = components['schemas']['RedemptionCode']
type RedemptionCodeStatus = components['schemas']['RedemptionCodeStatus']
type CreateResponse = components['schemas']['RedemptionCodeCreateResponse']
type ListQuery = NonNullable<operations['listRedemptionCodes']['parameters']['query']>

const PAGE_SIZES = [10, 20, 50] as const

interface FilterDraft {
  status: '' | RedemptionStatusFilter
  batch: string
  min: string
  max: string
}

const EMPTY_DRAFT: FilterDraft = { status: '', batch: '', min: '', max: '' }

function draftOf(search: RedemptionSearch): FilterDraft {
  return {
    status: search.status ?? '',
    batch: search.batch ?? '',
    min: search.min === undefined ? '' : String(search.min),
    max: search.max === undefined ? '' : String(search.max),
  }
}

function parseNonNegative(value: string): number | undefined {
  if (!/^\d+$/.test(value.trim())) return undefined
  const parsed = Number(value.trim())
  return Number.isSafeInteger(parsed) ? parsed : undefined
}

const STATUS_VARIANTS: Record<RedemptionCodeStatus, StatusBadgeVariant> = {
  unused: 'success',
  used: 'neutral',
  disabled: 'neutral',
  // 虚拟态（expires_at 已过但仍未使用）；黄色只为提醒，不代表错误。
  expired: 'warning',
}

function statusKey(status: RedemptionCodeStatus): string {
  return `admin.redemption.status_${status}`
}

/** `<1024` 管理页闸门（modules-04 UI §7.6）；无 matchMedia 的环境按桌面处理。 */
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

export interface RedemptionCodesPageProps {
  search: RedemptionSearch
  onSearchChange: (
    next: RedemptionSearch,
    options?: { replace?: boolean },
  ) => void
}

/**
 * F-04-16 兑换码管理页（`/redemption-codes`）。
 *
 * 列表投影**永不包含码值**：服务端只存 SHA-256 哈希，`code_mask` 固定
 * `••••••`，所以这一页不提供复制按钮。明文只在创建响应（F-04-15）出现
 * 一次，创建弹窗关闭后无法再取回。
 *
 * 分页与筛选都在 URL 里：刷新/分享链接后仍停留在同一批兑换码上，也让
 * 「上一页/查询」这类操作走浏览器历史而不是内部状态。
 */
export function RedemptionCodesPage({ search, onSearchChange }: RedemptionCodesPageProps) {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const wideEnough = useMinWidth(1024)

  const [draft, setDraft] = useState<FilterDraft>(() => draftOf(search))
  const [pageSize, setPageSize] = useState(20)
  const [createOpen, setCreateOpen] = useState(false)
  const [created, setCreated] = useState<CreateResponse | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<RedemptionCode | null>(null)
  const [bulkDeleteOpen, setBulkDeleteOpen] = useState(false)
  const [selected, setSelected] = useState<ReadonlySet<number>>(() => new Set())

  const listQuery = useQuery({
    queryKey: ['admin-redemption-codes', search, pageSize],
    enabled: wideEnough,
    placeholderData: keepPreviousData,
    queryFn: async () => {
      const query: ListQuery = { p: search.p, page_size: pageSize }
      if (search.status !== undefined) query.status = search.status
      if (search.batch !== undefined) query.batch_name = search.batch
      if (search.min !== undefined) query.min_credits = search.min
      if (search.max !== undefined) query.max_credits = search.max
      return requireData(await apiClient.GET('/api/redemption-codes', { params: { query } }))
    },
  })

  const invalidate = async (): Promise<void> => {
    await queryClient.invalidateQueries({ queryKey: ['admin-redemption-codes'] })
  }

  const setStatus = useMutation({
    mutationFn: async ({ row, status }: { row: RedemptionCode; status: 'unused' | 'disabled' }) => {
      await apiClient.PATCH('/api/redemption-codes/{id}', {
        params: { path: { id: row.id } },
        body: { status },
      })
    },
    // 显式不重试：409 是确定性拒绝（已使用的码不能再变更），重试只会把
    // 「已使用」的提示延后；429 更不该在客户端自动放大压力。QueryClient
    // 全局已是 retry=0，这里保留契约意图。
    retry: false,
    onSuccess: async () => {
      push({ tone: 'success', message: t('admin.redemption.status_success') })
      await invalidate()
    },
    onError: (error) => {
      push({ tone: 'error', message: actionError(error, t) })
      // 409/404 说明行状态已被别处改变（例如用户刚兑换完），必须重查，
      // 否则界面会继续展示过期状态并允许重复操作。
      if (error instanceof ApiError && (error.kind === 'conflict' || error.kind === 'not_found')) {
        void invalidate()
      }
    },
  })

  const remove = useMutation({
    mutationFn: async (row: RedemptionCode) => {
      await apiClient.DELETE('/api/redemption-codes/{id}', {
        params: { path: { id: row.id } },
      })
    },
    retry: false,
    onSuccess: async () => {
      push({ tone: 'success', message: t('admin.redemption.delete_success') })
      setDeleteTarget(null)
      await invalidate()
    },
    onError: (error) => {
      push({ tone: 'error', message: actionError(error, t) })
      if (error instanceof ApiError && (error.kind === 'conflict' || error.kind === 'not_found')) {
        setDeleteTarget(null)
        void invalidate()
      }
    },
  })

  const batchRemove = useMutation({
    mutationFn: async (ids: number[]) => {
      return requireData(
        await apiClient.POST('/api/redemption-codes/batch-delete', { body: { ids } }),
      )
    },
    retry: false,
    onSuccess: async (result) => {
      if (result.failed.length === 0) {
        push({
          tone: 'success',
          message: t('admin.redemption.bulk_delete_success', { count: result.succeeded }),
        })
      } else {
        // 后端逐条删除、失败项不中断其余项；把失败数量如实播报，而不是
        // 用「全部成功」掩盖部分失败（已使用的码按审计要求保留）。
        push({
          tone: 'error',
          message: t('admin.redemption.bulk_delete_partial', { count: result.failed.length }),
        })
      }
      setBulkDeleteOpen(false)
      setSelected(new Set())
      await invalidate()
    },
    onError: (error) => {
      push({ tone: 'error', message: actionError(error, t) })
    },
  })

  const data = listQuery.data
  const items = data?.items ?? []
  const total = data?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / pageSize))

  // offset 分页在删除后可能越过末页；回退到最后一页（web-00 §5.5）。
  useEffect(() => {
    if (!listQuery.isFetching && data !== undefined && search.p > pages) {
      onSearchChange({ ...search, p: pages }, { replace: true })
    }
  }, [listQuery.isFetching, data, search, pages, onSearchChange])

  // 换页/换筛选后上一批选中项已不在视野内，继续保留会让「删除选中」
  // 作用到看不见的行上。依赖用序列化后的原始值而不是 search 对象：
  // useSearch() 每次渲染可能返回新对象，直接依赖会让 effect 反复清空选择。
  const selectionKey = [
    search.p,
    search.status ?? '',
    search.batch ?? '',
    search.min ?? '',
    search.max ?? '',
    pageSize,
  ].join('|')
  useEffect(() => {
    setSelected(new Set())
  }, [selectionKey])

  function applyFilters(): void {
    const next: RedemptionSearch = { p: 1 }
    if (draft.status !== '') next.status = draft.status
    const batch = draft.batch.trim().slice(0, 20)
    if (batch !== '') next.batch = batch
    const min = parseNonNegative(draft.min)
    const max = parseNonNegative(draft.max)
    if (min !== undefined) next.min = min
    if (max !== undefined) next.max = max
    onSearchChange(next)
  }

  function resetFilters(): void {
    setDraft(EMPTY_DRAFT)
    onSearchChange({ p: 1 })
  }

  function toggleSelected(id: number, checked: boolean): void {
    setSelected((current) => {
      const next = new Set(current)
      if (checked) next.add(id)
      else next.delete(id)
      return next
    })
  }

  const allOnPageSelected =
    items.length > 0 && items.every((row) => selected.has(row.id))
  const selectedIds = [...selected]

  if (!wideEnough) {
    return (
      <div className="mx-auto max-w-[1440px] rounded-xl border border-border-hairline bg-surface-card p-8">
        <h1 className="text-xl font-semibold text-ink-1">{t('admin.redemption.title')}</h1>
        <p className="mt-2 text-sm text-ink-2">{t('admin.redemption.desktop_only')}</p>
      </div>
    )
  }

  return (
    <div className="mx-auto flex max-w-[1440px] flex-col gap-3">
      <div className="flex items-center justify-between">
        <h1 className="text-xl font-semibold text-ink-1">{t('admin.redemption.title')}</h1>
        <Button onClick={() => setCreateOpen(true)}>{t('admin.redemption.generate')}</Button>
      </div>

      <section className="rounded-xl border border-border-hairline bg-surface-subtle p-4">
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
          <div className="flex flex-col gap-1.5">
            <label htmlFor="redemption-filter-status" className="text-[13px] font-medium text-ink-1">
              {t('admin.redemption.filter_status')}
            </label>
            <select
              id="redemption-filter-status"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              value={draft.status}
              onChange={(event) => {
                setDraft((current) => ({
                  ...current,
                  status: event.target.value as FilterDraft['status'],
                }))
              }}
            >
              <option value="">{t('admin.redemption.filter_all')}</option>
              {(['unused', 'disabled', 'used', 'expired'] as const).map((status) => (
                <option key={status} value={status}>
                  {t(statusKey(status))}
                </option>
              ))}
            </select>
          </div>
          <TextField
            label={t('admin.redemption.filter_batch')}
            maxLength={20}
            value={draft.batch}
            onChange={(event) => {
              setDraft((current) => ({ ...current, batch: event.target.value }))
            }}
          />
          <TextField
            label={t('admin.redemption.filter_min')}
            inputMode="numeric"
            value={draft.min}
            onChange={(event) => {
              setDraft((current) => ({ ...current, min: event.target.value }))
            }}
          />
          <TextField
            label={t('admin.redemption.filter_max')}
            inputMode="numeric"
            value={draft.max}
            onChange={(event) => {
              setDraft((current) => ({ ...current, max: event.target.value }))
            }}
          />
        </div>
        <div className="mt-3 flex justify-end gap-2">
          <Button variant="secondary" onClick={resetFilters}>
            {t('admin.redemption.reset')}
          </Button>
          <Button onClick={applyFilters}>{t('admin.redemption.search')}</Button>
        </div>
      </section>

      <section className="rounded-xl border border-border-hairline bg-surface-card">
        <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border-hairline px-5 py-3">
          <div className="flex items-center gap-3">
            <span className="text-[13px] text-ink-2">{t('admin.redemption.total', { count: total })}</span>
            <span aria-live="polite" className="text-[13px] text-ink-2">
              {selectedIds.length > 0
                ? t('admin.redemption.selected', { count: selectedIds.length })
                : ''}
            </span>
          </div>
          <Button
            variant="danger"
            className="h-8 px-3 text-[13px]"
            disabled={selectedIds.length === 0 || batchRemove.isPending}
            onClick={() => setBulkDeleteOpen(true)}
          >
            {t('admin.redemption.delete_selected', { count: selectedIds.length })}
          </Button>
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
                : t('admin.redemption.error')}
            </span>
            <Button variant="secondary" onClick={() => void listQuery.refetch()}>
              {t('common.action.retry')}
            </Button>
          </div>
        ) : items.length === 0 ? (
          <div className="flex flex-col items-start gap-2 p-5">
            <p className="text-sm text-ink-2">
              {hasRedemptionFilters(search)
                ? t('admin.redemption.empty_filtered')
                : t('admin.redemption.empty')}
            </p>
            {hasRedemptionFilters(search) ? (
              <Button variant="secondary" onClick={resetFilters}>
                {t('admin.redemption.clear_filters')}
              </Button>
            ) : (
              <Button onClick={() => setCreateOpen(true)}>{t('admin.redemption.generate')}</Button>
            )}
          </div>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[1120px] text-sm">
              <thead>
                <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
                  <th scope="col" className="px-4 py-2">
                    <input
                      type="checkbox"
                      aria-label={t('admin.redemption.select_all')}
                      checked={allOnPageSelected}
                      onChange={(event) => {
                        setSelected(
                          event.target.checked
                            ? new Set(items.map((row) => row.id))
                            : new Set(),
                        )
                      }}
                    />
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">{t('admin.redemption.column_id')}</th>
                  <th scope="col" className="px-4 py-2 font-medium">{t('admin.redemption.column_batch')}</th>
                  <th scope="col" className="px-4 py-2 font-medium">{t('admin.redemption.column_status')}</th>
                  <th scope="col" className="px-4 py-2 font-medium">{t('admin.redemption.column_code')}</th>
                  <th scope="col" className="px-4 py-2 text-right font-medium">{t('admin.redemption.column_credits')}</th>
                  <th scope="col" className="px-4 py-2 font-medium">{t('admin.redemption.column_created')}</th>
                  <th scope="col" className="px-4 py-2 font-medium">{t('admin.redemption.column_expires')}</th>
                  <th scope="col" className="px-4 py-2 font-medium">{t('admin.redemption.column_used_by')}</th>
                  <th scope="col" className="px-4 py-2 font-medium">{t('admin.redemption.column_used_at')}</th>
                  <th scope="col" className="px-4 py-2 text-right font-medium">{t('admin.redemption.actions')}</th>
                </tr>
              </thead>
              <tbody>
                {items.map((row) => {
                  const terminal = row.status === 'used' || row.status === 'expired'
                  return (
                    <tr key={row.id} className="border-b border-border-hairline last:border-0 hover:bg-surface-hover">
                      <td className="px-4 py-3">
                        <input
                          type="checkbox"
                          aria-label={t('admin.redemption.select_row', { id: row.id })}
                          checked={selected.has(row.id)}
                          onChange={(event) => toggleSelected(row.id, event.target.checked)}
                        />
                      </td>
                      <td className="px-4 py-3 font-mono tabular-nums text-ink-1">{row.id}</td>
                      <td className="px-4 py-3 text-ink-1">{row.batch_name ?? '—'}</td>
                      <td className="px-4 py-3">
                        <StatusBadge variant={STATUS_VARIANTS[row.status]}>
                          {t(statusKey(row.status))}
                        </StatusBadge>
                      </td>
                      <td className="px-4 py-3 font-mono text-ink-2">{row.code_mask}</td>
                      <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                        {formatCredits(row.credits, locale)}
                      </td>
                      <td className="px-4 py-3 text-xs text-ink-2" title={row.created_at}>
                        {formatRelativeTime(row.created_at, locale)}
                      </td>
                      <td className="px-4 py-3 text-xs text-ink-2" title={row.expires_at ?? undefined}>
                        {row.expires_at === null || row.expires_at === undefined
                          ? t('admin.redemption.never_expires')
                          : formatRelativeTime(row.expires_at, locale)}
                      </td>
                      <td className="px-4 py-3 font-mono tabular-nums text-ink-2">
                        {row.used_by === null || row.used_by === undefined ? '—' : `#${row.used_by}`}
                      </td>
                      <td className="px-4 py-3 text-xs text-ink-2" title={row.used_at ?? undefined}>
                        {row.used_at === null || row.used_at === undefined
                          ? '—'
                          : formatRelativeTime(row.used_at, locale)}
                      </td>
                      <td className="px-4 py-3">
                        <div className="flex justify-end gap-1">
                          <Button
                            variant="ghost"
                            className="h-8 px-2 text-[13px]"
                            disabled={terminal || setStatus.isPending}
                            title={terminal ? t('admin.redemption.terminal_row') : undefined}
                            onClick={() => {
                              // 禁用/启用是可逆的状态切换，按设计不弹二次确认
                              // （modules-04 UI §7.4，与基线一致）；真正不可撤销
                              // 的删除才走 ConfirmDialog。
                              setStatus.mutate({
                                row,
                                status: row.status === 'unused' ? 'disabled' : 'unused',
                              })
                            }}
                          >
                            {row.status === 'unused'
                              ? t('admin.redemption.disable')
                              : t('admin.redemption.enable')}
                          </Button>
                          <Button
                            variant="ghost"
                            className="h-8 px-2 text-[13px] text-danger hover:bg-danger-bg"
                            disabled={terminal}
                            title={terminal ? t('admin.redemption.terminal_row') : undefined}
                            onClick={() => setDeleteTarget(row)}
                          >
                            {t('admin.redemption.delete')}
                          </Button>
                        </div>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}

        <div className="flex items-center justify-end gap-3 border-t border-border-hairline px-5 py-3 text-[13px] text-ink-2">
          <label htmlFor="redemption-page-size" className="sr-only">
            {t('admin.redemption.pagination_page_size')}
          </label>
          <select
            id="redemption-page-size"
            className="h-8 rounded-md border border-border-default bg-surface-card px-2 text-xs text-ink-1"
            value={pageSize}
            disabled={listQuery.isFetching}
            onChange={(event) => {
              setPageSize(Number(event.target.value))
              onSearchChange({ ...search, p: 1 }, { replace: true })
            }}
          >
            {PAGE_SIZES.map((size) => (
              <option key={size} value={size}>
                {size}
              </option>
            ))}
          </select>
          <Button
            variant="secondary"
            className="h-8 px-2 text-[13px]"
            disabled={listQuery.isFetching || search.p <= 1}
            onClick={() => onSearchChange({ ...search, p: search.p - 1 }, { replace: true })}
          >
            {t('admin.redemption.pagination_previous')}
          </Button>
          <span className="font-mono tabular-nums">
            {t('admin.redemption.pagination_page', { page: search.p, pages })}
          </span>
          <Button
            variant="secondary"
            className="h-8 px-2 text-[13px]"
            disabled={listQuery.isFetching || search.p >= pages}
            onClick={() => onSearchChange({ ...search, p: search.p + 1 }, { replace: true })}
          >
            {t('admin.redemption.pagination_next')}
          </Button>
        </div>
      </section>

      <CreateCodesSheet
        open={createOpen}
        onOpenChange={setCreateOpen}
        onCreated={(result) => {
          setCreated(result)
          void invalidate()
        }}
      />
      <CreateCodesResultDialog result={created} onClose={() => setCreated(null)} />

      <Dialog
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (!open) setDeleteTarget(null)
        }}
        role="alertdialog"
        title={t('admin.redemption.delete_title')}
        description={t('admin.redemption.delete_body')}
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
              {t('admin.redemption.confirm_delete')}
            </Button>
          </>
        }
      />

      <Dialog
        open={bulkDeleteOpen}
        onOpenChange={setBulkDeleteOpen}
        role="alertdialog"
        title={t('admin.redemption.delete_bulk_title', { count: selectedIds.length })}
        description={t('admin.redemption.delete_bulk_body')}
        footer={
          <>
            <Button variant="secondary" onClick={() => setBulkDeleteOpen(false)}>
              {t('common.action.cancel')}
            </Button>
            <Button
              variant="danger"
              disabled={batchRemove.isPending}
              onClick={() => batchRemove.mutate(selectedIds)}
            >
              {t('admin.redemption.confirm_delete')}
            </Button>
          </>
        }
      />
    </div>
  )
}

function actionError(error: unknown, t: TranslateFn): string {
  if (error instanceof ApiError) {
    if (error.kind === 'conflict') return t('admin.redemption.conflict')
    if (error.kind === 'rate_limited') return t('admin.redemption.rate_limited')
    if (error.kind === 'not_found') return t('admin.redemption.not_found')
  }
  return t('admin.redemption.action_error')
}
