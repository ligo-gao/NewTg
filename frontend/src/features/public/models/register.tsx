/**
 * 模型登记册（规格表 + 卡片）。
 *
 * 两种视图消费同一份 `ModelEntry`，只负责渲染：不发请求、不读 URL，展开
 * 状态与对比集合都由页面传入。读数来源必须诚实：`p50` 缺失时数字是 `—`
 * 而不是 `0`，没有 P95 就不写 P95 副行，均值来源不能借测速的措辞——均值
 * 不是分位数，绝不能冒充 P95。
 */

import { Fragment } from 'react'

import type { PublicModelCard } from '@/features/public/pricing/api'
import { copyText } from '@/features/public/pricing/copy'
import { formatContextTokens } from '@/features/public/pricing/format'
import { useI18n } from '@/shared/lib/i18n-context'
import type { TranslateFn } from '@/shared/lib/i18n-context'
import { useToastStore } from '@/shared/ui/toast'

import { formatMs } from './format'
import { ModelDetail } from './model-detail'
import { MAX_COMPARE, type ModelsRange, type ModelsView } from './params'
import type { ModelEntry, ModelPerformance } from './performance'
import { formatRmb } from './price'

export interface RegisterProps {
  entries: readonly ModelEntry[]
  view: ModelsView
  /**
   * 表格在当前视口不可渲染（<1440px，或 ≤760px 的窄屏）：视图无条件落到
   * 卡片，地址栏里的 `view` 仍保持用户原来的选择。判定由页面算一次后传入，
   * 工具栏与列表共用同一个值。
   */
  compact: boolean
  /** 指标窗口；展开行把它带进读数块标题。 */
  range: ModelsRange
  /**
   * 站点对外地址，由页面读一次后下传；展开行不再各自请求 `/api/status`。
   */
  serverAddress?: string
  compareIds: readonly string[]
  expandedId: string | null
  onToggleExpand: (modelId: string) => void
  onToggleCompare: (modelId: string) => void
  onOpenCompare: () => void
  onCall: (entry: ModelEntry) => void
}

interface RowActions {
  expandedId: string | null
  range: ModelsRange
  serverAddress?: string
  compareIds: readonly string[]
  onToggleExpand: (modelId: string) => void
  onToggleCompare: (modelId: string) => void
  onCall: (entry: ModelEntry) => void
}

/**
 * 「档案在场」的唯一判定：能力、厂商、上下文或最大输出任一存在即算在场，
 * 与详情「说明」块的拼装口径一致。表格的占位提示与卡片的副行共用这一条，
 * 任一视图都不会对同一条目给出相反判词，也别在两处各写一遍德摩根式。
 */
function profilePresent(card: PublicModelCard): boolean {
  return (
    card.capabilities.length > 0 ||
    card.vendor !== '' ||
    card.context_length !== null ||
    card.max_output !== null
  )
}

function detailIdOf(modelId: string): string {
  return `model-detail-${modelId}`
}

function displayNameOf(entry: ModelEntry): string {
  return entry.card.display_name === '' ? entry.card.model_id : entry.card.display_name
}

/**
 * 成功率的唯一措辞来源：键本身带 `{rate}` 占位符，必须传值；没有读数时
 * 用专门的 `success_none`，不能把字面 `{rate}` 或 `0%` 渲染出去。
 */
export function successLabel(perf: ModelPerformance, t: TranslateFn): string {
  if (perf.successRate === null) return t('public.models.table.success_none')
  return t('public.models.table.success', {
    rate: `${(perf.successRate * 100).toFixed(2)}%`,
  })
}

/**
 * 表格的模型格：名称、可复制的 model_id（与名称相同时只出现一次）、
 * 能力与厂商 chip。复制恒为 `model_id`——展示名不是可调用的标识。
 */
