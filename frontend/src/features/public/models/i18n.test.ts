import { describe, expect, it } from 'vitest'

import enPublic from '@/locales/en/public.json'
import { SUPPORTED_LOCALES, loadDictionary, translate } from '@/app/providers/i18n'

/**
 * `/models` 的文案以 `en` 为基线：七个随包语言必须逐键提供非空翻译，
 * 否则运行时会静默回退英文（web-00 §7.2）。键名本身即缺键信号。
 */
describe('model library copy', () => {
  it('defines every public.models key in all seven locales', async () => {
    const keys = Object.keys(enPublic).filter((key) => key.startsWith('models.'))
    expect(keys.length).toBeGreaterThan(40)
    for (const locale of SUPPORTED_LOCALES) {
      await loadDictionary(locale, 'public')
      const missing = keys.filter(
        (key) => translate(locale, `public.${key}`) === `public.${key}`,
      )
      expect(missing).toEqual([])
    }
  })
})
