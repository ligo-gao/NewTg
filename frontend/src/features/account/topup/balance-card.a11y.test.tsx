import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { BalanceCard } from './balance-card'

import account from '@/locales/en/account.json'
import { I18nContextProvider } from '@/shared/lib/i18n-context'

const MESSAGES: Record<string, string> = {
  'account.topup.balance_title': account.topup.balance_title,
  'account.topup.balance_unit_label': account.topup.balance_unit_label,
  'account.topup.balance_credits': account.topup.balance_credits,
  'account.topup.balance_yuan': account.topup.balance_yuan,
}

function renderCard() {
  render(
    <I18nContextProvider
      value={{ locale: 'en', t: (key) => MESSAGES[key] ?? key }}
    >
      <BalanceCard balance={5000} />
    </I18nContextProvider>,
  )
}

describe('wallet credits/yuan toggle accessibility (UI-5)', () => {
  it('uses a radiogroup with one tab stop', () => {
    renderCard()
    expect(screen.getByRole('radiogroup', { name: 'Balance unit' })).toBeTruthy()
    expect(screen.getByRole('radio', { name: 'Credits' }).tabIndex).toBe(0)
    expect(screen.getByRole('radio', { name: 'Yuan' }).tabIndex).toBe(-1)
  })

  it('moves focus and switches unit with the arrow keys', () => {
    renderCard()
    const credits = screen.getByRole('radio', { name: 'Credits' })
    credits.focus()

    fireEvent.keyDown(credits, { key: 'ArrowRight' })
    const yuan = screen.getByRole('radio', { name: 'Yuan' })
    expect(document.activeElement).toBe(yuan)
    expect(yuan.getAttribute('aria-checked')).toBe('true')
    expect(screen.getByTestId('balance-value').textContent).toBe('¥50.00')
    expect(yuan.tabIndex).toBe(0)
  })
})
