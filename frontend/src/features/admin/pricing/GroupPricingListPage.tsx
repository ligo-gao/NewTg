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

import { GroupPricingMutateSheet } from './GroupPricingMutateSheet'
import { PricingPagination } from './PricingPagination'
import {
  DEFAULT_QUOTA_PER_UNIT,
  formatGroupRatio,
  pricingModeKey,
  type PricingStatus,
} from './pricing-utils'
import { usePricingCapabilities } from './use-pricing-capabilities'

type GroupPricing = components['schemas']['GroupPricing']
type ListQuery = NonNullable<operations['listGroupPricing']['parameters']['query']>

type Tab = 'directory' | 'overrides'

interface OverrideFilters {
  group: string
  keyword: string
  status: '' | 'enabled' | 'disabled'
}

const EMPTY_FILTERS: OverrideFilters = { group: '', keyword: '', status: '' }

interface EditTarget {
  mode: 'create' | 'edit'
  override?: GroupPricing
}

/**
 * F-06-15 / F-06-16 分组定价页。
 *
 * 「分组目录」Tab 只读消费模块 07 的 GET /api/groups（owner F-07-10）；
 * 本页不为目录渲染任何新建/编辑/删除控件。「分组覆盖价」Tab 拥有
 * group_pricing，是本页唯一的写入口。
 */
