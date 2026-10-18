import { describe, expect, it } from 'vitest'

import { DEFAULT_AUTH_REDIRECT, sanitizeAuthRedirect } from './auth-redirect'

describe('sanitizeAuthRedirect', () => {
  it('keeps same-origin relative destinations', () => {
    expect(sanitizeAuthRedirect('/profile?tab=security')).toBe('/profile?tab=security')
    expect(sanitizeAuthRedirect('/admin/users')).toBe('/admin/users')
  })

  it('falls back for absolute, protocol-relative and backslash tricks', () => {
    expect(sanitizeAuthRedirect('https://evil.example/steal')).toBe(DEFAULT_AUTH_REDIRECT)
    expect(sanitizeAuthRedirect('//evil.example/steal')).toBe(DEFAULT_AUTH_REDIRECT)
    expect(sanitizeAuthRedirect('/\\evil.example/steal')).toBe(DEFAULT_AUTH_REDIRECT)
    expect(sanitizeAuthRedirect('javascript:alert(1)')).toBe(DEFAULT_AUTH_REDIRECT)
  })

  it('rejects control characters that the URL parser would strip', () => {
    // `%2F%09%2Fevil.example` arrives decoded as `/\t/evil.example`; the parser
    // removes the tab and turns it into the protocol-relative `//evil.example`.
    expect(sanitizeAuthRedirect('/\t/evil.example')).toBe(DEFAULT_AUTH_REDIRECT)
    expect(sanitizeAuthRedirect('/\n/evil.example')).toBe(DEFAULT_AUTH_REDIRECT)
    expect(sanitizeAuthRedirect('/\r/evil.example')).toBe(DEFAULT_AUTH_REDIRECT)
    expect(sanitizeAuthRedirect('/profile\t?next=https://evil.example')).toBe(
      DEFAULT_AUTH_REDIRECT,
    )
  })

  it('falls back for empty values and bare relative strings', () => {
    expect(sanitizeAuthRedirect(undefined)).toBe(DEFAULT_AUTH_REDIRECT)
    expect(sanitizeAuthRedirect('')).toBe(DEFAULT_AUTH_REDIRECT)
    expect(sanitizeAuthRedirect('profile')).toBe(DEFAULT_AUTH_REDIRECT)
  })
})
