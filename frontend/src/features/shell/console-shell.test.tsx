import { QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { render, screen, waitFor, within } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
} from 'vitest'

import { I18nProvider } from '@/app/providers/i18n'
import { createQueryClient } from '@/app/providers/query'
import { routeTree } from '@/routeTree.gen'
import { renderWithProviders } from '@/test-support/providers'

import {
  ConsoleWideScreenGate,
  readCollapsed,
  SIDEBAR_STORAGE_KEY,
} from './console-shell'

const ORIGIN = 'http://localhost:3000'

const originalMatchMedia = window.matchMedia

function installMatchMedia(matches: (query: string) => boolean): void {
  window.matchMedia = ((query: string) => ({
    matches: matches(query),
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  })) as unknown as typeof window.matchMedia
}

const server = setupServer()

const PUBLIC_STATUS = {
  system_initialized: true,
  auth: {
    signup_enabled: true,
    password_login_enabled: true,
    email_verification_required: false,
    turnstile: { enabled: false, site_key: '' },
  },
  password_login_encryption_enabled: false,
  languages: ['en'],
  default_language: 'en',
  nav: { modules: { chat: { playground: true } } },
}

const SUMMARY = {
  cost_total: 0,
  request_count: 0,
  success_rate: 0,
  prompt_tokens_total: 0,
  completion_tokens_total: 0,
  pre_deducted_total: 0,
  refunded_total: 0,
}

const RATES = {
  rpm_avg: 0,
  rpm_peak: 0,
  tpm_avg: 0,
  tpm_peak: 0,
  window_minutes: 0,
}

const MODEL_PERF = {
  window: { range: '24h', start: '', end: '', granularity: '1m' },
  buckets: [],
  summary: {},
  incidents: [],
  models: [],
  pipeline: { enabled: false, updated_at: null, stale: true },
  comparison: null,
}

beforeAll(() => {
  server.listen({ onUnhandledRequest: 'error' })
  window.scrollTo = () => {}
})
beforeEach(() => {
  window.localStorage.clear()
})
afterEach(() => {
  server.resetHandlers()
  // The viewport stub and the stored preference are per-test state.
  window.matchMedia = originalMatchMedia
  window.localStorage.clear()
})
afterAll(() => server.close())

function installSession(
  options: { notifications?: boolean; announcements?: boolean } = {},
): void {
  const withNotifications = options.notifications ?? true
  const withAnnouncements = options.announcements ?? true
  server.use(
    ...(withNotifications
      ? [
          http.get(`${ORIGIN}/api/notifications/unread-count`, () =>
            HttpResponse.json({ unread: 0 }),
          ),
          http.get(`${ORIGIN}/api/notifications`, () =>
            HttpResponse.json({
              items: [],
              total: 0,
              page: 1,
              page_size: 100,
            }),
          ),
        ]
      : []),
    ...(withAnnouncements
      ? [
          http.get(`${ORIGIN}/api/announcements/active`, () =>
            HttpResponse.json({ items: [] }),
          ),
        ]
      : []),
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({
        username: 'alice',
        role: 'user',
        status: 'enabled',
        permissions: [],
      }),
    ),
    http.get(`${ORIGIN}/api/usage-stats/summary`, () =>
      HttpResponse.json(SUMMARY),
    ),
    http.get(`${ORIGIN}/api/usage-stats/by-model`, () => HttpResponse.json([])),
    http.get(`${ORIGIN}/api/usage-stats/timeseries`, () => HttpResponse.json([])),
    http.get(`${ORIGIN}/api/usage-stats/rates`, () => HttpResponse.json(RATES)),
    http.get(`${ORIGIN}/api/model-perf`, () => HttpResponse.json(MODEL_PERF)),
    http.get(`${ORIGIN}/api/self`, () =>
      HttpResponse.json({
        language: 'en',
        sidebar_modules: null,
        record_ip_log: false,
        default_model: null,
        updated_at: '2026-10-01T00:00:00Z',
      }),
    ),
    http.get(`${ORIGIN}/api/wallet/summary`, () =>
      HttpResponse.json({
        balance: 0,
        held_credits: 0,
        total_consumed: 0,
        total_requests: 0,
        threshold: 1000,
        health: 'healthy',
        group: 'default',
      }),
    ),
    http.get(`${ORIGIN}/api/status`, () =>
      HttpResponse.json({ ...PUBLIC_STATUS, usable_groups: [] }),
    ),
  )
}

