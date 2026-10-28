import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from 'vitest'

import { renderWithProviders } from '@/test-support/providers'
import { useToastStore } from '@/shared/ui/toast'

import type { PerfConfig, PerfModels, PerfOverview } from './api'
import { PerformancePage } from './performance-page'

const ORIGIN = 'http://localhost:3000'
const ISO = '2026-10-20T10:00:00Z'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  vi.useRealTimers()
  // `document.hidden` 在失焦用例里被改成自有属性；删掉后回落到原型 getter。
  Reflect.deleteProperty(document, 'hidden')
  useToastStore.setState({ toasts: [] })
})
afterAll(() => server.close())

const MB = 1024 * 1024

function relativeIso(daysAgo: number): string {
  return new Date(Date.now() - daysAgo * 24 * 60 * 60 * 1000).toISOString()
}

/* ------------------------------------------------------------------------- *
 * 观测端点 fixture（overview / models / config / settings section）
 * ------------------------------------------------------------------------- */

type Vital = PerfOverview['vitals'][number]
type ModelRow = PerfModels['rows'][number]

function vital(overrides: Partial<Vital> = {}): Vital {
  return {
    key: 'gateway.rps',
    value: 21.44,
    unit: 'req/s',
    status: 'ok',
    details: [],
    series: [],
    ...overrides,
  }
}

function overviewFixture(
  overrides: Partial<PerfOverview> = {},
): PerfOverview {
  return {
    generated_at: ISO,
    range: '1h',
    windows: {
      range: '1h',
      start: '2026-10-20T09:00:00Z',
      end: ISO,
      step_seconds: 150,
      points: 24,
    },
    sources: [
      { name: 'prometheus', status: 'ok' },
      { name: 'heartbeat', status: 'ok' },
    ],
    vitals: [
      vital({
        key: 'gateway.rps',
        value: 21.44,
        details: [
          { key: 'p95', value: 1.84, unit: 's' },
          { key: 'errors', value: 0.0002, unit: 'ratio' },
        ],
        series: [
          [1_000, 20],
          [1_001, 21.44],
        ],
      }),
      vital({
        key: 'db.pool',
        value: 11,
        unit: 'conn',
        details: [
          { key: 'tps', value: 342, unit: 'count' },
          { key: 'backends', value: 11, unit: 'count' },
          { key: 'max_backends', value: 25, unit: 'count' },
        ],
      }),
      vital({
        key: 'redis.hit_rate',
        value: 0.962,
        unit: 'ratio',
        details: [
          { key: 'ops', value: 1204, unit: 'count' },
          { key: 'clients', value: 186, unit: 'count' },
        ],
      }),
      vital({
        key: 'host.disk',
        value: 0.71,
        status: 'warn',
        unit: 'ratio',
        details: [
          { key: 'cpu', value: 0.38, unit: 'ratio' },
          { key: 'mem', value: 0.62, unit: 'ratio' },
          { key: 'disk', value: 0.71, unit: 'ratio' },
        ],
      }),
    ],
    gateways: [
      {
        instance_id: 'gateway-1',
        role: 'gateway',
        version: '1.2.3',
        last_seen: ISO,
        heartbeat_fresh: true,
        metrics: {
          p95_ms: 12.4,
          error_rate: 0.0002,
          inflight: 3,
          goroutines: 214,
          heap_mb: 340,
        },
        metrics_status: 'ok',
      },
    ],
    resources: {
      host: {
        cpu: 0.38,
        mem: 0.62,
        disk: 0.71,
        net_in: 1_200_000,
        net_out: 900_000,
      },
      postgres: {
        backends: 11,
        max: 25,
        tps: 342,
        size_bytes: 1024 * MB,
        lock_wait: 0.2,
      },
      redis: {
        up: 1,
        clients: 186,
        mem_ratio: 0.4,
        hit_rate: 0.962,
        cmd_rate: 1204,
      },
    },
    ...overrides,
  }
}

function modelRow(overrides: Partial<ModelRow> = {}): ModelRow {
  return {
    model: 'claude-haiku-3-5',
    channels: 3,
    ttft_p50_ms: 420,
    ttft_p95_ms: 900,
    ttft_p50_over_30s: false,
    ttft_p95_over_30s: false,
    latency_p95_ms: 1800,
    latency_p95_over_30s: false,
    tps: 52.4,
    samples: 1400,
    success_rate: 0.998,
    probe: {
      last_tested_at: ISO,
      ok: true,
      runs: 3,
      ttft_p95_ms: 800,
      latency_p95_ms: 1700,
      success_rate: 1,
    },
    uptime: 0.997,
    status: 'normal',
    spark: Array.from({ length: 24 }, (_, index) => 40 + index),
    ...overrides,
  }
}

function modelsFixture(overrides: Partial<PerfModels> = {}): PerfModels {
  return {
    generated_at: ISO,
    sources: [{ name: 'probes', status: 'ok' }],
    rows: [
      modelRow(),
      modelRow({
        model: 'gpt-4o',
        status: 'incident',
        probe: {
          last_tested_at: '2026-10-20T09:30:00Z',
          ok: false,
          runs: 3,
          ttft_p95_ms: null,
          latency_p95_ms: null,
          success_rate: 0,
        },
        uptime: 0,
      }),
    ],
    summary: { models: 2, degraded: 0, incident: 1, no_sample: 0 },
    ...overrides,
  }
}

function configFixture(overrides: Partial<PerfConfig> = {}): PerfConfig {
  return {
    generated_at: ISO,
    schedule: {
      enabled: true,
      interval_hours: 6,
      runs: 3,
      concurrency: 2,
      source: 'settings',
      timezone: 'UTC',
      next_run_at: '2026-10-20T12:00:00Z',
      legacy_times: '',
    },
    env: [
      {
        key: 'PROMETHEUS_URL',
        value: 'http://prometheus:9090',
        configured: true,
        set: true,
        effect: 'restart',
      },
    ],
    fixed: [
      {
        key: 'probe_timeout',
        value: '30s',
        unit: 'duration',
        effect: 'code',
      },
    ],
    settings_groups: [
      {
        id: 'channel_health',
        tab: 'policy',
        link: '/admin/settings?tab=policy',
        items: [
          {
            key: 'rl.health_test_interval_minutes',
            value: 5,
            type: 'int',
            unit: 'minutes',
            source: 'settings',
          },
        ],
      },
    ],
    ...overrides,
  }
}

