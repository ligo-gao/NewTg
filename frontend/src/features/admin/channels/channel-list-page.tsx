import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Fragment,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ChangeEvent,
} from 'react'

import { ApiError } from '@/api/errors'
import { PageHeader } from '@/features/shell/page-header'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { Tag } from '@/shared/ui/tag'
import { pushErrorToast, useToastStore } from '@/shared/ui/toast'

import {
  batchChannelStatus,
  batchDeleteChannels,
  createIdempotencyKey,
  deleteChannel,
  listChannels,
  testChannel,
  updateChannel,
} from './api'
import { ChannelDetailPanel } from './channel-detail-panel'
import { ChannelModelCell } from './channel-model-cell'
import { ChannelMutateSheet } from './channel-mutate-sheet'
import { ChannelPerformanceDialog } from './channel-perf-dialog'
import { ChannelRowActions } from './channel-row-actions'
import { ChannelStatusBadge } from './channel-status-badge'
import { ChannelTestDialog } from './channel-test-dialog'
import { CrossProtocolForwardInfo } from './cross-protocol-forward-info'
import { ConfirmDialog } from './ui/confirm-dialog'
import { testErrorKey } from './test-error'
import {
  CHANNEL_TYPE_GROUP_ORDER,
  channelTypesInGroup,
  type BatchStatusAction,
  type Channel,
  type ChannelListParams,
  type ChannelSort,
  type ChannelStatus,
} from './types'

const PAGE_SIZES = [10, 20, 50, 100] as const

type SheetState =
  | { open: false }
  | { open: true; mode: 'create' }
  | { open: true; mode: 'edit'; channelId: number }

type DeleteTarget =
  | { kind: 'bulk' }
  | { kind: 'single'; channel: Channel }

/**
 * Channel list (modules-06 §2, F-06-01/F-06-02/F-06-08).
 *
 * Server state lives in the Query cache; filter/pagination state is local.
 * Every list-changing write invalidates the `['admin','channels']` prefix so
 * the table re-reads the authoritative row (status writes are optimistic only
 * in appearance, never in the cache).
 */
