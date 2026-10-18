import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { ConsoleHeader } from './_console'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function adminRow(permissions: string[] | null) {
  return {
    id: 1,
    username: 'root',
    display_name: null,
    email: null,
    role: 'admin',
    status: 'enabled',
    balance: 0,
    group: 'default',
    admin_permissions: permissions,
    totp_enabled: false,
    must_change_password: false,
    token_count: 0,
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
  }
}

/**
 * 管理员会话 + 调用者自己的权限行（能力探测的第二跳）。价格导航的可见性
 * 依赖这一行，所以除 me 之外还必须声明 /api/users。
 */
function installAdminActor({
  permissions = null,
  usersStatus,
  recordActor,
}: {
  permissions?: string[] | null
  usersStatus?: number
  recordActor?: () => void
} = {}) {
  server.use(
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({ username: 'root', role: 'admin', status: 'enabled' }),
    ),
    http.get(`${ORIGIN}/api/users`, () => {
      recordActor?.()
      if (usersStatus !== undefined) {
        return HttpResponse.json(
          { type: 'about:blank', title: 'Forbidden' },
          { status: usersStatus },
        )
      }
      return HttpResponse.json({
        items: [adminRow(permissions)],
        total: 1,
        page: 1,
        page_size: 20,
      })
    }),
  )
}

describe('console layout i18n', () => {
  it('renders resolved nav keys, admin links and the pricing section for a full admin', async () => {
    installAdminActor()

    const { getByRole, getByText, queryByText } = await renderWithProviders(
      <ConsoleHeader />,
      ['common', 'admin'],
    )

    const nav = getByRole('navigation')
    await waitFor(() => {
      expect(nav.textContent).toContain('Users')
      expect(nav.textContent).toContain('Groups')
      // Pricing 需要 /api/users 的能力行返回后才出现。
      expect(nav.textContent).toContain('Pricing')
      expect(nav.textContent).toContain('Model pricing')
      expect(nav.textContent).toContain('Group pricing')
    })
    expect(nav.textContent).toContain('Dashboard')
    expect(nav.textContent).toContain('Profile')
    expect(nav.textContent).toContain('Security')
    expect(getByText('NewTG')).toBeTruthy()
    expect(queryByText(/common\./)).toBeNull()
    expect(queryByText(/admin\./)).toBeNull()
  })

  it('hides the admin section from a non-admin session', async () => {
    let meCalls = 0
    let usersCalls = 0
    server.use(
      http.get(`${ORIGIN}/api/me`, () => {
        meCalls += 1
        return HttpResponse.json({ username: 'alice', role: 'user', status: 'enabled' })
      }),
      http.get(`${ORIGIN}/api/users`, () => {
        usersCalls += 1
        return HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 })
      }),
    )

    const { getByRole, findByText } = await renderWithProviders(
      <ConsoleHeader />,
      ['common', 'admin'],
    )

    await findByText('Dashboard')
    await waitFor(() => {
      expect(meCalls).toBe(1)
    })
    const nav = getByRole('navigation')
    expect(nav.textContent).toContain('Dashboard')
    expect(nav.textContent).not.toContain('Users')
    expect(nav.textContent).not.toContain('Groups')
    expect(nav.textContent).not.toContain('Pricing')
    // 非管理员不应触达用户列表（能力探测在角色检查处短路）。
    expect(usersCalls).toBe(0)
  })

  it('hides pricing links when the actor lacks pricing:read', async () => {
    let actorLoaded = false
    installAdminActor({
      permissions: ['users:read'],
      recordActor: () => {
        actorLoaded = true
      },
    })

    const { getByRole } = await renderWithProviders(<ConsoleHeader />, [
      'common',
      'admin',
    ])

    const nav = getByRole('navigation')
    await waitFor(() => {
      expect(nav.textContent).toContain('Users')
      expect(actorLoaded).toBe(true)
    })
    expect(nav.textContent).not.toContain('Pricing')
    expect(nav.textContent).not.toContain('Model pricing')
    expect(nav.textContent).not.toContain('Group pricing')
  })

  it('fails closed when the actor row cannot be read', async () => {
    let actorLoaded = false
    installAdminActor({
      usersStatus: 403,
      recordActor: () => {
        actorLoaded = true
      },
    })

    const { getByRole } = await renderWithProviders(<ConsoleHeader />, [
      'common',
      'admin',
    ])

    const nav = getByRole('navigation')
    await waitFor(() => {
      expect(nav.textContent).toContain('Users')
      expect(actorLoaded).toBe(true)
    })
    expect(nav.textContent).not.toContain('Pricing')
  })

  it('marks the current pricing route with aria-current', async () => {
    installAdminActor({ permissions: ['pricing:read'] })

    const { findByRole } = await renderWithProviders(
      <ConsoleHeader activePath="/admin/pricing/model-pricing" />,
      ['common', 'admin'],
    )

    const modelLink = await findByRole('link', { name: 'Model pricing' })
    const groupLink = await findByRole('link', { name: 'Group pricing' })
    expect(modelLink.getAttribute('aria-current')).toBe('page')
    expect(groupLink.getAttribute('aria-current')).toBeNull()
  })
})
