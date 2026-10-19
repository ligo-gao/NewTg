import { QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
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
  readyzProbeInterval,
  ServerErrorPage,
  ServiceUnavailablePage,
} from '@/app/error-status-page'
import { I18nProvider } from '@/app/providers/i18n'
import { createQueryClient } from '@/app/providers/query'
import { router as appRouter } from '@/app/router'
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

    const evil = await renderWithProviders(
      <ErrorStatusPage status={401} redirect="//evil.example" />,
    )
    expect(
      screen.getByRole('link', { name: 'Sign in' }).getAttribute('href'),
    ).toBe(`/sign-in?redirect=${encodeURIComponent('/dashboard')}`)
    evil.unmount()

    // 合法站内相对路径（含 query）原样保留，编码后进入 redirect。
    await renderWithProviders(
      <ErrorStatusPage status={401} redirect="/wallet?tab=1" />,
    )
    expect(
      screen.getByRole('link', { name: 'Sign in' }).getAttribute('href'),
    ).toBe(`/sign-in?redirect=${encodeURIComponent('/wallet?tab=1')}`)
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

  it('403 leaves for sign-in with the original target when the session turns 401', async () => {
    meUnauthorized()
    const onSessionExpired = vi.fn()
    await renderWithProviders(
      <ErrorStatusPage
        status={403}
        redirect="/admin/users?page=2"
        onSessionExpired={onSessionExpired}
      />,
    )

    // 回跳目标是「用户真正想去的页面」，不是 /403 自身。
    await waitFor(() => {
      expect(onSessionExpired).toHaveBeenCalledWith('/admin/users?page=2')
    })
  })

  it('403 re-probes /me every 30s and signs in when it turns 401 (TC-F-09-10-05)', async () => {
    // shouldAdvanceTime 让 waitFor 自身的轮询计时器仍能推进；否则全量 fake
    // timers 会让任何 waitFor 直接卡到超时。
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      let calls = 0
      server.use(
        http.get(`${ORIGIN}/api/me`, () => {
          calls += 1
          return calls === 1
            ? HttpResponse.json({
                username: 'alice',
                role: 'user',
                status: 'enabled',
              })
            : HttpResponse.json(
                { type: 'about:blank', title: 'Unauthorized' },
                { status: 401 },
              )
        }),
      )
      const onSessionExpired = vi.fn()
      await renderWithProviders(
        <ErrorStatusPage status={403} onSessionExpired={onSessionExpired} />,
      )
      await waitFor(() => {
        expect(calls).toBe(1)
      })

      // 停留期间会话被撤销：30s 观察间隔内必须发现并转登录。
      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_000)
      })
      await waitFor(() => {
        expect(onSessionExpired).toHaveBeenCalledWith('/')
      })
    } finally {
      vi.useRealTimers()
    }
  })

  it('403 shows the offline variant when the /me probe fails at transport level', async () => {
    // status 0 / kind=offline：浏览器 navigator.onLine 仍为 true（DNS/连接拒绝）。
    server.use(http.get(`${ORIGIN}/api/me`, () => HttpResponse.error()))

    await renderWithProviders(<ErrorStatusPage status={403} canGoBack={false} />)

    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 }).textContent).toBe(
        'Network disconnected',
      )
    })
    expect(screen.queryByText('Access forbidden')).toBeNull()
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

  it('404 prefers docs over pricing and opens docs in a new tab', async () => {
    statusHandler()
    const docs = await renderWithProviders(
      <NotFoundPageContent docsUrl="https://docs.example.com" pricingEnabled />,
    )
    const docsLink = screen.getByRole('link', { name: 'Read the docs' })
    expect(docsLink.getAttribute('href')).toBe('https://docs.example.com')
    expect(docsLink.getAttribute('target')).toBe('_blank')
    expect(screen.queryByRole('link', { name: 'View pricing' })).toBeNull()
    docs.unmount()

    await renderWithProviders(<NotFoundPageContent pricingEnabled />)
    const pricingLink = screen.getByRole('link', { name: 'View pricing' })
    expect(pricingLink.getAttribute('href')).toBe('/pricing')
    expect(screen.queryByRole('link', { name: 'Read the docs' })).toBeNull()
  })

  it('renders request_id only when required and copies it with an aria-live result', async () => {
    statusHandler()
    const writeText = vi.fn().mockResolvedValue(undefined)
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
  })

  it('announces a failed copy through the same aria-live region', async () => {
    statusHandler()
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        writeText: vi.fn().mockRejectedValue(new Error('denied')),
      },
    })

    await renderWithProviders(<NotFoundPageContent requestId="req_fail" />)
    fireEvent.click(screen.getByRole('button', { name: 'Copy request ID' }))

    await waitFor(() => {
      expect(screen.getByRole('status').textContent).toBe(
        'Copy failed. Please select the text manually.',
      )
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

  it('maps an offline ApiError to the offline variant instead of 500 semantics', async () => {
    const error = new ApiError({
      status: 0,
      type: 'about:blank',
      title: 'Network request failed',
      kind: 'offline',
    })
    await renderWithProviders(<ErrorFallback error={error} reset={() => undefined} />)

    expect(screen.getByRole('heading', { level: 1 }).textContent).toBe(
      'Network disconnected',
    )
    expect(
      screen.queryByText('Something went wrong on our side'),
    ).toBeNull()
  })

  it('marks the retry button busy and disabled while the retry is in flight', async () => {
    let finish!: () => void
    const onRetry = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          finish = resolve
        }),
    )
    await renderWithProviders(<ServerErrorPage onRetry={onRetry} />)

    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    const button = screen.getByRole('button', { name: 'Retry' }) as HTMLButtonElement
    await waitFor(() => {
      expect(button.getAttribute('aria-busy')).toBe('true')
    })
    expect(button.disabled).toBe(true)

    await act(async () => {
      finish()
    })
    await waitFor(() => {
      expect(button.getAttribute('aria-busy')).toBe('false')
    })
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

  it('lengthens the probe interval to 60s after a 429', () => {
    const rateLimited = new ApiError({
      status: 429,
      type: 'about:blank',
      title: 'Too many requests',
      kind: 'rate_limited',
    })
    expect(readyzProbeInterval(rateLimited, 30_000)).toBe(60_000)
    expect(readyzProbeInterval(undefined, 30_000)).toBe(30_000)
    expect(
      readyzProbeInterval(
        new ApiError({
          status: 503,
          type: 'about:blank',
          title: 'Degraded',
          kind: 'server',
        }),
        30_000,
      ),
    ).toBe(30_000)
  })

  it('shows the offline variant when /readyz fails at transport level', async () => {
    server.use(http.get(`${ORIGIN}/readyz`, () => HttpResponse.error()))

    await renderWithProviders(
      <ServiceUnavailablePage probeIntervalMs={60_000} onRetry={() => undefined} />,
    )

    await waitFor(() => {
      expect(screen.getByRole('heading', { level: 1 }).textContent).toBe(
        'Network disconnected',
      )
    })
    expect(screen.queryByText('Service temporarily unavailable')).toBeNull()
  })

  it('probes immediately when the browser comes back online', async () => {
    let calls = 0
    server.use(
      http.get(`${ORIGIN}/readyz`, () => {
        calls += 1
        return HttpResponse.json({ status: 'degraded', checks: {} })
      }),
    )
    Object.defineProperty(navigator, 'onLine', {
      configurable: true,
      value: false,
    })

    await renderWithProviders(
      <ServiceUnavailablePage probeIntervalMs={60_000} onRetry={() => undefined} />,
    )
    await waitFor(() => {
      expect(calls).toBeGreaterThanOrEqual(1)
    })
    const before = calls

    Object.defineProperty(navigator, 'onLine', {
      configurable: true,
      value: true,
    })
    fireEvent(window, new Event('online'))

    // 不等到 60s 间隔：恢复在线必须立即探测一次。
    await waitFor(() => {
      expect(calls).toBeGreaterThan(before)
    })
  })

  it('offers the same retry action inside the recovery banner', async () => {
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
    const onRetry = vi.fn()

    await renderWithProviders(
      <ServiceUnavailablePage probeIntervalMs={40} onRetry={onRetry} />,
    )
    await waitFor(
      () => {
        expect(screen.getByText('Service recovered. Try again.')).toBeTruthy()
      },
      { timeout: 3000 },
    )

    // 横幅内的按钮排在主操作区之前。
    fireEvent.click(screen.getAllByRole('button', { name: 'Retry' })[0] as HTMLElement)
    await waitFor(() => {
      expect(onRetry).toHaveBeenCalledTimes(1)
    })
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
  function renderAt(path: string) {
    const queryClient = createQueryClient()
    const router = createRouter({
      routeTree,
      context: { queryClient },
      history: createMemoryHistory({ initialEntries: [path] }),
      scrollRestoration: false,
    })
    render(
      <QueryClientProvider client={queryClient}>
        <I18nProvider modules={['common']}>
          <RouterProvider router={router} />
        </I18nProvider>
      </QueryClientProvider>,
    )
    return router
  }

  it('renders the shared 404 page for an unmatched path', async () => {
    statusHandler()
    renderAt('/no-such-page')

    expect(
      await screen.findByRole('heading', { name: 'Page not found' }),
    ).toBeTruthy()
  })

  it('serves /500 directly and offers retry when a redirect target exists', async () => {
    statusHandler()
    renderAt('/500?redirect=%2Fwallet')

    expect(
      await screen.findByRole('heading', {
        name: 'Something went wrong on our side',
      }),
    ).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Retry' })).toBeTruthy()
  })

  it('serves /503 directly and probes readiness', async () => {
    statusHandler()
    server.use(
      http.get(`${ORIGIN}/readyz`, () =>
        HttpResponse.json({ status: 'degraded', checks: {} }),
      ),
    )
    renderAt('/503')

    expect(
      await screen.findByRole('heading', {
        name: 'Service temporarily unavailable',
      }),
    ).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Retry' })).toBeTruthy()
  })

  it('wires the defaults on the real app router', async () => {
    statusHandler()
    // 真实 `app/router.tsx` 的兜底组件必须指向错误页，而不是框架默认英文屏。
    expect(appRouter.options.defaultNotFoundComponent).toBe(NotFoundPage)
    expect(typeof appRouter.options.defaultErrorComponent).toBe('function')

    render(
      <QueryClientProvider client={createQueryClient()}>
        <I18nProvider modules={['common']}>
          <RouterProvider router={appRouter} />
        </I18nProvider>
      </QueryClientProvider>,
    )
    await act(async () => {
      await appRouter.navigate({ to: '/404' })
    })
    expect(
      await screen.findByRole('heading', { name: 'Page not found' }),
    ).toBeTruthy()
  })
})