function settingsSectionFixture() {
  return {
    section: 'perf',
    updated_at: ISO,
    items: [
      {
        key: 'perf.schedule.enabled',
        value: true,
        type: 'bool',
        default: false,
        public: false,
        secret: false,
        dangerous: false,
      },
      {
        key: 'perf.schedule.interval_hours',
        value: 6,
        type: 'int',
        default: 6,
        public: false,
        secret: false,
        dangerous: false,
      },
      {
        key: 'perf.schedule.runs',
        value: 3,
        type: 'int',
        default: 3,
        public: false,
        secret: false,
        dangerous: false,
      },
      {
        key: 'perf.schedule.concurrency',
        value: 2,
        type: 'int',
        default: 2,
        public: false,
        secret: false,
        dangerous: false,
      },
    ],
  }
}

/* ------------------------------------------------------------------------- *
 * 维护动作端点 fixture
 * ------------------------------------------------------------------------- */

function statsFixture(overrides: Record<string, unknown> = {}) {
  return {
    instance_id: 'gateway-1',
    cache_stats: {
      enabled: true,
      file_count: 1284,
      total_bytes: 812 * MB,
      last_cleared_at: '2026-10-20T10:00:00Z',
    },
    memory_stats: {
      alloc_bytes: 340 * MB,
      sys_bytes: 512 * MB,
      num_gc: 128,
      num_goroutine: 214,
    },
    disk_cache_info: {
      path: '/var/cache/newtg',
      file_count: 1284,
      total_bytes: 812 * MB,
      last_modified: null,
    },
    disk_space_info: {
      total_bytes: 10 * 1024 * MB,
      free_bytes: 4 * 1024 * MB,
      used_bytes: 6 * 1024 * MB,
    },
    config: {
      enabled: true,
      path: '/var/cache/newtg',
      max_mb: 2048,
      threshold_mb: 256,
    },
    ...overrides,
  }
}

function logFile(name: string, size: number, daysAgo: number) {
  return { name, size, mod_time: relativeIso(daysAgo) }
}

function logsFixture(overrides: Record<string, unknown> = {}) {
  const files = [
    logFile('newtg-today.log', 1000, 0),
    logFile('newtg-5d.log', 2000, 5),
    logFile('newtg-40d.log', 3000, 40),
    logFile('newtg-60d.log', 4000, 60),
  ]
  return {
    enabled: true,
    log_dir: '/var/log/newtg',
    file_count: files.length,
    total_size: 10_000,
    oldest_time: files[3]?.mod_time ?? null,
    newest_time: files[0]?.mod_time ?? null,
    files,
    ...overrides,
  }
}

const RECENT = {
  items: [
    {
      time: '2026-10-20T10:00:00Z',
      actor: 'root',
      action: 'system.log_cleanup.run',
      route: 'DELETE /api/performance/logs',
      result: 'success',
      instance_id: 'gateway-1',
      mode: 'by_days',
      value: 30,
      deleted_count: 12,
      freed_bytes: 340 * MB,
      failed_count: 0,
      duration_ms: 0,
      skipped_active: true,
    },
    {
      time: '2026-10-20T09:00:00Z',
      actor: 'root',
      action: 'system.gc.run',
      route: 'POST /api/performance/gc',
      result: 'success',
      instance_id: 'gateway-1',
      deleted_count: 0,
      freed_bytes: 0,
      failed_count: 0,
      duration_ms: 7,
      skipped_active: false,
    },
  ],
}

/* ------------------------------------------------------------------------- *
 * MSW 装配
 * ------------------------------------------------------------------------- */

interface Capture {
  disk: { calls: number; headers: Headers[]; urls: URL[]; bodies: string[] }
  gc: { calls: number; headers: Headers[]; urls: URL[]; bodies: string[] }
  cleanup: { calls: number; headers: Headers[]; urls: URL[]; bodies: string[] }
  counts: {
    overview: number
    models: number
    config: number
    settings: number
    stats: number
    logs: number
    recent: number
  }
  overviewUrls: URL[]
  readHeaders: Headers[]
}

function emptyCapture(): Capture {
  return {
    disk: { calls: 0, headers: [], urls: [], bodies: [] },
    gc: { calls: 0, headers: [], urls: [], bodies: [] },
    cleanup: { calls: 0, headers: [], urls: [], bodies: [] },
    counts: {
      overview: 0,
      models: 0,
      config: 0,
      settings: 0,
      stats: 0,
      logs: 0,
      recent: 0,
    },
    overviewUrls: [],
    readHeaders: [],
  }
}

