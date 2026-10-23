import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useMemo, useState, type ChangeEvent } from 'react'

import { ApiError } from '@/api/errors'
import { PageHeader } from '@/features/shell/page-header'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { formatCredits } from '@/shared/lib/format'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { Tag } from '@/shared/ui/tag'
import { pushErrorToast, useToastStore } from '@/shared/ui/toast'

import {
  MAX_BATCH,
  MASKED_KEY,
  batchTokens,
  deleteToken,
  listTokens,
  setTokenEnabled,
  type BatchTokenAction,
  type Token,
  type TokenList,
  type TokenListParams,
  type TokenQuotaFilter,
  type TokenStatus,
} from './api'
import { ApiAddressesDialog } from './api-addresses-dialog'
import { writeClipboard } from './clipboard'
import { TokenMutateSheet } from './token-mutate-sheet'
import { TokenStatusBadge } from './token-status-badge'

const PAGE_SIZES = [10, 20, 50, 100] as const
const STATUS_OPTIONS: TokenStatus[] = ['enabled', 'disabled', 'expired', 'exhausted']
const QUOTA_OPTIONS: TokenQuotaFilter[] = ['normal', 'low', 'exhausted', 'unlimited']

type SortKey = 'created' | 'name' | 'quota'
type SortDir = 'ascending' | 'descending'

interface DeleteTarget {
  kind: 'single' | 'bulk'
  token?: Token
}

/**
 * Token list (modules-02 §2.1–§2.14, F-02-01/02/03/07/09/10/11/16/17).
 *
 * Server state lives in the Query cache. Filter changes reset the page but keep
 * the selection (the spec keeps a page-level selection across paging/filters);
 * every write invalidates the `['tokens']` prefix so the table re-reads the
 * authoritative derived status and quota. Sorting is page-local because the
 * list contract exposes no sort parameter — the server freezes
 * `created_at DESC`.
 */
