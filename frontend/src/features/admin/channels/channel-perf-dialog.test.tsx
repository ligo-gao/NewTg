import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { ChannelPerformanceDialog } from './channel-perf-dialog'
import {
  findRequest,
  installFetch,
  jsonResponse,
  makeChannel,
  problemResponse,
  renderWithProviders,
} from './test-utils'

/** One persisted aggregate as the backend returns it (success_rate 0..1). */
function perfResult(
  model: string,
  overrides: Record<string, unknown> = {},
): Record<string, unknown> {
  return {
    id: 1,
    channel_id: 7,
    model,
    provider: 'openai',
    runs: 3,
    success_count: 3,
    success_rate: 1,
    ttft_ms_p50: 120,
    ttft_ms_p95: 180,
    latency_ms_p50: 400,
    latency_ms_p95: 550,
    tps_avg: 42.35,
    output_tokens_avg: 250,
    created_at: '2026-10-01T00:00:00Z',
    ...overrides,
  }
}

describe('ChannelPerformanceDialog', () => {
  it('lists every supported model as a selected checkbox and offers 1/3/5 runs', async () => {
    installFetch(() =>
      problemResponse(404, { type: 'about:blank', title: 'not found' }),
    )

    renderWithProviders(
      <ChannelPerformanceDialog
        open
        channel={makeChannel({
          id: 7,
          name: 'tester',
          supported_models: ['m1', 'm2'],
        })}
        onOpenChange={() => {}}
        onCompleted={() => {}}
      />,
    )

    const dialog = await screen.findByRole('dialog')
    const checkboxes = within(dialog).getAllByRole('checkbox')
    expect(checkboxes).toHaveLength(2)
    expect((checkboxes[0] as HTMLInputElement).checked).toBe(true)
    expect((checkboxes[1] as HTMLInputElement).checked).toBe(true)

    const runs = within(dialog).getByLabelText('Runs') as HTMLSelectElement
    expect(runs.value).toBe('3')
    expect(within(runs).getAllByRole('option')).toHaveLength(3)

    await userEvent.setup().selectOptions(runs, '5')
    expect(runs.value).toBe('5')
  })

  it('posts every selected model with the requested runs and reports completion', async () => {
    const user = userEvent.setup()
    const onCompleted = vi.fn()
    const channel = makeChannel({
      id: 7,
      name: 'tester',
      supported_models: ['m1', 'm2'],
    })
    const requests = installFetch((request) => {
      if (
        request.method === 'POST' &&
        request.url.pathname === '/api/channels/7/performance-tests'
      ) {
        const body = request.body as { model: string }
        return jsonResponse(perfResult(body.model))
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(
      <ChannelPerformanceDialog
        open
        channel={channel}
        onOpenChange={() => {}}
        onCompleted={onCompleted}
      />,
    )

    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Start' }))

    await waitFor(() => {
      expect(
        requests.filter(
          (request) =>
            request.url.pathname === '/api/channels/7/performance-tests',
        ),
      ).toHaveLength(2)
    })

    const posts = requests.filter(
      (request) => request.method === 'POST',
    )
    expect(posts.map((request) => request.body)).toEqual(
      expect.arrayContaining([
        { model: 'm1', runs: 3 },
        { model: 'm2', runs: 3 },
      ]),
    )
    await waitFor(() => {
      expect(onCompleted).toHaveBeenCalledTimes(1)
    })
  })

  it('renders TTFT, latency, TPS, success rate and output tokens per model', async () => {
    const user = userEvent.setup()
    installFetch((request) => {
      if (request.method === 'POST') {
        return jsonResponse(
          perfResult('m1', {
            success_count: 2,
            success_rate: 2 / 3,
            ttft_ms_p50: 120,
            ttft_ms_p95: 180,
            latency_ms_p50: 400,
            latency_ms_p95: 550,
            tps_avg: 42.35,
            output_tokens_avg: 250,
          }),
        )
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(
      <ChannelPerformanceDialog
        open
        channel={makeChannel({ id: 7, supported_models: ['m1'] })}
        onOpenChange={() => {}}
        onCompleted={() => {}}
      />,
    )

    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Start' }))

    const row = await screen.findByTestId('perf-result-m1')
    await waitFor(() => {
      expect(row.textContent).toContain('120')
      expect(row.textContent).toContain('180')
      expect(row.textContent).toContain('400')
      expect(row.textContent).toContain('550')
      expect(row.textContent).toContain('42.4')
      expect(row.textContent).toContain('66.7%')
      expect(row.textContent).toContain('250')
    })
  })

  it('shows a mapped failure reason and never leaks the upstream payload', async () => {
    const user = userEvent.setup()
    installFetch(() =>
      new Response(
        JSON.stringify({
          type: 'https://newtg.dev/problems/upstream_error',
          title: 'Upstream failure',
          detail: 'provider exploded',
        }),
        {
          status: 502,
          headers: {
            'Content-Type': 'application/problem+json',
            'X-Request-ID': 'req-abc-123',
          },
        },
      ),
    )

    renderWithProviders(
      <ChannelPerformanceDialog
        open
        channel={makeChannel({ id: 7, supported_models: ['m1'] })}
        onOpenChange={() => {}}
        onCompleted={() => {}}
      />,
    )

    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Start' }))

    const row = await screen.findByTestId('perf-result-m1')
    await waitFor(() => {
      expect(row.textContent).toContain('Upstream error')
    })
    // Raw upstream payloads and request ids never reach this surface.
    expect(row.textContent).not.toContain('provider exploded')
    expect(row.textContent).not.toContain('req-abc-123')
  })

  it('keeps completed results when the user stops the run', async () => {
    const user = userEvent.setup()
    const channel = makeChannel({
      id: 7,
      supported_models: ['m1', 'm2', 'm3'],
    })
    const controllers: Array<() => void> = []
    const requests = installFetch((request) => {
      if (
        request.method === 'POST' &&
        request.url.pathname === '/api/channels/7/performance-tests'
      ) {
        const body = request.body as { model: string }
        return new Promise<Response>((resolve) => {
          controllers.push(() => {
            resolve(jsonResponse(perfResult(body.model)))
          })
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(
      <ChannelPerformanceDialog
        open
        channel={channel}
        onOpenChange={() => {}}
        onCompleted={() => {}}
      />,
    )

    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Start' }))

    // Concurrency 2 claims the first two models; stop before they resolve.
    await waitFor(() => {
      expect(controllers).toHaveLength(2)
    })
    await user.click(within(dialog).getByRole('button', { name: 'Stop' }))

    for (const finish of controllers) finish()

    await waitFor(() => {
      expect(
        within(dialog)
          .getByRole('button', { name: 'Start' })
          .hasAttribute('disabled'),
      ).toBe(false)
    })

    expect(await screen.findByTestId('perf-result-m1')).toBeTruthy()
    expect(await screen.findByTestId('perf-result-m2')).toBeTruthy()
    expect(
      screen.getByTestId('perf-result-m1').textContent,
    ).toContain('120')
    expect(
      screen.getByTestId('perf-result-m2').textContent,
    ).toContain('120')
    // The third model was never scheduled after Stop.
    expect(
      requests.filter(
        (request) =>
          request.url.pathname === '/api/channels/7/performance-tests',
      ),
    ).toHaveLength(2)
    expect(findRequest(requests, 'POST', '/api/channels/7/performance-tests'))
      .toBeDefined()
  })

  it('shows the empty state when the channel has no models', async () => {
    installFetch(() =>
      problemResponse(404, { type: 'about:blank', title: 'not found' }),
    )

    renderWithProviders(
      <ChannelPerformanceDialog
        open
        channel={makeChannel({ id: 9, name: 'empty', supported_models: [] })}
        onOpenChange={() => {}}
        onCompleted={() => {}}
      />,
    )

    const dialog = await screen.findByRole('dialog')
    expect(
      within(dialog).getByText('No models configured for this channel.'),
    ).toBeTruthy()
  })
})