function installPage(
  options: {
    overview?: PerfOverview
    models?: PerfModels
    config?: PerfConfig
    stats?: Record<string, unknown>
    logs?: Record<string, unknown>
    recent?: Record<string, unknown>
  } = {},
): Capture {
  const capture = emptyCapture()

  server.use(
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({
        username: 'root',
        role: 'admin',
        status: 'enabled',
        permissions: ['settings:manage'],
      }),
    ),
    http.get(`${ORIGIN}/api/performance/overview`, ({ request }) => {
      capture.counts.overview += 1
      const url = new URL(request.url)
      capture.overviewUrls.push(url)
      capture.readHeaders.push(new Headers(request.headers))
      // 服务端回显请求窗：范围切换后断言最新响应属于新窗口。
      const requested = url.searchParams.get('range')
      const range =
        requested === '15m' ||
        requested === '1h' ||
        requested === '6h' ||
        requested === '24h'
          ? requested
          : '1h'
      return HttpResponse.json(
        options.overview ?? overviewFixture({ range }),
      )
    }),
    http.get(`${ORIGIN}/api/performance/models`, ({ request }) => {
      capture.counts.models += 1
      capture.readHeaders.push(new Headers(request.headers))
      return HttpResponse.json(options.models ?? modelsFixture())
    }),
    http.get(`${ORIGIN}/api/performance/config`, ({ request }) => {
      capture.counts.config += 1
      capture.readHeaders.push(new Headers(request.headers))
      return HttpResponse.json(options.config ?? configFixture())
    }),
    http.get(`${ORIGIN}/api/settings`, ({ request }) => {
      capture.counts.settings += 1
      capture.readHeaders.push(new Headers(request.headers))
      return HttpResponse.json(settingsSectionFixture())
    }),
    http.get(`${ORIGIN}/api/performance/stats`, ({ request }) => {
      capture.counts.stats += 1
      capture.readHeaders.push(new Headers(request.headers))
      return HttpResponse.json(options.stats ?? statsFixture())
    }),
    http.get(`${ORIGIN}/api/performance/logs`, ({ request }) => {
      capture.counts.logs += 1
      capture.readHeaders.push(new Headers(request.headers))
      return HttpResponse.json(options.logs ?? logsFixture())
    }),
    http.get(`${ORIGIN}/api/performance/recent-operations`, ({ request }) => {
      capture.counts.recent += 1
      capture.readHeaders.push(new Headers(request.headers))
      return HttpResponse.json(options.recent ?? RECENT)
    }),
    http.delete(`${ORIGIN}/api/performance/disk-cache`, ({ request }) => {
      capture.disk.calls += 1
      capture.disk.headers.push(new Headers(request.headers))
      capture.disk.urls.push(new URL(request.url))
      return HttpResponse.json({
        instance_id: 'gateway-1',
        deleted_count: 12,
        freed_bytes: 340 * MB,
        failed_files: [],
      })
    }),
    http.post(`${ORIGIN}/api/performance/gc`, async ({ request }) => {
      capture.gc.calls += 1
      capture.gc.headers.push(new Headers(request.headers))
      capture.gc.urls.push(new URL(request.url))
      capture.gc.bodies.push(await request.clone().text())
      return HttpResponse.json({
        instance_id: 'gateway-1',
        duration_ms: 7,
        heap_alloc_before: 340 * MB,
        heap_alloc_after: 120 * MB,
        num_gc_before: 128,
        num_gc_after: 129,
      })
    }),
    http.delete(`${ORIGIN}/api/performance/logs`, ({ request }) => {
      capture.cleanup.calls += 1
      capture.cleanup.headers.push(new Headers(request.headers))
      capture.cleanup.urls.push(new URL(request.url))
      return HttpResponse.json({
        instance_id: 'gateway-1',
        mode: capture.cleanup.urls.at(-1)?.searchParams.get('mode'),
        value: Number(
          capture.cleanup.urls.at(-1)?.searchParams.get('value'),
        ),
        deleted_count: 2,
        freed_bytes: 7000,
        skipped_active: false,
        failed_files: [],
      })
    }),
  )

  return capture
}

async function renderPage() {
  await renderWithProviders(<PerformancePage />, ['common', 'admin'])
  await screen.findAllByTestId('vital')
}

/** 打开并确认磁盘缓存清理弹窗（用户路径与页面上的真实按钮一致）。 */
async function confirmDiskClear(user: ReturnType<typeof userEvent.setup>) {
  await user.click(
    screen.getByRole('button', { name: 'Clear inactive disk cache' }),
  )
  await user.click(
    within(await screen.findByRole('alertdialog')).getByRole('button', {
      name: 'Clear inactive disk cache',
    }),
  )
}

/** 打开并确认日志清理弹窗。 */
async function confirmLogCleanup(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('button', { name: 'Clean up log files' }))
  await user.click(
    within(await screen.findByRole('alertdialog')).getByRole('button', {
      name: 'Clean up log files',
    }),
  )
}

function jsonProblem(code: string, status: number) {
  return HttpResponse.json(
    {
      type: `https://newtg.dev/problems/${code}`,
      title: 'Rejected',
      status,
    },
    {
      status,
      headers: { 'Content-Type': 'application/problem+json' },
    },
  )
}

/* ------------------------------------------------------------------------- *
 * 编排结构（adjudications §1）
 * ------------------------------------------------------------------------- */

