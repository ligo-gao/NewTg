/// <reference types="vite/client" />

import { describe, expect, it } from 'vitest'

const MODULES = import.meta.glob('./*/{account,admin,auth,common,logs,public,settings}.json', {
  eager: true,
})

function leaves(value: unknown, prefix = ''): string[] {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return [prefix]
  return Object.entries(value).flatMap(([key, child]) =>
    leaves(child, prefix === '' ? key : `${prefix}.${key}`),
  )
}

describe('locale parity', () => {
  it('every locale carries the same keys as en in every domain', () => {
    const byLocale = new Map<string, Map<string, string[]>>()
    for (const [path, module] of Object.entries(MODULES)) {
      const match = /\.\/([^/]+)\/([^/]+)\.json$/.exec(path)
      if (match === null) continue
      const locale = match[1] ?? ''
      const domain = match[2] ?? ''
      const data = (module as { default?: unknown }).default ?? module
      const domains = byLocale.get(locale) ?? new Map<string, string[]>()
      domains.set(domain, leaves(data).sort())
      byLocale.set(locale, domains)
    }
    const en = byLocale.get('en')
    expect(en).toBeDefined()
    const expected = [...(en ?? new Map<string, string[]>()).entries()]
    for (const [locale, domains] of byLocale) {
      if (locale === 'en') continue
      expect({ locale, domains: [...domains.entries()] }).toEqual({ locale, domains: expected })
    }
  })
})
