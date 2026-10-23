import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { TextField } from '@/shared/ui/form'

import {
  overnightEndDay,
  parseHhMm,
  TIMEZONE_OPTIONS,
  weekdayKey,
  weekdayLongKey,
  type TimeConfigDraft,
  type TimeWindowDraft,
} from './pricing-utils'

export interface TimeWindowEditorProps {
  value: TimeConfigDraft
  /** 字段路径 → 已翻译的错误文案（如 `windows.0.start`）。 */
  errors?: Record<string, string>
  onChange: (next: TimeConfigDraft) => void
  /** 第二阶段能力关闭时（如高级表达式）整体禁用。 */
  disabled?: boolean
}

const ALL_DAYS = [1, 2, 3, 4, 5, 6, 7]

const SELECT_CLASSES =
  'h-10 rounded-lg border border-border-default bg-surface-card px-3 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50'

/**
 * 时段价格编辑器（F-06-14）：启用开关、IANA 时区、节假日策略、默认倍率
 * 与窗口列表。倍率在界面上是百分比（50 = 半价），提交时由
 * `strategyFromTimeConfig` 统一换算成十进制倍数。
 *
 * 跨夜窗口（end <= start）的 days 指窗口**开始**的那一天；行下方的提示
 * 明确写出结束日是次日，避免把 `days:[6] 22:00–08:00` 理解成周日早晨。
 */