export function ChannelListPage() {
  const { t } = useI18n()
  const queryClient = useQueryClient()

  const [keywordInput, setKeywordInput] = useState('')
  const [modelInput, setModelInput] = useState('')
  const [groupInput, setGroupInput] = useState('')
  const [keyword, setKeyword] = useState('')
  const [model, setModel] = useState('')
  const [status, setStatus] = useState<ChannelStatus | null>(null)
  const [types, setTypes] = useState<string[]>([])
  const [group, setGroup] = useState('')
  const [sort, setSort] = useState<ChannelSort>('priority')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState<number>(20)

  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [expandedId, setExpandedId] = useState<number | null>(null)
  // Row-scoped busy state. A Set rather than a single id: overlapping mutations
  // on different rows must not clear each other's lock when one finishes.
  const [busyRows, setBusyRows] = useState<Set<number>>(() => new Set())
  const [bulkBusy, setBulkBusy] = useState(false)
  const [sheet, setSheet] = useState<SheetState>({ open: false })
  const [testTarget, setTestTarget] = useState<Channel | null>(null)
  const [perfTarget, setPerfTarget] = useState<Channel | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<DeleteTarget | null>(null)
  const [deleteBusy, setDeleteBusy] = useState(false)
  // One Idempotency-Key per delete intent: retrying the same id set after a
  // failure replays the original request; a different selection mints a new one.
  const deleteIntent = useRef<{ signature: string; key: string } | null>(null)
  const selectAllRef = useRef<HTMLInputElement>(null)

  // F-06-01: keyword/model filtering is debounced by 300ms; the other filters
  // apply immediately and always reset to page 1 (modules-06 §2.2). Selection
  // is page-scoped, so a filter change clears it as well.
  useEffect(() => {
    const nextKeyword = keywordInput.trim()
    if (nextKeyword === keyword) return
    const timer = setTimeout(() => {
      setKeyword(nextKeyword)
      setPage(1)
      setSelected(new Set())
    }, 300)
    return () => {
      clearTimeout(timer)
    }
  }, [keywordInput, keyword])

  useEffect(() => {
    const nextModel = modelInput.trim()
    if (nextModel === model) return
    const timer = setTimeout(() => {
      setModel(nextModel)
      setPage(1)
      setSelected(new Set())
    }, 300)
    return () => {
      clearTimeout(timer)
    }
  }, [modelInput, model])

  // F-06-01 applies to every free-text filter: the group input must not fire a
  // request per keystroke either, and a changed filter invalidates the
  // page-scoped selection just like keyword/model do.
  useEffect(() => {
    const nextGroup = groupInput.trim()
    if (nextGroup === group) return
    const timer = setTimeout(() => {
      setGroup(nextGroup)
      setPage(1)
      setSelected(new Set())
    }, 300)
    return () => {
      clearTimeout(timer)
    }
  }, [groupInput, group])

  const params = useMemo<ChannelListParams>(
    () => ({
      page,
      pageSize,
      keyword,
      status,
      types,
      group: group.trim(),
      model,
      sort,
    }),
    [page, pageSize, keyword, status, types, group, model, sort],
  )

  const channelsQuery = useQuery({
    queryKey: ['admin', 'channels', 'list', params],
    queryFn: () => listChannels(params),
  })

  const error = channelsQuery.error
  useEffect(() => {
    if (error instanceof ApiError) pushErrorToast(error)
  }, [error])

  const items = channelsQuery.data?.items ?? []
  const total = channelsQuery.data?.total ?? 0
  const totalPages = Math.max(1, Math.ceil(total / pageSize))

  // Out-of-range pages fall back to the last page (modules-06 §2.2).
  useEffect(() => {
    if (channelsQuery.data !== undefined && total > 0 && page > totalPages) {
      setPage(totalPages)
    }
  }, [channelsQuery.data, page, total, totalPages])

  const hasFilters =
    keyword !== '' ||
    model !== '' ||
    status !== null ||
    types.length > 0 ||
    group.trim() !== ''

  function invalidate(): void {
    void queryClient.invalidateQueries({ queryKey: ['admin', 'channels'] })
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
    const all =
      items.length > 0 && items.every((channel) => selected.has(channel.id))
    setSelected(all ? new Set() : new Set(items.map((channel) => channel.id)))
  }

  /** Adds/removes one row's busy entry without touching other rows. */
  function setRowBusy(id: number, busy: boolean): void {
    setBusyRows((current) => {
      if (busy === current.has(id)) return current
      const next = new Set(current)
      if (busy) next.add(id)
      else next.delete(id)
      return next
    })
  }

  function clearFilters(): void {
    setKeywordInput('')
    setModelInput('')
    setGroupInput('')
    setKeyword('')
    setModel('')
    setStatus(null)
    setTypes([])
    setGroup('')
    setPage(1)
    clearSelection()
  }

  function reportError(reason: unknown): void {
    if (reason instanceof ApiError) {
      pushErrorToast(reason)
      return
    }
    useToastStore.getState().push({
      tone: 'error',
      message: t('common.channels.errors.generic'),
    })
  }

  function testReason(errorClass: string | undefined): string {
    return t(testErrorKey(errorClass))
  }

  async function handleToggleStatus(channel: Channel): Promise<void> {
    const target: 1 | 2 = channel.status === 1 ? 2 : 1
    setRowBusy(channel.id, true)
    try {
      await updateChannel(channel.id, {
        expected_version: channel.version,
        status: target,
      })
      useToastStore.getState().push({
        tone: 'success',
        message:
          target === 1
            ? t('common.channels.toast.enabled')
            : t('common.channels.toast.disabled'),
      })
      invalidate()
    } catch (reason) {
      if (reason instanceof ApiError && reason.kind === 'conflict') {
        useToastStore.getState().push({
          tone: 'error',
          message: t('common.channels.errors.statusConflict'),
        })
        invalidate()
      } else {
        reportError(reason)
      }
    } finally {
      setRowBusy(channel.id, false)
    }
  }

  async function handleSingleTest(channel: Channel): Promise<void> {
    const model = channel.test_model ?? channel.supported_models[0]
    if (model === undefined) {
      useToastStore.getState().push({
        tone: 'info',
        message: t('common.channels.toast.noModels'),
      })
      return
    }
    setRowBusy(channel.id, true)
    try {
      const result = await testChannel(channel.id, { model, stream: false })
      if (result.success) {
        useToastStore.getState().push({
          tone: 'success',
          message: t('common.channels.toast.testOk', {
            model: result.model,
            latency: result.latency_ms,
          }),
        })
      } else {
        useToastStore.getState().push({
          tone: 'error',
          message: t('common.channels.toast.testFailed', {
            model: result.model,
            reason: testReason(result.error_class),
          }),
        })
      }
      invalidate()
    } catch (reason) {
      if (reason instanceof ApiError && reason.kind === 'rate_limited') {
        useToastStore.getState().push({
          tone: 'error',
          message: t('common.channels.toast.rateLimited'),
        })
      } else {
        reportError(reason)
      }
    } finally {
      setRowBusy(channel.id, false)
    }
  }

  async function handleBulkStatus(action: BatchStatusAction): Promise<void> {
    if (selected.size === 0) return
    const ids = Array.from(selected)
    setBulkBusy(true)
    // Lock every row the batch mutates for its whole duration: a row switch or
    // kebab would otherwise start a competing write against the same channel
    // while the batch PATCH is in flight.
    for (const id of ids) setRowBusy(id, true)
    try {
      const result = await batchChannelStatus(action, ids)
      if (result.failed.length === 0) {
        useToastStore.getState().push({
          tone: 'success',
          message: t(
            action === 'enable'
              ? 'common.channels.toast.batchEnabled'
              : 'common.channels.toast.batchDisabled',
            { count: result.succeeded.length },
          ),
        })
      } else {
        useToastStore.getState().push({
          tone: 'info',
          message: t('common.channels.toast.batchPartial', {
            succeeded: result.succeeded.length,
            failed: result.failed.length,
            reason: result.failed[0]?.reason ?? '',
          }),
        })
      }
      clearSelection()
      invalidate()
    } catch (reason) {
      reportError(reason)
    } finally {
      for (const id of ids) setRowBusy(id, false)
      setBulkBusy(false)
    }
  }

  async function handleConfirmDelete(): Promise<void> {
    if (deleteTarget === null) return
    setDeleteBusy(true)
    try {
      if (deleteTarget.kind === 'bulk') {
        const ids = Array.from(selected)
        const signature = JSON.stringify(ids)
        if (deleteIntent.current?.signature !== signature) {
          deleteIntent.current = { signature, key: createIdempotencyKey() }
        }
        const result = await batchDeleteChannels(ids, deleteIntent.current.key)
        // The intent completed; a later bulk delete is a new one.
        deleteIntent.current = null
        if (result.failed.length === 0) {
          useToastStore.getState().push({
            tone: 'success',
            message: t('common.channels.toast.batchDeleted', {
              count: result.succeeded.length,
            }),
          })
        } else {
          useToastStore.getState().push({
            tone: 'info',
            message: t('common.channels.toast.batchPartial', {
              succeeded: result.succeeded.length,
              failed: result.failed.length,
              reason: result.failed[0]?.reason ?? '',
            }),
          })
        }
      } else {
        await deleteChannel(deleteTarget.channel.id)
        useToastStore.getState().push({
          tone: 'success',
          message: t('common.channels.toast.deleted'),
        })
      }
      setDeleteTarget(null)
      clearSelection()
      invalidate()
    } catch (reason) {
      reportError(reason)
    } finally {
      setDeleteBusy(false)
    }
  }

  function handleStatusFilter(event: ChangeEvent<HTMLSelectElement>): void {
    const value = event.target.value
    setStatus(value === '' ? null : (Number(value) as ChannelStatus))
    setPage(1)
    clearSelection()
  }

  function toggleTypeFilter(type: string): void {
    setTypes((current) =>
      current.includes(type)
        ? current.filter((entry) => entry !== type)
        : [...current, type],
    )
    setPage(1)
    clearSelection()
  }

  function changeSort(next: ChannelSort): void {
    setSort(next)
    setPage(1)
    clearSelection()
  }

  function changePageSize(event: ChangeEvent<HTMLSelectElement>): void {
    setPageSize(Number(event.target.value))
    setPage(1)
    clearSelection()
  }

  const allSelected =
    items.length > 0 && items.every((channel) => selected.has(channel.id))
  // The DOM checkbox needs an explicit indeterminate flag: `checked` only
  // models the all/none states, so a partial selection would otherwise look
  // unchecked. It is a property, not an attribute, so React cannot set it.
  const partiallySelected = selected.size > 0 && !allSelected
  useEffect(() => {
    if (selectAllRef.current !== null) {
      selectAllRef.current.indeterminate = partiallySelected
    }
  }, [partiallySelected])
  const deleteCount = deleteTarget?.kind === 'bulk' ? selected.size : 1
  const loading = channelsQuery.isPending
  // A bulk write must not run while any selected row still has a mutation in
  // flight (modules-06 §2: one write per row at a time). Checking every selected
  // row — not just the last-started one — keeps the lock correct when row
  // mutations overlap.
  const bulkDisabled =
    bulkBusy || Array.from(selected).some((id) => busyRows.has(id))

  return (
    <section className="flex flex-col gap-4">
      <PageHeader
        crumbs={[
          { label: t('common.console.label') },
          { label: t('common.nav.group.admin') },
          { label: t('common.channels.title') },
        ]}
        title={t('common.channels.title')}
        description={t('common.channels.subtitle')}
        actions={
          <Button
            onClick={() => {
              setSheet({ open: true, mode: 'create' })
            }}
          >
            {t('common.channels.new')}
          </Button>
        }
      />

      {/* Read-only by design: the switch lives in module 08 settings and M4 has
          no API for it, so no `enabled` prop is passed here (status unknown). */}
      <CrossProtocolForwardInfo />

      <div className="flex flex-wrap items-end gap-3">
        <div className="flex flex-col gap-1">
          <label htmlFor="channel-filter-keyword" className="text-meta text-ink-2">
            {t('common.channels.filters.keyword')}
          </label>
          <input
            id="channel-filter-keyword"
            value={keywordInput}
            placeholder={t('common.channels.filters.keywordPlaceholder')}
            disabled={loading}
            onChange={(event) => {
              setKeywordInput(event.target.value)
            }}
            className="h-9 w-56 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-50"
          />
        </div>

        <div className="flex flex-col gap-1">
          <label htmlFor="channel-filter-status" className="text-meta text-ink-2">
            {t('common.channels.filters.status')}
          </label>
          <select
            id="channel-filter-status"
            value={status === null ? '' : String(status)}
            disabled={loading}
            onChange={handleStatusFilter}
            className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-50"
          >
            <option value="">{t('common.channels.status.all')}</option>
            <option value="1">{t('common.channels.status.enabled')}</option>
            <option value="2">{t('common.channels.status.manual_disabled')}</option>
            <option value="3">{t('common.channels.status.auto_disabled')}</option>
          </select>
        </div>

        <details className="relative flex flex-col gap-1">
          <summary className="cursor-pointer text-meta text-ink-2">
            {types.length === 0
              ? t('common.channels.filters.allTypes')
              : `${t('common.channels.filters.type')} (${types.length})`}
          </summary>
          <div className="absolute top-7 z-10 max-h-72 w-64 overflow-y-auto rounded-lg border border-border-hairline bg-surface-card p-2 shadow-lg">
            {CHANNEL_TYPE_GROUP_ORDER.map((groupName) => (
              <fieldset key={groupName} className="mb-2">
                <legend className="px-1 text-badge text-ink-3">
                  {t(`common.channels.typeGroup.${groupName}`)}
                </legend>
                {channelTypesInGroup(groupName).map((type) => (
                  <label
                    key={type}
                    className="flex items-center gap-2 rounded px-1 py-0.5 text-meta text-ink-1 hover:bg-surface-hover"
                  >
                    <input
                      type="checkbox"
                      checked={types.includes(type)}
                      disabled={loading}
                      onChange={() => {
                        toggleTypeFilter(type)
                      }}
                    />
                    {type}
                  </label>
                ))}
              </fieldset>
            ))}
          </div>
        </details>

        <div className="flex flex-col gap-1">
          <label htmlFor="channel-filter-group" className="text-meta text-ink-2">
            {t('common.channels.filters.group')}
          </label>
          <input
            id="channel-filter-group"
            value={groupInput}
            placeholder={t('common.channels.filters.groupPlaceholder')}
            disabled={loading}
            onChange={(event) => {
              setGroupInput(event.target.value)
            }}
            className="h-9 w-40 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-50"
          />
        </div>

        <div className="flex flex-col gap-1">
          <label htmlFor="channel-filter-model" className="text-meta text-ink-2">
            {t('common.channels.filters.model')}
          </label>
          <input
            id="channel-filter-model"
            value={modelInput}
            placeholder={t('common.channels.filters.modelPlaceholder')}
            disabled={loading}
            onChange={(event) => {
              setModelInput(event.target.value)
            }}
            className="h-9 w-40 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-50"
          />
        </div>

        {hasFilters ? (
          <Button variant="ghost" onClick={clearFilters}>
            {t('common.channels.filters.clear')}
          </Button>
        ) : null}
      </div>

      <div className="flex justify-end">
        <p className="text-body text-ink-2" data-testid="channels-total">
          {loading ? '—' : t('common.channels.total', { total })}
        </p>
      </div>

      <div className="overflow-x-auto rounded-xl border border-border-hairline bg-surface-card">
        <table
          data-testid="channels-table"
          aria-busy={loading}
          className="w-full min-w-[1080px] border-collapse text-body"
        >
          <thead>
            <tr>
              <th scope="col" className="w-10 whitespace-nowrap border-b border-border-hairline bg-surface-subtle px-3 py-2">
                <input
                  ref={selectAllRef}
                  type="checkbox"
                  aria-label={t('common.channels.table.selectAll')}
                  checked={allSelected}
                  disabled={items.length === 0}
                  onChange={toggleSelectAll}
                />
              </th>
              <SortHeader
                label={t('common.channels.table.name')}
                column="name"
                sort={sort}
                onSort={changeSort}
              />
              <th scope="col" className="whitespace-nowrap border-b border-border-hairline bg-surface-subtle px-3 py-2 text-left text-meta text-ink-2">
                {t('common.channels.table.status')}
              </th>
              <th scope="col" className="whitespace-nowrap border-b border-border-hairline bg-surface-subtle px-3 py-2 text-left text-meta text-ink-2">
                {t('common.channels.table.models')}
              </th>
              <SortHeader
                label={t('common.channels.table.routingGroups')}
                column="priority"
                sort={sort}
                onSort={changeSort}
              />
              <th scope="col" className="hidden whitespace-nowrap border-b border-border-hairline bg-surface-subtle px-3 py-2 text-left text-meta text-ink-2 xl:table-cell">
                {t('common.channels.table.health24h')}
              </th>
              <th scope="col" className="whitespace-nowrap border-b border-border-hairline bg-surface-subtle px-3 py-2 text-right text-meta text-ink-2">
                {t('common.channels.table.actions')}
              </th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              Array.from({ length: 5 }, (_, index) => (
                <tr key={`skeleton-${index}`}>
                  <td colSpan={7} className="border-b border-border-hairline px-3 py-3">
                    <div className="h-6 animate-pulse rounded bg-surface-subtle" />
                  </td>
                </tr>
              ))
            ) : channelsQuery.isError ? (
              <tr>
                <td colSpan={7} className="px-3 py-8 text-center">
                  <div role="alert" className="flex flex-col items-center gap-2 text-body text-danger">
                    <p>{t('common.channels.error.load')}</p>
                    <Button
                      variant="secondary"
                      onClick={() => {
                        void channelsQuery.refetch()
                      }}
                    >
                      {t('common.channels.error.retry')}
                    </Button>
                  </div>
                </td>
              </tr>
            ) : items.length === 0 ? (
              <tr>
                <td colSpan={7} className="px-3 py-10 text-center">
                  <p className="text-body text-ink-2">
                    {hasFilters
                      ? t('common.channels.empty.filtered')
                      : t('common.channels.empty.none')}
                  </p>
                  {hasFilters ? (
                    <div className="mt-3 flex justify-center">
                      <Button variant="secondary" onClick={clearFilters}>
                        {t('common.channels.filters.clear')}
                      </Button>
                    </div>
                  ) : null}
                </td>
              </tr>
            ) : (
              items.map((channel) => {
                const busy = busyRows.has(channel.id)
                const expanded = expandedId === channel.id
                return (
                  <Fragment key={channel.id}>
                    <tr data-testid={`channel-row-${channel.id}`} className="hover:bg-surface-hover">
                      <td className="border-b border-border-hairline px-3 py-3">
                        <input
                          type="checkbox"
                          aria-label={t('common.channels.table.selectRow', {
                            name: channel.name,
                          })}
                          checked={selected.has(channel.id)}
                          onChange={() => {
                            toggleSelected(channel.id)
                          }}
                        />
                      </td>
                      <td className="border-b border-border-hairline px-3 py-3">
                        <div className="flex flex-col gap-1">
                          <div className="flex items-center gap-1">
                            <svg
                              aria-hidden="true"
                              viewBox="0 0 20 20"
                              fill="none"
                              stroke="currentColor"
                              strokeWidth="1.8"
                              strokeLinecap="round"
                              strokeLinejoin="round"
                              className={[
                                'size-3.5 shrink-0 text-ink-3 transition-transform',
                                expanded ? 'rotate-90' : '',
                              ].join(' ')}
                            >
                              <path d="m8 5 5 5-5 5" />
                            </svg>
                            <button
                              type="button"
                              aria-expanded={expanded}
                              aria-controls={
                                expanded ? `channel-detail-${channel.id}` : undefined
                              }
                              aria-label={t(
                                expanded
                                  ? 'common.channels.table.collapse'
                                  : 'common.channels.table.expand',
                                { name: channel.name },
                              )}
                              className="max-w-48 truncate text-left font-medium text-ink-1 underline-offset-2 hover:underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                              onClick={() => {
                                setExpandedId(expanded ? null : channel.id)
                              }}
                            >
                              {channel.name}
                            </button>
                          </div>
                          <span className="text-meta text-ink-3">{channel.type}</span>
                        </div>
                      </td>
                      <td className="border-b border-border-hairline px-3 py-3">
                        <ChannelStatusBadge
                          status={channel.status}
                          reason={channel.auto_disabled_reason}
                          updatedAt={channel.updated_at}
                        />
                      </td>
                      <td className="border-b border-border-hairline px-3 py-3">
                        <ChannelModelCell
                          channelId={channel.id}
                          channelName={channel.name}
                          models={channel.supported_models}
                          expanded={expanded}
                          onToggle={() => {
                            setExpandedId(expanded ? null : channel.id)
                          }}
                        />
                      </td>
                      <td className="border-b border-border-hairline px-3 py-3">
                        <div className="flex flex-col gap-1">
                          <GroupsCell groups={channel.group_tags} />
                          <span className="flex items-center gap-3 whitespace-nowrap text-meta text-ink-2">
                            <span>
                              {t('common.channels.table.priority')}{' '}
                              <span className="tabular-nums text-ink-1">{channel.priority}</span>
                            </span>
                            <span>
                              {t('common.channels.table.weight')}{' '}
                              <span className="tabular-nums text-ink-1">{channel.weight}</span>
                            </span>
                          </span>
                        </div>
                      </td>
                      <td className="hidden border-b border-border-hairline px-3 py-3 xl:table-cell">
                        <div className="flex flex-col gap-0.5 whitespace-nowrap">
                          <MetricsSuccessCell channel={channel} />
                          <MetricsLatencyCell channel={channel} />
                        </div>
                      </td>
                      <td className="border-b border-border-hairline px-3 py-3">
                        <div className="flex justify-end">
                          <ChannelRowActions
                            channel={channel}
                            busy={busy}
                            onToggleStatus={() => handleToggleStatus(channel)}
                            onTest={() => handleSingleTest(channel)}
                            onBatchTest={() => {
                              setTestTarget(channel)
                            }}
                            onEdit={() => {
                              setSheet({
                                open: true,
                                mode: 'edit',
                                channelId: channel.id,
                              })
                            }}
                            onDelete={() => {
                              setDeleteTarget({ kind: 'single', channel })
                            }}
                          />
                        </div>
                      </td>
                    </tr>
                    {expanded ? (
                      <tr>
                        <td
                          id={`channel-detail-${channel.id}`}
                          colSpan={7}
                          data-testid={`channel-detail-${channel.id}`}
                          className="border-b border-border-hairline bg-surface-subtle px-5 py-4"
                        >
                          <ChannelDetailPanel
                            channel={channel}
                            onPerformanceTest={() => {
                              setPerfTarget(channel)
                            }}
                          />
                        </td>
                      </tr>
                    ) : null}
                  </Fragment>
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
          onClick={() => {
            setPage((current) => Math.max(1, current - 1))
            clearSelection()
          }}
        >
          {t('common.channels.pagination.previous')}
        </Button>
        <p className="text-body text-ink-2" data-testid="channels-page-indicator">
          {loading ? '—' : t('common.channels.pagination.page', { page, totalPages })}
        </p>
        <div className="flex items-center gap-2">
          <label htmlFor="channel-page-size" className="text-body text-ink-2">
            {t('common.channels.pagination.pageSize')}
          </label>
          <select
            id="channel-page-size"
            value={String(pageSize)}
            disabled={loading}
            onChange={changePageSize}
            className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-50"
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
          onClick={() => {
            setPage((current) => Math.min(totalPages, current + 1))
            clearSelection()
          }}
        >
          {t('common.channels.pagination.next')}
        </Button>
      </div>

      {selected.size > 0 ? (
        <div
          role="toolbar"
          aria-label={t('common.channels.bulk.toolbar')}
          data-testid="bulk-bar"
          className="sticky bottom-4 flex flex-wrap items-center gap-2 rounded-xl border border-border-hairline bg-surface-card p-3 shadow-lg"
        >
          <span className="mr-2 text-body font-medium text-ink-1">
            {t('common.channels.bulk.selected', { count: selected.size })}
          </span>
          <Button
            variant="secondary"
            disabled={bulkDisabled}
            onClick={() => {
              void handleBulkStatus('enable')
            }}
          >
            {t('common.channels.bulk.enable')}
          </Button>
          <Button
            variant="secondary"
            disabled={bulkDisabled}
            onClick={() => {
              void handleBulkStatus('disable')
            }}
          >
            {t('common.channels.bulk.disable')}
          </Button>
          <Button
            variant="danger"
            disabled={bulkDisabled}
            onClick={() => {
              setDeleteTarget({ kind: 'bulk' })
            }}
          >
            {t('common.channels.bulk.delete')}
          </Button>
        </div>
      ) : null}

      <ChannelMutateSheet
        open={sheet.open}
        mode={sheet.open ? sheet.mode : 'create'}
        channelId={sheet.open && sheet.mode === 'edit' ? sheet.channelId : undefined}
        onOpenChange={(open) => {
          if (!open) setSheet({ open: false })
        }}
        onSaved={() => {
          setSheet({ open: false })
          invalidate()
        }}
      />

      <ChannelTestDialog
        open={testTarget !== null}
        channel={testTarget}
        onOpenChange={(open) => {
          if (!open) setTestTarget(null)
        }}
        onCompleted={invalidate}
      />

      <ChannelPerformanceDialog
        open={perfTarget !== null}
        channel={perfTarget}
        onOpenChange={(open) => {
          if (!open) setPerfTarget(null)
        }}
        onCompleted={invalidate}
      />

      <ConfirmDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (!open) setDeleteTarget(null)
        }}
        title={
          deleteTarget?.kind === 'single'
            ? t('common.channels.bulk.singleDeleteTitle')
            : t('common.channels.bulk.deleteTitle')
        }
        description={
          deleteTarget?.kind === 'single'
            ? t('common.channels.bulk.singleDeleteBody')
            : t('common.channels.bulk.deleteBody', { count: deleteCount })
        }
        confirmLabel={t('common.channels.bulk.confirmDelete')}
        cancelLabel={t('common.channels.bulk.cancel')}
        destructive
        busy={deleteBusy}
        onConfirm={() => {
          void handleConfirmDelete()
        }}
      />
    </section>
  )
}

