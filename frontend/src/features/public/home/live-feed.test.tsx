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
})
afterEach(() => {
  server.resetHandlers()
  vi.unstubAllGlobals()
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
    // A-1：逐行不含费用/积分，页面也不伪造聚合条。
    expect(container.textContent).not.toContain('credits')
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
    expect(await findByText('No recent calls yet')).toBeTruthy()
  })
})
