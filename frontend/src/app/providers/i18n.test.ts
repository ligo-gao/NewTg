import { beforeEach, describe, expect, it, vi } from 'vitest'

type I18nModule = typeof import('./i18n')

async function freshI18n(): Promise<I18nModule> {
  // The dictionary cache is module-level state; reset the module registry so
  // each test can observe the "nothing imported yet" starting point.
  return import('./i18n')
}

beforeEach(() => {
  vi.resetModules()
  vi.restoreAllMocks()
})

describe('lazy dictionary loading', () => {
  it('derives SUPPORTED_LOCALES from the loader map', async () => {
    const { SUPPORTED_LOCALES } = await freshI18n()
    expect(SUPPORTED_LOCALES).toEqual(['en'])
  })

  it('does not resolve keys until their domain is imported', async () => {
    const { loadDictionary, translate } = await freshI18n()

    expect(translate('en', 'common.app.title')).toBe('common.app.title')

    await loadDictionary('en', 'common')
    expect(translate('en', 'common.app.title')).toBe('NewTG')
  })

  it('loads domains independently', async () => {
    const { loadDictionary, translate } = await freshI18n()

    await loadDictionary('en', 'common')
    expect(translate('en', 'public.home.tagline')).toBe('public.home.tagline')

    await loadDictionary('en', 'public')
    expect(translate('en', 'public.home.tagline')).toBe(
      'One gateway for every model',
    )
  })

  it('shares one import between concurrent callers and caches the result', async () => {
    const { loadDictionary } = await freshI18n()

    const [first, second] = await Promise.all([
      loadDictionary('en', 'common'),
      loadDictionary('en', 'common'),
    ])

    expect(second).toBe(first)
    expect(await loadDictionary('en', 'common')).toBe(first)
  })

  it('interpolates values once the dictionary lands', async () => {
    const { loadDictionary, translate } = await freshI18n()
    await loadDictionary('en', 'common')

    expect(
      translate('en', 'common.toast.request_id', { requestId: 'req-42' }),
    ).toBe('request_id: req-42')
  })
})

describe('missing keys (web-00 §7.2)', () => {
  it('warns and renders the key name when a loaded domain lacks the key', async () => {
    const { loadDictionary, translate } = await freshI18n()
    await loadDictionary('en', 'common')
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})

    expect(translate('en', 'common.no.such.key')).toBe('common.no.such.key')
    expect(warn).toHaveBeenCalledWith(
      '[i18n] missing key: common.no.such.key (en)',
    )
  })

  it('stays silent while the domain chunk is still loading', async () => {
    const { translate } = await freshI18n()
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})

    expect(translate('en', 'public.home.tagline')).toBe(
      'public.home.tagline',
    )
    expect(warn).not.toHaveBeenCalled()
  })
})
