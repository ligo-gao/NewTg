import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'
import { useToastStore } from '@/shared/ui/toast'

import { GroupPricingMutateSheet } from './GroupPricingMutateSheet'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())
beforeEach(() => {
  useToastStore.setState({ toasts: [] })
})

function installHandlers() {
  server.use(
    http.get(`${ORIGIN}/api/groups`, () =>
      HttpResponse.json({
        items: [
          {
            id: 1,
            name: 'vip',
            description: null,
            ratio: 120,
            user_selectable: true,
            is_system: false,
            status: 1,
            users_count: 0,
            pricing_overrides_count: 0,
            channels_count: 0,
            created_at: '2026-10-01T10:00:00Z',
            updated_at: '2026-10-01T10:00:00Z',
          },
        ],
        total: 1,
        page: 1,
        page_size: 100,
      }),
    ),
    http.get(`${ORIGIN}/api/model-pricing/models`, () =>
      HttpResponse.json({
        items: [
          { model: 'gpt-5', mode: 'per_token', status: 1 },
          { model: 'tts-1', mode: 'per_request', status: 1 },
          { model: 'expr-model', mode: 'tiered_expr', status: 1 },
        ],
      }),
    ),
  )
}

async function renderSheet() {
  return renderWithProviders(
    <GroupPricingMutateSheet
      open
      mode="create"
      quotaPerUnit={100}
      onOpenChange={() => undefined}
      onSaved={() => undefined}
    />,
    ['common', 'admin'],
  )
}

describe('GroupPricingMutateSheet', () => {
  it('filters expression-priced sources out of the model selector', async () => {
    installHandlers()

    const { findByRole, queryByRole } = await renderSheet()

    expect(await findByRole('option', { name: 'gpt-5' })).toBeTruthy()
    expect(await findByRole('option', { name: 'tts-1' })).toBeTruthy()
    expect(queryByRole('option', { name: 'expr-model' })).toBeNull()
  })

  it('pins hidden dimensions to zero for a per-request source', async () => {
    installHandlers()
    const bodies: Record<string, unknown>[] = []
    server.use(
      http.post(`${ORIGIN}/api/group-pricing`, async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>)
        return HttpResponse.json(
          {
            id: 1,
            group: 'vip',
            model: 'tts-1',
            mode: 'per_request',
            prompt_price: 200,
            completion_price: 0,
            cache_read_price: 0,
            cache_write_price: 0,
            status: 1,
            source_status: 1,
            created_at: '2026-10-01T10:00:00Z',
            updated_at: '2026-10-01T10:00:00Z',
          },
          { status: 201 },
        )
      }),
    )

    const { findByRole, getByLabelText, getByRole, queryByLabelText } =
      await renderSheet()
    const user = userEvent.setup()

    await findByRole('option', { name: 'vip' })
    await user.selectOptions(getByLabelText('Group'), 'vip')
    await findByRole('option', { name: 'tts-1' })
    await user.selectOptions(getByLabelText('Model'), 'tts-1')

    // 按次源模型每次请求只计一个价；其他维度根本不渲染（UI §8.3）。
    expect(queryByLabelText('Completion price')).toBeNull()
    await user.type(getByLabelText('Price per request'), '200')
    await user.click(getByRole('button', { name: 'Save' }))

    await waitFor(() => {
      expect(bodies).toHaveLength(1)
    })
    expect(bodies[0]).toMatchObject({
      group: 'vip',
      model: 'tts-1',
      prompt_price: 200,
      completion_price: 0,
      cache_read_price: 0,
      cache_write_price: 0,
      status: 1,
    })
  })
})
