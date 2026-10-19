import { QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import {
  afterAll,
  afterEach,
  beforeAll,
  describe,
  expect,
  it,
  vi,
} from 'vitest'

import { ApiError } from '@/api/errors'
import { ErrorFallback } from '@/app/error-boundary'
import { NotFoundPage, NotFoundPageContent } from '@/app/not-found-page'
import {
  ErrorStatusPage,
  ServerErrorPage,
  ServiceUnavailablePage,
} from '@/app/error-status-page'
import { I18nProvider } from '@/app/providers/i18n'
import { createQueryClient } from '@/app/providers/query'
import { routeTree } from '@/routeTree.gen'
import { useToastStore } from '@/shared/ui/toast'
import { renderWithProviders } from '@/test-support/providers'

const ORIGIN = 'http://localhost:3000'

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
}

const server = setupServer()

beforeAll(() => {
  server.listen({ onUnhandledRequest: 'error' })
  // jsdom implements no layout; the router's scroll manager calls `scrollTo`.
  window.scrollTo = () => {}
})
afterEach(() => {
  server.resetHandlers()
  globalThis.localStorage?.clear()
  useToastStore.setState({ toasts: [] })
  Object.defineProperty(navigator, 'onLine', { configurable: true, value: true })
})
afterAll(() => server.close())

function statusHandler() {
  server.use(http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(PUBLIC_STATUS)))
}

function meUnauthorized() {
  server.use(
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json(
        { type: 'about:blank', title: 'Unauthorized' },
        { status: 401 },
      ),
    ),
  )
}

describe('error page shell', () => {
  it('401 renders the shared skeleton and sanitizes the redirect', async () => {
    const first = await renderWithProviders(<ErrorStatusPage status={401} />)

    expect(first.getByRole('heading', { level: 1 }).textContent).toBe(
      'Your session has expired',
    )
    expect(
      first.getByRole('link', { name: 'Sign in' }).getAttribute('href'),
    ).toBe('/sign-in')
    // 401/403 是确定性结果：不得出现重试按钮。
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull()
    first.unmount()

    await renderWithProviders(
      <ErrorStatusPage status={401} redirect="//evil.example" />,
    )
    expect(
      screen.getByRole('link', { name: 'Sign in' }).getAttribute('href'),
    ).toBe(`/sign-in?redirect=${encodeURIComponent('/dashboard')}`)
  })

  it('403 offers "go back" only when history exists', async () => {
    statusHandler()
    server.use(
      http.get(`${ORIGIN}/api/me`, () =>
        HttpResponse.json({ username: 'alice', role: 'user', status: 'enabled' }),
      ),
    )
    const { getByRole, unmount } = await renderWithProviders(
      <ErrorStatusPage status={403} canGoBack={false} />,
    )
    expect(getByRole('heading', { level: 1 }).textContent).toBe(
      'Access forbidden',
    )
    expect(getByRole('link', { name: 'Open console' }).getAttribute('href')).toBe(
      '/dashboard',
    )
    expect(screen.queryByRole('button', { name: 'Go back' })).toBeNull()
    unmount()

    await renderWithProviders(<ErrorStatusPage status={403} canGoBack />)
    expect(screen.getByRole('button', { name: 'Go back' })).toBeTruthy()
  })

  it('403 leaves for sign-in when the session turns 401 while mounted', async () => {
    meUnauthorized()
    const onSessionExpired = vi.fn()
    await renderWithProviders(
      <ErrorStatusPage status={403} onSessionExpired={onSessionExpired} />,
    )

    await waitFor(() => {
      expect(onSessionExpired).toHaveBeenCalledWith('/')
    })
  })

  it('404 hides request_id when it is unknown and never shows a retry', async () => {
    statusHandler()
    await renderWithProviders(<NotFoundPage />, ['common'])
    expect(screen.getByRole('heading', { level: 1 }).textContent).toBe(
      'Page not found',
    )
    expect(screen.queryByDisplayValue(/req_/)).toBeNull()
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull()
    // 后端尚未下发 site.docs_url / nav.pricing.enabled：次按钮必须隐藏。
    expect(screen.queryByRole('link', { name: 'Read the docs' })).toBeNull()
    expect(screen.queryByRole('link', { name: 'View pricing' })).toBeNull()
  })

  it('renders request_id only when required and copies it with an aria-live result', async () => {
    statusHandler()
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    })
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    })

    await renderWithProviders(<NotFoundPageContent requestId="req_abc123" />)

    expect(screen.getByDisplayValue('req_abc123')).toBeTruthy()
    // userEvent.setup() 会安装自己的 clipboard stub，覆盖上面的 spy，
    // 因此这里用原生 click 保留对 writeText 的断言。
    fireEvent.click(screen.getByRole('button', { name: 'Copy request ID' }))

    expect(writeText).toHaveBeenCalledWith('req_abc123')
    await waitFor(() => {
      expect(screen.getByRole('status').textContent).toBe('Copied')
    })
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: undefined,
    })
  })

  it('maps render errors to the 500 page with the error request_id', async () => {
    const reset = vi.fn()
    const error = new ApiError({
      status: 500,
      type: 'about:blank',
      title: 'boom',
      kind: 'server',
      request_id: 'req_500',
    })
    await renderWithProviders(<ErrorFallback error={error} reset={reset} />)

    expect(screen.getByRole('heading', { level: 1 }).textContent).toBe(
      'Something went wrong on our side',
    )
    expect(screen.getByDisplayValue('req_500')).toBeTruthy()

    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }))
    expect(reset).toHaveBeenCalledTimes(1)
    // 原始错误文本不得外露（GC-2）。
    expect(screen.queryByText('boom')).toBeNull()
  })

  it('hides retry when there is no original request', async () => {
    await renderWithProviders(<ServerErrorPage requestId="req_x" />)
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull()
  })

  it('switches 500 to the offline copy on a network failure', async () => {
    await renderWithProviders(
      <ServerErrorPage networkFailure onRetry={() => undefined} />,
    )
    expect(screen.getByRole('heading', { level: 1 }).textContent).toBe(
      'Network disconnected',
    )
    expect(
      (screen.getByRole('button', { name: 'Retry' }) as HTMLButtonElement)
        .disabled,
    ).toBe(true)
  })
})

