import { describe, expect, it } from 'vitest'

import { isForcedPasswordChangePath } from './auth-failures'

describe('isForcedPasswordChangePath', () => {
  it('matches the forced password-change route', () => {
    expect(isForcedPasswordChangePath('/security', '?force=password')).toBe(true)
  })

  it('does not match the ordinary security page', () => {
    expect(isForcedPasswordChangePath('/security', '')).toBe(false)
    expect(isForcedPasswordChangePath('/security', '?force=other')).toBe(false)
  })

  it('does not match other paths carrying the same query', () => {
    expect(isForcedPasswordChangePath('/dashboard', '?force=password')).toBe(
      false,
    )
    expect(isForcedPasswordChangePath('/401', '?force=password')).toBe(false)
  })
})