function ModelIdentity({ entry }: { entry: ModelEntry }) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)
  const name = displayNameOf(entry)
  // Compare the resolved name: an empty display_name already falls back to the
  // model_id, and printing the id twice is exactly what v3 removes.
  const showId = name !== entry.card.model_id
  const profileMissing = !profilePresent(entry.card)
  const copyLabel = `${t('public.models.table.copy_id')} ${name}`

  const copy = async () => {
    const ok = await copyText(entry.card.model_id)
    push({
      tone: ok ? 'success' : 'error',
      message: ok
        ? t('public.models.call.copied')
        : t('public.pricing.copy_failed'),
    })
  }

  return (
    <div className="model-cell min-w-0">
      <div className="model-name-row group flex min-w-0 items-center gap-1.5">
        <span
          className="model-name truncate text-public-body-strong text-ink-1"
          title={name}
        >
          {name}
        </span>
        <button
          type="button"
          className="copy-id inline-flex size-5 shrink-0 items-center justify-center text-ink-3 opacity-0 transition-opacity hover:text-ink-1 focus-visible:opacity-100 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none group-focus-within:opacity-100 group-hover:opacity-100"
          aria-label={copyLabel}
          title={copyLabel}
          onClick={(event) => {
            event.stopPropagation()
            void copy()
          }}
        >
          <svg
            aria-hidden="true"
            viewBox="0 0 16 16"
            className="size-3.5"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.5"
          >
            <rect x="5" y="5" width="8" height="8" rx="1.5" />
            <path d="M11 5V4a1 1 0 0 0-1-1H4a1 1 0 0 0-1 1v6a1 1 0 0 0 1 1h1" />
          </svg>
        </button>
      </div>
      {showId ? (
        <div className="model-id break-all font-mono text-public-caption text-ink-3">
          {entry.card.model_id}
        </div>
      ) : null}
      {profileMissing ? (
        <div className="model-chips mt-1 flex flex-wrap gap-1">
          <span className="chip plain border border-border-default px-1.5 text-public-label text-ink-3">
            {t('public.models.table.profile_missing')}
          </span>
        </div>
      ) : entry.card.capabilities.length > 0 || entry.card.vendor !== '' ? (
        <div className="model-chips mt-1 flex flex-wrap gap-1">
          {entry.card.capabilities.map((capability) => (
            <span
              className="chip border border-border-default px-1.5 text-public-label text-ink-2"
              key={capability}
            >
              {capability}
            </span>
          ))}
          {entry.card.vendor !== '' ? (
            <span className="chip plain border border-border-default px-1.5 text-public-label text-ink-3">
              {entry.card.vendor}
            </span>
          ) : null}
        </div>
      ) : null}
    </div>
  )
}

/**
 * 首字延迟格：只有真实存在的 P95 才写副行；读数缺失时整格是 `—`。
 * 非 chat 模型只有端到端延迟，必须带上口径标签，免得被读成首字。
 */
function LatencyCell({ perf }: { perf: ModelPerformance }) {
  const { t, locale } = useI18n()
  return (
    <div className="lat-cell">
      <span className="metric block text-public-caption tabular-nums text-ink-1">
        {formatMs(perf.p50, locale)}
      </span>
      {perf.p95 !== null ? (
        <span className="metric-sub block text-public-caption tabular-nums text-ink-3">
          P95 {formatMs(perf.p95, locale)}
        </span>
      ) : null}
      {perf.measure === 'latency' ? (
        <span className="metric-sub block text-public-caption text-ink-3">
          {t('public.models.source.measure.latency')}
        </span>
      ) : null}
    </div>
  )
}

/**
 * 价格格：按 token 是「输入价 · 输出价（每百万 token）」；按次只有一次
 * 调用价，单位由「每次」这一行说明——表头是整列默认口径，单行必须自己
 * 声明单位，不能把按次价读成每百万 token。任何缺失位置仍单独渲染 `—`。
 */
function PriceCell({ entry }: { entry: ModelEntry }) {
  const { t, locale } = useI18n()
  const { price } = entry
  if (price.perCall) {
    return (
      <div className="price-cell flex flex-col justify-center text-public-caption tabular-nums text-ink-1">
        <span>{formatRmb(price.input, locale) ?? '—'}</span>
        <span className="metric-sub block text-public-caption text-ink-3">
          {t('public.pricing.price_per_call')}
        </span>
      </div>
    )
  }
  return (
    <div className="price-cell flex items-center gap-1 text-public-caption tabular-nums text-ink-1">
      <span>{formatRmb(price.input, locale) ?? '—'}</span>
      <span aria-hidden="true" className="text-ink-3">
        {' · '}
      </span>
      <span>{formatRmb(price.output, locale) ?? '—'}</span>
    </div>
  )
}