describe('performance page composition', () => {
  beforeEach(() => {
    useToastStore.setState({ toasts: [] })
  })

  it('renders the header, vitals, six zones and recent operations in order', async () => {
    installPage()
    await renderPage()

    expect(
      screen.getByRole('heading', { level: 1, name: 'Performance' }),
    ).toBeTruthy()
    expect(
      screen.getByText(/Actions apply to the current instance only/),
    ).toBeTruthy()

    // 范围选择器：四个窗口，默认 1h（与 fetchPerfOverview 的缺省一致）。
    const ranges = within(
      screen.getByRole('radiogroup', { name: 'Time range' }),
    ).getAllByRole('radio')
    expect(ranges.map((radio) => radio.textContent)).toEqual([
      '15m',
      '1h',
      '6h',
      '24h',
    ])
    expect(
      screen.getByRole('radio', { name: '1h' }).getAttribute('aria-checked'),
    ).toBe('true')

    // 五格体检条：四格 Prometheus + 模型汇总。
    expect(screen.getAllByTestId('vital')).toHaveLength(5)

    // 模型表。
    const modelRows = screen.getAllByTestId('model-row')
    expect(modelRows).toHaveLength(2)
    expect(modelRows[0]?.textContent).toContain('claude-haiku-3-5')

    // 网关实例表。
    expect(screen.getAllByTestId('gateway-row')).toHaveLength(1)
    expect(screen.getByTestId('gateway-row').textContent).toContain(
      'gateway-1',
    )
    expect(screen.queryByTestId('gateway-local')).toBeNull()

    // 资源三卡。
    expect(screen.getAllByTestId('resource-card')).toHaveLength(3)

    // 运行参数区（测速表单 + 只读参数表）。
    expect(screen.getByTestId('probe-schedule-form')).toBeTruthy()
    expect(screen.getByTestId('settings-zone').textContent).toContain(
      'Model performance probing',
    )

    // 维护三卡原样保留。
    const disk = screen.getByTestId('performance-disk-card')
    expect(disk.textContent).toContain('Enabled')
    expect(disk.textContent).toContain('/var/cache/newtg')
    expect(disk.textContent).toContain('1,284 files')
    const progress = screen.getByRole('progressbar', {
      name: 'Disk cache usage',
    })
    expect(progress.getAttribute('aria-valuenow')).toBe('40')
    expect(progress.getAttribute('data-tone')).toBe('default')
    expect(screen.getByTestId('performance-gc-card').textContent).toContain(
      'GC runs 128',
    )
    expect(screen.getByTestId('performance-logs-card').textContent).toContain(
      '/var/log/newtg',
    )

    // 最近操作。
    const rows = await screen.findAllByTestId('performance-recent-row')
    expect(rows).toHaveLength(2)
    expect(within(rows[0] as HTMLElement).getByText('Log cleanup')).toBeTruthy()
    expect(within(rows[1] as HTMLElement).getByText('7 ms')).toBeTruthy()

    // 所有表头都是列头（读屏按列导航依赖 scope="col"）。
    for (const header of screen.getAllByRole('columnheader')) {
      expect(header.getAttribute('scope')).toBe('col')
    }
  })

  it('shows the degraded banner and empty Prometheus zones when monitoring is down', async () => {
    installPage({
      overview: overviewFixture({
        sources: [
          { name: 'prometheus', status: 'unavailable' },
          { name: 'heartbeat', status: 'ok' },
        ],
      }),
    })
    const { container } = await (async () => {
      const rendered = await renderWithProviders(<PerformancePage />, [
        'common',
        'admin',
      ])
      await screen.findAllByTestId('vital')
      return rendered
    })()

    expect(screen.getByTestId('performance-degraded-banner')).toBeTruthy()

    for (const key of [
      'gateway.rps',
      'db.pool',
      'redis.hit_rate',
      'host.disk',
    ]) {
      const cell = container.querySelector<HTMLElement>(
        `[data-vital="${key}"]`,
      )
      expect(cell?.querySelector('[data-testid="vital-figure"]')?.textContent).toBe(
        '—',
      )
    }

    // 资源区整块退化为空态。
    expect(screen.getByTestId('resources-unavailable')).toBeTruthy()
    // 断线时网关区补本机进程数字。
    expect(screen.getByTestId('gateway-local')).toBeTruthy()
    // 模型数据与维护卡不受监控断线影响。
    expect(screen.getAllByTestId('model-row')).toHaveLength(2)
    expect(screen.getByTestId('performance-disk-card').textContent).toContain(
      '1,284 files',
    )
    expect(screen.getByTestId('performance-gc-card').textContent).toContain(
      'Goroutines 214',
    )
  })

  it('treats a missing prometheus source entry as source down', async () => {
    installPage({
      overview: overviewFixture({
        sources: [{ name: 'heartbeat', status: 'ok' }],
      }),
    })
    await renderWithProviders(<PerformancePage />, ['common', 'admin'])
    await screen.findAllByTestId('vital')

    // 找不到 prometheus 那条不臆造数据源：按断线处理。
    expect(screen.getByTestId('performance-degraded-banner')).toBeTruthy()
    expect(screen.getByTestId('resources-unavailable')).toBeTruthy()
  })

  it('prefers the incident model for the fifth vital subtitle', async () => {
    installPage()
    const { container } = await renderWithProviders(<PerformancePage />, [
      'common',
      'admin',
    ])
    await screen.findAllByTestId('vital')

    const modelCell = container.querySelector<HTMLElement>(
      '[data-vital="models"]',
    )
    // incident（gpt-4o）优先于正常的 claude-haiku-3-5，即便它的探针更早。
    expect(modelCell?.textContent).toContain('gpt-4o')
    expect(modelCell?.textContent).not.toContain('claude-haiku-3-5')
  })

  it('does not request the new endpoints without settings:manage', async () => {
    const capture = installPage()
    server.use(
      http.get(`${ORIGIN}/api/me`, () =>
        HttpResponse.json({
          username: 'reader',
          role: 'admin',
          status: 'enabled',
          permissions: ['users:read'],
        }),
      ),
    )
    await renderWithProviders(<PerformancePage />, ['common', 'admin'])

    expect(
      await screen.findByText(
        'Read-only: your account lacks the settings:manage permission.',
      ),
    ).toBeTruthy()
    const counts = capture.counts
    expect(Object.values(counts)).toEqual([0, 0, 0, 0, 0, 0, 0])
  })

  it('renders the desktop-only notice below 1024px without requesting anything', async () => {
    const capture = installPage()
    vi.spyOn(window, 'matchMedia').mockImplementation(
      (query: string) =>
        ({
          matches: false,
          media: query,
          onchange: null,
          addEventListener: () => {},
          removeEventListener: () => {},
          addListener: () => {},
          removeListener: () => {},
          dispatchEvent: () => false,
        }) as unknown as MediaQueryList,
    )
    await renderWithProviders(<PerformancePage />, ['common', 'admin'])

    expect(
      await screen.findByText(
        'This page is available on desktop screens (1024px and wider).',
      ),
    ).toBeTruthy()
    const counts = capture.counts
    expect(Object.values(counts)).toEqual([0, 0, 0, 0, 0, 0, 0])
  })
})

/* ------------------------------------------------------------------------- *
 * 轮询 / 失焦 / 范围（adjudications §3、§6）
 * ------------------------------------------------------------------------- */

