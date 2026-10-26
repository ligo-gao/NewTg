import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import type { components } from '@/api/schema'
import { renderWithProviders } from '@/test-support/providers'
import { useToastStore } from '@/shared/ui/toast'

import { ModelPricingMutateSheet } from './ModelPricingMutateSheet'

type ModelPricing = components['schemas']['ModelPricing']
type PricingStrategy = components['schemas']['PricingStrategy']

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())
beforeEach(() => {
  useToastStore.setState({ toasts: [] })
})

function pricingRow(overrides: Partial<ModelPricing> = {}): ModelPricing {
  return {
    id: 1,
    model: 'deepseek-flash',
    mode: 'per_token',
    billing_expr: null,
    prompt_price: 200,
    completion_price: 800,
    cache_read_price: 4,
    cache_write_price: 250,
    author: 'Unknown',
    vendor: 'DeepSeek',
    status: 1,
    free: false,
    has_strategy: false,
    strategy_kind: 'flat',
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
    ...overrides,
  }
}

const STRATEGY: PricingStrategy = {
  version: 1,
  time: {
    enabled: true,
    timezone: 'Asia/Shanghai',
    holiday_policy: 'cn_offpeak',
    default_multiplier: 0.5,
    windows: [
      {
        name: 'Morning peak',
        days: [1, 2, 3, 4, 5],
        start: '09:00',
        end: '12:00',
        multiplier: 1,
      },
    ],
  },
}

function installCandidates(): void {
  server.use(
    http.get(`${ORIGIN}/api/model-pricing/candidates`, () =>
      HttpResponse.json({
        items: [
          {
            model: 'deepseek-flash',
            display_name: 'DeepSeek Flash',
            vendor: 'DeepSeek',
            context_length: 128000,
            sources: ['channel'],
            priced: false,
            mode: 'per_token',
          },
        ],
      }),
    ),
  )
}

function previewStrategyOk(body: { strategy?: PricingStrategy | null }) {
  return HttpResponse.json({
    match: {
      window: '',
      multiplier: 1,
      holiday_name: '',
      holiday_off_day: false,
      holiday_lookup_failed: false,
    },
    schedule: null,
    strategy: body.strategy ?? null,
    effective_rates: {
      prompt_price: 200,
      completion_price: 800,
      cache_read_price: 4,
      cache_write_price: 250,
    },
    total: { quota: 29, quota_per_unit: 100, amount_yuan: '0.29' },
  })
}

interface Handlers {
  created: Record<string, unknown>[]
  patched: Record<string, unknown>[]
  previews: Record<string, unknown>[]
}

function installSaveHandlers(): Handlers {
  const created: Record<string, unknown>[] = []
  const patched: Record<string, unknown>[] = []
  const previews: Record<string, unknown>[] = []
  server.use(
    http.post(`${ORIGIN}/api/model-pricing:preview-strategy`, async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>
      previews.push(body)
      return previewStrategyOk(body as { strategy?: PricingStrategy | null })
    }),
    http.post(`${ORIGIN}/api/model-pricing`, async ({ request }) => {
      created.push((await request.json()) as Record<string, unknown>)
      return HttpResponse.json(pricingRow(), { status: 201 })
    }),
    http.patch(`${ORIGIN}/api/model-pricing/deepseek-flash`, async ({ request }) => {
      patched.push((await request.json()) as Record<string, unknown>)
      return HttpResponse.json(pricingRow({ has_strategy: false, strategy_kind: 'flat' }))
    }),
  )
  return { created, patched, previews }
}

async function renderCreate() {
  return renderWithProviders(
    <ModelPricingMutateSheet
      open
      mode="create"
      quotaPerUnit={100}
      onOpenChange={() => undefined}
      onSaved={() => undefined}
    />,
    ['common', 'admin'],
  )
}

