import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import type { components } from '@/api/schema'
import { renderWithProviders } from '@/test-support/providers'

import { UserMutateSheet } from './UserMutateSheet'

type AdminUser = components['schemas']['AdminUser']

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function adminUser(overrides: Partial<AdminUser> = {}): AdminUser {
  return {
    id: 7,
    username: 'alice',
    display_name: null,
    email: null,
    role: 'user',
    status: 'enabled',
    balance: 1000,
    group: 'default',
    admin_permissions: null,
    totp_enabled: false,
    must_change_password: false,
    token_count: 0,
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
    ...overrides,
  }
}

/** The acting administrator is a FULL admin (admin_permissions = null). */
function installActorHandlers(actor: Partial<AdminUser> = {}) {
  server.use(
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({
        username: actor.username ?? 'root',
        role: 'admin',
        status: 'enabled',
      }),
    ),
    http.get(`${ORIGIN}/api/users`, () =>
      HttpResponse.json({
        items: [adminUser({ id: 1, username: 'root', role: 'admin', ...actor })],
        total: 1,
        page: 1,
        page_size: 20,
      }),
    ),
  )
}

async function renderSheet(user: AdminUser) {
  const onOpenChange = vi.fn()
  const onSaved = vi.fn()
  const result = await renderWithProviders(
    <UserMutateSheet
      open
      mode="edit"
      user={user}
      onOpenChange={onOpenChange}
      onSaved={onSaved}
    />,
    ['common', 'admin', 'auth'],
  )
  return { ...result, user: userEvent.setup(), onOpenChange, onSaved }
}

describe('UserMutateSheet edit diffing (I1)', () => {
  it('sends only the changed remark and omits role', async () => {
    installActorHandlers()
    const bodies: Record<string, unknown>[] = []
    let permissionCalls = 0

    server.use(
      http.patch(`${ORIGIN}/api/users/7`, async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>)
        return HttpResponse.json(adminUser({ remark: 'VIP' }))
      }),
      http.patch(`${ORIGIN}/api/users/7/permissions`, () => {
        permissionCalls += 1
        return HttpResponse.json(adminUser())
      }),
    )

    const { getByLabelText, getByRole, findByRole, user } = await renderSheet(
      adminUser(),
    )

    await findByRole('dialog')
    await user.type(getByLabelText('Remark'), 'VIP')
    await user.click(getByRole('button', { name: 'Save' }))

    await waitFor(() => {
      expect(bodies).toHaveLength(1)
    })
    expect(bodies[0]).toEqual({
      remark: 'VIP',
      updated_at: '2026-10-01T10:00:00Z',
    })
    expect(bodies[0]).not.toHaveProperty('role')
    expect(bodies[0]).not.toHaveProperty('display_name')
    expect(bodies[0]).not.toHaveProperty('group')
    expect(permissionCalls).toBe(0)
  })

  it('locks the role control when editing yourself (R-17 username match)', async () => {
    installActorHandlers({ username: 'alice', role: 'admin' })

    const { getByLabelText, getByText, findByRole } = await renderSheet(
      adminUser({ role: 'admin', admin_permissions: null }),
    )

    await findByRole('dialog')
    await waitFor(() => {
      // The actor row loads asynchronously; the self check is username-based
      // because MeView carries no id.
      expect((getByLabelText('Role') as HTMLSelectElement).disabled).toBe(true)
    })
    expect(getByText('You cannot change your own role')).toBeTruthy()
  })

  it('locks the role control when the actor lacks users:manage-roles', async () => {
    installActorHandlers({ admin_permissions: ['users:read', 'users:update'] })

    const { getByLabelText, getByText, findByRole } = await renderSheet(adminUser())

    await findByRole('dialog')
    await waitFor(() => {
      expect((getByLabelText('Role') as HTMLSelectElement).disabled).toBe(true)
    })
    expect(getByText('You do not have permission to manage roles')).toBeTruthy()
  })
})

