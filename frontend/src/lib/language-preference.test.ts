import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import {
  LANGUAGE_EXPLICIT_KEY,
  LANGUAGE_PENDING_KEY,
  LANGUAGE_SCHEMA_KEY,
  LANGUAGE_SCHEMA_VERSION,
  LOCALE_STORAGE_KEY,
  clearLanguageChoice,
  clearPendingSync,
  ensureLanguageSchema,
  hasExplicitChoice,
  hasPendingSync,
  isLocale,
  markLanguageChosen,
  readStoredLocale,
  writeStoredLocale,
} from './language-preference'

beforeEach(() => globalThis.localStorage.clear())
afterEach(() => vi.restoreAllMocks())

describe('language preference store', () => {
  it('reads back a written locale and ignores unknown values', () => {
    writeStoredLocale('fr')
    expect(readStoredLocale()).toBe('fr')
    globalThis.localStorage.setItem('newtg.language', 'de')
    expect(readStoredLocale()).toBeNull()
  })

  it('marks a gesture as explicit and unsynced', () => {
    markLanguageChosen('ja')
    expect(readStoredLocale()).toBe('ja')
    expect(hasExplicitChoice()).toBe(true)
    expect(hasPendingSync()).toBe(true)
  })

  it('clears only the sync markers on sign-out', () => {
    markLanguageChosen('ja')
    clearLanguageChoice()
    expect(readStoredLocale()).toBe('ja')
    expect(hasExplicitChoice()).toBe(false)
    expect(hasPendingSync()).toBe(false)
  })

  it('clearPendingSync keeps the explicit marker', () => {
    markLanguageChosen('ja')
    clearPendingSync()
    expect(readStoredLocale()).toBe('ja')
    expect(hasExplicitChoice()).toBe(true)
    expect(hasPendingSync()).toBe(false)
  })

  it('stamps the schema without backfilling explicit', () => {
    globalThis.localStorage.setItem('newtg.language', 'en')
    ensureLanguageSchema()
    expect(globalThis.localStorage.getItem('newtg.language.schema')).toBe('2')
    expect(hasExplicitChoice()).toBe(false)
  })

  it('survives unavailable storage', () => {
    const spy = vi
      .spyOn(Storage.prototype, 'getItem')
      .mockImplementation(() => {
        throw new Error('blocked')
      })
    try {
      expect(readStoredLocale()).toBeNull()
      expect(hasPendingSync()).toBe(false)
      expect(() => markLanguageChosen('en')).not.toThrow()
    } finally {
      spy.mockRestore()
    }
  })
})

/**
 * Cross-app storage contract. The marketing site hardcodes the same four
 * literals in `website/src/lib/language-preference.ts`; nothing fails to
 * compile if one side renames a key or bumps the schema, and the symptom is
 * exactly the bug this feature fixes ("pick a language on the website, get
 * switched back on the console"). Pin the literal values, not the internals.
 */
describe('cross-app storage contract', () => {
  it('pins the storage keys and the schema version shared with website/', () => {
    expect(LOCALE_STORAGE_KEY).toBe('newtg.language')
    expect(LANGUAGE_PENDING_KEY).toBe('newtg.language.pending')
    expect(LANGUAGE_EXPLICIT_KEY).toBe('newtg.language.explicit')
    expect(LANGUAGE_SCHEMA_KEY).toBe('newtg.language.schema')
    expect(LANGUAGE_SCHEMA_VERSION).toBe('2')
  })

  it('accepts exactly the seven bundled locales', () => {
    for (const locale of ['en', 'zh-CN', 'zh-TW', 'fr', 'ru', 'ja', 'vi']) {
      expect(isLocale(locale)).toBe(true)
    }
    for (const other of ['de', 'en-US', 'zh-cn', '']) {
      expect(isLocale(other)).toBe(false)
    }
  })

  it('leaves a gesture readable by all three marker readers', () => {
    markLanguageChosen('vi')
    expect(globalThis.localStorage.getItem(LOCALE_STORAGE_KEY)).toBe('vi')
    expect(globalThis.localStorage.getItem(LANGUAGE_EXPLICIT_KEY)).toBe('1')
    expect(globalThis.localStorage.getItem(LANGUAGE_PENDING_KEY)).toBe('1')
    expect(readStoredLocale()).toBe('vi')
    expect(hasExplicitChoice()).toBe(true)
    expect(hasPendingSync()).toBe(true)
  })
})
