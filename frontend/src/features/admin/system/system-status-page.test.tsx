import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { SystemStatusPage } from './system-status-page'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

describe('SystemStatusPage', () => {
  it('renders version, uptime, dependencies and instance heartbeats', async () => {
    server.use(
      http.get(`${ORIGIN}/api/system/status`, () =>
        HttpResponse.json({
          version: '1.4.2',
          uptime: 90_061,
          instances: [
            {
              node_name: 'gateway-1',
              role: 'gateway',
              status: 'active',
              version: '1.4.2',
              last_seen: '2026-10-19T10:00:00Z',
            },
            {
              name: 'worker-1',
              role: 'worker',
              status: 'stale',
              version: '1.4.1',
            },
          ],
          dependencies: { db: 'ok', redis: { status: 'ok' }, worker: 'down' },
        }),
      ),
    )

    await renderWithProviders(<SystemStatusPage />, ['common', 'settings'])

    expect((await screen.findAllByText('1.4.2')).length).toBeGreaterThan(0)
    expect(screen.getByText('1d 1h 1m')).toBeTruthy()
    expect(screen.getAllByText('Healthy')).toHaveLength(2)
    expect(screen.getByText('Degraded')).toBeTruthy()
    expect(screen.getByText('gateway-1')).toBeTruthy()
    expect(screen.getByText('worker-1')).toBeTruthy()
    expect(screen.getByText('stale')).toBeTruthy()
  })

  it('shows the empty instance state', async () => {
    server.use(
      http.get(`${ORIGIN}/api/system/status`, () =>
        HttpResponse.json({
          version: '1.4.2',
          uptime: 30,
          instances: [],
          dependencies: { db: true, redis: true, worker: true },
        }),
      ),
    )

    await renderWithProviders(<SystemStatusPage />, ['common', 'settings'])

    expect(await screen.findByText('No instance heartbeats. Check your deployment.')).toBeTruthy()
    expect(screen.getByText('30s')).toBeTruthy()
  })

  it('shows the error state and retries', async () => {
    const user = userEvent.setup()
    let calls = 0
    server.use(
      http.get(`${ORIGIN}/api/system/status`, () => {
        calls += 1
        return HttpResponse.json(
          { type: 'about:blank', title: 'Internal Server Error' },
          { status: 500 },
        )
      }),
    )

    await renderWithProviders(<SystemStatusPage />, ['common', 'settings'])

    expect(await screen.findByText(/Could not load system status/)).toBeTruthy()
    const before = calls
    await user.click(screen.getByRole('button', { name: 'Retry' }))
    await waitFor(() => expect(calls).toBeGreaterThan(before))
  })
})
