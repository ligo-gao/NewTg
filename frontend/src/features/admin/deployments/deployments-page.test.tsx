import { useState } from 'react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import {
  ModelDeploymentsPage,
  type DeploymentsTabId,
} from './deployments-page'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const SETTINGS = {
  enabled: true,
  has_secret: true,
  can_connect: true,
  updated_at: '2026-10-19T10:00:00Z',
}

function deployment(overrides: Record<string, unknown>) {
  return {
    id: 'd1',
    name: 'llm-a',
    status: 'running',
    completed_percent: 40,
    gpu_count: 2,
    replica_count: 4,
    hardware_name: 'A100',
    brand_name: 'NVIDIA',
    compute_minutes_served: 120,
    compute_minutes_remaining: 750,
    created_at: '2026-10-18T10:00:00Z',
    ...overrides,
  }
}

function installActor(permissions: string[]) {
  server.use(
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({
        username: 'root',
        role: 'admin',
        status: 'enabled',
        permissions,
      }),
    ),
  )
}

function installSettings(settings = SETTINGS) {
  server.use(
    http.get(`${ORIGIN}/api/deployments/settings`, () =>
      HttpResponse.json(settings),
    ),
  )
}

function installList(list: Record<string, unknown>) {
  server.use(http.get(`${ORIGIN}/api/deployments`, () => HttpResponse.json(list)))
}

function Harness({ initialTab = 'deployments' as DeploymentsTabId }) {
  const [tab, setTab] = useState<DeploymentsTabId>(initialTab)
  return <ModelDeploymentsPage tab={tab} onTabChange={setTab} />
}

describe('model deployments list tab (F-08-52)', () => {
  it('renders rows, six-state badges, unknown status and status counts', async () => {
    installActor(['settings:manage'])
    installSettings()
    installList({
      items: [
        deployment({}),
        deployment({ id: 'd2', name: 'llm-b', status: 'termination requested' }),
        deployment({ id: 'd3', name: 'llm-c', status: 'provisioning' }),
      ],
      total: 3,
      page: 1,
      page_size: 20,
      status_counts: {
        all: 3,
        running: 1,
        'termination requested': 1,
        provisioning: 1,
      },
    })

    await renderWithProviders(<Harness />, ['common', 'admin'])

    const rows = await screen.findAllByTestId('deployment-row')
    expect(rows).toHaveLength(3)
    expect(within(rows[0] as HTMLElement).getByText('Running')).toBeTruthy()
    expect(
      within(rows[1] as HTMLElement).getByText('Termination requested'),
    ).toBeTruthy()
    // 未知识别为中性徽章但状态原文照常显示（不隐藏）。
    expect(within(rows[2] as HTMLElement).getByText('provisioning')).toBeTruthy()

    const counts = screen.getByTestId('deployment-status-counts')
    expect(counts.textContent).toContain('All')
    expect(counts.textContent).toContain('Running')
    expect(counts.textContent).toContain('3')
    // offset 分页摘要
    expect(screen.getByText('1–3 of 3')).toBeTruthy()
  })

  it('sends the keyword to the server and shows the truncation warning', async () => {
    installActor(['settings:manage'])
    installSettings()
    let requestedKeyword = ''
    server.use(
      http.get(`${ORIGIN}/api/deployments`, ({ request }) => {
        const url = new URL(request.url)
        requestedKeyword = url.searchParams.get('keyword') ?? ''
        return HttpResponse.json({
          items: [deployment({ name: 'matched' })],
          total: 1,
          page: 1,
          page_size: 20,
          status_counts: { all: 1, running: 1 },
          search_truncated: true,
        })
      }),
    )

    await renderWithProviders(<Harness />, ['common', 'admin'])
    const user = userEvent.setup()
    await user.type(screen.getByLabelText('Search'), 'mat')
    await user.click(screen.getByRole('button', { name: 'Search' }))

    await waitFor(() => expect(requestedKeyword).toBe('mat'))
    expect(
      await screen.findByText(
        'Results exceed the scan limit; they may be incomplete.',
      ),
    ).toBeTruthy()
  })

  it('shows the disabled banner and deep-links to the settings tab', async () => {
    installActor(['settings:manage'])
    installSettings({ ...SETTINGS, enabled: false, can_connect: false })
    installList({
      items: [],
      total: 0,
      page: 1,
      page_size: 20,
      status_counts: { all: 0 },
    })

    await renderWithProviders(<Harness />, ['common', 'admin'])
    expect(
      (await screen.findAllByText(
        'io.net deployments are disabled. Enable and configure the API key in Settings.',
      )).length,
    ).toBeGreaterThan(0)

    const create = screen.getByRole('button', { name: 'Create deployment' })
    expect(create.hasAttribute('disabled')).toBe(true)

    await userEvent.setup().click(screen.getByRole('button', { name: 'Open settings' }))
    expect(
      await screen.findByText('io.net deployment settings'),
    ).toBeTruthy()
  })

  it('is read-only for actors without settings:manage', async () => {
    installActor(['users:read'])

    await renderWithProviders(<Harness />, ['common', 'admin'])
    expect(
      await screen.findByText(
        'Read-only: your account lacks the settings:manage permission.',
      ),
    ).toBeTruthy()
    expect(screen.queryByTestId('deployment-row')).toBeNull()
    expect(screen.queryByRole('button', { name: 'Create deployment' })).toBeNull()
  })
})
