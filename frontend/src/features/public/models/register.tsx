/**
 * 模型登记册（规格表 + 卡片）。
 *
 * 两种视图消费同一份 `ModelEntry`，只负责渲染：不发请求、不读 URL，展开
 * 状态与对比集合都由页面传入。读数来源必须诚实：`p50` 缺失时数字是 `—`
 * 而不是 `0`，`p95` 缺失时条形只画刻度点、标签写 `—`，均值来源额外标注
 * 「average」——均值不是分位数，绝不能冒充 P95。
 */

import { Fragment } from 'react'

import { formatContextTokens } from '@/features/public/pricing/format'
import { useI18n } from '@/shared/lib/i18n-context'
import type { TranslateFn } from '@/shared/lib/i18n-context'
import { useMediaQuery } from '@/shared/lib/use-media-query'

import {
  formatMs,
  formatPercent,
  formatSamples,
  formatTPS,
  latencyStripGeom,
} from './format'
import { ModelDetail } from './model-detail'
import { MAX_COMPARE, type ModelsView } from './params'
import type { ModelEntry, ModelPerformance, PerfSource } from './performance'
import { monogram, type CapabilitySlot } from './taxonomy'

export interface RegisterProps {
  entries: readonly ModelEntry[]
  view: ModelsView
  compareIds: readonly string[]
  slots: readonly CapabilitySlot[]
  expandedId: string | null
  onToggleExpand: (modelId: string) => void
  onToggleCompare: (modelId: string) => void
  onOpenCompare: () => void
  onCall: (entry: ModelEntry) => void
}

/** Table semantics need the column count once, for the expanded row's span. */
const TABLE_COLUMNS = 9

/** 视图切换宽度的唯一来源：1440px 以下表格放不下，统一落到卡片。 */
const WIDE_QUERY = '(min-width: 1440px)'

interface RowActions {
  expandedId: string | null
  compareIds: readonly string[]
  slots: readonly CapabilitySlot[]
  onToggleExpand: (modelId: string) => void
  onToggleCompare: (modelId: string) => void
  onCall: (entry: ModelEntry) => void
}

function detailIdOf(modelId: string): string {
  return `model-detail-${modelId}`
}

function displayNameOf(entry: ModelEntry): string {
  return entry.card.display_name === '' ? entry.card.model_id : entry.card.display_name
}

function sourceLabel(source: PerfSource, t: TranslateFn): string {
  if (source === 'test') return t('public.models.source.test')
  if (source === 'usage') return t('public.models.source.usage')
  return t('public.models.source.none')
}

function measureLabel(measure: ModelPerformance['measure'], t: TranslateFn): string {
  return measure === 'ttft'
    ? t('public.models.source.measure.ttft')
    : t('public.models.source.measure.latency')
}

/** 大小写变体与重复项只保留首次出现；未知能力原样保留。 */
function capabilityNames(capabilities: readonly string[]): string[] {
  const seen = new Set<string>()
  const out: string[] = []
  for (const raw of capabilities) {
    if (typeof raw !== 'string') continue
    const label = raw.trim()
    if (label === '') continue
    const value = label.toLowerCase()
    if (seen.has(value)) continue
    seen.add(value)
    out.push(label)
  }
  return out
}

function Monogram({ entry }: { entry: ModelEntry }) {
  return (
    <span
      className="monogram flex size-7 shrink-0 items-center justify-center border border-border-default bg-surface-subtle font-mono text-badge text-ink-2"
      aria-hidden="true"
    >
      {monogram(entry.card.vendor, displayNameOf(entry))}
    </span>
  )
}

function ModelIdentity({ entry }: { entry: ModelEntry }) {
  return (
    <div className="model-cell flex min-w-0 items-center gap-2.5">
      <Monogram entry={entry} />
      <div className="min-w-0">
        <div className="model-name text-meta font-semibold leading-5 text-ink-1">
          {displayNameOf(entry)}
        </div>
        <div className="model-id break-all font-mono text-badge text-ink-3">
          {entry.card.model_id}
        </div>
        {entry.card.vendor !== '' ? (
          <div className="text-badge text-ink-3">{entry.card.vendor}</div>
        ) : null}
      </div>
    </div>
  )
}

function CapabilityBarcode({
  entry,
  slots,
}: {
  entry: ModelEntry
  slots: readonly CapabilitySlot[]
}) {
  const { t } = useI18n()
  const names = capabilityNames(entry.card.capabilities)
  const present = new Set(names.map((value) => value.toLowerCase()))
  const label = names.join(t('public.models.list_separator'))

  return (
    <div
      className="barcode flex h-4 items-center gap-0.5"
      role={label === '' ? undefined : 'img'}
      aria-label={label}
      title={label}
    >
      {slots.map((slot) => (
        <i
          key={slot.value}
          aria-hidden="true"
          className={present.has(slot.value) ? 'on' : ''}
        />
      ))}
    </div>
  )
}

