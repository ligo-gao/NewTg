import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { PermissionMatrix } from './PermissionMatrix'

function Harness() {
  const [value, setValue] = useState<string[]>([])
  return (
    <>
      <output data-testid="value">{JSON.stringify(value)}</output>
      <PermissionMatrix value={value} onChange={setValue} />
    </>
  )
}

describe('PermissionMatrix', () => {
  it('renders the frozen 20 keys in 8 domain groups plus the full-access toggle', async () => {
    const { getAllByRole, getByRole } = await renderWithProviders(<Harness />, [
      'common',
      'admin',
    ])

    const groups = getAllByRole('group')
    expect(groups).toHaveLength(8)

    const checkboxes = getAllByRole('checkbox')
    expect(checkboxes).toHaveLength(21)
    expect(getByRole('checkbox', { name: /Full access/ })).toBeTruthy()

    for (const name of [
      /View users/,
      /Create users/,
      /Edit users/,
      /Disable users/,
      /Delete users/,
      /Adjust quota/,
      /Manage roles/,
      /Reset security/,
      /Manage groups/,
      /View channels/,
      /Manage channels/,
      /Manage sensitive channel operations/,
      /View pricing/,
      /Manage pricing/,
      /Manage redemption codes/,
      /View orders/,
      /Manage orders/,
      /View logs/,
      /View prompt text/,
      /Manage settings/,
    ]) {
      expect(getByRole('checkbox', { name })).toBeTruthy()
    }
  })

  it('gates the matrix behind the "*" toggle and restores the previous selection', async () => {
    const { getAllByRole, getByRole, getByTestId } = await renderWithProviders(
      <Harness />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    const viewUsers = getByRole('checkbox', { name: /View users/ })
    await user.click(viewUsers)
    expect(getByTestId('value').textContent).toBe('["users:read"]')

    const fullAccess = getByRole('checkbox', { name: /Full access/ })
    await user.click(fullAccess)
    expect(getByTestId('value').textContent).toBe('["*"]')

    for (const checkbox of getAllByRole('checkbox')) {
      if (checkbox !== fullAccess) {
        expect((checkbox as HTMLInputElement).disabled).toBe(true)
      }
    }

    await user.click(fullAccess)
    expect(getByTestId('value').textContent).toBe('["users:read"]')
    expect((viewUsers as HTMLInputElement).checked).toBe(true)
    expect((getByRole('checkbox', { name: /View logs/ }) as HTMLInputElement).disabled).toBe(
      false,
    )
  })
})
