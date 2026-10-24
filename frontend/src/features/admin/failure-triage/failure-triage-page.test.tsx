import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import {
  installFetch,
  jsonResponse,
  problemResponse,
  requestsFor,
} from '@/features/logs/test-utils'
import { renderWithProviders } from '@/test-support/providers'
import { useToastStore } from '@/shared/ui/toast'

import { FailureTriagePage } from './failure-triage-page'

/**
 * 页面测试使用仓库既有的 `installFetch` 桩（没有 MSW），三个场景都通过
 * 真实 English 词典渲染，因此断言的是管理员最终看到的句子，而不是 key。
 */

const REQUEST_ID = '6de20d22a7b14c2f9e0d5a3b8c1f4e27'

const LOG = {
  id: 1,
  request_id: REQUEST_ID,
  token_id: 2,
  token_name: 'prod',
  channel_id: 7,
  upstream: 'gpt-4o-2024-08-06',
  model: 'gpt-4o',
  group: 'default',
  prompt_tokens: 120,
  completion_tokens: 40,
  cache_read_tokens: 0,
  cache_write_tokens: 0,
  pre_deducted: 0,
  cost: 1200,
  refunded: 1200,
  latency_ms: 320,
  ttft: null,
  stream: false,
  status: 'error',
  error_class: 'invalid_request',
  error_message: '••••••',
  retry_chain: [],
  created_at: '2026-10-24T09:41:22Z',
  username: 'alice',
  other: {},
}

const EVIDENCE = {
  endpoint: '/v1/chat/completions',
  route_from: '',
  route_to: '',
  conversion: [],
  upstream_status: 0,
  attempts: 0,
  channel_type: '',
  channel_id: null,
  request_structure: 'invalid',
  reject_stage: 'request_structure',
  billing: { cost: 1200, pre_deducted: 0, refunded: 1200 },
  error_class: 'invalid_request',
  created_at: '2026-10-24T09:41:22Z',
}

const RECENT = {
  items: [
    {
      request_id: REQUEST_ID,
      created_at: '2026-10-24T09:41:22Z',
      model: 'gpt-4o',
      channel_id: 7,
      status: 'error',
      owner: 'user',
      sub: 'request_structure',
      rule: 'USR-01',
    },
  ],
  counts: { user: 1, channel: 0, gateway: 0, unknown: 0, total: 1 },
  window: {
    start: '2026-10-23T09:41:22Z',
    end: '2026-10-24T09:41:22Z',
  },
}

const CHANNEL_RECENT = {
  ...RECENT,
  items: [
    {
      request_id: REQUEST_ID,
      created_at: '2026-10-24T09:52:10Z',
      model: 'kimi-k2',
      channel_id: 3,
      status: 'error',
      owner: 'channel',
      sub: 'upstream_auth',
      rule: 'CH-01',
    },
  ],
  counts: { user: 0, channel: 1, gateway: 0, unknown: 0, total: 1 },
}

const FOUND = {  found: true,
  request_id: REQUEST_ID,
  log: LOG,
  evidence: EVIDENCE,
  verdict: {
    owner: 'user',
    sub: 'request_structure',
    rule: 'USR-01',
    signals: [
      {
        code: 'request_structure_invalid',
        vote: 'user',
      },
      {
        code: 'no_upstream_attempt',
        vote: 'exclude-channel',
      },
    ],
  },
}

interface Scenario {
  recent?: unknown
  recentStatus?: number
  triage?: unknown
}

/** 后端在计数前先按 owner 过滤；桩必须复刻这一点。 */
function scopeToOwner(data: typeof RECENT, owner: string | null): typeof RECENT {
  if (owner === null) return data
  const items = data.items.filter((item) => item.owner === owner)
  const counts = { user: 0, channel: 0, gateway: 0, unknown: 0, total: items.length }
  for (const item of items) {
    if (item.owner === 'user') counts.user += 1
    else if (item.owner === 'channel') counts.channel += 1
    else if (item.owner === 'gateway') counts.gateway += 1
    else counts.unknown += 1
  }
  return { ...data, items, counts }
}

