import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError } from '@/api/errors'
import { listChannels, updateChannel } from '@/features/admin/channels/api'
import type {
  Channel,
  ChannelListParams,
} from '@/features/admin/channels/types'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'
import { StatusBadge } from '@/shared/ui/status-badge'

import { AccountDialog } from './account-dialog'
import {
  createIdempotencyKey,
  deleteAccount,
  listAccounts,
  updateAccount,
  type UpstreamAccount,
} from './api'
import { ACCOUNTS_QUERY_KEY } from './query-keys'

/**
 * 渠道键与渠道控制台共用 `['admin', 'channels']` 前缀：控制台里的增删改
 * 与这里的归属写入都会让对方重取。末尾的 `reconcile-binding` 表明本查询
 * 用自己的窗口参数（一页 100 行），不能与列表页的键共用缓存。
 */
const CHANNELS_QUERY_KEY = [
  'admin',
  'channels',
  'list',
  'reconcile-binding',
] as const
const CHANNELS_INVALIDATE_KEY = ['admin', 'channels'] as const

/**
 * 归属卡片只读一页（100 行）：这里的任务是「给渠道对上账号」，不是批量
 * 管理。超过一页时页面明确说明被截断；剩余渠道请到渠道控制台处理。
 */
const CHANNEL_LIST_PARAMS: ChannelListParams = {
  page: 1,
  pageSize: 100,
  keyword: '',
  status: null,
  types: [],
  group: '',
  model: '',
  sort: 'priority',
}

type AccountFormState =
  | { mode: 'create' }
  | { mode: 'edit'; account: UpstreamAccount }

function problemMessage(error: unknown): string {
  if (error instanceof ApiError) return error.detail ?? error.title
  return error instanceof Error ? error.message : String(error)
}

/**
 * 上游账号与渠道归属 Tab（任务 16）。
 *
 * 两张卡片共用一次账号列表：上表管理账号生命周期，下表把渠道归到账号。
 * 绑定写入非乐观——下拉的值永远来自服务端行，成功后按前缀失效渠道与账号
 * 查询；409 不重试，重取把下拉恢复成服务端值，问题详情单独成一条告警。
 * 账号删除同时有两道闸：已加载渠道引用时按钮先禁用（便利提示），服务端
 * 仍会在真正被引用时以 409 拒绝（权威判定）。
 */
