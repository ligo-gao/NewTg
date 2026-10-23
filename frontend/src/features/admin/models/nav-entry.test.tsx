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

describe('models console entry', () => {
  it('renders the entry for an admin holding channels:read', async () => {
    const { container } = await renderSidebar('admin', ['channels:read'])
    expect(container.querySelector('a[href="/admin/models"]')).toBeTruthy()
  })

  it('hides the entry from admins without channels:read', async () => {
    const { container } = await renderSidebar('admin', ['channels:manage'])
    expect(container.querySelector('a[href="/admin/models"]')).toBeNull()
  })

  it('hides the entry from non-admin sessions', async () => {
    const { container } = await renderSidebar('user', ['channels:read'])
    expect(container.querySelector('a[href="/admin/models"]')).toBeNull()
  })
})