function installScenario(scenario: Scenario = {}) {
  return installFetch((request) => {
    const path = request.url.pathname
    if (path === '/api/usage-logs/triage/recent') {
      if (scenario.recentStatus !== undefined && scenario.recentStatus >= 400) {
        return problemResponse(scenario.recentStatus, {
          type: 'about:blank',
          title: 'failed',
        })
      }
      return jsonResponse(
        scopeToOwner(
          (scenario.recent ?? RECENT) as typeof RECENT,
          request.url.searchParams.get('owner'),
        ),
      )
    }
    if (path.startsWith('/api/usage-logs/triage/')) {
      return jsonResponse(
        scenario.triage ?? { found: false, request_id: REQUEST_ID },
      )
    }
    return problemResponse(404, { type: 'about:blank', title: 'not found' })
  })
}

function caseRequests(requests: ReturnType<typeof installScenario>) {
  return requests.filter(
    (request) =>
      request.url.pathname.startsWith('/api/usage-logs/triage/') &&
      request.url.pathname !== '/api/usage-logs/triage/recent',
  )
}

async function searchFor(value: string) {
  const user = userEvent.setup()
  await renderWithProviders(<FailureTriagePage />, ['common', 'admin'])
  const input = screen.getByLabelText('requestId')
  await user.clear(input)
  await user.type(input, value)
  await user.click(screen.getByRole('button', { name: 'Search' }))
  return user
}

