import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { fireEvent, screen, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { SandboxTab } from './sandbox-tab'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const UPDATED_AT = '2026-10-19T10:00:00Z'

function installList() {
  server.use(
    http.get(`${ORIGIN}/api/plugin/task`, () =>
      HttpResponse.json({
        items: [
          {
            key: 'kling',
            name: 'Kling',
            icon: null,
            source: 'builtin',
            builtin: true,
            enabled: true,
            active_version: '1.0.0',
            description: null,
            runtime_status: 'registered',
            bound_channels: 0,
            updated_at: UPDATED_AT,
          },
        ],
        total: 1,
        page: 1,
        page_size: 100,
      }),
    ),
    http.get(`${ORIGIN}/api/plugin/task/kling/versions`, () =>
      HttpResponse.json({
        items: [
          {
            id: 'v1',
            key: 'kling',
            version: '1.0.0',
            api_version: 'v1',
            source_hash: 'd'.repeat(64),
            icon: null,
            enabled: true,
            active: true,
            uploaded_by: 1,
            created_at: UPDATED_AT,
          },
        ],
        total: 1,
      }),
    ),
  )
}

function renderTab(overrides: Partial<Parameters<typeof SandboxTab>[0]> = {}) {
  return renderWithProviders(<SandboxTab canManage online {...overrides} />, [
    'common',
    'admin',
  ])
}

async function selectPlugin(): Promise<void> {
  await screen.findByRole('option', { name: /Kling/ })
  await userEvent.selectOptions(screen.getByLabelText('Plugin'), 'kling')
}

describe('sandbox tab (F-08-35)', () => {
  it('disables the outbound checkbox with an explanation and states zero side effects', async () => {
    installList()
    await renderTab()

    const outbound = screen.getByRole('checkbox', {
      name: 'Allow real outbound',
    }) as HTMLInputElement
    expect(outbound.disabled).toBe(true)
    expect(outbound.checked).toBe(false)
    expect(
      screen.getByText(
        'The outbound executor is not implemented, so this option is unavailable and cannot be enabled.',
      ),
    ).toBeTruthy()
    expect(
      screen.getByText(
        'A dry run writes no task, deducts no credits, and makes no outbound request.',
      ),
    ).toBeTruthy()
  })

  it('rejects invalid JSON locally without sending a request', async () => {
    installList()
    let requests = 0
    server.use(
      http.post(`${ORIGIN}/api/plugin/task/kling/dryrun`, () => {
        requests += 1
        return HttpResponse.json({})
      }),
    )

    await renderTab()
    await selectPlugin()
    fireEvent.change(screen.getByLabelText('Sample request (JSON)'), {
      target: { value: '{"seconds": ' },
    })
    await userEvent.click(screen.getByRole('button', { name: 'Run dry run' }))

    expect(await screen.findByText('The sample request is not valid JSON.')).toBeTruthy()
    expect(requests).toBe(0)
  })

  it('sends the sample request and highlights a status outside the contract vocabulary', async () => {
    installList()
    let body: unknown = null
    server.use(
      http.post(`${ORIGIN}/api/plugin/task/kling/dryrun`, async ({ request }) => {
        body = await request.json()
        return HttpResponse.json({
          key: 'kling',
          version: '1.0.0',
          endpoint: 'submit',
          result: { status: 'BOGUS' },
          duration_ms: 12,
        })
      }),
    )

    await renderTab()
    await selectPlugin()
    await userEvent.click(screen.getByRole('button', { name: 'Run dry run' }))

    const status = await screen.findByTestId('sandbox-parsed-status')
    expect(status.textContent).toContain(
      'Status value is not in the contract vocabulary: BOGUS',
    )
    expect(screen.getByText('Took 12 ms')).toBeTruthy()
    expect(body).toMatchObject({
      endpoint: 'submit',
      payload: {},
      allow_outbound: false,
    })
  })

  it('shows a contract status when the return value is a known state', async () => {
    installList()
    server.use(
      http.post(`${ORIGIN}/api/plugin/task/kling/dryrun`, () =>
        HttpResponse.json({
          key: 'kling',
          version: '1.0.0',
          endpoint: 'query',
          result: 'SUCCESS',
          duration_ms: 3,
        }),
      ),
    )

    await renderTab()
    await selectPlugin()
    await userEvent.selectOptions(screen.getByLabelText('Endpoint'), 'query')
    await userEvent.click(screen.getByRole('button', { name: 'Run dry run' }))

    expect(await screen.findByText('Status: SUCCESS')).toBeTruthy()
  })

  it('renders structured issues from a failed dry run', async () => {
    installList()
    server.use(
      http.post(`${ORIGIN}/api/plugin/task/kling/dryrun`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/plugin_dryrun_failed',
            title: 'Dry run failed',
            status: 422,
            detail: 'the plugin hook could not be executed',
            issues: [
              {
                key: 'kling',
                fieldPath: 'hooks.parseSubmitResponse',
                locale: '',
                rule: 'runtime',
                message: 'TypeError: x is undefined',
              },
            ],
          },
          { status: 422, headers: { 'Content-Type': 'application/problem+json' } },
        ),
      ),
    )

    await renderTab()
    await selectPlugin()
    await userEvent.click(screen.getByRole('button', { name: 'Run dry run' }))

    const errorBox = await screen.findByTestId('sandbox-error')
    expect(errorBox.textContent).toContain('hooks.parseSubmitResponse')
    expect(errorBox.textContent).toContain('TypeError: x is undefined')
  })

  it('does not offer the dry run without settings:manage', async () => {
    installList()
    await renderTab({ canManage: false })
    await waitFor(() =>
      expect(
        (screen.getByRole('button', { name: 'Run dry run' }) as HTMLButtonElement).disabled,
      ).toBe(true),
    )
  })
})
