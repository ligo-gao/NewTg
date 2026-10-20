import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

import type { components } from '@/api/schema'
import { useToastStore } from '@/shared/ui/toast'
import { renderWithProviders } from '@/test-support/providers'

import { SubscriptionPlanDrawer } from './subscription-plan-drawer'

type SubscriptionPlan = components['schemas']['SubscriptionPlan']

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())
beforeEach(() => {
  useToastStore.setState({ toasts: [] })
})

function planRow(overrides: Partial<SubscriptionPlan> = {}): SubscriptionPlan {
  return {
    id: 1,
    plan_key: 'pro-monthly',
    title: 'Pro Monthly',
    description: 'monthly plan',
    period_days: 30,
    price_amount: 5000,
    currency: 'CNY',
    grant_credits: 0,
    reset_on_renew: true,
    allow_wallet_overflow: true,
    model_scope: null,
    concurrency_limit: null,
    grace_days: 0,
    status: 'enabled',
    sort_order: 1,
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
    ...overrides,
  }
}

async function renderDrawer(props: {
  mode?: 'create' | 'edit'
  plan?: SubscriptionPlan
}) {
  const onOpenChange = vi.fn()
  const onSaved = vi.fn()
  await renderWithProviders(
    <SubscriptionPlanDrawer
      open
      mode={props.mode ?? 'create'}
      plan={props.plan}
      onOpenChange={onOpenChange}
      onSaved={onSaved}
    />,
    ['common', 'admin'],
  )
  return { onOpenChange, onSaved }
}

describe('SubscriptionPlanDrawer', () => {
  it('explains 0 = unlimited and leaves the model scope empty by default', async () => {
    await renderDrawer({ mode: 'create' })

    expect(screen.getByText('New subscription plan')).toBeTruthy()
    expect(
      screen.getByText('Credits granted each period. 0 = unlimited.'),
    ).toBeTruthy()
    expect(
      screen.getByText(/Leave empty for all models/),
    ).toBeTruthy()
    expect(
      (screen.getByLabelText('Grant Credits') as HTMLInputElement).value,
    ).toBe('0')
    expect((screen.getByLabelText('Currency') as HTMLInputElement).value).toBe('CNY')
  })

  it('creates a plan, converting yuan to cents and empty scope to null', async () => {
    let body: unknown
    server.use(
      http.post(`${ORIGIN}/api/subscription-plans`, async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(planRow(), { status: 201 })
      }),
    )
    const { onSaved } = await renderDrawer({ mode: 'create' })
    const user = userEvent.setup()

    await user.type(screen.getByLabelText('Plan Key'), 'team-yearly')
    await user.type(screen.getByLabelText('Title'), 'Team Yearly')
    await user.clear(screen.getByLabelText('Price Amount'))
    await user.type(screen.getByLabelText('Price Amount'), '199.90')
    await user.clear(screen.getByLabelText('Period Days'))
    await user.type(screen.getByLabelText('Period Days'), '365')
    await user.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(body).toMatchObject({
      plan_key: 'team-yearly',
      title: 'Team Yearly',
      description: null,
      period_days: 365,
      price_amount: 19990,
      currency: 'CNY',
      grant_credits: 0,
      model_scope: null,
      concurrency_limit: null,
      status: 'enabled',
      sort_order: 0,
    })
  })

  it('maps a 409 plan_key_conflict to an inline form error', async () => {
    server.use(
      http.post(`${ORIGIN}/api/subscription-plans`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/plan_key_conflict',
            title: 'plan_key_conflict',
          },
          { status: 409 },
        ),
      ),
    )
    await renderDrawer({ mode: 'create' })
    const user = userEvent.setup()

    await user.type(screen.getByLabelText('Plan Key'), 'pro-monthly')
    await user.type(screen.getByLabelText('Title'), 'Duplicate')
    await user.click(screen.getByRole('button', { name: 'Save' }))

    expect(
      await screen.findByText('A plan with this plan key already exists.'),
    ).toBeTruthy()
  })

  it('maps 422 problem.fields back to the offending input', async () => {
    server.use(
      http.post(`${ORIGIN}/api/subscription-plans`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/validation',
            title: 'validation_failed',
            fields: ['title'],
          },
          { status: 422 },
        ),
      ),
    )
    await renderDrawer({ mode: 'create' })
    const user = userEvent.setup()

    await user.type(screen.getByLabelText('Plan Key'), 'valid-key')
    await user.type(screen.getByLabelText('Title'), 'Valid title')
    await user.click(screen.getByRole('button', { name: 'Save' }))

    expect(await screen.findByText('Enter a title of 1–64 characters.')).toBeTruthy()
    expect(screen.getByLabelText('Title').getAttribute('aria-invalid')).toBe('true')
  })

  it('edits a plan, keeping the stable key and existing model scope', async () => {
    let body: unknown
    server.use(
      http.put(`${ORIGIN}/api/subscription-plans/1`, async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(planRow({ title: 'Pro Monthly v2' }))
      }),
    )
    const { onSaved } = await renderDrawer({
      mode: 'edit',
      plan: planRow({ model_scope: ['gpt-4o', 'claude-sonnet'] }),
    })
    const user = userEvent.setup()

    expect(screen.getByText('Edit subscription plan')).toBeTruthy()
    const key = screen.getByLabelText('Plan Key') as HTMLInputElement
    expect(key.disabled).toBe(true)

    await user.clear(screen.getByLabelText('Title'))
    await user.type(screen.getByLabelText('Title'), 'Pro Monthly v2')
    await user.clear(screen.getByLabelText('Grant Credits'))
    await user.type(screen.getByLabelText('Grant Credits'), '1000')
    await user.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    expect(body).toMatchObject({
      plan_key: 'pro-monthly',
      title: 'Pro Monthly v2',
      grant_credits: 1000,
      model_scope: ['gpt-4o', 'claude-sonnet'],
    })
  })

  it('supports adding and removing model scope rows', async () => {
    await renderDrawer({ mode: 'create' })
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Add model' }))
    await user.type(screen.getByLabelText('Model name 1'), 'gpt-4o')
    expect(screen.getByDisplayValue('gpt-4o')).toBeTruthy()

    await user.click(screen.getByRole('button', { name: 'Remove model 1' }))
    expect(screen.queryByDisplayValue('gpt-4o')).toBeNull()
  })

  it('shows the offline notice and disables saving', async () => {
    Object.defineProperty(globalThis.navigator, 'onLine', {
      configurable: true,
      get: () => false,
    })
    window.dispatchEvent(new Event('offline'))
    try {
      await renderDrawer({ mode: 'create' })
      expect(
        screen.getByText(/You are offline\. Write actions are disabled/),
      ).toBeTruthy()
      expect(screen.getByRole('button', { name: 'Save' })).toHaveProperty(
        'disabled',
        true,
      )
    } finally {
      Object.defineProperty(globalThis.navigator, 'onLine', {
        configurable: true,
        get: () => true,
      })
      window.dispatchEvent(new Event('online'))
    }
  })
})

describe('SubscriptionPlanDrawer accessible structure', () => {
  it('exposes section headings and labelled fields', async () => {
    await renderDrawer({ mode: 'create' })
    const dialog = screen.getByRole('dialog')
    for (const heading of ['Basics', 'Period and quota', 'Scope and limits', 'Status']) {
      expect(within(dialog).getByRole('heading', { name: heading })).toBeTruthy()
    }
    expect(within(dialog).getByLabelText('Description')).toBeTruthy()
    expect(within(dialog).getByLabelText('Sort Order')).toBeTruthy()
  })
})
