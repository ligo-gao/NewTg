import { describe, expect, it } from 'vitest'

import {
  DEFAULT_USAGE_LOG_SEARCH,
  isUuid,
  normalizeRequestId,
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
        token_id: 'abc',
        tab: 'performance',
      }),
    ).toEqual(DEFAULT_USAGE_LOG_SEARCH)
  })

  it('keeps a malformed request_id so the page can report it', () => {
    // The URL wins over the default view: dropping the value silently would
    // leave the user on an unfiltered list with no explanation (F-05-01).
    expect(
      validateUsageLogSearch({ request_id: 'not-a-uuid' }).request_id,
    ).toBe('not-a-uuid')
  })

  it('serialises only the stats tab and normalises an explicit logs tab', () => {
    expect(validateUsageLogSearch({ tab: 'stats' }).tab).toBe('stats')
    expect(validateUsageLogSearch({ tab: 'logs' }).tab).toBeUndefined()
    expect(validateUsageLogSearch({ tab: 'nope' }).tab).toBeUndefined()
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

  it('recognises compact and hyphenated hex IDs, without version bits', () => {
    expect(isUuid('9f3a8d2e-1b2c-4d5e-8f90-112233445566')).toBe(true)
    // The gateway mints opaque 32-hex IDs (no RFC version/variant bits).
    expect(isUuid('9f3a8d2e1b2c4d5e8f90112233445566')).toBe(true)
    // 8-4-4-4-12 with arbitrary hex nibbles (version 0 / variant 0) is still
    // a valid lookup key: the backend parses it with uuid.Parse.
    expect(isUuid('9f3a8d2e-1b2c-0d5e-0f90-112233445566')).toBe(true)
    expect(isUuid('9f3a8d2e-1b2c-4d5e-8f90-11223344556')).toBe(false)
    expect(isUuid('9f3a8d2e1b2c4d5e8f9011223344556')).toBe(false)
    expect(isUuid('hello')).toBe(false)
  })

  it('normalises request IDs for comparison', () => {
    expect(
      normalizeRequestId('9F3A8D2E-1B2C-4D5E-8F90-112233445566'),
    ).toBe('9f3a8d2e1b2c4d5e8f90112233445566')
    expect(normalizeRequestId('9f3a8d2e1b2c4d5e8f90112233445566')).toBe(
      '9f3a8d2e1b2c4d5e8f90112233445566',
    )
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
