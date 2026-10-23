import { describe, expect, it } from 'vitest'

import {
  convertCreditsPerK,
  creditsToCny,
  currencyReference,
  formatCredit,
  formatPerfMilliseconds,
  formatPerfTPS,
  formatRatio,
  formatSuccessRate,
  formatTotal,
  parseCredit,
  rechargeReference,
} from './format'

describe('pricing amount formatting (int64 strings, never Number)', () => {
  it('parses only non-negative integer strings', () => {
    expect(parseCredit('0')).toBe(0n)
    expect(parseCredit('9007199254740993')).toBe(9007199254740993n)
    expect(parseCredit('')).toBeNull()
    expect(parseCredit('-1')).toBeNull()
    expect(parseCredit('1.5')).toBeNull()
    expect(parseCredit('abc')).toBeNull()
    expect(parseCredit(null)).toBeNull()
  })

  it('formats credits beyond Number.MAX_SAFE_INTEGER without precision loss', () => {
    // 2^53 + 1 用 Number 会变成 9007199254740992；字符串路径必须保持原值。
    expect(formatCredit('9007199254740993', 'en')).toBe('9,007,199,254,740,993')
    expect(formatCredit('500', 'en')).toBe('500')
    expect(formatCredit(null, 'en')).toBe('—')
  })

  it('converts credits per 1K tokens with up to 4 decimals', () => {
    expect(convertCreditsPerK('500', 'en')).toBe('0.5')
    expect(convertCreditsPerK('1500', 'en')).toBe('1.5')
    expect(convertCreditsPerK('1', 'en')).toBe('0.001')
    expect(convertCreditsPerK('1234567', 'en')).toBe('1,234.567')
    expect(convertCreditsPerK(null, 'en')).toBeNull()
  })

  it('converts credits to CNY with quota_per_unit and two decimals', () => {
    // quota_per_unit=100: 10,000 积分 = ¥100.00（TC-F-09-19-02）。
    expect(creditsToCny('10000', 100, 'en')).toBe('100.00')
    expect(creditsToCny('1', 100, 'en')).toBe('0.01')
    // 非零余数也必须保留 2 位：1510 / 100 = 15.10，而不是 15.1。
    expect(creditsToCny('1510', 100, 'en')).toBe('15.10')
    expect(creditsToCny('10', 0, 'en')).toBeNull()
  })

  it('derives the USD reference from the exchange rate', () => {
    const reference = currencyReference('10000', {
      quota_per_unit: 100,
      usd_exchange_rate: 7.1,
    }, 'en')
    expect(reference).not.toBeNull()
    expect(reference?.cny).toBe('¥100.00')
    expect(reference?.usd).toBe('$14.08')

    const tail = currencyReference('1510', {
      quota_per_unit: 100,
      usd_exchange_rate: 7.1,
    }, 'en')
    expect(tail?.cny).toBe('¥15.10')
    expect(tail?.usd).toBe('$2.13')
    // 充值价与双币种共用同一条整数分换算路径，不能一个 15.10、一个 15.1。
    expect(rechargeReference('1510', {
      quota_per_unit: 100,
      usd_exchange_rate: 7.1,
    }, 'en')).toBe('¥15.10')
  })

  it('formats ratios and totals', () => {
    expect(formatRatio(120)).toBe('1.20x')
    expect(formatRatio(null)).toBeNull()
    expect(formatTotal('128', 'en')).toBe('128')
    expect(formatTotal('128', 'de')).toBe('128')
  })

  it('formats performance values and renders missing data as a dash', () => {
    expect(formatPerfMilliseconds(210, 'en')).toBe('210 ms')
    expect(formatPerfMilliseconds(0, 'en')).toBe('0 ms')
    expect(formatPerfMilliseconds(null, 'en')).toBe('—')
    expect(formatPerfMilliseconds(undefined, 'en')).toBe('—')
    expect(formatPerfMilliseconds(Number.NaN, 'en')).toBe('—')

    expect(formatPerfTPS(42.55, 'en')).toBe('42.6')
    expect(formatPerfTPS(0, 'en')).toBe('0')
    expect(formatPerfTPS(null, 'en')).toBe('—')

    expect(formatSuccessRate(0.99, 'en')).toBe('99%')
    expect(formatSuccessRate(0.9833, 'en')).toBe('98.3%')
    expect(formatSuccessRate(null, 'en')).toBe('—')
  })
})