export function GroupPricingListPage() {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const capabilities = usePricingCapabilities()

  const [tab, setTab] = useState<Tab>('directory')
  const [directoryPage, setDirectoryPage] = useState(1)
  const [directoryPageSize, setDirectoryPageSize] = useState(20)
  const [draft, setDraft] = useState<OverrideFilters>(EMPTY_FILTERS)
  const [applied, setApplied] = useState<OverrideFilters>(EMPTY_FILTERS)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [editTarget, setEditTarget] = useState<EditTarget | null>(null)
  const [disableTarget, setDisableTarget] = useState<GroupPricing | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<GroupPricing | null>(null)

  const directoryQuery = useQuery({
    queryKey: ['admin-pricing-groups', 'directory', directoryPage, directoryPageSize],
    enabled: capabilities.canRead && tab === 'directory',
    placeholderData: keepPreviousData,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/groups', {
          params: {
            query: { p: directoryPage, page_size: directoryPageSize },
          },
        }),
      ),
  })

  // 覆盖价筛选与新建抽屉共用的选项源。GroupPricingMutateSheet 使用同一个
  // key，因此打开抽屉时复用缓存，不会重复请求。
  const groupOptionsQuery = useQuery({
    queryKey: ['admin-pricing-groups', 'options'],
    enabled: capabilities.canRead,
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/groups', {
          params: { query: { p: 1, page_size: 100 } },
        }),
      ),
    staleTime: 30_000,
  })

  const overridesQuery = useQuery({
    queryKey: ['admin-group-pricing', applied, page, pageSize],
    enabled: capabilities.canRead && tab === 'overrides',
    placeholderData: keepPreviousData,
    queryFn: async () => {
      const query: ListQuery = { p: page, page_size: pageSize, sort: '-updated_at' }
      if (applied.group !== '') query.group = applied.group
      if (applied.keyword.trim() !== '') query.keyword = applied.keyword.trim()
      if (applied.status !== '') query.status = applied.status
      return requireData(
        await apiClient.GET('/api/group-pricing', { params: { query } }),
      )
    },
  })

  const setStatus = useMutation({
    mutationFn: async ({
      row,
      status,
    }: {
      row: GroupPricing
      status: PricingStatus
    }) => {
      await apiClient.PATCH('/api/group-pricing/{group}/{model}', {
        params: { path: { group: row.group, model: row.model } },
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
      await queryClient.invalidateQueries({ queryKey: ['admin-group-pricing'] })
    },
    onError: (error) => {
      const conflict = error instanceof ApiError && error.kind === 'conflict'
      push({
        tone: 'error',
        message: conflict ? t('admin.pricing.conflict') : t('admin.pricing.action_error'),
      })
      if (conflict) {
        void queryClient.invalidateQueries({ queryKey: ['admin-group-pricing'] })
      }
    },
  })

  const remove = useMutation({
    mutationFn: async (row: GroupPricing) => {
      // 与模型定价相同的转义规则：openapi-fetch 会编码查询值，带 +08:00
      // 偏移的时间戳能完整到达服务端（绝不手工拼字符串）。
      await apiClient.DELETE('/api/group-pricing/{group}/{model}', {
        params: {
          path: { group: row.group, model: row.model },
          query: { updated_at: row.updated_at },
        },
      })
    },
    onSuccess: async () => {
      push({ tone: 'success', message: t('admin.pricing.deleted_success') })
      setDeleteTarget(null)
      await queryClient.invalidateQueries({ queryKey: ['admin-group-pricing'] })
    },
    onError: (error) => {
      const conflict = error instanceof ApiError && error.kind === 'conflict'
      push({
        tone: 'error',
        message: conflict ? t('admin.pricing.conflict') : t('admin.pricing.action_error'),
      })
      if (conflict) {
        void queryClient.invalidateQueries({ queryKey: ['admin-group-pricing'] })
      }
    },
  })

  const directory = directoryQuery.data
  const directoryItems = directory?.items ?? []
  const directoryTotal = directory?.total ?? 0
  const directoryPages = Math.max(1, Math.ceil(directoryTotal / directoryPageSize))
  const hasNoCustomGroups =
    directoryItems.length > 0 && directoryItems.every((group) => group.is_system)

  const overrides = overridesQuery.data
  const items = overrides?.items ?? []
  const total = overrides?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / pageSize))
  const quotaPerUnit = overrides?.quota_per_unit ?? DEFAULT_QUOTA_PER_UNIT

  useEffect(() => {
    if (!overridesQuery.isFetching && total > 0 && page > pages) setPage(pages)
  }, [overridesQuery.isFetching, total, page, pages])

  useEffect(() => {
    if (!directoryQuery.isFetching && directoryTotal > 0 && directoryPage > directoryPages) {
      setDirectoryPage(directoryPages)
    }
  }, [directoryQuery.isFetching, directoryTotal, directoryPage, directoryPages])

  const hasFilters =
    applied.group !== '' || applied.keyword.trim() !== '' || applied.status !== ''

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
      <h1 className="text-xl font-semibold text-ink-1">{t('admin.pricing.group_title')}</h1>

      <div role="tablist" aria-label={t('admin.pricing.group_title')} className="flex gap-1">
        <Button
          role="tab"
          aria-selected={tab === 'directory'}
          variant={tab === 'directory' ? 'secondary' : 'ghost'}
          onClick={() => setTab('directory')}
        >
          {t('admin.pricing.group_tab_directory')}
        </Button>
        <Button
          role="tab"
          aria-selected={tab === 'overrides'}
          variant={tab === 'overrides' ? 'secondary' : 'ghost'}
          onClick={() => setTab('overrides')}
        >
          {t('admin.pricing.group_tab_overrides')}
        </Button>
      </div>

      {tab === 'directory' ? (
        <section className="rounded-xl border border-border-hairline bg-surface-card">
          <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border-hairline px-5 py-3">
            <div>
              <h2 className="text-sm font-semibold text-ink-1">
                {t('admin.pricing.group_tab_directory')}
              </h2>
              <p className="text-xs text-ink-3">{t('admin.pricing.directory_note')}</p>
            </div>
            <a
              href="/admin/users/groups"
              className="text-[13px] font-medium text-brand hover:underline"
            >
              {t('admin.pricing.manage_groups')}
            </a>
          </div>

          {directoryQuery.isPending ? (
            <div className="space-y-2 p-5" aria-busy="true">
              {[0, 1, 2, 3, 4].map((index) => (
                <div key={index} className="h-10 animate-pulse rounded bg-surface-subtle" />
              ))}
            </div>
          ) : directoryQuery.isError ? (
            <div className="flex items-center gap-3 p-5 text-sm text-ink-2">
              <span>
                {directoryQuery.error instanceof ApiError &&
                directoryQuery.error.kind === 'forbidden'
                  ? t('common.error403.body')
                  : t('admin.pricing.group_load_error')}
              </span>
              <Button variant="secondary" onClick={() => void directoryQuery.refetch()}>
                {t('common.action.retry')}
              </Button>
            </div>
          ) : directoryItems.length === 0 ? (
            <p className="p-5 text-sm text-ink-2">{t('admin.pricing.directory_empty')}</p>
          ) : (
            <>
              {hasNoCustomGroups ? (
                <div className="flex items-center justify-between gap-2 border-b border-border-hairline px-5 py-2 text-xs text-ink-3">
                  <span>{t('admin.pricing.directory_empty')}</span>
                  <a href="/admin/users/groups" className="text-brand hover:underline">
                    {t('admin.pricing.manage_groups')}
                  </a>
                </div>
              ) : null}
              <div className="overflow-x-auto">
                <table className="w-full min-w-[760px] text-sm">
                  <thead>
                    <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
                      <th scope="col" className="px-4 py-2 font-medium">
                        {t('admin.pricing.group_name')}
                      </th>
                      <th scope="col" className="px-4 py-2 text-right font-medium">
                        {t('admin.pricing.group_ratio')}
                      </th>
                      <th scope="col" className="px-4 py-2 font-medium">
                        {t('admin.pricing.user_selectable')}
                      </th>
                      <th scope="col" className="px-4 py-2 text-right font-medium">
                        {t('admin.pricing.enabled_channels')}
                      </th>
                      <th scope="col" className="px-4 py-2 text-right font-medium">
                        {t('admin.pricing.users_count')}
                      </th>
                      <th scope="col" className="px-4 py-2 font-medium">
                        {t('admin.pricing.column_status')}
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {directoryItems.map((group) => (
                      <tr
                        key={group.name}
                        className="border-b border-border-hairline last:border-0 hover:bg-surface-hover"
                      >
                        <td className="px-4 py-3">
                          <span className="font-mono text-ink-1">{group.name}</span>{' '}
                          {group.is_system ? (
                            <Tag variant="neutral">{t('admin.pricing.system')}</Tag>
                          ) : null}
                        </td>
                        <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                          {formatGroupRatio(group.ratio)}
                        </td>
                        <td className="px-4 py-3 text-ink-2">
                          {group.user_selectable
                            ? t('admin.pricing.yes')
                            : t('admin.pricing.no')}
                        </td>
                        <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                          {formatCredits(group.channels_count)}
                        </td>
                        <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                          {formatCredits(group.users_count)}
                        </td>
                        <td className="px-4 py-3">
                          <StatusBadge variant={group.status === 1 ? 'success' : 'neutral'}>
                            {group.status === 1
                              ? t('admin.pricing.status_enabled')
                              : t('admin.pricing.status_disabled')}
                          </StatusBadge>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </>
          )}

          <PricingPagination
            page={directoryPage}
            pages={directoryPages}
            pageSize={directoryPageSize}
            disabled={directoryQuery.isFetching}
            onPageChange={setDirectoryPage}
            onPageSizeChange={(size) => {
              setDirectoryPageSize(size)
              setDirectoryPage(1)
            }}
          />
        </section>
      ) : (
        <>
          <section className="rounded-xl border border-border-hairline bg-surface-subtle p-4">
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
              <div className="flex flex-col gap-1.5">
                <label
                  htmlFor="override-filter-group"
                  className="text-[13px] font-medium text-ink-1"
                >
                  {t('admin.pricing.group')}
                </label>
                <select
                  id="override-filter-group"
                  className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                  value={draft.group}
                  onChange={(event) => {
                    setDraft((current) => ({ ...current, group: event.target.value }))
                  }}
                >
                  <option value="">{t('admin.pricing.all')}</option>
                  {(groupOptionsQuery.data?.items ?? []).map((group) => (
                    <option key={group.name} value={group.name}>
                      {group.name}
                    </option>
                  ))}
                </select>
              </div>
              <TextField
                label={t('admin.pricing.keyword')}
                value={draft.keyword}
                maxLength={128}
                onChange={(event) => {
                  setDraft((current) => ({ ...current, keyword: event.target.value }))
                }}
              />
              <div className="flex flex-col gap-1.5">
                <label
                  htmlFor="override-filter-status"
                  className="text-[13px] font-medium text-ink-1"
                >
                  {t('admin.pricing.status')}
                </label>
                <select
                  id="override-filter-status"
                  className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                  value={draft.status}
                  onChange={(event) => {
                    setDraft((current) => ({
                      ...current,
                      status: event.target.value as OverrideFilters['status'],
                    }))
                  }}
                >
                  <option value="">{t('admin.pricing.all')}</option>
                  <option value="enabled">{t('admin.pricing.status_enabled')}</option>
                  <option value="disabled">{t('admin.pricing.status_disabled')}</option>
                </select>
              </div>
            </div>
            <div className="mt-3 flex justify-end gap-2">
              <Button
                variant="secondary"
                onClick={() => {
                  setDraft(EMPTY_FILTERS)
                  setApplied(EMPTY_FILTERS)
                  setPage(1)
                }}
              >
                {t('admin.pricing.reset')}
              </Button>
              <Button
                onClick={() => {
                  setApplied(draft)
                  setPage(1)
                }}
              >
                {t('admin.pricing.search')}
              </Button>
            </div>
          </section>

          <section className="rounded-xl border border-border-hairline bg-surface-card">
            <div className="flex items-center justify-between border-b border-border-hairline px-5 py-3">
              {capabilities.canManage ? (
                <Button onClick={() => setEditTarget({ mode: 'create' })}>
                  {t('admin.pricing.override_new')}
                </Button>
              ) : (
                <span />
              )}
              <span className="text-[13px] text-ink-2">
                {t('admin.pricing.total', { count: total })}
              </span>
            </div>

            {overridesQuery.isPending ? (
              <div className="space-y-2 p-5" aria-busy="true">
                {[0, 1, 2, 3, 4].map((index) => (
                  <div key={index} className="h-10 animate-pulse rounded bg-surface-subtle" />
                ))}
              </div>
            ) : overridesQuery.isError ? (
              <div className="flex items-center gap-3 p-5 text-sm text-ink-2">
                <span>{t('admin.pricing.override_error')}</span>
                <Button variant="secondary" onClick={() => void overridesQuery.refetch()}>
                  {t('common.action.retry')}
                </Button>
              </div>
            ) : items.length === 0 ? (
              <div className="flex flex-col items-start gap-2 p-5">
                <p className="text-sm text-ink-2">
                  {hasFilters
                    ? t('admin.pricing.empty_filtered')
                    : t('admin.pricing.override_empty')}
                </p>
                {hasFilters ? (
                  <Button
                    variant="secondary"
                    onClick={() => {
                      setDraft(EMPTY_FILTERS)
                      setApplied(EMPTY_FILTERS)
                      setPage(1)
                    }}
                  >
                    {t('admin.pricing.clear_filters')}
                  </Button>
                ) : null}
              </div>
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full min-w-[1080px] text-sm">
                  <thead>
                    <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
                      <th scope="col" rowSpan={2} className="px-4 py-2 font-medium">
                        {t('admin.pricing.group')}
                      </th>
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
                          key={`${row.group}/${row.model}`}
                          className="border-b border-border-hairline last:border-0 hover:bg-surface-hover"
                        >
                          <td className="px-4 py-3 font-mono text-ink-1">{row.group}</td>
                          <td className="px-4 py-3">
                            <span className="font-mono text-ink-1">{row.model}</span>{' '}
                            {row.source_status === 2 ? (
                              <Tag variant="warning">{t('admin.pricing.source_disabled')}</Tag>
                            ) : row.source_status === 0 ? (
                              <Tag variant="danger">{t('admin.pricing.source_missing')}</Tag>
                            ) : null}
                          </td>
                          <td className="px-4 py-3">
                            <Tag>{t(pricingModeKey(row.mode))}</Tag>
                          </td>
                          <td className="border-l border-border-hairline px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                            {formatCredits(row.prompt_price)}
                          </td>
                          <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                            {perRequest ? '—' : formatCredits(row.completion_price)}
                          </td>
                          <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                            {perRequest ? '—' : formatCredits(row.cache_read_price)}
                          </td>
                          <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                            {perRequest ? '—' : formatCredits(row.cache_write_price)}
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
                                  onClick={() =>
                                    setEditTarget({ mode: 'edit', override: row })
                                  }
                                >
                                  {t('admin.pricing.edit')}
                                </Button>
                                <Button
                                  variant="ghost"
                                  className="h-8 px-2 text-[13px]"
                                  disabled={setStatus.isPending}
                                  onClick={() => {
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
              disabled={overridesQuery.isFetching}
              onPageChange={setPage}
              onPageSizeChange={(size) => {
                setPageSize(size)
                setPage(1)
              }}
            />
          </section>
        </>
      )}

      <GroupPricingMutateSheet
        open={editTarget !== null}
        mode={editTarget?.mode ?? 'create'}
        override={editTarget?.override}
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
        title={t('admin.pricing.override_delete_title')}
        description={t('admin.pricing.override_delete_body')}
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
