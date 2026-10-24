import { useMutation } from '@tanstack/react-query'
import { useEffect, useState, type FormEvent } from 'react'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Sheet } from '@/shared/ui/base/sheet'
import { FormAlert } from '@/shared/ui/form'

import {
  ackItem,
  createIdempotencyKey,
  type ReconcileAckRequest,
  type ReconcileItem,
} from './api'

export interface ItemSheetProps {
  /** `null` 表示抽屉关闭；由父组件从最新的列表数据里解析，确认后自动刷新。 */
  item: ReconcileItem | null
  onOpenChange: (open: boolean) => void
  canManage: boolean
  /** 确认成功后的回调（结果 Tab 用它失效明细列表）。 */
  onAckSuccess?: () => void
}

type AckStatus = NonNullable<ReconcileAckRequest['status']>

function problemMessage(error: unknown): string {
  if (error instanceof ApiError) return error.detail ?? error.title
  return error instanceof Error ? error.message : String(error)
}

/** NULL 一律显示长破折号：既不是 0，也不是 NaN/null。 */
function formatCount(value: number | null | undefined): string {
  return value === null || value === undefined ? '—' : String(value)
}

function formatMinor(value: number | null | undefined): string {
  if (value === null || value === undefined) return '—'
  return `¥${(value / 100).toFixed(2)}`
}

interface ComparisonRow {
  key: string
  label: string
  local: number | null
  upstream: number | null
  delta: number | null
  money?: boolean
}

/**
 * 六行对照：缓存命中 / 未命中 / 写入、输出、请求数、金额。
 * 本地没有缓存写入与账单金额这两个口径，对应单元格保持 NULL（—），
 * 不拿别的字段代替。
 */
function buildRows(item: ReconcileItem, t: (key: string) => string): ComparisonRow[] {
  const snapshotRequests = item.snapshot_requests ?? null
  return [
    {
      key: 'cache_hit',
      label: t('admin.upstream_reconciliation.item.metric_cache_hit'),
      local: item.local_cache_hit_tokens,
      upstream: item.snapshot_cache_hit_tokens ?? null,
      delta: item.delta_cache_hit_tokens ?? null,
    },
    {
      key: 'cache_miss',
      label: t('admin.upstream_reconciliation.item.metric_cache_miss'),
      local: item.local_cache_miss_tokens,
      upstream: item.snapshot_cache_miss_tokens ?? null,
      delta: item.delta_cache_miss_tokens ?? null,
    },
    {
      key: 'cache_write',
      label: t('admin.upstream_reconciliation.item.metric_cache_write'),
      local: null,
      upstream: item.snapshot_cache_write_tokens ?? null,
      delta: null,
    },
    {
      key: 'output',
      label: t('admin.upstream_reconciliation.item.metric_output'),
      local: item.local_output_tokens,
      upstream: item.snapshot_output_tokens ?? null,
      delta: item.delta_output_tokens ?? null,
    },
    {
      key: 'requests',
      label: t('admin.upstream_reconciliation.item.metric_requests'),
      local: item.local_requests,
      upstream: snapshotRequests,
      delta:
        snapshotRequests === null ? null : item.local_requests - snapshotRequests,
    },
    {
      key: 'amount',
      label: t('admin.upstream_reconciliation.item.metric_amount'),
      local: null,
      upstream: item.snapshot_amount_minor ?? null,
      delta: null,
      money: true,
    },
  ]
}

/**
 * 对账明细详情抽屉（任务 14）。
 *
 * 所有数字直接来自 run 的 item 行；确认动作的 `note` 必须始终出现在请求
 * 体里（契约 required，空串合法，缺字段 400），因此 textarea 无论是否
 * 填写都原样提交。
 */
