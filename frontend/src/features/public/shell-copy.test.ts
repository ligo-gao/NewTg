import { describe, expect, it } from 'vitest'

import {
  COPY_BY_LOCALE,
  SHELL_LOCALES,
  formatShellCopy,
  getShellCopy,
  isShellLocale,
  type ShellCopyKey,
} from '@newtg/public-shell'

/**
 * The shell dictionary (`packages/public-shell/src/copy`) is the only copy the
 * header and footer render, in both apps. A locale that has drifted (missing
 * key, empty string, a dropped `{site}` placeholder) would ship a literal key
 * or a wrong sentence on a public page, so parity is a hard test rather than a
 * review convention.
 */
describe('shell copy', () => {
  const reference = COPY_BY_LOCALE.en
  const referenceKeys = Object.keys(reference).sort()

  it('covers every bundled locale', () => {
    expect(Object.keys(COPY_BY_LOCALE).sort()).toEqual([...SHELL_LOCALES].sort())
  })

  for (const locale of SHELL_LOCALES) {
    it(`${locale} carries the same keys as en`, () => {
      expect(Object.keys(COPY_BY_LOCALE[locale]).sort()).toEqual(referenceKeys)
    })

    it(`${locale} has no empty or untranslated placeholder values`, () => {
      for (const [key, value] of Object.entries(COPY_BY_LOCALE[locale])) {
        expect(value.trim(), `${locale} ${key}`).not.toBe('')
        expect(value, `${locale} ${key}`).not.toBe(key)
      }
    })

    it(`${locale} keeps the template placeholders of en`, () => {
      for (const [key, value] of Object.entries(reference)) {
        const expected = (value.match(/\{\w+\}/g) ?? []).sort()
        const actual = (
          COPY_BY_LOCALE[locale][key as ShellCopyKey].match(/\{\w+\}/g) ?? []
        ).sort()
        expect(actual, `${locale} ${key}`).toEqual(expected)
      }
    })
  }

  it('falls back to English for an unknown locale', () => {
    expect(getShellCopy('de')).toBe(COPY_BY_LOCALE.en)
    expect(getShellCopy('')).toBe(COPY_BY_LOCALE.en)
  })

  it('returns the requested locale verbatim', () => {
    expect(getShellCopy('ja')).toBe(COPY_BY_LOCALE.ja)
  })

  it('recognises only the bundled locale identifiers', () => {
    expect(isShellLocale('zh-CN')).toBe(true)
    expect(isShellLocale('zh')).toBe(false)
  })

  it('interpolates the copyright template and leaves unknown names alone', () => {
    expect(formatShellCopy('© {year} {site}', { year: 2026, site: 'NEWTG' })).toBe(
      '© 2026 NEWTG',
    )
    expect(formatShellCopy('{site} home', {})).toBe('{site} home')
  })
})
