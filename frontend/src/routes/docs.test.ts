import { describe, expect, it } from 'vitest'

import { docsDisabled, docsNeedsSignIn, resolveDocsTarget } from '@/features/public/docs-page'

describe('/docs ruling (F-09-38)', () => {
  it('accepts absolute http(s) and single-slash relative targets', () => {
    expect(resolveDocsTarget('https://docs.example.com')).toEqual({
      kind: 'external',
      href: 'https://docs.example.com',
    })
    expect(resolveDocsTarget('/guide')).toEqual({ kind: 'internal', href: '/guide' })
  })

  it('rejects empty, protocol-relative and script URLs', () => {
    expect(resolveDocsTarget('').kind).toBe('unavailable')
    expect(resolveDocsTarget('   ').kind).toBe('unavailable')
    expect(resolveDocsTarget('//evil.example').kind).toBe('unavailable')
    expect(resolveDocsTarget('javascript:alert(1)').kind).toBe('unavailable')
    expect(resolveDocsTarget('data:text/html,hi').kind).toBe('unavailable')
  })

  it('applies nav.docs.require_auth to direct links', () => {
    const nav = { docs: { enabled: true, require_auth: true } }
    expect(docsNeedsSignIn(nav, false)).toBe(true)
    expect(docsNeedsSignIn(nav, true)).toBe(false)
    expect(docsNeedsSignIn({ docs: { enabled: false, require_auth: true } }, false)).toBe(false)
    expect(docsNeedsSignIn(undefined, false)).toBe(false)
  })

  it('treats nav.docs.enabled=false as "docs do not exist" (P2)', () => {
    // The entry is hidden from every surface; `/docs` must not keep
    // redirecting to `site.docs_url` behind the operator's back.
    expect(docsDisabled({ docs: { enabled: false, require_auth: false } })).toBe(true)
    expect(docsDisabled({ docs: { enabled: true, require_auth: false } })).toBe(false)
    // Missing values keep the documented default (`enabled=true`).
    expect(docsDisabled(undefined)).toBe(false)
  })
})
