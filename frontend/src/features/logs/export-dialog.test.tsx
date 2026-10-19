import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { ComponentProps } from 'react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { ExportDialog } from './export-dialog'
import type { UsageLogSearch } from './search'

const ORIGIN = 'http://localhost:3000'

const SEARCH: UsageLogSearch = {
  range: '24h',
  model: 'gpt-4o',
  status: 'error',
  p: 1,
  page_size: 20,
}

const server = setupServer()
let downloaded: { filename: string } | undefined
let exportRequests: URL[] = []

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  vi.restoreAllMocks()
  exportRequests = []
  downloaded = undefined
  Object.defineProperty(navigator, 'onLine', {
    configurable: true,
    value: true,
  })
})
afterAll(() => server.close())

beforeEach(() => {
  // jsdom 不实现 object URL；下载断言只关心文件名与点击一次。
  Object.defineProperty(URL, 'createObjectURL', {
    configurable: true,
    value: vi.fn(() => 'blob:mock'),
  })
  Object.defineProperty(URL, 'revokeObjectURL', {
    configurable: true,
    value: vi.fn(),
  })
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(
    function captureDownload(this: HTMLAnchorElement) {
      downloaded = { filename: this.download }
    },
  )
})

function dialog(
  overrides: Partial<ComponentProps<typeof ExportDialog>> = {},
) {
  const onOpenChange = vi.fn()
  const element = (
    <ExportDialog
      open
      onOpenChange={onOpenChange}
      search={SEARCH}
      filteredTotal={42}
      revealSensitive={false}
      canRevealSensitive={false}
      canFilterByUsername={false}
      {...overrides}
    />
  )
  return { onOpenChange, element }
}

