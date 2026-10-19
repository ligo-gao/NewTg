import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { CreateDeploymentDrawer } from './create-deployment-drawer'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function installCatalog() {
  server.use(
    http.get(`${ORIGIN}/api/deployments/check-name`, ({ request }) =>
      HttpResponse.json({
        name: new URL(request.url).searchParams.get('name') ?? '',
        available: true,
      }),
    ),
    http.get(`${ORIGIN}/api/deployments/hardware-types`, () =>
      HttpResponse.json({
        hardware_types: [
          { id: 1, name: 'A100', available_replicas: 8, brand_name: 'NVIDIA' },
        ],
        total: 1,
        total_available: 8,
      }),
    ),
    http.get(`${ORIGIN}/api/deployments/locations`, () =>
      HttpResponse.json({
        locations: [{ id: 1, name: 'us-east' }],
        total: 1,
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
        price_breakdown: {
          compute_cost: 10,
          network_cost: 1.2,
          storage_cost: 1.2,
        },
        estimation_valid: true,
      }),
    ),
  )
}

async function fillWizard(): Promise<void> {
  const user = userEvent.setup()
  const name = screen.getByLabelText('Deployment name')
  await user.type(name, 'llm-new')
  await user.tab()
  expect(await screen.findByText('Name is available')).toBeTruthy()
  await user.type(screen.getByLabelText('Image URL'), 'https://registry.example.com/llm:1')
  await user.click(screen.getByRole('button', { name: 'Next' }))

  await screen.findByRole('option', { name: 'NVIDIA A100' })
  await user.selectOptions(screen.getByLabelText('Hardware type'), '1')
  await user.click(await screen.findByRole('checkbox', { name: /us-east/ }))
  await user.click(screen.getByRole('button', { name: 'Next' }))

  // 价格估算 400ms 防抖后出现
  expect(await screen.findByText('12.40 USD')).toBeTruthy()
  await user.click(screen.getByRole('button', { name: 'Next' }))

  await user.click(screen.getByRole('checkbox', { name: /provider-side costs/ }))
}

describe('create deployment drawer (F-08-55)', () => {
  it('walks four steps and submits once with an Idempotency-Key', async () => {
    installCatalog()
    const onClose = vi.fn()
    let headers: Headers | null = null
    let body: Record<string, unknown> | null = null
    server.use(
      http.post(`${ORIGIN}/api/deployments`, async ({ request }) => {
        headers = request.headers
        body = (await request.json()) as Record<string, unknown>
        return HttpResponse.json(
          { deployment_id: 'd9', status: 'deployment requested' },
          { status: 202 },
        )
      }),
    )

    await renderWithProviders(<CreateDeploymentDrawer open onClose={onClose} />, [
      'common',
      'admin',
    ])
    await fillWizard()

    expect(screen.getByText(/I understand this will incur provider-side costs/)).toBeTruthy()
    await userEvent.setup().click(screen.getByRole('button', { name: 'Create deployment' }))

    await waitFor(() => expect(onClose).toHaveBeenCalled())
    expect(headers).not.toBeNull()
    expect((headers as unknown as Headers).get('Idempotency-Key')).toMatch(/\S+/)
    expect(body).toMatchObject({
      name: 'llm-new',
      hardware_id: 1,
      location_ids: [1],
      gpus_per_container: 1,
      replica_count: 1,
      duration_hours: 24,
      image_url: 'https://registry.example.com/llm:1',
    })
    // 秘密字段未填写时不出现在请求体。
    expect(body).not.toHaveProperty('registry_secret')
    expect(body).not.toHaveProperty('secret_env_variables')
  })

  it('reuses the same key when retrying a failed submission', async () => {
    installCatalog()
    const onClose = vi.fn()
    const keys: string[] = []
    let attempts = 0
    server.use(
      http.post(`${ORIGIN}/api/deployments`, ({ request }) => {
        attempts += 1
        keys.push(request.headers.get('Idempotency-Key') ?? '')
        if (attempts === 1) {
          return HttpResponse.json(
            { type: 'about:blank', title: 'boom', detail: 'provider error' },
            { status: 502 },
          )
        }
        return HttpResponse.json(
          { deployment_id: 'd9', status: 'deployment requested' },
          { status: 202 },
        )
      }),
    )

    await renderWithProviders(<CreateDeploymentDrawer open onClose={onClose} />, [
      'common',
      'admin',
    ])
    await fillWizard()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Create deployment' }))
    expect(await screen.findByText(/provider error/)).toBeTruthy()

    await user.click(screen.getByRole('button', { name: 'Create deployment' }))
    await waitFor(() => expect(onClose).toHaveBeenCalled())
    expect(keys).toHaveLength(2)
    expect(keys[0]).not.toBe('')
    expect(keys[0]).toBe(keys[1])
  })

  it('does not auto-retry on 504 and reports an unknown result', async () => {
    installCatalog()
    const onClose = vi.fn()
    let attempts = 0
    server.use(
      http.post(`${ORIGIN}/api/deployments`, () => {
        attempts += 1
        return HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/deploy_provider_timeout',
            title: 'Provider timeout',
            detail: 'the provider did not answer',
          },
          { status: 504, headers: { 'Content-Type': 'application/problem+json' } },
        )
      }),
    )

    await renderWithProviders(<CreateDeploymentDrawer open onClose={onClose} />, [
      'common',
      'admin',
    ])
    await fillWizard()
    await userEvent.setup().click(screen.getByRole('button', { name: 'Create deployment' }))

    expect(
      await screen.findByText(
        'The result is unknown. Refresh first to confirm the provider state before retrying.',
      ),
    ).toBeTruthy()
    expect(attempts).toBe(1)
    expect(
      screen.getByRole('button', { name: 'Close and refresh' }),
    ).toBeTruthy()

    // 不自动重试，且提交按钮被禁用。
    await new Promise((resolve) => setTimeout(resolve, 50))
    expect(attempts).toBe(1)
    expect(
      screen.getByRole('button', { name: 'Create deployment' }).hasAttribute('disabled'),
    ).toBe(true)
    expect(onClose).not.toHaveBeenCalled()
  })

  it('does not silently treat a cleared GPU count as 1', async () => {
    installCatalog()
    await renderWithProviders(
      <CreateDeploymentDrawer open onClose={vi.fn()} />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()
    await user.type(screen.getByLabelText('Deployment name'), 'llm-new')
    await user.type(screen.getByLabelText('Image URL'), 'https://registry.example.com/llm:1')
    await user.click(screen.getByRole('button', { name: 'Next' }))

    await screen.findByRole('option', { name: 'NVIDIA A100' })
    await user.selectOptions(screen.getByLabelText('Hardware type'), '1')
    await user.click(await screen.findByRole('checkbox', { name: /us-east/ }))
    const gpu = screen.getByLabelText('GPUs per container')
    await user.clear(gpu)
    await user.click(screen.getByRole('button', { name: 'Next' }))

    expect(await screen.findByText('Enter a positive integer.')).toBeTruthy()
    // 仍停留在硬件与地域步骤。
    expect(screen.getByLabelText('Hardware type')).toBeTruthy()
    expect(screen.queryByLabelText('Replicas')).toBeNull()
  })
})
