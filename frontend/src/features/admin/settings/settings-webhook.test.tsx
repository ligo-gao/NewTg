import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import type { SettingsItem, SettingsSectionResponse } from './api'
import { tabConfig } from './sections'
import { SettingsSectionForm } from './settings-section-form'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  vi.restoreAllMocks()
  server.resetHandlers()
})
afterAll(() => server.close())

/**
 * The settings transport reads the CSRF cookie through `document.cookie`.
 * jsdom refuses real `__Host-` cookies over http, so every case gets a
 * controllable reader; CSRF-specific cases mutate `csrfToken`.
 */
let csrfToken: string | undefined

beforeEach(() => {
  csrfToken = 'stale-token'
  vi.spyOn(document, 'cookie', 'get').mockImplementation(() =>
    csrfToken === undefined ? '' : `__Host-nkt_csrf=${csrfToken}`,
  )
})

const CONFIG = tabConfig('notify').sections.find((entry) => entry.id === 'notify.webhook')!

function item(key: string, value: unknown, overrides: Partial<SettingsItem> = {}): SettingsItem {
  return {
    key,
    value,
    type: typeof value === 'boolean' ? 'bool' : Array.isArray(value) ? 'list' : 'string',
    default: value,
    public: false,
    secret: false,
    dangerous: false,
    ...overrides,
  }
}

interface SectionState {
  updated_at: string | null
  items: SettingsItem[]
}

function webhookState(overrides: Partial<Record<string, unknown>> = {}): SectionState {
  const values: Record<string, unknown> = {
    'notify.webhook.enabled': false,
    'notify.webhook.url': 'https://hooks.example.com/notify',
    'notify.webhook.secret': '******',
    'notify.webhook.events': ['balance_low'],
    ...overrides,
  }
  return {
    updated_at: '2026-10-19T10:00:00Z',
    items: [
      item('notify.webhook.enabled', values['notify.webhook.enabled']),
      item('notify.webhook.url', values['notify.webhook.url']),
      item('notify.webhook.secret', values['notify.webhook.secret'], {
        secret: true,
        default: null,
      }),
      item('notify.webhook.events', values['notify.webhook.events'], {
        type: 'list',
        default: ['balance_low'],
      }),
    ],
  }
}

interface SaveLog {
  body: Record<string, unknown> | undefined
}

interface Harness {
  state: SectionState
  saves: SaveLog[]
  tests: number
  /** `X-CSRF-Token` seen by each test-endpoint POST, in order. */
  testCsrf: Array<string | null>
}

/**
 * Installs the frozen settings + webhook-test contract. `onTest` lets a case
 * override the test endpoint answer without touching the section handlers.
 */
function installHandlers(
  state: SectionState = webhookState(),
  onTest?: () => Response | Promise<Response>,
): Harness {
  const harness: Harness = { state, saves: [], tests: 0, testCsrf: [] }

  server.use(
    http.get(`${ORIGIN}/api/settings`, ({ request }) => {
      const section = new URL(request.url).searchParams.get('section')
      if (section !== 'notify.webhook') {
        return HttpResponse.json({ type: 'about:blank', title: 'unknown section' }, { status: 404 })
      }
      return HttpResponse.json<SettingsSectionResponse>({
        section,
        updated_at: state.updated_at,
        items: state.items,
      })
    }),
    http.put(`${ORIGIN}/api/settings`, async ({ request }) => {
      const body = (await request.json()) as {
        values?: Record<string, unknown>
        expect_updated_at?: string | null
      }
      harness.saves.push({ body: body as Record<string, unknown> })
      for (const [key, value] of Object.entries(body.values ?? {})) {
        const existing = state.items.find((entry) => entry.key === key)
        if (existing !== undefined) existing.value = value
      }
      state.updated_at = '2026-10-19T11:00:00Z'
      return HttpResponse.json<SettingsSectionResponse>({
        section: 'notify.webhook',
        updated_at: state.updated_at,
        items: state.items,
      })
    }),
    http.post(`${ORIGIN}/api/settings/test/webhook`, ({ request }) => {
      harness.tests += 1
      harness.testCsrf.push(request.headers.get('X-CSRF-Token'))
      if (onTest !== undefined) return onTest()
      return HttpResponse.json({ ok: true, status_code: 200 })
    }),
  )

  return harness
}

async function renderForm(readOnly = false, harness: Harness = installHandlers()) {
  const onDirtyChange = vi.fn()
  await renderWithProviders(
    <SettingsSectionForm
      config={CONFIG}
      readOnly={readOnly}
      onDirtyChange={onDirtyChange}
    />,
    ['settings'],
  )
  await screen.findByTestId('settings-section-notify.webhook')
  return { harness, onDirtyChange }
}