export function AccountsTab({ canManage }: { canManage: boolean }) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [form, setForm] = useState<AccountFormState | null>(null)
  const [pendingBindings, setPendingBindings] = useState<ReadonlySet<number>>(
    () => new Set(),
  )

  const accountsQuery = useQuery({
    queryKey: ACCOUNTS_QUERY_KEY,
    // 不传 status：停用的账号同样要列出来，否则无法重新启用。
    // 页大小固定为一页 100：归属下拉必须看到尽可能多的账号；一页 100 是
    // 服务端上限，取缺省 20 会让第 21 个之后的账号在下拉里悄悄消失。
    queryFn: ({ signal }) => listAccounts({ page_size: 100 }, signal),
    retry: false,
    staleTime: 30_000,
  })
  const accounts = accountsQuery.data?.items ?? []
  const accountsTotal = accountsQuery.data?.total ?? 0
  const accountsTruncated = accountsTotal > accounts.length

  const channelsQuery = useQuery({
    queryKey: CHANNELS_QUERY_KEY,
    queryFn: () => listChannels(CHANNEL_LIST_PARAMS),
    retry: false,
    staleTime: 30_000,
  })
  const channels = channelsQuery.data?.items ?? []
  const channelTotal = channelsQuery.data?.total ?? 0
  const truncated = channelTotal > channels.length
  // 已加载渠道引用到的账号 id：用于删除按钮的本地禁用提示。
  const referencedAccountIds = new Set(
    channels.flatMap((channel) =>
      channel.upstream_account_id === null ||
      channel.upstream_account_id === undefined
        ? []
        : [channel.upstream_account_id],
    ),
  )

  const deleteMutation = useMutation({
    mutationFn: (payload: { id: number; idempotencyKey: string }) =>
      deleteAccount(payload.id, payload.idempotencyKey),
    retry: false,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ACCOUNTS_QUERY_KEY })
    },
  })

  const statusMutation = useMutation({
    mutationFn: (payload: {
      account: UpstreamAccount
      status: 1 | 2
      idempotencyKey: string
    }) =>
      updateAccount(
        payload.account.id,
        {
          expected_updated_at: payload.account.updated_at,
          status: payload.status,
        },
        payload.idempotencyKey,
      ),
    retry: false,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ACCOUNTS_QUERY_KEY })
    },
    onError: () => {
      // 409 后重取，让行拿到服务端的 updated_at，重试才有意义。
      void queryClient.invalidateQueries({ queryKey: ACCOUNTS_QUERY_KEY })
    },
  })

  const bindingMutation = useMutation({
    mutationFn: (payload: { channel: Channel; accountId: number | null }) =>
      updateChannel(payload.channel.id, {
        expected_version: payload.channel.version,
        upstream_account_id: payload.accountId,
      }),
    retry: false,
    onMutate: (variables) => {
      // 每个渠道各自挂起，避免 A 行的请求未落在 B 行的下拉上，也避免
      // A 行在首次请求未回时再发一次带着旧 version 的写入。
      setPendingBindings((previous) => {
        const next = new Set(previous)
        next.add(variables.channel.id)
        return next
      })
    },
    onSettled: (_data, _error, variables) => {
      setPendingBindings((previous) => {
        const next = new Set(previous)
        next.delete(variables.channel.id)
        return next
      })
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: CHANNELS_INVALIDATE_KEY })
      void queryClient.invalidateQueries({ queryKey: ACCOUNTS_QUERY_KEY })
    },
    onError: () => {
      // 409 不重试也不保留本地选择：重取服务端值，让下拉回到真实状态。
      void queryClient.invalidateQueries({ queryKey: CHANNELS_INVALIDATE_KEY })
    },
  })

  function requestDelete(account: UpstreamAccount): void {
    const confirmed = globalThis.confirm(
      t('admin.upstream_reconciliation.accounts.delete_confirm', {
        name: account.name,
      }),
    )
    if (!confirmed) return
    deleteMutation.mutate({
      id: account.id,
      idempotencyKey: createIdempotencyKey(),
    })
  }

  function toggleStatus(account: UpstreamAccount): void {
    statusMutation.mutate({
      account,
      status: account.status === 1 ? 2 : 1,
      idempotencyKey: createIdempotencyKey(),
    })
  }

  function bind(channel: Channel, rawValue: string): void {
    bindingMutation.mutate({
      channel,
      accountId: rawValue === '' ? null : Number(rawValue),
    })
  }

  const cardClasses =
    'flex flex-col gap-3 rounded-xl border border-border-hairline bg-surface-card p-4'

  return (
    <div className="flex flex-col gap-4" data-testid="accounts-tab">
      {!canManage ? (
        <FormAlert tone="info">
          {t('admin.upstream_reconciliation.accounts.read_only')}
        </FormAlert>
      ) : null}

      <section className={cardClasses} data-testid="accounts-card">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <h2 className="text-body font-medium text-ink-1">
              {t('admin.upstream_reconciliation.accounts.title')}
            </h2>
            <p className="mt-1 text-meta text-ink-3">
              {t('admin.upstream_reconciliation.accounts.subtitle')}
            </p>
          </div>
          <Button
            className="h-9"
            disabled={!canManage}
            onClick={() => {
              setForm({ mode: 'create' })
            }}
          >
            {t('admin.upstream_reconciliation.accounts.create')}
          </Button>
        </div>

        {accountsQuery.isPending ? (
          <p className="text-body text-ink-3">
            {t('admin.upstream_reconciliation.accounts.loading')}
          </p>
        ) : null}
        {accountsQuery.isError ? (
          <FormAlert tone="danger">
            {t('admin.upstream_reconciliation.accounts.load_error')}{' '}
            {problemMessage(accountsQuery.error)}
          </FormAlert>
        ) : null}
        {accountsQuery.isSuccess && accounts.length === 0 ? (
          <p className="text-body text-ink-3">
            {t('admin.upstream_reconciliation.accounts.empty')}
          </p>
        ) : null}

        {deleteMutation.isError ? (
          <FormAlert tone="danger">
            {problemMessage(deleteMutation.error)}
          </FormAlert>
        ) : null}
        {statusMutation.isError ? (
          <FormAlert tone="danger">
            {problemMessage(statusMutation.error)}
          </FormAlert>
        ) : null}

        {accounts.length > 0 ? (
          <div className="overflow-x-auto rounded-xl border border-border-default">
            <table className="w-full min-w-[980px] text-left text-body">
              <thead className="text-meta text-ink-3">
                <tr>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.upstream_reconciliation.accounts.form.name')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.upstream_reconciliation.accounts.form.provider')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.upstream_reconciliation.accounts.form.currency')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.upstream_reconciliation.accounts.form.timezone')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.upstream_reconciliation.accounts.form.note')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('common.channels.table.status')}
                  </th>
                  <th scope="col" className="px-4 py-2 text-right font-medium">
                    {t('common.channels.table.actions')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {accounts.map((account) => {
                  const bound = referencedAccountIds.has(account.id)
                  return (
                    <tr
                      key={account.id}
                      data-testid="account-row"
                      className="border-t border-border-hairline"
                    >
                      <td className="px-4 py-3 text-ink-1">{account.name}</td>
                      <td className="px-4 py-3 font-mono text-meta text-ink-2">
                        {account.provider}
                      </td>
                      <td className="px-4 py-3 font-mono text-meta text-ink-2">
                        {account.currency}
                      </td>
                      <td className="px-4 py-3 font-mono text-meta text-ink-2">
                        {account.timezone}
                      </td>
                      <td className="px-4 py-3 text-meta text-ink-3">
                        {account.note}
                      </td>
                      <td className="px-4 py-3">
                        <StatusBadge
                          variant={account.status === 1 ? 'success' : 'neutral'}
                        >
                          {t(
                            account.status === 1
                              ? 'admin.upstream_reconciliation.accounts.status_active'
                              : 'admin.upstream_reconciliation.accounts.status_disabled',
                          )}
                        </StatusBadge>
                      </td>
                      <td className="px-4 py-3">
                        <div className="flex flex-wrap items-center justify-end gap-2">
                          {bound ? (
                            <span
                              id={`account-delete-hint-${account.id}`}
                              className="text-meta text-ink-3"
                            >
                              {t(
                                'admin.upstream_reconciliation.accounts.delete_blocked',
                              )}
                            </span>
                          ) : null}
                          <Button
                            variant="secondary"
                            className="h-8 px-2 text-meta"
                            disabled={!canManage || statusMutation.isPending}
                            onClick={() => {
                              setForm({ mode: 'edit', account })
                            }}
                          >
                            {t('admin.upstream_reconciliation.accounts.edit')}
                          </Button>
                          <Button
                            variant="secondary"
                            className="h-8 px-2 text-meta"
                            disabled={!canManage || statusMutation.isPending}
                            onClick={() => {
                              toggleStatus(account)
                            }}
                          >
                            {t(
                              account.status === 1
                                ? 'admin.upstream_reconciliation.accounts.disable'
                                : 'admin.upstream_reconciliation.accounts.enable',
                            )}
                          </Button>
                          <Button
                            variant="secondary"
                            className="h-8 px-2 text-meta"
                            // 客户端提示只是便利：渠道列表只加载了一页，
                            // 服务端 409 才是「仍被引用」的权威判定。
                            disabled={
                              !canManage || deleteMutation.isPending || bound
                            }
                            aria-describedby={
                              bound
                                ? `account-delete-hint-${account.id}`
                                : undefined
                            }
                            onClick={() => {
                              requestDelete(account)
                            }}
                          >
                            {t('admin.upstream_reconciliation.accounts.delete')}
                          </Button>
                        </div>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        ) : null}

        {accountsTruncated ? (
          // 账号被截断时必须明说：下拉里缺一个账号，管理员「修正」归属时
          // 会悄悄覆盖掉真实的账号指向。
          <p className="text-meta text-ink-3">
            {t('admin.upstream_reconciliation.accounts.truncated', {
              shown: accounts.length,
              total: accountsTotal,
            })}
          </p>
        ) : null}
      </section>

      <section className={cardClasses} data-testid="binding-card">
        <div>
          <h2 className="text-body font-medium text-ink-1">
            {t('admin.upstream_reconciliation.accounts.binding.title')}
          </h2>
          <p className="mt-1 text-meta text-ink-3">
            {t('admin.upstream_reconciliation.accounts.binding.subtitle')}
          </p>
        </div>

        {channelsQuery.isError ? (
          <FormAlert tone="danger">
            {problemMessage(channelsQuery.error)}
          </FormAlert>
        ) : null}
        {channelsQuery.isSuccess && channels.length === 0 ? (
          <p className="text-body text-ink-3">
            {t('admin.upstream_reconciliation.accounts.binding.empty')}
          </p>
        ) : null}

        {bindingMutation.isError ? (
          <FormAlert tone="danger">
            {t('admin.upstream_reconciliation.accounts.binding.error')}{' '}
            {problemMessage(bindingMutation.error)}
          </FormAlert>
        ) : null}
        {bindingMutation.isSuccess ? (
          <FormAlert tone="success">
            {t('admin.upstream_reconciliation.accounts.binding.saved')}
          </FormAlert>
        ) : null}

        {channels.length > 0 ? (
          <div className="overflow-x-auto rounded-xl border border-border-default">
            <table className="w-full min-w-[640px] text-left text-body">
              <thead className="text-meta text-ink-3">
                <tr>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.upstream_reconciliation.accounts.binding.channel')}
                  </th>
                  <th scope="col" className="px-4 py-2 font-medium">
                    {t('admin.upstream_reconciliation.accounts.binding.account')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {channels.map((channel) => {
                  const rowPending = pendingBindings.has(channel.id)
                  return (
                    <tr
                      key={channel.id}
                      data-testid="binding-row"
                      className="border-t border-border-hairline"
                    >
                      {/* 行表头必须是 th：scope 只在 th 上有效，td 上的 scope 是
                          无效标记，读屏不会把它当表头念。 */}
                      <th
                        scope="row"
                        className="px-4 py-3 text-left font-normal text-ink-1"
                      >
                        {channel.name}
                      </th>
                      <td className="px-4 py-3">
                        <select
                          aria-label={`${t(
                            'admin.upstream_reconciliation.accounts.binding.account',
                          )}: ${channel.name}`}
                          value={
                            channel.upstream_account_id === null ||
                            channel.upstream_account_id === undefined
                              ? ''
                              : String(channel.upstream_account_id)
                          }
                          disabled={!canManage || rowPending}
                          onChange={(event) => {
                            bind(channel, event.target.value)
                          }}
                          className="h-9 w-full max-w-64 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1 disabled:cursor-not-allowed disabled:opacity-50"
                        >
                          <option value="">
                            {t(
                              'admin.upstream_reconciliation.accounts.binding.unbound',
                            )}
                          </option>
                          {accounts.map((account) => (
                            <option key={account.id} value={String(account.id)}>
                              {account.status === 2
                                ? `${account.name} — ${t(
                                    'admin.upstream_reconciliation.accounts.status_disabled',
                                  )}`
                                : account.name}
                            </option>
                          ))}
                        </select>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        ) : null}

        {truncated ? (
          <p className="text-meta text-ink-3">
            {t('admin.upstream_reconciliation.accounts.binding.truncated', {
              shown: channels.length,
              total: channelTotal,
            })}
          </p>
        ) : null}

        {accountsTruncated ? (
          // 账号被截断时必须明说：下拉里缺一个账号，管理员「修正」归属时
          // 会悄悄覆盖掉真实的账号指向。
          <p className="text-meta text-ink-3">
            {t('admin.upstream_reconciliation.accounts.truncated', {
              shown: accounts.length,
              total: accountsTotal,
            })}
          </p>
        ) : null}
      </section>

      <AccountDialog
        open={form !== null}
        mode={form?.mode ?? 'create'}
        account={form?.mode === 'edit' ? form.account : null}
        canManage={canManage}
        onOpenChange={(open) => {
          if (!open) setForm(null)
        }}
        onSaved={() => {
          void queryClient.invalidateQueries({ queryKey: ACCOUNTS_QUERY_KEY })
        }}
      />
    </div>
  )
}
