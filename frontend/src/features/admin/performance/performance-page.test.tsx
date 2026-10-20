import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'
import { useToastStore } from '@/shared/ui/toast'

import { PerformancePage } from './performance-page'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  useToastStore.setState({ toasts: [] })
})
afterAll(() => server.close())

const MB = 1024 * 1024

function relativeIso(daysAgo: number): string {
  return new Date(Date.now() - daysAgo * 24 * 60 * 60 * 1000).toISOString()
}

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

interface Capture {
  /** MSW handler 被调用的次数：断言没有静默重试（>1 即重试）。 */
  calls: number
  headers: Headers[]
  urls: URL[]
  bodies: string[]
}

interface GetCapture {
  stats: number
  logs: number
  recent: number
  headers: Headers[]
}

function installPage(options: {
  stats?: Record<string, unknown>
  logs?: Record<string, unknown>
  recent?: Record<string, unknown>
} = {}): {
  disk: Capture
  gc: Capture
  cleanup: Capture
  gets: GetCapture
} {
  const disk: Capture = { calls: 0, headers: [], urls: [], bodies: [] }
  const gc: Capture = { calls: 0, headers: [], urls: [], bodies: [] }
  const cleanup: Capture = { calls: 0, headers: [], urls: [], bodies: [] }
  const gets: GetCapture = { stats: 0, logs: 0, recent: 0, headers: [] }

  server.use(
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({
        username: 'root',
        role: 'admin',
        status: 'enabled',
        permissions: ['settings:manage'],
      }),
    ),
    http.get(`${ORIGIN}/api/performance/stats`, ({ request }) => {
      gets.stats += 1
      gets.headers.push(new Headers(request.headers))
      return HttpResponse.json(options.stats ?? statsFixture())
    }),
    http.get(`${ORIGIN}/api/performance/logs`, ({ request }) => {
      gets.logs += 1
      gets.headers.push(new Headers(request.headers))
      return HttpResponse.json(options.logs ?? logsFixture())
    }),
    http.get(`${ORIGIN}/api/performance/recent-operations`, ({ request }) => {
      gets.recent += 1
      gets.headers.push(new Headers(request.headers))
      return HttpResponse.json(options.recent ?? RECENT)
    }),
    http.delete(`${ORIGIN}/api/performance/disk-cache`, ({ request }) => {
      disk.calls += 1
      disk.headers.push(new Headers(request.headers))
      disk.urls.push(new URL(request.url))
      return HttpResponse.json({
        instance_id: 'gateway-1',
        deleted_count: 12,
        freed_bytes: 340 * MB,
        failed_files: [],
      })
    }),
    http.post(`${ORIGIN}/api/performance/gc`, async ({ request }) => {
      gc.calls += 1
      gc.headers.push(new Headers(request.headers))
      gc.urls.push(new URL(request.url))
      gc.bodies.push(await request.clone().text())
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
      cleanup.calls += 1
      cleanup.headers.push(new Headers(request.headers))
      cleanup.urls.push(new URL(request.url))
      return HttpResponse.json({
        instance_id: 'gateway-1',
        mode: cleanup.urls[cleanup.urls.length - 1]?.searchParams.get('mode'),
        value: Number(
          cleanup.urls[cleanup.urls.length - 1]?.searchParams.get('value'),
        ),
        deleted_count: 2,
        freed_bytes: 7000,
        skipped_active: false,
        failed_files: [],
      })
    }),
  )
  return { disk, gc, cleanup, gets }
}

