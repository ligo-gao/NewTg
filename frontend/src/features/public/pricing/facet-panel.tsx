/**
 * facet 侧栏（F-09-15）与移动端筛选 Sheet（F-09-23）。
 *
 * 渲染后端**真正接受**的六组参数：vendor / capability / context / group /
 * quotaType / endpointType。D1c 起 `/api/pricing` 返回元数据 facets，
 * 此前恒为空数组的三组（capability/context/endpointType）随之解禁；
 * 空组仍显示「无」而不是整组隐藏，保持六组的稳定布局（F-09-15 六态）。
 *
 * 计数为 0 的候选置灰且不可聚焦；已选中的 0 计数项仍可取消，否则用户
 * 会被自己的筛选锁死。
 */

import { useEffect, useId, useState, type ReactNode } from 'react'

import type { PublicFacetOption, PublicFacets } from './api'
import { capabilityLabel, contextBucketLabel, endpointLabel } from './metadata'
import type { PricingQuotaType, PricingSearch } from './params'
import {
  activeFilterCount,
  capabilityList,
  clearedFilters,
  contextList,
  endpointTypeList,
  vendorList,
  withCapabilities,
  withContexts,
  withEndpointTypes,
  withVendors,
} from './params'
import { MobileSheet } from '@/features/public/mobile-sheet'
import { useI18n } from '@/shared/lib/i18n-context'
import { Tag } from '@/shared/ui/tag'

const VENDOR_VISIBLE_LIMIT = 8

function CountTag({ count }: { count: number }) {
  const { t, locale } = useI18n()
  const formatted = new Intl.NumberFormat(locale).format(count)
  return (
    <Tag variant="neutral" ariaLabel={t('public.pricing.models_count', { count: formatted })}>
      <span className="tabular-nums">{formatted}</span>
    </Tag>
  )
}

function FacetGroup({
  title,
  children,
}: {
  title: string
  children: ReactNode
}) {
  const id = useId()
  return (
    <div role="group" aria-labelledby={id} className="border-t border-border-hairline py-3 first:border-t-0 first:pt-0">
      <h3 id={id} className="mb-1 text-public-label text-ink-1">
        {title}
      </h3>
      {children}
    </div>
  )
}

interface OptionRowProps {
  checked: boolean
  disabled: boolean
  onSelect: () => void
  children: ReactNode
  count?: number
  type?: 'checkbox' | 'radio'
  name?: string
}

function OptionRow({
  checked,
  disabled,
  onSelect,
  children,
  count,
  type = 'checkbox',
  name,
}: OptionRowProps) {
  return (
    <label
      aria-disabled={disabled ? true : undefined}
      className={[
        'flex h-8 cursor-pointer items-center gap-2 rounded-md px-1 text-public-caption',
        checked ? 'bg-brand-subtle text-ink-1' : 'text-ink-1 hover:bg-surface-hover',
        disabled ? 'cursor-not-allowed text-ink-3 opacity-60' : '',
      ]
        .filter(Boolean)
        .join(' ')}
    >
      <input
        type={type}
        name={name}
        checked={checked}
        disabled={disabled}
        onChange={() => {
          if (!disabled) onSelect()
        }}
        className="size-4 shrink-0 accent-[var(--brand)] focus-visible:shadow-[var(--focus-ring)]"
      />
      <span className="min-w-0 flex-1 truncate">{children}</span>
      {count === undefined ? null : <CountTag count={count} />}
    </label>
  )
}

export interface PricingFacetPanelProps {
  facets: PublicFacets | undefined
  value: PricingSearch
  onChange: (next: PricingSearch) => void
}

/**
 * 多选 facet 组的通用渲染。新三组与 vendor 共用同一套交互与 0 计数禁用
 * 规则；抽出来是为了避免复制三份「选中后写回 URL」的逻辑（写回口径一旦
 * 分叉就会出现漏掉 `p: 1` 的组）。
 */
function MultiFacetGroup({
  title,
  options,
  selected,
  labelOf,
  onToggle,
}: {
  title: string
  options: readonly PublicFacetOption[]
  selected: readonly string[]
  labelOf: (option: PublicFacetOption) => string
  onToggle: (value: string) => void
}) {
  const { t } = useI18n()
  return (
    <FacetGroup title={title}>
      {options.length === 0 ? (
        <p className="px-1 text-ink-3">{t('public.pricing.none')}</p>
      ) : (
        options.map((option) => {
          const checked = selected.includes(option.value)
          return (
            <OptionRow
              key={option.value}
              checked={checked}
              disabled={option.count === 0 && !checked}
              onSelect={() => onToggle(option.value)}
              count={option.count}
            >
              {labelOf(option)}
            </OptionRow>
          )
        })
      )}
    </FacetGroup>
  )
}

