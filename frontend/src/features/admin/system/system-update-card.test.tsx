import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { SystemUpdateCard } from './system-update-card'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const base = {
  enabled: true,
  source_configured: true,
  channel: 'stable' as const,
  current_version: '1.0.0',
  cached: true,
  cache_state: 'fresh' as const,
  min_check_interval_seconds: 60,
}

describe('SystemUpdateCard', () => {
  it('shows the current/latest version and can trigger a check now', async () => {
    const user = userEvent.setup()
    server.use(
      http.get(`${ORIGIN}/api/system/update-check`, () =>
        HttpResponse.json({
          ...base,
          state: 'update_available',
          latest: '1.1.0',
          published_at: '2026-10-20T08:00:00Z',
          notes_url: 'https://example.com/releases/1.1.0',
          notes: 'Release notes text',
          checked_at: '2026-10-22T08:00:00Z',
        }),
      ),
      http.post(`${ORIGIN}/api/system/update-check`, () =>
        HttpResponse.json({
          ...base,
          cached: false,
          cache_state: 'miss',
          state: 'up_to_date',
          latest: '1.1.0',
          checked_at: '2026-10-22T09:00:00Z',
        }),
      ),
    )

    await renderWithProviders(<SystemUpdateCard currentVersion="1.0.0" />, [
      'common',
      'settings',
    ])

    expect(await screen.findByText('A new version is available: 1.1.0')).toBeTruthy()
    expect(screen.getByTestId('system-update-latest').textContent).toBe('1.1.0')
    expect(screen.getByText('Release notes text')).toBeTruthy()
    expect(screen.getByRole('link', { name: 'View release notes' }).getAttribute('rel')).toBe(
      'noopener noreferrer',
    )

    await user.click(screen.getByTestId('system-update-check-now'))
    await waitFor(() => {
      expect(screen.getByTestId('system-update-state').textContent).toBe(
        'You are on the latest version.',
      )
    })
  })

  it('renders the disabled empty state without a check button action', async () => {
    server.use(
      http.get(`${ORIGIN}/api/system/update-check`, () =>
        HttpResponse.json({
          ...base,
          enabled: false,
          source_configured: false,
          cached: false,
          state: 'disabled',
        }),
      ),
    )

    await renderWithProviders(<SystemUpdateCard currentVersion="1.0.0" />, [
      'common',
      'settings',
    ])

    expect(await screen.findByText('Update check is off.')).toBeTruthy()
    expect(
      (screen.getByTestId('system-update-check-now') as HTMLButtonElement).disabled,
    ).toBe(true)
  })

  it('surfaces the SSRF rejection and keeps the last successful result', async () => {
    server.use(
      http.get(`${ORIGIN}/api/system/update-check`, () =>
        HttpResponse.json({
          ...base,
          cached: false,
          cache_state: 'miss',
          state: 'error',
          error: 'update source blocked by outbound security policy',
          error_kind: 'ssrf_blocked',
          checked_at: '2026-10-22T09:00:00Z',
          last_success: {
            latest: '1.0.0',
            checked_at: '2026-10-21T09:00:00Z',
          },
        }),
      ),
    )

    await renderWithProviders(<SystemUpdateCard currentVersion="1.0.0" />, [
      'common',
      'settings',
    ])

    expect(
      (await screen.findAllByText(
        'The target was blocked by the outbound security policy. For a private address, allow private addresses on this card.',
      )).length,
    ).toBeGreaterThan(0)
    expect(screen.getByText(/Last success: 1.0.0/)).toBeTruthy()
    // 更新源地址绝不出现在界面（响应本身也不含该字段）。
    expect(screen.queryByText(/10\.0\.0\./)).toBeNull()
  })

  it('counts down when the server rejects a second immediate check', async () => {
    const user = userEvent.setup()
    server.use(
      http.get(`${ORIGIN}/api/system/update-check`, () =>
        HttpResponse.json({
          ...base,
          state: 'up_to_date',
          latest: '1.0.0',
        }),
      ),
      http.post(
        `${ORIGIN}/api/system/update-check`,
        () =>
          HttpResponse.json(
            { type: 'about:blank', title: 'Update check rate limited', status: 429 },
            { status: 429, headers: { 'Retry-After': '60' } },
          ),
      ),
    )

    await renderWithProviders(<SystemUpdateCard currentVersion="1.0.0" />, [
      'common',
      'settings',
    ])
    await screen.findByText('You are on the latest version.')
    await user.click(screen.getByTestId('system-update-check-now'))

    expect(await screen.findByText(/Try again in (59|60) seconds\./)).toBeTruthy()
    expect(
      (screen.getByTestId('system-update-check-now') as HTMLButtonElement).disabled,
    ).toBe(true)
  })
})
