import { useState } from 'react'
import {
  keepPreviousData,
  useMutation,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query'

import { ApiError } from '@/api/errors'
import {
  createIdempotencyKey,
  deleteAnnouncement,
  listAnnouncements,
  offlineAnnouncement,
  publishAnnouncement,
  type AnnouncementSummary,
} from '@/features/announcements/api'
import {
  ANNOUNCEMENT_ACTIVE_QUERY_KEY,
  ANNOUNCEMENT_LIST_QUERY_KEY,
  announcementDetailQueryKey,
} from '@/features/announcements/query-keys'
import { useI18n } from '@/shared/lib/i18n-context'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'
import { StatusBadge } from '@/shared/ui/status-badge'
import { Tag } from '@/shared/ui/tag'
import { useToastStore } from '@/shared/ui/toast'

import { AnnouncementDrawer } from './announcement-drawer'
import {
  ANNOUNCEMENT_CATEGORIES,
  ANNOUNCEMENT_PAGE_SIZES,
  ANNOUNCEMENT_STATUS_FILTERS,
  DEFAULT_ANNOUNCEMENT_PAGE_SIZE,
  type AnnouncementsSearch,
} from './search'
import { useOnlineStatus } from './use-online'
import {
  formatDateTime,
  statusBadgeVariant,
} from './utils'

export interface AnnouncementsPageProps {
  search: AnnouncementsSearch
  onSearchChange: (patch: Partial<AnnouncementsSearch>) => void
}

/**
 * 公告管理页（UI §6.1.1、F-08-04）。
 *
 * 独立列表页，不进 Settings Shell：站点设置页只提供入口按钮。读写均由
 * `settings:manage` 收口（与站点设置同源，不新增公告专用权限键）；路由
 * 层已做 403 守卫，这里只负责六态与状态机呈现。
 */
export function AnnouncementsPage({
  search,
  onSearchChange,
}: AnnouncementsPageProps) {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const online = useOnlineStatus()

  const pageSize = search.page_size ?? DEFAULT_ANNOUNCEMENT_PAGE_SIZE
  const page = search.p ?? 1

  const [drawerOpen, setDrawerOpen] = useState(false)
  const [editingId, setEditingId] = useState<string | null>(null)
  const [publishTarget, setPublishTarget] = useState<AnnouncementSummary | null>(
    null,
  )
  const [deleteTarget, setDeleteTarget] = useState<AnnouncementSummary | null>(
    null,
  )

  const listQuery = useQuery({
    queryKey: [...ANNOUNCEMENT_LIST_QUERY_KEY, search],
    queryFn: ({ signal }) =>
      listAnnouncements(
        {
          p: page,
          page_size: pageSize,
          status: search.status,
          category: search.category,
        },
        signal,
      ),
    // 翻页时保留上一页，避免表格整块闪成骨架。
    placeholderData: keepPreviousData,
  })

  async function invalidate(): Promise<void> {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ANNOUNCEMENT_LIST_QUERY_KEY }),
      // 管理端动作会改变用户可见集合，弹层缓存不能继续持有旧快照。
      queryClient.invalidateQueries({ queryKey: ANNOUNCEMENT_ACTIVE_QUERY_KEY }),
    ])
  }

  function actionError(error: unknown): string {
    if (error instanceof ApiError) {
      if (error.kind === 'offline') return t('admin.announcements.error.offline')
      if (error.kind === 'rate_limited') {
        return error.retry_after !== undefined
          ? t('admin.announcements.rate_limited_retry', {
              seconds: error.retry_after,
            })
          : t('admin.announcements.rate_limited')
      }
      if (error.kind === 'not_found') {
        return t('admin.announcements.error.not_found')
      }
      if (error.kind === 'conflict') {
        return t('admin.announcements.error.conflict')
      }
    }
    return t('admin.announcements.error.action')
  }

  const publishMutation = useMutation({
    // 一次用户意图一个 key；仅 CSRF 重放复用。
    mutationFn: (row: AnnouncementSummary) =>
      publishAnnouncement(row.id, createIdempotencyKey()),
    retry: false,
    onSuccess: async (detail) => {
      // 服务端返回值是列表投影之外的权威详情；写回缓存保证随后打开编辑
      // 抽屉时看到的是发布后的状态，而不是 staleTime: Infinity 的旧快照。
      queryClient.setQueryData(announcementDetailQueryKey(detail.id), detail)
      push({ tone: 'success', message: t('admin.announcements.toast.published') })
      setPublishTarget(null)
      await invalidate()
    },
    onError: (error) => {
      push({ tone: 'error', message: actionError(error) })
      if (
        error instanceof ApiError &&
        (error.kind === 'not_found' || error.kind === 'conflict')
      ) {
        setPublishTarget(null)
        void invalidate()
      }
    },
  })

  const offlineMutation = useMutation({
    // 一次用户意图一个 key；仅 CSRF 重放复用。
    mutationFn: (row: AnnouncementSummary) =>
      offlineAnnouncement(row.id, createIdempotencyKey()),
    retry: false,
    onSuccess: async (detail) => {
      queryClient.setQueryData(announcementDetailQueryKey(detail.id), detail)
      push({ tone: 'success', message: t('admin.announcements.toast.offline') })
      await invalidate()
    },
    onError: (error) => {
      push({ tone: 'error', message: actionError(error) })
      if (
        error instanceof ApiError &&
        (error.kind === 'not_found' || error.kind === 'conflict')
      ) {
        void invalidate()
      }
    },
  })

  const deleteMutation = useMutation({
    mutationFn: (row: AnnouncementSummary) => deleteAnnouncement(row.id),
    retry: false,
    onSuccess: async (_data, row) => {
      // 详情缓存指向已删除实体，直接移除而不是留着等一次必然 404 的重取。
      queryClient.removeQueries({ queryKey: announcementDetailQueryKey(row.id) })
      push({ tone: 'success', message: t('admin.announcements.toast.deleted') })
      setDeleteTarget(null)
      await invalidate()
      // 删除本页最后一行后，当前页码可能已不存在（例如第 2 页只有 1 行）；
      // 回退一页，避免列表空白却停在越界页码。
      if (rows.length === 1 && page > 1) {
        onSearchChange({ p: page - 1 })
      }
    },
    onError: (error) => {
      push({ tone: 'error', message: actionError(error) })
      if (
        error instanceof ApiError &&
        (error.kind === 'not_found' || error.kind === 'conflict')
      ) {
        setDeleteTarget(null)
        void invalidate()
      }
    },
  })

  const busy =
    publishMutation.isPending ||
    offlineMutation.isPending ||
    deleteMutation.isPending
  const writesDisabled = !online || busy

  const rows = listQuery.data?.items ?? []
  const total = listQuery.data?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / pageSize))

  function openCreate(): void {
    setEditingId(null)
    setDrawerOpen(true)
  }

  function openEdit(row: AnnouncementSummary): void {
    setEditingId(row.id)
    setDrawerOpen(true)
  }

  function listErrorMessage(): string {
    const error = listQuery.error
    if (error instanceof ApiError) {
      if (error.kind === 'offline') return t('admin.announcements.error.offline')
      if (error.kind === 'rate_limited') {
        return error.retry_after !== undefined
          ? t('admin.announcements.rate_limited_retry', {
              seconds: error.retry_after,
            })
          : t('admin.announcements.rate_limited')
      }
    }
    return t('admin.announcements.error.load')
  }

  return (
    <div className="mx-auto flex max-w-[1520px] flex-col gap-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-page-title font-semibold text-ink-1">
            {t('admin.announcements.page.title')}
          </h1>
          <p className="mt-1 text-body text-ink-2">
            {t('admin.announcements.page.subtitle')}
          </p>
        </div>
        <Button
          disabled={writesDisabled}
          title={!online ? t('admin.announcements.page.offline_write_disabled') : undefined}
          onClick={openCreate}
        >
          {t('admin.announcements.page.create')}
        </Button>
      </header>

      {!online ? (
        <FormAlert tone="warning">
          {t('admin.announcements.page.offline_snapshot')}
        </FormAlert>
      ) : null}

      <div className="flex flex-wrap items-end gap-3">
        <div className="flex flex-col gap-1.5">
          <label
            htmlFor="announcement-status-filter"
            className="text-meta font-medium text-ink-1"
          >
            {t('admin.announcements.filter.status_label')}
          </label>
          <select
            id="announcement-status-filter"
            className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            value={search.status ?? ''}
            onChange={(event) => {
              const value = event.target.value
              onSearchChange({
                status:
                  value === ''
                    ? undefined
                    : (value as AnnouncementSummary['display_status']),
                // 换筛选必须回到第一页，否则可能落在不存在的页码上。
                p: 1,
              })
            }}
          >
            <option value="">{t('admin.announcements.filter.status_all')}</option>
            {ANNOUNCEMENT_STATUS_FILTERS.map((status) => (
              <option key={status} value={status}>
                {t(`admin.announcements.status.${status}`)}
              </option>
            ))}
          </select>
        </div>

        <div className="flex flex-col gap-1.5">
          <label
            htmlFor="announcement-category-filter"
            className="text-meta font-medium text-ink-1"
          >
            {t('admin.announcements.filter.category_label')}
          </label>
          <select
            id="announcement-category-filter"
            className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            value={search.category ?? ''}
            onChange={(event) => {
              const value = event.target.value
              onSearchChange({
                category:
                  value === ''
                    ? undefined
                    : (value as AnnouncementSummary['category']),
                p: 1,
              })
            }}
          >
            <option value="">{t('admin.announcements.filter.category_all')}</option>
            {ANNOUNCEMENT_CATEGORIES.map((category) => (
              <option key={category} value={category}>
                {t(`admin.announcements.category.${category}`)}
              </option>
            ))}
          </select>
        </div>
      </div>

      {listQuery.isError ? (
        <div className="flex flex-wrap items-center gap-3" data-testid="announcement-list-error">
          <FormAlert tone="danger">{listErrorMessage()}</FormAlert>
          <Button
            variant="secondary"
            onClick={() => {
              void listQuery.refetch()
            }}
          >
            {t('admin.announcements.error.retry')}
          </Button>
        </div>
      ) : null}

      {listQuery.isPending ? (
        <div>
          <p className="sr-only" role="status">
            {t('admin.announcements.loading')}
          </p>
          <div
            data-testid="announcement-table-skeleton"
            aria-hidden="true"
            className="space-y-2"
          >
            {Array.from({ length: 5 }, (_, index) => (
              <div key={index} className="h-9 w-full rounded bg-surface-subtle" />
            ))}
          </div>
        </div>
      ) : null}

      {listQuery.isSuccess && rows.length === 0 ? (
        <section
          data-testid="announcement-empty"
          className="flex flex-col items-center gap-3 rounded-xl border border-border-default bg-surface-card px-6 py-12 text-center"
        >
          <svg
            aria-hidden="true"
            viewBox="0 0 64 64"
            className="size-12 text-ink-3"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
            strokeLinejoin="round"
          >
            <path d="M12 52V20l22-10v44L12 44v8z" />
            <path d="M34 18c6 4 8 9 8 14s-2 10-8 14" />
            <path d="M44 12c8 6 12 13 12 20s-4 14-12 20" />
          </svg>
          <h2 className="text-card-title font-semibold text-ink-1">
            {t('admin.announcements.empty.title')}
          </h2>
          <p className="max-w-md text-body text-ink-2">
            {t('admin.announcements.empty.body')}
          </p>
          <Button
            disabled={writesDisabled}
            title={!online ? t('admin.announcements.page.offline_write_disabled') : undefined}
            onClick={openCreate}
          >
            {t('admin.announcements.empty.action')}
          </Button>
        </section>
      ) : null}

      {listQuery.isSuccess && rows.length > 0 ? (
        <>
          <div className="overflow-x-auto rounded-xl border border-border-default">
            <table className="w-full text-left text-body">
              <thead className="bg-surface-subtle text-meta text-ink-3">
                <tr>
                  <th scope="col" className="px-3 py-2 font-medium">
                    {t('admin.announcements.columns.title')}
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    {t('admin.announcements.columns.category')}
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    {t('admin.announcements.columns.status')}
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    {t('admin.announcements.columns.publish_at')}
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    {t('admin.announcements.columns.expire_at')}
                  </th>
                  <th scope="col" className="px-3 py-2 font-medium">
                    {t('admin.announcements.columns.actions')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {rows.map((row) => {
                  const display = row.display_status
                  const canEdit =
                    display === 'draft' ||
                    display === 'published' ||
                    display === 'offline'
                  return (
                    <tr
                      key={row.id}
                      data-testid="announcement-row"
                      data-status={display}
                      className="border-t border-border-hairline align-middle"
                    >
                      <td className="max-w-[360px] px-3 py-2">
                        <span className="font-medium text-ink-1">{row.title}</span>
                      </td>
                      <td className="px-3 py-2">
                        <Tag ariaLabel={t('admin.announcements.columns.category')}>
                          {t(`admin.announcements.category.${row.category}`)}
                        </Tag>
                      </td>
                      <td className="px-3 py-2">
                        <StatusBadge variant={statusBadgeVariant(display)}>
                          {t(`admin.announcements.status.${display}`)}
                        </StatusBadge>
                      </td>
                      <td className="px-3 py-2 text-meta text-ink-2">
                        {formatDateTime(row.publish_at, locale)}
                      </td>
                      <td className="px-3 py-2 text-meta text-ink-2">
                        {row.expire_at === null || row.expire_at === undefined
                          ? t('admin.announcements.no_expiry')
                          : formatDateTime(row.expire_at, locale)}
                      </td>
                      <td className="px-3 py-2">
                        <div className="flex flex-wrap items-center gap-1">
                          {canEdit ? (
                            <Button
                              variant="ghost"
                              className="h-7 px-2 text-badge"
                              disabled={writesDisabled}
                              title={!online ? t('admin.announcements.page.offline_write_disabled') : undefined}
                              aria-label={t('admin.announcements.actions.edit_aria', {
                                title: row.title,
                              })}
                              onClick={() => openEdit(row)}
                            >
                              {t('admin.announcements.actions.edit')}
                            </Button>
                          ) : null}
                          {display === 'draft' ? (
                            <Button
                              variant="ghost"
                              className="h-7 px-2 text-badge"
                              disabled={writesDisabled}
                              aria-label={t('admin.announcements.actions.publish_aria', {
                                title: row.title,
                              })}
                              onClick={() => setPublishTarget(row)}
                            >
                              {t('admin.announcements.actions.publish')}
                            </Button>
                          ) : null}
                          {display === 'published' || display === 'expired' ? (
                            <Button
                              variant="ghost"
                              className="h-7 px-2 text-badge"
                              disabled={writesDisabled}
                              aria-label={t('admin.announcements.actions.offline_aria', {
                                title: row.title,
                              })}
                              onClick={() => offlineMutation.mutate(row)}
                            >
                              {t('admin.announcements.actions.offline')}
                            </Button>
                          ) : null}
                          <Button
                            variant="ghost"
                            className="h-7 px-2 text-badge text-danger"
                            disabled={writesDisabled}
                            aria-label={t('admin.announcements.actions.delete_aria', {
                              title: row.title,
                            })}
                            onClick={() => setDeleteTarget(row)}
                          >
                            {t('admin.announcements.actions.delete')}
                          </Button>
                        </div>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>

          <nav
            aria-label={t('admin.announcements.pagination.label')}
            className="flex flex-wrap items-center justify-between gap-3 text-meta text-ink-3"
          >
            <span>
              {t('admin.announcements.pagination.summary', {
                from: total === 0 ? 0 : (page - 1) * pageSize + 1,
                to: Math.min(page * pageSize, total),
                total,
              })}
            </span>
            <span className="flex items-center gap-2">
              <label htmlFor="announcement-page-size" className="sr-only">
                {t('admin.announcements.pagination.page_size')}
              </label>
              <select
                id="announcement-page-size"
                className="h-8 rounded-md border border-border-default bg-surface-card px-2 text-badge text-ink-1"
                value={pageSize}
                onChange={(event) => {
                  onSearchChange({
                    page_size: Number(
                      event.target.value,
                    ) as AnnouncementsSearch['page_size'],
                    p: 1,
                  })
                }}
              >
                {ANNOUNCEMENT_PAGE_SIZES.map((size) => (
                  <option key={size} value={size}>
                    {size}
                  </option>
                ))}
              </select>
              <Button
                variant="secondary"
                className="h-7 px-2 text-badge"
                disabled={page <= 1 || listQuery.isFetching}
                onClick={() => onSearchChange({ p: Math.max(1, page - 1) })}
              >
                {t('admin.announcements.pagination.previous')}
              </Button>
              <span>
                {t('admin.announcements.pagination.page_of', {
                  page,
                  pages,
                })}
              </span>
              <Button
                variant="secondary"
                className="h-7 px-2 text-badge"
                disabled={page >= pages || listQuery.isFetching}
                onClick={() => onSearchChange({ p: page + 1 })}
              >
                {t('admin.announcements.pagination.next')}
              </Button>
            </span>
          </nav>
        </>
      ) : null}

      <AnnouncementDrawer
        open={drawerOpen}
        announcementId={editingId}
        online={online}
        onClose={() => setDrawerOpen(false)}
        onSaved={invalidate}
      />

      <Dialog
        open={publishTarget !== null}
        onOpenChange={(open) => {
          if (!open) setPublishTarget(null)
        }}
        role="alertdialog"
        // 破坏性/立即生效的确认：初始焦点落在「取消」，避免回车误确认。
        initialFocus="first"
        title={t('admin.announcements.confirm.publish_title')}
        description={t('admin.announcements.confirm.publish_body')}
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onClick={() => setPublishTarget(null)}>
              {t('common.action.cancel')}
            </Button>
            <Button
              disabled={writesDisabled}
              onClick={() => {
                if (publishTarget !== null) publishMutation.mutate(publishTarget)
              }}
            >
              {t('admin.announcements.confirm.publish_confirm')}
            </Button>
          </div>
        }
      />

      <Dialog
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (!open) setDeleteTarget(null)
        }}
        role="alertdialog"
        // 删除不可撤销：初始焦点落在「取消」。
        initialFocus="first"
        title={t('admin.announcements.confirm.delete_title')}
        description={t('admin.announcements.confirm.delete_body')}
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onClick={() => setDeleteTarget(null)}>
              {t('common.action.cancel')}
            </Button>
            <Button
              variant="danger"
              disabled={writesDisabled}
              onClick={() => {
                if (deleteTarget !== null) deleteMutation.mutate(deleteTarget)
              }}
            >
              {t('admin.announcements.confirm.delete_confirm')}
            </Button>
          </div>
        }
      />
    </div>
  )
}