function LatencyCell({ perf }: { perf: ModelPerformance }) {
  const { t } = useI18n()
  const geometry = latencyStripGeom(perf.p50, perf.p95)
  const measure = measureLabel(perf.measure, t)
  const stripLabel = `${measure} P50 ${formatMs(perf.p50)} · P95 ${formatMs(perf.p95)}`
  const markerLeft = geometry.markerOnly
    ? geometry.leftPct
    : geometry.leftPct + geometry.widthPct

  return (
    <div className="lat-cell">
      <div className="lat-row flex items-center gap-2">
        <span className="metric shrink-0 font-mono text-meta tabular-nums text-ink-1">
          {formatMs(perf.p50)}
        </span>
        <span
          className={`range-strip relative h-2 w-[90px] shrink-0 border-b border-border-default${
            perf.p50 === null ? ' is-empty' : ''
          }`}
          role="img"
          aria-label={stripLabel}
        >
          {perf.p50 !== null ? (
            <span
              className="range-p50 absolute bottom-0 left-0 h-1 bg-brand"
              style={{ width: `${geometry.leftPct}%` }}
            />
          ) : null}
          {geometry.markerOnly ? null : (
            <span
              className="range-p95 absolute bottom-0 h-1 bg-brand-line"
              style={{
                left: `${geometry.leftPct}%`,
                width: `${geometry.widthPct}%`,
              }}
            />
          )}
          <span
            className="range-tick absolute bottom-0 h-2 w-px bg-brand-hover"
            style={{ left: `${markerLeft}%` }}
          />
        </span>
      </div>
      <span className="metric-sub block font-mono text-badge tabular-nums text-ink-3">
        {measure} P50 {formatMs(perf.p50)} · P95 {formatMs(perf.p95)}
      </span>
      <span className="metric-sub flex flex-wrap items-center gap-x-1 text-badge text-ink-3">
        <span>{sourceLabel(perf.source, t)}</span>
        {perf.source === 'usage' ? (
          <span>{t('public.models.source.average')}</span>
        ) : null}
        {perf.lowSample ? (
          <span className="text-warning">
            {t('public.models.source.low_sample')}
          </span>
        ) : null}
      </span>
    </div>
  )
}

function CompareCheckbox({
  entry,
  checked,
  disabled,
  onToggleCompare,
}: {
  entry: ModelEntry
  checked: boolean
  disabled: boolean
  onToggleCompare: (modelId: string) => void
}) {
  const { t } = useI18n()
  return (
    <input
      type="checkbox"
      className="pick size-3.5 shrink-0"
      checked={checked}
      disabled={disabled}
      title={
        disabled
          ? t('public.models.register.compare_full', { max: MAX_COMPARE })
          : undefined
      }
      aria-label={`${t('public.models.table.compare')} ${displayNameOf(entry)}`}
      onChange={() => onToggleCompare(entry.card.model_id)}
      onClick={(event) => event.stopPropagation()}
    />
  )
}

function CallButton({
  entry,
  onCall,
}: {
  entry: ModelEntry
  onCall: (entry: ModelEntry) => void
}) {
  const { t } = useI18n()
  return (
    <button
      type="button"
      className="call-btn inline-flex min-h-7 items-center justify-center border border-border-default px-2 text-badge leading-none text-ink-2 hover:border-brand hover:bg-brand-subtle hover:text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      aria-label={`${t('public.models.table.call')} ${displayNameOf(entry)}`}
      onClick={(event) => {
        event.stopPropagation()
        onCall(entry)
      }}
    >
      {t('public.models.table.call')}
    </button>
  )
}

function ExpandButton({
  entry,
  expanded,
  onToggleExpand,
}: {
  entry: ModelEntry
  expanded: boolean
  onToggleExpand: (modelId: string) => void
}) {
  const { t } = useI18n()
  return (
    <button
      type="button"
      className="row-expand grid size-6 shrink-0 place-items-center border border-transparent text-ink-3 hover:border-border-default hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      aria-expanded={expanded}
      aria-controls={detailIdOf(entry.card.model_id)}
      aria-label={`${
        expanded
          ? t('public.models.table.collapse')
          : t('public.models.table.expand')
      } ${displayNameOf(entry)}`}
      onClick={(event) => {
        event.stopPropagation()
        onToggleExpand(entry.card.model_id)
      }}
    >
      <svg
        width="13"
        height="13"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
      >
        <path d="m9 5 7 7-7 7" />
      </svg>
    </button>
  )
}

