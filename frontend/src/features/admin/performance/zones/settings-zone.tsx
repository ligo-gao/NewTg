import { useId } from 'react'

import type { SettingsSectionResponse } from '@/features/admin/settings/api'
import { useI18n } from '@/shared/lib/i18n-context'
import type { TranslateFn } from '@/shared/lib/i18n-context'
import { StatusBadge } from '@/shared/ui/status-badge'

import type { PerfConfig } from '../api'
import { formatCount } from '../utils'
import { ZoneRail } from './primitives'
import { ProbeScheduleForm, SCHEDULE_KEYS } from './probe-schedule-form'

/**
 * 「运行参数」区（Task 17 / adjudications §4–§5）。
 *
 * 三块内容：
 *   1. 测速节奏四项——本页唯一可在线改的设置（ProbeScheduleForm）；
 *   2. A 类业务设置组——只读展示当前值与来源，仅给设置页入口，不在此复刻表单；
 *   3. 部署参数（B 类环境变量 / C 类代码内置）——只读表格。
 *
 * 所有值都来自服务端实际生效快照；未知键回退原始键名，绝不硬编码英文标签。
 */

type EnvRow = PerfConfig['env'][number]
type SettingsGroup = PerfConfig['settings_groups'][number]
type SettingItem = SettingsGroup['items'][number]

const GROUP_KEYS: Record<string, string> = {
  channel_health: 'admin.performance.settings.group_health',
  rate_limit: 'admin.performance.settings.group_ratelimit',
  retention: 'admin.performance.settings.group_retention',
}

const UNIT_KEYS: Record<string, string> = {
  minutes: 'admin.performance.settings.unit_minutes',
  seconds: 'admin.performance.settings.unit_seconds',
  days: 'admin.performance.settings.unit_days',
  months: 'admin.performance.settings.unit_months',
  rpm: 'admin.performance.settings.unit_rpm',
}

/** 键名 → `settings.param_*` / `settings.key_*` 的语言键；未命中回退原键名。 */
function translatedOrKey(
  candidate: string,
  fallback: string,
  t: TranslateFn,
): string {
  const text = t(candidate)
  return text === candidate ? fallback : text
}

function paramLabel(key: string, t: TranslateFn): string {
  return translatedOrKey(
    `admin.performance.settings.param_${key.toLowerCase()}`,
    key,
    t,
  )
}

function settingLabel(key: string, t: TranslateFn): string {
  return translatedOrKey(
    `admin.performance.settings.key_${key.replaceAll('.', '_')}`,
    key,
    t,
  )
}

function groupTitle(id: string, t: TranslateFn): string {
  const candidate = GROUP_KEYS[id]
  return candidate === undefined
    ? id
    : translatedOrKey(candidate, id, t)
}

/** `count` 不拼单位（demo 是裸数字）；`onoff` 由徽标表达。 */
function unitSuffix(unit: string, t: TranslateFn): string {
  const candidate = UNIT_KEYS[unit]
  return candidate === undefined ? '' : ` ${t(candidate)}`
}

function settingValueText(item: SettingItem, locale: string): string {
  if (item.value === null || item.value === undefined) return '—'
  if (typeof item.value === 'number') {
    return Number.isFinite(item.value) ? formatCount(item.value, locale) : '—'
  }
  if (typeof item.value === 'string') {
    return item.value === '' ? '—' : item.value
  }
  // 布尔值由 SettingValue 提前分流成开/关徽标，走不到这里。
  return JSON.stringify(item.value)
}

function SettingValue({
  item,
  locale,
  t,
}: {
  item: SettingItem
  locale: string
  t: TranslateFn
}) {
  // 布尔一律用开/关徽标：dangerous 项尤其不能出现任何可操作控件。
  if (typeof item.value === 'boolean') {
    return (
      <StatusBadge variant={item.value ? 'success' : 'neutral'}>
        {t(
          item.value
            ? 'admin.performance.settings.on'
            : 'admin.performance.settings.off',
        )}
      </StatusBadge>
    )
  }
  return (
    <span className="font-mono tabular-nums text-ink-1">
      {settingValueText(item, locale)}
      {unitSuffix(item.unit, t)}
    </span>
  )
}

function SourceBadge({ source, t }: { source: string; t: TranslateFn }) {
  const fromSettings = source === 'settings'
  return (
    <StatusBadge variant={fromSettings ? 'brand' : 'neutral'}>
      {t(
        fromSettings
          ? 'admin.performance.settings.source_settings_badge'
          : 'admin.performance.settings.source_default',
      )}
    </StatusBadge>
  )
}

function SettingsGroupBlock({
  group,
  locale,
  t,
}: {
  group: SettingsGroup
  locale: string
  t: TranslateFn
}) {
  return (
    <section
      data-testid="settings-group"
      data-group={group.id}
      className="rounded-xl border border-border-default bg-surface-card p-5"
    >
      <header className="flex items-start justify-between gap-4">
        <h3 className="text-card-title font-semibold leading-[22px] text-ink-1">
          {groupTitle(group.id, t)}
        </h3>
        {group.link === '' ? null : (
          <a
            href={group.link}
            className="text-meta text-brand underline underline-offset-2"
          >
            {t('admin.performance.settings.link')}
          </a>
        )}
      </header>
      <dl className="mt-3 grid grid-cols-1 gap-x-6 gap-y-2.5 sm:grid-cols-2 lg:grid-cols-3">
        {group.items.map((item) => (
          <div
            key={item.key}
            className="flex items-center justify-between gap-3"
          >
            <dt className="text-meta text-ink-3">{settingLabel(item.key, t)}</dt>
            <dd className="flex items-center gap-2 text-body">
              <SettingValue item={item} locale={locale} t={t} />
              <SourceBadge source={item.source} t={t} />
            </dd>
          </div>
        ))}
      </dl>
    </section>
  )
}