export function TimeWindowEditor({
  value,
  errors = {},
  onChange,
  disabled = false,
}: TimeWindowEditorProps) {
  const { t } = useI18n()

  function update(patch: Partial<TimeConfigDraft>): void {
    onChange({ ...value, ...patch })
  }

  function updateWindow(index: number, patch: Partial<TimeWindowDraft>): void {
    update({
      windows: value.windows.map((window, current) =>
        current === index ? { ...window, ...patch } : window,
      ),
    })
  }

  function toggleDay(index: number, day: number): void {
    const window = value.windows[index]
    if (window === undefined) return
    const days = window.days.includes(day)
      ? window.days.filter((current) => current !== day)
      : [...window.days, day].sort((a, b) => a - b)
    updateWindow(index, { days })
  }

  function addWindow(): void {
    update({
      windows: [
        ...value.windows,
        {
          name: '',
          days: [1, 2, 3, 4, 5],
          start: '09:00',
          end: '18:00',
          multiplier: '100',
        },
      ],
    })
  }

  function removeWindow(index: number): void {
    update({ windows: value.windows.filter((_, current) => current !== index) })
  }

  return (
    <section
      aria-label={t('admin.pricing.time_section')}
      className="flex flex-col gap-3 rounded-lg border border-border-hairline bg-surface-card p-3"
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-col">
          <h3 className="text-sm font-semibold text-ink-1">
            {t('admin.pricing.time_section')}
          </h3>
          <p className="text-xs text-ink-3">{t('admin.pricing.time_section_hint')}</p>
        </div>
        <label className="flex items-center gap-2 text-[13px] font-medium text-ink-1">
          <input
            type="checkbox"
            className="size-4 rounded border-border-default accent-[var(--brand)]"
            checked={value.enabled}
            disabled={disabled}
            aria-label={t('admin.pricing.time_enabled')}
            onChange={(event) => {
              update({ enabled: event.target.checked })
            }}
          />
          {t('admin.pricing.time_enabled')}
        </label>
      </div>

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <div className="flex flex-col gap-1.5">
          <label htmlFor="pricing-time-timezone" className="text-[13px] font-medium text-ink-1">
            {t('admin.pricing.time_timezone')}
          </label>
          <select
            id="pricing-time-timezone"
            className={SELECT_CLASSES}
            value={value.timezone}
            disabled={disabled}
            aria-invalid={errors.timezone !== undefined ? true : undefined}
            onChange={(event) => {
              update({ timezone: event.target.value })
            }}
          >
            {TIMEZONE_OPTIONS.map((zone) => (
              <option key={zone} value={zone}>
                {zone}
              </option>
            ))}
          </select>
          {errors.timezone !== undefined ? (
            <p className="text-xs text-danger">{errors.timezone}</p>
          ) : null}
        </div>

        <div className="flex flex-col gap-1.5">
          <label htmlFor="pricing-holiday-policy" className="text-[13px] font-medium text-ink-1">
            {t('admin.pricing.time_holiday')}
          </label>
          <select
            id="pricing-holiday-policy"
            className={SELECT_CLASSES}
            value={value.holidayPolicy}
            disabled={disabled}
            onChange={(event) => {
              update({ holidayPolicy: event.target.value as TimeConfigDraft['holidayPolicy'] })
            }}
          >
            <option value="none">{t('admin.pricing.holiday_none')}</option>
            <option value="cn_offpeak">{t('admin.pricing.holiday_cn_offpeak')}</option>
          </select>
          <p className="text-xs text-ink-3">{t('admin.pricing.time_holiday_hint')}</p>
        </div>
      </div>

      {value.enabled ? (
        <>
          <div className="flex flex-wrap items-end gap-3">
            <div className="flex w-48 flex-col gap-1.5">
              <TextField
                label={t('admin.pricing.time_default_multiplier')}
                inputMode="decimal"
                value={value.defaultMultiplier}
                disabled={disabled}
                error={errors.defaultMultiplier}
                onChange={(event) => {
                  update({ defaultMultiplier: event.target.value })
                }}
              />
            </div>
            <p className="pb-2 text-xs text-ink-3">{t('admin.pricing.time_default_hint')}</p>
          </div>

          <div className="flex flex-col gap-2">
            <div className="flex items-center justify-between">
              <h4 className="text-[13px] font-medium text-ink-1">
                {t('admin.pricing.time_windows')}
              </h4>
              <Button
                variant="secondary"
                className="h-8 px-2 text-[13px]"
                disabled={disabled}
                onClick={addWindow}
              >
                {t('admin.pricing.time_add_window')}
              </Button>
            </div>

            {value.windows.length === 0 ? (
              <p className="text-xs text-ink-3">{t('admin.pricing.time_windows_empty')}</p>
            ) : (
              <ul className="flex flex-col gap-2">
                {value.windows.map((window, index) => {
                  const overnight =
                    parseHhMm(window.start) !== null &&
                    parseHhMm(window.end) !== null &&
                    (parseHhMm(window.end) as number) <= (parseHhMm(window.start) as number)
                  const label = index + 1
                  return (
                    <li
                      key={index}
                      role="group"
                      aria-label={`${t('admin.pricing.time_window')} ${label}`}
                      className="flex flex-col gap-2 rounded-lg border border-border-hairline bg-surface-subtle p-2"
                    >
                      <div className="flex flex-wrap items-end gap-2">
                        <div className="min-w-[160px] flex-1">
                          <TextField
                            label={t('admin.pricing.time_window_name')}
                            value={window.name}
                            maxLength={60}
                            disabled={disabled}
                            placeholder={t('admin.pricing.time_window_name_placeholder')}
                            onChange={(event) => {
                              updateWindow(index, { name: event.target.value })
                            }}
                          />
                        </div>
                        <div className="flex w-28 flex-col gap-1.5">
                          <label
                            htmlFor={`pricing-window-start-${index}`}
                            className="text-[13px] font-medium text-ink-1"
                          >
                            {t('admin.pricing.time_start')} {label}
                          </label>
                          <input
                            id={`pricing-window-start-${index}`}
                            type="time"
                            className={`${SELECT_CLASSES} w-full`}
                            value={window.start}
                            disabled={disabled}
                            aria-invalid={
                              errors[`windows.${index}.start`] !== undefined ? true : undefined
                            }
                            onChange={(event) => {
                              updateWindow(index, { start: event.target.value })
                            }}
                          />
                        </div>
                        <div className="flex w-28 flex-col gap-1.5">
                          <label
                            htmlFor={`pricing-window-end-${index}`}
                            className="text-[13px] font-medium text-ink-1"
                          >
                            {t('admin.pricing.time_end')} {label}
                          </label>
                          <input
                            id={`pricing-window-end-${index}`}
                            type="time"
                            className={`${SELECT_CLASSES} w-full`}
                            value={window.end}
                            disabled={disabled}
                            aria-invalid={
                              errors[`windows.${index}.end`] !== undefined ? true : undefined
                            }
                            onChange={(event) => {
                              updateWindow(index, { end: event.target.value })
                            }}
                          />
                        </div>
                        <div className="w-28">
                          <TextField
                            label={`${t('admin.pricing.time_multiplier')} ${label}`}
                            inputMode="decimal"
                            value={window.multiplier}
                            disabled={disabled}
                            error={errors[`windows.${index}.multiplier`]}
                            onChange={(event) => {
                              updateWindow(index, { multiplier: event.target.value })
                            }}
                          />
                        </div>
                        <Button
                          variant="ghost"
                          className="h-9 px-2 text-[13px] text-danger hover:bg-danger-bg"
                          disabled={disabled}
                          aria-label={`${t('admin.pricing.time_remove_window')} ${label}`}
                          onClick={() => {
                            removeWindow(index)
                          }}
                        >
                          {t('admin.pricing.time_remove')}
                        </Button>
                      </div>

                      <div
                        role="group"
                        aria-label={t('admin.pricing.time_days')}
                        className="flex flex-wrap gap-1"
                      >
                        {ALL_DAYS.map((day) => {
                          const pressed = window.days.includes(day)
                          return (
                            <button
                              key={day}
                              type="button"
                              aria-pressed={pressed}
                              aria-label={t(weekdayLongKey(day))}
                              disabled={disabled}
                              className={[
                                'h-7 min-w-9 rounded-md border px-1.5 text-xs font-medium',
                                'focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none',
                                pressed
                                  ? 'border-brand-line bg-brand-subtle text-brand'
                                  : 'border-border-default bg-surface-card text-ink-2',
                              ].join(' ')}
                              onClick={() => {
                                toggleDay(index, day)
                              }}
                            >
                              {t(weekdayKey(day))}
                            </button>
                          )
                        })}
                      </div>

                      {errors[`windows.${index}.start`] !== undefined ? (
                        <p className="text-xs text-danger">
                          {errors[`windows.${index}.start`]}
                        </p>
                      ) : null}
                      {errors[`windows.${index}.end`] !== undefined ? (
                        <p className="text-xs text-danger">
                          {errors[`windows.${index}.end`]}
                        </p>
                      ) : null}

                      {overnight ? (
                        <p className="text-xs text-ink-3">
                          {t('admin.pricing.time_overnight_hint', {
                            startDay:
                              window.days.length > 0
                                ? t(weekdayLongKey(window.days[0] ?? 1))
                                : t(weekdayKey(1)),
                            endDay:
                              window.days.length > 0
                                ? t(weekdayLongKey(overnightEndDay(window.days[0] ?? 1)))
                                : t(weekdayKey(2)),
                            start: window.start,
                            end: window.end,
                          })}
                        </p>
                      ) : null}
                    </li>
                  )
                })}
              </ul>
            )}
          </div>
        </>
      ) : (
        <p className="text-xs text-ink-3">{t('admin.pricing.time_disabled_hint')}</p>
      )}
    </section>
  )
}

/** 常用 IANA 时区；保存时由服务端 time.LoadLocation 再次校验。 */
