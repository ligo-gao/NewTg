import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor, within } from '@testing-library/react'
import { useState } from 'react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

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
    await user.click(screen.getByRole('button', { name: 'Keep editing' }))
    expect(screen.queryByText('You have unsaved changes')).toBeNull()
  })
})