// ---------------------------------------------------------------------------
// Small presentational pieces
// ---------------------------------------------------------------------------

interface SortHeaderProps {
  label: string
  column: ChannelSort
  sort: ChannelSort
  onSort: (column: ChannelSort) => void
  align?: 'left' | 'right'
}

function SortHeader({ label, column, sort, onSort, align = 'left' }: SortHeaderProps) {
  const active = sort === column
  // The backend supports one direction per column (relay-02 routing):
  // priority is descending, name ascending.
  const direction = column === 'name' ? 'ascending' : 'descending'
  const ariaSort: 'ascending' | 'descending' | 'none' = active ? direction : 'none'
  return (
    <th
      scope="col"
      aria-sort={ariaSort}
      className={[
        'whitespace-nowrap border-b border-border-hairline bg-surface-subtle px-3 py-2 text-meta text-ink-2',
        align === 'right' ? 'text-right' : 'text-left',
      ].join(' ')}
    >
      <button
        type="button"
        onClick={() => {
          onSort(column)
        }}
        className="inline-flex items-center gap-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      >
        {label}
        {active ? (
          <span aria-hidden="true">{direction === 'ascending' ? '↑' : '↓'}</span>
        ) : null}
      </button>
    </th>
  )
}

function GroupsCell({ groups }: { groups: string[] | null }) {
  const { t } = useI18n()
  if (groups === null || groups.length === 0) {
    return <span className="text-meta text-ink-2">{t('common.channels.table.allGroups')}</span>
  }
  return (
    <span className="flex flex-wrap gap-1">
      {groups.map((group) => (
        <Tag key={group}>{group}</Tag>
      ))}
    </span>
  )
}

