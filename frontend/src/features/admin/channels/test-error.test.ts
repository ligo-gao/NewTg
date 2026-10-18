import { describe, expect, it } from 'vitest'

import { testErrorKey } from './test-error'

/**
 * The backend is being aligned to the modules-06 F-06-06 `error_class` enum
 * (auth | timeout | server | network | param). Every value must map to its
 * message; anything outside the enum degrades to "Unknown error" rather than
 * leaking a raw upstream string (GC-6).
 */
describe('testErrorKey', () => {
  it.each([
    ['auth', 'common.channels.testError.auth'],
    ['timeout', 'common.channels.testError.timeout'],
    ['server', 'common.channels.testError.server'],
    ['network', 'common.channels.testError.network'],
    ['param', 'common.channels.testError.param'],
  ])('maps %s to its localized reason', (errorClass, expectedKey) => {
    expect(testErrorKey(errorClass)).toBe(expectedKey)
  })

  it('falls back to "unknown" for an absent class', () => {
    expect(testErrorKey(undefined)).toBe('common.channels.testError.unknown')
  })

  it('falls back to "unknown" for values outside the enum', () => {
    expect(testErrorKey('upstream_auth')).toBe(
      'common.channels.testError.unknown',
    )
    expect(testErrorKey('')).toBe('common.channels.testError.unknown')
  })
})
