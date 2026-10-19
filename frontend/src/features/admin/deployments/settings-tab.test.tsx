import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { SettingsTab } from './settings-tab'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const SETTINGS = {
  enabled: true,
  has_secret: true,
  can_connect: true,
  updated_at: '2026-10-19T10:00:00Z',
}

function installSettings(
  overrides: Record<string, unknown> = {},
  onPut?: (body: Record<string, unknown>) => void,
) {
  server.use(
    http.get(`${ORIGIN}/api/deployments/settings`, () =>
      HttpResponse.json({ ...SETTINGS, ...overrides }),
    ),
    http.put(`${ORIGIN}/api/deployments/settings`, async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>
      onPut?.(body)
      return HttpResponse.json({
        ...SETTINGS,
        enabled: body.enabled as boolean,
        has_secret: body.clear_api_key === true ? false : true,
        updated_at: '2026-10-19T10:01:00Z',
      })
    }),
  )
}

describe('deployment settings tab (F-08-51)', () => {
  it('confirms before disabling and sends only the changed fields', async () => {
    let putBody: Record<string, unknown> | null = null
    installSettings({}, (body) => {
      putBody = body
    })

    await renderWithProviders(<SettingsTab canManage />, ['common', 'admin'])
    const user = userEvent.setup()

    expect(await screen.findByText('Configured')).toBeTruthy()
    await user.click(screen.getByRole('switch', { name: 'Enable io.net deployments' }))
    await user.click(screen.getByRole('button', { name: 'Save changes' }))

    const dialog = await screen.findByRole('alertdialog')
    expect(
      within(dialog).getByText('Disable io.net deployments'),
    ).toBeTruthy()
    // 焦点落在取消上（危险确认默认）。
    expect(document.activeElement).toBe(
      within(dialog).getByRole('button', { name: 'Cancel' }),
    )
    await user.click(within(dialog).getByRole('button', { name: 'Save changes' }))

    await waitFor(() => expect(putBody).not.toBeNull())
    expect(putBody).toMatchObject({
      enabled: false,
      updated_at: '2026-10-19T10:00:00Z',
    })
    // 留空 = 不更新密钥。
    expect(putBody).not.toHaveProperty('api_key')
    expect(putBody).not.toHaveProperty('clear_api_key')
    expect(await screen.findByText('Settings saved.')).toBeTruthy()
  })

  it('requires confirmation before clearing the API key', async () => {
    let putBody: Record<string, unknown> | null = null
    installSettings({}, (body) => {
      putBody = body
    })

    await renderWithProviders(<SettingsTab canManage />, ['common', 'admin'])
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Clear' }))
    const dialog = await screen.findByRole('alertdialog')
    expect(within(dialog).getByText('Clear API key')).toBeTruthy()
    await user.click(within(dialog).getByRole('button', { name: 'Clear' }))
    await user.click(screen.getByRole('button', { name: 'Save changes' }))

    await waitFor(() => expect(putBody).not.toBeNull())
    expect(putBody).toMatchObject({ enabled: true, clear_api_key: true })
  })

  it('shows connection test results inline and respects Retry-After', async () => {
    installSettings()
    let calls = 0
    server.use(
      http.post(`${ORIGIN}/api/deployments/settings/test-connection`, () => {
        calls += 1
        if (calls === 1) {
          return HttpResponse.json(
            {
              type: 'https://newtg.dev/problems/deploy_rate_limited',
              title: 'Too many requests',
              detail: 'connection test is limited',
            },
            { status: 429, headers: { 'Retry-After': '42' } },
          )
        }
        return HttpResponse.json({
          ok: true,
          hardware_count: 32,
          total_available: 128,
          latency_ms: 320,
        })
      }),
    )

    await renderWithProviders(<SettingsTab canManage />, ['common', 'admin'])
    const user = userEvent.setup()

    await user.click(await screen.findByRole('button', { name: 'Test connection' }))
    expect(await screen.findByText('Too many connection tests. Try again later.')).toBeTruthy()
    const countdownButton = screen.getByRole('button', { name: /Try again in 4[12]s/ })
    expect(countdownButton.hasAttribute('disabled')).toBe(true)
  })

  it('renders the egress note and disables writes without permission', async () => {
    installSettings()
    await renderWithProviders(<SettingsTab canManage={false} />, ['common', 'admin'])

    expect(await screen.findByText('api.io.solutions:443')).toBeTruthy()
    expect(
      screen.getByText(/No inbound listener; no provider callbacks/),
    ).toBeTruthy()
    expect(screen.getByRole('switch').hasAttribute('disabled')).toBe(true)
    expect(screen.queryByRole('button', { name: 'Save changes' })).toBeNull()
  })
})