export function PricingFacetPanel({ facets, value, onChange }: PricingFacetPanelProps) {
  const { t, locale } = useI18n()
  const [showAllVendors, setShowAllVendors] = useState(false)
  const groupName = useId()
  const quotaName = useId()

  if (facets === undefined) {
    return <p className="text-public-caption text-ink-3">{t('public.pricing.filters_unavailable')}</p>
  }

  const vendors = facets.vendors
  const selectedVendors = vendorList(value)
  const visibleVendors = showAllVendors ? vendors : vendors.slice(0, VENDOR_VISIBLE_LIMIT)
  const hiddenVendorCount = Math.max(0, vendors.length - VENDOR_VISIBLE_LIMIT)

  const toggleVendor = (vendor: string) => {
    const next = selectedVendors.includes(vendor)
      ? selectedVendors.filter((item) => item !== vendor)
      : [...selectedVendors, vendor]
    onChange({ ...value, p: 1, vendor: withVendors(next) })
  }

  function toggleEnum(
    selected: readonly string[],
    entry: string,
    write: (next: string[]) => string,
    key: 'capability' | 'context' | 'endpointType',
  ) {
    const next = selected.includes(entry)
      ? selected.filter((item) => item !== entry)
      : [...selected, entry]
    onChange({ ...value, p: 1, [key]: write(next) })
  }

  const selectedCapabilities = capabilityList(value)
  const selectedContexts = contextList(value)
  const selectedEndpointTypes = endpointTypeList(value)

  const pricingTypes = facets.pricing_types ?? []
  const countOf = (type: 'token' | 'request') =>
    pricingTypes.find((entry) => entry.value === type)?.count ?? 0
  // pricing_types 的计数之和就是当前（忽略 quotaType 自身选择的）总数。
  const totalModels = pricingTypes.reduce((sum, entry) => sum + entry.count, 0)
  const quotaOptions: { value: PricingQuotaType; label: string; count: number; disabled: boolean }[] = [
    {
      value: 'all',
      label: t('public.pricing.all_types'),
      count: totalModels,
      disabled: false,
    },
    {
      value: 'token',
      label: t('public.pricing.type_token'),
      count: countOf('token'),
      disabled: countOf('token') === 0,
    },
    {
      value: 'request',
      label: t('public.pricing.type_request'),
      count: countOf('request'),
      disabled: countOf('request') === 0,
    },
  ]

  const hasFilters = activeFilterCount(value) > 0

  return (
    <div className="text-public-caption">
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-public-body-strong text-ink-1">
          {t('public.pricing.filters')}
        </h2>
        <button
          type="button"
          onClick={() => onChange(clearedFilters(value))}
          disabled={!hasFilters}
          className="rounded px-1 text-public-caption text-brand hover:underline disabled:cursor-not-allowed disabled:text-ink-3 disabled:no-underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {t('public.pricing.reset')}
        </button>
      </div>

      <FacetGroup title={t('public.pricing.vendors')}>
        {vendors.length === 0 ? (
          <p className="px-1 text-ink-3">{t('public.pricing.none')}</p>
        ) : (
          <>
            <button
              type="button"
              onClick={() => onChange({ ...value, p: 1, vendor: '' })}
              disabled={selectedVendors.length === 0}
              className="mb-1 rounded px-1 text-public-caption text-brand hover:underline disabled:cursor-not-allowed disabled:text-ink-3 disabled:no-underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            >
              {t('public.pricing.all_vendors')}
            </button>
            {visibleVendors.map((vendor) => {
              const checked = selectedVendors.includes(vendor.value)
              return (
                <OptionRow
                  key={vendor.value}
                  checked={checked}
                  disabled={vendor.count === 0 && !checked}
                  onSelect={() => toggleVendor(vendor.value)}
                  count={vendor.count}
                >
                  {vendor.label === '' ? vendor.value : vendor.label}
                </OptionRow>
              )
            })}
            {hiddenVendorCount > 0 && !showAllVendors ? (
              <button
                type="button"
                aria-expanded={false}
                onClick={() => setShowAllVendors(true)}
                className="px-1 text-public-caption text-brand hover:underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              >
                {t('public.pricing.show_more', {
                  count: new Intl.NumberFormat(locale).format(hiddenVendorCount),
                })}
              </button>
            ) : null}
          </>
        )}
      </FacetGroup>

      <MultiFacetGroup
        title={t('public.pricing.capabilities')}
        options={facets.capabilities}
        selected={selectedCapabilities}
        labelOf={(option) => capabilityLabel(option.value, t)}
        onToggle={(entry) =>
          toggleEnum(selectedCapabilities, entry, withCapabilities, 'capability')
        }
      />

      <MultiFacetGroup
        title={t('public.pricing.context_length')}
        options={facets.context}
        selected={selectedContexts}
        labelOf={(option) => contextBucketLabel(option.value, t)}
        onToggle={(entry) =>
          toggleEnum(selectedContexts, entry, withContexts, 'context')
        }
      />

      <FacetGroup title={t('public.pricing.groups')}>
        {facets.groups.length === 0 ? (
          <p className="px-1 text-ink-3">{t('public.pricing.none')}</p>
        ) : (
          <>
            <OptionRow
              type="radio"
              name={groupName}
              checked={value.group === ''}
              disabled={false}
              onSelect={() => onChange({ ...value, p: 1, group: '' })}
            >
              {t('public.pricing.all_groups')}
            </OptionRow>
            {facets.groups.map((group) => {
              const checked = value.group === group.value
              const ratio = group.ratio === null ? null : `${(group.ratio / 100).toFixed(2)}x`
              return (
                <OptionRow
                  key={group.value}
                  type="radio"
                  name={groupName}
                  checked={checked}
                  disabled={group.count === 0 && !checked}
                  // 单选组再次点击已选项等于取消（F-09-15 分支流程）。
                  onSelect={() =>
                    onChange({ ...value, p: 1, group: checked ? '' : group.value })
                  }
                  count={group.count}
                >
                  <span className="inline-flex items-center gap-1.5">
                    {group.label === '' ? group.value : group.label}
                    {ratio === null ? null : (
                      <span className="text-public-caption text-ink-2 tabular-nums">{ratio}</span>
                    )}
                  </span>
                </OptionRow>
              )
            })}
          </>
        )}
      </FacetGroup>

      <FacetGroup title={t('public.pricing.pricing_type')}>
        {quotaOptions.map((option) => (
          <OptionRow
            key={option.value}
            type="radio"
            name={quotaName}
            checked={value.quotaType === option.value}
            disabled={option.disabled && value.quotaType !== option.value}
            onSelect={() => onChange({ ...value, p: 1, quotaType: option.value })}
            count={option.count}
          >
            {option.label}
          </OptionRow>
        ))}
      </FacetGroup>

      <MultiFacetGroup
        title={t('public.pricing.endpoint_types')}
        options={facets.endpoints}
        selected={selectedEndpointTypes}
        labelOf={(option) => endpointLabel(option.value, t)}
        onToggle={(entry) =>
          toggleEnum(
            selectedEndpointTypes,
            entry,
            withEndpointTypes,
            'endpointType',
          )
        }
      />
    </div>
  )
}

