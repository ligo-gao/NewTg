import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { PluginDetailDrawer, type PluginDetailTab } from './detail-drawer'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const UPDATED_AT = '2026-10-19T10:00:00Z'

function version(overrides: Record<string, unknown> = {}) {
  return {
    id: 'v-1.0.0',
    key: 'kling',
    version: '1.0.0',
    api_version: 'v1',
    source_hash: 'a'.repeat(64),
    icon: null,
    enabled: true,
    active: false,
    uploaded_by: 3,
    created_at: UPDATED_AT,
    ...overrides,
  }
}

function detail(overrides: Record<string, unknown> = {}) {
  return {
    plugin: {
      key: 'kling',
      name: 'Kling',
      icon: null,
      source: 'builtin',
      builtin: true,
      enabled: true,
      active_version: '1.0.0',
      description: 'Kling video tasks',
      runtime_status: 'registered',
      bound_channels: 0,
      updated_at: UPDATED_AT,
    },
    versions: [
      version({ id: 'v-2', version: '2.0.0', source_hash: 'b'.repeat(64), active: false }),
      version({ id: 'v-1', version: '1.0.0', active: true }),
    ],
    source_text: 'line one\nline two',
    source_version: '1.0.0',
    ...overrides,
  }
}

function renderDrawer(
  initialTab: PluginDetailTab = 'metadata',
  overrides: Partial<Parameters<typeof PluginDetailDrawer>[0]> = {},
) {
  return renderWithProviders(
    <PluginDetailDrawer
      open
      pluginKey="kling"
      initialTab={initialTab}
      canManage
      online
      onClose={() => undefined}
      onChanged={() => undefined}
      {...overrides}
    />,
    ['common', 'admin'],
  )
}

function installDetail(data = detail()) {
  server.use(
    http.get(`${ORIGIN}/api/plugin/task/kling`, () => HttpResponse.json(data)),
  )
}

describe('plugin detail drawer (F-08-29/31)', () => {
  it('renders the six tabs and metadata', async () => {
    installDetail()
    await renderDrawer()

    expect(await screen.findByRole('tab', { name: 'Metadata' })).toBeTruthy()
    for (const name of [
      'Endpoints',
      'Models',
      'Usage schema',
      'Integrity',
      'Change history',
    ]) {
      expect(screen.getByRole('tab', { name: new RegExp(name) })).toBeTruthy()
    }
    expect(await screen.findByText('Kling video tasks')).toBeTruthy()
    expect(screen.getAllByText('kling').length).toBeGreaterThan(0)
  })

  it('marks versions without data as not exposed instead of faking tables', async () => {
    installDetail()
    await renderDrawer()
    await userEvent.click(await screen.findByRole('tab', { name: 'Endpoints' }))
    expect(
      screen.getByText(
        'The current API does not expose this structured declaration for the selected version.',
      ),
    ).toBeTruthy()
  })

  it('activates a version through an alertdialog whose initial focus is Cancel', async () => {
    let body: unknown = null
    installDetail()
    server.use(
      http.post(`${ORIGIN}/api/plugin/task/kling/activate`, async ({ request }) => {
        body = await request.json()
        return HttpResponse.json({ plugin: detail().plugin, version: version() })
      }),
    )

    await renderDrawer('history')
    const rows = await screen.findAllByTestId('plugin-version-row')
    expect(rows).toHaveLength(2)
    await userEvent.click(within(rows[0] as HTMLElement).getByRole('button', { name: 'Activate' }))

    const confirm = await screen.findByRole('alertdialog')
    await waitFor(() =>
      expect(confirm.contains(document.activeElement)).toBe(true),
    )
    expect((document.activeElement as HTMLElement).textContent).toBe('Cancel')
    expect(
      within(confirm).getByText(
        'The new version must still parse responses of in-flight tasks. If it fails, activate an older version here to roll back.',
      ),
    ).toBeTruthy()

    await userEvent.click(within(confirm).getByRole('button', { name: 'Activate' }))
    await waitFor(() =>
      expect(body).toMatchObject({ version: '2.0.0', expect_updated_at: UPDATED_AT }),
    )
    expect(await screen.findByText('Version 1.0.0 activated.')).toBeTruthy()
  })

  it('blocks deleting the only version and confirms active-version promotion', async () => {
    installDetail(
      detail({
        versions: [version({ id: 'only', version: '1.0.0', active: true })],
      }),
    )
    await renderDrawer('history')

    const row = await screen.findByTestId('plugin-version-row')
    const deleteButton = within(row).getByRole('button', {
      name: 'Delete',
    }) as HTMLButtonElement
    expect(deleteButton.disabled).toBe(true)
    expect(screen.getByText('The only version cannot be deleted.')).toBeTruthy()
  })

  it('confirms deletion of an active version with the promotion warning', async () => {
    installDetail()
    await renderDrawer('history')
    const rows = await screen.findAllByTestId('plugin-version-row')
    const activeRow = rows.find((row) => row.textContent?.includes('Current'))
    expect(activeRow).toBeTruthy()
    await userEvent.click(
      within(activeRow as HTMLElement).getByRole('button', { name: 'Delete' }),
    )

    const confirm = await screen.findByRole('alertdialog')
    expect(
      within(confirm).getByText(
        'Deleting the active version promotes the previous version. In-flight tasks are unaffected.',
      ),
    ).toBeTruthy()
  })

  it('compares a version against the active source with a read-only diff', async () => {
    installDetail()
    server.use(
      http.get(`${ORIGIN}/api/plugin/task/kling/source`, ({ request }) => {
        const url = new URL(request.url)
        return HttpResponse.json({
          key: 'kling',
          version: url.searchParams.get('version') ?? '1.0.0',
          source_hash: 'b'.repeat(64),
          source_text: 'line one\nline changed',
        })
      }),
    )

    await renderDrawer('history')
    const rows = await screen.findAllByTestId('plugin-version-row')
    await userEvent.click(
      within(rows[0] as HTMLElement).getByRole('button', { name: 'Compare' }),
    )

    const diff = await screen.findByTestId('plugin-source-diff')
    expect(diff.textContent).toContain('line changed')
    expect(diff.textContent).toContain('line two')
    expect(within(diff).queryByRole('textbox')).toBeNull()
  })

  it('shows compile errors in the integrity tab', async () => {
    installDetail(
      detail({
        plugin: {
          ...detail().plugin,
          runtime_status: 'compile_failed',
          compile_error: 'plugin.js:42 unexpected token',
          runtime_errors: [
            {
              key: 'kling',
              fieldPath: 'plugin.js:42',
              locale: '',
              rule: 'compile',
              message: 'unexpected token',
            },
          ],
        },
      }),
    )
    await renderDrawer('integrity')

    const viewer = await screen.findByTestId('plugin-source-viewer')
    expect(viewer.getAttribute('aria-readonly')).toBe('true')
    expect(screen.getByText(/plugin.js:42: unexpected token/)).toBeTruthy()
    expect(screen.getByText('Read-only')).toBeTruthy()
  })
})