/** 吞吐的唯一口径：固定一位小数；缺失或非法值 `—`（表格与卡片共用）。 */
export function tpsText(tps: number | null): string {
  return tps === null || !Number.isFinite(tps) || tps < 0
    ? '—'
    : `${tps.toFixed(1)} t/s`
}

function TpsCell({ perf }: { perf: ModelPerformance }) {
  return (
    <span className="tps-cell metric text-public-caption tabular-nums text-ink-1">
      {tpsText(perf.tps)}
    </span>
  )
}

/**
 * 实测格：来源决定计数口径（测速次数 / 真实调用次数），副行只放成功率。
 * 低样本是警示而不是读数，只有存在读数时才会由 `perf.lowSample` 标出。
 */
function EvidenceCell({ entry }: { entry: ModelEntry }) {
  const { t, locale } = useI18n()
  const { perf } = entry
  const count = new Intl.NumberFormat(locale).format(perf.samples)
  const source =
    perf.source === 'test'
      ? t('public.models.table.probe_runs', { count })
      : perf.source === 'usage'
        ? t('public.models.table.calls', { count })
        : t('public.models.table.evidence_none')
  const success = successLabel(entry.perf, t)

  return (
    <div className="evidence-cell">
      <span className="block text-public-caption text-ink-1">{source}</span>
      <span className="metric-sub block text-public-caption tabular-nums text-ink-3">
        {success}
      </span>
      {perf.lowSample ? (
        <span className="metric-sub block text-public-caption text-warning">
          {t('public.models.source.low_sample')}
        </span>
      ) : null}
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
      <span className="ctx-num text-public-caption tabular-nums text-ink-1">
        {context}
      </span>
      <span className="metric-sub block text-public-caption tabular-nums text-ink-3">
        {t('public.models.detail.max_output')} {output}
      </span>
    </>
  )
}

function TableRow({
  entry,
  actions,
  showContext,
  columnCount,
}: {
  entry: ModelEntry
  actions: RowActions
  showContext: boolean
  columnCount: number
}) {
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
          <PriceCell entry={entry} />
        </td>
        {showContext ? (
          <td className="px-2 py-2">
            <ContextCell entry={entry} />
          </td>
        ) : null}
        <td className="px-2 py-2">
          <LatencyCell perf={entry.perf} />
        </td>
        <td className="px-2 py-2">
          <TpsCell perf={entry.perf} />
        </td>
        <td className="px-2 py-2">
          <EvidenceCell entry={entry} />
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
          <td colSpan={columnCount} className="p-0">
            <ModelDetail
              entry={entry}
              range={actions.range}
              serverAddress={actions.serverAddress}
              onCall={actions.onCall}
              onToggleCompare={actions.onToggleCompare}
              compared={checked}
              compareFull={full}
            />
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
  // 列只在有真值时出现：档案全空时整列隐藏，而不是留一列破折号。
  const showContext = entries.some(
    (entry) =>
      entry.card.context_length !== null || entry.card.max_output !== null,
  )
  const columnCount = showContext ? 8 : 7
  // 整列都是按次计价时，表头也不能再声称「¥/百万 token」；混合目录两种单位
  // 并存，任何带单位的表头都会把其中一半的行说错，所以退到中性的「价格」。
  const allPerCall =
    entries.length > 0 && entries.every((entry) => entry.price.perCall === true)
  const anyPerCall = entries.some((entry) => entry.price.perCall === true)
  const mixed = anyPerCall && !allPerCall

  return (
    <div className="table-wrap relative w-full overflow-x-auto rounded-xl border border-border-default bg-surface-card">
      <table className="register w-full min-w-[1040px] border-collapse text-public-caption [table-layout:fixed]">
        <caption className="sr-only">{t('public.models.title')}</caption>
        {/*
          每一列都写了宽度：`table-layout: fixed` 只把余量按列宽比例摊给各列，
          有一列留空（原来给 `c-model`）时它会把整份余量吃下去，模型名和价格
          之间就多出几百像素的空白。列宽之和按表格可用宽度（宽屏主栏约
          1100–1180px）定，余量再按比例摊开。模型名已 `truncate`，窄列不会撑破。
        */}
        <colgroup>
          <col className="c-pick w-8" />
          <col className="c-model w-[300px]" />
          <col className="c-price w-[168px]" />
          {showContext ? <col className="c-ctx w-[104px]" /> : null}
          <col className="c-lat w-[190px]" />
          <col className="c-tps w-[90px]" />
          <col className="c-ok w-[118px]" />
          <col className="c-exp w-[40px]" />
        </colgroup>
        <thead className="bg-surface-subtle text-left text-public-label text-ink-2">
          <tr>
            <th scope="col" className="px-2 py-2">
              <span className="sr-only">{t('public.models.table.compare')}</span>
            </th>
            <th scope="col" className="px-2 py-2">
              {t('public.models.table.model')}
            </th>
            <th scope="col" className="px-2 py-2">
              <span className="block">
                {t(
                  allPerCall
                    ? 'public.pricing.price_per_call'
                    : mixed
                      ? 'public.models.table.price_mixed'
                      : 'public.models.table.price',
                )}
              </span>
              {allPerCall || mixed ? null : (
                <span className="block font-normal text-ink-3">
                  {t('public.models.table.price_sub')}
                </span>
              )}
            </th>
            {showContext ? (
              <th scope="col" className="px-2 py-2">
                {t('public.models.table.context')}
              </th>
            ) : null}
            <th scope="col" className="px-2 py-2">
              {t('public.models.table.latency')}
            </th>
            <th scope="col" className="px-2 py-2">
              {t('public.models.table.speed')}
            </th>
            <th scope="col" className="px-2 py-2">
              {t('public.models.table.evidence')}
            </th>
            <th scope="col" className="px-2 py-2">
              <span className="sr-only">{t('public.models.table.expand')}</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {entries.map((entry) => (
            <TableRow
              key={entry.card.model_id}
              entry={entry}
              actions={actions}
              showContext={showContext}
              columnCount={columnCount}
            />
          ))}
        </tbody>
      </table>
    </div>
  )
}