async function renderPage() {
  await renderWithProviders(<PerformancePage />, ['common', 'admin'])
  await screen.findByText('Instance gateway-1')
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

describe('performance page (F-08-48～F-08-50)', () => {
  beforeEach(() => {
    useToastStore.setState({ toasts: [] })
  })

  it('renders the page header, three cards and recent operations', async () => {
    installPage()
    await renderPage()

    expect(screen.getByRole('heading', { level: 1, name: 'Performance' })).toBeTruthy()
    expect(screen.getByText(/Actions apply to the current instance only/)).toBeTruthy()

    const disk = screen.getByTestId('performance-disk-card')
    expect(disk.textContent).toContain('Enabled')
    expect(disk.textContent).toContain('/var/cache/newtg')
    expect(disk.textContent).toContain('1,284 files')
    expect(disk.textContent).toContain('812.0 MB')
    expect(disk.textContent).toContain('2.0 GB')
    expect(disk.textContent).toContain('Threshold 256.0 MB')

    const progress = screen.getByRole('progressbar', {
      name: 'Disk cache usage',
    })
    expect(progress.getAttribute('aria-valuemin')).toBe('0')
    expect(progress.getAttribute('aria-valuemax')).toBe('100')
    expect(progress.getAttribute('aria-valuenow')).toBe('40')
    expect(progress.getAttribute('data-tone')).toBe('default')

    const gcCard = screen.getByTestId('performance-gc-card')
    expect(gcCard.textContent).toContain('Heap allocation 340.0 MB')
    expect(gcCard.textContent).toContain('GC runs 128')
    expect(gcCard.textContent).toContain('Goroutines 214')

    const logsCard = screen.getByTestId('performance-logs-card')
    expect(logsCard.textContent).toContain('/var/log/newtg')
    expect(logsCard.textContent).toContain('The active log file is never deleted.')

    const rows = await screen.findAllByTestId('performance-recent-row')
    expect(rows).toHaveLength(2)
    expect(within(rows[0] as HTMLElement).getByText('Log cleanup')).toBeTruthy()
    expect(
      within(rows[0] as HTMLElement).getByText('Keep by days · 30'),
    ).toBeTruthy()
    expect(within(rows[0] as HTMLElement).getByText('Success')).toBeTruthy()
    expect(within(rows[1] as HTMLElement).getByText('7 ms')).toBeTruthy()

    // 表头是列头，读屏按列导航依赖 scope="col"。
    for (const header of screen.getAllByRole('columnheader')) {
      expect(header.getAttribute('scope')).toBe('col')
    }
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
      within(dialog).getByRole('button', { name: 'Clear inactive disk cache' }),
    )

    await waitFor(() => expect(capture.disk.calls).toBe(1))
    expect(capture.disk.headers[0]?.get('X-Confirm-Action')).toBe('true')
    // L5：写请求回传页面统计所属实例，实例漂移时 409 才可达。
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
    await waitFor(() => expect(result.getAttribute('data-tone')).toBe('warning'))
    expect(result.textContent).toContain('2 failed')
    // L3：partial 失败是 200 业务结果，不得静默重试（handler 只被调用一次）。
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
    // L5：GC 按契约把实例放在 body，而不是 query。
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
        return HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/instance_mismatch',
            title: 'Instance mismatch',
            status: 409,
          },
          { status: 409, headers: { 'Content-Type': 'application/problem+json' } },
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
    expect(result.textContent).toContain(
      'The target instance is not the current instance. Switch to it and try again.',
    )
    // L3：409 是确定性业务错误，handler 只应被调用一次（无静默重试）。
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
    // L3：429 不在页内退避重发，handler 只被调用一次。
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
    expect(screen.getByText('Offline snapshot; data may be stale.')).toBeTruthy()

    expect(
      (screen.getByRole('button', {
        name: 'Clear inactive disk cache',
      }) as HTMLButtonElement).disabled,
    ).toBe(true)
    expect(
      (screen.getByRole('button', {
        name: 'Run runtime GC',
      }) as HTMLButtonElement).disabled,
    ).toBe(true)
    expect(
      (screen.getByRole('button', {
        name: 'Clean up log files',
      }) as HTMLButtonElement).disabled,
    ).toBe(true)

    // 离线 Toast 只播报一次。
    const messages = useToastStore
      .getState()
      .toasts.map((toast) => toast.message)
    expect(messages.filter((message) => message === 'Network connection lost.')).toHaveLength(1)

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

  it('renders an inline error with retry when a card endpoint is rate limited', async () => {
    const capture = installPage()
    server.use(
      http.get(`${ORIGIN}/api/performance/stats`, () => {
        capture.gets.stats += 1
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
    // L3：GET 429 同样不自动重试，handler 只被调用一次。
    expect(capture.gets.stats).toBe(1)
    // 其他卡照常渲染，429 没有升级成整页错误。
    expect(screen.getByTestId('performance-logs-card').textContent).toContain(
      '/var/log/newtg',
    )
    expect((await screen.findAllByTestId('performance-recent-row')).length).toBeGreaterThan(0)
  })

  // -------------------------------------------------------------------------
  // M1：写操作传输失败并入离线态
  // -------------------------------------------------------------------------
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
      expect(screen.queryByTestId('performance-offline-snapshot')).toBeNull(),
    )
    await waitFor(() =>
      expect(
        (screen.getByRole('button', {
          name: 'Run runtime GC',
        }) as HTMLButtonElement).disabled,
      ).toBe(false),
    )
  })

  // -------------------------------------------------------------------------
  // L5：写请求钉住页面展示实例
  // -------------------------------------------------------------------------
  it('omits instance_id when stats never loaded (no instance to pin)', async () => {
    const capture = installPage()
    server.use(
      http.get(`${ORIGIN}/api/performance/stats`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/internal_error',
            title: 'Internal Server Error',
            status: 500,
          },
          {
            status: 500,
            headers: { 'Content-Type': 'application/problem+json' },
          },
        ),
      ),
    )
    await renderWithProviders(<PerformancePage />, ['common', 'admin'])
    await screen.findByTestId('performance-logs-preview')
    const user = userEvent.setup()

    await confirmLogCleanup(user)
    await waitFor(() => expect(capture.cleanup.calls).toBe(1))
    expect(capture.cleanup.urls[0]?.searchParams.has('instance_id')).toBe(false)
  })

  // -------------------------------------------------------------------------
  // L6：空态原因由 config.enabled 区分
  // -------------------------------------------------------------------------
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

    expect(
      screen.getByText('Cache directory is not accessible.'),
    ).toBeTruthy()
    expect(
      screen.queryByText(/turned off by a deployment environment variable/),
    ).toBeNull()
    expect(
      (screen.getByRole('button', {
        name: 'Clear inactive disk cache',
      }) as HTMLButtonElement).disabled,
    ).toBe(true)
  })

  // -------------------------------------------------------------------------
  // L2：部分失败结果行也说明活动文件被跳过
  // -------------------------------------------------------------------------
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
    await waitFor(() => expect(result.getAttribute('data-tone')).toBe('warning'))
    expect(result.textContent).toContain('1 failed')
    expect(result.textContent).toContain('The active log file was skipped.')
    expect(capture.cleanup.calls).toBe(1)
  })

  // -------------------------------------------------------------------------
  // L4：确认弹窗与请求边界
  // -------------------------------------------------------------------------
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
      within(dialog).getByRole('button', { name: 'Clear inactive disk cache' }),
    )

    const working = await within(dialog).findByRole('button', {
      name: 'Working…',
    })
    await waitFor(() =>
      expect((working as HTMLButtonElement).disabled).toBe(true),
    )
    expect(
      (within(dialog).getByRole('button', {
        name: 'Cancel',
      }) as HTMLButtonElement).disabled,
    ).toBe(true)
    // pending 期间不能重复提交。
    expect(capture.disk.calls).toBe(1)

    release?.()
    await screen.findByTestId('performance-disk-result')
  })

  it('never sends X-Confirm-Action on read requests', async () => {
    const capture = installPage()
    await renderPage()

    await waitFor(() => expect(capture.gets.recent).toBe(1))
    expect(capture.gets.stats).toBe(1)
    expect(capture.gets.logs).toBe(1)
    for (const headers of capture.gets.headers) {
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
      const problem = () =>
        HttpResponse.json(
          {
            type: `https://newtg.dev/problems/${code}`,
            title: 'Rejected',
            status: 422,
          },
          {
            status: 422,
            headers: { 'Content-Type': 'application/problem+json' },
          },
        )
      if (target === 'disk') {
        server.use(
          http.delete(`${ORIGIN}/api/performance/disk-cache`, () => {
            capture.disk.calls += 1
            return problem()
          }),
        )
      } else {
        server.use(
          http.delete(`${ORIGIN}/api/performance/logs`, () => {
            capture.cleanup.calls += 1
            return problem()
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

    const row = (await screen.findAllByTestId('performance-recent-row'))[0] as HTMLElement
    expect(within(row).getByText('Failed')).toBeTruthy()
    expect(within(row).getByText('Active file skipped')).toBeTruthy()
    expect(within(row).getByText('2 failed')).toBeTruthy()
  })
})
