import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { useState } from 'react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'
import { ToastViewport, useToastStore } from '@/shared/ui/toast'

import type { SettingsItem, SettingsSectionResponse } from './api'
import { SettingsShell } from './settings-shell'
import type { SettingsTab } from './sections'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const ME_ADMIN = {
  username: 'admin',
  email: 'admin@example.com',
  role: 'admin',
  status: 'enabled',
  permissions: ['*'],
}

function item(
  key: string,
  value: unknown,
  overrides: Partial<SettingsItem> = {},
): SettingsItem {
  return {
    key,
    value,
    type: typeof value === 'number' ? 'int' : typeof value === 'boolean' ? 'bool' : 'string',
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

function sectionResponse(section: string, state: SectionState): SettingsSectionResponse {
  return { section, updated_at: state.updated_at, items: state.items }
}

const SITE_STATE: SectionState = {
  updated_at: '2026-10-19T10:00:00Z',
  items: [
    item('site.title', 'NewTG'),
    item('site.server_address', 'https://gateway.example.com'),
    item('site.footer_html', ''),
  ],
}

const NAV_STATE: SectionState = {
  updated_at: '2026-10-19T10:00:00Z',
  items: [
    item('nav.home.enabled', true),
    item('nav.console.enabled', true),
    item('nav.modules.console', {
      dashboard: true,
      tokens: true,
      usage_logs: true,
      wallet: true,
      profile: true,
      security: true,
    }),
  ],
}

const AUTH_STATE: SectionState = {
  updated_at: '2026-10-19T10:00:00Z',
  items: [
    item('auth.signup_enabled', true),
    item('auth.password_login_enabled', true),
  ],
}

const TURNSTILE_STATE: SectionState = {
  updated_at: '2026-10-19T10:00:00Z',
  items: [
    item('auth.turnstile.enabled', true),
    item('auth.turnstile.site_key', 'site-key'),
    item('auth.turnstile.secret_key', '******', { secret: true, default: null }),
  ],
}

const BILLING_STATE: SectionState = {
  updated_at: '2026-10-19T10:00:00Z',
  items: [
    item('billing.quota_per_unit', 100, { dangerous: true }),
    item('billing.display_unit', 'credits'),
  ],
}

const PAY_STATE: SectionState = {
  updated_at: '2026-10-19T10:00:00Z',
  items: [
    item('pay.online_enabled', true, { dangerous: true }),
    item('pay.amount_presets_cny', [10, 30]),
  ],
}

const PAY_STRIPE_STATE: SectionState = {
  updated_at: '2026-10-19T10:00:00Z',
  items: [item('pay.stripe.enabled', false)],
}

const PAY_ALIPAY_STATE: SectionState = {
  updated_at: '2026-10-19T10:00:00Z',
  items: [item('pay.alipay.enabled', false)],
}

const POLICY_STATE: SectionState = {
  updated_at: '2026-10-19T10:00:00Z',
  items: [
    item('rl.health.auto_disable_enabled', false, { dangerous: true }),
    item('rl.health.disable_keywords', ['foo', 'bar'], { dangerous: true }),
  ],
}

interface RequestLog {
  section: string
  body: Record<string, unknown> | undefined
}

/**
 * Installs the settings contract. `stores` is mutable so a PUT updates what
 * the next GET returns, mirroring the real section reload after a save.
 */
function installSettingsHandlers(
  stores: Record<string, SectionState>,
  requests: RequestLog[] = [],
): RequestLog[] {
  server.use(
    http.get(`${ORIGIN}/api/me`, () => HttpResponse.json(ME_ADMIN)),
    http.get(`${ORIGIN}/api/settings`, ({ request }) => {
      const section = new URL(request.url).searchParams.get('section') ?? ''
      const state = stores[section]
      if (state === undefined) {
        return HttpResponse.json(
          { type: 'about:blank', title: 'unknown section' },
          { status: 404 },
        )
      }
      return HttpResponse.json(sectionResponse(section, state))
    }),
    http.put(`${ORIGIN}/api/settings`, async ({ request }) => {
      const section = new URL(request.url).searchParams.get('section') ?? ''
      const body = (await request.json()) as {
        values?: Record<string, unknown>
        expect_updated_at?: string | null
      }
      requests.push({ section, body: body as Record<string, unknown> })
      const state = stores[section]
      if (state === undefined) {
        return HttpResponse.json({ type: 'about:blank', title: 'unknown section' }, { status: 404 })
      }
      for (const [key, value] of Object.entries(body.values ?? {})) {
        const existing = state.items.find((entry) => entry.key === key)
        if (existing !== undefined) existing.value = value
      }
      state.updated_at = '2026-10-19T11:00:00Z'
      return HttpResponse.json(sectionResponse(section, state))
    }),
  )
  return requests
}

function Harness({ initial = 'site' as SettingsTab }) {
  const [tab, setTab] = useState<SettingsTab>(initial)
  return <SettingsShell tab={tab} onTabChange={setTab} />
}

async function renderShell(tab: SettingsTab = 'site') {
  const requests: RequestLog[] = []
  installSettingsHandlers(
    {
      site: structuredClone(SITE_STATE),
      nav: structuredClone(NAV_STATE),
      auth: structuredClone(AUTH_STATE),
      'auth.turnstile': structuredClone(TURNSTILE_STATE),
      billing: structuredClone(BILLING_STATE),
      pay: structuredClone(PAY_STATE),
      'pay.stripe': structuredClone(PAY_STRIPE_STATE),
      'pay.alipay': structuredClone(PAY_ALIPAY_STATE),
      'rl.health': structuredClone(POLICY_STATE),
    },
    requests,
  )
  await renderWithProviders(
    <>
      <Harness initial={tab} />
      <ToastViewport />
    </>,
    ['common', 'settings'],
  )
  return requests
}

beforeEach(() => {
  server.resetHandlers()
  useToastStore.setState({ toasts: [] })
})

describe('SettingsShell contract', () => {
  it('loads the active tab sections and renders server values', async () => {
    await renderShell('site')

    expect(await screen.findByDisplayValue('NewTG')).toBeTruthy()
    expect(await screen.findByDisplayValue('https://gateway.example.com')).toBeTruthy()
    expect(screen.getByTestId('settings-section-site')).toBeTruthy()
    expect(screen.getByTestId('settings-section-nav')).toBeTruthy()
    expect(screen.getByRole('tab', { name: 'Site' }).getAttribute('aria-selected')).toBe('true')
  })

  it('marks the section dirty and saves only that section with the optimistic token', async () => {
    const user = userEvent.setup()
    const requests = await renderShell('site')

    const title = await screen.findByLabelText('Site title')
    await user.clear(title)
    await user.type(title, 'Token Gateway')
    const siteCard = screen.getByTestId('settings-section-site')
    const save = within(siteCard).getByRole('button', { name: 'Save changes' })
    await waitFor(() => expect((save as HTMLButtonElement).disabled).toBe(false))
    await user.click(save)

    await waitFor(() => expect(requests).toHaveLength(1))
    expect(requests[0]?.section).toBe('site')
    expect(requests[0]?.body?.values).toEqual({ 'site.title': 'Token Gateway' })
    expect(requests[0]?.body?.expect_updated_at).toBe('2026-10-19T10:00:00Z')
    // Contract v2 body shape: no confirmed_by/session members.
    expect(Object.keys(requests[0]?.body ?? {}).sort()).toEqual([
      'expect_updated_at',
      'section',
      'values',
    ])
    expect(await screen.findByText(/Basic information saved/)).toBeTruthy()
  })

  it('shows the conflict dialog on 409 and reloads the section on refresh', async () => {
    const user = userEvent.setup()
    let reads = 0
    await renderShell('site')
    server.use(
      http.get(`${ORIGIN}/api/settings`, ({ request }) => {
        const section = new URL(request.url).searchParams.get('section') ?? ''
        reads += section === 'site' ? 1 : 0
        return HttpResponse.json(
          sectionResponse(section, section === 'site' ? SITE_STATE : NAV_STATE),
        )
      }),
      http.put(`${ORIGIN}/api/settings`, () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'conflict' },
          { status: 409 },
        ),
      ),
    )

    const title = await screen.findByLabelText('Site title')
    await user.clear(title)
    await user.type(title, 'Changed')
    const siteCard = screen.getByTestId('settings-section-site')
    await user.click(within(siteCard).getByRole('button', { name: 'Save changes' }))

    expect(await screen.findByText('Settings changed elsewhere')).toBeTruthy()
    const before = reads
    await user.click(screen.getByRole('button', { name: 'Refresh' }))
    await waitFor(() => expect(reads).toBeGreaterThan(before))
  })

  it('shows field-level errors returned by the server', async () => {
    const user = userEvent.setup()
    await renderShell('site')
    server.use(
      http.put(`${ORIGIN}/api/settings`, () =>
        HttpResponse.json(
          {
            type: 'about:blank',
            title: 'validation failed',
            errors: [{ field: 'site.title', message: 'Title is reserved.' }],
          },
          { status: 400 },
        ),
      ),
    )

    const title = await screen.findByLabelText('Site title')
    await user.clear(title)
    await user.type(title, 'Reserved')
    const siteCard = screen.getByTestId('settings-section-site')
    await user.click(within(siteCard).getByRole('button', { name: 'Save changes' }))

    expect(await screen.findByText('Title is reserved.')).toBeTruthy()
  })

  it('renders the section error state when the read is forbidden', async () => {
    server.use(
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json(ME_ADMIN)),
      http.get(`${ORIGIN}/api/settings`, () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'forbidden' },
          { status: 403 },
        ),
      ),
    )
    await renderWithProviders(<Harness initial="site" />, ['common', 'settings'])

    expect(await screen.findAllByText('You do not have access to these settings.')).toHaveLength(2)
  })

  it('is read-only without settings:manage', async () => {
    server.use(
      http.get(`${ORIGIN}/api/me`, () =>
        HttpResponse.json({ ...ME_ADMIN, permissions: ['users:read'] }),
      ),
      http.get(`${ORIGIN}/api/settings`, ({ request }) => {
        const section = new URL(request.url).searchParams.get('section') ?? ''
        return HttpResponse.json(
          sectionResponse(section, section === 'site' ? SITE_STATE : NAV_STATE),
        )
      }),
    )
    await renderWithProviders(<Harness initial="site" />, ['common', 'settings'])

    expect(
      await screen.findByText(/your account lacks the settings:manage permission/i),
    ).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Save changes' })).toBeNull()
    expect((await screen.findByLabelText('Site title') as HTMLInputElement).disabled).toBe(true)
  })

  it('masks configured secrets and sends a secret only when a new value is typed', async () => {
    const user = userEvent.setup()
    const requests = await renderShell('auth')

    const secret = await screen.findByLabelText('Secret key')
    expect((secret as HTMLInputElement).value).toBe('')
    expect(screen.getByText('Configured')).toBeTruthy()

    await user.type(secret, 'new-secret')
    const card = screen.getByTestId('settings-section-auth.turnstile')
    await user.click(within(card).getByRole('button', { name: 'Save changes' }))

    await waitFor(() => expect(requests).toHaveLength(1))
    expect(requests[0]?.section).toBe('auth.turnstile')
    expect(requests[0]?.body?.values).toEqual({ 'auth.turnstile.secret_key': 'new-secret' })
  })

  it('guards tab navigation when a section is dirty', async () => {
    const user = userEvent.setup()
    await renderShell('site')

    const title = await screen.findByLabelText('Site title')
    await user.type(title, '!')
    await user.click(screen.getByRole('tab', { name: 'Billing' }))

    expect(await screen.findByText('You have unsaved changes')).toBeTruthy()
    expect(screen.getByRole('alertdialog')).toBeTruthy()
    await user.click(screen.getByRole('button', { name: 'Keep editing' }))
    expect(screen.queryByText('You have unsaved changes')).toBeNull()
  })

  it('does not validate a field that was edited and then reverted (F2)', async () => {
    const user = userEvent.setup()
    const requests = await renderShell('site')

    const title = await screen.findByLabelText('Site title')
    await user.type(title, 'Draft')
    await user.clear(title)
    await user.type(title, 'NewTG')
    const footer = screen.getByLabelText('Footer HTML')
    await user.type(footer, 'hello')

    const card = screen.getByTestId('settings-section-site')
    await user.click(within(card).getByRole('button', { name: 'Save changes' }))

    await waitFor(() => expect(requests).toHaveLength(1))
    expect(requests[0]?.body?.values).toEqual({ 'site.footer_html': 'hello' })
    expect(screen.queryByText('This field is required.')).toBeNull()
  })

  it('keeps raw text for number lists until save (F3)', async () => {
    const user = userEvent.setup()
    const requests = await renderShell('payment')

    const presets = await screen.findByLabelText('Top-up presets (CNY)')
    await user.clear(presets)
    await user.type(presets, '10,30,50')
    // The comma must still be in the input after typing.
    expect((presets as HTMLInputElement).value).toBe('10,30,50')

    const card = screen.getByTestId('settings-section-pay')
    await user.click(within(card).getByRole('button', { name: 'Save changes' }))

    await waitFor(() => expect(requests).toHaveLength(1))
    expect(requests[0]?.body?.values).toEqual({ 'pay.amount_presets_cny': [10, 30, 50] })
  })

  it('keeps newlines in line lists and asks for dangerous confirmation (F1/F3)', async () => {
    const user = userEvent.setup()
    const requests = await renderShell('policy')

    const keywords = await screen.findByLabelText('Disable keywords (one per line)')
    await user.clear(keywords)
    await user.type(keywords, 'foo{enter}bar{enter}baz')
    expect((keywords as HTMLTextAreaElement).value).toBe('foo\nbar\nbaz')

    const card = screen.getByTestId('settings-section-rl.health')
    await user.click(within(card).getByRole('button', { name: 'Save changes' }))

    expect(await screen.findByRole('alertdialog')).toBeTruthy()
    expect(requests).toHaveLength(0)
    await user.click(screen.getByRole('button', { name: 'Save anyway' }))

    await waitFor(() => expect(requests).toHaveLength(1))
    expect(requests[0]?.body?.values).toEqual({
      'rl.health.disable_keywords': ['foo', 'bar', 'baz'],
    })
    expect(requests[0]?.body?.dangerous_confirmed).toBe(true)
    expect(requests[0]?.body?.confirmed_by).toBeUndefined()
    expect(Object.keys(requests[0]?.body ?? {}).sort()).toEqual([
      'dangerous_confirmed',
      'expect_updated_at',
      'section',
      'values',
    ])
  })

  it('does not confirm the safe direction of a dangerous switch (F5)', async () => {
    const user = userEvent.setup()
    const stores = {
      site: structuredClone(SITE_STATE),
      nav: structuredClone(NAV_STATE),
      sec: {
        updated_at: '2026-10-19T10:00:00Z',
        items: [
          item('sec.ssrf.allow_private_ip', true, {
            dangerous: true,
            danger_direction: 'on',
          }),
        ],
      } satisfies SectionState,
    }
    const requests = installSettingsHandlers(stores)
    await renderWithProviders(
      <>
        <Harness initial="security" />
        <ToastViewport />
      </>,
      ['common', 'settings'],
    )

    const toggle = await screen.findByRole('switch', { name: 'Allow private addresses' })
    await user.click(toggle)
    const card = screen.getByTestId('settings-section-sec')
    await user.click(within(card).getByRole('button', { name: 'Save changes' }))

    await waitFor(() => expect(requests).toHaveLength(1))
    expect(screen.queryByRole('alertdialog')).toBeNull()
    expect(requests[0]?.body?.dangerous_confirmed).toBeUndefined()
  })

  it('sends dangerous_confirmed only for the server-declared risky direction (N1)', async () => {
    const user = userEvent.setup()
    const stores = {
      site: structuredClone(SITE_STATE),
      nav: structuredClone(NAV_STATE),
      pay: {
        updated_at: '2026-10-19T10:00:00Z',
        items: [
          item('pay.online_enabled', true, {
            dangerous: true,
            danger_direction: 'off',
          }),
          item('pay.amount_presets_cny', [10, 30]),
        ],
      } satisfies SectionState,
      'pay.stripe': structuredClone(PAY_STRIPE_STATE),
      'pay.alipay': structuredClone(PAY_ALIPAY_STATE),
    }
    const requests = installSettingsHandlers(stores)
    await renderWithProviders(
      <>
        <Harness initial="payment" />
        <ToastViewport />
      </>,
      ['common', 'settings'],
    )

    await user.click(await screen.findByRole('switch', { name: 'Online payments' }))
    const card = screen.getByTestId('settings-section-pay')
    await user.click(within(card).getByRole('button', { name: 'Save changes' }))

    // Risky direction: no PUT may leave the browser before the confirmation.
    expect(await screen.findByRole('alertdialog')).toBeTruthy()
    expect(requests).toHaveLength(0)
    expect(
      requests.filter((entry) => entry.body?.dangerous_confirmed !== true),
    ).toHaveLength(0)

    await user.click(screen.getByRole('button', { name: 'Save anyway' }))
    await waitFor(() => expect(requests).toHaveLength(1))
    expect(requests[0]?.body?.values).toEqual({ 'pay.online_enabled': false })
    expect(requests[0]?.body?.dangerous_confirmed).toBe(true)
  })

  it('shows 429 as a toast without leaking into field errors', async () => {
    const user = userEvent.setup()
    await renderShell('site')
    server.use(
      http.put(`${ORIGIN}/api/settings`, () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'rate limited' },
          { status: 429, headers: { 'Retry-After': '5' } },
        ),
      ),
    )

    const title = await screen.findByLabelText('Site title')
    await user.clear(title)
    await user.type(title, 'Changed')
    const card = screen.getByTestId('settings-section-site')
    await user.click(within(card).getByRole('button', { name: 'Save changes' }))

    expect(await screen.findByText('Too many requests. Please try again later.')).toBeTruthy()
  })

  it('retries a failed load', async () => {
    const user = userEvent.setup()
    let calls = 0
    server.use(
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json(ME_ADMIN)),
      http.get(`${ORIGIN}/api/settings`, () => {
        calls += 1
        return HttpResponse.json(
          { type: 'about:blank', title: 'Internal Server Error' },
          { status: 500 },
        )
      }),
    )
    await renderWithProviders(<Harness initial="site" />, ['common', 'settings'])

    expect((await screen.findAllByText(/Could not load this section/)).length).toBeGreaterThan(0)
    const before = calls
    await user.click(screen.getAllByRole('button', { name: 'Retry' })[0]!)
    await waitFor(() => expect(calls).toBeGreaterThan(before))
  })

  it('renders unknown registry keys with a generic control', async () => {
    const site = structuredClone(SITE_STATE)
    site.items.push(item('site.mystery', 'surprise'))
    installSettingsHandlers({
      site,
      nav: structuredClone(NAV_STATE),
    })
    await renderWithProviders(<Harness initial="site" />, ['common', 'settings'])

    expect(await screen.findByText('site.mystery')).toBeTruthy()
    expect(screen.getByDisplayValue('surprise')).toBeTruthy()
  })

  it('moves between tabs with arrow keys and only links the active panel', async () => {
    await renderShell('site')

    const siteTab = await screen.findByRole('tab', { name: 'Site' })
    expect(siteTab.getAttribute('aria-controls')).toBe('settings-panel-site')
    expect(
      screen.getByRole('tab', { name: 'Billing' }).getAttribute('aria-controls'),
    ).toBeNull()

    siteTab.focus()
    fireEvent.keyDown(screen.getByRole('tablist'), { key: 'ArrowRight' })
    await waitFor(() => {
      expect(
        screen.getByRole('tab', { name: 'Authentication' }).getAttribute('aria-selected'),
      ).toBe('true')
    })
  })

  it('registers a beforeunload guard while dirty', async () => {
    const user = userEvent.setup()
    await renderShell('site')

    const title = await screen.findByLabelText('Site title')
    await user.type(title, '!')
    const card = screen.getByTestId('settings-section-site')
    await waitFor(() => {
      expect(
        (within(card).getByRole('button', { name: 'Save changes' }) as HTMLButtonElement).disabled,
      ).toBe(false)
    })

    const event = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(event)
    expect(event.defaultPrevented).toBe(true)
  })

  it('blocks turning off both sign-in methods (F5)', async () => {
    const user = userEvent.setup()
    const requests = await renderShell('auth')

    await user.click(await screen.findByRole('switch', { name: 'Allow sign-up' }))
    await user.click(screen.getByRole('switch', { name: 'Allow password sign-in' }))
    const card = screen.getByTestId('settings-section-auth')
    await user.click(within(card).getByRole('button', { name: 'Save changes' }))

    expect(await screen.findByText('Keep at least one sign-in method enabled.')).toBeTruthy()
    expect(requests).toHaveLength(0)
  })

  it('blocks enabling Turnstile without a secret (F5)', async () => {
    const user = userEvent.setup()
    const stores = {
      auth: structuredClone(AUTH_STATE),
      'auth.turnstile': {
        updated_at: '2026-10-19T10:00:00Z',
        items: [
          item('auth.turnstile.enabled', false),
          item('auth.turnstile.site_key', 'site-key'),
          item('auth.turnstile.secret_key', null, { secret: true, default: null }),
        ],
      } satisfies SectionState,
    }
    const requests = installSettingsHandlers(stores)
    await renderWithProviders(
      <>
        <Harness initial="auth" />
        <ToastViewport />
      </>,
      ['common', 'settings'],
    )

    await user.click(await screen.findByRole('switch', { name: 'Enable Turnstile' }))
    const card = screen.getByTestId('settings-section-auth.turnstile')
    await user.click(within(card).getByRole('button', { name: 'Save changes' }))

    expect(
      await screen.findByText('Secret key is required when Turnstile is enabled.'),
    ).toBeTruthy()
    expect(requests).toHaveLength(0)
  })

  it('clears a secret by sending null and refetches to Not configured (F4)', async () => {
    const user = userEvent.setup()
    const confirm = vi.spyOn(globalThis, 'confirm').mockReturnValue(true)
    const requests = await renderShell('auth')

    expect(await screen.findByText('Configured')).toBeTruthy()
    // Turning Turnstile off first keeps the cross-field "enabled without a
    // secret" rule satisfied while the secret itself is cleared.
    await user.click(screen.getByRole('switch', { name: 'Enable Turnstile' }))
    await user.click(screen.getByRole('button', { name: 'Clear' }))
    expect(confirm).toHaveBeenCalled()

    const card = screen.getByTestId('settings-section-auth.turnstile')
    await user.click(within(card).getByRole('button', { name: 'Save changes' }))

    await waitFor(() => expect(requests).toHaveLength(1))
    expect(requests[0]?.body?.values).toEqual({
      'auth.turnstile.enabled': false,
      'auth.turnstile.secret_key': null,
    })
    expect(await screen.findByText('Not configured')).toBeTruthy()
  })
})
