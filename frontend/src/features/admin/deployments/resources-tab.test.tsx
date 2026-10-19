import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { ResourcesTab } from './resources-tab'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function installCatalog() {
  server.use(
    http.get(`${ORIGIN}/api/deployments/hardware-types`, () =>
      HttpResponse.json({
        hardware_types: [
          {
            id: 1,
            name: 'A100',
            available_replicas: 8,
            brand_name: 'NVIDIA',
            gpu_count: 2,
            vram: '80GB',
            location_count: 3,
          },
        ],
        total: 1,
        total_available: 8,
      }),
    ),
    http.get(`${ORIGIN}/api/deployments/locations`, () =>
      HttpResponse.json({
        locations: [
          { id: 1, name: 'us-east' },
          { id: 2, name: 'eu-west' },
        ],
        total: 2,
      }),
    ),
    http.get(`${ORIGIN}/api/deployments/available-replicas`, () =>
      HttpResponse.json({
        replicas: [
          {
            location_id: 1,
            location_name: 'us-east',
            hardware_id: 1,
            available_count: 8,
            max_gpus: 4,
          },
        ],
      }),
    ),
    http.post(`${ORIGIN}/api/deployments/price-estimation`, () =>
      HttpResponse.json({
        estimated_cost: 12.4,
        currency: 'USD',
        price_breakdown: { compute_cost: 10, network_cost: 1.2, storage_cost: 1.2 },
        estimation_valid: true,
      }),
    ),
  )
}

describe('resources & pricing tab (F-08-53 / F-08-54)', () => {
  it('renders the hardware and location catalogs', async () => {
    installCatalog()
    await renderWithProviders(<ResourcesTab />, ['common', 'admin'])

    expect(await screen.findByText('NVIDIA')).toBeTruthy()
    expect(screen.getByText('80GB')).toBeTruthy()
    // 地域同时出现在地域目录与价格估算器的多选里。
    expect(screen.getAllByText('us-east').length).toBeGreaterThan(0)
    expect(screen.getAllByText('eu-west').length).toBeGreaterThan(0)
  })

  it('queries available replicas for a hardware/GPU combination', async () => {
    installCatalog()
    await renderWithProviders(<ResourcesTab />, ['common', 'admin'])
    const user = userEvent.setup()
    const section = await screen.findByTestId('replica-query')
    await within(section).findByRole('option', { name: 'A100' })

    await user.selectOptions(within(section).getByLabelText('Hardware'), '1')
    await user.click(within(section).getByRole('button', { name: 'Query' }))

    const result = await screen.findByTestId('replica-result')
    expect(result.textContent).toContain('us-east')
    expect(result.textContent).toContain('8')
    expect(result.textContent).toContain('4')
  })

  it('estimates a provider-side price and always shows the non-billing hint', async () => {
    installCatalog()
    await renderWithProviders(<ResourcesTab />, ['common', 'admin'])
    const user = userEvent.setup()
    const section = await screen.findByTestId('price-estimator')

    // 提示常驻，不依赖估算是否成功。
    expect(
      within(section).getByText('Provider estimate; not charged to user credits.'),
    ).toBeTruthy()

    await within(section).findByRole('option', { name: 'A100' })
    await user.selectOptions(within(section).getByLabelText('Hardware'), '1')
    const location = await within(section).findByRole('checkbox', { name: 'us-east' })
    await user.click(location)
    await user.click(within(section).getByRole('button', { name: 'Estimate' }))

    expect(await within(section).findByText('12.40 USD')).toBeTruthy()
    expect(
      within(section).getByText('Provider estimate; not charged to user credits.'),
    ).toBeTruthy()
    expect(within(section).getByText('Compute')).toBeTruthy()
    expect(within(section).getByText('10.00 USD')).toBeTruthy()
  })
})
