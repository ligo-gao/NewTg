import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  clearLanguageChoice,
  clearPendingSync,
  ensureLanguageSchema,
  hasExplicitChoice,
  hasPendingSync,
  markLanguageChosen,
  readStoredLocale,
  writeStoredLocale,
} from './language-preference'

beforeEach(() => globalThis.localStorage.clear())

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
    expect(readStoredLocale()).toBeNull()
    expect(hasPendingSync()).toBe(false)
    expect(() => markLanguageChosen('en')).not.toThrow()
    spy.mockRestore()
  })
})
