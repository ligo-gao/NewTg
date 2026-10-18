import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import type { components } from '@/api/schema'
import { renderWithProviders } from '@/test-support/providers'
import { useToastStore } from '@/shared/ui/toast'

import { GroupPricingListPage } from './GroupPricingListPage'

type GroupPricing = components['schemas']['GroupPricing']

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())
beforeEach(() => {
  useToastStore.setState({ toasts: [] })
})

function adminGroup(name: string, isSystem: boolean) {
  return {
    id: isSystem ? 1 : 2,
    name,
    description: null,
    ratio: isSystem ? 100 : 120,
    user_selectable: true,
    is_system: isSystem,
    status: 1,
    users_count: 0,
    pricing_overrides_count: 0,
    channels_count: 0,
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
  }
}

function overrideRow(overrides: Partial<GroupPricing> = {}): GroupPricing {
  return {
    id: 1,
    group: 'vip',
    model: 'gpt-5',
    mode: 'per_token',
    prompt_price: 400,
    completion_price: 1200,
    cache_read_price: 40,
    cache_write_price: 120,
    status: 1,
    source_status: 2,
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
    ...overrides,
  }
}

function installHandlers({
  groups = [adminGroup('default', true), adminGroup('vip', false)],
  overrides = [overrideRow()],
}: {
  groups?: ReturnType<typeof adminGroup>[]
  overrides?: GroupPricing[]
} = {}) {
  server.use(
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({ username: 'root', role: 'admin', status: 'enabled' }),
    ),
    http.get(`${ORIGIN}/api/users`, () =>
      HttpResponse.json({
        items: [
          {
            id: 1,
            username: 'root',
            display_name: null,
            email: null,
            role: 'admin',
            status: 'enabled',
            balance: 0,
            group: 'default',
            admin_permissions: null,
            totp_enabled: false,
            must_change_password: false,
            token_count: 0,
            created_at: '2026-10-01T10:00:00Z',
            updated_at: '2026-10-01T10:00:00Z',
          },
        ],
        total: 1,
        page: 1,
        page_size: 20,
      }),
    ),
    http.get(`${ORIGIN}/api/groups`, ({ request }) => {
      const params = new URL(request.url).searchParams
      return HttpResponse.json({
        items: groups,
        total: groups.length,
        page: Number(params.get('p') ?? '1'),
        page_size: Number(params.get('page_size') ?? '20'),
      })
    }),
    http.get(`${ORIGIN}/api/group-pricing`, ({ request }) => {
      const params = new URL(request.url).searchParams
      return HttpResponse.json({
        items: overrides,
        total: overrides.length,
        page: Number(params.get('p') ?? '1'),
        page_size: Number(params.get('page_size') ?? '20'),
        quota_per_unit: 100,
      })
    }),
    http.get(`${ORIGIN}/api/model-pricing/models`, () =>
      HttpResponse.json({
        items: [{ model: 'gpt-5', mode: 'per_token', status: 1 }],
      }),
    ),
  )
}

describe('GroupPricingListPage', () => {
  it('shows the read-only directory and the override table with source warnings', async () => {
    installHandlers()

    const { findAllByText, findByText, getByRole } = await renderWithProviders(
      <GroupPricingListPage />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    expect(await findByText('default')).toBeTruthy()
    expect(getByRole('link', { name: 'Manage groups' }).getAttribute('href')).toBe(
      '/admin/users/groups',
    )
    // 目录是只读的：分组没有任何写控件。
    expect(getByRole('table')).toBeTruthy()

    await user.click(getByRole('tab', { name: 'Group price overrides' }))

    // vip 与 gpt-5 也会出现在筛选/选择器选项里，因此断言至少出现一次，
    // 而不是要求唯一。
    expect((await findAllByText('vip')).length).toBeGreaterThan(0)
    expect((await findAllByText('gpt-5')).length).toBeGreaterThan(0)
    expect(await findByText('Source pricing is disabled')).toBeTruthy()
  })

  it('shows the custom-group empty note when only system groups exist', async () => {
    installHandlers({ groups: [adminGroup('default', true)] })

    const { findByText } = await renderWithProviders(<GroupPricingListPage />, [
      'common',
      'admin',
    ])

    expect(await findByText('No custom groups yet.')).toBeTruthy()
  })

  it('loads the model selector from the pricing models endpoint', async () => {
    installHandlers()

    const { findByRole } = await renderWithProviders(
      <GroupPricingListPage />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    await user.click(await findByRole('tab', { name: 'Group price overrides' }))
    await user.click(await findByRole('button', { name: 'New override' }))

    expect(await findByRole('option', { name: 'gpt-5' })).toBeTruthy()
  })
})
