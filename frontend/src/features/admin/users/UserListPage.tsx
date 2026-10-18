import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components, operations } from '@/api/schema'
import { formatCredits } from '@/shared/lib/format'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { StatusBadge } from '@/shared/ui/status-badge'
import { Tag } from '@/shared/ui/tag'
import { TextField } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

import { AccountRecoveryDialogs } from './AccountRecoveryDialogs'
import { AdjustQuotaDialog } from './AdjustQuotaDialog'
import { UserMutateSheet } from './UserMutateSheet'

type AdminUser = components['schemas']['AdminUser']
type UserStatus = components['schemas']['UserStatus']
type UserRole = components['schemas']['UserRole']

interface Filters {
  keyword: string
  status: '' | UserStatus
  role: '' | UserRole
  group: string
}

const EMPTY_FILTERS: Filters = { keyword: '', status: '', role: '', group: '' }
const PAGE_SIZES = [10, 20, 50] as const

interface EditTarget {
  mode: 'create' | 'edit'
  user?: AdminUser
}

/** F-07-01..F-07-03 user list: filters → toolbar → table → offset pagination. */
export function UserListPage() {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)

  const [draft, setDraft] = useState<Filters>(EMPTY_FILTERS)
  const [applied, setApplied] = useState<Filters>(EMPTY_FILTERS)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState<number>(20)
  const [editTarget, setEditTarget] = useState<EditTarget | null>(null)
  const [quotaUser, setQuotaUser] = useState<AdminUser | null>(null)
  const [recoveryUser, setRecoveryUser] = useState<AdminUser | null>(null)
  const [disableUser, setDisableUser] = useState<AdminUser | null>(null)

  const usersQuery = useQuery({
    queryKey: ['admin-users', applied, page, pageSize],
    queryFn: async () => {
      const query: NonNullable<operations['listUsers']['parameters']['query']> = {
        p: page,
        page_size: pageSize,
      }
      if (applied.keyword.trim() !== '') query.keyword = applied.keyword.trim()
      if (applied.status !== '') query.status = applied.status
      if (applied.role !== '') query.role = applied.role
      if (applied.group !== '') query.group = applied.group
      return requireData(await apiClient.GET('/api/users', { params: { query } }))
    },
  })

  const groupsQuery = useQuery({
    queryKey: ['admin-groups', 'filter-options'],
    queryFn: async () =>
      requireData(
        await apiClient.GET('/api/groups', {
          params: { query: { p: 1, page_size: 100 } },
        }),
      ),
  })

  const setStatus = useMutation({
    mutationFn: async ({ id, status }: { id: number; status: UserStatus }) => {
      await apiClient.POST('/api/users/{id}/status', {
        params: { path: { id } },
        body: { status },
      })
    },
    onSuccess: async (_data, variables) => {
      push({
        tone: 'success',
        message:
          variables.status === 'disabled'
            ? t('admin.users.disable_success')
            : t('admin.users.enable_success'),
      })
      await queryClient.invalidateQueries({ queryKey: ['admin-users'] })
    },
    onError: () => {
      push({ tone: 'error', message: t('admin.users.action_error') })
    },
  })

  const items = usersQuery.data?.items ?? []
  const total = usersQuery.data?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / pageSize))

  function applyFilters(): void {
    setApplied(draft)
    setPage(1)
  }

  function resetFilters(): void {
    setDraft(EMPTY_FILTERS)
    setApplied(EMPTY_FILTERS)
    setPage(1)
  }

  return (
    <div className="mx-auto flex max-w-[1440px] flex-col gap-3">
      <h1 className="text-xl font-semibold text-ink-1">{t('admin.users.title')}</h1>
      <section className="rounded-xl border border-border-hairline bg-surface-subtle p-4">
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
          <TextField
            label={t('admin.users.keyword')}
            value={draft.keyword}
            maxLength={64}
            onChange={(event) => {
              setDraft((current) => ({ ...current, keyword: event.target.value }))
            }}
          />
          <div className="flex flex-col gap-1.5">
            <label htmlFor="filter-status" className="text-[13px] font-medium text-ink-1">
              {t('admin.users.status')}
            </label>
            <select
              id="filter-status"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              value={draft.status}
              onChange={(event) => {
                setDraft((current) => ({
                  ...current,
                  status: event.target.value as Filters['status'],
                }))
              }}
            >
              <option value="">{t('admin.users.all')}</option>
              <option value="enabled">{t('admin.users.status_enabled')}</option>
              <option value="disabled">{t('admin.users.status_disabled')}</option>
            </select>
          </div>
          <div className="flex flex-col gap-1.5">
            <label htmlFor="filter-role" className="text-[13px] font-medium text-ink-1">
              {t('admin.users.role')}
            </label>
            <select
              id="filter-role"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              value={draft.role}
              onChange={(event) => {
                setDraft((current) => ({
                  ...current,
                  role: event.target.value as Filters['role'],
                }))
              }}
            >
              <option value="">{t('admin.users.all')}</option>
              <option value="user">{t('admin.users.role_user')}</option>
              <option value="admin">{t('admin.users.role_admin')}</option>
            </select>
          </div>
          <div className="flex flex-col gap-1.5">
            <label htmlFor="filter-group" className="text-[13px] font-medium text-ink-1">
              {t('admin.users.group')}
            </label>
            <select
              id="filter-group"
              className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              value={draft.group}
              onChange={(event) => {
                setDraft((current) => ({ ...current, group: event.target.value }))
              }}
            >
              <option value="">{t('admin.users.all')}</option>
              {(groupsQuery.data?.items ?? []).map((group) => (
                <option key={group.name} value={group.name}>
                  {group.name}
                </option>
              ))}
            </select>
          </div>
        </div>
        <div className="mt-3 flex justify-end gap-2">
          <Button variant="secondary" onClick={resetFilters}>
            {t('admin.users.reset')}
          </Button>
          <Button onClick={applyFilters}>{t('admin.users.search')}</Button>
        </div>
      </section>

      <section className="rounded-xl border border-border-hairline bg-surface-card">
        <div className="flex items-center justify-between border-b border-border-hairline px-5 py-3">
          <Button onClick={() => setEditTarget({ mode: 'create' })}>
            {t('admin.users.add')}
          </Button>
          <span className="text-[13px] text-ink-2">
            {t('admin.users.total', { count: total })}
          </span>
        </div>

        {usersQuery.isPending ? (
          <div className="space-y-2 p-5" aria-busy="true">
            {[0, 1, 2, 3, 4].map((index) => (
              <div key={index} className="h-10 animate-pulse rounded bg-surface-subtle" />
            ))}
          </div>
        ) : usersQuery.isError ? (
          <div className="flex items-center gap-3 p-5 text-sm text-ink-2">
            <span>{t('admin.users.error')}</span>
            <Button variant="secondary" onClick={() => void usersQuery.refetch()}>
              {t('common.action.retry')}
            </Button>
          </div>
        ) : items.length === 0 ? (
          <p className="p-5 text-sm text-ink-2">{t('admin.users.empty')}</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[960px] text-sm">
              <thead>
                <tr className="border-b border-border-hairline text-left text-xs text-ink-2">
                  <th className="px-4 py-2 font-medium">{t('admin.users.column_id')}</th>
                  <th className="px-4 py-2 font-medium">
                    {t('admin.users.column_username')}
                  </th>
                  <th className="px-4 py-2 font-medium">{t('admin.users.column_status')}</th>
                  <th className="px-4 py-2 text-right font-medium">
                    {t('admin.users.column_balance')}
                  </th>
                  <th className="px-4 py-2 font-medium">{t('admin.users.column_group')}</th>
                  <th className="px-4 py-2 font-medium">{t('admin.users.column_role')}</th>
                  <th className="px-4 py-2 font-medium">{t('admin.users.column_created')}</th>
                  <th className="px-4 py-2 text-right font-medium">
                    {t('admin.users.actions')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {items.map((user) => (
                  <tr key={user.id} className="border-b border-border-hairline last:border-0 hover:bg-surface-hover">
                    <td className="px-4 py-3 font-mono text-xs tabular-nums text-ink-2">
                      {user.id}
                    </td>
                    <td className="px-4 py-3">
                      {user.display_name ? (
                        <div>
                          <p className="text-ink-1">{user.display_name}</p>
                          <p className="text-xs text-ink-2">@{user.username}</p>
                        </div>
                      ) : (
                        <span className="text-ink-1">@{user.username}</span>
                      )}
                    </td>
                    <td className="px-4 py-3">
                      <StatusBadge
                        variant={user.status === 'enabled' ? 'success' : 'neutral'}
                      >
                        {user.status === 'enabled'
                          ? t('admin.users.status_enabled')
                          : t('admin.users.status_disabled')}
                      </StatusBadge>
                    </td>
                    <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                      {formatCredits(user.balance)}
                    </td>
                    <td className="px-4 py-3">
                      <Tag>{user.group}</Tag>
                    </td>
                    <td className="px-4 py-3">
                      <Tag variant={user.role === 'admin' ? 'admin' : 'neutral'}>
                        {user.role === 'admin'
                          ? t('admin.users.role_admin')
                          : t('admin.users.role_user')}
                      </Tag>
                    </td>
                    <td
                      className="px-4 py-3 text-xs text-ink-2"
                      title={user.created_at}
                    >
                      {formatRelativeTime(user.created_at, locale)}
                    </td>
                    <td className="px-4 py-3">
                      <div className="flex justify-end gap-1">
                        <Button
                          variant="ghost"
                          className="h-8 px-2 text-[13px]"
                          onClick={() => setEditTarget({ mode: 'edit', user })}
                        >
                          {t('admin.users.edit')}
                        </Button>
                        <Button
                          variant="ghost"
                          className="h-8 px-2 text-[13px]"
                          onClick={() => setQuotaUser(user)}
                        >
                          {t('admin.users.adjust_quota')}
                        </Button>
                        <Button
                          variant="ghost"
                          className="h-8 px-2 text-[13px]"
                          onClick={() => setRecoveryUser(user)}
                        >
                          {t('admin.users.account_recovery')}
                        </Button>
                        <Button
                          variant="ghost"
                          className="h-8 px-2 text-[13px] text-danger hover:bg-danger-bg"
                          disabled={setStatus.isPending}
                          onClick={() => {
                            // F-07-04: disabling revokes sessions and tokens, so
                            // it is confirmed; enabling is safe and immediate.
                            if (user.status === 'enabled') {
                              setDisableUser(user)
                              return
                            }
                            setStatus.mutate({ id: user.id, status: 'enabled' })
                          }}
                        >
                          {user.status === 'enabled'
                            ? t('admin.users.disable')
                            : t('admin.users.enable')}
                        </Button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}

        <div className="flex items-center justify-end gap-3 border-t border-border-hairline px-5 py-3 text-[13px] text-ink-2">
          <label htmlFor="page-size" className="sr-only">
            {t('admin.users.page_size')}
          </label>
          <select
            id="page-size"
            className="h-8 rounded-md border border-border-default bg-surface-card px-2 text-xs text-ink-1"
            value={pageSize}
            onChange={(event) => {
              setPageSize(Number(event.target.value))
              setPage(1)
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
            disabled={page <= 1}
            onClick={() => setPage((current) => Math.max(1, current - 1))}
          >
            {t('admin.users.pagination_previous')}
          </Button>
          <span className="font-mono tabular-nums">
            {t('admin.users.pagination_page', { page, pages })}
          </span>
          <Button
            variant="secondary"
            className="h-8 px-2 text-[13px]"
            disabled={page >= pages}
            onClick={() => setPage((current) => current + 1)}
          >
            {t('admin.users.pagination_next')}
          </Button>
        </div>
      </section>

      <UserMutateSheet
        open={editTarget !== null}
        mode={editTarget?.mode ?? 'create'}
        user={editTarget?.user}
        onOpenChange={(open) => {
          if (!open) setEditTarget(null)
        }}
        onSaved={() => setEditTarget(null)}
      />

      {quotaUser !== null ? (
        <AdjustQuotaDialog
          user={quotaUser}
          open
          onOpenChange={(open) => {
            if (!open) setQuotaUser(null)
          }}
          onAdjusted={() => setQuotaUser(null)}
        />
      ) : null}

      {recoveryUser !== null ? (
        <AccountRecoveryDialogs
          user={recoveryUser}
          onClose={() => setRecoveryUser(null)}
        />
      ) : null}

      <Dialog
        open={disableUser !== null}
        onOpenChange={(open) => {
          if (!open) setDisableUser(null)
        }}
        title={t('admin.users.disable_title')}
        description={
          disableUser === null
            ? undefined
            : t('admin.users.disable_body', { username: disableUser.username })
        }
        footer={
          <>
            <Button variant="secondary" onClick={() => setDisableUser(null)}>
              {t('common.action.cancel')}
            </Button>
            <Button
              disabled={setStatus.isPending}
              onClick={() => {
                if (disableUser === null) return
                setStatus.mutate(
                  { id: disableUser.id, status: 'disabled' },
                  { onSuccess: () => setDisableUser(null) },
                )
              }}
            >
              {t('admin.users.disable')}
            </Button>
          </>
        }
      />
    </div>
  )
}
