import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { act, fireEvent, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { LiveFeedSection } from './live-feed'

const ORIGIN = 'http://localhost:3000'

interface SseEvent {
  id: string
  ts: string
  model: string
  upstream: string
  tokens_in: number
  tokens_out: number
  latency_ms: number
  status: 'ok' | 'error'
}

function feedEvent(overrides: Partial<SseEvent> = {}): SseEvent {
  return {
    id: 'a1b2c3d4',
    ts: '2026-10-20T15:04:05Z',
    model: 'gpt-4o-mini',
    upstream: 'openai',
    tokens_in: 1024,
    tokens_out: 512,
    latency_ms: 830,
    status: 'ok',
    ...overrides,
  }
}

type Listener = (event: MessageEvent<string>) => void

class MockEventSource {
  static instances: MockEventSource[] = []
  readonly url: string
  onopen: (() => void) | null = null
  onerror: (() => void) | null = null
  closed = false
  private readonly listeners = new Map<string, Set<Listener>>()

  constructor(url: string) {
    this.url = url
    MockEventSource.instances.push(this)
  }

  addEventListener(type: string, listener: Listener): void {
    const set = this.listeners.get(type) ?? new Set<Listener>()
    set.add(listener)
    this.listeners.set(type, set)
  }

  close(): void {
    this.closed = true
  }

  emit(type: string, data: unknown): void {
    for (const listener of this.listeners.get(type) ?? []) {
      listener({ data: JSON.stringify(data) } as MessageEvent<string>)
    }
  }
}

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
beforeEach(() => {
  MockEventSource.instances = []
  // L1：SSE onopen 会并行播种一次列表快照；默认返回空快照，单测按需覆盖。
  server.use(
    http.get(`${ORIGIN}/api/home/live-feed`, () =>
      HttpResponse.json({ events: [] }),
    ),
  )
})
afterEach(() => {
  server.resetHandlers()
  vi.unstubAllGlobals()
  vi.useRealTimers()
})
afterAll(() => server.close())

function useSse() {
  vi.stubGlobal('EventSource', MockEventSource)
}

describe('home live feed (F-09-02)', () => {
  it('renders SSE events with tokens, latency and status but no cost', async () => {
    useSse()
    const { findByText, getByRole, container } = await renderWithProviders(
      <LiveFeedSection />,
      ['public'],
    )

    expect(await findByText('Live call feed')).toBeTruthy()
    const source = MockEventSource.instances[0]
    expect(source?.url).toBe('/api/home/live-feed/stream')

    act(() => {
      source?.onopen?.()
      source?.emit('call', feedEvent())
    })

    expect(await findByText('Live')).toBeTruthy()
    const table = getByRole('table')
    expect(within(table).getByText('gpt-4o-mini')).toBeTruthy()
    expect(within(table).getByText('openai')).toBeTruthy()
    expect(table.textContent).toContain('1,024')
    expect(table.textContent).toContain('512')
    expect(within(table).getByText('830 ms')).toBeTruthy()
    expect(within(table).getByText('Success')).toBeTruthy()
    // A-1：逐行不含费用/积分，页面也不伪造聚合条；加强为整块文本不得出现
    // 任何费用词汇（cost/credit/price），而不只是不出现 credits。
    expect(container.textContent).not.toMatch(/credits?|cost|price|billing/i)
    expect(container.textContent).not.toContain('$')
  })

  it('seeds a 20-event snapshot when SSE opens', async () => {
    useSse()
    const paths: string[] = []
    server.use(
      http.get(`${ORIGIN}/api/home/live-feed`, ({ request }) => {
        const url = new URL(request.url)
        paths.push(`${url.pathname}?${url.searchParams.toString()}`)
        return HttpResponse.json({
          events: [feedEvent({ id: 'seed-1', model: 'seeded-model' })],
        })
      }),
    )

    const { findByText, findAllByText } = await renderWithProviders(
      <LiveFeedSection />,
      ['public'],
    )
    await findByText('Live call feed')
    const source = MockEventSource.instances[0]
    act(() => source?.onopen?.())

    expect((await findAllByText('seeded-model')).length).toBeGreaterThan(0)
    expect(paths[0]).toBe('/api/home/live-feed?limit=20')
  })

  it('deduplicates repeated event ids', async () => {
    useSse()
    const { findByText, getByRole } = await renderWithProviders(
      <LiveFeedSection />,
      ['public'],
    )
    await findByText('Live call feed')
    const source = MockEventSource.instances[0]
    act(() => {
      source?.onopen?.()
      source?.emit('call', feedEvent())
      source?.emit('call', feedEvent())
    })
    const table = await waitFor(() => getByRole('table'))
    expect(within(table).getAllByText('gpt-4o-mini')).toHaveLength(1)
  })

  it('falls back to 5s polling when EventSource is unavailable', async () => {
    vi.stubGlobal('EventSource', undefined)
    const paths: string[] = []
    server.use(
      http.get(`${ORIGIN}/api/home/live-feed`, ({ request }) => {
        const url = new URL(request.url)
        paths.push(`${url.pathname}?${url.searchParams.toString()}`)
        return HttpResponse.json({ events: [feedEvent()] })
      }),
    )

    const { findAllByText, findByText } = await renderWithProviders(
      <LiveFeedSection />,
      ['public'],
    )
    expect((await findAllByText('gpt-4o-mini')).length).toBeGreaterThan(0)
    expect(await findByText('Live')).toBeTruthy()
    expect(paths[0]).toBe('/api/home/live-feed?limit=20')
  })

  it('renders the 5-minute aggregate window from the polling endpoint', async () => {
    vi.stubGlobal('EventSource', undefined)
    server.use(
      http.get(`${ORIGIN}/api/home/live-feed`, () =>
        HttpResponse.json({
          events: [feedEvent()],
          window: { calls: 128, cost_credits: 1530 },
        }),
      ),
    )

    const { findByTestId, findByText } = await renderWithProviders(
      <LiveFeedSection />,
      ['public'],
    )
    await findByText('Live call feed')
    const bar = await findByTestId('live-feed-window')
    expect(bar.textContent).toBe(
      'Last 5 minutes · 128 calls · 1,530 credits',
    )
    // 聚合条是唯一的费用读数；逐行表格仍不含费用字段。
    expect(bar.textContent).toContain('1,530')
  })

  it('hides the aggregate bar when the window payload is malformed', async () => {
    vi.stubGlobal('EventSource', undefined)
    server.use(
      http.get(`${ORIGIN}/api/home/live-feed`, () =>
        HttpResponse.json({
          events: [feedEvent()],
          window: { calls: -1, cost_credits: 'n/a' },
        }),
      ),
    )

    const { findAllByText, queryByTestId } = await renderWithProviders(
      <LiveFeedSection />,
      ['public'],
    )
    expect((await findAllByText('gpt-4o-mini')).length).toBeGreaterThan(0)
    expect(queryByTestId('live-feed-window')).toBeNull()
  })

  it('pauses and resumes the stream with the switch', async () => {
    useSse()
    const { findByText, getByRole, getByText } = await renderWithProviders(
      <LiveFeedSection />,
      ['public'],
    )
    await findByText('Live call feed')
    const first = MockEventSource.instances[0]
    act(() => first?.onopen?.())

    fireEvent.click(getByRole('switch', { name: 'Pause live updates' }))
    expect(first?.closed).toBe(true)
    expect(getByText('Paused')).toBeTruthy()
    expect(
      getByText('Live updates are paused. Resume to see the latest calls.'),
    ).toBeTruthy()

    fireEvent.click(getByRole('switch', { name: 'Resume live updates' }))
    await waitFor(() => expect(MockEventSource.instances).toHaveLength(2))
    const second = MockEventSource.instances[1]
    act(() => second?.onopen?.())
    expect(getByText('Live')).toBeTruthy()
  })

  it('pauses while the page is hidden and reconnects when visible again', async () => {
    useSse()
    let visibility: DocumentVisibilityState = 'visible'
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => visibility,
    })
    const { findByText, getByText } = await renderWithProviders(
      <LiveFeedSection />,
      ['public'],
    )
    await findByText('Live call feed')
    const first = MockEventSource.instances[0]
    act(() => first?.onopen?.())

    visibility = 'hidden'
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'))
    })
    await waitFor(() => expect(first?.closed).toBe(true))
    expect(getByText('Paused')).toBeTruthy()

    visibility = 'visible'
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'))
    })
    await waitFor(() => expect(MockEventSource.instances).toHaveLength(2))

    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => 'visible',
    })
  })

  it('degrades to polling after the SSE connection errors and shows reconnect copy', async () => {
    useSse()
    server.use(
      http.get(`${ORIGIN}/api/home/live-feed`, () =>
        HttpResponse.json({ events: [feedEvent({ model: 'claude-haiku' })] }),
      ),
    )
    const { findByText, findAllByText } = await renderWithProviders(
      <LiveFeedSection />,
      ['public'],
    )
    await findByText('Live call feed')
    const source = MockEventSource.instances[0]
    act(() => source?.onopen?.())
    act(() => source?.onerror?.())

    expect(source?.closed).toBe(true)
    expect((await findAllByText('claude-haiku')).length).toBeGreaterThan(0)
    expect(await findByText('Live')).toBeTruthy()
  })

  it('shows the empty state while remaining live', async () => {
    useSse()
    const { findByText } = await renderWithProviders(
      <LiveFeedSection />,
      ['public'],
    )
    await findByText('Live call feed')
    const source = MockEventSource.instances[0]
    act(() => source?.onopen?.())
    expect(await findByText('Waiting for new calls')).toBeTruthy()
  })

  it('hides the section when the feed endpoint returns 404', async () => {
    vi.stubGlobal('EventSource', undefined)
    server.use(
      http.get(`${ORIGIN}/api/home/live-feed`, () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'Not found', status: 404 },
          { status: 404 },
        ),
      ),
    )
    const { findByText, queryByTestId } = await renderWithProviders(
      <LiveFeedSection />,
      ['public'],
    )
    await findByText('Live call feed')
    await waitFor(() => expect(queryByTestId('live-feed-section')).toBeNull())
  })

  it('backs off 5s, 15s, 60s on repeated 429 without reporting a disconnect', async () => {
    vi.useFakeTimers()
    vi.stubGlobal('EventSource', undefined)
    let calls = 0
    server.use(
      http.get(`${ORIGIN}/api/home/live-feed`, () => {
        calls += 1
        return HttpResponse.json(
          { type: 'about:blank', title: 'Too many requests', status: 429 },
          { status: 429 },
        )
      }),
    )

    const { getByText, queryByText } = await renderWithProviders(
      <LiveFeedSection />,
      ['public'],
    )
    // 假时钟下不要用 findBy*/waitFor（它们依赖真实定时器）；首屏标题同步存在。
    expect(getByText('Live call feed')).toBeTruthy()
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0)
    })
    expect(calls).toBe(1)

    // 429 是限流退避，不是断线：不出现 Reconnecting 徽标或断开提示。
    expect(queryByText('Reconnecting')).toBeNull()
    expect(queryByText(/connection dropped/i)).toBeNull()

    await act(async () => {
      await vi.advanceTimersByTimeAsync(4999)
    })
    expect(calls).toBe(1)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1)
    })
    expect(calls).toBe(2)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(14_999)
    })
    expect(calls).toBe(2)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1)
    })
    expect(calls).toBe(3)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(59_999)
    })
    expect(calls).toBe(3)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1)
    })
    expect(calls).toBe(4)
    expect(queryByText('Reconnecting')).toBeNull()
  })

  it('reconnects immediately on the online event instead of waiting out the backoff', async () => {
    vi.useFakeTimers()
    vi.stubGlobal('EventSource', undefined)
    let calls = 0
    server.use(
      http.get(`${ORIGIN}/api/home/live-feed`, () => {
        calls += 1
        if (calls === 1) {
          return HttpResponse.json(
            { type: 'about:blank', title: 'Too many requests', status: 429 },
            { status: 429 },
          )
        }
        return HttpResponse.json({
          events: [feedEvent({ id: 'online-1', model: 'recovered-model' })],
        })
      }),
    )

    const { getByText, getAllByText } = await renderWithProviders(
      <LiveFeedSection />,
      ['public'],
    )
    expect(getByText('Live call feed')).toBeTruthy()
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0)
    })
    expect(calls).toBe(1)

    act(() => {
      window.dispatchEvent(new Event('online'))
    })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0)
    })
    expect(calls).toBe(2)
    expect(getAllByText('recovered-model').length).toBeGreaterThan(0)
    expect(getByText('Live')).toBeTruthy()
  })

  it('polls every 5s after a successful snapshot', async () => {
    vi.useFakeTimers()
    vi.stubGlobal('EventSource', undefined)
    let calls = 0
    server.use(
      http.get(`${ORIGIN}/api/home/live-feed`, () => {
        calls += 1
        return HttpResponse.json({
          events: [feedEvent({ id: `poll-${calls}` })],
        })
      }),
    )

    const { getByText } = await renderWithProviders(
      <LiveFeedSection />,
      ['public'],
    )
    expect(getByText('Live call feed')).toBeTruthy()
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0)
    })
    expect(calls).toBe(1)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(4999)
    })
    expect(calls).toBe(1)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1)
    })
    expect(calls).toBe(2)
  })
})
