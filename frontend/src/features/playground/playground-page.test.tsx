import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import {
  findRequest,
  installFetch,
  jsonResponse,
} from '@/features/tokens/test-utils'

import { PlaygroundPage } from './playground-page'

const STATUS = {
  usable_groups: [
    { name: 'default', description: '', ratio: 100, models: ['alpha', 'beta'] },
    { name: 'pro', description: '', ratio: 150, models: ['gamma'] },
  ],
  nav: { modules: { chat: { playground: true } } },
}

const SELF = {
  language: 'en',
  sidebar_modules: null,
  record_ip_log: false,
  default_model: 'beta',
  updated_at: '2026-10-01T00:00:00Z',
}

const ME = {
  username: 'alice',
  role: 'user',
  status: 'enabled',
  permissions: [],
}

const WALLET = {
  balance: 12000,
  held_credits: 0,
  total_consumed: 0,
  total_requests: 0,
  threshold: 1000,
  health: 'healthy',
  group: 'default',
}

const ENCODER = new TextEncoder()

function sse(chunks: string[], headers: Record<string, string> = {}): Response {
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(ENCODER.encode(chunk))
      controller.close()
    },
  })
  return new Response(stream, {
    status: 200,
    headers: { 'Content-Type': 'text/event-stream', ...headers },
  })
}

function baseResponse(request: { method: string; url: URL }): Response | null {
  if (request.method === 'GET' && request.url.pathname === '/api/status') {
    return jsonResponse(STATUS)
  }
  if (request.method === 'GET' && request.url.pathname === '/api/self') {
    return jsonResponse(SELF)
  }
  if (request.method === 'GET' && request.url.pathname === '/api/me') {
    return jsonResponse(ME)
  }
  if (request.method === 'GET' && request.url.pathname === '/api/wallet/summary') {
    return jsonResponse(WALLET)
  }
  return null
}

