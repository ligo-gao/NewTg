import { describe, expect, it } from 'vitest'

import {
  buildWalletTransactionsQuery,
  clearWalletFilters,
  datetimeLocalToIso,
  hasWalletFilters,
  validateWalletAmountRange,
  validateWalletRange,
  validateWalletSearch,
  walletRefForQuery,
} from './search'

describe('validateWalletSearch (R-M3-01)', () => {
  it('defaults to the top-up tab and keeps ?tab=transactions', () => {
    expect(validateWalletSearch({}).tab).toBeUndefined()
    expect(validateWalletSearch({ tab: 'topup' }).tab).toBeUndefined()
    expect(validateWalletSearch({ tab: 'bogus' }).tab).toBeUndefined()
    expect(validateWalletSearch({ tab: 'transactions' }).tab).toBe('transactions')
  })

  it('drops unknown enum values and keeps valid filters', () => {
    const search = validateWalletSearch({
      type: 'consume_pre',
      status: 'hold',
      dir: 'out',
      min: '100',
      max: 500,
      ref: 'abc',
      p: '2',
      page_size: '50',
    })
    expect(search).toMatchObject({
      type: 'consume_pre',
      status: 'hold',
      dir: 'out',
      min: 100,
      max: 500,
      ref: 'abc',
      p: 2,
      page_size: 50,
    })
    expect(validateWalletSearch({ type: 'nope' }).type).toBeUndefined()
    expect(validateWalletSearch({ dir: 'sideways' }).dir).toBeUndefined()
    // R-M3-10：UI 只接受 10/20/50。
    expect(validateWalletSearch({ page_size: '100' }).page_size).toBe(20)
  })
})

describe('range and amount validation', () => {
  it('accepts empty and equal bounds, rejects reversed and >90d', () => {
    expect(validateWalletRange(undefined, undefined)).toBeNull()
    expect(validateWalletRange('2026-10-01T00:00', '2026-10-01T00:00')).toBeNull()
    expect(validateWalletRange('2026-10-02T00:00', '2026-10-01T00:00')).toBe('order')
    expect(validateWalletRange('2026-01-01T00:00', '2026-10-01T00:00')).toBe('too_long')
    expect(validateWalletRange('not-a-date', undefined)).toBe('invalid')
  })

  it('rejects negative/non-integer amounts and min>max', () => {
    expect(validateWalletAmountRange('', '')).toBeNull()
    expect(validateWalletAmountRange('100', '100')).toBeNull()
    expect(validateWalletAmountRange('500', '100')).toBe('order')
    expect(validateWalletAmountRange('-1', '')).toBe('invalid')
    expect(validateWalletAmountRange('1.5', '')).toBe('invalid')
  })
})

describe('query building', () => {
  it('converts datetime-local to ISO and normalizes UUID refs (R-M3-07)', () => {
    const query = buildWalletTransactionsQuery({
      p: 1,
      page_size: 20,
      from: '2026-10-01T10:00',
      to: '2026-10-02T10:00',
      ref: '9F3A8D2E-1B2C-4D5E-8F90-112233445566',
      type: 'consume',
      status: 'settled',
      dir: 'out',
      min: 100,
      max: 500,
    })
    expect(query.from).toBe(new Date('2026-10-01T10:00').toISOString())
    expect(query.to).toBe(new Date('2026-10-02T10:00').toISOString())
    expect(query.ref).toBe('9f3a8d2e1b2c4d5e8f90112233445566')
    expect(query).toMatchObject({
      p: 1,
      page_size: 20,
      type: 'consume',
      status: 'settled',
      dir: 'out',
      min: 100,
      max: 500,
    })
  })

  it('keeps non-UUID references exact', () => {
    expect(walletRefForQuery('424242')).toBe('424242')
    expect(walletRefForQuery('batch/a')).toBe('batch/a')
    expect(datetimeLocalToIso('')).toBeUndefined()
    expect(datetimeLocalToIso('garbage')).toBeUndefined()
  })

  it('clearWalletFilters keeps the tab and page size only', () => {
    const cleared = clearWalletFilters({
      tab: 'transactions',
      type: 'bonus',
      ref: 'x',
      p: 3,
      page_size: 50,
    })
    expect(cleared).toEqual({ tab: 'transactions', p: 1, page_size: 50 })
    expect(hasWalletFilters(cleared)).toBe(false)
    expect(hasWalletFilters({ p: 1, page_size: 20, dir: 'in' })).toBe(true)
  })
})