describe('performance page polling', () => {
  it('polls overview every 30s and models every 60s', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const capture = installPage()
    const rendered = await renderWithProviders(<PerformancePage />, [
      'common',
      'admin',
    ])
    await screen.findAllByTestId('vital')

    const base = { ...capture.counts }

    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000)
    })
    expect(capture.counts.overview).toBe(base.overview + 1)
    expect(capture.counts.models).toBe(base.models)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000)
    })
    expect(capture.counts.models).toBe(base.models + 1)

    rendered.unmount()
  })

  it('stops every interval while the tab is hidden', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const capture = installPage()
    const rendered = await renderWithProviders(<PerformancePage />, [
      'common',
      'admin',
    ])
    await screen.findAllByTestId('vital')

    const before = { ...capture.counts }
    Object.defineProperty(document, 'hidden', {
      configurable: true,
      get: () => true,
    })
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'))
    })

    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000)
    })
    expect(capture.counts).toEqual(before)

    rendered.unmount()
  })

  it('changes only the overview request when the range changes', async () => {
    const capture = installPage()
    await renderPage()
    expect(capture.overviewUrls[0]?.searchParams.get('range')).toBe('1h')
    const modelsBefore = capture.counts.models
    const user = userEvent.setup()

    await user.click(screen.getByRole('radio', { name: '6h' }))

    await waitFor(() => expect(capture.counts.overview).toBe(2))
    expect(capture.overviewUrls.at(-1)?.searchParams.get('range')).toBe('6h')
    expect(capture.counts.models).toBe(modelsBefore)
    expect(
      screen.getByRole('radio', { name: '6h' }).getAttribute('aria-checked'),
    ).toBe('true')
  })
})

/* ------------------------------------------------------------------------- *
 * 维护动作（迁出后行为不变）
 * ------------------------------------------------------------------------- */

