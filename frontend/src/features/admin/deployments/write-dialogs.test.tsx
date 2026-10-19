import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import type { DeploymentActionTarget } from './deployments-tab'
import { ExtendDialog } from './extend-dialog'
import { TerminateDialog } from './terminate-dialog'
import { UpdateDialog } from './update-dialog'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const DETAIL = {
  id: 'd1',
  status: 'running',
  hardware_id: 1,
  hardware_name: 'A100',
  brand_name: 'NVIDIA',
  gpus_per_container: 2,
  total_containers: 1,
  locations: [{ id: 1, name: 'us-east' }],
  container_config: {
    image_url: 'https://registry.example.com/llm:1',
    entrypoint: ['python', 'serve.py'],
    env_variables: { MODEL: 'llm' },
    traffic_port: 8080,
  },
}

function target(kind: DeploymentActionTarget['kind']): DeploymentActionTarget {
  return { kind, id: 'd1', name: 'llm-a', status: 'running' }
}

function installDetail() {
  server.use(http.get(`${ORIGIN}/api/deployments/d1`, () => HttpResponse.json(DETAIL)))
}

describe('deployment write dialogs (F-08-57 / F-08-58)', () => {
  it('extends only after an estimate, reusing one Idempotency-Key', async () => {
    installDetail()
    server.use(
      http.post(`${ORIGIN}/api/deployments/price-estimation`, () =>
        HttpResponse.json({
          estimated_cost: 30,
          currency: 'USD',
          price_breakdown: { compute_cost: 30 },
          estimation_valid: true,
        }),
      ),
      http.post(`${ORIGIN}/api/deployments/d1/extend`, ({ request }) => {
        expect(request.headers.get('Idempotency-Key')).toMatch(/\S+/)
        return HttpResponse.json(
          { deployment_id: 'd1', status: 'extend requested' },
          { status: 202 },
        )
      }),
    )

    const onClose = vi.fn()
    await renderWithProviders(
      <ExtendDialog target={target('extend')} onClose={onClose} />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()
    const confirm = screen.getByRole('button', { name: 'Confirm extension' })
    expect(confirm.hasAttribute('disabled')).toBe(true)

    const estimateButton = screen.getByRole('button', { name: 'Estimate' })
    await waitFor(() => expect(estimateButton.hasAttribute('disabled')).toBe(false))
    await user.click(estimateButton)
    expect((await screen.findAllByText('30.00 USD')).length).toBeGreaterThan(0)
    expect(
      screen.getByText('This extension adds 24 hours, estimated at 30 USD.'),
    ).toBeTruthy()
    await user.click(confirm)
    await waitFor(() => expect(onClose).toHaveBeenCalled())
  })

  it('requires the exact deployment name before terminating', async () => {
    let deleteKey = ''
    server.use(
      http.delete(`${ORIGIN}/api/deployments/d1`, ({ request }) => {
        deleteKey = request.headers.get('Idempotency-Key') ?? ''
        return HttpResponse.json(
          { deployment_id: 'd1', status: 'termination requested' },
          { status: 202 },
        )
      }),
    )

    const onClose = vi.fn()
    await renderWithProviders(
      <TerminateDialog target={target('terminate')} onClose={onClose} />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()
    const dialog = await screen.findByRole('alertdialog')
    const confirm = within(dialog).getByRole('button', {
      name: 'Terminate deployment',
    })
    expect(confirm.hasAttribute('disabled')).toBe(true)

    const input = within(dialog).getByLabelText('Deployment name')
    // 初始焦点落在取消，而不是 destructive 按钮。
    await waitFor(() =>
      expect(document.activeElement).toBe(
        within(dialog).getByRole('button', { name: 'Cancel' }),
      ),
    )
    await user.type(input, 'llm-a')
    expect(confirm.hasAttribute('disabled')).toBe(false)

    await user.click(confirm)
    await waitFor(() => expect(onClose).toHaveBeenCalled())
    expect(deleteKey).not.toBe('')
  })

  it('warns about restarts and omits blank secret fields', async () => {
    installDetail()
    let patchBody: Record<string, unknown> | null = null
    server.use(
      http.patch(`${ORIGIN}/api/deployments/d1`, async ({ request }) => {
        patchBody = (await request.json()) as Record<string, unknown>
        return HttpResponse.json({ deployment_id: 'd1', status: 'running' })
      }),
    )

    const onClose = vi.fn()
    await renderWithProviders(
      <UpdateDialog target={target('update')} onClose={onClose} />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()
    await waitFor(() =>
      expect(screen.getByLabelText('Image URL')).toHaveProperty(
        'value',
        'https://registry.example.com/llm:1',
      ),
    )

    await user.click(screen.getByRole('button', { name: 'Save changes' }))
    const dialog = await screen.findByRole('alertdialog')
    expect(
      within(dialog).getByText(
        'Updating may restart the container and cause brief downtime.',
      ),
    ).toBeTruthy()
    await user.click(within(dialog).getByRole('button', { name: 'Save changes' }))

    await waitFor(() => expect(patchBody).not.toBeNull())
    // 秘密留空 = 不更新；未改动的镜像也不进入变更体。
    expect(patchBody).not.toHaveProperty('registry_secret')
    expect(patchBody).not.toHaveProperty('image_url')
    expect(patchBody).toMatchObject({ env_variables: { MODEL: 'llm' } })
    await waitFor(() => expect(onClose).toHaveBeenCalled())
  })
})
