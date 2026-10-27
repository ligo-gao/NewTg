import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import type { components } from '@/api/schema'
import { renderWithProviders } from '@/test-support/providers'

import { GroupListPage } from './GroupListPage'

type AdminGroup = components['schemas']['AdminGroup']
type DiscountRule = components['schemas']['DiscountRule']

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

function discountRule(overrides: Partial<DiscountRule> = {}): DiscountRule {
  return {
    id: 5,
    model_name: 'gpt-4o',
    rate_percent: 90,
    valid_from: null,
    valid_until: null,
    enabled: true,
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
    ...overrides,
  }
}

/** One `acme` group in the list plus the two endpoints the discount table hits. */
function installDiscountDefaults(rules: DiscountRule[] = []) {
  server.use(
    http.get(`${ORIGIN}/api/groups`, () =>
      HttpResponse.json({
        items: [group({ id: 4, name: 'acme' })],
        total: 1,
        page: 1,
        page_size: 20,
      }),
    ),
    http.get(`${ORIGIN}/api/groups/acme/discounts`, () =>
      HttpResponse.json({ items: rules }),
    ),
    http.get(`${ORIGIN}/api/groups/discount-models`, () =>
      HttpResponse.json({ items: [{ model_name: 'gpt-4o' }] }),
    ),
  )
}

/** Renders the page and opens the row's discount sheet. */
async function openDiscountSheet() {
  const result = await renderWithProviders(<GroupListPage />, [
    'common',
    'admin',
  ])
  const user = userEvent.setup()
  const row = await result.findByRole('row', { name: /acme/ })
  await user.click(within(row).getByRole('button', { name: 'Discounts' }))
  const sheet = await result.findByRole('dialog')
  return { ...result, user, sheet }
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

describe('GroupListPage group discounts', () => {
  it('opens the group discount dialog', async () => {
    installDiscountDefaults()

    const { sheet } = await openDiscountSheet()

    expect(
      within(sheet).getByRole('heading', { name: 'Discounts for acme' }),
    ).toBeTruthy()
    expect(sheet.textContent).toContain(
      'Personal and group discounts do not stack; the lower effective rate applies.',
    )
  })

  it('lists group rules', async () => {
    installDiscountDefaults([discountRule()])

    const { findByText } = await openDiscountSheet()

    expect(await findByText('gpt-4o')).toBeTruthy()
  })

  it('creates a rule from the dialog', async () => {
    let created = 0
    let rulesCalls = 0
    let body: Record<string, unknown> | undefined

    installDiscountDefaults()
    server.use(
      http.get(`${ORIGIN}/api/groups/acme/discounts`, () => {
        rulesCalls += 1
        return HttpResponse.json({ items: [] })
      }),
      http.post(`${ORIGIN}/api/groups/acme/discounts`, async ({ request }) => {
        created += 1
        body = (await request.json()) as Record<string, unknown>
        return HttpResponse.json(discountRule(), { status: 201 })
      }),
    )

    const { findByRole, getByLabelText, getByRole, user } =
      await openDiscountSheet()

    await user.click(await findByRole('button', { name: 'Add rule' }))
    await user.type(getByLabelText('Model'), 'gpt-4o')
    await user.type(getByLabelText('Effective rate (%)'), '90')
    await user.click(getByRole('button', { name: 'Save rule' }))

    await waitFor(() => {
      expect(created).toBe(1)
    })
    expect(body?.model_name).toBe('gpt-4o')
    expect(body?.rate_percent).toBe(90)
    // The real cache key is ['discount-rules','group',name]; invalidation of an
    // active query refetches it, which is what the operator sees.
    await waitFor(() => {
      expect(rulesCalls).toBeGreaterThanOrEqual(2)
    })
  })
})