/**
 * 24h success-rate cell (W16-1). `channel.metrics` is absent when the logs
 * database is unavailable and `success_rate` is null when the channel had no
 * traffic — both render a muted dash rather than inventing 0%. A real rate is
 * coloured by threshold (99%+ success, 95%+ warning, lower danger), and when
 * neither metric exists the cell pairs the dash with a muted "No data" line.
 */
function MetricsSuccessCell({ channel }: { channel: Channel }) {
  const { t } = useI18n()
  const rate = channel.metrics?.success_rate ?? null
  const noData =
    rate === null && (channel.metrics?.avg_latency_ms ?? null) === null
  const text = rate === null ? '—' : `${(rate * 100).toFixed(1)}%`
  // Thresholds mirror the approved channel preview: 99%+ is healthy, 95%+
  // needs watching, anything lower is failing. No data stays muted.
  const tone =
    rate === null
      ? 'text-ink-3'
      : rate >= 0.99
        ? 'text-success'
        : rate >= 0.95
          ? 'text-warning'
          : 'text-danger'
  return (
    <span className="flex flex-col gap-0.5">
      <span
        data-testid={`channel-metrics-rate-${channel.id}`}
        aria-label={`${t('common.channels.table.successRate24h')}: ${text}`}
        className={`text-meta font-semibold tabular-nums ${tone}`}
      >
        {text}
      </span>
      {noData ? (
        <span className="text-badge text-ink-3">
          {t('common.channels.table.noHealthData')}
        </span>
      ) : null}
    </span>
  )
}

/** 24h average latency cell (W16-1). Null means "no samples". */
function MetricsLatencyCell({ channel }: { channel: Channel }) {
  const { t } = useI18n()
  const latency = channel.metrics?.avg_latency_ms ?? null
  // With no samples at all the success cell owns the single em dash and the
  // muted "No data" line; this value would otherwise repeat the dash.
  const hasAnyMetrics =
    (channel.metrics?.success_rate ?? null) !== null || latency !== null
  const text = latency === null ? '—' : `${Math.round(latency)} ms`
  return (
    <span
      data-testid={`channel-metrics-latency-${channel.id}`}
      aria-label={`${t('common.channels.table.avgLatency24h')}: ${text}`}
      className={[
        'text-meta tabular-nums text-ink-2',
        hasAnyMetrics ? '' : 'hidden',
      ].join(' ')}
    >
      {text}
    </span>
  )
}