export interface PricingFilterSheetProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  facets: PublicFacets | undefined
  value: PricingSearch
  onApply: (next: PricingSearch) => void
}

export function PricingFilterSheet({
  open,
  onOpenChange,
  facets,
  value,
  onApply,
}: PricingFilterSheetProps) {
  const { t } = useI18n()
  const [draft, setDraft] = useState(value)

  // Sheet 只有在打开时同步一次当前 URL；这样「取消」不会把草稿写回，
  // 也避免后台数据刷新时把用户正在编辑的选项弹回去。
  useEffect(() => {
    if (open) setDraft(value)
  }, [open, value])

  return (
    <MobileSheet
      open={open}
      onOpenChange={onOpenChange}
      title={t('public.pricing.filters')}
      closeLabel={t('public.pricing.close')}
      footer={
        <button
          type="button"
          onClick={() => {
            onApply(draft)
            onOpenChange(false)
          }}
          className="h-11 w-full rounded-lg bg-brand px-4 text-public-body font-medium text-white hover:bg-brand-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {t('public.pricing.apply_filters')}
        </button>
      }
    >
      <div className="max-h-[65vh] overflow-y-auto py-2">
        <PricingFacetPanel facets={facets} value={draft} onChange={setDraft} />
      </div>
    </MobileSheet>
  )
}