describe('ExportDialog (F-05-07)', () => {
  it('downloads with the server filename and sends the frozen query contract', async () => {
    server.use(
      http.get(`${ORIGIN}/api/usage-logs/export`, ({ request }) => {
        exportRequests.push(new URL(request.url))
        return HttpResponse.text('a\tb', {
          headers: {
            'Content-Disposition':
              'attachment; filename="usage-logs-filtered-20261019-1200.txt"',
            'Content-Type': 'text/plain; charset=utf-8',
          },
        })
      }),
    )
    const user = userEvent.setup()
    const { element, onOpenChange } = dialog()
    await renderWithProviders(element, ['common', 'logs'])

    await user.click(screen.getByTestId('export-format-txt'))
    await user.click(screen.getByTestId('export-download'))

    await waitFor(() => {
      expect(downloaded?.filename).toBe('usage-logs-filtered-20261019-1200.txt')
    })
    expect(onOpenChange).toHaveBeenCalledWith(false)
    const request = exportRequests.at(-1)
    expect(request?.searchParams.get('scope')).toBe('filtered')
    expect(request?.searchParams.get('format')).toBe('txt')
    expect(request?.searchParams.get('reveal_sensitive')).toBe('0')
    expect(request?.searchParams.get('range')).toBe('24h')
    expect(request?.searchParams.get('model')).toBe('gpt-4o')
    expect(request?.searchParams.get('status')).toBe('error')
    // 分页参数不进入导出：导出覆盖整个筛选范围。
    expect(request?.searchParams.has('p')).toBe(false)
  })

  it('exports all visible rows with scope=all and never adds username for a user', async () => {
    server.use(
      http.get(`${ORIGIN}/api/usage-logs/export`, ({ request }) => {
        exportRequests.push(new URL(request.url))
        return HttpResponse.text('# markdown', {
          headers: {
            'Content-Disposition': 'attachment; filename="usage-logs-all.md"',
          },
        })
      }),
    )
    const user = userEvent.setup()
    const { element } = dialog({
      search: { ...SEARCH, username: 'alice' },
    })
    await renderWithProviders(element, ['common', 'logs'])

    await user.click(screen.getByTestId('export-scope-all'))
    await user.click(screen.getByTestId('export-download'))

    await waitFor(() => {
      expect(exportRequests.length).toBe(1)
    })
    const request = exportRequests[0]
    expect(request?.searchParams.get('scope')).toBe('all')
    expect(request?.searchParams.get('format')).toBe('markdown')
    expect(request?.searchParams.has('username')).toBe(false)
  })

  it('keeps the dialog open and shows the row-limit error on 422', async () => {
    server.use(
      http.get(`${ORIGIN}/api/usage-logs/export`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/export_row_limit',
            title: 'Export row limit exceeded',
            detail: 'EXPORT_ROW_LIMIT: export is limited to 10000 rows',
          },
          { status: 422 },
        ),
      ),
    )
    const user = userEvent.setup()
    const { element, onOpenChange } = dialog()
    await renderWithProviders(element, ['common', 'logs'])

    await user.click(screen.getByTestId('export-download'))

    expect(
      await screen.findByText(
        'Up to 10,000 rows per export. Narrow the range or export in batches.',
      ),
    ).toBeTruthy()
    expect(onOpenChange).not.toHaveBeenCalledWith(false)
    expect(screen.getByTestId('export-download')).toBeTruthy()
  })

  it('shows the empty-range message on 204 instead of downloading a file', async () => {
    server.use(
      http.get(
        `${ORIGIN}/api/usage-logs/export`,
        () => new HttpResponse(null, { status: 204 }),
      ),
    )
    const user = userEvent.setup()
    const { element } = dialog()
    await renderWithProviders(element, ['common', 'logs'])

    await user.click(screen.getByTestId('export-download'))

    expect(await screen.findByText('No records to export')).toBeTruthy()
    expect(downloaded).toBeUndefined()
  })

  it('disables Download when the current filter has zero rows', async () => {
    const user = userEvent.setup()
    const { element } = dialog({ filteredTotal: 0 })
    await renderWithProviders(element, ['common', 'logs'])

    const download = screen.getByTestId('export-download') as HTMLButtonElement
    expect(download.disabled).toBe(true)

    // 「全部」不受当前筛选 0 条影响：服务端会给出准确答复（204/文件）。
    await user.click(screen.getByTestId('export-scope-all'))
    expect(download.disabled).toBe(false)
  })

  it('offers the sensitive option only with log.prompt.view', async () => {
    const { element } = dialog({ canRevealSensitive: true })
    const { unmount } = await renderWithProviders(element, ['common', 'logs'])
    expect(screen.getByTestId('export-sensitive')).toBeTruthy()
    unmount()

    const denied = dialog({ canRevealSensitive: false })
    await renderWithProviders(denied.element, ['common', 'logs'])
    expect(screen.queryByTestId('export-sensitive')).toBeNull()
    expect(
      screen.getByText('No permission to include sensitive content'),
    ).toBeTruthy()
  })

  it('shows an inline failure with retry for a server error', async () => {
    let attempts = 0
    server.use(
      http.get(`${ORIGIN}/api/usage-logs/export`, () => {
        attempts += 1
        return HttpResponse.json(
          { type: 'about:blank', title: 'Internal server error' },
          { status: 500 },
        )
      }),
    )
    const user = userEvent.setup()
    const { element } = dialog()
    await renderWithProviders(element, ['common', 'logs'])

    await user.click(screen.getByTestId('export-download'))
    expect(
      await screen.findByText('Export failed. Please retry.'),
    ).toBeTruthy()
    await user.click(screen.getByTestId('export-retry'))
    await waitFor(() => {
      expect(attempts).toBe(2)
    })
  })

  it('disables Download from the first frame when opened offline', async () => {
    Object.defineProperty(navigator, 'onLine', {
      configurable: true,
      value: false,
    })
    const { element } = dialog()
    await renderWithProviders(element, ['common', 'logs'])

    const download = screen.getByTestId('export-download') as HTMLButtonElement
    expect(download.disabled).toBe(true)
    expect(screen.getByText('Network disconnected')).toBeTruthy()
    // 离线时不给「重试」：恢复连接后由用户手动重新发起（§7.5）。
    expect(screen.queryByTestId('export-retry')).toBeNull()
  })

  it('disables Download while the list is still loading', async () => {
    const { element } = dialog({ listLoading: true })
    await renderWithProviders(element, ['common', 'logs'])
    expect(
      (screen.getByTestId('export-download') as HTMLButtonElement).disabled,
    ).toBe(true)
  })

  it('sends reveal_sensitive=1 only when the sensitive option is checked', async () => {
    server.use(
      http.get(`${ORIGIN}/api/usage-logs/export`, ({ request }) => {
        exportRequests.push(new URL(request.url))
        return HttpResponse.text('data', {
          headers: { 'Content-Disposition': 'attachment; filename="x.txt"' },
        })
      }),
    )
    const user = userEvent.setup()
    const { element } = dialog({
      canRevealSensitive: true,
      revealSensitive: false,
    })
    await renderWithProviders(element, ['common', 'logs'])

    const sensitive = screen.getByTestId('export-sensitive') as HTMLInputElement
    expect(sensitive.checked).toBe(false)
    await user.click(sensitive)
    await user.click(screen.getByTestId('export-download'))

    await waitFor(() => {
      expect(exportRequests.length).toBe(1)
    })
    expect(exportRequests[0]?.searchParams.get('reveal_sensitive')).toBe('1')
  })

  it('closes the dialog and calls the standard forbidden path on 403', async () => {
    server.use(
      http.get(`${ORIGIN}/api/usage-logs/export`, () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'Forbidden' },
          { status: 403 },
        ),
      ),
    )
    const user = userEvent.setup()
    const onForbidden = vi.fn()
    const { element, onOpenChange } = dialog({ onForbidden })
    await renderWithProviders(element, ['common', 'logs'])

    await user.click(screen.getByTestId('export-download'))
    await waitFor(() => {
      expect(onForbidden).toHaveBeenCalledTimes(1)
    })
    expect(onOpenChange).toHaveBeenCalledWith(false)
  })
})