beforeEach(() => {
  sessionStorage.clear()
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('PlaygroundPage', () => {
  it('selects the default model, streams SSE and shows the usage summary', async () => {
    const user = userEvent.setup()
    const requests = installFetch((request) => {
      const base = baseResponse(request)
      if (base !== null) return base
      if (request.url.pathname === '/pg/chat/completions') {
        return sse(
          [
            'data: {"id":"chatcmpl-req-1","choices":[{"delta":{"content":"Hel"}}]}\n\n',
            'data: {"id":"chatcmpl-req-1","choices":[{"delta":{"content":"lo"}}]}\n\n',
            'data: {"id":"chatcmpl-req-1","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}\n\n',
            'data: [DONE]\n\n',
          ],
          { 'X-Request-ID': 'req-1' },
        )
      }
      if (request.method === 'GET' && request.url.pathname === '/api/usage-logs') {
        return jsonResponse({
          items: [{ id: 1, request_id: 'req-1', cost: 7, token_id: null }],
          total: 1,
          page: 1,
          page_size: 1,
        })
      }
      return jsonResponse({}, 404)
    })

    await renderWithProviders(<PlaygroundPage />, ['common', 'account'])

    const select = (await screen.findByLabelText(
      'Select a model',
    )) as HTMLSelectElement
    await waitFor(() => {
      expect(select.value).toBe('beta')
    })
    expect(screen.getByTestId('playground-welcome')).toBeTruthy()

    await user.type(screen.getByLabelText('Message'), 'Say hello')
    await user.click(screen.getByRole('button', { name: 'Send' }))

    expect(await screen.findByText('Hello')).toBeTruthy()
    await waitFor(() => {
      expect(screen.getByText(/7 credits this turn/)).toBeTruthy()
    })
    expect(screen.getByText(/prompt 3/)).toBeTruthy()
    expect(screen.getByText(/completion 2/)).toBeTruthy()
    expect(screen.getByText(/no API token used/)).toBeTruthy()

    const post = findRequest(requests, 'POST', '/pg/chat/completions')
    expect(post?.headers.get('Authorization')).toBeNull()
    expect(post?.body).toMatchObject({
      model: 'beta',
      stream: true,
      stream_options: { include_usage: true },
      messages: [{ role: 'user', content: 'Say hello' }],
    })

    // 会话只写 sessionStorage（用户 + 模型隔离键），不落服务端。
    const stored = sessionStorage.getItem('newtg.playground.v1.alice.beta')
    expect(stored).not.toBeNull()
    expect(stored).toContain('Hello')
  })

  it('stops a stream, keeps the generated text and marks it stopped', async () => {
    const user = userEvent.setup()
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const raw =
          typeof input === 'string'
            ? input
            : input instanceof URL
              ? input.toString()
              : input.url
        const url = new URL(raw, 'http://localhost:3000')
        const base = baseResponse({ method: init?.method ?? 'GET', url })
        if (base !== null) return base
        let streamController:
          | ReadableStreamDefaultController<Uint8Array>
          | null = null
        const stream = new ReadableStream<Uint8Array>({
          start(controller) {
            streamController = controller
            controller.enqueue(
              ENCODER.encode(
                'data: {"id":"chatcmpl-slow","choices":[{"delta":{"content":"Partial"}}]}\n\n',
              ),
            )
          },
        })
        const signal = input instanceof Request ? input.signal : init?.signal
        signal?.addEventListener('abort', () => {
          streamController?.error(new DOMException('Aborted', 'AbortError'))
        })
        return new Response(stream, {
          status: 200,
          headers: {
            'Content-Type': 'text/event-stream',
            'X-Request-ID': 'slow-1',
          },
        })
      }),
    )

    await renderWithProviders(<PlaygroundPage />, ['common', 'account'])
    await screen.findByLabelText('Select a model')
    await user.type(screen.getByLabelText('Message'), 'go')
    await user.click(screen.getByRole('button', { name: 'Send' }))

    expect(await screen.findByText('Partial')).toBeTruthy()
    await user.click(screen.getByRole('button', { name: 'Stop' }))

    // 「Stopped」同时出现在消息标记与屏幕阅读器播报区，按集合断言。
    expect((await screen.findAllByText('Stopped')).length).toBeGreaterThan(0)
    expect(screen.getByText('Partial')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Send' })).toBeTruthy()
  })

  it('shows the unavailable-model card on 403 and keeps the user message', async () => {
    const user = userEvent.setup()
    installFetch((request) => {
      const base = baseResponse(request)
      if (base !== null) return base
      if (request.url.pathname === '/pg/chat/completions') {
        return jsonResponse(
          {
            error: {
              message: 'model not available',
              type: 'newtg_relay_error',
              code: 'model_not_available',
            },
          },
          403,
        )
      }
      return jsonResponse({}, 404)
    })

    await renderWithProviders(<PlaygroundPage />, ['common', 'account'])
    await screen.findByLabelText('Select a model')
    await user.type(screen.getByLabelText('Message'), 'hello')
    await user.click(screen.getByRole('button', { name: 'Send' }))

    expect((await screen.findByRole('alert')).textContent).toContain(
      'This model is currently unavailable',
    )
    expect(screen.getByText('This request was not billed.')).toBeTruthy()
    expect(screen.getByText('hello')).toBeTruthy()
  })

  it('keeps the draft after a 429', async () => {
    const user = userEvent.setup()
    installFetch((request) => {
      const base = baseResponse(request)
      if (base !== null) return base
      if (request.url.pathname === '/pg/chat/completions') {
        return jsonResponse(
          {
            error: {
              message: 'rate limited',
              type: 'newtg_relay_error',
              code: 'rate_limit_exceeded',
            },
          },
          429,
        )
      }
      return jsonResponse({}, 404)
    })

    await renderWithProviders(<PlaygroundPage />, ['common', 'account'])
    await screen.findByLabelText('Select a model')
    const input = screen.getByLabelText('Message') as HTMLTextAreaElement
    await user.type(input, 'keep me')
    await user.click(screen.getByRole('button', { name: 'Send' }))

    await waitFor(() => {
      expect(input.value).toBe('keep me')
    })
    // 本轮未受理：不应留下半条用户消息。
    expect(screen.queryByText('keep me', { selector: 'p' })).toBeNull()
  })

  it('replays once through the shared CSRF refresh on 403 csrf_failed', async () => {
    const user = userEvent.setup()
    let posts = 0
    const requests = installFetch((request) => {
      const base = baseResponse(request)
      if (base !== null) return base
      if (request.url.pathname === '/pg/chat/completions') {
        posts += 1
        if (posts === 1) {
          return jsonResponse({ type: 'csrf_failed', title: 'csrf failed' }, 403)
        }
        return sse([
          'data: {"id":"chatcmpl-ok","choices":[{"delta":{"content":"ok"}}]}\n\n',
          'data: [DONE]\n\n',
        ])
      }
      if (request.method === 'GET' && request.url.pathname === '/api/usage-logs') {
        return jsonResponse({ items: [], total: 0, page: 1, page_size: 1 })
      }
      return jsonResponse({}, 404)
    })

    await renderWithProviders(<PlaygroundPage />, ['common', 'account'])
    await screen.findByLabelText('Select a model')
    await user.type(screen.getByLabelText('Message'), 'hi')
    await user.click(screen.getByRole('button', { name: 'Send' }))

    expect(await screen.findByText('ok')).toBeTruthy()
    expect(posts).toBe(2)
    // 刷新会话 cookie 走的是与契约客户端相同的 GET /api/me。
    expect(
      requests.filter(
        (request) =>
          request.method === 'GET' && request.url.pathname === '/api/me',
      ).length,
    ).toBeGreaterThan(1)
  })

  it('restores the session for the current user and model from sessionStorage', async () => {
    sessionStorage.setItem(
      'newtg.playground.v1.alice.beta',
      JSON.stringify([
        { id: 'm1', role: 'user', content: 'Restored hello' },
        {
          id: 'm2',
          role: 'assistant',
          content: 'Restored reply',
          model: 'beta',
        },
      ]),
    )
    installFetch((request) => baseResponse(request) ?? jsonResponse({}, 404))

    await renderWithProviders(<PlaygroundPage />, ['common', 'account'])

    expect(await screen.findByText('Restored hello')).toBeTruthy()
    expect(screen.getByText('Restored reply')).toBeTruthy()
  })
})
