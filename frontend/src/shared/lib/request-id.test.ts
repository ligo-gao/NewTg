import { describe, expect, it } from 'vitest'

import { isUuid, normalizeRequestId } from './request-id'

describe('request-id helpers', () => {
  it('recognises compact and hyphenated hex IDs, without version bits', () => {
    // The gateway mints opaque 32-hex IDs (no RFC version/variant bits).
    expect(isUuid('9f3a8d2e1b2c4d5e8f90112233445566')).toBe(true)
    expect(isUuid('9f3a8d2e-1b2c-4d5e-8f90-112233445566')).toBe(true)
    // Arbitrary hex nibbles (version 0 / variant 0) are still valid lookup
    // keys: the backend parses them with `uuid.Parse`.
    expect(isUuid('9f3a8d2e-1b2c-0d5e-0f90-112233445566')).toBe(true)
    expect(isUuid('9f3a8d2e-1b2c-4d5e-8f90-11223344556')).toBe(false)
    expect(isUuid('9f3a8d2e1b2c4d5e8f9011223344556')).toBe(false)
    expect(isUuid('hello')).toBe(false)
  })

  it('normalises hyphenated, uppercase IDs to the canonical comparison key', () => {
    expect(
      normalizeRequestId('9F3A8D2E-1B2C-4D5E-8F90-112233445566'),
    ).toBe('9f3a8d2e1b2c4d5e8f90112233445566')
  })

  it('leaves compact lowercase IDs unchanged', () => {
    expect(normalizeRequestId('9f3a8d2e1b2c4d5e8f90112233445566')).toBe(
      '9f3a8d2e1b2c4d5e8f90112233445566',
    )
  })

  it('does not treat non-UUID references as UUIDs', () => {
    expect(isUuid('424242')).toBe(false)
    expect(isUuid('batch/a')).toBe(false)
  })
})