describe('503 recovery', () => {
  it('shows the recovery banner after /readyz turns ready', async () => {
    let calls = 0
    server.use(
      http.get(`${ORIGIN}/readyz`, () => {
        calls += 1
        return HttpResponse.json({
          status: calls === 1 ? 'degraded' : 'ok',
          checks: {},
        })
      }),
    )

    await renderWithProviders(
      <ServiceUnavailablePage probeIntervalMs={40} onRetry={() => undefined} />,
    )

    expect(screen.getByRole('heading', { level: 1 }).textContent).toBe(
      'Service temporarily unavailable',
    )
    await waitFor(
      () => {
        expect(screen.getByText('Service recovered. Try again.')).toBeTruthy()
      },
      { timeout: 3000 },
    )
  })

  it('renders the status page link only when site.status_page_url exists', async () => {
    server.use(
      http.get(`${ORIGIN}/readyz`, () =>
        HttpResponse.json({ status: 'degraded', checks: {} }),
      ),
    )

    const { unmount } = await renderWithProviders(
      <ServiceUnavailablePage probeIntervalMs={60_000} onRetry={() => undefined} />,
    )
    expect(screen.queryByRole('link', { name: 'View status page' })).toBeNull()
    unmount()

    await renderWithProviders(
      <ServiceUnavailablePage
        statusPageUrl="https://status.example.com"
        probeIntervalMs={60_000}
        onRetry={() => undefined}
      />,
    )
    const link = screen.getByRole('link', { name: 'View status page' })
    expect(link.getAttribute('href')).toBe('https://status.example.com')
    expect(link.getAttribute('target')).toBe('_blank')
  })
})

describe('route mapping', () => {
  it('renders the shared 404 page for an unmatched path', async () => {
    statusHandler()
    const queryClient = createQueryClient()
    const router = createRouter({
      routeTree,
      context: { queryClient },
      history: createMemoryHistory({ initialEntries: ['/no-such-page'] }),
      scrollRestoration: false,
    })
    render(
      <QueryClientProvider client={queryClient}>
        <I18nProvider modules={['common']}>
          <RouterProvider router={router} />
        </I18nProvider>
      </QueryClientProvider>,
    )

    expect(
      await screen.findByRole('heading', { name: 'Page not found' }),
    ).toBeTruthy()
  })
})
