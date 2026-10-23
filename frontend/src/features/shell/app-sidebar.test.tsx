import { describe, expect, it, vi } from 'vitest'

import type { DictionaryModule } from '@/app/providers/i18n'
import type { Me } from '@/lib/session'
import { renderWithProviders } from '@/test-support/providers'

import { AppSidebar } from './app-sidebar'

const FULL_ADMIN = {
  role: 'admin',
  username: 'root',
  permissions: ['*'],
} as unknown as Me
const USER = { role: 'user', username: 'alice', permissions: [] } as unknown as Me
const LIMITED_ADMIN = {
  role: 'admin',
  username: 'ops',
  permissions: ['users:read'],
} as unknown as Me

async function renderSidebar(
  me: Me,
  pathname = '/tokens',
  collapsed = false,
  modules: readonly DictionaryModule[] = ['common'],
) {
  const onToggleCollapsed = vi.fn()
  const result = await renderWithProviders(
    <AppSidebar
      me={me}
      pathname={pathname}
      collapsed={collapsed}
      onToggleCollapsed={onToggleCollapsed}
      playgroundEnabled
    />,
    modules,
  )
  return { ...result, onToggleCollapsed }
}

describe('AppSidebar', () => {
  it('renders every group and admin entry for a full admin', async () => {
    const { container } = await renderSidebar(FULL_ADMIN)

    expect(container.querySelector('a[href="/tokens"]')).toBeTruthy()
    expect(container.querySelector('a[href="/admin/users"]')).toBeTruthy()
    expect(container.querySelector('a[href="/admin/settings"]')).toBeTruthy()
    expect(container.querySelector('a[href="/about"]')).toBeTruthy()
    expect(container.textContent).toContain('Workspace')
    expect(container.textContent).toContain('Management')
    expect(container.textContent).not.toMatch(/common\./)
  })

  it('hides the admin and system groups from an ordinary user', async () => {
    const { container } = await renderSidebar(USER)

    expect(container.querySelector('a[href="/tokens"]')).toBeTruthy()
    expect(container.querySelector('a[href="/admin/users"]')).toBeNull()
    expect(container.querySelector('a[href="/admin/settings"]')).toBeNull()
    expect(container.textContent).not.toContain('Management')
  })

  it('honors fine-grained permissions for an admin', async () => {
    const { container } = await renderSidebar(LIMITED_ADMIN)

    expect(container.querySelector('a[href="/admin/users"]')).toBeTruthy()
    expect(container.querySelector('a[href="/admin/channels"]')).toBeNull()
    expect(container.querySelector('a[href="/admin/models"]')).toBeNull()
    expect(container.querySelector('a[href="/admin/users/groups"]')).toBeNull()
  })

  it('marks the longest matching route as current', async () => {
    const { container } = await renderSidebar(FULL_ADMIN, '/dashboard/usage-logs')

    expect(
      container
        .querySelector('a[href="/dashboard/usage-logs"]')
        ?.getAttribute('aria-current'),
    ).toBe('page')
    expect(
      container
        .querySelector('a[href="/dashboard"]')
        ?.hasAttribute('aria-current'),
    ).toBe(false)
  })

  it('keeps icons and calls the collapse callback', async () => {
    const { container, getByTestId, onToggleCollapsed } = await renderSidebar(
      FULL_ADMIN,
      '/tokens',
      true,
    )

    expect(container.querySelector('a[href="/tokens"]')).toBeTruthy()
    expect(container.textContent).not.toContain('API')
    getByTestId('sidebar-collapse').click()
    expect(onToggleCollapsed).toHaveBeenCalledTimes(1)
  })

  it('drills into the settings sections instead of the root nav', async () => {
    const { container, getByTestId } = await renderSidebar(
      FULL_ADMIN,
      '/admin/settings',
      false,
      ['common', 'settings'],
    )

    const back = container.querySelector('a[href="/dashboard"]')
    expect(back).toBeTruthy()
    expect(back?.textContent).toContain('Back to console')
    expect(container.querySelector('[data-testid="settings-nav-slot"]')).toBeTruthy()
    expect(
      container
        .querySelector('[data-testid="settings-nav-slot"]')
        ?.getAttribute('data-collapsed'),
    ).toBe('false')
    expect(container.querySelector('a[href="/tokens"]')).toBeNull()
    expect(container.textContent).not.toContain('Management')
    // The footer collapse control survives the drill-in.
    expect(getByTestId('sidebar-collapse')).toBeTruthy()
  })

  it('marks the settings slot collapsed so the drill-in can hide its text', async () => {
    const { container } = await renderSidebar(
      FULL_ADMIN,
      '/admin/settings',
      true,
      ['common', 'settings'],
    )

    const slot = container.querySelector('[data-testid="settings-nav-slot"]')
    expect(slot?.getAttribute('data-collapsed')).toBe('true')
    expect(slot?.className).toContain('group')
    expect(slot?.className).toContain('overflow-x-hidden')
    expect(container.querySelector('a[href="/tokens"]')).toBeNull()
  })

  it('treats nested settings routes as settings mode', async () => {
    const { container } = await renderSidebar(
      FULL_ADMIN,
      '/admin/settings/payment',
      false,
      ['common', 'settings'],
    )

    expect(container.querySelector('[data-testid="settings-nav-slot"]')).toBeTruthy()
    expect(container.querySelector('a[href="/tokens"]')).toBeNull()
  })

  it('keeps the root navigation on a non-settings route', async () => {
    const { container } = await renderSidebar(FULL_ADMIN, '/admin/users')

    expect(container.querySelector('a[href="/tokens"]')).toBeTruthy()
    expect(container.querySelector('[data-testid="settings-nav-slot"]')).toBeNull()
    expect(container.querySelector('a[href="/dashboard"]')).toBeTruthy()
  })
})
