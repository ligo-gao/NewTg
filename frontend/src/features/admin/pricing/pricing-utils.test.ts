import { describe, expect, it } from 'vitest'

import {
  createDefaultTimeConfig,
  effectiveCacheReadPrice,
  effectiveCacheWritePrice,
  formatMultiplierPercent,
  matchLocalTimeRule,
  parseMultiplierPercent,
  strategyFromTimeConfig,
  timeConfigFromStrategy,
  wallClockParts,
  weekdayPeakPreset,
  type TimeConfigDraft,
} from './pricing-utils'

const OVERNIGHT: TimeConfigDraft = {
  enabled: true,
  timezone: 'Asia/Shanghai',
  holidayPolicy: 'none',
  defaultMultiplier: '100',
  windows: [
    {
      name: 'Weekend night',
      days: [6],
      start: '22:00',
      end: '08:00',
      multiplier: '50',
    },
  ],
}

describe('pricing-utils multipliers', () => {
  it('converts percent text to decimal multipliers within range', () => {
    expect(parseMultiplierPercent('50')).toEqual({ value: 0.5, invalid: false })
    expect(parseMultiplierPercent('125')).toEqual({ value: 1.25, invalid: false })
    expect(parseMultiplierPercent('0')).toEqual({ value: null, invalid: true })
    expect(parseMultiplierPercent('10001')).toEqual({ value: null, invalid: true })
    expect(parseMultiplierPercent('')).toEqual({ value: null, invalid: false })
    expect(formatMultiplierPercent(0.5)).toBe('50')
    expect(formatMultiplierPercent(1.25)).toBe('125')
  })

  it('round-trips a strategy through the editor draft', () => {
    const built = strategyFromTimeConfig(OVERNIGHT)
    expect(built.errors).toEqual({})
    expect(built.strategy?.time?.default_multiplier).toBe(1)
    expect(built.strategy?.time?.windows?.[0]?.multiplier).toBe(0.5)

    const draft = timeConfigFromStrategy(built.strategy)
    expect(draft.enabled).toBe(true)
    expect(draft.defaultMultiplier).toBe('100')
    expect(draft.windows[0]).toMatchObject({
      name: 'Weekend night',
      days: [6],
      start: '22:00',
      end: '08:00',
      multiplier: '50',
    })
  })

  it('does not build a strategy while time pricing is disabled', () => {
    const built = strategyFromTimeConfig(createDefaultTimeConfig())
    expect(built.strategy).toBeNull()
    expect(built.errors).toEqual({})
  })
})

describe('pricing-utils time matching', () => {
  it('uses the start day for overnight windows', () => {
    // Saturday 23:00 — window started on Saturday.
    expect(matchLocalTimeRule(OVERNIGHT, 6, 23 * 60)).toEqual({
      window: 'Weekend night',
      multiplier: 0.5,
    })
    // Sunday 02:00 — still the Saturday-started window.
    expect(matchLocalTimeRule(OVERNIGHT, 7, 2 * 60)).toEqual({
      window: 'Weekend night',
      multiplier: 0.5,
    })
    // Saturday 21:00 — before the window opens.
    expect(matchLocalTimeRule(OVERNIGHT, 6, 21 * 60)).toEqual({
      window: '',
      multiplier: 1,
    })
    // Monday 02:00 — after the window closed.
    expect(matchLocalTimeRule(OVERNIGHT, 1, 2 * 60)).toEqual({
      window: '',
      multiplier: 1,
    })
  })

  it('lets the last matching window win', () => {
    const config: TimeConfigDraft = {
      ...OVERNIGHT,
      windows: [
        { name: 'Base', days: [1], start: '09:00', end: '18:00', multiplier: '100' },
        { name: 'Late', days: [1], start: '12:00', end: '18:00', multiplier: '80' },
      ],
    }
    expect(matchLocalTimeRule(config, 1, 13 * 60)).toEqual({
      window: 'Late',
      multiplier: 0.8,
    })
  })

  it('derives the weekday from the wall-clock value', () => {
    expect(wallClockParts('2026-10-26T09:30')).toMatchObject({
      date: '2026-10-26',
      day: 1,
      minutes: 570,
      time: '09:30',
    })
    expect(wallClockParts('2026-11-01T00:00')?.day).toBe(7)
    expect(wallClockParts('not-a-time')).toBeNull()
  })
})

describe('cache tier fallbacks', () => {
  it('falls back to the miss price when the hit price is unset', () => {
    expect(effectiveCacheReadPrice(200, null)).toBe(200)
    expect(effectiveCacheReadPrice(200, 0)).toBe(0)
    expect(effectiveCacheReadPrice(null, null)).toBeNull()
  })

  it('falls back to miss × 1.25 for cache writes', () => {
    expect(effectiveCacheWritePrice(200, null)).toBe(250)
    expect(effectiveCacheWritePrice(200, 0)).toBe(0)
    expect(effectiveCacheWritePrice(null, null)).toBeNull()
  })
})

describe('weekdayPeakPreset', () => {
  it('fills two weekday peak windows with a 50% default, for any model', () => {
    const t = (key: string) => key
    expect(weekdayPeakPreset(t)).toEqual({
      enabled: true,
      timezone: 'Asia/Shanghai',
      holidayPolicy: 'cn_offpeak',
      defaultMultiplier: '50',
      windows: [
        {
          name: 'admin.pricing.time_preset_window_morning',
          days: [1, 2, 3, 4, 5],
          start: '09:00',
          end: '12:00',
          multiplier: '100',
        },
        {
          name: 'admin.pricing.time_preset_window_afternoon',
          days: [1, 2, 3, 4, 5],
          start: '14:00',
          end: '18:00',
          multiplier: '100',
        },
      ],
    })
  })

  it('produces a schedule that evaluates to peak inside and idle outside', () => {
    const config = weekdayPeakPreset((key) => key)
    expect(matchLocalTimeRule(config, 1, 10 * 60).multiplier).toBe(1)
    expect(matchLocalTimeRule(config, 1, 13 * 60).multiplier).toBe(0.5)
    expect(matchLocalTimeRule(config, 6, 10 * 60).multiplier).toBe(0.5)
  })
})
