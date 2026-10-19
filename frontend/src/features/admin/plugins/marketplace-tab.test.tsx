import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { MarketplaceTab } from './marketplace-tab'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const SOURCE = {
  id: 's1',
  name: 'Official',
  url: 'https://market.example/index.json',
  enabled: true,
  last_check_status: 'ok',
  last_check_at: '2026-10-19T10:00:00Z',
  last_error: null,
}

function marketPlugin(overrides: Record<string, unknown> = {}) {
  return {
    key: 'kling',
    name: 'Kling',
    description: 'Kling video tasks',
    latest_version: '2.0.0',
    versions: [
      {
        version: '2.0.0',
        api_version: 'v1',
        source_url: 'https://market.example/kling-2.0.0.js',
        source_hash: 'c'.repeat(64),
        source_id: 's1',
        installed: false,
      },
    ],
    installed: false,
    installed_version: undefined,
    upgrade_available: false,
    conflicts: [],
    ...overrides,
  }
}

function home(plugins: Record<string, unknown>[], sources = [SOURCE]) {
  return {
    schema_version: 1,
    plugins,
    sources,
    cached_at: '2026-10-19T10:05:00Z',
  }
}

function renderTab(overrides: Partial<Parameters<typeof MarketplaceTab>[0]> = {}) {
  return renderWithProviders(
    <MarketplaceTab canManage online enabled {...overrides} />,
    ['common', 'admin'],
  )
}

describe('marketplace tab (F-08-33/34)', () => {
  it('renders install state badges and installs with activate off by default', async () => {
    let body: unknown = null
    server.use(
      http.get(`${ORIGIN}/api/plugin/task/marketplace`, () =>
        HttpResponse.json(
          home([
            marketPlugin({ key: 'new', name: 'New', latest_version: '1.0.0' }),
            marketPlugin({ key: 'old', name: 'Old', installed: true, installed_version: '2.0.0' }),
            marketPlugin({
              key: 'up',
              name: 'Upgradable',
              installed: true,
              installed_version: '1.0.0',
              upgrade_available: true,
            }),
          ]),
        ),
      ),
      http.post(`${ORIGIN}/api/plugin/task/marketplace/install`, async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(
          {
            installed: true,
            created: true,
            activated: false,
            source_id: 's1',
            key: 'new',
            version: '1.0.0',
            plugin: {},
            version_row: {},
          },
          { status: 201 },
        )
      }),
    )

    await renderTab()

    const cards = await screen.findAllByTestId('marketplace-card')
    expect(cards).toHaveLength(3)
    expect(within(cards[1] as HTMLElement).getByText('Installed')).toBeTruthy()
    expect(within(cards[2] as HTMLElement).getByText('Upgrade available')).toBeTruthy()
    // 已安装且不可升级的卡片没有安装按钮。
    expect(within(cards[1] as HTMLElement).queryByRole('button', { name: 'Install' })).toBeNull()

    await userEvent.click(within(cards[0] as HTMLElement).getByRole('button', { name: 'Install' }))

    const activate = screen.getByRole('checkbox', { name: 'Activate now' })
    expect((activate as HTMLInputElement).checked).toBe(false)
    await userEvent.click(screen.getByRole('button', { name: 'Install' }))

    await waitFor(() =>
      expect(body).toMatchObject({
        source_id: 's1',
        key: 'new',
        version: '2.0.0',
        activate: false,
      }),
    )
  })

  it('refuses installation when the source hash does not match', async () => {
    server.use(
      http.get(`${ORIGIN}/api/plugin/task/marketplace`, () =>
        HttpResponse.json(home([marketPlugin()])),
      ),
      http.post(`${ORIGIN}/api/plugin/task/marketplace/install`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/plugin_hash_mismatch',
            title: 'Plugin hash mismatch',
            status: 422,
            detail: 'the downloaded package hash does not match the index',
          },
          { status: 422, headers: { 'Content-Type': 'application/problem+json' } },
        ),
      ),
    )

    await renderTab()
    const card = await screen.findByTestId('marketplace-card')
    await userEvent.click(within(card).getByRole('button', { name: 'Install' }))
    await userEvent.click(screen.getByRole('button', { name: 'Install' }))

    expect(
      await screen.findByText(
        'Integrity check failed. The source may have been tampered with. Installation refused.',
      ),
    ).toBeTruthy()
  })

  it('shows the no-source empty state and opens source management', async () => {
    server.use(
      http.get(`${ORIGIN}/api/plugin/task/marketplace`, () =>
        HttpResponse.json(home([], [])),
      ),
      http.get(`${ORIGIN}/api/plugin/task/marketplace/sources`, () =>
        HttpResponse.json({ items: [], total: 0 }),
      ),
    )

    await renderTab()
    expect(await screen.findByText('No marketplace source configured.')).toBeTruthy()
    const manageButtons = screen.getAllByRole('button', {
      name: 'Manage marketplace sources',
    })
    await userEvent.click(manageButtons[0] as HTMLElement)

    expect(await screen.findByText('Marketplace sources')).toBeTruthy()
    expect(screen.getAllByText('No marketplace source configured.').length).toBeGreaterThan(0)
  })

  it('validates the https prefix and saves a new source after the danger confirm', async () => {
    let putBody: unknown = null
    server.use(
      http.get(`${ORIGIN}/api/plugin/task/marketplace`, () =>
        HttpResponse.json(home([marketPlugin()])),
      ),
      http.get(`${ORIGIN}/api/plugin/task/marketplace/sources`, () =>
        HttpResponse.json({ items: [SOURCE], total: 1 }),
      ),
      http.put(`${ORIGIN}/api/plugin/task/marketplace/sources`, async ({ request }) => {
        putBody = await request.json()
        return HttpResponse.json({
          items: [SOURCE, { ...SOURCE, id: 's2', name: 'Backup', url: 'https://b.example/i.json' }],
          total: 2,
        })
      }),
    )

    await renderTab()
    await userEvent.click(
      await screen.findByRole('button', { name: 'Manage marketplace sources' }),
    )

    await screen.findByTestId('plugin-source-row')
    await userEvent.type(screen.getByLabelText('Name'), 'Backup')
    await userEvent.type(screen.getByLabelText('https URL'), 'http://b.example/i.json')
    await userEvent.click(screen.getByRole('button', { name: 'Add source' }))
    expect(await screen.findByText('Marketplace sources must use https.')).toBeTruthy()

    await userEvent.clear(screen.getByLabelText('https URL'))
    await userEvent.type(screen.getByLabelText('https URL'), 'https://b.example/i.json')
    await userEvent.click(screen.getByRole('button', { name: 'Add source' }))

    const confirm = await screen.findByRole('alertdialog')
    expect(
      within(confirm).getByText(
        'Marketplace index content will be listed here and can be installed. Make sure you trust the source.',
      ),
    ).toBeTruthy()
    await userEvent.click(within(confirm).getByRole('button', { name: 'Add source' }))

    await waitFor(() =>
      expect(putBody).toMatchObject({
        sources: [
          { id: 's1', name: 'Official', url: SOURCE.url, enabled: true },
          { name: 'Backup', url: 'https://b.example/i.json', enabled: true },
        ],
      }),
    )
  })

  it('renders the disabled state instead of the grid when the marketplace is off', async () => {
    await renderTab({ enabled: false })
    expect(await screen.findByText('The marketplace is disabled.')).toBeTruthy()
  })
})
