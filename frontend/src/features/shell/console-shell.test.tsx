import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { ConsoleWideScreenGate } from './console-shell'

describe('ConsoleWideScreenGate', () => {
  it('explains the wide-screen requirement', async () => {
    const { getByRole, getByText } = await renderWithProviders(
      <ConsoleWideScreenGate />,
      ['common'],
    )

    expect(getByRole('heading', { level: 1 }).textContent).toBe(
      'Use a wide screen',
    )
    expect(
      getByText(/designed for wide screens/i),
    ).toBeTruthy()
  })
})
