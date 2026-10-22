import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { PricingTab } from './pricing-tab'
import { RuntimeTab } from './runtime-tab'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

describe('pricing tab (F-08-37)', () => {
  it('shows the pricing empty state', async () => {
    server.use(
      http.get(`${ORIGIN}/api/plugin/task`, () =>
        HttpResponse.json({ items: [], total: 0, page: 1, page_size: 100 }),
      ),
    )

    await renderWithProviders(<PricingTab />, ['common', 'admin'])
    expect(await screen.findByTestId('plugin-pricing-empty')).toBeTruthy()
  })
})

const RUNTIME_INSTANCE = {
  instance_id: 'gateway-1',
  node: 'node-a',
  role: 'gateway',
  version: '1.5.0',
  local: true,
  started_at: '2026-10-19T09:00:00Z',
  last_seen: '2026-10-19T10:00:00Z',
  generation: 12,
  revision: 'rev-abc123',
  reloaded_at: '2026-10-19T10:00:00Z',
  plugins: [
    {
      key: 'kling',
      status: 'registered',
      version: '1.0.0',
      layer: 'factory',
    },
  ],
}

const RUNTIME = {
  instances: [RUNTIME_INSTANCE],
  aggregate_unavailable: false,
}

describe('runtime tab (F-08-36)', () => {
  it('renders per-node generation metadata and the node-local hint', async () => {
    server.use(
      http.get(`${ORIGIN}/api/plugin/task/runtime/status`, () =>
        HttpResponse.json(RUNTIME),
      ),
    )

    await renderWithProviders(<RuntimeTab />, ['common', 'admin'])

    const card = await screen.findByTestId('runtime-node-card')
    expect(card.textContent).toContain('gateway-1')
    expect(card.textContent).toContain('generation 12')
    expect(card.textContent).toContain('revision rev-abc123')
    expect(screen.getByTestId('runtime-local-badge')).toBeTruthy()
    expect(
      screen.getByText('generation is node-local. Compare across nodes by DB revision.'),
    ).toBeTruthy()
    expect(screen.getByText('No plugin errors.')).toBeTruthy()
  })

  it('renders every replica in the aggregate and flags Redis degradation', async () => {
    server.use(
      http.get(`${ORIGIN}/api/plugin/task/runtime/status`, () =>
        HttpResponse.json({
          aggregate_unavailable: true,
          instances: [
            RUNTIME_INSTANCE,
            {
              ...RUNTIME_INSTANCE,
              instance_id: 'worker-1',
              node: 'node-b',
              role: 'worker',
              local: false,
            },
          ],
        }),
      ),
    )

    await renderWithProviders(<RuntimeTab />, ['common', 'admin'])

    const cards = await screen.findAllByTestId('runtime-node-card')
    expect(cards.map((card) => card.getAttribute('data-instance-id'))).toEqual([
      'gateway-1',
      'worker-1',
    ])
    expect(screen.getAllByTestId('runtime-local-badge')).toHaveLength(1)
    expect(screen.getByTestId('runtime-aggregate-unavailable').textContent).toContain(
      'Redis aggregation unavailable',
    )
  })

  it('shows per-plugin errors with the failing node status', async () => {
    server.use(
      http.get(`${ORIGIN}/api/plugin/task/runtime/status`, () =>
        HttpResponse.json({
          ...RUNTIME,
          instances: [
            {
              ...RUNTIME_INSTANCE,
              reload_error: 'reload failed',
              plugins: [
                {
                  key: 'mj',
                  status: 'compile_failed',
                  version: '0.9.1',
                  errors: [
                    {
                      key: 'mj',
                      fieldPath: 'plugin.js:42',
                      locale: '',
                      rule: 'compile',
                      message: 'unexpected token',
                    },
                  ],
                },
              ],
            },
          ],
        }),
      ),
    )

    await renderWithProviders(<RuntimeTab />, ['common', 'admin'])

    const errorRow = await screen.findByTestId('runtime-plugin-error')
    expect(errorRow.textContent).toContain('mj')
    expect(errorRow.textContent).toContain('Compile failed')
    expect(errorRow.textContent).toContain('plugin.js:42: unexpected token')
    expect(screen.getByText('Rebuild failed')).toBeTruthy()
  })

  it('fetches only on demand and refreshes manually', async () => {
    let requests = 0
    server.use(
      http.get(`${ORIGIN}/api/plugin/task/runtime/status`, () => {
        requests += 1
        return HttpResponse.json(RUNTIME)
      }),
    )

    await renderWithProviders(<RuntimeTab />, ['common', 'admin'])
    await screen.findByTestId('runtime-node-card')
    expect(requests).toBe(1)

    // 不自动轮询：等待一段真实时间后请求数不变。
    await new Promise((resolve) => setTimeout(resolve, 250))
    expect(requests).toBe(1)

    await userEvent.click(screen.getByRole('button', { name: 'Refresh' }))
    await waitFor(() => expect(requests).toBe(2))
    expect(screen.getByTestId('runtime-refreshed-at')).toBeTruthy()
  })
})
