/**
 * 模型库筛选面板：四组可多选的 facet、仅显示有测量的开关与重置。
 *
 * 与定价页共享同一套 URL 口径：能力是「满足全部」（AND），供应商/上下文/
 * 端点组内是「任一满足」（OR）。所有写回都合并进传入的 `search`，面板本身
 * 不持有筛选状态；唯一的本地状态是「展开更多」。
 *
 * 已选值可能不在 facet 里（共享链接被手改、或目录刚失去该枚举）：这些值
 * 仍然渲染成可取消勾选的条目，标签显示 URL 原值。否则用户会被自己无法
 * 移除的筛选锁死。
 */

import { useState, type ReactNode } from 'react'

import { SwitchControl } from '@/features/public/pricing/controls'
import {
  capabilityLabel,
  contextBucketLabel,
  endpointLabel,
} from '@/features/public/pricing/metadata'
import {
  normalizeCapabilities,
  normalizeContexts,
  normalizeEndpointTypes,
  normalizeVendors,
  withCapabilities,
  withContexts,
  withEndpointTypes,
  withVendors,
} from '@/features/public/pricing/params'
import { useI18n } from '@/shared/lib/i18n-context'

import { activeFilterCount } from './filters'
import { validateModelsSearch, type ModelsSearch } from './params'
import type { ModelFacets } from './taxonomy'

export interface ModelFiltersProps {
  facets: ModelFacets
  search: ModelsSearch
  onChange: (next: ModelsSearch, replace?: boolean) => void
  onClose?: () => void
}

/** 能力与供应商超过该数量时先折叠，展开由用户决定。 */
const VISIBLE_LIMIT = 8

interface FacetOption {
  value: string
  count?: number
}

interface VisibleOptions {
  visible: FacetOption[]
  hidden: number
}

/**
 * 折叠时保留前 8 项；已选但排在 8 项之后的值仍要显示，否则用户无法在不
 * 展开的情况下取消它。facet 中不存在的已选值追加在末尾（count 缺省）。
 */
function extraOptions(
  options: readonly FacetOption[],
  selected: readonly string[],
): FacetOption[] {
  return selected
    .filter((value) => !options.some((option) => option.value === value))
    .map((value) => ({ value }))
}

/**
 * facet 顺序不变，已选但不在 facet 里的值追加在末尾（无 count）。上下文与
 * 端点组不折叠，用这个直接拼出完整选项，避免已选值无处可取消。
 */
function withSelectedExtras(
  options: readonly FacetOption[],
  selected: readonly string[],
): FacetOption[] {
  return [...options, ...extraOptions(options, selected)]
}

function visibleOptions(
  options: readonly FacetOption[],
  selected: readonly string[],
  expanded: boolean,
): VisibleOptions {
  const extras = extraOptions(options, selected)

  if (expanded || options.length <= VISIBLE_LIMIT) {
    return { visible: [...options, ...extras], hidden: 0 }
  }

  const head = options.slice(0, VISIBLE_LIMIT)
  const headValues = new Set(head.map((option) => option.value))
  const pinned = options.filter(
    (option) =>
      !headValues.has(option.value) && selected.includes(option.value),
  )
  return {
    visible: [...head, ...pinned, ...extras],
    hidden: options.length - head.length - pinned.length,
  }
}

function FacetFieldset({
  title,
  children,
}: {
  title: string
  children: ReactNode
}) {
  return (
    <fieldset className="border-t border-border-hairline py-3 first:border-t-0 first:pt-0">
      <legend className="mb-1 text-public-label text-ink-1">
        {title}
      </legend>
      {children}
    </fieldset>
  )
}

function CheckRow({
  checked,
  count,
  onToggle,
  children,
}: {
  checked: boolean
  count?: number
  onToggle: () => void
  children: ReactNode
}) {
  const { locale } = useI18n()
  return (
    <label
      className={[
        'flex h-8 cursor-pointer items-center gap-2 rounded-md px-1 text-public-caption',
        checked ? 'bg-brand-subtle text-ink-1' : 'text-ink-1 hover:bg-surface-hover',
      ].join(' ')}
    >
      <input
        type="checkbox"
        checked={checked}
        onChange={onToggle}
        className="size-4 shrink-0 accent-[var(--brand)] focus-visible:shadow-[var(--focus-ring)]"
      />
      <span className="min-w-0 flex-1 truncate">{children}</span>
      {count === undefined ? null : (
        <span
          aria-hidden="true"
          className="shrink-0 tabular-nums text-ink-3"
        >
          {new Intl.NumberFormat(locale).format(count)}
        </span>
      )}
    </label>
  )
}

type ExpandableGroup = 'capabilities' | 'vendors'

