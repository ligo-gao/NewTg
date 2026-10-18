import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import type { components } from '@/api/schema'
import { renderWithProviders } from '@/test-support/providers'

import { GroupListPage } from './GroupListPage'

type AdminGroup = components['schemas']['AdminGroup']

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function group(overrides: Partial<AdminGroup>): AdminGroup {
  return {
    id: 1,
    name: 'default',
    description: null,
    ratio: 100,
    user_selectable: true,
    is_system: false,
    status: 1,
    users_count: 0,
    pricing_overrides_count: 0,
    channels_count: 0,
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
    ...overrides,
  }
}

describe('GroupListPage reference guard (I9/R-38)', () => {
  it('disables delete for referenced groups and explains the 409 semantics', async () => {
    server.use(
      http.get(`${ORIGIN}/api/groups`, () =>
        HttpResponse.json({
          items: [
            group({ id: 1, name: 'default', is_system: true }),
            group({ id: 2, name: 'vip', users_count: 5 }),
            group({ id: 3, name: 'empty' }),
          ],
          total: 3,
          page: 1,
          page_size: 20,
        }),
      ),
    )

    const { findByRole, getByRole } = await renderWithProviders(
      <GroupListPage />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    const vipRow = await findByRole('row', { name: /vip/ })
    const vipDelete = within(vipRow).getByRole('button', { name: 'Delete' })
    expect((vipDelete as HTMLButtonElement).disabled).toBe(true)
    expect(vipDelete.getAttribute('title')).toContain(
      'Groups referenced by users, tokens or pricing cannot be deleted',
    )

    const emptyRow = getByRole('row', { name: /empty/ })
    await user.click(within(emptyRow).getByRole('button', { name: 'Delete' }))

    // The confirmation copy must not promise the old "users fall back to
    // default" behaviour: the server returns 409 for any referenced group.
    const dialog = await findByRole('dialog')
    expect(dialog.textContent).toContain(
      'Groups referenced by users, tokens or pricing cannot be deleted',
    )
  })
})
