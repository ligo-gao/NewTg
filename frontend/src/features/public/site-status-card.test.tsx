import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { fireEvent, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { SiteStatusCard } from './site-status-card'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const READY_BODY = {
  status: 'ok',
  checks: { postgres: 'ok', redis: 'ok', migration: 'ok', config: 'ok', startup: 'ok' },
  version: '0.1.0',
  node: 'node-1',
}

describe('SiteStatusCard (F-09-13 recovery probe)', () => {
  it('shows the operational state from /readyz', async () => {
    server.use(http.get(`${ORIGIN}/readyz`, () => HttpResponse.json(READY_BODY)))

    const { findByText, getByRole } = await renderWithProviders(<SiteStatusCard />, ['public'])

    const label = await findByText('Operational')
    expect(label).toBeTruthy()
    expect(getByRole('status').getAttribute('aria-live')).toBe('polite')
    // 状态双编码：圆点 + 文本，不能只靠颜色（modules-00 §4.6.4）。
    expect(getByRole('status').querySelector('[data-testid="status-badge-dot"]')).toBeTruthy()
  })

  it('maps a 503 degraded probe to the partial-degradation state', async () => {
    server.use(
      http.get(`${ORIGIN}/readyz`, () =>
        HttpResponse.json(
          {
            ...READY_BODY,
            status: 'degraded',
            checks: { ...READY_BODY.checks, redis: 'fail' },
          },
          { status: 503 },
        ),
      ),
    )

    const { findByText } = await renderWithProviders(<SiteStatusCard />, ['public'])

    expect(await findByText('Partial degradation')).toBeTruthy()
  })

  it('shows an unknown state when the probe cannot be reached and retries', async () => {
    let fail = true
    server.use(
      http.get(`${ORIGIN}/readyz`, () =>
        fail
          ? HttpResponse.error()
          : HttpResponse.json(READY_BODY),
      ),
    )

    const { findByText, getByRole, queryByText } = await renderWithProviders(
      <SiteStatusCard />,
      ['public'],
    )

    expect(await findByText('Status unavailable')).toBeTruthy()
    // 探测失败绝不显示「运行正常」。
    expect(queryByText('Operational')).toBeNull()

    fail = false
    fireEvent.click(getByRole('button', { name: 'Retry' }))
    await waitFor(() => {
      expect(queryByText('Operational')).toBeTruthy()
    })
  })
})
