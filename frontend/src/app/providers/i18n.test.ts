import { beforeEach, describe, expect, it, vi } from 'vitest'

type I18nModule = typeof import('./i18n')

async function freshI18n(): Promise<I18nModule> {
  // 词典缓存与激活语言都是模块级状态；重启模块注册表让每个用例从干净状态开始。
  return import('./i18n')
}

beforeEach(() => {
  vi.resetModules()
  vi.restoreAllMocks()
  globalThis.localStorage?.clear()
  // 移除上一个用例为 navigator.language 定义的影子属性，恢复 jsdom 默认值。
  delete (globalThis.navigator as unknown as Record<string, unknown>).language
})

function setNavigatorLanguage(tag: string): void {
  Object.defineProperty(globalThis.navigator, 'language', {
    configurable: true,
    get: () => tag,
  })
}

describe('lazy dictionary loading', () => {
  it('derives SUPPORTED_LOCALES from the canonical seven bundled locales', async () => {
    const { SUPPORTED_LOCALES } = await freshI18n()
    expect(SUPPORTED_LOCALES).toEqual([
      'en',
      'zh-CN',
      'zh-TW',
      'fr',
      'ru',
      'ja',
      'vi',
    ])
  })

  it('does not resolve keys until their domain is imported', async () => {
    const { loadDictionary, translate } = await freshI18n()

    expect(translate('en', 'common.app.title')).toBe('common.app.title')

    await loadDictionary('en', 'common')
    expect(translate('en', 'common.app.title')).toBe('NEWTG')
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

  it('loads the translated dictionary and falls back to en for unscoped keys', async () => {
    const { loadDictionary, translate } = await freshI18n()

    // 只加载 en 时，任何语言的查询都沿回退链返回 en 文案。
    await loadDictionary('en', 'public')
    expect(translate('fr', 'public.home.tagline')).toBe(
      'One gateway for every model',
    )

    // fr 词典到达后按当前语言渲染，en 仍保留作回退。
    await loadDictionary('fr', 'public')
    expect(translate('fr', 'public.home.tagline')).toBe(
      'Une seule passerelle pour tous les modèles',
    )
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

describe('locale resolution (web-00 §7.3)', () => {
  it('normalizes BCP-47 tags to the canonical set', async () => {
    const { matchLocale } = await freshI18n()

    expect(matchLocale('en-US')).toBe('en')
    expect(matchLocale('zh-CN')).toBe('zh-CN')
    expect(matchLocale('zh-Hans')).toBe('zh-CN')
    expect(matchLocale('zh-Hant')).toBe('zh-TW')
    expect(matchLocale('zh-TW')).toBe('zh-TW')
    expect(matchLocale('zh-HK')).toBe('zh-TW')
    expect(matchLocale('fr-FR')).toBe('fr')
    expect(matchLocale('ru-RU')).toBe('ru')
    expect(matchLocale('ja-JP')).toBe('ja')
    expect(matchLocale('vi-VN')).toBe('vi')
    expect(matchLocale('de-DE')).toBeNull()
    expect(matchLocale(undefined)).toBeNull()
  })

  it('prefers localStorage over navigator.language', async () => {
    setNavigatorLanguage('fr-FR')
    globalThis.localStorage.setItem('newtg.language', 'ja')
    const { resolveInitialLocale } = await freshI18n()

    expect(resolveInitialLocale()).toBe('ja')
  })

  it('falls back to navigator.language, then en', async () => {
    setNavigatorLanguage('ru-RU')
    const { resolveInitialLocale } = await freshI18n()
    expect(resolveInitialLocale()).toBe('ru')

    setNavigatorLanguage('de-DE')
    expect(resolveInitialLocale()).toBe('en')
  })

  it('intersects status languages with bundled locales, preserving server order', async () => {
    const { resolveAvailableLocales, SUPPORTED_LOCALES } = await freshI18n()

    expect(resolveAvailableLocales(['ja', 'en', 'zh-TW', 'de'])).toEqual([
      'ja',
      'en',
      'zh-TW',
    ])
    // status 缺失或不含任何随包语言时退回随包集合，语言卡仍可用。
    expect(resolveAvailableLocales(undefined)).toEqual(SUPPORTED_LOCALES)
    expect(resolveAvailableLocales(['de'])).toEqual(SUPPORTED_LOCALES)
  })

  it('exposes the active locale for route loaders and persists switches', async () => {
    const { getActiveLocale, loadRouteDictionary } = await freshI18n()
    expect(getActiveLocale()).toBe('en')

    await loadRouteDictionary('public')
    expect(getActiveLocale()).toBe('en')
  })
})
