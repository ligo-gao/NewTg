import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { ChannelListPage } from './channel-list-page'
import {
  findRequest,
  installFetch,
  jsonResponse,
  makeChannel,
  makeListResponse,
  problemResponse,
  renderWithProviders,
} from './test-utils'

describe('ChannelListPage', () => {
  it('renders rows and sends the expected filter and pagination params', async () => {
    const user = userEvent.setup()
    const items = [
      makeChannel({ id: 1, name: 'alpha', priority: 10 }),
      makeChannel({
        id: 2,
        name: 'beta',
        status: 2,
        priority: 5,
        supported_models: ['claude-sonnet'],
        group_tags: ['vip'],
      }),
    ]
    const requests = installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeListResponse(items, { total: 25 }))
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)

    expect(await screen.findByText('alpha')).toBeTruthy()
    expect(screen.getByText('beta')).toBeTruthy()
    const table = screen.getByTestId('channels-table')
    expect(within(table).getByText('Manually disabled')).toBeTruthy()
    expect(within(table).getByText('All groups')).toBeTruthy()
    expect(
      screen.getByRole('columnheader', { name: 'Priority' }).getAttribute('aria-sort'),
    ).toBe('descending')

    const firstList = findRequest(requests, 'GET', '/api/channels')
    expect(firstList?.url.searchParams.get('p')).toBe('1')
    expect(firstList?.url.searchParams.get('page_size')).toBe('20')
    expect(firstList?.url.searchParams.get('sort')).toBe('priority')

    await user.selectOptions(screen.getByLabelText('Status'), '2')
    await waitFor(() => {
      const latest = findRequest(requests, 'GET', '/api/channels')
      expect(latest?.url.searchParams.get('status')).toBe('2')
      expect(latest?.url.searchParams.get('p')).toBe('1')
    })

    await user.selectOptions(screen.getByLabelText('Rows per page'), '10')
    await waitFor(() => {
      const latest = findRequest(requests, 'GET', '/api/channels')
      expect(latest?.url.searchParams.get('page_size')).toBe('10')
      expect(latest?.url.searchParams.get('p')).toBe('1')
    })

    await user.click(screen.getByRole('button', { name: 'Next page' }))
    await waitFor(() => {
      expect(
        findRequest(requests, 'GET', '/api/channels')?.url.searchParams.get('p'),
      ).toBe('2')
    })
  })

  it('shows the empty state when no channels exist', async () => {
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeListResponse([]))
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)

    expect(await screen.findByText('No channels yet')).toBeTruthy()
    expect(screen.getAllByRole('button', { name: 'New Channel' }).length).toBeGreaterThan(0)
  })

  it('shows an inline error state and retries the query', async () => {
    const user = userEvent.setup()
    const requests = installFetch(() =>
      problemResponse(500, {
        type: 'https://newtg.dev/problems/internal_error',
        title: 'Internal Server Error',
      }),
    )

    renderWithProviders(<ChannelListPage />)

    expect(await screen.findByText('Failed to load channels.')).toBeTruthy()
    const before = requests.length

    await user.click(screen.getByRole('button', { name: 'Retry' }))
    await waitFor(() => {
      expect(requests.length).toBeGreaterThan(before)
    })
  })

  it('runs bulk enable/disable immediately and confirms bulk delete', async () => {
    const user = userEvent.setup()
    const items = [
      makeChannel({ id: 1, name: 'alpha' }),
      makeChannel({ id: 2, name: 'beta' }),
    ]
    const requests = installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeListResponse(items))
      }
      if (request.method === 'PATCH' && request.url.pathname === '/api/channels/batch') {
        return jsonResponse({ succeeded: [1, 2], failed: [] })
      }
      if (
        request.method === 'POST' &&
        request.url.pathname === '/api/channels/batch-delete'
      ) {
        return jsonResponse({ succeeded: [1, 2], failed: [] })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('alpha')

    await user.click(screen.getByLabelText('Select alpha'))
    await user.click(screen.getByLabelText('Select beta'))
    expect(screen.getByText('2 selected')).toBeTruthy()

    // R-P19: bulk enable/disable never ask for confirmation.
    await user.click(screen.getByRole('button', { name: 'Enable selected' }))
    expect(screen.queryByRole('dialog')).toBeNull()
    await waitFor(() => {
      const batch = findRequest(requests, 'PATCH', '/api/channels/batch')
      expect(batch?.body).toEqual({ action: 'enable', ids: [1, 2] })
    })
    await waitFor(() => {
      expect(screen.queryByTestId('bulk-bar')).toBeNull()
    })

    await user.click(screen.getByLabelText('Select alpha'))
    await user.click(screen.getByLabelText('Select beta'))
    await user.click(screen.getByRole('button', { name: 'Disable selected' }))
    await waitFor(() => {
      const batches = requests.filter(
        (request) =>
          request.method === 'PATCH' &&
          request.url.pathname === '/api/channels/batch',
      )
      expect(batches.at(-1)?.body).toEqual({ action: 'disable', ids: [1, 2] })
    })
    await waitFor(() => {
      expect(screen.queryByTestId('bulk-bar')).toBeNull()
    })

    await user.click(screen.getByLabelText('Select alpha'))
    await user.click(screen.getByLabelText('Select beta'))
    await user.click(screen.getByRole('button', { name: 'Delete selected' }))

    const dialog = await screen.findByRole('dialog')
    expect(
      within(dialog).getByText('Delete 2 channels? This action cannot be undone.'),
    ).toBeTruthy()
    expect(
      findRequest(requests, 'POST', '/api/channels/batch-delete'),
    ).toBeUndefined()

    await user.click(within(dialog).getByRole('button', { name: 'Delete' }))
    await waitFor(() => {
      expect(
        findRequest(requests, 'POST', '/api/channels/batch-delete')?.body,
      ).toEqual({ ids: [1, 2] })
    })
  })

  it('sends expected_version on status toggles and reports a 409 conflict', async () => {
    const user = userEvent.setup()
    const channel = makeChannel({
      id: 5,
      name: 'gamma',
      version: '2026-10-02T03:04:05.000Z',
    })
    const requests = installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeListResponse([channel]))
      }
      if (request.method === 'PATCH' && request.url.pathname === '/api/channels/5') {
        return problemResponse(409, {
          type: 'https://newtg.dev/problems/channel_version_conflict',
          title: 'Channel version conflict',
          detail: 'the channel was modified by someone else; refresh and retry',
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('gamma')

    await user.click(
      screen.getByRole('switch', { name: 'Disable channel gamma' }),
    )

    await waitFor(() => {
      expect(
        findRequest(requests, 'PATCH', '/api/channels/5')?.body,
      ).toEqual({
        expected_version: '2026-10-02T03:04:05.000Z',
        status: 2,
      })
    })
    expect(
      await screen.findByText(
        'Channel status changed by someone else. Please refresh.',
      ),
    ).toBeTruthy()
  })
})