function ContextCell({ entry }: { entry: ModelEntry }) {
  const { t, locale } = useI18n()
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
    <>
      <span className="ctx-num font-mono text-meta tabular-nums text-ink-1">
        {context}
      </span>
      <span className="metric-sub block font-mono text-badge tabular-nums text-ink-3">
        {t('public.models.detail.max_output')} {output}
      </span>
    </>
  )
}

function MetricValue({ value }: { value: string }) {
  return (
    <span className="metric font-mono text-meta tabular-nums text-ink-1">
      {value}
    </span>
  )
}

function TableRow({ entry, actions }: { entry: ModelEntry; actions: RowActions }) {
  const id = entry.card.model_id
  const expanded = actions.expandedId === id
  const checked = actions.compareIds.includes(id)
  const full = !checked && actions.compareIds.length >= MAX_COMPARE

  return (
    <Fragment>
      <tr
        className="row h-11 cursor-pointer border-t border-border-hairline hover:bg-surface-hover"
        onClick={() => actions.onToggleExpand(id)}
      >
        <td className="px-2 py-2">
          <CompareCheckbox
            entry={entry}
            checked={checked}
            disabled={full}
            onToggleCompare={actions.onToggleCompare}
          />
        </td>
        <td className="px-2 py-2">
          <ModelIdentity entry={entry} />
        </td>
        <td className="px-2 py-2">
          <CapabilityBarcode entry={entry} slots={actions.slots} />
        </td>
        <td className="px-2 py-2">
          <ContextCell entry={entry} />
        </td>
        <td className="px-2 py-2">
          <LatencyCell perf={entry.perf} />
        </td>
        <td className="px-2 py-2">
          <MetricValue value={formatTPS(entry.perf.tps)} />
        </td>
        <td className="px-2 py-2">
          <MetricValue value={formatPercent(entry.perf.successRate)} />
        </td>
        <td className="px-2 py-2">
          <CallButton entry={entry} onCall={actions.onCall} />
        </td>
        <td className="px-2 py-2">
          <ExpandButton
            entry={entry}
            expanded={expanded}
            onToggleExpand={actions.onToggleExpand}
          />
        </td>
      </tr>
      {expanded ? (
        <tr className="detail-row" id={detailIdOf(id)}>
          <td colSpan={TABLE_COLUMNS} className="p-0">
            <ModelDetail entry={entry} />
          </td>
        </tr>
      ) : null}
    </Fragment>
  )
}

function ModelTable({
  entries,
  actions,
}: {
  entries: readonly ModelEntry[]
  actions: RowActions
}) {
  const { t } = useI18n()
  return (
    <div className="table-wrap w-full overflow-x-auto rounded-xl border border-border-default bg-surface-card">
      <table className="register w-full min-w-[1440px] border-collapse text-meta [table-layout:fixed]">
        <caption className="sr-only">{t('public.models.title')}</caption>
        <colgroup>
          <col className="c-pick w-8" />
          <col className="c-model" />
          <col className="c-cap w-24" />
          <col className="c-ctx w-28" />
          <col className="c-lat w-[206px]" />
          <col className="c-tps w-24" />
          <col className="c-ok w-[108px]" />
          <col className="c-call w-[86px]" />
          <col className="c-exp w-[42px]" />
        </colgroup>
        <thead className="bg-surface-subtle text-left text-badge text-ink-2">
          <tr>
            <th scope="col" className="px-2 py-2">
              <span className="sr-only">{t('public.models.table.compare')}</span>
            </th>
            <th scope="col" className="px-2 py-2">
              {t('public.models.table.model')}
            </th>
            <th scope="col" className="px-2 py-2">
              {t('public.models.table.capabilities')}
            </th>
            <th scope="col" className="px-2 py-2">
              {t('public.models.table.context')}
            </th>
            <th scope="col" className="px-2 py-2">
              {t('public.models.table.latency')}
            </th>
            <th scope="col" className="px-2 py-2">
              {t('public.models.table.speed')}
            </th>
            <th scope="col" className="px-2 py-2">
              {t('public.models.table.success')}
            </th>
            <th scope="col" className="px-2 py-2">
              {t('public.models.table.call')}
            </th>
            <th scope="col" className="px-2 py-2">
              <span className="sr-only">{t('public.models.table.expand')}</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {entries.map((entry) => (
            <TableRow key={entry.card.model_id} entry={entry} actions={actions} />
          ))}
        </tbody>
      </table>
    </div>
  )
}

