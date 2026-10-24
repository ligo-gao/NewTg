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

/**
 * One leaf per key with its message, so a parity check can compare both the
 * key set and the body of each translation.
 */
function messages(value: unknown, prefix = '', out = new Map<string, string>()): Map<string, string> {
  if (typeof value === 'object' && value !== null && !Array.isArray(value)) {
    for (const [key, child] of Object.entries(value)) {
      messages(child, prefix === '' ? key : `${prefix}.${key}`, out)
    }
    return out
  }
  out.set(prefix, typeof value === 'string' ? value : JSON.stringify(value))
  return out
}

/** Multiset of `{placeholder}` names, sorted so order is not significant. */
function placeholders(message: string): string[] {
  return [...message.matchAll(/\{([a-zA-Z0-9_]+)\}/g)]
    .map((match) => match[1] ?? '')
    .sort()
}

function localesByDomain(): Map<string, Map<string, Map<string, string>>> {
  const byLocale = new Map<string, Map<string, Map<string, string>>>()
  for (const [path, module] of Object.entries(MODULES)) {
    const match = /\.\/([^/]+)\/([^/]+)\.json$/.exec(path)
    if (match === null) continue
    const locale = match[1] ?? ''
    const domain = match[2] ?? ''
    const data = (module as { default?: unknown }).default ?? module
    const domains = byLocale.get(locale) ?? new Map<string, Map<string, string>>()
    domains.set(domain, messages(data))
    byLocale.set(locale, domains)
  }
  return byLocale
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

  it('every locale keeps the same {placeholder} tokens as en in every leaf', () => {
    const byLocale = localesByDomain()
    const en = byLocale.get('en')
    expect(en).toBeDefined()
    for (const [locale, domains] of byLocale) {
      if (locale === 'en') continue
      for (const [domain, localized] of domains) {
        const reference = en?.get(domain)
        expect(reference).toBeDefined()
        for (const [key, message] of localized) {
          const english = reference?.get(key)
          // A missing key is the key-parity test's job; skip it here so this
          // assertion reports only placeholder drift.
          if (english === undefined) continue
          // A translation that drops `{value}` leaves the UI printing a
          // literal placeholder-less sentence; compare the multiset of names,
          // not just the presence of the key.
          expect({ locale, domain, key, tokens: placeholders(message) }).toEqual({
            locale,
            domain,
            key,
            tokens: placeholders(english),
          })
        }
      }
    }
  })
})
