import { describe, expect, it } from 'vitest'

import {
  DEFAULT_USAGE_LOG_SEARCH,
  isUuid,
  validateUsageLogSearch,
  validateCustomRange,
} from './search'

describe('usage log search validation', () => {
  it('falls back to defaults for unknown or malformed values', () => {
    expect(
      validateUsageLogSearch({
        range: 'forever',
        p: '-2',
        page_size: '999',
        status: 'maybe',
        request_id: 'not-a-uuid',
        token_id: 'abc',
      }),
    ).toEqual(DEFAULT_USAGE_LOG_SEARCH)
  })

  it('keeps valid filter values, including the admin username', () => {
    expect(
      validateUsageLogSearch({
        range: '7d',
        model: 'gpt-4o',
        group: 'default',
        token_id: '12',
        request_id: '9f3a8d2e-1b2c-4d5e-8f90-112233445566',
        status: 'refused',
        username: 'alice',
        p: '3',
        page_size: '50',
      }),
    ).toEqual({
      range: '7d',
      model: 'gpt-4o',
      group: 'default',
      token_id: 12,
      request_id: '9f3a8d2e-1b2c-4d5e-8f90-112233445566',
      status: 'refused',
      username: 'alice',
      p: 3,
      page_size: 50,
    })
  })

  it('recognises UUIDs only', () => {
    expect(isUuid('9f3a8d2e-1b2c-4d5e-8f90-112233445566')).toBe(true)
    expect(isUuid('9f3a8d2e-1b2c-4d5e-8f90-11223344556')).toBe(false)
    expect(isUuid('hello')).toBe(false)
  })

  it('validates custom ranges', () => {
    expect(validateCustomRange('', '')).toBe('required')
    expect(validateCustomRange('2026-04-02T00:00', '2026-01-01T00:00')).toBe(
      'order',
    )
    expect(validateCustomRange('2026-01-01T00:00', '2026-04-02T00:00')).toBe(
      'too_long',
    )
    expect(validateCustomRange('2026-01-01T00:00', '2026-02-01T00:00')).toBeNull()
  })
})
