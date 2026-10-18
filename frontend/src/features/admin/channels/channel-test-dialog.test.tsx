import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { ChannelTestDialog } from './channel-test-dialog'
import {
  installFetch,
  jsonResponse,
  makeChannel,
  problemResponse,
  renderWithProviders,
} from './test-utils'

describe('ChannelTestDialog', () => {
  it('tests every selected model and reports the summary', async () => {
    const user = userEvent.setup()
    const channel = makeChannel({
      id: 7,
      name: 'tester',
      supported_models: ['m1', 'm2'],
    })
    const requests = installFetch((request) => {
      if (
        request.method === 'POST' &&
        request.url.pathname === '/api/channels/7/test'
      ) {
        const body = request.body as { model: string }
        return jsonResponse({ success: true, latency_ms: 42, model: body.model })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(
      <ChannelTestDialog
        open
        channel={channel}
        onOpenChange={() => {}}
        onCompleted={() => {}}
      />,
    )

    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Start test' }))

    await waitFor(() => {
      expect(
        requests.filter(
          (request) => request.url.pathname === '/api/channels/7/test',
        ),
      ).toHaveLength(2)
    })

    const bodies = requests.map((request) => request.body)
    expect(bodies).toEqual(
      expect.arrayContaining([
        { model: 'm1', stream: false },
        { model: 'm2', stream: false },
      ]),
    )
    await waitFor(() => {
      expect(screen.getByTestId('test-summary').textContent).toContain(
        '2 succeeded, 0 failed',
      )
    })
    expect(
      screen.getByRole('progressbar').getAttribute('aria-valuenow'),
    ).toBe('2')
  })

  it('renders the mapped reason for a failed model', async () => {
    const user = userEvent.setup()
    const channel = makeChannel({
      id: 11,
      name: 'failer',
      supported_models: ['m1'],
    })
    installFetch(() =>
      jsonResponse({
        success: false,
        latency_ms: 0,
        model: 'm1',
        error_class: 'auth',
      }),
    )

    renderWithProviders(
      <ChannelTestDialog
        open
        channel={channel}
        onOpenChange={() => {}}
        onCompleted={() => {}}
      />,
    )

    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Start test' }))

    expect(await within(dialog).findByText(/Authentication failed/)).toBeTruthy()
    expect(within(dialog).queryByText(/Unknown error/)).toBeNull()
    await waitFor(() => {
      expect(screen.getByTestId('test-summary').textContent).toContain(
        '0 succeeded, 1 failed',
      )
    })
  })

  it('shows the empty state when the channel has no models', async () => {
    installFetch(() =>
      problemResponse(404, { type: 'about:blank', title: 'not found' }),
    )

    renderWithProviders(
      <ChannelTestDialog
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
