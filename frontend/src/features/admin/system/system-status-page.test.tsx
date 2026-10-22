import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { SystemStatusPage } from './system-status-page'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
beforeEach(() => {
  // W17-2：系统状态页新增「系统任务」卡，会请求 /api/system/tasks。
  // 默认给空列表；需要任务数据的用例自行覆盖。
  server.use(
    http.get(`${ORIGIN}/api/system/tasks`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
    ),
  )
})
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

describe('SystemStatusPage', () => {
  it('renders version, uptime, dependencies and instance heartbeats', async () => {
    server.use(
      http.get(`${ORIGIN}/api/instances`, () =>
        HttpResponse.json({
          current_instance_id: 'gateway-1',
          items: [
            {
              instance_id: 'gateway-1',
              role: 'gateway',
              status: 'active',
              version: '1.4.2',
              last_seen: '2026-10-19T10:00:00Z',
            },
            {
              instance_id: 'worker-1',
              role: 'worker',
              status: 'stale',
              version: '1.4.1',
              last_seen: '2026-10-19T09:58:00Z',
            },
          ],
        }),
      ),
      http.get(`${ORIGIN}/api/system/status`, () =>
        HttpResponse.json({
          version: '1.4.2',
          uptime: 90_061,
          instances: [],
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
      http.get(`${ORIGIN}/api/instances`, () =>
        HttpResponse.json({ current_instance_id: 'gateway-1', items: [] }),
      ),
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
      http.get(`${ORIGIN}/api/instances`, () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'Service Unavailable' },
          { status: 503 },
        ),
      ),
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

  it('hides the instance card without settings:manage', async () => {
    server.use(
      http.get(`${ORIGIN}/api/instances`, () =>
        HttpResponse.json(
          { type: 'https://newtg.dev/problems/forbidden', title: 'Forbidden' },
          { status: 403 },
        ),
      ),
      http.get(`${ORIGIN}/api/system/status`, () =>
        HttpResponse.json({
          version: '1.4.2',
          uptime: 30,
          instances: [{ name: 'gateway-1', role: 'gateway', status: 'active' }],
          dependencies: { db: 'ok', redis: 'ok', worker: 'unknown' },
        }),
      ),
    )

    await renderWithProviders(<SystemStatusPage />, ['common', 'settings'])

    expect((await screen.findAllByText('1.4.2')).length).toBeGreaterThan(0)
    expect(screen.queryByTestId('system-status-instances')).toBeNull()
    expect(screen.queryByText('gateway-1')).toBeNull()
  })

  it('falls back to the status payload when the registry is unavailable', async () => {
    server.use(
      http.get(`${ORIGIN}/api/instances`, () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'Service Unavailable' },
          { status: 503 },
        ),
      ),
      http.get(`${ORIGIN}/api/system/status`, () =>
        HttpResponse.json({
          version: '1.4.2',
          uptime: 30,
          instances: [{ name: 'gateway-1', role: 'gateway', status: 'active' }],
          dependencies: { db: 'ok', redis: 'ok', worker: 'unknown' },
        }),
      ),
    )

    await renderWithProviders(<SystemStatusPage />, ['common', 'settings'])

    expect(await screen.findByTestId('system-status-instances')).toBeTruthy()
    expect(screen.getByText('gateway-1')).toBeTruthy()
  })
})
