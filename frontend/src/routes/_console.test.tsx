import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { ConsoleHeader } from './_console'

describe('console layout i18n', () => {
  it('renders resolved common.nav keys, never raw dictionary keys', async () => {
    const { getByRole, getByText, queryByText } = await renderWithProviders(
      <ConsoleHeader />,
      ['common'],
    )

    const nav = getByRole('navigation')
    expect(nav.textContent).toContain('Dashboard')
    expect(nav.textContent).toContain('Profile')
    expect(nav.textContent).toContain('Security')
    expect(nav.textContent).toContain('Users')
    expect(nav.textContent).toContain('Groups')
    expect(getByText('NewTG')).toBeTruthy()
    expect(queryByText(/common\./)).toBeNull()
  })
})
