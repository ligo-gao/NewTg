import { QueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { fireEvent, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { clearRankingsEtagCache, type RankingsPeriod } from './api'
import { rankingsAccessRedirect } from './guard'
import { RankingsPage } from './page'

const ORIGIN = 'http://localhost:3000'

interface RequestRecord {
  period: string | null
  ifNoneMatch: string | null
}

function statusFixture(overrides: Record<string, unknown> = {}) {
  return {
    system_initialized: true,
    auth: {
      signup_enabled: true,
      password_login_enabled: true,
      email_verification_required: false,
      turnstile: { enabled: false, site_key: '' },
    },
    password_login_encryption_enabled: false,
    languages: ['en', 'zh-CN', 'zh-TW', 'fr', 'ru', 'ja', 'vi'],
    default_language: 'en',
    site: {
      title: 'Acme Gateway',
      logo_url: '',
      server_address: 'https://api.example.com/',
      docs_url: 'https://docs.example.com',
      footer_html: '',
      about_html: '',
      filing_no: '',
      status_page_url: '',
      timezone: 'Asia/Shanghai',
      seo: { description: 'Acme gateway', og_image_url: '' },
      home_custom_enabled: false,
      home_blocks: { hero: true, live_feed: false, features: true, stats: true, onboarding: true },
    },
    nav: {
      home: { enabled: true, require_auth: false },
      console: { enabled: true, require_auth: true },
      pricing: { enabled: true, require_auth: false },
      rankings: { enabled: true, require_auth: false },
      docs: { enabled: true, require_auth: false },
      about: { enabled: true, require_auth: false },
    },
    ...overrides,
  }
}

function rankingsFixture(period: RankingsPeriod) {
  return {
    period,
    timezone: 'Asia/Shanghai',
    window: {
      start: '2026-10-13T00:00:00+08:00',
      end: '2026-10-20T09:55:00+08:00',
    },
    computed_at: '2026-10-20T09:55:00+08:00',
    freshness_seconds: 300,
    sample_threshold: 100,
    status: 'ok',
    models: [
      {
        rank: 1,
        model: 'gpt-4o',
        display_name: 'GPT-4o',
        author: 'OpenAI',
        total_tokens: 1_240_000,
        requests: 4021,
        delta_tokens: 50_000,
        delta_pct: 4.2,
        prev_rank: 1,
      },
      {
        rank: 2,
        model: 'claude-3-5-sonnet',
        display_name: 'Claude 3.5 Sonnet',
        author: 'Anthropic',
        total_tokens: 986_000,
        requests: 3118,
        delta_tokens: -86_000,
        delta_pct: -8.1,
        prev_rank: 2,
      },
      {
        rank: 2,
        model: 'gemini-2.5-pro',
        display_name: 'Gemini 2.5 Pro',
        author: 'Google',
        total_tokens: 986_000,
        requests: 2400,
        delta_tokens: null,
        delta_pct: null,
        prev_rank: null,
      },
    ],
    movers: {
      up: [
        { model: 'claude-3-5-sonnet', author: 'Anthropic', delta_pct: 18.4, cur_tokens: 900_000, prev_tokens: 760_000 },
      ],
      down: [
        { model: 'gemini-2.5-pro', author: 'Google', delta_pct: -9.7, cur_tokens: 512_000, prev_tokens: 567_000 },
      ],
    },
    providers: [
      { rank: 1, author: 'OpenAI', total_tokens: 456_800_000, share_pct: 42.1, slot: 1 },
      { rank: 2, author: 'Anthropic', total_tokens: 302_100_000, share_pct: 27.8, slot: 2 },
      { rank: 3, author: 'Google', total_tokens: 189_400_000, share_pct: 17.4, slot: 3 },
    ],
    trend: {
      granularity: 'day',
      buckets: ['2026-10-18T00:00:00+08:00', '2026-10-19T00:00:00+08:00'],
      series: [
        { model: 'gpt-4o', slot: 1, tokens: [1000, 2000] },
        { model: 'claude-3-5-sonnet', slot: 2, tokens: [800, 900] },
        { model: 'Other', slot: null, tokens: [500, 600] },
      ],
    },
  }
}

function emptyFixture(period: RankingsPeriod) {
  return {
    ...rankingsFixture(period),
    status: 'empty',
    computed_at: null,
    freshness_seconds: null,
    sample_threshold: 100,
    models: [],
    movers: { up: [], down: [] },
    providers: [],
    trend: { granularity: 'day', buckets: [], series: [] },
  }
}

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
beforeEach(() => {
  clearRankingsEtagCache()
})
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function installBaseHandlers(
  onRankings: (request: Request) => Response,
  status: Record<string, unknown> = statusFixture(),
) {
  server.use(
    http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(status)),
    http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
    http.get(`${ORIGIN}/api/rankings`, ({ request }) => onRankings(request)),
  )
}

function Harness({ initial = 'week' as RankingsPeriod }) {
  const [period, setPeriod] = useState<RankingsPeriod>(initial)
  return <RankingsPage period={period} onPeriodChange={setPeriod} />
}

describe('rankings page (F-09-29 ~ F-09-33)', () => {
  it('renders every section and caches each period after the first load', async () => {
    const requests: RequestRecord[] = []
    installBaseHandlers((request) => {
      const url = new URL(request.url)
      const period = (url.searchParams.get('period') ?? 'week') as RankingsPeriod
      requests.push({
        period,
        ifNoneMatch: request.headers.get('If-None-Match'),
      })
      const etag = `"${period}-1"`
      return HttpResponse.json(rankingsFixture(period), { headers: { ETag: etag } })
    })

    const { findByRole, getByRole, getAllByText } = await renderWithProviders(
      <Harness />,
      ['public'],
    )

    // 区块一：模型榜（并列名次 2、2）。
    await findByRole('heading', { name: 'Model Rankings' })
    expect(getAllByText('GPT-4o').length).toBeGreaterThan(0)
    expect(getAllByText('OpenAI').length).toBeGreaterThan(0)
    // 区块二：升降榜。
    expect(getByRole('heading', { name: 'Momentum' })).toBeTruthy()
    expect(getByRole('heading', { name: 'Historical Trend' })).toBeTruthy()
    expect(getByRole('heading', { name: 'Provider Share' })).toBeTruthy()
    expect(getAllByText('OpenAI').length).toBeGreaterThan(0)
    expect(requests.filter((entry) => entry.period === 'week')).toHaveLength(1)

    // 四周期切换：每个周期只请求一次；切回已加载周期不再请求。
    fireEvent.click(getByRole('tab', { name: 'Today' }))
    await waitFor(() =>
      expect(requests.some((entry) => entry.period === 'today')).toBe(true),
    )
    fireEvent.click(getByRole('tab', { name: 'This Month' }))
    await waitFor(() =>
      expect(requests.some((entry) => entry.period === 'month')).toBe(true),
    )
    fireEvent.click(getByRole('tab', { name: 'This Year' }))
    await waitFor(() =>
      expect(requests.some((entry) => entry.period === 'year')).toBe(true),
    )
    fireEvent.click(getByRole('tab', { name: 'This Week' }))
    await findByRole('heading', { name: 'Model Rankings' })
    fireEvent.click(getByRole('tab', { name: 'Today' }))
    await findByRole('heading', { name: 'Model Rankings' })

    expect(requests.filter((entry) => entry.period === 'week')).toHaveLength(1)
    expect(requests.filter((entry) => entry.period === 'today')).toHaveLength(1)
    expect(requests.filter((entry) => entry.period === 'month')).toHaveLength(1)
    expect(requests.filter((entry) => entry.period === 'year')).toHaveLength(1)
  })

  it('sends If-None-Match on manual refresh and keeps data on 304', async () => {
    const requests: RequestRecord[] = []
    installBaseHandlers((request) => {
      const url = new URL(request.url)
      const period = (url.searchParams.get('period') ?? 'week') as RankingsPeriod
      const etag = `"${period}-1"`
      const ifNoneMatch = request.headers.get('If-None-Match')
      requests.push({ period, ifNoneMatch })
      if (ifNoneMatch === etag) {
        return new Response(null, { status: 304, headers: { ETag: etag } })
      }
      return HttpResponse.json(rankingsFixture(period), { headers: { ETag: etag } })
    })

    const { findAllByText, getByRole } = await renderWithProviders(
      <Harness />,
      ['public'],
    )
    await findAllByText('GPT-4o')
    fireEvent.click(getByRole('button', { name: 'Refresh' }))

    await waitFor(() => expect(requests).toHaveLength(2))
    expect(requests[1]?.ifNoneMatch).toBe('"week-1"')
    // 304 无 body：沿用上一次的数据，不出现错误态。
    expect((await findAllByText('GPT-4o')).length).toBeGreaterThan(0)
    expect(getByRole('button', { name: 'Refresh' })).toBeTruthy()
  })

  it('counts down a 429 from Retry-After and retries automatically', async () => {
    let calls = 0
    installBaseHandlers(() => {
      calls += 1
      if (calls === 1) {
        return HttpResponse.json(
          { type: 'about:blank', title: 'Too many requests', status: 429 },
          { status: 429, headers: { 'Retry-After': '1' } },
        )
      }
      return HttpResponse.json(rankingsFixture('week'), { headers: { ETag: '"week-1"' } })
    })

    const { findAllByText, findByText, getByRole } = await renderWithProviders(
      <Harness />,
      ['public'],
    )
    expect(await findByText(/Too many requests\. Please retry in 1s/)).toBeTruthy()
    await waitFor(() => expect(calls).toBe(2), { timeout: 4000 })
    expect((await findAllByText('GPT-4o')).length).toBeGreaterThan(0)
    expect(getByRole('heading', { name: 'Model Rankings' })).toBeTruthy()
  })

  it('renders movers, trend and share, and switches every chart to a table', async () => {
    installBaseHandlers(() =>
      HttpResponse.json(rankingsFixture('week'), { headers: { ETag: '"week-1"' } }),
    )

    const { findByRole, getByRole, container } = await renderWithProviders(
      <Harness />,
      ['public'],
    )
    await findByRole('heading', { name: 'Model Rankings' })

    // 升降榜百分比、趋势图例、供应商份额读数。
    expect(getByRole('heading', { name: /Rising/ })).toBeTruthy()
    expect(getByRole('heading', { name: /Falling/ })).toBeTruthy()
    expect(container.textContent).toContain('18.4%')
    expect(container.textContent).toContain('9.7%')
    expect(getByRole('heading', { name: 'Historical Trend' })).toBeTruthy()
    expect(container.textContent).toContain('42.1%')

    // 模型榜：精确值表格（含并列名次 2、2）。
    const modelSection = document.getElementById('model-rankings') as HTMLElement
    fireEvent.click(within(modelSection).getByRole('button', { name: 'Table' }))
    expect(within(modelSection).getByTestId('model-table')).toBeTruthy()
    expect(within(modelSection).getAllByText('1,240,000').length).toBeGreaterThan(0)
    expect(within(modelSection).getByText('New')).toBeTruthy()

    // 趋势：表格视图提供精确值。
    const trendSection = document.getElementById('trend') as HTMLElement
    fireEvent.click(within(trendSection).getByRole('button', { name: 'Table' }))
    expect(within(trendSection).getByTestId('trend-table')).toBeTruthy()

    // 份额：表格视图含合计行。
    const shareSection = document.getElementById('market-share') as HTMLElement
    fireEvent.click(within(shareSection).getByRole('button', { name: 'Table' }))
    expect(within(shareSection).getByText('Total')).toBeTruthy()
  })

  it('renders the cold-start guide with threshold copy and configured CTAs', async () => {
    installBaseHandlers(() =>
      HttpResponse.json(emptyFixture('week'), { headers: { ETag: '"week-empty"' } }),
    )

    const { findByTestId, getByRole, getAllByText } = await renderWithProviders(
      <Harness />,
      ['public'],
    )
    const guide = await findByTestId('rankings-cold-start')
    expect(within(guide).getByText('The leaderboard is gathering data')).toBeTruthy()
    expect(guide.textContent).toContain('100')
    expect(within(guide).getByRole('link', { name: 'View Pricing' }).getAttribute('href')).toBe('/pricing')
    expect(within(guide).getByRole('link', { name: 'Quick Start' }).getAttribute('href')).toBe(
      'https://docs.example.com',
    )
    expect(within(guide).getByRole('link', { name: 'Sign Up Free' }).getAttribute('href')).toBe(
      '/sign-up',
    )
    // 四区块以「待累积」占位，不留空白。
    expect(getAllByText('Waiting for data')).toHaveLength(4)
    expect(getByRole('tab', { name: 'This Week' })).toBeTruthy()
  })

  it('shows a retryable error for a 503', async () => {
    installBaseHandlers(() =>
      HttpResponse.json(
        { type: 'about:blank', title: 'Service unavailable', status: 503 },
        { status: 503 },
      ),
    )
    const { findByText, getByRole } = await renderWithProviders(
      <Harness />,
      ['public'],
    )
    expect(await findByText('Service temporarily unavailable')).toBeTruthy()
    expect(getByRole('button', { name: 'Retry' })).toBeTruthy()
  })
})

describe('rankings route guard (F-09-29/F-09-35)', () => {
  it('redirects guests to sign-in when require_auth is on', async () => {
    const guarded = statusFixture()
    guarded.nav = {
      ...guarded.nav,
      rankings: { enabled: true, require_auth: true },
    }
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(guarded)),
      http.get(`${ORIGIN}/api/me`, () => HttpResponse.json({}, { status: 401 })),
    )
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    const target = await rankingsAccessRedirect(
      queryClient,
      '/rankings',
      '?period=week',
    )
    expect(target).toBe('/sign-in?redirect=%2Frankings%3Fperiod%3Dweek')
  })

  it('silently redirects home when the section is disabled', async () => {
    const disabled = statusFixture()
    disabled.nav = {
      ...disabled.nav,
      rankings: { enabled: false, require_auth: false },
    }
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(disabled)),
    )
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    expect(
      await rankingsAccessRedirect(queryClient, '/rankings', '?period=week'),
    ).toBe('/')
  })
})