function renderConsoleAt(path: string) {
  const queryClient = createQueryClient()
  const router = createRouter({
    routeTree,
    context: { queryClient },
    history: createMemoryHistory({ initialEntries: [path] }),
    scrollRestoration: false,
  })
  return {
    ...render(
      <QueryClientProvider client={queryClient}>
        <I18nProvider modules={['common']}>
          <RouterProvider router={router} />
        </I18nProvider>
      </QueryClientProvider>,
    ),
    router,
  }
}

describe('ConsoleWideScreenGate', () => {
  it('explains the wide-screen requirement', async () => {
    const { getByRole, getByText } = await renderWithProviders(
      <ConsoleWideScreenGate />,
      ['common'],
    )

    expect(getByRole('heading', { level: 1 }).textContent).toBe(
      'Use a wide screen',
    )
    expect(
      getByText(/designed for wide screens/i),
    ).toBeTruthy()
  })
})

describe('readCollapsed', () => {
  it('returns a stored collapsed preference regardless of the viewport', () => {
    window.localStorage.setItem(SIDEBAR_STORAGE_KEY, 'collapsed')
    installMatchMedia(() => false)

    expect(readCollapsed()).toBe(true)
  })

  it('returns a stored expanded preference in the 1024–1279px band', () => {
    window.localStorage.setItem(SIDEBAR_STORAGE_KEY, 'expanded')
    installMatchMedia((query) => query === '(max-width: 1279px)')

    expect(readCollapsed()).toBe(false)
  })

  it('collapses by default only while the narrow query matches', () => {
    installMatchMedia((query) => query === '(max-width: 1279px)')
    expect(readCollapsed()).toBe(true)

    installMatchMedia(() => false)
    expect(readCollapsed()).toBe(false)
  })
})

describe('ConsoleShell wide-screen gate', () => {
  it('renders the gate, not the shell, below 1024px', async () => {
    installSession()
    installMatchMedia((query) => query === '(max-width: 1279px)')

    const { container } = renderConsoleAt('/dashboard')

    expect(
      await screen.findByRole('heading', { name: 'Use a wide screen' }),
    ).toBeTruthy()
    expect(container.querySelector('aside')).toBeNull()
    expect(container.querySelector('nav')).toBeNull()
    expect(container.querySelector('#main')).toBeNull()
  })

  it('renders the sidebar and the page content at 1024px and wider', async () => {
    installSession()
    installMatchMedia((query) => query === '(min-width: 1024px)')

    const { container } = renderConsoleAt('/dashboard')

    await waitFor(() => {
      expect(container.querySelector('aside')).not.toBeNull()
    })
    expect(container.querySelector('#main')).not.toBeNull()
    expect(
      screen.queryByRole('heading', { name: 'Use a wide screen' }),
    ).toBeNull()
  })

  it('renders a compact topbar without the marketing navigation', async () => {
    installSession()
    installMatchMedia((query) => query === '(min-width: 1024px)')

    const { container } = renderConsoleAt('/dashboard')

    await waitFor(() => {
      expect(container.querySelector('aside')).not.toBeNull()
    })

    // The console owns a single header; the marketing navigation (brand,
    // Models/Pricing/Docs links) is gone.
    expect(
      screen.queryByRole('navigation', { name: 'Primary navigation' }),
    ).toBeNull()
    expect(screen.queryByRole('link', { name: 'NEWTG' })).toBeNull()

    const headers = container.querySelectorAll('header')
    expect(headers.length).toBe(1)
    const topbar = headers[0] as HTMLElement
    expect(
      within(topbar).getByRole('link', { name: 'Back to home' }).getAttribute(
        'href',
      ),
    ).toBe('/')
    expect(within(topbar).getByLabelText('Interface language')).toBeTruthy()
    expect(within(topbar).getByLabelText('Theme')).toBeTruthy()
  })

  it('does not mount blocked widgets on the forced password-change route', async () => {
    // A restricted (temporary-password) session may only use /api/me and
    // /api/password. Notifications and announcements answer 403, and the auth
    // failure handler would hard-navigate back to this same route — the reload
    // loop this test guards against. Leaving their handlers out makes any
    // request a hard msw error.
    installSession({ notifications: false, announcements: false })
    installMatchMedia((query) => query === '(min-width: 1024px)')

    renderConsoleAt('/security?force=password')

    expect(
      await screen.findByRole('heading', {
        level: 2,
        name: 'Set a new password',
      }),
    ).toBeTruthy()
    expect(
      screen.queryByRole('button', { name: /^Notifications/ }),
    ).toBeNull()
  })
})
