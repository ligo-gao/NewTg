import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { InstalledTab } from './installed-tab'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const UPDATED_AT = '2026-10-19T10:00:00Z'

function plugin(overrides: Record<string, unknown> = {}) {
  return {
    key: 'kling',
    name: 'Kling',
    icon: null,
    source: 'builtin',
    builtin: true,
    enabled: true,
    active_version: '1.2.0',
    description: null,
    runtime_status: 'registered',
    bound_channels: 0,
    updated_at: UPDATED_AT,
    ...overrides,
  }
}

function page(items: Record<string, unknown>[], total = items.length, pageNumber = 1) {
  return { items, total, page: pageNumber, page_size: 20 }
}

function renderTab(overrides: Partial<Parameters<typeof InstalledTab>[0]> = {}) {
  return renderWithProviders(
    <InstalledTab
      canManage
      online
      factoryDisabledKeys={[]}
      factorySettingsUpdatedAt={null}
      onFactorySaved={() => undefined}
      onGoRuntime={() => undefined}
      onGoUpload={() => undefined}
      {...overrides}
    />,
    ['common', 'admin'],
  )
}

describe('installed tab (F-08-28)', () => {
  it('renders all five runtime badges without hiding compile failures', async () => {
    server.use(
      http.get(`${ORIGIN}/api/plugin/task`, () =>
        HttpResponse.json(
          page([
            plugin({ key: 'a', name: 'A', runtime_status: 'registered' }),
            plugin({ key: 'b', name: 'B', runtime_status: 'disabled_fallback' }),
            plugin({ key: 'c', name: 'C', runtime_status: 'disabled' }),
            plugin({ key: 'd', name: 'D', runtime_status: 'not_registered' }),
            plugin({
              key: 'e',
              name: 'E',
              runtime_status: 'compile_failed',
              compile_error: 'plugin.js:42 compile error',
            }),
          ]),
        ),
      ),
    )

    await renderTab()

    const rows = await screen.findAllByTestId('plugin-row')
    expect(rows).toHaveLength(5)
    expect(within(rows[0] as HTMLElement).getByText('Registered')).toBeTruthy()
    expect(
      within(rows[1] as HTMLElement).getByText('Fallback to uploaded'),
    ).toBeTruthy()
    expect(within(rows[2] as HTMLElement).getByText('Disabled')).toBeTruthy()
    expect(within(rows[3] as HTMLElement).getByText('Not registered')).toBeTruthy()
    expect(within(rows[4] as HTMLElement).getByText('Compile failed')).toBeTruthy()
    // 编译失败行不隐藏，并提供直达完整性页签的入口。
    expect(
      within(rows[4] as HTMLElement).getByRole('button', { name: 'View error' }),
    ).toBeTruthy()
  })

  it('opens the detail drawer on the integrity tab from a compile failure', async () => {
    const failing = plugin({
      key: 'mj',
      name: 'MJ',
      runtime_status: 'compile_failed',
      compile_error: 'plugin.js:42 unexpected token',
      runtime_errors: [
        {
          key: 'mj',
          fieldPath: 'plugin.js:42',
          locale: '',
          rule: 'compile',
          message: 'unexpected token',
        },
      ],
    })
    server.use(
      http.get(`${ORIGIN}/api/plugin/task`, () => HttpResponse.json(page([failing]))),
      http.get(`${ORIGIN}/api/plugin/task/mj`, () =>
        HttpResponse.json({
          plugin: failing,
          versions: [
            {
              id: 'v1',
              key: 'mj',
              version: '0.9.1',
              api_version: 'v1',
              source_hash: 'a'.repeat(64),
              icon: null,
              enabled: true,
              active: true,
              uploaded_by: 7,
              created_at: UPDATED_AT,
            },
          ],
          source_text: 'globalThis.__NEWTG_PLUGIN__ = {}',
          source_version: '0.9.1',
        }),
      ),
    )

    await renderTab()
    await userEvent.click(await screen.findByRole('button', { name: 'View error' }))

    expect(
      await screen.findByRole('tab', { name: 'Integrity', selected: true }),
    ).toBeTruthy()
    const viewer = await screen.findByTestId('plugin-source-viewer')
    // 源查看器只读：无编辑控件，aria-readonly 明示。
    expect(viewer.getAttribute('aria-readonly')).toBe('true')
    expect(within(viewer).queryByRole('textbox')).toBeNull()
    expect(screen.getByText('Read-only')).toBeTruthy()
    expect(screen.getByText(/plugin.js:42: unexpected token/)).toBeTruthy()
  })

  it('sends filters to the server and resets to the first page', async () => {
    const requested: URL[] = []
    server.use(
      http.get(`${ORIGIN}/api/plugin/task`, ({ request }) => {
        requested.push(new URL(request.url))
        return HttpResponse.json(page([plugin()]))
      }),
    )

    await renderTab()
    await screen.findAllByTestId('plugin-row')

    await userEvent.selectOptions(screen.getByLabelText('Source'), 'upload')
    await userEvent.selectOptions(screen.getByLabelText('Runtime status'), 'compile_failed')

    await waitFor(() => {
      const last = requested[requested.length - 1]
      expect(last?.searchParams.get('source')).toBe('upload')
      expect(last?.searchParams.get('runtime_status')).toBe('compile_failed')
      expect(last?.searchParams.get('p')).toBe('1')
    })
  })

  it('shows the registry-not-loaded empty state and deep-links to runtime', async () => {
    server.use(
      http.get(`${ORIGIN}/api/plugin/task`, () => HttpResponse.json(page([], 0))),
    )
    const onGoRuntime = vi.fn()

    await renderTab({ onGoRuntime })

    expect(
      await screen.findByText('The plugin registry is not loaded. See runtime status.'),
    ).toBeTruthy()
    expect(screen.queryByText('No matching plugins.')).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'Open runtime status' }))
    expect(onGoRuntime).toHaveBeenCalledTimes(1)
  })

  it('toggles a plugin optimistically and rolls back on failure', async () => {
    let body: unknown = null
    server.use(
      http.get(`${ORIGIN}/api/plugin/task`, () => HttpResponse.json(page([plugin()]))),
      http.post(`${ORIGIN}/api/plugin/task/kling/status`, async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(
          { message: 'boom' },
          { status: 500, headers: { 'Content-Type': 'application/problem+json' } },
        )
      }),
    )

    await renderTab()
    const toggle = await screen.findByRole('switch', { name: 'Enable Kling' })
    expect(toggle.getAttribute('aria-checked')).toBe('true')

    await userEvent.click(toggle)

    // 请求发出后失败：开关回滚到服务端状态。
    await waitFor(() => expect(body).not.toBeNull())
    await waitFor(() => {
      expect(screen.getByRole('switch', { name: 'Enable Kling' }).getAttribute('aria-checked')).toBe('true')
    })
    expect(body).toMatchObject({ enabled: false, expect_updated_at: UPDATED_AT })
  })

  it('hides write affordances without settings:manage and shows the factory banner', async () => {
    server.use(
      http.get(`${ORIGIN}/api/plugin/task`, () => HttpResponse.json(page([plugin()]))),
    )

    await renderTab({ canManage: false, factoryDisabledKeys: ['kling', 'sora'] })

    expect(
      await screen.findByText('2 built-in plugin(s) are globally disabled'),
    ).toBeTruthy()
    await screen.findAllByTestId('plugin-row')
    expect(screen.queryByRole('switch')).toBeNull()
    expect(screen.queryByRole('button', { name: 'Manage' })).toBeNull()
    expect(screen.getByRole('button', { name: 'Details' })).toBeTruthy()
  })

  it('paginates with an offset summary', async () => {
    const requested: URL[] = []
    server.use(
      http.get(`${ORIGIN}/api/plugin/task`, ({ request }) => {
        const url = new URL(request.url)
        requested.push(url)
        const pageNumber = Number(url.searchParams.get('p') ?? '1')
        return HttpResponse.json(
          page(
            [plugin({ key: `p${pageNumber}`, name: `Plugin ${pageNumber}` })],
            45,
            pageNumber,
          ),
        )
      }),
    )

    await renderTab()
    expect(await screen.findByText('1–20 of 45')).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Next' }))

    await waitFor(() => {
      expect(requested[requested.length - 1]?.searchParams.get('p')).toBe('2')
    })
    expect(await screen.findByText('Page 2 of 3')).toBeTruthy()
    expect(await screen.findByText('21–40 of 45')).toBeTruthy()
  })
})
