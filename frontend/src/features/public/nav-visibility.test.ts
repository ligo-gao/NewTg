import { describe, expect, it } from 'vitest'

import {
  isNavEntryVisible,
  isSafePublicHref,
  navEntryOf,
  publicRouteGuardRedirect,
  signInHref,
  visibleNavItems,
} from './nav-visibility'

const FULL_NAV = {
  home: { enabled: true, require_auth: false },
  console: { enabled: true, require_auth: true },
  models: { enabled: true, require_auth: false },
  pricing: { enabled: true, require_auth: false },
  rankings: { enabled: true, require_auth: false },
  docs: { enabled: true, require_auth: false },
  about: { enabled: true, require_auth: false },
}

describe('nav visibility matrix (TC-F-09-34-01)', () => {
  it('applies enabled && (!require_auth || isLoggedIn)', () => {
    expect(isNavEntryVisible(FULL_NAV, 'pricing', true)).toBe(true)
    expect(isNavEntryVisible(FULL_NAV, 'pricing', false)).toBe(true)

    const guarded = { ...FULL_NAV, pricing: { enabled: true, require_auth: true } }
    expect(isNavEntryVisible(guarded, 'pricing', true)).toBe(true)
    expect(isNavEntryVisible(guarded, 'pricing', false)).toBe(false)

    const disabled = { ...FULL_NAV, rankings: { enabled: false, require_auth: false } }
    expect(isNavEntryVisible(disabled, 'rankings', true)).toBe(false)
  })

  it('defaults missing console auth to true and other entries to false', () => {
    expect(navEntryOf(undefined, 'console')).toEqual({ enabled: true, require_auth: true })
    expect(navEntryOf(undefined, 'about')).toEqual({ enabled: true, require_auth: false })
  })

  it('requires a non-empty docs URL in addition to the switch', () => {
    expect(isNavEntryVisible(FULL_NAV, 'docs', false, 'https://docs.example.com')).toBe(true)
    expect(isNavEntryVisible(FULL_NAV, 'docs', false, '')).toBe(false)
    expect(isNavEntryVisible(FULL_NAV, 'docs', false, '   ')).toBe(false)
  })

  it('rejects unsafe docs targets instead of rendering them as links', () => {
    for (const unsafe of [
      'javascript:alert(1)',
      'data:text/html,hi',
      '//evil.example',
      '/\\evil.example',
      'ftp://example.com',
    ]) {
      expect(isSafePublicHref(unsafe)).toBe(false)
      expect(isNavEntryVisible(FULL_NAV, 'docs', true, unsafe)).toBe(false)
    }
    expect(isSafePublicHref('/guide')).toBe(true)
    expect(isSafePublicHref('https://docs.example.com')).toBe(true)
    expect(visibleNavItems(FULL_NAV, true, 'javascript:alert(1)').some((i) => i.key === 'docs')).toBe(false)
  })

  it('pins the documented nav order and drops a disabled entry in place', () => {
    expect(
      visibleNavItems(FULL_NAV, true, 'https://docs.example.com').map((item) => item.key),
    ).toEqual(['console', 'models', 'pricing', 'rankings', 'about', 'docs'])

    const withoutPricing = {
      ...FULL_NAV,
      pricing: { enabled: false, require_auth: false },
    }
    expect(
      visibleNavItems(withoutPricing, true, 'https://docs.example.com').map(
        (item) => item.key,
      ),
    ).toEqual(['console', 'models', 'rankings', 'about', 'docs'])
  })

  it('keeps render order and marks absolute docs targets external', () => {
    const items = visibleNavItems(
      { ...FULL_NAV, rankings: { enabled: false, require_auth: false } },
      true,
      'https://docs.example.com',
    )
    expect(items.map((item) => item.key)).toEqual([
      'console',
      'models',
      'pricing',
      'about',
      'docs',
    ])
    expect(items.find((item) => item.key === 'docs')?.external).toBe(true)

    const relative = visibleNavItems(FULL_NAV, true, '/guide')
    expect(relative.find((item) => item.key === 'docs')?.external).toBe(false)
  })

  it('leaves no slot when every entry is disabled', () => {
    const off = {
      home: { enabled: false, require_auth: false },
      console: { enabled: false, require_auth: true },
      models: { enabled: false, require_auth: false },
      pricing: { enabled: false, require_auth: false },
      rankings: { enabled: false, require_auth: false },
      docs: { enabled: false, require_auth: false },
      about: { enabled: false, require_auth: false },
    }
    expect(visibleNavItems(off, true, 'https://docs.example.com')).toEqual([])
  })
})

describe('direct-link guard (F-09-35)', () => {
  it('encodes the full pathname and search into the sign-in hand-off', () => {
    expect(signInHref('/dashboard/usage-logs', '?page=2')).toBe(
      '/sign-in?redirect=%2Fdashboard%2Fusage-logs%3Fpage%3D2',
    )
  })

  it('silently sends disabled public sections back home', () => {
    const nav = { ...FULL_NAV, pricing: { enabled: false, require_auth: false } }
    expect(publicRouteGuardRedirect('/pricing', '', nav, true)).toBe('/')
  })

  it('redirects guests to sign-in for require_auth sections', () => {
    const nav = { ...FULL_NAV, about: { enabled: true, require_auth: true } }
    expect(publicRouteGuardRedirect('/about', '?tab=oss', nav, false)).toBe(
      '/sign-in?redirect=%2Fabout%3Ftab%3Doss',
    )
    expect(publicRouteGuardRedirect('/about', '?tab=oss', nav, true)).toBeNull()
  })

  it('does not guard unknown public paths', () => {
    expect(publicRouteGuardRedirect('/legal/terms', '', FULL_NAV, false)).toBeNull()
  })

  it('silently sends a disabled model library back home', () => {
    const nav = { ...FULL_NAV, models: { enabled: false, require_auth: false } }
    expect(publicRouteGuardRedirect('/models', '', nav, true)).toBe('/')
  })

  it('redirects guests to sign-in when the model library requires auth', () => {
    const nav = { ...FULL_NAV, models: { enabled: true, require_auth: true } }
    expect(publicRouteGuardRedirect('/models', '?cap=vision', nav, false)).toBe(
      '/sign-in?redirect=%2Fmodels%3Fcap%3Dvision',
    )
    expect(publicRouteGuardRedirect('/models', '?cap=vision', nav, true)).toBeNull()
  })
})
