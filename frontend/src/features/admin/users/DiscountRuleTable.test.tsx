import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { fireEvent, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import type { components } from '@/api/schema'
import enAdmin from '@/locales/en/admin.json'
import { renderWithProviders } from '@/test-support/providers'

import { DiscountRuleTable, toDatetimeLocal } from './DiscountRuleTable'

type DiscountRule = components['schemas']['DiscountRule']

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

/**
 * The timezone tests must fail on the old `slice(0, 16)` implementation even
 * on a UTC CI runner, where local time and UTC coincide. Pin a non-UTC zone so
 * the round-trip assertion is a real guard regardless of the host.
 */
const HOST_TZ = process.env.TZ

beforeAll(() => {
  process.env.TZ = 'Asia/Shanghai'
  server.listen({ onUnhandledRequest: 'error' })
})
afterEach(() => server.resetHandlers())
afterAll(() => {
  server.close()
  if (HOST_TZ === undefined) {
    delete process.env.TZ
  } else {
    process.env.TZ = HOST_TZ
  }
})

/** `YYYY-MM-DD` in the runtime's local calendar, matching the table's display. */
function localDate(iso: string): string {
  const date = new Date(iso)
  const pad = (part: number): string => String(part).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`
}

function rule(overrides: Partial<DiscountRule> = {}): DiscountRule {
  return {
    id: 5,
    model_name: 'gpt-4o',
    rate_percent: 90,
    valid_from: '2026-11-01T00:00:00Z',
    valid_until: null,
    enabled: true,
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
    ...overrides,
  }
}

/** Default pickers/selectors; every render hits both endpoints. */
function installDefaults(items: DiscountRule[] = [rule()]) {
  server.use(
    http.get(`${ORIGIN}/api/users/7/discounts`, () =>
      HttpResponse.json({ items }),
    ),
    http.get(`${ORIGIN}/api/users/discount-models`, () =>
      HttpResponse.json({
        items: [{ model_name: 'gpt-4o' }, { model_name: 'claude-sonnet-4' }],
      }),
    ),
  )
}

async function renderTable() {
  const result = await renderWithProviders(
    <DiscountRuleTable targetType="user" targetID="7" />,
    ['common', 'admin'],
  )
  return { ...result, user: userEvent.setup() }
}

describe('DiscountRuleTable', () => {
  it('renders rules with rate and window', async () => {
    installDefaults()

    const { findByText, getByText } = await renderTable()

    expect(await findByText('gpt-4o')).toBeTruthy()
    // The provider's default test locale is `en`, where the hint speaks in the
    // charged percentage rather than the fold number.
    expect(
      getByText(
        enAdmin.user_form.discount_rate_percent.replace('{value}', '90'),
      ),
    ).toBeTruthy()
    // Derive the day locally: the table renders in the viewer's calendar, so a
    // hardcoded UTC day would fail on negative-offset machines.
    expect(getByText(`${localDate('2026-11-01T00:00:00Z')} ~ —`)).toBeTruthy()
  })

  it('shows the empty state', async () => {
    installDefaults([])

    const { findByText } = await renderTable()

    expect(await findByText('No discount rules yet.')).toBeTruthy()
  })

  it('blocks rates outside 0-100', async () => {
    let created = 0
    server.use(
      http.post(`${ORIGIN}/api/users/7/discounts`, () => {
        created += 1
        return HttpResponse.json(rule(), { status: 201 })
      }),
    )
    installDefaults()

    const { findByText, getByLabelText, getByRole, user } = await renderTable()

    await findByText('gpt-4o')
    await user.click(getByRole('button', { name: 'Add rule' }))
    await user.type(getByLabelText('Model'), 'gpt-4o')
    await user.type(getByLabelText('Effective rate (%)'), '101')
    await user.click(getByRole('button', { name: 'Save rule' }))

    expect(
      await findByText('Enter a whole number from 0 to 100.'),
    ).toBeTruthy()
    expect(created).toBe(0)
  })

  it('rejects an inverted window', async () => {
    let created = 0
    server.use(
      http.post(`${ORIGIN}/api/users/7/discounts`, () => {
        created += 1
        return HttpResponse.json(rule(), { status: 201 })
      }),
    )
    installDefaults()

    const { findByText, getByLabelText, getByRole, user } = await renderTable()

    await findByText('gpt-4o')
    await user.click(getByRole('button', { name: 'Add rule' }))
    await user.type(getByLabelText('Model'), 'gpt-4o')
    await user.type(getByLabelText('Effective rate (%)'), '90')
    fireEvent.change(getByLabelText('Valid from'), {
      target: { value: '2026-11-02T00:00' },
    })
    fireEvent.change(getByLabelText('Valid until'), {
      target: { value: '2026-11-01T00:00' },
    })
    await user.click(getByRole('button', { name: 'Save rule' }))

    expect(
      await findByText('Valid from must be earlier than valid until.'),
    ).toBeTruthy()
    expect(created).toBe(0)
  })

  it('warns that 100 has no benefit', async () => {
    installDefaults()

    const { findByText, getByLabelText, getByRole, user } = await renderTable()

    await findByText('gpt-4o')
    await user.click(getByRole('button', { name: 'Add rule' }))
    await user.type(getByLabelText('Effective rate (%)'), '100')

    expect(
      await findByText('Full price (no actual discount)'),
    ).toBeTruthy()
  })

  it('deletes after confirmation', async () => {
    let deletedPath: string | undefined
    server.use(
      http.delete(`${ORIGIN}/api/users/7/discounts/:id`, ({ request }) => {
        deletedPath = new URL(request.url).pathname
        return new HttpResponse(null, { status: 204 })
      }),
    )
    installDefaults()

    const { findByText, getByRole, user } = await renderTable()

    await findByText('gpt-4o')
    await user.click(getByRole('button', { name: 'Delete' }))
    await user.click(getByRole('button', { name: 'Delete rule' }))

    await waitFor(() => {
      expect(deletedPath).toBe('/api/users/7/discounts/5')
    })
  })

  it('round-trips datetime-local values without a timezone shift', () => {
    const iso = '2026-11-01T00:00:00Z'

    const local = toDatetimeLocal(iso)

    // The picker value is local time; feeding it back must land on the same
    // instant, independent of the machine's TZ.
    expect(new Date(local).toISOString()).toBe(new Date(iso).toISOString())
    expect(toDatetimeLocal(null)).toBe('')
  })

  it('searches models through the debounced query parameter', async () => {
    const queries: string[] = []
    installDefaults()
    server.use(
      http.get(`${ORIGIN}/api/users/discount-models`, ({ request }) => {
        const url = new URL(request.url)
        queries.push(url.searchParams.get('q') ?? '')
        return HttpResponse.json({ items: [{ model_name: 'gpt-4o' }] })
      }),
    )

    const { findByText, getByLabelText, getByRole, user } = await renderTable()

    await findByText('gpt-4o')
    await user.click(getByRole('button', { name: 'Add rule' }))
    await user.type(getByLabelText('Model'), 'gpt')

    await waitFor(
      () => {
        expect(queries).toContain('gpt')
      },
      { timeout: 2000 },
    )
  })

  it('patches edited rules without model_name and keeps the listed window', async () => {
    let patchBody: Record<string, unknown> | undefined
    installDefaults([
      rule({
        valid_from: '2026-11-01T00:00:00Z',
        valid_until: '2026-11-05T00:00:00Z',
      }),
    ])
    server.use(
      http.patch(`${ORIGIN}/api/users/7/discounts/5`, async ({ request }) => {
        patchBody = (await request.json()) as Record<string, unknown>
        return HttpResponse.json(rule({ rate_percent: 80 }))
      }),
    )

    const { findByText, getByLabelText, getByRole, user } = await renderTable()

    await findByText('gpt-4o')
    await user.click(getByRole('button', { name: 'Edit' }))
    const rateInput = getByLabelText('Effective rate (%)')
    await user.clear(rateInput)
    await user.type(rateInput, '80')
    await user.click(getByRole('button', { name: 'Save rule' }))

    await waitFor(() => {
      expect(patchBody).toBeDefined()
    })
    // The server answers 422 `model_immutable` when this key is present at all.
    expect(patchBody).not.toHaveProperty('model_name')
    expect(patchBody?.rate_percent).toBe(80)
    expect(patchBody?.enabled).toBe(true)
    // Editing the rate alone must not shift the validity window.
    expect(new Date(String(patchBody?.valid_from)).toISOString()).toBe(
      new Date('2026-11-01T00:00:00Z').toISOString(),
    )
    expect(new Date(String(patchBody?.valid_until)).toISOString()).toBe(
      new Date('2026-11-05T00:00:00Z').toISOString(),
    )
  })

  it('does not show a rate hint before a rate is entered', async () => {
    installDefaults()

    const { findByText, getByRole, queryByText, user } = await renderTable()

    await findByText('gpt-4o')
    await user.click(getByRole('button', { name: 'Add rule' }))

    expect(queryByText('Free')).toBeNull()
  })

  it('does not save the rule from Enter in the model picker or the checkbox', async () => {
    let created = 0
    installDefaults()
    server.use(
      http.post(`${ORIGIN}/api/users/7/discounts`, () => {
        created += 1
        return HttpResponse.json(rule(), { status: 201 })
      }),
    )

    const { findByText, getByLabelText, getByRole, user } = await renderTable()

    await findByText('gpt-4o')
    await user.click(getByRole('button', { name: 'Add rule' }))
    await user.type(getByLabelText('Model'), 'gpt-4o')
    await user.type(getByLabelText('Effective rate (%)'), '90')

    // The model input backs a datalist: Enter belongs to the picker, not to
    // the rule form. The checkbox keeps its native keyboard semantics too.
    const modelInput = getByLabelText('Model')
    modelInput.focus()
    await user.keyboard('{Enter}')
    const enabledCheckbox = getByLabelText('Enabled') as HTMLInputElement
    enabledCheckbox.focus()
    await user.keyboard('{Enter}')

    expect(created).toBe(0)

    // The explicit button still saves normally.
    await user.click(getByRole('button', { name: 'Save rule' }))
    await waitFor(() => {
      expect(created).toBe(1)
    })
  })
})