function ModelCard({ entry, actions }: { entry: ModelEntry; actions: RowActions }) {
  const { t, locale } = useI18n()
  const id = entry.card.model_id
  const expanded = actions.expandedId === id
  const checked = actions.compareIds.includes(id)
  const full = !checked && actions.compareIds.length >= MAX_COMPARE

  return (
    <li className="card min-w-0 bg-surface-card p-3.5">
      <div className="card-top flex items-start gap-2.5">
        <Monogram entry={entry} />
        <div className="card-id min-w-0 flex-1">
          <div className="model-name text-meta font-semibold leading-5 text-ink-1">
            {displayNameOf(entry)}
          </div>
          <div className="model-id break-all font-mono text-badge text-ink-3">
            {entry.card.model_id}
          </div>
          {entry.card.vendor !== '' ? (
            <div className="text-badge text-ink-3">{entry.card.vendor}</div>
          ) : null}
        </div>
        <CompareCheckbox
          entry={entry}
          checked={checked}
          disabled={full}
          onToggleCompare={actions.onToggleCompare}
        />
      </div>

      <div className="mt-2.5">
        <CapabilityBarcode entry={entry} slots={actions.slots} />
      </div>

      <div className="card-foot mt-2.5 flex flex-wrap items-center justify-between gap-2 border-t border-border-hairline pt-2.5">
        <span className="text-badge tabular-nums text-ink-3">
          {t('public.models.table.latency')} {formatMs(entry.perf.p50)}
        </span>
        <span className="text-badge tabular-nums text-ink-3">
          {t('public.models.table.speed')} {formatTPS(entry.perf.tps)}
        </span>
        <span className="text-badge tabular-nums text-ink-3">
          {t('public.models.table.success')} {formatPercent(entry.perf.successRate)}
        </span>
        <div className="card-actions flex shrink-0 items-center gap-1.5">
          <CallButton entry={entry} onCall={actions.onCall} />
          <ExpandButton
            entry={entry}
            expanded={expanded}
            onToggleExpand={actions.onToggleExpand}
          />
        </div>
        <span className="flex basis-full flex-wrap items-center gap-x-1 text-badge text-ink-3">
          <span>{sourceLabel(entry.perf.source, t)}</span>
          {entry.perf.source === 'usage' ? (
            <span>{t('public.models.source.average')}</span>
          ) : null}
          {entry.perf.lowSample ? (
            <span className="text-warning">
              {t('public.models.source.low_sample')}
            </span>
          ) : null}
          <span className="tabular-nums">
            {formatSamples(entry.perf.samples, locale)}
          </span>
        </span>
      </div>

      {expanded ? (
        <div
          className="card-detail mt-2.5 border-t border-border-hairline pt-2.5"
          id={detailIdOf(id)}
        >
          <ModelDetail entry={entry} />
        </div>
      ) : null}
    </li>
  )
}

function CompareTray({
  compareIds,
  onOpenCompare,
}: {
  compareIds: readonly string[]
  onOpenCompare: () => void
}) {
  const { t } = useI18n()
  if (compareIds.length === 0) return null
  return (
    <div
      className="compare-tray mt-3 flex items-center justify-between gap-2 border border-border-default bg-surface-subtle px-3 py-2"
      role="region"
      aria-label={t('public.models.compare.title')}
    >
      <span className="text-badge tabular-nums text-ink-2">
        {t('public.models.register.compare_count', {
          count: compareIds.length,
          max: MAX_COMPARE,
        })}
      </span>
      <button
        type="button"
        onClick={onOpenCompare}
        className="inline-flex min-h-7 items-center border border-brand bg-brand-subtle px-2 text-badge text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      >
        {t('public.models.register.open_compare')}
      </button>
    </div>
  )
}

export function Register({
  entries,
  view,
  compareIds,
  slots,
  expandedId,
  onToggleExpand,
  onToggleCompare,
  onOpenCompare,
  onCall,
}: RegisterProps) {
  const wide = useMediaQuery(WIDE_QUERY)
  const effectiveView: ModelsView = wide ? view : 'cards'
  const actions: RowActions = {
    expandedId,
    compareIds,
    onToggleExpand,
    onToggleCompare,
    onCall,
    slots,
  }

  if (entries.length === 0) return null

  return (
    <>
      {effectiveView === 'table' ? (
        <ModelTable entries={entries} actions={actions} />
      ) : (
        <ul className="cards grid gap-px">
          {entries.map((entry) => (
            <ModelCard key={entry.card.model_id} entry={entry} actions={actions} />
          ))}
        </ul>
      )}
      <CompareTray compareIds={compareIds} onOpenCompare={onOpenCompare} />
    </>
  )
}