export function ModelFilters({
  facets,
  search,
  onChange,
  onClose,
}: ModelFiltersProps) {
  const { t, locale } = useI18n()
  const [expanded, setExpanded] = useState<Record<ExpandableGroup, boolean>>({
    capabilities: false,
    vendors: false,
  })

  const selectedCapabilities = normalizeCapabilities(search.cap)
  const selectedVendors = normalizeVendors(search.vendor)
  const selectedContexts = normalizeContexts(search.context)
  const selectedEndpointTypes = normalizeEndpointTypes(search.endpoint)

  const capabilityOptions = visibleOptions(
    facets.capabilities,
    selectedCapabilities,
    expanded.capabilities,
  )
  const vendorOptions = visibleOptions(
    facets.vendors,
    selectedVendors,
    expanded.vendors,
  )
  const contextOptions = withSelectedExtras(facets.contexts, selectedContexts)
  const endpointOptions = withSelectedExtras(
    facets.endpoints,
    selectedEndpointTypes,
  )

  // 没有选项的组整组不渲染；已选但 facet 里不存在的值由 withSelectedExtras /
  // visibleOptions 追加成条目，因此「只有额外选中项」的组仍有内容。
  const hasCapabilityOptions = capabilityOptions.visible.length > 0
  const hasVendorOptions = vendorOptions.visible.length > 0
  const hasContextOptions = contextOptions.length > 0
  const hasEndpointOptions = endpointOptions.length > 0
  // 能力 / 厂商 / 上下文三组全空 = 运营方还没填 model_meta，用一条提示代替
  // 三组空标题（协议端点组仍按自己的选项单独判断）。
  const hasProfileFacets =
    hasCapabilityOptions || hasVendorOptions || hasContextOptions
  const hasFilters = activeFilterCount(search) > 0

  const toggle = (
    selected: readonly string[],
    value: string,
    write: (next: readonly string[]) => string,
    key: 'cap' | 'vendor' | 'context' | 'endpoint',
  ) => {
    const next = selected.includes(value)
      ? selected.filter((item) => item !== value)
      : [...selected, value]
    onChange({ ...search, [key]: write(next) })
  }

  const expand = (group: ExpandableGroup) =>
    setExpanded((current) => ({ ...current, [group]: true }))

  const showMore = (group: ExpandableGroup, hidden: number) =>
    hidden > 0 ? (
      <button
        type="button"
        onClick={() => expand(group)}
        className="px-1 text-public-caption text-brand hover:underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
      >
        {t('public.models.filters.show_more', {
          count: new Intl.NumberFormat(locale).format(hidden),
        })}
      </button>
    ) : null

  return (
    <div className="text-public-caption">
      <div className="mb-2 flex items-center justify-between gap-2">
        <h2 className="text-public-body-strong text-ink-1">
          {t('public.models.filters.title')}
        </h2>
        <div className="flex items-center gap-2">
          {/* 恢复 Task 4 的默认值（sort/range/compare 与各筛选组），只保留
              用户当前选择的视图。 */}
          <button
            type="button"
            onClick={() => onChange(validateModelsSearch({ view: search.view }))}
            disabled={!hasFilters}
            className="rounded px-1 text-public-caption text-brand hover:underline disabled:cursor-not-allowed disabled:text-ink-3 disabled:no-underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            {t('public.models.filters.reset')}
          </button>
          {onClose === undefined ? null : (
            <button
              type="button"
              onClick={onClose}
              className="rounded px-1 text-public-caption text-ink-2 hover:text-ink-1 hover:underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            >
              {t('public.models.filters.close')}
            </button>
          )}
        </div>
      </div>

      <div>
        {hasProfileFacets ? (
          <>
            {hasCapabilityOptions ? (
              <FacetFieldset title={t('public.models.filters.capabilities')}>
                {capabilityOptions.visible.map((option) => (
                  <CheckRow
                    key={option.value}
                    checked={selectedCapabilities.includes(option.value)}
                    count={option.count}
                    onToggle={() =>
                      toggle(
                        selectedCapabilities,
                        option.value,
                        withCapabilities,
                        'cap',
                      )
                    }
                  >
                    {capabilityLabel(option.value, t)}
                  </CheckRow>
                ))}
                {showMore('capabilities', capabilityOptions.hidden)}
              </FacetFieldset>
            ) : null}

            {hasVendorOptions ? (
              <FacetFieldset title={t('public.models.filters.vendors')}>
                {vendorOptions.visible.map((option) => (
                  <CheckRow
                    key={option.value}
                    checked={selectedVendors.includes(option.value)}
                    count={option.count}
                    onToggle={() =>
                      toggle(
                        selectedVendors,
                        option.value,
                        withVendors,
                        'vendor',
                      )
                    }
                  >
                    {/* Facet 只有小写 value，没有展示名；按原值渲染。 */}
                    {option.value}
                  </CheckRow>
                ))}
                {showMore('vendors', vendorOptions.hidden)}
              </FacetFieldset>
            ) : null}

            {hasContextOptions ? (
              <FacetFieldset title={t('public.models.filters.context')}>
                {contextOptions.map((option) => (
                  <CheckRow
                    key={option.value}
                    checked={selectedContexts.includes(option.value)}
                    count={option.count}
                    onToggle={() =>
                      toggle(
                        selectedContexts,
                        option.value,
                        withContexts,
                        'context',
                      )
                    }
                  >
                    {contextBucketLabel(option.value, t)}
                  </CheckRow>
                ))}
              </FacetFieldset>
            ) : null}
          </>
        ) : (
          <p className="rounded-lg border border-border-hairline bg-surface-subtle px-3 py-2.5 text-public-caption text-ink-3">
            <strong className="font-medium text-ink-2">
              {t('public.models.filters.profile_missing')}
            </strong>{' '}
            {t('public.models.filters.profile_missing_hint')}
          </p>
        )}

        {hasEndpointOptions ? (
          <FacetFieldset title={t('public.models.filters.endpoint')}>
            {endpointOptions.map((option) => (
              <CheckRow
                key={option.value}
                checked={selectedEndpointTypes.includes(option.value)}
                count={option.count}
                onToggle={() =>
                  toggle(
                    selectedEndpointTypes,
                    option.value,
                    withEndpointTypes,
                    'endpoint',
                  )
                }
              >
                {endpointLabel(option.value, t)}
              </CheckRow>
            ))}
          </FacetFieldset>
        ) : null}
      </div>

      <div className="border-t border-border-hairline py-3">
        <h3 className="mb-1 text-public-label text-ink-1">
          {t('public.models.filters.data')}
        </h3>
        <SwitchControl
          label={t('public.models.filters.measured')}
          checked={search.measured}
          onChange={(measured) => onChange({ ...search, measured })}
        >
          {t('public.models.filters.measured')}
        </SwitchControl>
      </div>
    </div>
  )
}
