import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { DeploymentDetailDrawer } from './detail-drawer'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const DETAIL = {
  id: 'd1',
  status: 'running',
  created_at: '2026-10-18T10:00:00Z',
  started_at: '2026-10-18T10:05:00Z',
  amount_paid: 42.5,
  total_gpus: 2,
  gpus_per_container: 2,
  total_containers: 1,
  hardware_id: 1,
  hardware_name: 'A100',
  brand_name: 'NVIDIA',
  locations: [{ id: 1, name: 'us-east' }],
  compute_minutes_remaining: 750,
  container_config: {
    entrypoint: ['python', 'serve.py'],
    // provider 只回非秘密环境变量；秘密值永不返回。
    env_variables: { MODEL: 'llm' },
    traffic_port: 8080,
    image_url: 'https://registry.example.com/llm:1',
  },
}

function installDetail() {
  server.use(
    http.get(`${ORIGIN}/api/deployments/d1`, () => HttpResponse.json(DETAIL)),
    http.get(`${ORIGIN}/api/deployments/d1/containers`, () =>
      HttpResponse.json({
        containers: [
          {
            container_id: 'c1',
            status: 'running',
            hardware: 'A100',
            gpus_per_container: 2,
            runtime_seconds: 3600,
            public_url: 'https://c1.io.example/',
            events: [{ time: '2026-10-18T10:05:00Z', message: 'started' }],
          },
        ],
        total: 1,
      }),
    ),
    http.get(`${ORIGIN}/api/deployments/d1/containers/c1`, () =>
      HttpResponse.json({
        container_id: 'c1',
        status: 'running',
        hardware: 'A100',
        gpus_per_container: 2,
        runtime_seconds: 3600,
        public_url: 'https://c1.io.example/',
        events: [{ time: '2026-10-18T10:05:00Z', message: 'started' }],
      }),
    ),
    http.get(`${ORIGIN}/api/deployments/d1/logs`, ({ request }) => {
      const token = new URL(request.url).searchParams.get('page_token')
      if (token === 'page-2') {
        return HttpResponse.json({
          logs: ['line-2'],
          next_page_token: '',
          has_more: false,
        })
      }
      return HttpResponse.json({
        logs: ['line-1'],
        next_page_token: 'page-2',
        has_more: true,
      })
    }),
  )
}

function renderDrawer() {
  return renderWithProviders(
    <DeploymentDetailDrawer
      deploymentId="d1"
      deploymentName="llm-a"
      canManage
      enabled
      onClose={vi.fn()}
      onAction={vi.fn()}
    />,
    ['common', 'admin'],
  )
}

describe('deployment detail drawer (F-08-56)', () => {
  it('renders overview fields and never renders secret values', async () => {
    installDetail()
    await renderDrawer()

    expect(await screen.findByText('llm-a')).toBeTruthy()
    expect(await screen.findByText('NVIDIA A100')).toBeTruthy()
    expect(screen.getByText('us-east')).toBeTruthy()
    expect(screen.getByText('12h 30m')).toBeTruthy()
    expect(screen.getByText('42.5')).toBeTruthy()
    expect(screen.getByText('https://registry.example.com/llm:1')).toBeTruthy()
    expect(screen.getByText('MODEL')).toBeTruthy()
    expect(screen.getByText('Secret values are never displayed.')).toBeTruthy()
  })

  it('lists containers and opens a nested container drawer', async () => {
    installDetail()
    await renderDrawer()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('tab', { name: /Containers/ }))

    const link = await screen.findByRole('link', { name: 'Public URL' })
    // 公网 URL 只展示、不代理：新窗口 + noopener noreferrer。
    expect(link.getAttribute('href')).toBe('https://c1.io.example/')
    expect(link.getAttribute('target')).toBe('_blank')
    expect(link.getAttribute('rel')).toBe('noopener noreferrer')

    await user.click(screen.getByRole('button', { name: 'c1' }))
    expect(await screen.findByText(/started/)).toBeTruthy()
    expect(screen.getAllByText('https://c1.io.example/').length).toBeGreaterThan(0)
  })

  it('loads logs with a page token and appends the next page', async () => {
    installDetail()
    await renderDrawer()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('tab', { name: 'Logs' }))

    await waitFor(() => {
      expect(screen.getByTestId('deployment-log').textContent).toContain('line-1')
    })
    expect(
      screen.getByText('Logs may contain sensitive data. Do not share them.'),
    ).toBeTruthy()

    await user.click(screen.getByRole('button', { name: 'Load more' }))
    await waitFor(() => {
      const log = screen.getByTestId('deployment-log')
      expect(log.textContent).toContain('line-1')
      expect(log.textContent).toContain('line-2')
    })
    expect(screen.getByText('No more log lines.')).toBeTruthy()
  })
})
