/**
 * 模型对比弹框。
 *
 * 表格按「字段为行、模型为列」排布：列顺序**就是 URL 里 compare 的顺序**，
 * 不按目录顺序重排，这样用户分享的链接与打开的表格一致。所有读数直接取自
 * 页面已经算好的 `entry.perf`——这里不发请求，缺值一律渲染 `—`。
 *
 * 移除 / 清空是可选能力：页面负责把结果写回 URL，组件只调用回调；没有
 * 传入回调时不渲染对应按钮（避免出现点了没反应的控件）。
 */

import type { ReactNode } from 'react'

import { formatContextTokens } from '@/features/public/pricing/format'
import { useI18n } from '@/shared/lib/i18n-context'
import { Dialog } from '@/shared/ui/base/dialog'

import {
  formatMs,
  formatPercent,
  formatSamples,
  formatTPS,
} from './format'
import type { ModelEntry, PerfSource } from './performance'
import { uniqueCapabilityNames } from './taxonomy'

export interface CompareDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** 顺序即 URL 里 compare 的顺序。 */
  entries: readonly ModelEntry[]
  onRemove?: (modelId: string) => void
  onClear?: () => void
}

function displayNameOf(entry: ModelEntry): string {
  return entry.card.display_name === ''
    ? entry.card.model_id
    : entry.card.display_name
}

function sourceLabel(source: PerfSource, t: (key: string) => string): string {
  if (source === 'test') return t('public.models.source.test')
  if (source === 'usage') return t('public.models.source.usage')
  return t('public.models.source.none')
}

function RowLabel({ children }: { children: ReactNode }) {
  return (
    <th
      scope="row"
      className="w-32 shrink-0 border-t border-border-hairline py-2 pr-3 text-left align-top text-badge font-medium text-ink-3"
    >
      {children}
    </th>
  )
}

function Cell({ children }: { children: ReactNode }) {
  return (
    <td className="border-t border-border-hairline px-3 py-2 align-top text-meta tabular-nums text-ink-1">
      {children}
    </td>
  )
}

export function CompareDialog({
  open,
  onOpenChange,
  entries,
  onRemove,
  onClear,
}: CompareDialogProps) {
  const { t, locale } = useI18n()

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={t('public.models.compare.title')}
      panelClassName="max-w-none! xl:max-w-[960px]!"
      closeLabel={t('public.models.compare.close')}
      footer={
        onClear === undefined ? undefined : (
          <button
            type="button"
            onClick={onClear}
            className="rounded-md border border-border-default px-3 py-1 text-meta font-medium text-ink-1 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            {t('public.models.compare.clear')}
          </button>
        )
      }
    >
      {entries.length === 0 ? (
        <p className="text-body text-ink-2">{t('public.models.compare.empty')}</p>
      ) : (
        <>
          <p className="mb-2 text-badge text-ink-3 md:hidden">
            {t('public.models.compare.scroll_hint')}
          </p>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[640px] border-collapse">
              <caption className="sr-only">
                {t('public.models.compare.title')}
              </caption>
              <thead>
                <tr>
                  <th scope="col" className="w-32 pb-2 pr-3 text-left">
                    <span className="sr-only">
                      {t('public.models.compare.title')}
                    </span>
                  </th>
                  {entries.map((entry) => (
                    <th
                      key={entry.card.model_id}
                      scope="col"
                      className="px-3 pb-2 text-left align-top"
                    >
                      <div className="flex items-start justify-between gap-2">
                        <div className="min-w-0">
                          <div className="text-meta font-semibold text-ink-1">
                            {displayNameOf(entry)}
                          </div>
                          <div className="break-all font-mono text-badge font-normal text-ink-3">
                            {entry.card.model_id}
                          </div>
                        </div>
                        {onRemove === undefined ? null : (
                          <button
                            type="button"
                            aria-label={t('public.models.compare.remove', {
                              name: displayNameOf(entry),
                            })}
                            onClick={() => onRemove(entry.card.model_id)}
                            className="shrink-0 rounded p-0.5 leading-none text-ink-3 hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                          >
                            <span aria-hidden="true">×</span>
                          </button>
                        )}
                      </div>
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                <tr>
                  <RowLabel>{t('public.models.compare.vendor')}</RowLabel>
                  {entries.map((entry) => (
                    <Cell key={entry.card.model_id}>
                      {entry.card.vendor === '' ? '—' : entry.card.vendor}
                    </Cell>
                  ))}
                </tr>
                <tr>
                  <RowLabel>{t('public.models.table.capabilities')}</RowLabel>
                  {entries.map((entry) => {
                    const names = uniqueCapabilityNames(entry.card.capabilities)
                    return (
                      <Cell key={entry.card.model_id}>
                        {names.length === 0
                          ? '—'
                          : names.join(t('public.models.list_separator'))}
                      </Cell>
                    )
                  })}
                </tr>
                <tr>
                  <RowLabel>{t('public.models.table.context')}</RowLabel>
                  {entries.map((entry) => {
                    const { card } = entry
                    const context =
                      card.context_length === null
                        ? '—'
                        : formatContextTokens(card.context_length, locale)
                    const output =
                      card.max_output === null
                        ? '—'
                        : formatContextTokens(card.max_output, locale)
                    return (
                      <Cell key={card.model_id}>
                        <span className="font-mono">{context}</span>
                        <span className="block text-badge text-ink-3">
                          {t('public.models.detail.max_output')} {output}
                        </span>
                      </Cell>
                    )
                  })}
                </tr>
                <tr>
                  <RowLabel>{t('public.models.table.latency')}</RowLabel>
                  {entries.map((entry) => (
                    <Cell key={entry.card.model_id}>
                      <span className="font-mono">
                        {formatMs(entry.perf.p50)}
                      </span>
                      <span className="block font-mono text-badge text-ink-3">
                        P95 {formatMs(entry.perf.p95)}
                      </span>
                    </Cell>
                  ))}
                </tr>
                <tr>
                  <RowLabel>{t('public.models.table.speed')}</RowLabel>
                  {entries.map((entry) => (
                    <Cell key={entry.card.model_id}>
                      {formatTPS(entry.perf.tps)}
                    </Cell>
                  ))}
                </tr>
                <tr>
                  <RowLabel>{t('public.models.table.success')}</RowLabel>
                  {entries.map((entry) => (
                    <Cell key={entry.card.model_id}>
                      {formatPercent(entry.perf.successRate)}
                    </Cell>
                  ))}
                </tr>
                <tr>
                  <RowLabel>{t('public.models.compare.samples')}</RowLabel>
                  {entries.map((entry) => (
                    <Cell key={entry.card.model_id}>
                      {formatSamples(entry.perf.samples, locale)}
                    </Cell>
                  ))}
                </tr>
                <tr>
                  <RowLabel>{t('public.models.compare.source')}</RowLabel>
                  {entries.map((entry) => (
                    <Cell key={entry.card.model_id}>
                      {sourceLabel(entry.perf.source, t)}
                    </Cell>
                  ))}
                </tr>
              </tbody>
            </table>
          </div>
        </>
      )}
    </Dialog>
  )
}