describe('Webhook settings section', () => {
  it('loads saved values and masks the configured secret', async () => {
    await renderForm()

    expect(await screen.findByDisplayValue('https://hooks.example.com/notify')).toBeTruthy()
    const secret = screen.getByLabelText('Signing secret') as HTMLInputElement
    expect(secret.value).toBe('')
    expect(screen.getByText('Configured')).toBeTruthy()
    expect((screen.getByRole('checkbox', { name: 'Low balance' }) as HTMLInputElement).checked).toBe(
      true,
    )
    expect(
      (within(screen.getByTestId('settings-section-notify.webhook')).getByRole('button', {
        name: 'Save changes',
      }) as HTMLButtonElement).disabled,
    ).toBe(true)
  })

  it('shows server field errors from a 422 on the matching field', async () => {
    const user = userEvent.setup()
    // The overriding handler replaces the harness PUT, so `harness.saves`
    // would stay empty whether or not the request was sent. Count attempts
    // inside the handler that actually answers instead.
    let saveAttempts = 0
    await renderForm()
    server.use(
      http.put(`${ORIGIN}/api/settings`, () => {
        saveAttempts += 1
        return HttpResponse.json(
          {
            type: 'validation_failed',
            title: 'validation failed',
            errors: [
              {
                field: 'notify.webhook.url',
                message: 'must use https when webhook is enabled',
              },
            ],
          },
          { status: 422 },
        )
      }),
    )

    const url = await screen.findByLabelText('Endpoint URL')
    await user.clear(url)
    await user.type(url, 'https://hooks.example.com/v2')
    await user.click(screen.getByRole('button', { name: 'Save changes' }))

    expect(await screen.findByText('must use https when webhook is enabled')).toBeTruthy()
    expect(saveAttempts).toBe(1)
  })

  it('leaves an untouched secret out of the payload when saving other fields', async () => {
    const user = userEvent.setup()
    const { harness } = await renderForm()

    await user.click(await screen.findByRole('checkbox', { name: 'Low balance' }))
    await user.click(within(screen.getByTestId('settings-section-notify.webhook')).getByRole('button', {
      name: 'Save changes',
    }))

    await waitFor(() => expect(harness.saves).toHaveLength(1))
    expect(harness.saves[0]?.body?.values).toEqual({ 'notify.webhook.events': [] })
    expect(
      (harness.saves[0]?.body?.values as Record<string, unknown>)['notify.webhook.secret'],
    ).toBeUndefined()
  })

  it('warns that an empty event subscription delivers nothing', async () => {
    const user = userEvent.setup()
    await renderForm()

    expect(
      screen.queryByText('No events subscribed. Nothing will be delivered.'),
    ).toBeNull()
    await user.click(await screen.findByRole('checkbox', { name: 'Low balance' }))
    expect(
      await screen.findByText('No events subscribed. Nothing will be delivered.'),
    ).toBeTruthy()

    await user.click(screen.getByRole('checkbox', { name: 'Payment succeeded' }))
    expect(
      screen.queryByText('No events subscribed. Nothing will be delivered.'),
    ).toBeNull()
  })

  it('clears the secret by sending null', async () => {
    const user = userEvent.setup()
    vi.spyOn(globalThis, 'confirm').mockReturnValue(true)
    const { harness } = await renderForm()

    await user.click(await screen.findByRole('button', { name: 'Clear' }))
    await user.click(within(screen.getByTestId('settings-section-notify.webhook')).getByRole('button', {
      name: 'Save changes',
    }))

    await waitFor(() => expect(harness.saves).toHaveLength(1))
    expect(harness.saves[0]?.body?.values).toEqual({ 'notify.webhook.secret': null })
    expect(await screen.findByText('Not configured')).toBeTruthy()
  })

  it('reports a successful test event with the target status code', async () => {
    const user = userEvent.setup()
    const { harness } = await renderForm(false)

    await user.click(await screen.findByRole('button', { name: 'Send test event' }))

    expect(await screen.findByText('Endpoint responded with 200.')).toBeTruthy()
    expect(harness.tests).toBe(1)
    // The test action never dirties the settings draft.
    expect(
      (within(screen.getByTestId('settings-section-notify.webhook')).getByRole('button', {
        name: 'Save changes',
      }) as HTMLButtonElement).disabled,
    ).toBe(true)
  })

  it('reports a non-2xx target response from the 200 test result', async () => {
    const user = userEvent.setup()
    await renderForm(false, installHandlers(webhookState(), () =>
      HttpResponse.json({ ok: false, status_code: 500 }),
    ))

    await user.click(await screen.findByRole('button', { name: 'Send test event' }))
    expect(await screen.findByText('Endpoint responded with 500.')).toBeTruthy()
  })

  it('maps a 504 transport failure to the timeout message', async () => {
    const user = userEvent.setup()
    await renderForm(false, installHandlers(webhookState(), () =>
      HttpResponse.json(
        {
          type: 'gateway_timeout',
          title: 'Webhook request failed',
          detail: 'the endpoint did not return an HTTP response in time',
        },
        { status: 504 },
      ),
    ))

    await user.click(await screen.findByRole('button', { name: 'Send test event' }))
    expect(await screen.findByText('The endpoint did not respond in time.')).toBeTruthy()
  })

  it('maps webhook_not_configured to the configuration message', async () => {
    const user = userEvent.setup()
    await renderForm(false, installHandlers(webhookState(), () =>
      HttpResponse.json(
        {
          type: 'https://newtg.dev/problems/webhook_not_configured',
          title: 'Webhook is not configured',
          detail: 'configure notify.webhook.url and notify.webhook.secret before testing',
        },
        { status: 422 },
      ),
    ))

    await user.click(await screen.findByRole('button', { name: 'Send test event' }))

    expect(
      await screen.findByText('Save the endpoint URL and signing secret before testing.'),
    ).toBeTruthy()
    // Fixed copy only: the internal detail is never rendered.
    expect(screen.queryByText(/configure notify\.webhook/)).toBeNull()
  })

  it('maps webhook_target_blocked to the outbound-target message', async () => {
    const user = userEvent.setup()
    await renderForm(false, installHandlers(webhookState(), () =>
      HttpResponse.json(
        {
          type: 'https://newtg.dev/problems/webhook_target_blocked',
          title: 'Webhook target is not allowed',
          detail: 'the configured URL is not an allowed outbound target',
        },
        { status: 422 },
      ),
    ))

    await user.click(await screen.findByRole('button', { name: 'Send test event' }))

    expect(
      await screen.findByText(
        'The saved endpoint is not an allowed outbound target. Change the URL or allow this host, then try again.',
      ),
    ).toBeTruthy()
    // The blocked message must not be conflated with "not configured": the
    // administrator's next action is different (change target vs. fill in).
    expect(
      screen.queryByText('Save the endpoint URL and signing secret before testing.'),
    ).toBeNull()
    expect(screen.queryByText(/not an allowed outbound target$/)).toBeNull()
  })

  it('reports a transport TypeError as offline', async () => {
    const user = userEvent.setup()
    await renderForm(false, installHandlers(webhookState(), () => HttpResponse.error()))

    await user.click(await screen.findByRole('button', { name: 'Send test event' }))

    // A rejected fetch must be normalized to a status-0 Problem, otherwise
    // the panel falls into the generic "Test failed: …" branch.
    expect(
      await screen.findByText('You are offline. Check your connection and try again.'),
    ).toBeTruthy()
  })

  it('refreshes the CSRF cookie and replays the test exactly once', async () => {
    const user = userEvent.setup()
    let postCalls = 0
    let meCalls = 0
    const sentTokens: Array<string | null> = []
    await renderForm(false, installHandlers(webhookState()))
    server.use(
      http.get(`${ORIGIN}/api/me`, () => {
        meCalls += 1
        csrfToken = 'fresh-token'
        return HttpResponse.json({ id: 'me' })
      }),
      http.post(`${ORIGIN}/api/settings/test/webhook`, ({ request }) => {
        postCalls += 1
        sentTokens.push(request.headers.get('X-CSRF-Token'))
        if (postCalls === 1) {
          return HttpResponse.json(
            {
              type: 'https://newtg.dev/problems/csrf_failed',
              title: 'CSRF validation failed',
            },
            { status: 403 },
          )
        }
        return HttpResponse.json({ ok: true, status_code: 200 })
      }),
    )

    await user.click(await screen.findByRole('button', { name: 'Send test event' }))

    expect(await screen.findByText('Endpoint responded with 200.')).toBeTruthy()
    expect(postCalls).toBe(2)
    expect(meCalls).toBe(1)
    expect(sentTokens).toEqual(['stale-token', 'fresh-token'])
  })

  it('sends X-CSRF-Token on the test POST', async () => {
    const user = userEvent.setup()
    const { harness } = await renderForm(false)

    await user.click(await screen.findByRole('button', { name: 'Send test event' }))

    await waitFor(() => expect(harness.tests).toBe(1))
    expect(harness.testCsrf).toEqual(['stale-token'])
  })

  it('starts an inline countdown from Retry-After on 429', async () => {
    const user = userEvent.setup()
    await renderForm(false, installHandlers(webhookState(), () =>
      HttpResponse.json(
        { type: 'rate_limited', title: 'Too many requests' },
        { status: 429, headers: { 'Retry-After': '5' } },
      ),
    ))

    await user.click(await screen.findByRole('button', { name: 'Send test event' }))

    expect(
      await screen.findByText('Too many test events. Please wait before trying again.'),
    ).toBeTruthy()
    // The countdown must use the server's Retry-After verbatim, not a default.
    const retry = await screen.findByRole('button', { name: 'Retry in 5s' })
    expect((retry as HTMLButtonElement).disabled).toBe(true)
  })

  it('disables the test button until an endpoint URL is saved', async () => {
    const user = userEvent.setup()
    const { harness } = await renderForm(
      false,
      installHandlers(webhookState({ 'notify.webhook.url': '' })),
    )

    expect(await screen.findByText('Save an endpoint URL before sending a test.')).toBeTruthy()
    const button = screen.getByRole('button', { name: 'Send test event' }) as HTMLButtonElement
    expect(button.disabled).toBe(true)
    await user.click(button)
    expect(harness.tests).toBe(0)
  })

  it('rejects an http endpoint locally before any request (ruling 08-3)', async () => {
    const user = userEvent.setup()
    const { harness } = await renderForm()

    const url = await screen.findByLabelText('Endpoint URL')
    await user.clear(url)
    await user.type(url, 'http://hooks.example.com/notify')
    await user.click(screen.getByRole('button', { name: 'Save changes' }))

    expect(await screen.findByText('Enter an absolute https URL.')).toBeTruthy()
    expect(harness.saves).toHaveLength(0)
  })

  it('enables the webhook when URL and secret are already configured (D2-11)', async () => {
    const user = userEvent.setup()
    const { harness } = await renderForm()

    // The saved secret is masked; the untouched field must not be re-sent.
    await user.click(await screen.findByRole('switch', { name: 'Enable webhook' }))
    await user.click(within(screen.getByTestId('settings-section-notify.webhook')).getByRole('button', {
      name: 'Save changes',
    }))

    await waitFor(() => expect(harness.saves).toHaveLength(1))
    expect(harness.saves[0]?.body?.values).toEqual({ 'notify.webhook.enabled': true })
    expect(harness.saves[0]?.body?.expect_updated_at).toBe('2026-10-19T10:00:00Z')
  })

  it('blocks an https URL with userinfo or a fragment (client mirrors the guard)', async () => {
    const user = userEvent.setup()
    const { harness } = await renderForm()

    const url = await screen.findByLabelText('Endpoint URL')
    for (const candidate of [
      'https://user:pass@hooks.example.com/notify',
      'https://hooks.example.com/notify#fragment',
    ]) {
      await user.clear(url)
      await user.type(url, candidate)
      await user.click(screen.getByRole('button', { name: 'Save changes' }))
      expect(
        await screen.findByText('Enter an https URL without credentials or a fragment.'),
      ).toBeTruthy()
    }

    expect(harness.saves).toHaveLength(0)
  })

  it('counts URL length in code points, matching the server rune limit', async () => {
    const { harness } = await renderForm()

    // 900 supplementary-plane characters are 1800 UTF-16 units but only 900
    // runes; the whole URL is 926 runes, which the server accepts. A UTF-16
    // length check would reject it.
    const url = (await screen.findByLabelText('Endpoint URL')) as HTMLInputElement
    fireEvent.change(url, {
      target: { value: `https://hooks.example.com/${'😀'.repeat(900)}` },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Save changes' }))

    await waitFor(() => expect(harness.saves).toHaveLength(1))
    expect(screen.queryByText('At most 1024 characters.')).toBeNull()
  })

  it('blocks enabling without an endpoint URL and signing secret (D2-11)', async () => {
    const user = userEvent.setup()
    const { harness } = await renderForm(
      false,
      installHandlers(
        webhookState({ 'notify.webhook.url': '', 'notify.webhook.secret': null }),
      ),
    )

    await user.click(await screen.findByRole('switch', { name: 'Enable webhook' }))
    await user.click(within(screen.getByTestId('settings-section-notify.webhook')).getByRole('button', {
      name: 'Save changes',
    }))

    expect(await screen.findByText('An endpoint URL is required when the webhook is enabled.')).toBeTruthy()
    expect(await screen.findByText('A signing secret is required when the webhook is enabled.')).toBeTruthy()
    expect(harness.saves).toHaveLength(0)
  })

  it('hides every write control without settings:manage', async () => {
    await renderForm(true)

    expect(await screen.findByDisplayValue('https://hooks.example.com/notify')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Send test event' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Save changes' })).toBeNull()
    expect((screen.getByLabelText('Endpoint URL') as HTMLInputElement).disabled).toBe(true)
  })
})
