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

const RUNTIME = {
  node: 'gateway-1',
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
    expect(
      screen.getByText('generation is node-local. Compare across nodes by DB revision.'),
    ).toBeTruthy()
    expect(screen.getByText('No plugin errors.')).toBeTruthy()
  })

  it('shows per-plugin errors with the failing node status', async () => {
    server.use(
      http.get(`${ORIGIN}/api/plugin/task/runtime/status`, () =>
        HttpResponse.json({
          ...RUNTIME,
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