interface ParamRow {
  label: string
  value: string
  source: string
  effect: string
}

function ParamsTable({
  testId,
  rows,
  t,
}: {
  testId: string
  rows: readonly ParamRow[]
  t: TranslateFn
}) {
  return (
    <div className="overflow-x-auto">
      <table
        data-testid={testId}
        className="w-full min-w-[640px] text-left text-body"
      >
        <thead>
          <tr className="border-b border-border-hairline text-badge text-ink-3">
            <th scope="col" className="px-3 py-2 font-medium">
              {t('admin.performance.settings.col_param')}
            </th>
            <th scope="col" className="px-3 py-2 font-medium">
              {t('admin.performance.settings.col_value')}
            </th>
            <th scope="col" className="px-3 py-2 font-medium">
              {t('admin.performance.settings.col_source')}
            </th>
            <th scope="col" className="px-3 py-2 font-medium">
              {t('admin.performance.settings.col_effect')}
            </th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr
              key={`${row.label}-${row.value}`}
              className="border-b border-border-hairline last:border-b-0"
            >
              <th scope="row" className="px-3 py-2 text-left font-normal text-ink-1">
                {row.label}
              </th>
              <td className="px-3 py-2 font-mono tabular-nums text-ink-1">
                {row.value}
              </td>
              <td className="px-3 py-2 text-meta text-ink-2">{row.source}</td>
              <td className="px-3 py-2">
                <StatusBadge variant="neutral-outline">{row.effect}</StatusBadge>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function envValue(row: EnvRow, t: TranslateFn): string {
  // METRICS_TOKEN 没有 value 键：只报是否设置，永不回显值。
  if (row.value === undefined) {
    return t(
      row.set
        ? 'admin.performance.settings.set'
        : 'admin.performance.settings.unset',
    )
  }
  if (!row.configured || row.value === '') {
    return t('admin.performance.settings.unset')
  }
  return row.value
}

export interface SettingsZoneProps {
  section: SettingsSectionResponse
  config: PerfConfig
  canManage: boolean
  onSaved: () => void
  /** 见 ProbeScheduleForm：没有后端触发端点时调用方不传。 */
  onRunNow?: () => void
}

export function SettingsZone({
  section,
  config,
  canManage,
  onSaved,
  onRunNow,
}: SettingsZoneProps) {
  const { t, locale } = useI18n()
  const titleId = useId()

  const envRows: ParamRow[] = config.env.map((row) => ({
    label: paramLabel(row.key, t),
    value: envValue(row, t),
    source: t('admin.performance.settings.source_env'),
    effect: t('admin.performance.settings.effect_restart'),
  }))

  const fixedRows: ParamRow[] = config.fixed.map((row) => ({
    label: paramLabel(row.key, t),
    value: row.value,
    source: t('admin.performance.settings.source_default'),
    effect: t('admin.performance.settings.effect_code'),
  }))

  // 副标题的规模 = 本区四个测速键里实际存在于 section 的项数（部署未装配
  // 调度器时少一些）。缺失的键在表单里本来就是只读，不能算「可在线编辑」。
  const presentSectionKeys = new Set(section.items.map((item) => item.key))
  const editableCount = SCHEDULE_KEYS.filter((key) =>
    presentSectionKeys.has(key),
  ).length

  return (
    <section
      data-testid="settings-zone"
      aria-labelledby={titleId}
      className="space-y-4"
    >
      <ZoneRail
        titleId={titleId}
        title={t('admin.performance.settings.title')}
        sub={t('admin.performance.settings.rail_sub', {
          count: editableCount,
        })}
        note={t('admin.performance.settings.rail_note')}
      />
      <ProbeScheduleForm
        section={section}
        config={config}
        canManage={canManage}
        onSaved={onSaved}
        onRunNow={onRunNow}
      />

      {config.settings_groups.map((group) => (
        <SettingsGroupBlock
          key={group.id}
          group={group}
          locale={locale}
          t={t}
        />
      ))}

      <section
        data-testid="settings-readonly"
        className="rounded-xl border border-border-default bg-surface-card p-5"
      >
        <header>
          <h3 className="text-card-title font-semibold leading-[22px] text-ink-1">
            {t('admin.performance.settings.readonly_title')}
          </h3>
          <p className="mt-1 max-w-[72ch] text-meta text-ink-3">
            {t('admin.performance.settings.readonly_note')}
          </p>
        </header>
        <div className="mt-4 space-y-4">
          <ParamsTable
            testId="settings-env-table"
            rows={envRows}
            t={t}
          />
          <ParamsTable
            testId="settings-fixed-table"
            rows={fixedRows}
            t={t}
          />
        </div>
      </section>
    </section>
  )
}