describe('failure triage page', () => {
  it('renders the conclusion, verdict stamp and evidence for a found request', async () => {
    const requests = installScenario({ triage: FOUND })
    await searchFor(REQUEST_ID)

    expect(
      await screen.findByText('The user request is invalid'),
    ).toBeTruthy()
    const stamp = screen.getByTestId('verdict-stamp')
    expect(stamp.getAttribute('data-owner')).toBe('user')
    expect(stamp.textContent).toContain('User request')
    expect(screen.getAllByTestId('evidence-row').length).toBeGreaterThan(0)
    expect(screen.getByText('Points to user')).toBeTruthy()
    expect(screen.getByText('Excludes channel')).toBeTruthy()
    expect(screen.getByTestId('fact-model').textContent).toContain('gpt-4o')
    expect(screen.getByText('1 most recent')).toBeTruthy()
    // Minor 3：动作链接必须带上 requestId，管理员不必手工复制粘贴。
    const usageLogsLink = screen.getByRole('link', { name: 'Open' })
    expect(usageLogsLink.getAttribute('href')).toBe(
      `/dashboard/usage-logs?request_id=${REQUEST_ID}`,
    )
    expect(caseRequests(requests)).toHaveLength(1)
  })

  it('shows the not-found guidance for a well-formed unknown id', async () => {
    const requests = installScenario({
      triage: { found: false, request_id: REQUEST_ID },
    })
    await searchFor(REQUEST_ID)

    const notFound = await screen.findByTestId('case-not-found')
    expect(notFound.textContent).toContain(REQUEST_ID)
    expect(screen.getByText(/pre-relay rejections are never recorded/)).toBeTruthy()
    expect(screen.getByText(/copied incompletely/)).toBeTruthy()
    expect(screen.getByText(/retention period/)).toBeTruthy()
    expect(screen.queryByTestId('verdict-stamp')).toBeNull()
    expect(caseRequests(requests)).toHaveLength(1)
  })

  it('rejects a malformed id inline without calling the case endpoint', async () => {
    const requests = installScenario()
    await searchFor('not-a-request-id')

    expect(await screen.findByTestId('search-invalid')).toBeTruthy()
    expect(screen.getByTestId('search-invalid').textContent).toContain(
      'valid requestId',
    )
    expect(caseRequests(requests)).toHaveLength(0)
    expect(
      requestsFor(requests, '/api/usage-logs/triage/recent'),
    ).toHaveLength(1)
  })

  it('attributes a quota refusal to the user and localises the zero upstream status', async () => {
    installScenario({
      triage: {
        found: true,
        request_id: REQUEST_ID,
        log: {
          ...LOG,
          status: 'refused',
          model: '',
          channel_id: null,
          upstream: null,
          cost: 0,
          refunded: 0,
        },
        evidence: {
          ...EVIDENCE,
          request_structure: '',
          reject_stage: 'quota',
          billing: { cost: 0, pre_deducted: 0, refunded: 0 },
        },
        verdict: {
          owner: 'user',
          sub: 'quota',
          rule: 'USR-02',
          signals: [
            {
              code: 'refused_before_upstream',
              vote: 'user',
              params: { stage: 'quota' },
            },
            {
              code: 'no_upstream_attempt',
              vote: 'exclude-channel',
            },
          ],
        },
      },
    })
    await searchFor(REQUEST_ID)

    expect(
      await screen.findByText(
        'The gateway rejected the request before reaching upstream',
      ),
    ).toBeTruthy()
    const stamp = screen.getByTestId('verdict-stamp')
    expect(stamp.getAttribute('data-owner')).toBe('user')
    expect(stamp.textContent).toContain('Insufficient quota')
    expect(
      screen.getByText('The gateway rejected the request before upstream (Insufficient quota)'),
    ).toBeTruthy()
    const upstreamStatus = screen.getByTestId('fact-upstream_status')
    expect(upstreamStatus.textContent).toContain('No upstream response')
    expect(upstreamStatus.textContent).not.toMatch(/\b0\b/)
    const attempts = screen.getByTestId('fact-attempts')
    expect(attempts.textContent).toContain('0 (not sent)')
    // 退款/解析失败的行会存空 model；空串必须显示为破折号而不是空白格。
    expect(screen.getByTestId('fact-model').textContent).toContain('—')
    expect(screen.getByTestId('fact-upstream_model').textContent).toContain('—')
  })

  it('scopes the header lead to the active owner filter and hides unseen owners', async () => {
    installScenario({ recent: CHANNEL_RECENT })
    await renderWithProviders(<FailureTriagePage />, ['common', 'admin'])
    expect(await screen.findByText('1 most recent failures')).toBeTruthy()

    const user = userEvent.setup()
    await user.click(
      screen.getByRole('button', { name: 'Upstream channel' }),
    )

    expect(
      await screen.findByText('1 most recent failures · Upstream channel'),
    ).toBeTruthy()
    expect(screen.getByText('Upstream channel 1')).toBeTruthy()
    expect(screen.queryByText(/^User request \d+$/)).toBeNull()
    expect(screen.queryByText(/^Gateway \d+$/)).toBeNull()
  })

  it('shows an error toast when the clipboard is unavailable', async () => {
    installScenario({ triage: FOUND })
    const user = await searchFor(REQUEST_ID)
    await screen.findByTestId('case-file')

    const descriptor = Object.getOwnPropertyDescriptor(
      window.navigator,
      'clipboard',
    )
    Object.defineProperty(window.navigator, 'clipboard', {
      value: undefined,
      configurable: true,
    })
    try {
      useToastStore.setState({ toasts: [] })
      const copyButton = screen.getAllByRole('button', { name: 'Copy' })[0]
      if (copyButton === undefined) throw new Error('copy button missing')
      await user.click(copyButton)

      const toast = useToastStore.getState().toasts.at(-1)
      expect(toast?.tone).toBe('error')
      expect(toast?.message).toBe('Could not copy to the clipboard.')
    } finally {
      if (descriptor === undefined) {
        delete (window.navigator as { clipboard?: unknown }).clipboard
      } else {
        Object.defineProperty(window.navigator, 'clipboard', descriptor)
      }
      useToastStore.setState({ toasts: [] })
    }
  })

  it('never leaves a raw {placeholder} when a signal omits params', async () => {
    installScenario({
      triage: {
        ...FOUND,
        evidence: {
          ...EVIDENCE,
          request_structure: 'valid',
        },
        verdict: {
          owner: 'unknown',
          sub: '',
          rule: 'UNKNOWN',
          signals: [
            { code: 'request_structure_valid', vote: 'exclude-user' },
            { code: 'conversion_absent', vote: 'neutral' },
            { code: 'future_signal_code', vote: 'neutral' },
          ],
        },
      },
    })
    await searchFor(REQUEST_ID)

    const file = await screen.findByTestId('case-file')
    expect(file.textContent).not.toContain('{')
    expect(file.textContent).toContain('The original request structure is valid')
    expect(file.textContent).toContain('No protocol conversion: Unknown protocol')
    expect(file.textContent).toContain('Unrecognised evidence')
    expect(screen.getByTestId('verdict-stamp').getAttribute('data-owner')).toBe(
      'unknown',
    )
  })
})