export function ItemSheet({
  item,
  onOpenChange,
  canManage,
  onAckSuccess,
}: ItemSheetProps) {
  const { t } = useI18n()
  const [status, setStatus] = useState<AckStatus>('acknowledged')
  const [note, setNote] = useState('')

  const itemId = item?.id ?? null

  const ackMutation = useMutation({
    mutationFn: (payload: { id: number; status: AckStatus; note: string }) =>
      ackItem(
        payload.id,
        { status: payload.status, note: payload.note },
        createIdempotencyKey(),
      ),
    retry: false,
    onSuccess: () => {
      onAckSuccess?.()
    },
  })
  const resetAck = ackMutation.reset

  // 切换到另一条明细时清空上一次的草稿与结果；同一明细刷新（确认后）则
  // 保留刚输入的说明。
  useEffect(() => {
    setStatus('acknowledged')
    setNote('')
    resetAck()
  }, [itemId, resetAck])

  function submit(event: FormEvent<HTMLFormElement>): void {
    event.preventDefault()
    if (item === null) return
    ackMutation.mutate({ id: item.id, status, note })
  }

  const rows = item === null ? [] : buildRows(item, t)
  const formula =
    item === null
      ? ''
      : t('admin.upstream_reconciliation.item.formula', {
          delta_hit: formatCount(item.delta_cache_hit_tokens),
          delta_miss: formatCount(item.delta_cache_miss_tokens),
          delta_output: formatCount(item.delta_output_tokens),
          snapshot_hit: formatCount(item.snapshot_cache_hit_tokens),
          snapshot_miss: formatCount(item.snapshot_cache_miss_tokens),
          snapshot_output: formatCount(item.snapshot_output_tokens),
          delta_bp: formatCount(item.delta_token_bp),
        })

  return (
    <Sheet
      open={item !== null}
      onOpenChange={onOpenChange}
      widthClassName="max-w-[560px]"
      title={
        item === null
          ? ''
          : t('admin.upstream_reconciliation.item.title', {
              model: item.upstream_model,
              date: item.usage_date,
            })
      }
      description={
        item === null
          ? undefined
          : t('admin.upstream_reconciliation.item.subtitle')
      }
      footer={
        <>
          <Button variant="secondary" onClick={() => onOpenChange(false)}>
            {t('common.action.close')}
          </Button>
          <Button
            type="submit"
            form="reconcile-item-ack-form"
            variant="secondary"
            disabled={!canManage || ackMutation.isPending}
          >
            {ackMutation.isPending
              ? t('admin.upstream_reconciliation.item.ack_saving')
              : t('admin.upstream_reconciliation.item.ack')}
          </Button>
        </>
      }
    >
      {item === null ? null : (
        <div className="flex flex-col gap-4">
          <table className="w-full text-left text-body">
            <thead className="text-meta text-ink-3">
              <tr>
                <th scope="col" className="py-1 pr-2 font-medium">
                  {t('admin.upstream_reconciliation.item.col_metric')}
                </th>
                <th scope="col" className="py-1 pl-2 text-right font-medium">
                  {t('admin.upstream_reconciliation.item.col_local')}
                </th>
                <th scope="col" className="py-1 pl-2 text-right font-medium">
                  {t('admin.upstream_reconciliation.item.col_upstream')}
                </th>
                <th scope="col" className="py-1 pl-2 text-right font-medium">
                  {t('admin.upstream_reconciliation.item.col_delta')}
                </th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => (
                <tr
                  key={row.key}
                  data-testid={`comparison-row-${row.key}`}
                  className="border-t border-border-hairline"
                >
                  <td className="py-1.5 pr-2 text-ink-1">{row.label}</td>
                  <td className="py-1.5 pl-2 text-right font-mono tabular-nums text-ink-1">
                    {row.money ? formatMinor(row.local) : formatCount(row.local)}
                  </td>
                  <td className="py-1.5 pl-2 text-right font-mono tabular-nums text-ink-1">
                    {row.money
                      ? formatMinor(row.upstream)
                      : formatCount(row.upstream)}
                  </td>
                  <td className="py-1.5 pl-2 text-right font-mono tabular-nums text-ink-1">
                    {row.money ? formatMinor(row.delta) : formatCount(row.delta)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>

          <div className="rounded-lg border border-border-hairline bg-surface-subtle p-3">
            <p className="text-meta font-medium text-ink-1">
              {t('admin.upstream_reconciliation.item.formula_title')}
            </p>
            <code className="mt-1 block whitespace-pre-wrap font-mono text-meta text-ink-2">
              {formula}
            </code>
          </div>

          {!canManage ? (
            <FormAlert tone="info">
              {t('admin.upstream_reconciliation.item.read_only')}
            </FormAlert>
          ) : null}

          <form
            id="reconcile-item-ack-form"
            className="flex flex-col gap-3"
            onSubmit={submit}
          >
            <label
              htmlFor="reconcile-item-ack-status"
              className="flex flex-col gap-1.5 text-meta font-medium text-ink-1"
            >
              {t('admin.upstream_reconciliation.item.ack_status')}
              <select
                id="reconcile-item-ack-status"
                value={status}
                disabled={!canManage || ackMutation.isPending}
                onChange={(event) => {
                  setStatus(event.target.value as AckStatus)
                }}
                className="h-10 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 disabled:cursor-not-allowed disabled:opacity-50"
              >
                <option value="acknowledged">
                  {t('admin.upstream_reconciliation.results.status_acknowledged')}
                </option>
                <option value="resolved">
                  {t('admin.upstream_reconciliation.results.status_resolved')}
                </option>
              </select>
            </label>

            <label
              htmlFor="reconcile-item-ack-note"
              className="flex flex-col gap-1.5 text-meta font-medium text-ink-1"
            >
              {t('admin.upstream_reconciliation.item.note')}
              <textarea
                id="reconcile-item-ack-note"
                value={note}
                maxLength={2000}
                rows={3}
                disabled={!canManage || ackMutation.isPending}
                placeholder={t(
                  'admin.upstream_reconciliation.item.note_placeholder',
                )}
                onChange={(event) => {
                  setNote(event.target.value)
                }}
                className="rounded-lg border border-border-default bg-surface-card px-3 py-2 text-body text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50"
              />
            </label>

            {ackMutation.isError ? (
              <FormAlert tone="danger">
                {problemMessage(ackMutation.error)}
              </FormAlert>
            ) : null}
            {ackMutation.isSuccess ? (
              <FormAlert tone="success">
                {t('admin.upstream_reconciliation.item.ack_success')}
              </FormAlert>
            ) : null}
          </form>
        </div>
      )}
    </Sheet>
  )
}
