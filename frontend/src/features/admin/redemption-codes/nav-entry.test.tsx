import { describe, expect, it } from 'vitest'

import { AppSidebar } from '@/features/shell/app-sidebar'
import type { Me } from '@/lib/session'
import { renderWithProviders } from '@/test-support/providers'

function renderSidebar(role: 'admin' | 'user', permissions: string[]) {
  const me = { role, username: 'alice', permissions } as unknown as Me
  return renderWithProviders(
    <AppSidebar
      me={me}
      pathname="/dashboard"
      collapsed={false}
      onToggleCollapsed={() => {}}
      playgroundEnabled
    />,
    ['common'],
  )
}

describe('redemption codes console entry', () => {
  it('renders the entry for an admin holding redemption:manage', async () => {
    const { container } = await renderSidebar('admin', ['redemption:manage'])
    expect(container.querySelector('a[href="/redemption-codes"]')).toBeTruthy()
  })

  it('hides the entry from admins without redemption:manage', async () => {
    const { container } = await renderSidebar('admin', [])
    expect(container.querySelector('a[href="/redemption-codes"]')).toBeNull()
  })

  it('hides the entry from non-admin sessions', async () => {
    const { container } = await renderSidebar('user', ['redemption:manage'])
    expect(container.querySelector('a[href="/redemption-codes"]')).toBeNull()
  })
})