describe('PricingMutateSheet strategy payload', () => {
  it('sends the normalized strategy with decimal multipliers', async () => {
    installCandidates()
    const handlers = installSaveHandlers()
    const { getByLabelText, getByRole } = await renderCreate()
    const user = userEvent.setup()

    await user.type(getByLabelText('Model'), 'deepseek-flash')
    await user.type(getByLabelText('Input price (cache miss)'), '200')
    await user.click(getByLabelText('Enable time pricing'))
    const defaultMultiplier = getByLabelText('Default multiplier (%)')
    await user.clear(defaultMultiplier)
    await user.type(defaultMultiplier, '50')
    await user.click(getByRole('button', { name: 'Add window' }))
    await user.click(getByRole('button', { name: 'Save' }))

    await waitFor(() => {
      expect(handlers.created).toHaveLength(1)
    })
    expect(handlers.created[0]?.strategy).toEqual({
      version: 1,
      time: {
        enabled: true,
        timezone: 'Asia/Shanghai',
        holiday_policy: 'none',
        default_multiplier: 0.5,
        windows: [
          {
            name: '',
            days: [1, 2, 3, 4, 5],
            start: '09:00',
            end: '18:00',
            multiplier: 1,
          },
        ],
      },
    })
    // 保存前的服务端一致性校验使用同一份草稿。
    expect(handlers.previews).toHaveLength(1)
    expect(handlers.previews[0]?.mode).toBe('per_token')
  })

  it('omits strategy entirely when time pricing is disabled', async () => {
    installCandidates()
    const handlers = installSaveHandlers()
    const { getByLabelText, getByRole } = await renderCreate()
    const user = userEvent.setup()

    await user.type(getByLabelText('Model'), 'deepseek-flash')
    await user.type(getByLabelText('Input price (cache miss)'), '200')
    await user.click(getByRole('button', { name: 'Save' }))

    await waitFor(() => {
      expect(handlers.created).toHaveLength(1)
    })
    expect('strategy' in (handlers.created[0] ?? {})).toBe(false)
    expect(handlers.previews).toHaveLength(0)
  })

  it('loads the stored strategy and clears it when time pricing is turned off', async () => {
    const row = pricingRow({ has_strategy: true, strategy_kind: 'peak_offpeak' })
    const handlers = installSaveHandlers()
    server.use(
      http.get(`${ORIGIN}/api/model-pricing/deepseek-flash`, () =>
        HttpResponse.json({ ...row, strategy: STRATEGY }),
      ),
    )

    const { getByLabelText, getByRole } = await renderWithProviders(
      <ModelPricingMutateSheet
        open
        mode="edit"
        pricing={row}
        quotaPerUnit={100}
        onOpenChange={() => undefined}
        onSaved={() => undefined}
      />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    await waitFor(() => {
      expect((getByLabelText('Enable time pricing') as HTMLInputElement).checked).toBe(true)
    })
    expect((getByLabelText('Window name') as HTMLInputElement).value).toBe('Morning peak')

    await user.click(getByLabelText('Enable time pricing'))
    await user.click(getByRole('button', { name: 'Save' }))

    await waitFor(() => {
      expect(handlers.patched).toHaveLength(1)
    })
    expect(handlers.patched[0]?.strategy).toBeNull()
    // 关闭时段后不再需要服务端试算。
    expect(handlers.previews).toHaveLength(0)
  })

  it('blocks saving when the stored strategy cannot be loaded', async () => {
    const row = pricingRow({ has_strategy: true, strategy_kind: 'peak_offpeak' })
    const handlers = installSaveHandlers()
    server.use(
      http.get(`${ORIGIN}/api/model-pricing/deepseek-flash`, () =>
        HttpResponse.json({ type: 'about:blank', title: 'Server error' }, { status: 500 }),
      ),
    )

    const { findByText, getByRole } = await renderWithProviders(
      <ModelPricingMutateSheet
        open
        mode="edit"
        pricing={row}
        quotaPerUnit={100}
        onOpenChange={() => undefined}
        onSaved={() => undefined}
      />,
      ['common', 'admin'],
    )

    expect(
      await findByText(
        'Could not load the stored strategy. Retry before saving so existing time pricing is not cleared.',
      ),
    ).toBeTruthy()
    expect(
      (getByRole('button', { name: 'Save' }) as HTMLButtonElement).disabled,
    ).toBe(true)
    expect(handlers.patched).toHaveLength(0)
  })

  it('blocks saving when the server rejects the strategy', async () => {
    installCandidates()
    const handlers = installSaveHandlers()
    server.use(
      http.post(`${ORIGIN}/api/model-pricing:preview-strategy`, () =>
        HttpResponse.json(
          {
            type: 'about:blank',
            title: 'Validation failed',
            fields: ['strategy.time.windows[0].start'],
          },
          { status: 422 },
        ),
      ),
    )
    const { findByText, getByLabelText, getByRole } = await renderCreate()
    const user = userEvent.setup()

    await user.type(getByLabelText('Model'), 'deepseek-flash')
    await user.type(getByLabelText('Input price (cache miss)'), '200')
    await user.click(getByLabelText('Enable time pricing'))
    await user.click(getByRole('button', { name: 'Add window' }))
    await user.click(getByRole('button', { name: 'Save' }))

    expect(await findByText('Enter a valid start time.')).toBeTruthy()
    expect(handlers.created).toHaveLength(0)
  })

  it('blocks saving a locally invalid multiplier before calling the server', async () => {
    installCandidates()
    const handlers = installSaveHandlers()
    const { findByText, getByLabelText, getByRole } = await renderCreate()
    const user = userEvent.setup()

    await user.type(getByLabelText('Model'), 'deepseek-flash')
    await user.type(getByLabelText('Input price (cache miss)'), '200')
    await user.click(getByLabelText('Enable time pricing'))
    const defaultMultiplier = getByLabelText('Default multiplier (%)')
    await user.clear(defaultMultiplier)
    await user.type(defaultMultiplier, '0')
    await user.click(getByRole('button', { name: 'Save' }))

    expect(await findByText('Enter a multiplier from 1 to 10000 %.')).toBeTruthy()
    expect(handlers.created).toHaveLength(0)
    expect(handlers.previews).toHaveLength(0)
  })
})