/**
 * 卡片：演示稿 cardView 的结构——对比勾选与名称在最上，档案副行、能力 chip、
 * 三格读数（首字 P50 / 输出吞吐 / 输入价），底行是最大输出与「详情」。
 * 读数与表格共用同一套格式函数，两种视图永远不会对同一个数字给出不同写法。
 */
function ModelCard({ entry, actions }: { entry: ModelEntry; actions: RowActions }) {
  const { t, locale } = useI18n()
  const { card, perf } = entry
  const id = card.model_id
  const name = displayNameOf(entry)
  const expanded = actions.expandedId === id
  const checked = actions.compareIds.includes(id)
  const full = !checked && actions.compareIds.length >= MAX_COMPARE

  const context =
    card.context_length === null
      ? null
      : formatContextTokens(card.context_length, locale)
  /**
   * 与表格 `ModelIdentity` 共用 `profilePresent`：能力、厂商、上下文或最大
   * 输出任一存在即算有档案。副行按「有哪段写哪段」拼装；只有能力或只有
   * 上下文/最大输出的「部分档案」不再打印档案缺失提示（副行留空，chips
   * 自己说明），避免和表格视图对同一条目给出互相矛盾的说法。
   */
  const hasProfile = profilePresent(card)
  const profileParts: string[] = []
  if (card.vendor !== '') profileParts.push(card.vendor)
  if (context !== null) {
    profileParts.push(t('public.pricing.context', { value: context }))
  }
  const output =
    card.max_output === null
      ? null
      : formatContextTokens(card.max_output, locale)
  const latencyLabel = t(
    perf.measure === 'ttft'
      ? 'public.models.detail.ttft_p50'
      : 'public.models.detail.latency_p50',
  )

  return (
    <li className="card flex min-w-0 flex-col gap-3 rounded-lg border border-border-default bg-surface-card p-4">
      <div className="card-top flex items-start gap-2.5">
        <CompareCheckbox
          entry={entry}
          checked={checked}
          disabled={full}
          onToggleCompare={actions.onToggleCompare}
        />
        <div className="card-id min-w-0 flex-1">
          <div className="model-name truncate text-public-body-strong text-ink-1">
            {name}
          </div>
          {name === id ? null : (
            <div className="model-id break-all font-mono text-public-caption text-ink-3">
              {id}
            </div>
          )}
          {profileParts.length > 0 ? (
            <div className="card-subline text-public-caption text-ink-3">
              {profileParts.join(' · ')}
            </div>
          ) : hasProfile ? null : (
            <div className="card-subline text-public-caption text-ink-3">
              {t('public.models.table.profile_missing')}
            </div>
          )}
        </div>
      </div>

      {card.capabilities.length === 0 ? null : (
        <div className="card-chips flex flex-wrap gap-1">
          {card.capabilities.map((capability) => (
            <span
              className="chip border border-border-default px-1.5 text-public-label text-ink-2"
              key={capability}
            >
              {capability}
            </span>
          ))}
        </div>
      )}

      <div className="card-metrics grid grid-cols-3 gap-2.5 border-t border-border-hairline pt-3">
        <div className="card-metric">
          <span className="card-metric-value block text-public-caption tabular-nums text-ink-1">
            {formatMs(perf.p50, locale)}
          </span>
          <span className="card-metric-label block text-public-caption text-ink-3">
            {latencyLabel}
          </span>
        </div>
        <div className="card-metric">
          <span className="card-metric-value block text-public-caption tabular-nums text-ink-1">
            {tpsText(perf.tps)}
          </span>
          <span className="card-metric-label block text-public-caption text-ink-3">
            {t('public.models.detail.tps')}
          </span>
        </div>
        <div className="card-metric card-price">
          <span className="card-metric-value block text-public-caption tabular-nums text-ink-1">
            {formatRmb(entry.price.input, locale) ?? '—'}
          </span>
          <span className="card-metric-label block text-public-caption text-ink-3">
            {t(
              entry.price.perCall
                ? 'public.pricing.price_per_call'
                : 'public.models.compare.price_input',
            )}
          </span>
        </div>
      </div>

      <div className="card-foot flex items-center justify-between gap-2.5 border-t border-border-hairline pt-3">
        <span className="card-output text-public-caption tabular-nums text-ink-3">
          {output === null
            ? '—'
            : `${output} ${t('public.models.detail.max_output')}`}
        </span>
        <button
          type="button"
          className="card-detail-btn inline-flex min-h-7 items-center justify-center border border-border-default px-2 text-public-caption leading-none text-ink-2 hover:border-brand hover:bg-brand-subtle hover:text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          aria-expanded={expanded}
          aria-controls={detailIdOf(id)}
          aria-label={`${
            expanded
              ? t('public.models.table.collapse')
              : t('public.models.table.expand')
          } ${name}`}
          onClick={(event) => {
            event.stopPropagation()
            actions.onToggleExpand(id)
          }}
        >
          {expanded
            ? t('public.models.table.collapse')
            : t('public.models.table.expand')}
        </button>
      </div>

      {expanded ? (
        <div
          className="card-detail mt-2.5 border-t border-border-hairline pt-2.5"
          id={detailIdOf(id)}
        >
          <ModelDetail
            entry={entry}
            range={actions.range}
            serverAddress={actions.serverAddress}
            onCall={actions.onCall}
            onToggleCompare={actions.onToggleCompare}
            compared={checked}
            compareFull={full}
          />
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
      <span className="text-public-caption tabular-nums text-ink-2">
        {t('public.models.register.compare_count', {
          count: compareIds.length,
          max: MAX_COMPARE,
        })}
      </span>
      <button
        type="button"
        onClick={onOpenCompare}
        className="inline-flex min-h-7 items-center border border-brand bg-brand-subtle px-2 text-public-caption text-brand focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      >
        {t('public.models.register.open_compare')}
      </button>
    </div>
  )
}

export function Register({
  entries,
  view,
  compact,
  range,
  serverAddress,
  compareIds,
  expandedId,
  onToggleExpand,
  onToggleCompare,
  onOpenCompare,
  onCall,
}: RegisterProps) {
  // 表格不可用时（窄屏或 <1440px）卡片是布局强制，优先于地址栏里的 view。
  const effectiveView: ModelsView = compact ? 'cards' : view
  const actions: RowActions = {
    expandedId,
    range,
    serverAddress,
    compareIds,
    onToggleExpand,
    onToggleCompare,
    onCall,
  }

  if (entries.length === 0) return null

  return (
    <>
      {effectiveView === 'table' ? (
        <ModelTable entries={entries} actions={actions} />
      ) : (
        <ul className="cards grid gap-3.5 sm:grid-cols-2 xl:grid-cols-3">
          {entries.map((entry) => (
            <ModelCard key={entry.card.model_id} entry={entry} actions={actions} />
          ))}
        </ul>
      )}
      <CompareTray compareIds={compareIds} onOpenCompare={onOpenCompare} />
    </>
  )
}
