import userEvent from '@testing-library/user-event'
import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { BalanceCard } from './balance-card'

describe('BalanceCard', () => {
  it('switches between integer credits and yuan (100 credits = 1 yuan)', async () => {
    await renderWithProviders(<BalanceCard balance={123450} />, [
      'common',
      'account',
    ])
    const user = userEvent.setup()

    expect(screen.getByText('123,450')).toBeTruthy()

    await user.click(screen.getByRole('radio', { name: 'Yuan' }))
    expect(screen.getByText('¥1,234.50')).toBeTruthy()

    await user.click(screen.getByRole('radio', { name: 'Credits' }))
    expect(screen.getByText('123,450')).toBeTruthy()
  })

  it('renders an em dash while the balance is not known yet', async () => {
    await renderWithProviders(<BalanceCard balance={null} />, [
      'common',
      'account',
    ])

    expect(screen.getByTestId('balance-value').textContent).toBe('—')
  })
})
