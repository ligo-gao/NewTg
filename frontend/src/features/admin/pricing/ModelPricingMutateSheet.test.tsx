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
    model: 'gpt-5',
    mode: 'per_token',
    billing_expr: null,
    prompt_price: 500,
    completion_price: 1500,
    cache_read_price: 50,
    cache_write_price: 150,
    author: 'Unknown',
    vendor: null,
    status: 1,
    free: false,
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
    ...overrides,
  }
}

function previewSuccess(mode = 'per_token', quota = 12) {
  return HttpResponse.json({
    quota,
    quota_per_unit: 100,
    amount_yuan: (quota / 100).toFixed(2),
    mode,
    free_model: false,
    clamped: false,
    clamp: null,
  })
}

function installPreview(handler?: (spec: Record<string, unknown>) => Response) {
  server.use(
    http.post(`${ORIGIN}/api/model-pricing:preview`, async ({ request }) => {
      const body = (await request.json()) as { spec: Record<string, unknown> }
      if (handler !== undefined) return handler(body.spec)
      const mode = typeof body.spec.mode === 'string' ? body.spec.mode : 'per_token'
      return previewSuccess(mode === 'tiered_expr' ? 'tiered_expr' : mode)
    }),
  )
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

describe('PricingMutateSheet expression preview', () => {
  it('marks an invalid expression red at the editor and previews a valid one', async () => {
    installPreview((spec) => {
      const expr = typeof spec.expr === 'string' ? spec.expr.trim() : ''
      if (expr.endsWith('*')) {
        return HttpResponse.json(
          {
            type: 'about:blank',
            title: 'Invalid request',
            detail: 'pricing: unexpected end of expression',
          },
          { status: 400 },
        )
      }
      return previewSuccess('tiered_expr')
    })

    const { findByText, findAllByText, getByLabelText, getByRole } =
      await renderWithProviders(
        <ModelPricingMutateSheet
          open
          mode="create"
          quotaPerUnit={100}
          onOpenChange={() => undefined}
          onSaved={() => undefined}
        />,
        ['common', 'admin'],
      )
    const user = userEvent.setup()

    await user.click(getByRole('button', { name: 'Expression' }))
    const editor = getByLabelText('Billing expression')
    await user.type(editor, 'p *')

    const errors = await findAllByText('The expression is invalid.')
    expect(errors.length).toBeGreaterThan(0)
    expect(editor.getAttribute('aria-invalid')).toBe('true')

    await user.clear(editor)
    await user.type(editor, 'p * 0.001')

    expect(await findByText('12 credits')).toBeTruthy()
    expect(editor.getAttribute('aria-invalid')).toBeNull()
  })

  it('offers a retry when the preview itself fails', async () => {
    let calls = 0
    server.use(
      http.post(`${ORIGIN}/api/model-pricing:preview`, () => {
        calls += 1
        if (calls === 1) {
          return HttpResponse.json(
            { type: 'about:blank', title: 'Server error' },
            { status: 500 },
          )
        }
        return previewSuccess()
      }),
    )

    const { findByText, getByRole } = await renderWithProviders(
      <ModelPricingMutateSheet
        open
        mode="create"
        quotaPerUnit={100}
        onOpenChange={() => undefined}
        onSaved={() => undefined}
      />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    expect(await findByText('Preview failed.')).toBeTruthy()
    await user.click(getByRole('button', { name: 'Retry preview' }))
    expect(await findByText('12 credits')).toBeTruthy()
  })
})

describe('PricingMutateSheet price unit toggle', () => {
  it('submits whole credits when the operator types yuan', async () => {
    installPreview()
    const bodies: Record<string, unknown>[] = []
    server.use(
      http.post(`${ORIGIN}/api/model-pricing`, async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>)
        return HttpResponse.json(pricingRow({ prompt_price: 150 }), { status: 201 })
      }),
    )

    const { findByText, getByLabelText, getByRole } = await renderCreate()
    const user = userEvent.setup()

    await user.type(getByLabelText('Model'), 'gpt-x')
    await user.click(getByRole('button', { name: 'Yuan' }))
    await user.type(getByLabelText('Prompt price'), '1.5')

    expect(await findByText('= 150 credits')).toBeTruthy()
    await user.click(getByRole('button', { name: 'Save' }))

    await waitFor(() => {
      expect(bodies).toHaveLength(1)
    })
    expect(bodies[0]).toMatchObject({
      model: 'gpt-x',
      mode: 'per_token',
      prompt_price: 150,
      status: 1,
    })
  })
})

describe('PricingMutateSheet optimistic locking', () => {
  it('keeps the input and shows the conflict copy on a 409', async () => {
    installPreview()
    server.use(
      http.patch(`${ORIGIN}/api/model-pricing/gpt-5`, () =>
        HttpResponse.json(
          {
            type: 'about:blank',
            title: 'Conflict',
            detail: 'updated_at mismatch',
          },
          { status: 409 },
        ),
      ),
    )

    const { findByText, getByLabelText, getByRole } = await renderWithProviders(
      <ModelPricingMutateSheet
        open
        mode="edit"
        pricing={pricingRow()}
        quotaPerUnit={100}
        onOpenChange={() => undefined}
        onSaved={() => undefined}
      />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    const prompt = getByLabelText('Prompt price')
    await user.clear(prompt)
    await user.type(prompt, '999')
    await user.click(getByRole('button', { name: 'Save' }))

    expect(
      await findByText(
        'This pricing was changed by someone else. Reopen the sheet to continue from the latest version.',
      ),
    ).toBeTruthy()
    // 操作者的输入不会被回滚（web-00 §5.6）。
    expect((prompt as HTMLInputElement).value).toBe('999')
  })

  it('leaves expression mode with an explicit billing_expr:null', async () => {
    installPreview()
    const bodies: Record<string, unknown>[] = []
    server.use(
      http.patch(`${ORIGIN}/api/model-pricing/gpt-5`, async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>)
        return HttpResponse.json(
          pricingRow({ mode: 'per_token', billing_expr: null, prompt_price: 100 }),
        )
      }),
    )

    const { getByLabelText, getByRole } = await renderWithProviders(
      <ModelPricingMutateSheet
        open
        mode="edit"
        pricing={pricingRow({
          mode: 'tiered_expr',
          billing_expr: 'p * 0.001',
          prompt_price: null,
          completion_price: null,
          cache_read_price: null,
          cache_write_price: null,
        })}
        quotaPerUnit={100}
        onOpenChange={() => undefined}
        onSaved={() => undefined}
      />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    await user.click(getByRole('button', { name: 'Per token' }))
    await user.type(getByLabelText('Prompt price'), '100')
    await user.click(getByRole('button', { name: 'Save' }))

    await waitFor(() => {
      expect(bodies).toHaveLength(1)
    })
    expect(bodies[0]).toMatchObject({
      updated_at: '2026-10-01T10:00:00Z',
      mode: 'per_token',
      billing_expr: null,
      prompt_price: 100,
    })
  })
})
