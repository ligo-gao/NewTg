import { useQueries, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'
import { StatusBadge } from '@/shared/ui/status-badge'

import {
  listItems,
  listRuns,
  type ReconcileItem,
  type ReconcileSeverity,
} from './api'
import { ItemSheet } from './item-sheet'
import {
  SEVERITY_LABEL_KEYS,
  SEVERITY_ORDER,
  itemStatusLabel,
  itemStatusVariant,
  severityLabel,
  severityVariant,
} from './status'

/** 运行列表与明细列表共享前缀：详情抽屉确认后按前缀失效。 */
const RUNS_QUERY_KEY = ['upstream-reconciliations', 'runs'] as const
const ITEMS_QUERY_KEY = ['upstream-reconciliations', 'items'] as const

/** 与服务端 offset 分页契约一致；筛选变化时回到第一页。 */
const PAGE_SIZE = 20

export interface ResultsTabProps {
  runId: number | null
  onSelectRun: (id: number | null) => void
  canManage: boolean
}

function problemMessage(error: unknown): string {
  if (error instanceof ApiError) return error.detail ?? error.title
  return error instanceof Error ? error.message : String(error)
}

/** NULL 一律显示长破折号：既不是 0，也不是 NaN/null。 */
function formatCount(value: number | null | undefined): string {
  return value === null || value === undefined ? '—' : String(value)
}

/**
 * 金额线上是整数分；`toFixed` 只做格式化，不参与任何求和。
 * 负毛利保持 `¥-12.34` 的形式（符号属于数值，不另造前缀）。
 */
function formatMinor(value: number | null | undefined): string {
  if (value === null || value === undefined) return '—'
  return `¥${(value / 100).toFixed(2)}`
}

function localTokenTotal(item: ReconcileItem): number {
  return (
    item.local_cache_hit_tokens +
    item.local_cache_miss_tokens +
    item.local_output_tokens
  )
}

/**
 * 上游 token 合计只在三个维度都上报时可计算；缺任一维度时快照只是
 * 部分口径，合计会误导，故返回 NULL。
 */
function snapshotTokenTotal(item: ReconcileItem): number | null {
  const hit = item.snapshot_cache_hit_tokens
  const miss = item.snapshot_cache_miss_tokens
  const output = item.snapshot_output_tokens
  if (hit === null || hit === undefined) return null
  if (miss === null || miss === undefined) return null
  if (output === null || output === undefined) return null
  return hit + miss + output
}

/**
 * 对账结果 Tab（任务 14）。
 *
 * 运行选择由页面持有（`runId`），本组件只负责读运行列表并在尚未选择时
 * 自动选中最新一条。严重度计数没有独立端点，因此对 7 个严重度各发一次
 * `page_size=1` 的列表请求读 `total`——它们按运行缓存，不随筛选变化重取。
 * 表格的分页与严重度筛选全部交给服务端。
 */
export function ResultsTab({ runId, onSelectRun, canManage }: ResultsTabProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [severity, setSeverity] = useState<ReconcileSeverity | null>(null)
  const [page, setPage] = useState(1)
  const [selectedId, setSelectedId] = useState<number | null>(null)

  const runsQuery = useQuery({
    queryKey: RUNS_QUERY_KEY,
    queryFn: ({ signal }) => listRuns({}, signal),
    retry: false,
    staleTime: 30_000,
  })
  const runs = runsQuery.data?.items ?? []

  // 页面首次进入时没有选中运行：运行列表加载完成后选最新一条（服务端
  // 已按 started_at 降序返回）。只在 runId 仍为空时补选一次。
  useEffect(() => {
    if (runId !== null) return
    const newest = runsQuery.data?.items[0]
    if (newest !== undefined) onSelectRun(newest.id)
  }, [runId, runsQuery.data, onSelectRun])

  const countQueries = useQueries({
    queries: SEVERITY_ORDER.map((value) => ({
      queryKey: [...ITEMS_QUERY_KEY, 'count', runId, value] as const,
      queryFn: ({ signal }: { signal: AbortSignal }) => {
        if (runId === null) throw new Error('count query without a run id')
        return listItems(runId, { severity: value, page_size: 1 }, signal)
      },
      enabled: runId !== null,
      retry: false,
      staleTime: 30_000,
    })),
  })

  const severityCounts: Partial<Record<ReconcileSeverity, number>> = {}
  SEVERITY_ORDER.forEach((value, index) => {
    const total = countQueries[index]?.data?.total
    if (total !== undefined) severityCounts[value] = total
  })

  const itemsQuery = useQuery({
    queryKey: [...ITEMS_QUERY_KEY, 'list', runId, severity, page] as const,
    queryFn: ({ signal }) => {
      if (runId === null) throw new Error('items query without a run id')
      return listItems(
        runId,
        {
          severity: severity ?? undefined,
          p: page,
          page_size: PAGE_SIZE,
        },
        signal,
      )
    },
    enabled: runId !== null,
    retry: false,
    // 只在运行不变时沿用上一份数据（翻页 / 筛选）；换运行必须先清空表格，
    // 否则旧运行的行会挂在新运行的标题下并仍然可点。
    placeholderData: (previous, previousQuery) => {
      if (previous === undefined) return undefined
      const previousRunId = previousQuery?.queryKey[ITEMS_QUERY_KEY.length + 1]
      return previousRunId === runId ? previous : undefined
    },
  })

  const items = itemsQuery.data?.items ?? []
  const total = itemsQuery.data?.total ?? 0
  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE))
  // 占位数据（上一页 / 上一筛选）不是当前请求的结果：据此解析详情会让抽屉
  // 显示一条不属于当前视图的明细。
  const selectedItem =
    selectedId === null || itemsQuery.isPlaceholderData
      ? null
      : (items.find((item) => item.id === selectedId) ?? null)

  function chooseSeverity(next: ReconcileSeverity | null): void {
    setSeverity(next)
    setPage(1)
  }

  function changeRun(value: string): void {
    const next = Number(value)
    onSelectRun(Number.isInteger(next) && next > 0 ? next : null)
    setSeverity(null)
    setPage(1)
    setSelectedId(null)
  }

  return (
    <div className="flex flex-col gap-3" data-testid="results-tab">
      <div className="flex flex-wrap items-end gap-3">
        <label className="flex flex-col gap-1 text-meta text-ink-2">
          {t('admin.upstream_reconciliation.results.run')}
          <select
            aria-label={t('admin.upstream_reconciliation.results.run')}
            value={runId === null ? '' : String(runId)}
            disabled={runsQuery.isPending}
            onChange={(event) => {
              changeRun(event.target.value)
            }}
            className="h-10 min-w-64 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1 disabled:cursor-not-allowed disabled:opacity-50"
          >
            <option value="">
              {t('admin.upstream_reconciliation.results.select_run')}
            </option>
            {runs.map((run) => (
              <option key={run.id} value={String(run.id)}>
                {t('admin.upstream_reconciliation.results.run_option', {
                  id: run.id,
                  start: run.window_start,
                  end: run.window_end,
                })}
              </option>
            ))}
          </select>
        </label>
      </div>

      {runsQuery.isError ? (
        <FormAlert tone="danger">{problemMessage(runsQuery.error)}</FormAlert>
      ) : null}
      {runsQuery.isSuccess && runs.length === 0 ? (
        <FormAlert tone="info">
          {t('admin.upstream_reconciliation.results.no_runs')}
        </FormAlert>
      ) : null}
      {runId === null && runs.length > 0 ? (
        <FormAlert tone="info">
          {t('admin.upstream_reconciliation.results.select_hint')}
        </FormAlert>
      ) : null}

      {runId !== null ? (
        <>
          <div
            role="group"
            aria-label={t('admin.upstream_reconciliation.results.col_severity')}
            className="flex flex-wrap gap-2"
          >
            <button
              type="button"
              aria-pressed={severity === null}
              onClick={() => {
                chooseSeverity(null)
              }}
              className={chipClasses(severity === null)}
            >
              {t('admin.upstream_reconciliation.results.filter_all')}
            </button>
            {SEVERITY_ORDER.map((value) => {
              const count = severityCounts[value]
              const label = t(SEVERITY_LABEL_KEYS[value])
              return (
                <button
                  key={value}
                  type="button"
                  aria-pressed={severity === value}
                  onClick={() => {
                    chooseSeverity(value)
                  }}
                  className={chipClasses(severity === value)}
                >
                  {count === undefined
                    ? label
                    : t('admin.upstream_reconciliation.results.chip_count', {
                        label,
                        count,
                      })}
                </button>
              )
            })}
          </div>

          {itemsQuery.isError ? (
            <FormAlert tone="danger">
              {problemMessage(itemsQuery.error)}
            </FormAlert>
          ) : null}

          {itemsQuery.isPending ? (
            <p className="text-body text-ink-3">
              {t('admin.upstream_reconciliation.results.loading')}
            </p>
          ) : null}

          {itemsQuery.isSuccess && items.length === 0 ? (
            <FormAlert tone="info">
              {severity === null
                ? t('admin.upstream_reconciliation.results.empty')
                : t('admin.upstream_reconciliation.results.empty_filtered')}
            </FormAlert>
          ) : null}

          {items.length > 0 ? (
            <div className="overflow-x-auto rounded-xl border border-border-default">
              <table className="w-full min-w-[1180px] text-left text-body">
                <thead className="text-meta text-ink-3">
                  <tr>
                    <th scope="col" className="px-4 py-2 font-medium">
                      {t('admin.upstream_reconciliation.results.col_date')}
                    </th>
                    <th scope="col" className="px-4 py-2 font-medium">
                      {t('admin.upstream_reconciliation.results.col_model')}
                    </th>
                    <th scope="col" className="px-4 py-2 font-medium">
                      {t('admin.upstream_reconciliation.results.col_severity')}
                    </th>
                    <th scope="col" className="px-4 py-2 text-right font-medium">
                      {t(
                        'admin.upstream_reconciliation.results.col_delta_bp',
                      )}
                    </th>
                    <th scope="col" className="px-4 py-2 text-right font-medium">
                      {t(
                        'admin.upstream_reconciliation.results.col_local_tokens',
                      )}
                    </th>
                    <th scope="col" className="px-4 py-2 text-right font-medium">
                      {t(
                        'admin.upstream_reconciliation.results.col_snapshot_tokens',
                      )}
                    </th>
                    <th scope="col" className="px-4 py-2 text-right font-medium">
                      {t('admin.upstream_reconciliation.results.col_revenue')}
                    </th>
                    <th scope="col" className="px-4 py-2 text-right font-medium">
                      {t(
                        'admin.upstream_reconciliation.results.col_upstream_cost',
                      )}
                    </th>
                    <th scope="col" className="px-4 py-2 text-right font-medium">
                      {t('admin.upstream_reconciliation.results.col_margin')}
                    </th>
                    <th scope="col" className="px-4 py-2 font-medium">
                      {t('admin.upstream_reconciliation.results.col_status')}
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {items.map((item) => (
                    <tr
                      key={item.id}
                      data-testid="item-row"
                      className="cursor-pointer border-t border-border-hairline last:border-0 hover:bg-surface-hover"
                      onClick={() => {
                        setSelectedId(item.id)
                      }}
                    >
                      <td className="px-4 py-3 font-mono tabular-nums text-ink-2">
                        {item.usage_date}
                      </td>
                      <td className="px-4 py-3">
                        <button
                          type="button"
                          aria-label={t(
                            'admin.upstream_reconciliation.results.row_action',
                            { model: item.upstream_model, date: item.usage_date },
                          )}
                          onClick={(event) => {
                            // 行本身也可打开；按钮用于键盘与读屏，避免冒泡触发两次。
                            event.stopPropagation()
                            setSelectedId(item.id)
                          }}
                          className="font-mono text-meta text-brand hover:underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                        >
                          {item.upstream_model}
                        </button>
                      </td>
                      <td className="px-4 py-3">
                        <StatusBadge variant={severityVariant(item.severity)}>
                          {severityLabel(item.severity, t)}
                        </StatusBadge>
                      </td>
                      <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                        {formatCount(item.delta_token_bp)}
                      </td>
                      <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                        {formatCount(localTokenTotal(item))}
                      </td>
                      <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                        {formatCount(snapshotTokenTotal(item))}
                      </td>
                      <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                        {formatMinor(item.revenue_minor)}
                      </td>
                      <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                        {formatMinor(item.upstream_cost_minor)}
                      </td>
                      <td className="px-4 py-3 text-right font-mono tabular-nums text-ink-1">
                        {formatMinor(item.margin_minor)}
                      </td>
                      <td className="px-4 py-3">
                        <StatusBadge variant={itemStatusVariant(item.status)}>
                          {itemStatusLabel(item.status, t)}
                        </StatusBadge>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : null}

          {items.length > 0 ? (
            <div className="flex items-center justify-end gap-3 text-meta text-ink-2">
              <span className="tabular-nums">
                {t('admin.upstream_reconciliation.results.total', {
                  count: total,
                })}
              </span>
              <Button
                variant="secondary"
                className="h-8 px-2 text-meta"
                disabled={itemsQuery.isFetching || page <= 1}
                onClick={() => {
                  setPage((current) => Math.max(1, current - 1))
                }}
              >
                {t('admin.upstream_reconciliation.results.pagination_previous')}
              </Button>
              <span className="font-mono tabular-nums">
                {t('admin.upstream_reconciliation.results.pagination_page', {
                  page,
                  pages,
                })}
              </span>
              <Button
                variant="secondary"
                className="h-8 px-2 text-meta"
                disabled={itemsQuery.isFetching || page >= pages}
                onClick={() => {
                  setPage((current) => current + 1)
                }}
              >
                {t('admin.upstream_reconciliation.results.pagination_next')}
              </Button>
            </div>
          ) : null}
        </>
      ) : null}

      <ItemSheet
        item={selectedItem}
        onOpenChange={(open) => {
          if (!open) setSelectedId(null)
        }}
        canManage={canManage}
        onAckSuccess={() => {
          // 行上的状态徽标与严重度计数都以服务端为准：确认后按前缀失效。
          void queryClient.invalidateQueries({ queryKey: ITEMS_QUERY_KEY })
        }}
      />
    </div>
  )
}

function chipClasses(active: boolean): string {
  return [
    'h-8 rounded-full border px-3 text-meta',
    active
      ? 'border-brand-line bg-brand-subtle font-medium text-brand'
      : 'border-border-default bg-surface-card text-ink-2 hover:bg-surface-hover hover:text-ink-1',
  ].join(' ')
}