export function TokenListPage() {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)

  const [keywordInput, setKeywordInput] = useState('')
  const [keyword, setKeyword] = useState('')
  const [status, setStatus] = useState<TokenStatus | ''>('')
  const [group, setGroup] = useState('')
  const [quota, setQuota] = useState<TokenQuotaFilter | ''>('')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState<number>(20)
  const [sort, setSort] = useState<{ key: SortKey; dir: SortDir }>({
    key: 'created',
    dir: 'descending',
  })

  const [selected, setSelected] = useState<Set<number>>(() => new Set())
  const [busyRows, setBusyRows] = useState<Set<number>>(() => new Set())
  const [bulkBusy, setBulkBusy] = useState(false)
  const [batchFailures, setBatchFailures] = useState<BatchFailureState | null>(null)

  const [createOpen, setCreateOpen] = useState(false)
  const [editId, setEditId] = useState<number | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<DeleteTarget | null>(null)
  const [deleteBusy, setDeleteBusy] = useState(false)
  const [addressesOpen, setAddressesOpen] = useState(false)
  const [guidanceToken, setGuidanceToken] = useState<Token | null>(null)

  useEffect(() => {
    const next = keywordInput.trim()
    if (next === keyword) return
    const timer = setTimeout(() => {
      setKeyword(next)
      setPage(1)
    }, 300)
    return () => {
      clearTimeout(timer)
    }
  }, [keywordInput, keyword])

  // Copy-key guidance auto-dismisses after 5s (modules-02 §2.8); repeating the
  // click restarts the timer because the state object changes identity.
  useEffect(() => {
    if (guidanceToken === null) return
    const timer = setTimeout(() => setGuidanceToken(null), 5000)
    return () => {
      clearTimeout(timer)
    }
  }, [guidanceToken])

  const params = useMemo<TokenListParams>(
    () => ({
      page,
      pageSize,
      ...(keyword !== '' ? { keyword } : {}),
      ...(status !== '' ? { status } : {}),
      ...(group !== '' ? { group } : {}),
      ...(quota !== '' ? { quota } : {}),
    }),
    [page, pageSize, keyword, status, group, quota],
  )

  const listQuery = useQuery({
    queryKey: ['tokens', 'list', params],
    queryFn: () => listTokens(params),
  })

  const items = useMemo(() => listQuery.data?.items ?? [], [listQuery.data])
  const total = listQuery.data?.total ?? 0
  const totalPages = Math.max(1, Math.ceil(total / pageSize))

  // Out-of-range pages fall back to the last page.
  useEffect(() => {
    if (listQuery.data !== undefined && total > 0 && page > totalPages) {
      setPage(totalPages)
    }
  }, [listQuery.data, page, total, totalPages])

  const sortedItems = useMemo(() => {
    const copy = [...items]
    copy.sort((left, right) => {
      let result = 0
      if (sort.key === 'name') result = left.name.localeCompare(right.name)
      else if (sort.key === 'created') {
        result = Date.parse(left.created_at) - Date.parse(right.created_at)
      } else {
        const a = left.remain_quota ?? Number.POSITIVE_INFINITY
        const b = right.remain_quota ?? Number.POSITIVE_INFINITY
        result = a - b
      }
      return sort.dir === 'ascending' ? result : -result
    })
    return copy
  }, [items, sort])

  const groupOptions = useMemo(() => {
    const names = new Set(items.map((token) => token.group))
    if (group !== '') names.add(group)
    return Array.from(names)
  }, [items, group])

  const hasFilters =
    keyword !== '' || status !== '' || group !== '' || quota !== ''

  function invalidate(): void {
    void queryClient.invalidateQueries({ queryKey: ['tokens'] })
  }

  function clearFilters(): void {
    setKeywordInput('')
    setKeyword('')
    setStatus('')
    setGroup('')
    setQuota('')
    setPage(1)
  }

  function clearSelection(): void {
    setSelected(new Set())
  }

  function toggleSelected(id: number): void {
    setSelected((current) => {
      const next = new Set(current)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  function toggleSelectAll(): void {
    const all = items.length > 0 && items.every((token) => selected.has(token.id))
    setSelected(all ? new Set() : new Set(items.map((token) => token.id)))
  }

  function setRowBusy(id: number, busy: boolean): void {
    setBusyRows((current) => {
      if (busy === current.has(id)) return current
      const next = new Set(current)
      if (busy) next.add(id)
      else next.delete(id)
      return next
    })
  }

  function changeSort(key: SortKey): void {
    setSort((current) =>
      current.key === key
        ? {
            key,
            dir: current.dir === 'ascending' ? 'descending' : 'ascending',
          }
        : { key, dir: key === 'name' ? 'ascending' : 'descending' },
    )
  }

  async function copyText(text: string, message: string): Promise<void> {
    if (await writeClipboard(text)) {
      push({ tone: 'success', message })
    } else {
      push({ tone: 'error', message: t('common.tokens.toast.copyFailed') })
    }
  }

  function copyConnectionInfo(token: Token): void {
    const origin = typeof location !== 'undefined' ? location.origin : ''
    const template = [
      `${t('common.tokens.apiAddresses.baseUrl')}: ${origin}/v1`,
      `${t('common.tokens.apiAddresses.apiKey')}: ${MASKED_KEY} (${t(
        'common.tokens.apiAddresses.connectionNote',
      )})`,
      `${t('common.tokens.apiAddresses.authorization')}: ${t(
        'common.tokens.apiAddresses.bearerPlaceholder',
      )}`,
    ].join('\n')
    void copyText(template, t('common.tokens.toast.connectionCopied'))
    void token
  }

  async function handleToggle(token: Token): Promise<void> {
    const enable = token.status !== 'enabled'
    setRowBusy(token.id, true)
    const listKey = ['tokens', 'list', params]
    const previous = queryClient.getQueryData<TokenList>(listKey)
    // Optimistic appearance only: the derived badge is replaced on the next
    // authoritative read, and a failed write restores the snapshot.
    queryClient.setQueryData<TokenList>(listKey, (current) =>
      current === undefined
        ? current
        : {
            ...current,
            items: current.items?.map((item) =>
              item.id === token.id
                ? { ...item, status: enable ? 'enabled' : 'disabled' }
                : item,
            ),
          },
    )
    try {
      await setTokenEnabled(token.id, enable)
      push({
        tone: 'success',
        message: enable
          ? token.status === 'expired'
            ? t('common.tokens.toast.enabledExpired')
            : token.status === 'exhausted'
              ? t('common.tokens.toast.enabledExhausted')
              : t('common.tokens.toast.enabled')
          : t('common.tokens.toast.disabled'),
      })
      invalidate()
    } catch (error) {
      queryClient.setQueryData(listKey, previous)
      // A version conflict means this row is stale; refetch so the next
      // attempt carries the authoritative `updated_at`.
      if (error instanceof ApiError && error.kind === 'conflict') {
        invalidate()
      }
      reportError(error)
    } finally {
      setRowBusy(token.id, false)
    }
  }

  function reportError(error: unknown): void {
    if (error instanceof ApiError) {
      if (error.kind === 'rate_limited') {
        push({ tone: 'error', message: t('common.tokens.toast.rateLimited') })
        return
      }
      pushErrorToast(error)
      return
    }
    push({ tone: 'error', message: t('common.toast.error') })
  }

  async function handleBatch(action: BatchTokenAction): Promise<void> {
    if (selected.size === 0) return
    if (selected.size > MAX_BATCH) {
      push({ tone: 'info', message: t('common.tokens.bulk.limit') })
      return
    }
    const ids = Array.from(selected)
    setBulkBusy(true)
    try {
      const result = await batchTokens(action, ids)
      setBatchFailures({
        succeeded: result.succeeded,
        failed: result.failed,
      })
      if (result.failed.length === 0) {
        push({
          tone: 'success',
          message:
            action === 'enable'
              ? t('common.tokens.toast.batchEnabled', { count: result.succeeded })
              : action === 'disable'
                ? t('common.tokens.toast.batchDisabled', { count: result.succeeded })
                : t('common.tokens.toast.batchDeleted', { count: result.succeeded }),
        })
      } else {
        push({
          tone: 'info',
          message: t('common.tokens.toast.batchPartial', {
            succeeded: result.succeeded,
            failed: result.failed.length,
          }),
        })
      }
      // A totally failed batch keeps the selection so the user can retry; a
      // partial success clears it like the successful path.
      if (result.succeeded > 0) clearSelection()
      invalidate()
    } catch (error) {
      reportError(error)
    } finally {
      setBulkBusy(false)
    }
  }
  async function confirmDelete(): Promise<void> {
    if (deleteTarget === null) return
    setDeleteBusy(true)
    try {
      if (deleteTarget.kind === 'single' && deleteTarget.token !== undefined) {
        await deleteToken(deleteTarget.token.id, deleteTarget.token.updated_at)
        push({ tone: 'success', message: t('common.tokens.toast.deleted') })
      } else {
        const ids = Array.from(selected)
        if (ids.length > MAX_BATCH) {
          push({ tone: 'info', message: t('common.tokens.bulk.limit') })
          return
        }
        const result = await batchTokens('delete', ids)
        setBatchFailures({ succeeded: result.succeeded, failed: result.failed })
        push({
          tone: result.failed.length === 0 ? 'success' : 'info',
          message: t('common.tokens.toast.batchDeleted', { count: result.succeeded }),
        })
        if (result.succeeded > 0) clearSelection()
      }
      setDeleteTarget(null)
      invalidate()
    } catch (error) {
      // DELETE also carries the optimistic-lock version; a 409 is resolved by
      // refreshing the stale row plus the standard error toast.
      if (error instanceof ApiError && error.kind === 'conflict') {
        push({ tone: 'error', message: t('common.tokens.conflict.body') })
        setDeleteTarget(null)
        invalidate()
      } else {
        reportError(error)
      }
    } finally {
      setDeleteBusy(false)
    }
  }

  function handleStatusFilter(event: ChangeEvent<HTMLSelectElement>): void {
    const value = event.target.value
    setStatus(value === '' ? '' : (value as TokenStatus))
    setPage(1)
  }

  const allSelected = items.length > 0 && items.every((token) => selected.has(token.id))
  const someSelected = items.some((token) => selected.has(token.id)) && !allSelected
  const loading = listQuery.isPending

  return (
    <section className="mx-auto flex max-w-[1440px] flex-col gap-4">
      <PageHeader
        crumbs={[
          { label: t('common.console.label') },
          { label: t('common.nav.group.build') },
          { label: t('common.tokens.title') },
        ]}
        title={t('common.tokens.title')}
        description={t('common.tokens.page.description')}
        actions={
          <>
            {/* F-02-18 / web-00 §4.3.3: the token page's toolbar carries the
                entry to per-key model limits, so users do not have to know the
                `/model-config` URL. A real anchor keeps middle-click and
                keyboard behaviour native. */}
            <a
              href="/model-config"
              className="inline-flex h-9 items-center rounded-lg border border-border-default bg-surface-card px-3 text-sm font-medium text-ink-1 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            >
              {t('common.nav.model_config')}
            </a>
            <Button variant="secondary" onClick={() => setAddressesOpen(true)}>
              {t('common.tokens.apiAddresses.title')}
            </Button>
            <Button onClick={() => setCreateOpen(true)}>{t('common.tokens.new')}</Button>
          </>
        }
      />

      <div className="flex flex-wrap items-end gap-3 rounded-xl border border-border-hairline bg-surface-card p-3">
        <div className="flex flex-col gap-1">
          <label htmlFor="token-filter-keyword" className="text-xs text-ink-2">
            {t('common.tokens.filters.search')}
          </label>
          <input
            id="token-filter-keyword"
            type="search"
            value={keywordInput}
            placeholder={t('common.tokens.filters.searchPlaceholder')}
            disabled={loading}
            onChange={(event) => {
              setKeywordInput(event.target.value)
            }}
            className="h-10 w-64 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-50"
          />
        </div>

        <div className="flex flex-col gap-1">
          <label htmlFor="token-filter-status" className="text-xs text-ink-2">
            {t('common.tokens.filters.status')}
          </label>
          <select
            id="token-filter-status"
            value={status}
            disabled={loading}
            onChange={handleStatusFilter}
            className="h-10 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1 disabled:opacity-50"
          >
            <option value="">{t('common.tokens.status.all')}</option>
            {STATUS_OPTIONS.map((value) => (
              <option key={value} value={value}>
                {t(`common.tokens.status.${value}`)}
              </option>
            ))}
          </select>
        </div>

        <div className="flex flex-col gap-1">
          <label htmlFor="token-filter-group" className="text-xs text-ink-2">
            {t('common.tokens.filters.group')}
          </label>
          <select
            id="token-filter-group"
            value={group}
            disabled={loading}
            onChange={(event) => {
              setGroup(event.target.value)
              setPage(1)
            }}
            className="h-10 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1 disabled:opacity-50"
          >
            <option value="">{t('common.tokens.filters.allGroups')}</option>
            {groupOptions.map((name) => (
              <option key={name} value={name}>
                {name}
              </option>
            ))}
          </select>
        </div>

        <div className="flex flex-col gap-1">
          <label htmlFor="token-filter-quota" className="text-xs text-ink-2">
            {t('common.tokens.filters.quota')}
          </label>
          <select
            id="token-filter-quota"
            value={quota}
            disabled={loading}
            onChange={(event) => {
              setQuota(event.target.value === '' ? '' : (event.target.value as TokenQuotaFilter))
              setPage(1)
            }}
            className="h-10 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1 disabled:opacity-50"
          >
            <option value="">{t('common.tokens.filters.allQuotas')}</option>
            {QUOTA_OPTIONS.map((value) => (
              <option key={value} value={value}>
                {t(`common.tokens.quota.${value}`)}
              </option>
            ))}
          </select>
        </div>

        {hasFilters ? (
          <Button variant="ghost" onClick={clearFilters}>
            {t('common.tokens.filters.clear')}
          </Button>
        ) : null}
      </div>

      <div className="flex flex-wrap items-center justify-between gap-3 text-xs text-ink-2">
        <p>{t('common.tokens.page.count', { count: listQuery.data?.total ?? 0 })}</p>
        <Button
          variant="ghost"
          className="h-7 px-2 text-xs"
          onClick={() => {
            void listQuery.refetch()
          }}
        >
          {t('common.action.refresh')}
        </Button>
      </div>

      {guidanceToken !== null ? (
        <div
          role="alert"
          data-testid="copy-key-guidance"
          className="flex items-center justify-between gap-3 rounded-lg border-l-4 border-warning bg-warning-bg px-3 py-2 text-xs text-warning"
        >
          <span>{t('common.tokens.guidance.body')}</span>
          <span className="flex shrink-0 items-center gap-2">
            <Button
              variant="secondary"
              onClick={() => {
                const id = guidanceToken.id
                setGuidanceToken(null)
                setEditId(id)
              }}
            >
              {t('common.tokens.guidance.reset')}
            </Button>
            <Button variant="ghost" onClick={() => setGuidanceToken(null)}>
              {t('common.tokens.guidance.gotIt')}
            </Button>
          </span>
        </div>
      ) : null}

      {batchFailures !== null && batchFailures.failed.length > 0 ? (
        <div
          role="alert"
          data-testid="batch-failures"
          className="rounded-lg border-l-2 border-danger bg-surface-card px-3 py-2 text-xs text-ink-1"
        >
          <p className="font-medium">
            {t('common.tokens.bulk.partial', {
              succeeded: batchFailures.succeeded,
              failed: batchFailures.failed.length,
            })}
          </p>
          <ul className="mt-1 list-disc pl-5">
            {batchFailures.failed.map((failure) => (
              <li key={failure.id}>
                {failure.name !== '' ? `${failure.name}: ` : `#${failure.id}: `}
                {failure.detail}
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      {selected.size > 0 ? (
        <div
          role="toolbar"
          aria-label={t('common.tokens.bulk.toolbar')}
          className="flex flex-wrap items-center gap-2 rounded-lg border border-border-hairline bg-surface-hover p-2"
        >
          <span className="mr-2 text-sm font-medium text-ink-1">
            {t('common.tokens.bulk.selected', { count: selected.size })}
          </span>
          <Button
            variant="secondary"
            disabled={bulkBusy || selected.size > MAX_BATCH}
            onClick={() => {
              void handleBatch('enable')
            }}
          >
            {t('common.tokens.bulk.enable')}
          </Button>
          <Button
            variant="secondary"
            disabled={bulkBusy || selected.size > MAX_BATCH}
            onClick={() => {
              void handleBatch('disable')
            }}
          >
            {t('common.tokens.bulk.disable')}
          </Button>
          <Button
            variant="danger"
            disabled={bulkBusy || selected.size > MAX_BATCH}
            onClick={() => setDeleteTarget({ kind: 'bulk' })}
          >
            {t('common.tokens.bulk.delete')}
          </Button>
          <Button variant="ghost" onClick={clearSelection}>
            {t('common.tokens.bulk.clear')}
          </Button>
          {selected.size > MAX_BATCH ? (
            <span className="text-xs text-danger">{t('common.tokens.bulk.limit')}</span>
          ) : null}
        </div>
      ) : null}

      <div className="overflow-x-auto rounded-xl border border-border-hairline bg-surface-card">
        <table
          data-testid="tokens-table"
          className="w-full min-w-[1080px] border-collapse text-sm"
        >
          <thead>
            <tr>
              <th scope="col" className="w-10 px-3 py-2">
                <input
                  type="checkbox"
                  aria-label={t('common.tokens.table.selectAll')}
                  aria-checked={someSelected ? 'mixed' : allSelected}
                  checked={allSelected}
                  disabled={items.length === 0}
                  onChange={toggleSelectAll}
                />
              </th>
              <SortHeader
                label={t('common.tokens.table.name')}
                column="name"
                sort={sort}
                onSort={changeSort}
              />
              <th scope="col" className={HEADER_CLASS}>
                {t('common.tokens.table.status')}
              </th>
              <th scope="col" className={HEADER_CLASS}>
                {t('common.tokens.table.key')}
              </th>
              <SortHeader
                label={t('common.tokens.table.quota')}
                column="quota"
                sort={sort}
                onSort={changeSort}
              />
              <th scope="col" className={HEADER_CLASS}>
                {t('common.tokens.table.group')}
              </th>
              <th scope="col" className={HEADER_CLASS}>
                {t('common.tokens.table.models')}
              </th>
              <th scope="col" className={HEADER_CLASS}>
                {t('common.tokens.table.ips')}
              </th>
              <SortHeader
                label={t('common.tokens.table.created')}
                column="created"
                sort={sort}
                onSort={changeSort}
              />
              <th scope="col" className={HEADER_CLASS}>
                {t('common.tokens.table.expires')}
              </th>
              <th scope="col" className={HEADER_CLASS}>
                {t('common.tokens.table.actions')}
              </th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr data-testid="tokens-loading">
                <td colSpan={11} className="px-3 py-3">
                  <div className="flex flex-col gap-2">
                    {Array.from({ length: 5 }, (_, index) => (
                      <div
                        key={index}
                        className="h-6 animate-pulse rounded bg-surface-subtle"
                      />
                    ))}
                  </div>
                </td>
              </tr>
            ) : listQuery.isError ? (
              <tr>
                <td colSpan={11} className="px-3 py-8 text-center">
                  <div role="alert" className="flex flex-col items-center gap-2 text-sm">
                    <p className="text-danger">{t('common.tokens.error.load')}</p>
                    <Button
                      variant="secondary"
                      onClick={() => {
                        void listQuery.refetch()
                      }}
                    >
                      {t('common.tokens.error.retry')}
                    </Button>
                  </div>
                </td>
              </tr>
            ) : sortedItems.length === 0 ? (
              <tr>
                <td colSpan={11} className="px-3 py-10 text-center">
                  <p className="text-sm font-medium text-ink-1">
                    {hasFilters ? t('common.tokens.empty.filtered') : t('common.tokens.empty.none')}
                  </p>
                  {hasFilters ? (
                    <p className="mt-1 text-xs text-ink-2">
                      <button
                        type="button"
                        className="underline"
                        onClick={clearFilters}
                      >
                        {t('common.tokens.filters.clear')}
                      </button>
                    </p>
                  ) : (
                    <p className="mt-1 text-xs text-ink-2">
                      {t('common.tokens.empty.description')}
                    </p>
                  )}
                </td>
              </tr>
            ) : (
              sortedItems.map((token) => {
                const busy = busyRows.has(token.id)
                return (
                  <tr
                    key={token.id}
                    data-testid={`token-row-${token.id}`}
                    className="hover:bg-surface-hover"
                  >
                    <td className="px-3 py-2">
                      <input
                        type="checkbox"
                        aria-label={t('common.tokens.table.selectRow', { name: token.name })}
                        checked={selected.has(token.id)}
                        onChange={() => {
                          toggleSelected(token.id)
                        }}
                      />
                    </td>
                    <td className="max-w-52 truncate px-3 py-2 font-medium text-ink-1" title={token.name}>
                      {token.name}
                    </td>
                    <td className="px-3 py-2">
                      <TokenStatusBadge
                        status={token.status}
                        expiresAt={token.expires_at ?? null}
                      />
                    </td>
                    <td className="px-3 py-2">
                      <div className="flex items-center gap-1">
                        <code
                          data-testid="masked-key"
                          aria-label={t('common.tokens.table.maskedKey')}
                          className="whitespace-nowrap font-mono text-[13px] text-ink-1"
                        >
                          {MASKED_KEY}
                        </code>
                        <button
                          type="button"
                          aria-label={t('common.tokens.table.copyKey', { name: token.name })}
                          className="text-ink-3 hover:text-ink-1"
                          onClick={() => setGuidanceToken(token)}
                        >
                          ⧉
                        </button>
                      </div>
                    </td>
                    <td className="px-3 py-2">
                      <QuotaCell token={token} />
                    </td>
                    <td className="px-3 py-2">
                      {token.group === 'auto' ? (
                        <Tag variant={token.auto_groups?.length ? 'neutral' : 'warning'}>
                          {token.auto_groups?.length
                            ? t('common.tokens.table.autoGroupsCount', {
                                count: token.auto_groups.length,
                              })
                            : t('common.tokens.table.autoGroupsUnavailable')}
                        </Tag>
                      ) : (
                        <Tag>{token.group}</Tag>
                      )}
                    </td>
                    <td className="px-3 py-2 text-xs text-ink-2">
                      {token.model_limits === null ||
                      token.model_limits === undefined ||
                      token.model_limits.length === 0 ? (
                        <span className="text-ink-3">—</span>
                      ) : (
                        <span title={token.model_limits.join(', ')}>
                          {t('common.tokens.table.moreModels', {
                            count: token.model_limits.length,
                          })}
                        </span>
                      )}
                    </td>
                    <td className="px-3 py-2 text-xs text-ink-2">
                      {token.allow_ips === null ||
                      token.allow_ips === undefined ||
                      token.allow_ips.length === 0 ? (
                        <span className="text-ink-3">—</span>
                      ) : (
                        <span title={token.allow_ips.join(', ')}>
                          {t('common.tokens.table.ipCount', { count: token.allow_ips.length })}
                        </span>
                      )}
                    </td>
                    <td className="px-3 py-2 text-xs text-ink-2">
                      <TimeCell iso={token.created_at} />
                      <div className="text-ink-3">
                        {token.last_used_at === null ||
                        token.last_used_at === undefined
                          ? t('common.tokens.table.neverUsed')
                          : t('common.tokens.table.lastUsed', {
                              time: formatRelativeTime(token.last_used_at, locale),
                            })}
                      </div>
                    </td>
                    <td className="px-3 py-2 text-xs">
                      <ExpiresCell iso={token.expires_at ?? null} />
                    </td>
                    <td className="px-3 py-2">
                      <div className="flex items-center gap-1">
                        <button
                          type="button"
                          aria-label={t(
                            token.status === 'enabled'
                              ? 'common.tokens.table.disable'
                              : 'common.tokens.table.enable',
                            { name: token.name },
                          )}
                          aria-busy={busy}
                          disabled={busy}
                          className="px-1 text-ink-2 hover:text-ink-1 disabled:opacity-40"
                          onClick={() => {
                            void handleToggle(token)
                          }}
                        >
                          {busy ? '…' : '⏻'}
                        </button>
                        <button
                          type="button"
                          aria-label={t('common.tokens.table.copyConnection', {
                            name: token.name,
                          })}
                          className="px-1 text-ink-2 hover:text-ink-1"
                          onClick={() => copyConnectionInfo(token)}
                        >
                          🔗
                        </button>
                        <button
                          type="button"
                          aria-label={t('common.tokens.table.edit', { name: token.name })}
                          className="px-1 text-ink-2 hover:text-ink-1"
                          onClick={() => setEditId(token.id)}
                        >
                          ✎
                        </button>
                        <button
                          type="button"
                          aria-label={t('common.tokens.table.delete', { name: token.name })}
                          className="px-1 text-danger hover:text-danger"
                          onClick={() => setDeleteTarget({ kind: 'single', token })}
                        >
                          🗑
                        </button>
                      </div>
                    </td>
                  </tr>
                )
              })
            )}
          </tbody>
        </table>
      </div>

      <div className="flex flex-wrap items-center justify-between gap-3">
        <Button
          variant="secondary"
          disabled={page <= 1 || loading}
          onClick={() => setPage((current) => Math.max(1, current - 1))}
        >
          {t('common.tokens.pagination.previous')}
        </Button>
        <p className="text-sm text-ink-2">
          {t('common.tokens.pagination.page', { page, totalPages })}
        </p>
        <div className="flex items-center gap-2">
          <label htmlFor="token-page-size" className="text-sm text-ink-2">
            {t('common.tokens.pagination.pageSize')}
          </label>
          <select
            id="token-page-size"
            value={String(pageSize)}
            disabled={loading}
            onChange={(event) => {
              setPageSize(Number(event.target.value))
              setPage(1)
            }}
            className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
          >
            {PAGE_SIZES.map((size) => (
              <option key={size} value={String(size)}>
                {size}
              </option>
            ))}
          </select>
        </div>
        <Button
          variant="secondary"
          disabled={page >= totalPages || loading}
          onClick={() => setPage((current) => Math.min(totalPages, current + 1))}
        >
          {t('common.tokens.pagination.next')}
        </Button>
      </div>

      <TokenMutateSheet
        open={createOpen}
        mode="create"
        onOpenChange={setCreateOpen}
        onSaved={invalidate}
      />
      <TokenMutateSheet
        open={editId !== null}
        mode="edit"
        tokenId={editId ?? undefined}
        onOpenChange={(open) => {
          if (!open) setEditId(null)
        }}
        onSaved={invalidate}
      />

      <Dialog
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (!open && !deleteBusy) setDeleteTarget(null)
        }}
        title={t('common.tokens.delete.title')}
        description={
          deleteTarget?.kind === 'single' && deleteTarget.token !== undefined
            ? t('common.tokens.delete.singleBody', { name: deleteTarget.token.name })
            : t('common.tokens.delete.batchBody', { count: selected.size })
        }
        dismissible={!deleteBusy}
        footer={
          <>
            <Button
              variant="secondary"
              disabled={deleteBusy}
              onClick={() => setDeleteTarget(null)}
            >
              {t('common.tokens.delete.cancel')}
            </Button>
            <Button
              variant="danger"
              disabled={deleteBusy}
              onClick={() => {
                void confirmDelete()
              }}
            >
              {deleteBusy ? t('common.tokens.delete.deleting') : t('common.tokens.delete.confirm')}
            </Button>
          </>
        }
      />

      <ApiAddressesDialog open={addressesOpen} onOpenChange={setAddressesOpen} />
    </section>
  )
}

interface BatchFailureState {
  succeeded: number
  failed: Array<{ id: number; name: string; detail: string }>
}

const HEADER_CLASS =
  'border-b border-border-hairline bg-surface-subtle px-3 py-2 text-left text-xs font-medium text-ink-2'

interface SortHeaderProps {
  label: string
  column: SortKey
  sort: { key: SortKey; dir: SortDir }
  onSort: (column: SortKey) => void
}

function SortHeader({ label, column, sort, onSort }: SortHeaderProps) {
  const active = sort.key === column
  return (
    <th
      scope="col"
      aria-sort={active ? sort.dir : 'none'}
      className={HEADER_CLASS}
    >
      <button
        type="button"
        onClick={() => {
          onSort(column)
        }}
        className="inline-flex items-center gap-1"
      >
        {label}
        {active ? (
          <span aria-hidden="true">
            {sort.dir === 'ascending' ? '↑' : '↓'}
          </span>
        ) : null}
      </button>
    </th>
  )
}

function TimeCell({ iso }: { iso: string }) {
  const { locale } = useI18n()
  const timestamp = Date.parse(iso)
  const absolute = Number.isNaN(timestamp)
    ? iso
    : new Date(timestamp).toLocaleString(locale)
  return (
    <span title={absolute}>
      {Number.isNaN(timestamp) ? iso : formatRelativeTime(iso, locale)}
    </span>
  )
}

function ExpiresCell({ iso }: { iso: string | null }) {
  const { t, locale } = useI18n()
  if (iso === null) return <span className="text-ink-3">{t('common.tokens.table.never')}</span>
  const timestamp = Date.parse(iso)
  const soon = !Number.isNaN(timestamp) && timestamp - Date.now() < 7 * 86_400_000
  return (
    <span className={soon ? 'text-warning' : 'text-ink-2'} title={iso}>
      {Number.isNaN(timestamp) ? iso : new Date(timestamp).toLocaleDateString(locale)}
    </span>
  )
}

function QuotaCell({ token }: { token: Token }) {
  const { t, locale } = useI18n()
  if (token.unlimited_quota) {
    return <span className="text-xs text-ink-2">{t('common.tokens.table.unlimited')}</span>
  }
  const remain = token.remain_quota ?? 0
  const percent = Math.max(0, Math.min(100, token.quota_percent ?? 0))
  const tone =
    percent === 0 ? 'bg-danger' : percent < 10 ? 'bg-warning' : 'bg-success'
  return (
    <div className="flex flex-col gap-1">
      <span className="tabular-nums text-ink-1">
        {formatCredits(remain, locale)}{' '}
        <span className="text-xs text-ink-3">{t('common.tokens.table.credits')}</span>
      </span>
      {token.quota_percent !== null && token.quota_percent !== undefined ? (
        <div
          role="progressbar"
          aria-label={t('common.tokens.table.quotaAria', { name: token.name })}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={percent}
          className="h-1 w-full overflow-hidden rounded bg-surface-subtle"
        >
          <div className={`h-full ${tone}`} style={{ width: `${percent}%` }} />
        </div>
      ) : null}
    </div>
  )
}
