import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { LanguageSwitch } from './language-switch'

describe('LanguageSwitch', () => {
  it('lists every bundled language and reports the chosen one', async () => {
    const onChange = vi.fn()

    const { getByLabelText } = await renderWithProviders(
      <LanguageSwitch label="Interface language" onChange={onChange} />,
    )
    const select = getByLabelText('Interface language') as HTMLSelectElement

    expect(select.tagName).toBe('SELECT')
    expect(select.value).toBe('en')
    expect(Array.from(select.options, (option) => option.value)).toEqual([
      'en',
      'zh-CN',
      'zh-TW',
      'fr',
      'ru',
      'ja',
      'vi',
    ])

    // Endonyms only: the locale code stays in `value`, never in the visible
    // label (WS2 language-picker contract).
    for (const option of Array.from(select.options)) {
      expect(option.textContent).not.toMatch(/\([a-z-]+\)$/i)
    }
    expect(select.options[0]?.textContent).toBe('English')

    const user = userEvent.setup()
    await user.selectOptions(select, 'fr')

    expect(onChange).toHaveBeenCalledWith('fr')
    expect(document.documentElement.lang).toBe('fr')
  })

  it('keeps the label for assistive technology in the footer variant', async () => {
    const { getByLabelText } = await renderWithProviders(
      <LanguageSwitch label="Interface language" variant="footer" />,
    )

    expect(getByLabelText('Interface language').tagName).toBe('SELECT')
  })
})
