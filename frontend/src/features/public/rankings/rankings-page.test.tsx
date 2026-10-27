import { QueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { fireEvent, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import {
  clearRankingsEtagCache,
  type RankingsPeriod,
  type RankingsResponse,
} from './api'
import { rankingsAccessRedirect } from './guard'
import { RankingsPage } from './page'
import { needsRankingsUrlRepair, validateRankingsSearch } from './params'

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

/**
 * The live week snapshot the demo was built from: two models, 191,412,351
 * tokens / 2,928 requests, one provider (`Unknown` — the channel profile is
 * empty). 176,585,172 / 191,412,351 = 92.3%, 2,595 / 2,928 = 88.6%, and
 * 176,585,172 / 2,595 = 68,048 tokens per request.
 */
function rankingsFixture(period: RankingsPeriod): RankingsResponse {
  return {
    period,
    timezone: 'Asia/Shanghai',
    window: {
      start: '2026-10-19T00:00:00+08:00',
      end: '2026-10-25T23:59:00+08:00',
    },
    computed_at: '2026-10-25T10:55:00+08:00',
    freshness_seconds: 300,
    sample_threshold: 100,
    status: 'ok',
    models: [
      {
        rank: 1,
        model: 'deepseek-flash',
        display_name: 'deepseek-flash',
        author: 'Unknown',
        total_tokens: 176_585_172,
        requests: 2_595,
        delta_tokens: null,
        delta_pct: null,
        prev_rank: null,
      },
      {
        rank: 2,
        model: 'kimi-for-coding',
        display_name: 'kimi-for-coding',
        author: 'Unknown',
        total_tokens: 14_827_179,
        requests: 333,
        delta_tokens: null,
        delta_pct: null,
        prev_rank: null,
      },
    ],
    movers: { up: [], down: [] },
    providers: [
      { rank: 1, author: 'Unknown', total_tokens: 191_412_351, share_pct: 100, slot: 1 },
    ],
    trend: {
      granularity: 'day',
      buckets: ['2026-10-24T00:00:00+08:00'],
      series: [
        { model: 'deepseek-flash', slot: 1, tokens: [176_585_172] },
        { model: 'kimi-for-coding', slot: 2, tokens: [14_827_179] },
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
  it('renders the five-cell summary, the usage ladder and the provider share', async () => {
    installBaseHandlers((request) => {
      const url = new URL(request.url)
      const period = (url.searchParams.get('period') ?? 'week') as RankingsPeriod
      return HttpResponse.json(rankingsFixture(period), {
        headers: { ETag: `"${period}-1"` },
      })
    })

    const { findByText, getByTestId, getByText, container } =
      await renderWithProviders(<Harness />, ['public'])

    await findByText('Total tokens')
    expect(getByText('Listed models')).toBeTruthy()
    expect(getByText('Total requests')).toBeTruthy()
    expect(getByText('Providers')).toBeTruthy()
    expect(container.textContent).toContain('191,412,351')
    // 第五格：总 token ÷ 总请求，读数与单位都在同一格里。
    const efficiency = getByTestId('rank-efficiency')
    expect(efficiency.textContent).toContain('65,373')
    expect(efficiency.textContent).toContain('tokens/request')

    const ladder = getByTestId('rank-ladder')
    const rows = within(ladder).getAllByRole('listitem')
    expect(rows).toHaveLength(2)
    expect(rows[0]?.textContent).toContain('deepseek-flash')
    expect(rows[0]?.textContent).toContain('92.3%')
    expect(rows[0]?.textContent).toContain('2,595 requests · 68,048 t/r')
    expect(rows[1]?.textContent).toContain('7.7%')

    // One provider, shown exactly as the API labels it.
    const providers = getByTestId('rank-providers')
    expect(within(providers).getAllByRole('listitem')).toHaveLength(1)
    expect(providers.textContent).toContain('Unknown')
    expect(providers.textContent).toContain('100.0%')
    expect(getByTestId('share-bar')).toBeTruthy()

    // Empty momentum is the real state of the first complete period.
    expect(getByTestId('rank-movers').textContent).toContain(
      'first full period',
    )

    // 实际调用榜来自接口，绝不带示例数据标记；标记只属于右侧测评榜。
    expect(ladder.textContent).not.toMatch(/sample data/i)
    expect(getByTestId('benchmark-ladder').closest('section')?.textContent).toContain(
      'Sample data',
    )
  })

  it('switches the same rows to the request share and the per-request average', async () => {
    installBaseHandlers(() =>
      HttpResponse.json(rankingsFixture('week'), { headers: { ETag: '"week-1"' } }),
    )

    const { findByText, getByTestId, getByRole } = await renderWithProviders(
      <Harness />,
      ['public'],
    )
    await findByText('Total tokens')

    const ladder = getByTestId('rank-ladder')
    expect(ladder.textContent).toContain('92.3%')
    expect(ladder.textContent).toContain('2,595 requests')

    fireEvent.click(getByRole('button', { name: 'Requests' }))

    const rows = within(ladder).getAllByRole('listitem')
    expect(rows).toHaveLength(2)
    expect(rows[0]?.getAttribute('data-model')).toBe('deepseek-flash')
    expect(rows[0]?.textContent).toContain('88.6%')
    expect(rows[0]?.textContent).toContain('2,595 requests · 68,048 t/r')
    expect(rows[1]?.textContent).toContain('11.4%')
  })

  it('folds blank model authors into exactly one Unknown provider row', async () => {
    const fixture = rankingsFixture('week')
    fixture.models = fixture.models.map((model) => ({ ...model, author: '' }))
    fixture.providers = fixture.providers.map((provider) => ({
      ...provider,
      author: '',
    }))
    installBaseHandlers(() =>
      HttpResponse.json(fixture, { headers: { ETag: '"week-blank"' } }),
    )

    const { findByText, getByTestId, queryByRole } = await renderWithProviders(
      <Harness />,
      ['public'],
    )
    // A blank author must not collapse the page into a validation error.
    await findByText('Total tokens')
    expect(queryByRole('alert')).toBeNull()

    const providers = getByTestId('rank-providers')
    expect(within(providers).getAllByRole('listitem')).toHaveLength(1)
    expect(providers.textContent).toContain('Unknown')
  })

  it('renders mover percentages only when the response carries them', async () => {
    const fixture = rankingsFixture('week')
    fixture.movers = {
      up: [
        {
          model: 'deepseek-flash',
          author: 'Unknown',
          delta_pct: 18.4,
          cur_tokens: 176_585_172,
          prev_tokens: 149_000_000,
        },
      ],
      down: [
        {
          model: 'kimi-for-coding',
          author: 'Unknown',
          delta_pct: -9.7,
          cur_tokens: 14_827_179,
          prev_tokens: 16_400_000,
        },
      ],
    }
    installBaseHandlers(() =>
      HttpResponse.json(fixture, { headers: { ETag: '"week-movers"' } }),
    )

    const { findByText, getByTestId } = await renderWithProviders(
      <Harness />,
      ['public'],
    )
    await findByText('Total tokens')

    const movers = getByTestId('rank-movers')
    expect(movers.textContent).toContain('Top gainers')
    expect(movers.textContent).toContain('18.4%')
    expect(movers.textContent).toContain('Top decliners')
    expect(movers.textContent).toContain('-9.7%')
  })

  it('omits the heading for a direction with no movers', async () => {
    const fixture = rankingsFixture('week')
    fixture.movers.up = [
      {
        model: 'deepseek-flash',
        author: 'Unknown',
        delta_pct: 18.4,
        cur_tokens: 176_585_172,
        prev_tokens: 149_000_000,
      },
    ]
    installBaseHandlers(() =>
      HttpResponse.json(fixture, { headers: { ETag: '"week-up"' } }),
    )

    const { findByText, getByTestId, queryByText } = await renderWithProviders(
      <Harness />,
      ['public'],
    )
    await findByText('Total tokens')

    const movers = getByTestId('rank-movers')
    expect(movers.textContent).toContain('Top gainers')
    expect(queryByText('Top decliners')).toBeNull()
    // 只有一侧有数据时不该留下一个空的 <ul>。
    expect(movers.querySelectorAll('ul')).toHaveLength(1)
  })

  it('renders 100% for a lone model and no average when it has no requests', async () => {
    const fixture = rankingsFixture('week')
    fixture.models = [
      {
        rank: 1,
        model: 'deepseek-flash',
        display_name: 'deepseek-flash',
        author: 'Unknown',
        total_tokens: 176_585_172,
        requests: 0,
        delta_tokens: null,
        delta_pct: null,
        prev_rank: null,
      },
    ]
    installBaseHandlers(() =>
      HttpResponse.json(fixture, { headers: { ETag: '"week-zero"' } }),
    )

    const { findByText, getByTestId, container } = await renderWithProviders(
      <Harness />,
      ['public'],
    )
    await findByText('Total tokens')

    const rows = within(getByTestId('rank-ladder')).getAllByRole('listitem')
    expect(rows).toHaveLength(1)
    expect(rows[0]?.textContent).toContain('100.0%')
    // 0 requests → 0 / 0 must never leak a NaN into any readout, and the row
    // drops the per-request average instead of printing Infinity.
    expect(container.textContent).not.toContain('NaN')
    expect(rows[0]?.textContent).not.toContain('t/r')
    expect(within(getByTestId('rank-efficiency')).getByText('—')).toBeTruthy()
  })

  it('renders the statistics panel from the response, not from copy', async () => {
    installBaseHandlers(() =>
      HttpResponse.json(rankingsFixture('week'), { headers: { ETag: '"week-1"' } }),
    )

    const { findByText, getByTestId } = await renderWithProviders(<Harness />, [
      'public',
    ])
    await findByText('Total tokens')

    const facts = getByTestId('rank-facts')
    expect(facts.textContent).toContain('Statistics')
    expect(facts.textContent).toContain('Samples')
    expect(facts.textContent).toContain('2,928')
    expect(facts.textContent).toContain('Live')
    expect(facts.textContent).toContain('All')
    // 冻结的刷新间隔：周榜每 30 分钟。
    expect(facts.textContent).toContain('Every 30 min')
  })

  it('lists eight global benchmark rows and opens a detail dialog from a row', async () => {
    installBaseHandlers(() =>
      HttpResponse.json(rankingsFixture('week'), { headers: { ETag: '"week-1"' } }),
    )

    const { findByText, findByRole, getByTestId, queryByRole } =
      await renderWithProviders(<Harness />, ['public'])
    await findByText('Total tokens')

    const ladder = getByTestId('benchmark-ladder')
    const rows = within(ladder).getAllByRole('button')
    expect(rows).toHaveLength(8)
    expect(rows[0]?.textContent).toContain('Claude Opus 5')
    expect(rows[0]?.textContent).toContain('MMLU 89.2')
    expect(rows[0]?.textContent).toContain('HumanEval 92.0')
    // The third badge depends on the row: Arena Elo or C-Eval.
    expect(rows[0]?.textContent).toContain('1,284')
    expect(rows[4]?.textContent).toContain('C-Eval 91.6')
    expect(rows[0]?.getAttribute('aria-haspopup')).toBe('dialog')

    fireEvent.click(rows[0] as HTMLElement)

    const dialog = await findByRole('dialog')
    expect(within(dialog).getByText('Claude Opus 5')).toBeTruthy()
    expect(dialog.textContent).toContain('Benchmark details')
    expect(dialog.textContent).toContain('General capability')
    expect(dialog.textContent).toContain('Model information')
    expect(dialog.textContent).toMatch(/sample data/i)
    expect(document.body.style.overflow).toBe('hidden')

    fireEvent.click(within(dialog).getByRole('button', { name: 'Close' }))
    await waitFor(() => expect(queryByRole('dialog')).toBeNull())
    expect(document.body.style.overflow).toBe('')
    // 焦点还给触发行，键盘用户不会掉回页面顶部。
    expect(document.activeElement).toBe(rows[0])
  })

  it('opens the dialog from the keyboard and closes it on Escape', async () => {
    installBaseHandlers(() =>
      HttpResponse.json(rankingsFixture('week'), { headers: { ETag: '"week-1"' } }),
    )

    const { findByText, findByRole, getByTestId, queryByRole } =
      await renderWithProviders(<Harness />, ['public'])
    await findByText('Total tokens')

    const row = within(getByTestId('benchmark-ladder')).getAllByRole(
      'button',
    )[1] as HTMLElement
    fireEvent.keyDown(row, { key: 'Enter' })
    expect((await findByRole('dialog')).textContent).toContain('GPT-4.5 Turbo')

    fireEvent.keyDown(document, { key: 'Escape' })
    await waitFor(() => expect(queryByRole('dialog')).toBeNull())
  })

  it('caches each period after the first load', async () => {
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

    const { findByText, getByRole } = await renderWithProviders(
      <Harness />,
      ['public'],
    )
    await findByText('Total tokens')
    expect(requests.filter((entry) => entry.period === 'week')).toHaveLength(1)

    fireEvent.click(getByRole('tab', { name: 'Today' }))
    await waitFor(() =>
      expect(requests.some((entry) => entry.period === 'today')).toBe(true),
    )
    fireEvent.click(getByRole('tab', { name: 'This month' }))
    await waitFor(() =>
      expect(requests.some((entry) => entry.period === 'month')).toBe(true),
    )
    fireEvent.click(getByRole('tab', { name: 'This year' }))
    await waitFor(() =>
      expect(requests.some((entry) => entry.period === 'year')).toBe(true),
    )
    fireEvent.click(getByRole('tab', { name: 'This week' }))
    await findByText('Total tokens')
    fireEvent.click(getByRole('tab', { name: 'Today' }))
    await findByText('Total tokens')

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
    await findAllByText('deepseek-flash')
    fireEvent.click(getByRole('button', { name: 'Refresh' }))

    await waitFor(() => expect(requests).toHaveLength(2))
    expect(requests[1]?.ifNoneMatch).toBe('"week-1"')
    // 304 无 body：沿用上一次的数据，不出现错误态。
    expect((await findAllByText('deepseek-flash')).length).toBeGreaterThan(0)
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

    const { findAllByText, findByText } = await renderWithProviders(
      <Harness />,
      ['public'],
    )
    expect(await findByText(/Too many requests\. Retry in 1s/)).toBeTruthy()
    await waitFor(() => expect(calls).toBe(2), { timeout: 4000 })
    expect((await findAllByText('deepseek-flash')).length).toBeGreaterThan(0)
    expect(await findByText('Total tokens')).toBeTruthy()
  })

  it('renders the cold-start guide with threshold copy and configured CTAs', async () => {
    installBaseHandlers(() =>
      HttpResponse.json(emptyFixture('week'), { headers: { ETag: '"week-empty"' } }),
    )

    const { findByTestId, getByRole, getAllByText, queryByTestId } =
      await renderWithProviders(<Harness />, ['public'])
    const guide = await findByTestId('rankings-cold-start')
    expect(
      within(guide).getByText('The leaderboard is gathering data'),
    ).toBeTruthy()
    expect(guide.textContent).toContain('100')
    expect(within(guide).getByRole('link', { name: 'View pricing' }).getAttribute('href')).toBe('/pricing')
    expect(within(guide).getByRole('link', { name: 'Quick start' }).getAttribute('href')).toBe(
      'https://docs.example.com',
    )
    expect(within(guide).getByRole('link', { name: 'Sign up free' }).getAttribute('href')).toBe(
      '/sign-up',
    )
    // 四区块以「待累积」占位，不留空白；冷启动没有梯子或测评榜。
    expect(getAllByText('Accumulating')).toHaveLength(4)
    // 引导卡与四个占位在同一个 .rank-cold 容器里（40px 节奏由 CSS 提供）。
    const cold = guide.parentElement
    expect(cold?.className).toContain('rank-cold')
    expect(cold?.querySelectorAll('section')).toHaveLength(4)
    expect(queryByTestId('rank-ladder')).toBeNull()
    expect(queryByTestId('benchmark-ladder')).toBeNull()
    expect(getByRole('tab', { name: 'This week' })).toBeTruthy()
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

  it('renders a retryable error when the response fails validation (plain Error)', async () => {
    // normalizeRankings 抛普通 Error（不是 ApiError）；此前查询已失败但
    // `error !== null` 为假，主区渲染空白。必须用 isError 判定。
    installBaseHandlers(() => HttpResponse.json({ unexpected: true }))
    const { findByText, getByRole, queryByTestId } = await renderWithProviders(
      <Harness />,
      ['public'],
    )
    expect(await findByText('Failed to load. Please retry.')).toBeTruthy()
    expect(getByRole('button', { name: 'Retry' })).toBeTruthy()
    expect(queryByTestId('rankings-skeleton')).toBeNull()
  })

  it('stops auto-retry and offers a manual retry after a second consecutive 429', async () => {
    let calls = 0
    installBaseHandlers(() => {
      calls += 1
      return HttpResponse.json(
        { type: 'about:blank', title: 'Too many requests', status: 429 },
        { status: 429, headers: { 'Retry-After': '1' } },
      )
    })
    const { findByText, findByRole, queryByText } = await renderWithProviders(
      <Harness />,
      ['public'],
    )
    expect(await findByText(/Retry in 1s/)).toBeTruthy()
    // 第一次倒计时归零自动重试；第二个 429 必须回到可手动重试的错误态，
    // 而不是永久停在「0 秒」。
    expect(
      await findByRole('button', { name: 'Retry' }, { timeout: 4000 }),
    ).toBeTruthy()
    expect(calls).toBe(2)
    expect(queryByText(/Retry in 0s/)).toBeNull()
  })

  it('warns when the snapshot is stale', async () => {
    installBaseHandlers(() =>
      HttpResponse.json(
        { ...rankingsFixture('week'), freshness_seconds: 7201 },
        { headers: { ETag: '"week-stale"' } },
      ),
    )
    const { findByText } = await renderWithProviders(<Harness />, ['public'])
    expect(
      await findByText('Leaderboard data is delayed; figures are for reference'),
    ).toBeTruthy()
  })

  it('recomputes freshness on a 304 using computed_at and local now', async () => {
    const computedAt = new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString()
    installBaseHandlers((request) => {
      const etag = '"week-old"'
      if (request.headers.get('If-None-Match') === etag) {
        return new Response(null, { status: 304, headers: { ETag: etag } })
      }
      return HttpResponse.json(
        {
          ...rankingsFixture('week'),
          computed_at: computedAt,
          freshness_seconds: 0,
        },
        { headers: { ETag: etag } },
      )
    })
    const delayed = 'Leaderboard data is delayed; figures are for reference'
    const { findByText, queryByText, getByRole } = await renderWithProviders(
      <Harness />,
      ['public'],
    )
    await findByText('Total tokens')
    // 初始响应自报 0s（fresh），刷新命中 304 后按 computed_at 重算成 stale。
    expect(queryByText(delayed)).toBeNull()
    fireEvent.click(getByRole('button', { name: 'Refresh' }))
    expect(await findByText(delayed)).toBeTruthy()
  })

  it('does not send If-None-Match after a cold-start response without ETag', async () => {
    const requests: RequestRecord[] = []
    installBaseHandlers((request) => {
      requests.push({
        period: new URL(request.url).searchParams.get('period'),
        ifNoneMatch: request.headers.get('If-None-Match'),
      })
      // 冷启动快照按契约不下发 ETag。
      return HttpResponse.json(emptyFixture('week'))
    })
    const { findByTestId, getByRole } = await renderWithProviders(
      <Harness />,
      ['public'],
    )
    await findByTestId('rankings-cold-start')
    fireEvent.click(getByRole('button', { name: 'Refresh' }))
    await waitFor(() => expect(requests).toHaveLength(2))
    expect(requests[1]?.ifNoneMatch).toBeNull()
  })

  it('adds times to a same-day window and keeps weekly endpoints date-only', async () => {
    const today = rankingsFixture('today')
    today.window = {
      start: '2026-10-25T00:00:00+08:00',
      end: '2026-10-25T09:24:59+08:00',
    }
    installBaseHandlers((request) => {
      const period = new URL(request.url).searchParams.get('period') ?? 'week'
      const fixture = period === 'today' ? today : rankingsFixture('week')
      return HttpResponse.json(fixture, {
        headers: { ETag: `"${period}-range"` },
      })
    })

    const { findByText, getByRole, container } = await renderWithProviders(
      <Harness initial="today" />,
      ['public'],
    )
    await findByText('Total tokens')

    const windowValue = () =>
      container.querySelector('.rank-window b')?.textContent ?? ''
    // 两端同一天时必须带时间，且是两个不同的字符串（否则打印两次同一天）。
    expect(windowValue()).toMatch(
      /Oct 25, 2026, \d{1,2}:\d{2}\s?[AP]M – Oct 25, 2026, \d{1,2}:\d{2}\s?[AP]M/,
    )

    fireEvent.click(getByRole('tab', { name: 'This week' }))
    await waitFor(() =>
      expect(windowValue()).toBe('Oct 19, 2026 – Oct 25, 2026'),
    )
  })

  it('falls back to the local timezone when the response timezone is invalid', async () => {
    installBaseHandlers(() =>
      HttpResponse.json(
        { ...rankingsFixture('week'), timezone: 'Mars/Olympus' },
        { headers: { ETag: '"week-bad-tz"' } },
      ),
    )
    const { findByRole, container } = await renderWithProviders(
      <Harness />,
      ['public'],
    )
    expect(
      await findByRole('heading', { name: 'Statistics' }),
    ).toBeTruthy()
    // 站点时区非法时回退浏览器时区，窗口日期仍可读且不崩页。
    expect(container.textContent).toContain('2026')
  })

  it('normalizes an invalid period in the URL to the default week', () => {
    expect(validateRankingsSearch({ period: 'decade' })).toEqual({
      period: 'week',
    })
    expect(validateRankingsSearch({ period: 'today' })).toEqual({
      period: 'today',
    })
    expect(needsRankingsUrlRepair('?period=decade')).toBe(true)
    expect(needsRankingsUrlRepair('?period=week')).toBe(false)
    expect(needsRankingsUrlRepair('?period=week&period=month')).toBe(true)
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
