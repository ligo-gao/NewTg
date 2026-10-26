import { useI18n } from '@/shared/lib/i18n-context'

import {
  formatMultiplierPercent,
  matchLocalTimeRule,
  parseMultiplierPercent,
  wallClockForWeekday,
  wallClockParts,
  weekdayKey,
  weekdayLongKey,
  type TimeConfigDraft,
} from './pricing-utils'

export interface PricingBandProps {
  config: TimeConfigDraft
  /** 当前试算时间（`datetime-local` 值）。 */
  selected: string
  /** 点击格子后写回新的试算时间。 */
  onSelect: (value: string) => void
}

const ALL_DAYS = [1, 2, 3, 4, 5, 6, 7]
const ALL_HOURS = Array.from({ length: 24 }, (_, index) => index)

type BandKind = 'peak' | 'off' | 'base'

function kindFor(multiplier: number, peak: number, flat: boolean): BandKind {
  // 全部时段同价：没有峰谷可谈，整条带都是「统一价」。
  if (flat) return 'base'
  // 用极小容差比较浮点倍数，避免 0.5*100 之类的噪声把最高档显示成低谷。
  if (Math.abs(multiplier - peak) < 1e-9) return 'peak'
  return 'off'
}

/**
 * 7×24 时段带（F-06-14）：按星期与小时展示峰/平/谷，点击格子把试算
 * 时间跳转到该时段。节假日不在这里展开（日历在服务端），带只反映
 * 星期与窗口规则——这正是本地试算使用的同一套判定。
 */
export function PricingBand({ config, selected, onSelect }: PricingBandProps) {
  const { t } = useI18n()

  if (!config.enabled) {
    return (
      <p className="text-meta text-ink-3" data-testid="pricing-band-disabled">
        {t('admin.pricing.band_disabled')}
      </p>
    )
  }

  const fallback = parseMultiplierPercent(config.defaultMultiplier)
  const defaultMultiplier = fallback.invalid ? 1 : (fallback.value ?? 1)
  // 峰 = 整个策略里的最高倍率；其余（含默认倍率时段）都按低谷着色，
  // 否则「工作日白天 100%、高峰 150%」里的 12 小时会看起来没有价格。
  const multipliers = [
    defaultMultiplier,
    ...config.windows.map((window) => {
      const parsed = parseMultiplierPercent(window.multiplier)
      return parsed.invalid ? 1 : (parsed.value ?? 1)
    }),
  ]
  const peakMultiplier = Math.max(...multipliers)
  const flat =
    config.windows.length === 0 ||
    multipliers.every((multiplier) => Math.abs(multiplier - peakMultiplier) < 1e-9)
  const selection = wallClockParts(selected)

  return (
    <section aria-label={t('admin.pricing.band_title')} className="flex flex-col gap-2">
      <div className="overflow-x-auto">
        <div
          role="grid"
          aria-label={t('admin.pricing.band_title')}
          data-testid="pricing-band"
          className="grid min-w-[640px] grid-cols-[36px_repeat(24,minmax(18px,1fr))] gap-[2px]"
        >
          <div aria-hidden="true" />
          {ALL_HOURS.map((hour) => (
            <div
              key={`head-${hour}`}
              className="text-center text-badge tabular-nums text-ink-3"
            >
              {hour % 3 === 0 ? hour : ''}
            </div>
          ))}
          {ALL_DAYS.map((day) => (
            <BandRow
              key={day}
              config={config}
              day={day}
              peakMultiplier={peakMultiplier}
              flat={flat}
              selectedDay={selection?.day ?? null}
              selectedHour={
                selection === null ? null : Math.floor(selection.minutes / 60)
              }
              selected={selected}
              onSelect={onSelect}
              dayLabel={t(weekdayKey(day))}
              dayLongLabel={t(weekdayLongKey(day))}
              defaultRule={t('admin.pricing.band_default_rule')}
              cellLabel={t('admin.pricing.band_cell')}
            />
          ))}
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-3 text-meta text-ink-3">
        <span className="flex items-center gap-1">
          <span aria-hidden="true" className="size-3 rounded-sm bg-warning-bg" />
          {t('admin.pricing.band_legend_peak')}
        </span>
        <span className="flex items-center gap-1">
          <span aria-hidden="true" className="size-3 rounded-sm bg-success-bg" />
          {t('admin.pricing.band_legend_off')}
        </span>
        <span className="flex items-center gap-1">
          <span aria-hidden="true" className="size-3 rounded-sm bg-surface-subtle" />
          {t('admin.pricing.band_legend_base')}
        </span>
        <span>{t('admin.pricing.band_hint')}</span>
      </div>
    </section>
  )
}

interface BandRowProps {
  config: TimeConfigDraft
  day: number
  peakMultiplier: number
  flat: boolean
  selectedDay: number | null
  selectedHour: number | null
  selected: string
  onSelect: (value: string) => void
  dayLabel: string
  dayLongLabel: string
  defaultRule: string
  cellLabel: string
}

function BandRow({
  config,
  day,
  peakMultiplier,
  flat,
  selectedDay,
  selectedHour,
  selected,
  onSelect,
  dayLabel,
  dayLongLabel,
  defaultRule,
  cellLabel,
}: BandRowProps) {
  return (
    <>
      <div className="flex items-center text-badge font-medium text-ink-2">{dayLabel}</div>
      {ALL_HOURS.map((hour) => {
        const match = matchLocalTimeRule(config, day, hour * 60)
        const kind = kindFor(match.multiplier, peakMultiplier, flat)
        const isSelected = selectedDay === day && selectedHour === hour
        const rule = match.window === '' ? defaultRule : match.window
        const label = cellLabel
          .replace('{day}', dayLongLabel)
          .replace('{hour}', `${String(hour).padStart(2, '0')}:00`)
          .replace('{rule}', rule)
          .replace('{multiplier}', formatMultiplierPercent(match.multiplier))
        return (
          <button
            key={`${day}-${hour}`}
            type="button"
            data-kind={kind}
            data-day={day}
            data-hour={hour}
            aria-current={isSelected ? 'true' : undefined}
            aria-label={label}
            title={label}
            className={[
              'h-5 w-full rounded-sm border',
              'focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
              kind === 'peak'
                ? 'border-warning-line bg-warning-bg'
                : kind === 'off'
                  ? 'border-success-line bg-success-bg'
                  : 'border-border-hairline bg-surface-subtle',
              isSelected ? 'ring-2 ring-brand-line' : '',
            ].join(' ')}
            onClick={() => {
              onSelect(wallClockForWeekday(selected, day, hour))
            }}
          />
        )
      })}
    </>
  )
}