describe('UserMutateSheet promotion defaults (I3)', () => {
  it('seeds the server default permissions and sends no permissions PATCH', async () => {
    installActorHandlers()
    const userBodies: Record<string, unknown>[] = []
    let permissionCalls = 0

    server.use(
      http.patch(`${ORIGIN}/api/users/7`, async ({ request }) => {
        userBodies.push((await request.json()) as Record<string, unknown>)
        return HttpResponse.json(adminUser({ role: 'admin' }))
      }),
      http.patch(`${ORIGIN}/api/users/7/permissions`, () => {
        permissionCalls += 1
        return HttpResponse.json(adminUser({ role: 'admin' }))
      }),
    )

    const { getByLabelText, getByRole, findByRole, user } = await renderSheet(
      adminUser(),
    )

    await findByRole('dialog')
    await user.selectOptions(getByLabelText('Role'), 'admin')

    // The matrix mirrors the server's promotion defaults instead of FULL.
    const viewUsers = getByLabelText(/View users/) as HTMLInputElement
    const viewLogs = getByLabelText(/View logs/) as HTMLInputElement
    expect(viewUsers.checked).toBe(true)
    expect(viewLogs.checked).toBe(true)
    expect((getByLabelText(/Full access/) as HTMLInputElement).checked).toBe(false)

    await user.click(getByRole('button', { name: 'Save' }))

    await waitFor(() => {
      expect(userBodies).toHaveLength(1)
    })
    expect(userBodies[0]?.role).toBe('admin')
    // Untouched defaults equal the server's promotion result: no PATCH.
    expect(permissionCalls).toBe(0)
  })

  it('skips the permissions PATCH when demoting to user', async () => {
    installActorHandlers()
    const userBodies: Record<string, unknown>[] = []
    let permissionCalls = 0

    server.use(
      http.patch(`${ORIGIN}/api/users/7`, async ({ request }) => {
        userBodies.push((await request.json()) as Record<string, unknown>)
        return HttpResponse.json(adminUser({ role: 'user' }))
      }),
      http.patch(`${ORIGIN}/api/users/7/permissions`, () => {
        permissionCalls += 1
        return HttpResponse.json(adminUser())
      }),
    )

    const { getByLabelText, getByRole, findByRole, user } = await renderSheet(
      adminUser({ role: 'admin', admin_permissions: ['users:read', 'logs:read'] }),
    )

    await findByRole('dialog')
    await user.selectOptions(getByLabelText('Role'), 'user')
    await user.click(getByRole('button', { name: 'Save' }))

    await waitFor(() => {
      expect(userBodies).toHaveLength(1)
    })
    expect(userBodies[0]?.role).toBe('user')
    expect(permissionCalls).toBe(0)
  })
})

describe('UserMutateSheet partial failure (I2)', () => {
  it('orders permissions first and reports a partial save when the user PATCH fails', async () => {
    installActorHandlers()
    const order: string[] = []

    server.use(
      http.patch(`${ORIGIN}/api/users/7/permissions`, async ({ request }) => {
        order.push('permissions')
        const body = (await request.json()) as { admin_permissions: string[] }
        expect(body.admin_permissions).toContain('pricing:read')
        return HttpResponse.json(adminUser())
      }),
      http.patch(`${ORIGIN}/api/users/7`, () => {
        order.push('user')
        return HttpResponse.json(
          { type: 'conflict', title: 'Conflict' },
          { status: 500 },
        )
      }),
    )

    const { getByLabelText, getByRole, findByRole, user } = await renderSheet(
      adminUser({ role: 'admin', admin_permissions: ['users:read', 'logs:read'] }),
    )

    await findByRole('dialog')
    await user.type(getByLabelText('Remark'), 'changed')
    await user.click(getByLabelText(/View pricing/))
    await user.click(getByRole('button', { name: 'Save' }))

    const alert = await findByRole('alert')
    expect(alert.textContent).toContain('Partially saved')
    expect(order).toEqual(['permissions', 'user'])
  })

  it('shows the superset rejection on the matrix and sends no user PATCH', async () => {
    // The actor can manage roles but does not hold `pricing:read`; the target
    // already has it, so keeping the matrix unchanged-but-touched still sends
    // a grant outside the actor's set and the server rejects it.
    installActorHandlers({
      admin_permissions: ['users:read', 'logs:read', 'users:manage-roles'],
    })
    let userPatchCalls = 0

    server.use(
      http.patch(`${ORIGIN}/api/users/7/permissions`, () =>
        HttpResponse.json(
          { type: 'forbidden', title: 'Forbidden' },
          { status: 403 },
        ),
      ),
      http.patch(`${ORIGIN}/api/users/7`, () => {
        userPatchCalls += 1
        return HttpResponse.json(adminUser())
      }),
    )

    const { getByLabelText, getByRole, findByRole, findByText, user } =
      await renderSheet(
        adminUser({
          role: 'admin',
          admin_permissions: ['users:read', 'logs:read', 'pricing:read'],
        }),
      )

    await findByRole('dialog')
    await user.type(getByLabelText('Remark'), 'changed')
    // Unchecking an allowed key still changes the submitted array, which
    // includes the target's pre-existing (actor-unheld) `pricing:read`.
    await user.click(getByLabelText(/View logs/))
    await user.click(getByRole('button', { name: 'Save' }))

    expect(
      await findByText('You cannot grant permissions beyond your own'),
    ).toBeTruthy()
    expect(userPatchCalls).toBe(0)
  })
})