describe('maintenance actions', () => {
  beforeEach(() => {
    useToastStore.setState({ toasts: [] })
  })

  it('shows explanatory empty states and disables writes when features are off', async () => {
    installPage({
      stats: statsFixture({
        cache_stats: {
          enabled: false,
          file_count: 0,
          total_bytes: 0,
          last_cleared_at: null,
        },
        config: { enabled: false, path: '', max_mb: 0, threshold_mb: 0 },
      }),
      logs: {
        enabled: false,
        log_dir: '',
        file_count: 0,
        total_size: 0,
        oldest_time: null,
        newest_time: null,
        files: [],
      },
      recent: { items: [] },
    })
    await renderPage()

    expect(
      screen.getByText(
        'Disk cache is disabled (turned off by a deployment environment variable).',
      ),
    ).toBeTruthy()
    expect(
      (screen.getByRole('button', {
        name: 'Clear inactive disk cache',
      }) as HTMLButtonElement).disabled,
    ).toBe(true)
    expect(screen.getByText('Log directory is not configured.')).toBeTruthy()
    expect(
      (screen.getByRole('button', {
        name: 'Clean up log files',
      }) as HTMLButtonElement).disabled,
    ).toBe(true)
    expect(screen.getByText('No operations yet.')).toBeTruthy()
  })

  it('previews the log deletion locally without calling the delete endpoint', async () => {
    const capture = installPage()
    await renderPage()
    const user = userEvent.setup()

    // 默认按天数保留 30 天：today(0d) 与 5d 保留，40d/60d 计入删除。
    const preview = screen.getByTestId('performance-logs-preview')
    await waitFor(() => expect(preview.textContent).toContain('2 files'))
    expect(preview.textContent).toContain('6.8 KB')
    expect(capture.cleanup.calls).toBe(0)

    await user.click(screen.getByLabelText('Keep by file count'))
    const input = screen.getByLabelText('Keep value')
    await user.clear(input)
    await user.type(input, '3')

    await waitFor(() => expect(preview.textContent).toContain('1 files'))
    expect(capture.cleanup.calls).toBe(0)
  })

  it('validates the cleanup value before allowing confirmation', async () => {
    const capture = installPage()
    await renderPage()
    const user = userEvent.setup()

    const input = screen.getByLabelText('Keep value')
    await user.clear(input)

    expect(screen.getByText('Enter a keep value.')).toBeTruthy()
    expect(
      (screen.getByRole('button', {
        name: 'Clean up log files',
      }) as HTMLButtonElement).disabled,
    ).toBe(true)

    await user.type(input, '40000')
    expect(
      screen.getByText('Enter a whole number between 1 and 36500.'),
    ).toBeTruthy()
    expect(capture.cleanup.calls).toBe(0)
  })

  it('requires confirmation and sends X-Confirm-Action for disk cache cleanup', async () => {
    const capture = installPage()
    await renderPage()
    const user = userEvent.setup()

    await user.click(
      screen.getByRole('button', { name: 'Clear inactive disk cache' }),
    )
    const dialog = await screen.findByRole('alertdialog')
    expect(capture.disk.calls).toBe(0)
    // 焦点落在取消上，避免回车误触不可逆操作。
    await waitFor(() =>
      expect(document.activeElement?.textContent).toBe('Cancel'),
    )

    await user.click(
      within(dialog).getByRole('button', {
        name: 'Clear inactive disk cache',
      }),
    )

    await waitFor(() => expect(capture.disk.calls).toBe(1))
    expect(capture.disk.headers[0]?.get('X-Confirm-Action')).toBe('true')
    // 写请求回传页面统计所属实例，实例漂移时 409 才可达。
    expect(capture.disk.urls[0]?.searchParams.get('instance_id')).toBe(
      'gateway-1',
    )
    const result = await screen.findByTestId('performance-disk-result')
    expect(result.getAttribute('data-tone')).toBe('success')
    expect(result.textContent).toContain('Deleted 12 files')
    expect(result.textContent).toContain('340.0 MB')
    expect(screen.queryByTestId('performance-offline-snapshot')).toBeNull()
  })

  it('shows a partial-failure warning when some files could not be deleted', async () => {
    const capture = installPage()
    server.use(
      http.delete(`${ORIGIN}/api/performance/disk-cache`, () => {
        capture.disk.calls += 1
        return HttpResponse.json({
          instance_id: 'gateway-1',
          deleted_count: 10,
          freed_bytes: 1000,
          failed_files: ['a.cache', 'b.cache'],
        })
      }),
    )
    await renderPage()
    const user = userEvent.setup()

    await user.click(
      screen.getByRole('button', { name: 'Clear inactive disk cache' }),
    )
    await user.click(
      within(await screen.findByRole('alertdialog')).getByRole('button', {
        name: 'Clear inactive disk cache',
      }),
    )

    const result = await screen.findByTestId('performance-disk-result')
    await waitFor(() =>
      expect(result.getAttribute('data-tone')).toBe('warning'),
    )
    expect(result.textContent).toContain('2 failed')
    // partial 失败是 200 业务结果，不得静默重试。
    expect(capture.disk.calls).toBe(1)
  })

  it('confirms and runs the runtime GC with the confirm header', async () => {
    const capture = installPage()
    await renderPage()
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Run runtime GC' }))
    const dialog = await screen.findByRole('alertdialog')
    await user.click(
      within(dialog).getByRole('button', { name: 'Run runtime GC' }),
    )

    await waitFor(() => expect(capture.gc.calls).toBe(1))
    expect(capture.gc.headers[0]?.get('X-Confirm-Action')).toBe('true')
    // GC 按契约把实例放在 body，而不是 query。
    expect(JSON.parse(capture.gc.bodies[0] ?? '{}')).toEqual({
      instance_id: 'gateway-1',
    })
    const result = await screen.findByTestId('performance-gc-result')
    expect(result.textContent).toContain('7 ms')
    expect(result.textContent).toContain('340.0 MB → 120.0 MB')
  })

  it('confirms and cleans logs with mode and value in the query', async () => {
    const capture = installPage()
    await renderPage()
    const user = userEvent.setup()

    await user.click(
      screen.getByRole('button', { name: 'Clean up log files' }),
    )
    const dialog = await screen.findByRole('alertdialog')
    expect(dialog.textContent).toContain('2 log files')
    expect(dialog.textContent).toContain('This cannot be undone.')
    await user.click(
      within(dialog).getByRole('button', { name: 'Clean up log files' }),
    )

    await waitFor(() => expect(capture.cleanup.calls).toBe(1))
    const url = capture.cleanup.urls[0]
    expect(url?.searchParams.get('mode')).toBe('by_days')
    expect(url?.searchParams.get('value')).toBe('30')
    expect(url?.searchParams.get('instance_id')).toBe('gateway-1')
    expect(capture.cleanup.headers[0]?.get('X-Confirm-Action')).toBe('true')
    const result = await screen.findByTestId('performance-logs-result')
    expect(result.textContent).toContain('Deleted 2 files')
  })

  it('maps server problem codes to localized inline messages', async () => {
    const capture = installPage()
    server.use(
      http.delete(`${ORIGIN}/api/performance/disk-cache`, () => {
        capture.disk.calls += 1
        return jsonProblem('instance_mismatch', 409)
      }),
    )
    await renderPage()
    const user = userEvent.setup()

    await user.click(
      screen.getByRole('button', { name: 'Clear inactive disk cache' }),
    )
    await user.click(
      within(await screen.findByRole('alertdialog')).getByRole('button', {
        name: 'Clear inactive disk cache',
      }),
    )

    const result = await screen.findByTestId('performance-disk-result')
    expect(result.textContent).toContain(
      'The target instance is not the current instance. Switch to it and try again.',
    )
    // 409 是确定性业务错误，handler 只应被调用一次（无静默重试）。
    expect(capture.disk.calls).toBe(1)
  })

  it('shows 429 inline without replacing the page with a full error state', async () => {
    const capture = installPage()
    server.use(
      http.delete(`${ORIGIN}/api/performance/disk-cache`, () => {
        capture.disk.calls += 1
        return HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/rate_limited',
            title: 'Too many requests',
            status: 429,
          },
          {
            status: 429,
            headers: {
              'Content-Type': 'application/problem+json',
              'Retry-After': '12',
            },
          },
        )
      }),
    )
    await renderPage()
    const user = userEvent.setup()

    await user.click(
      screen.getByRole('button', { name: 'Clear inactive disk cache' }),
    )
    await user.click(
      within(await screen.findByRole('alertdialog')).getByRole('button', {
        name: 'Clear inactive disk cache',
      }),
    )

    const result = await screen.findByTestId('performance-disk-result')
    expect(result.textContent).toContain('Too many requests')
    expect(result.textContent).toContain('12 seconds')
    // 429 不在页内退避重发。
    expect(capture.disk.calls).toBe(1)
    // 三张卡仍在页面上，没有退避到整页错误。
    expect(screen.getByTestId('performance-disk-card')).toBeTruthy()
    expect(screen.getByTestId('performance-gc-card')).toBeTruthy()
    expect(screen.getByTestId('performance-logs-card')).toBeTruthy()
  })

  it('keeps the last snapshot and disables writes while offline', async () => {
    installPage()
    await renderPage()

    window.dispatchEvent(new Event('offline'))

    expect(
      await screen.findByTestId('performance-offline-snapshot'),
    ).toBeTruthy()
    expect(
      screen.getByText('Offline snapshot; data may be stale.'),
    ).toBeTruthy()

    for (const name of [
      'Clear inactive disk cache',
      'Run runtime GC',
      'Clean up log files',
    ]) {
      expect(
        (screen.getByRole('button', { name }) as HTMLButtonElement).disabled,
      ).toBe(true)
    }

    // 离线 Toast 只播报一次。
    const messages = useToastStore
      .getState()
      .toasts.map((toast) => toast.message)
    expect(
      messages.filter((message) => message === 'Network connection lost.'),
    ).toHaveLength(1)

    // 恢复在线后写按钮重新可用。
    window.dispatchEvent(new Event('online'))
    await waitFor(() =>
      expect(
        (screen.getByRole('button', {
          name: 'Run runtime GC',
        }) as HTMLButtonElement).disabled,
      ).toBe(false),
    )
  })

  it('renders an inline error with retry when the stats endpoint is rate limited', async () => {
    const capture = installPage()
    server.use(
      http.get(`${ORIGIN}/api/performance/stats`, () => {
        capture.counts.stats += 1
        return HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/rate_limited',
            title: 'Too many requests',
            status: 429,
          },
          {
            status: 429,
            headers: {
              'Content-Type': 'application/problem+json',
              'Retry-After': '5',
            },
          },
        )
      }),
    )
    await renderWithProviders(<PerformancePage />, ['common', 'admin'])

    const error = await screen.findByTestId('performance-disk-error')
    expect(error.textContent).toContain('Too many requests')
    expect(within(error).getByRole('button', { name: 'Retry' })).toBeTruthy()
    // GET 429 同样不自动重试，handler 只被调用一次。
    expect(capture.counts.stats).toBe(1)
    // 其他区照常渲染，429 没有升级成整页错误。
    expect(screen.getAllByTestId('vital')).toHaveLength(5)
    expect(screen.getByTestId('performance-logs-card').textContent).toContain(
      '/var/log/newtg',
    )
    expect(
      (await screen.findAllByTestId('performance-recent-row')).length,
    ).toBeGreaterThan(0)
  })

  it('enters offline (snapshot + disabled writes) when a write fails at transport level', async () => {
    const capture = installPage()
    server.use(
      http.delete(`${ORIGIN}/api/performance/disk-cache`, () => {
        capture.disk.calls += 1
        return HttpResponse.error()
      }),
    )
    await renderPage()
    const user = userEvent.setup()

    await confirmDiskClear(user)

    const result = await screen.findByTestId('performance-disk-result')
    // 传输失败用本地化离线文案，而不是让用户以为可以原样重试。
    expect(result.textContent).toContain(
      'Network connection lost. Check your connection and try again.',
    )
    expect(result.getAttribute('data-tone')).toBe('danger')
    expect(screen.getByTestId('performance-offline-snapshot')).toBeTruthy()
    expect(
      (screen.getByRole('button', {
        name: 'Clear inactive disk cache',
      }) as HTMLButtonElement).disabled,
    ).toBe(true)
    // 传输失败同样只尝试一次。
    expect(capture.disk.calls).toBe(1)

    // 在线事件清除写离线态：写按钮恢复可用，快照提示消失。
    window.dispatchEvent(new Event('online'))
    await waitFor(() =>
      expect(
        screen.queryByTestId('performance-offline-snapshot'),
      ).toBeNull(),
    )
    await waitFor(() =>
      expect(
        (screen.getByRole('button', {
          name: 'Run runtime GC',
        }) as HTMLButtonElement).disabled,
      ).toBe(false),
    )
  })

  it('recovers from the write-offline state when refresh GETs succeed', async () => {
    const capture = installPage()
    server.use(
      http.delete(`${ORIGIN}/api/performance/disk-cache`, () => {
        capture.disk.calls += 1
        return HttpResponse.error()
      }),
    )
    await renderPage()
    const user = userEvent.setup()

    // navigator 保持在线：这条路径不会派发 online 事件，只能靠 GET 成功恢复。
    expect(window.navigator.onLine).toBe(true)
    await confirmDiskClear(user)

    await screen.findByTestId('performance-offline-snapshot')
    expect(
      (screen.getByRole('button', {
        name: 'Clear inactive disk cache',
      }) as HTMLButtonElement).disabled,
    ).toBe(true)
    expect(capture.disk.calls).toBe(1)

    const statsBefore = capture.counts.stats
    await user.click(screen.getByRole('button', { name: 'Refresh' }))

    await waitFor(() =>
      expect(capture.counts.stats).toBe(statsBefore + 1),
    )
    await waitFor(() =>
      expect(
        screen.queryByTestId('performance-offline-snapshot'),
      ).toBeNull(),
    )
    for (const name of [
      'Clear inactive disk cache',
      'Run runtime GC',
      'Clean up log files',
    ]) {
      await waitFor(() =>
        expect(
          (screen.getByRole('button', { name }) as HTMLButtonElement).disabled,
        ).toBe(false),
      )
    }
    // 恢复只来自成功取数，没有额外重发写请求。
    expect(capture.disk.calls).toBe(1)
  })

  it('keeps the write-offline state when the refresh GETs also fail', async () => {
    const capture = installPage()
    server.use(
      http.delete(`${ORIGIN}/api/performance/disk-cache`, () => {
        capture.disk.calls += 1
        return HttpResponse.error()
      }),
    )
    await renderPage()
    const user = userEvent.setup()

    await confirmDiskClear(user)
    await screen.findByTestId('performance-offline-snapshot')

    // 首次取数成功后才把 GET 切到传输失败。
    let failedGets = 0
    server.use(
      http.get(`${ORIGIN}/api/performance/stats`, () => {
        failedGets += 1
        return HttpResponse.error()
      }),
      http.get(`${ORIGIN}/api/performance/logs`, () => HttpResponse.error()),
      http.get(`${ORIGIN}/api/performance/recent-operations`, () =>
        HttpResponse.error(),
      ),
    )
    await user.click(screen.getByRole('button', { name: 'Refresh' }))

    // 取数失败不构成可达证据：离线态与禁用保持。
    await waitFor(() => expect(failedGets).toBe(1))
    expect(screen.getByTestId('performance-offline-snapshot')).toBeTruthy()
    expect(
      (screen.getByRole('button', {
        name: 'Run runtime GC',
      }) as HTMLButtonElement).disabled,
    ).toBe(true)
  })

  it('omits instance_id when stats never loaded (no instance to pin)', async () => {
    const capture = installPage()
    server.use(
      http.get(`${ORIGIN}/api/performance/stats`, () =>
        jsonProblem('internal_error', 500),
      ),
    )
    await renderWithProviders(<PerformancePage />, ['common', 'admin'])
    await screen.findByTestId('performance-logs-preview')
    const user = userEvent.setup()

    await confirmLogCleanup(user)
    await waitFor(() => expect(capture.cleanup.calls).toBe(1))
    expect(capture.cleanup.urls[0]?.searchParams.has('instance_id')).toBe(false)
  })

  it('explains an inaccessible cache directory when enabled but unreadable', async () => {
    installPage({
      stats: statsFixture({
        cache_stats: {
          enabled: false,
          file_count: 0,
          total_bytes: 0,
          last_cleared_at: null,
        },
        config: {
          enabled: true,
          path: '/var/cache/newtg',
          max_mb: 2048,
          threshold_mb: 256,
        },
      }),
    })
    await renderPage()

    expect(screen.getByText('Cache directory is not accessible.')).toBeTruthy()
    expect(
      screen.queryByText(/turned off by a deployment environment variable/),
    ).toBeNull()
    expect(
      (screen.getByRole('button', {
        name: 'Clear inactive disk cache',
      }) as HTMLButtonElement).disabled,
    ).toBe(true)
  })

  it('shows skipped_active on a partial log-cleanup result', async () => {
    const capture = installPage()
    server.use(
      http.delete(`${ORIGIN}/api/performance/logs`, () => {
        capture.cleanup.calls += 1
        return HttpResponse.json({
          instance_id: 'gateway-1',
          mode: 'by_days',
          value: 30,
          deleted_count: 1,
          freed_bytes: 100,
          skipped_active: true,
          failed_files: ['locked.log'],
        })
      }),
    )
    await renderPage()
    const user = userEvent.setup()
    await confirmLogCleanup(user)

    const result = await screen.findByTestId('performance-logs-result')
    await waitFor(() =>
      expect(result.getAttribute('data-tone')).toBe('warning'),
    )
    expect(result.textContent).toContain('1 failed')
    expect(result.textContent).toContain('The active log file was skipped.')
    expect(capture.cleanup.calls).toBe(1)
  })

  it('does not send a request when the confirmation is cancelled', async () => {
    const capture = installPage()
    await renderPage()
    const user = userEvent.setup()

    await user.click(
      screen.getByRole('button', { name: 'Clear inactive disk cache' }),
    )
    const dialog = await screen.findByRole('alertdialog')
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))

    await waitFor(() => expect(screen.queryByRole('alertdialog')).toBeNull())
    expect(capture.disk.calls).toBe(0)
    expect(screen.queryByTestId('performance-disk-result')).toBeNull()
  })

  it('disables both confirm buttons while a write is pending', async () => {
    const capture = installPage()
    let release: (() => void) | undefined
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })
    server.use(
      http.delete(`${ORIGIN}/api/performance/disk-cache`, async () => {
        capture.disk.calls += 1
        await gate
        return HttpResponse.json({
          instance_id: 'gateway-1',
          deleted_count: 0,
          freed_bytes: 0,
          failed_files: [],
        })
      }),
    )
    await renderPage()
    const user = userEvent.setup()

    await user.click(
      screen.getByRole('button', { name: 'Clear inactive disk cache' }),
    )
    const dialog = await screen.findByRole('alertdialog')
    await user.click(
      within(dialog).getByRole('button', {
        name: 'Clear inactive disk cache',
      }),
    )

    const working = await within(dialog).findByRole('button', {
      name: 'Working…',
    })
    await waitFor(() =>
      expect((working as HTMLButtonElement).disabled).toBe(true),
    )
    expect(
      (
        within(dialog).getByRole('button', {
          name: 'Cancel',
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true)
    // pending 期间不能重复提交。
    expect(capture.disk.calls).toBe(1)

    release?.()
    await screen.findByTestId('performance-disk-result')
  })

  it('never sends X-Confirm-Action on read requests', async () => {
    const capture = installPage()
    await renderPage()

    await waitFor(() =>
      expect(capture.readHeaders).toHaveLength(7),
    )
    for (const headers of capture.readHeaders) {
      expect(headers.get('X-Confirm-Action')).toBeNull()
    }
  })

  it.each([
    [
      'confirm_required',
      'The action was not confirmed. Please try again.',
      'disk',
    ],
    ['disk_cache_not_configured', 'Disk cache is not configured.', 'disk'],
    ['log_dir_not_configured', 'Log directory is not configured.', 'logs'],
    ['invalid_mode', 'Invalid cleanup mode.', 'logs'],
    ['invalid_value', 'Invalid value.', 'logs'],
  ] as const)(
    'maps %s to a localized inline message',
    async (code, message, target) => {
      const capture = installPage()
      if (target === 'disk') {
        server.use(
          http.delete(`${ORIGIN}/api/performance/disk-cache`, () => {
            capture.disk.calls += 1
            return jsonProblem(code, 422)
          }),
        )
      } else {
        server.use(
          http.delete(`${ORIGIN}/api/performance/logs`, () => {
            capture.cleanup.calls += 1
            return jsonProblem(code, 422)
          }),
        )
      }
      await renderPage()
      const user = userEvent.setup()

      if (target === 'disk') {
        await confirmDiskClear(user)
        const result = await screen.findByTestId('performance-disk-result')
        await waitFor(() =>
          expect(result.getAttribute('data-tone')).toBe('danger'),
        )
        expect(result.textContent).toContain(message)
        expect(capture.disk.calls).toBe(1)
      } else {
        await confirmLogCleanup(user)
        const result = await screen.findByTestId('performance-logs-result')
        await waitFor(() =>
          expect(result.getAttribute('data-tone')).toBe('danger'),
        )
        expect(result.textContent).toContain(message)
        expect(capture.cleanup.calls).toBe(1)
      }
    },
  )

  it('renders "never cleared" when last_cleared_at is null', async () => {
    installPage({
      stats: statsFixture({
        cache_stats: {
          enabled: true,
          file_count: 0,
          total_bytes: 0,
          last_cleared_at: null,
        },
      }),
    })
    await renderPage()

    expect(screen.getByTestId('performance-disk-card').textContent).toContain(
      'Never cleared',
    )
  })

  it('renders failed and skipped-active recent operations', async () => {
    installPage({
      recent: {
        items: [
          {
            time: '2026-10-20T10:00:00Z',
            actor: 'root',
            action: 'system.log_cleanup.run',
            route: 'DELETE /api/performance/logs',
            result: 'failed',
            instance_id: 'gateway-1',
            mode: 'by_days',
            value: 30,
            deleted_count: 0,
            freed_bytes: 0,
            failed_count: 2,
            duration_ms: 0,
            skipped_active: true,
          },
        ],
      },
    })
    await renderPage()

    const row = (
      await screen.findAllByTestId('performance-recent-row')
    )[0] as HTMLElement
    expect(within(row).getByText('Failed')).toBeTruthy()
    expect(within(row).getByText('Active file skipped')).toBeTruthy()
    expect(within(row).getByText('2 failed')).toBeTruthy()
  })
})
