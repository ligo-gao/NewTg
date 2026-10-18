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
    expect(within(table).getByText('Manually Disabled')).toBeTruthy()
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
    expect(screen.getAllByRole('button', { name: 'New Channel' })).toHaveLength(1)
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

    // modules-00 §4.8: the confirm key is armed after 300ms.
    const confirm = within(dialog).getByRole('button', {
      name: 'Delete',
    }) as HTMLButtonElement
    await waitFor(() => {
      expect(confirm.disabled).toBe(false)
    })
    await user.click(confirm)
    await waitFor(() => {
      expect(
        findRequest(requests, 'POST', '/api/channels/batch-delete')?.body,
      ).toEqual({ ids: [1, 2] })
    })
    // batch-delete is idempotent: the write carries a UUID key.
    expect(
      findRequest(requests, 'POST', '/api/channels/batch-delete')?.headers.get(
        'Idempotency-Key',
      ),
    ).toMatch(/^[0-9a-f-]{36}$/)
  })

  it('clears the selection when the debounced keyword filter applies', async () => {
    const user = userEvent.setup()
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeListResponse([makeChannel({ id: 1, name: 'alpha' })]))
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('alpha')
    await user.click(screen.getByLabelText('Select alpha'))
    expect(screen.getByTestId('bulk-bar')).toBeTruthy()

    await user.type(screen.getByLabelText('Keyword'), 'alpha')
    await waitFor(
      () => {
        expect(screen.queryByTestId('bulk-bar')).toBeNull()
      },
      { timeout: 2000 },
    )
  })

  it('maps a failed single-channel test into the toast reason', async () => {
    const user = userEvent.setup()
    const channel = makeChannel({
      id: 12,
      name: 'toasty',
      supported_models: ['gpt-4'],
    })
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeListResponse([channel]))
      }
      if (
        request.method === 'POST' &&
        request.url.pathname === '/api/channels/12/test'
      ) {
        return jsonResponse({
          success: false,
          latency_ms: 5,
          model: 'gpt-4',
          error_class: 'auth',
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('toasty')

    await user.click(screen.getByRole('button', { name: 'Test connection' }))
    expect(
      await screen.findByText('Connection failed · gpt-4 · Authentication failed'),
    ).toBeTruthy()
    expect(screen.queryByText(/Unknown error/)).toBeNull()
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

  it('debounces the group filter and clears the selection when it applies', async () => {
    const user = userEvent.setup({ delay: null })
    const requests = installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(
          makeListResponse([
            makeChannel({ id: 1, name: 'alpha', group_tags: ['vip'] }),
          ]),
        )
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('alpha')
    await user.click(screen.getByLabelText('Select alpha'))
    expect(screen.getByTestId('bulk-bar')).toBeTruthy()

    await user.type(screen.getByLabelText('Group'), 'vip')

    await waitFor(
      () => {
        expect(
          findRequest(requests, 'GET', '/api/channels')?.url.searchParams.get(
            'group',
          ),
        ).toBe('vip')
      },
      { timeout: 2000 },
    )
    // Changing the filter invalidates the page-scoped selection.
    expect(screen.queryByTestId('bulk-bar')).toBeNull()
    // The 300ms debounce collapsed every keystroke into a single request.
    expect(
      requests.filter((request) => request.url.searchParams.has('group')),
    ).toHaveLength(1)
  })

  it('reuses the bulk-delete idempotency key when retrying the same selection', async () => {
    const user = userEvent.setup()
    const items = [
      makeChannel({ id: 1, name: 'alpha' }),
      makeChannel({ id: 2, name: 'beta' }),
    ]
    let deleteCalls = 0
    const requests = installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeListResponse(items))
      }
      if (
        request.method === 'POST' &&
        request.url.pathname === '/api/channels/batch-delete'
      ) {
        deleteCalls += 1
        if (deleteCalls === 1) {
          return problemResponse(500, {
            type: 'https://newtg.dev/problems/internal_error',
            title: 'Internal Server Error',
          })
        }
        return jsonResponse({ succeeded: [1, 2], failed: [] })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('alpha')
    await user.click(screen.getByLabelText('Select alpha'))
    await user.click(screen.getByLabelText('Select beta'))
    await user.click(screen.getByRole('button', { name: 'Delete selected' }))

    const dialog = await screen.findByRole('dialog')
    const confirm = within(dialog).getByRole('button', {
      name: 'Delete',
    }) as HTMLButtonElement
    await waitFor(() => {
      expect(confirm.disabled).toBe(false)
    })

    // The first attempt fails; the dialog stays open for a retry.
    await user.click(confirm)
    await waitFor(() => {
      expect(deleteCalls).toBe(1)
    })
    expect(screen.getByRole('dialog')).toBeTruthy()

    await user.click(confirm)
    await waitFor(() => {
      expect(deleteCalls).toBe(2)
    })
    const deletes = requests.filter(
      (request) =>
        request.method === 'POST' &&
        request.url.pathname === '/api/channels/batch-delete',
    )
    expect(deletes[0]?.headers.get('Idempotency-Key')).not.toBeNull()
    // Same intent, same key: the backend can replay instead of deleting twice.
    expect(deletes[1]?.headers.get('Idempotency-Key')).toBe(
      deletes[0]?.headers.get('Idempotency-Key'),
    )
    expect(deletes[1]?.body).toEqual({ ids: [1, 2] })
  })

  it('uses singular copy when deleting a single row', async () => {
    const user = userEvent.setup()
    const requests = installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeListResponse([makeChannel({ id: 1, name: 'alpha' })]))
      }
      if (request.method === 'DELETE' && request.url.pathname === '/api/channels/1') {
        return new Response(null, { status: 204 })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('alpha')

    await user.click(screen.getByRole('button', { name: 'Delete' }))

    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('Delete channel')).toBeTruthy()
    expect(within(dialog).queryByText('Delete channels')).toBeNull()
    expect(
      within(dialog).getByText('Delete this channel? This action cannot be undone.'),
    ).toBeTruthy()

    const confirm = within(dialog).getByRole('button', {
      name: 'Delete',
    }) as HTMLButtonElement
    await waitFor(() => {
      expect(confirm.disabled).toBe(false)
    })
    await user.click(confirm)
    await waitFor(() => {
      expect(
        findRequest(requests, 'DELETE', '/api/channels/1'),
      ).toBeDefined()
    })
  })

  it('disables all row and bulk actions and shows a spinner while a row mutation runs', async () => {
    const user = userEvent.setup()
    let release: ((response: Response) => void) | null = null
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(
          makeListResponse([
            makeChannel({
              id: 1,
              name: 'alpha',
              supported_models: ['gpt-4'],
            }),
          ]),
        )
      }
      if (
        request.method === 'POST' &&
        request.url.pathname === '/api/channels/1/test'
      ) {
        return new Promise<Response>((resolve) => {
          release = resolve
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('alpha')
    // Selecting the row also lets us check that the bulk bar is locked while
    // a selected row is mutating.
    await user.click(screen.getByLabelText('Select alpha'))
    await user.click(screen.getByRole('button', { name: 'Test connection' }))

    expect(await screen.findByTestId('channel-row-busy-1')).toBeTruthy()

    const disabled = (name: string): boolean =>
      (screen.getByRole('button', { name }) as HTMLButtonElement).disabled
    expect(
      (screen.getByRole('switch', { name: 'Disable channel alpha' }) as HTMLButtonElement)
        .disabled,
    ).toBe(true)
    expect(disabled('Batch test')).toBe(true)
    expect(disabled('Edit')).toBe(true)
    expect(disabled('Delete')).toBe(true)
    expect(disabled('Enable selected')).toBe(true)
    expect(disabled('Disable selected')).toBe(true)
    expect(disabled('Delete selected')).toBe(true)

    await waitFor(() => {
      expect(release).not.toBeNull()
    })
    const resolve = release as ((response: Response) => void) | null
    resolve?.(jsonResponse({ success: true, latency_ms: 3, model: 'gpt-4' }))

    await waitFor(() => {
      expect(screen.queryByTestId('channel-row-busy-1')).toBeNull()
    })
    expect(disabled('Edit')).toBe(false)
  })

  it('keeps each row busy independently when two row mutations overlap', async () => {
    const user = userEvent.setup()
    const releases = new Map<number, (response: Response) => void>()
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(
          makeListResponse([
            makeChannel({ id: 1, name: 'alpha', supported_models: ['gpt-4'] }),
            makeChannel({ id: 2, name: 'beta', supported_models: ['gpt-4'] }),
          ]),
        )
      }
      const testMatch = /^\/api\/channels\/(\d+)\/test$/.exec(
        request.url.pathname,
      )
      if (request.method === 'POST' && testMatch !== null) {
        const id = Number(testMatch[1])
        return new Promise<Response>((resolve) => {
          releases.set(id, resolve)
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('alpha')
    // Both rows are selected so the bulk bar reflects any busy selected row.
    await user.click(screen.getByLabelText('Select alpha'))
    await user.click(screen.getByLabelText('Select beta'))

    const alphaRow = screen.getByTestId('channel-row-1')
    const betaRow = screen.getByTestId('channel-row-2')
    await user.click(
      within(alphaRow).getByRole('button', { name: 'Test connection' }),
    )
    expect(await screen.findByTestId('channel-row-busy-1')).toBeTruthy()

    // Only row 1 is busy, so row 2 can start its own mutation.
    await user.click(
      within(betaRow).getByRole('button', { name: 'Test connection' }),
    )
    expect(await screen.findByTestId('channel-row-busy-2')).toBeTruthy()

    const bulkEnable = screen.getByRole('button', {
      name: 'Enable selected',
    }) as HTMLButtonElement
    expect(bulkEnable.disabled).toBe(true)

    await waitFor(() => {
      expect(releases.has(1)).toBe(true)
    })
    releases.get(1)?.(jsonResponse({ success: true, latency_ms: 1, model: 'gpt-4' }))

    // Row 1 finishing must not clear row 2's busy entry, and the bulk action
    // must stay locked while the selected row 2 is still in flight.
    await waitFor(() => {
      expect(screen.queryByTestId('channel-row-busy-1')).toBeNull()
    })
    expect(screen.getByTestId('channel-row-busy-2')).toBeTruthy()
    expect(bulkEnable.disabled).toBe(true)

    await waitFor(() => {
      expect(releases.has(2)).toBe(true)
    })
    releases.get(2)?.(jsonResponse({ success: true, latency_ms: 1, model: 'gpt-4' }))

    await waitFor(() => {
      expect(screen.queryByTestId('channel-row-busy-2')).toBeNull()
    })
    expect(bulkEnable.disabled).toBe(false)
  })

  it('renders the expanded list row without a credentials cell', async () => {
    const user = userEvent.setup()
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        // List rows never carry credentials_masked or credentials_set (GC-6).
        return jsonResponse(
          makeListResponse([makeChannel({ id: 3, name: 'maskless' })]),
        )
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('maskless')
    await user.click(
      screen.getByRole('button', { name: 'Expand details for maskless' }),
    )

    const detail = await screen.findByTestId('channel-detail-3')
    expect(within(detail).queryByText('API key')).toBeNull()
    expect(within(detail).queryByText('Set')).toBeNull()
    expect(within(detail).queryByText('Not set')).toBeNull()
    // The non-credential fields still render.
    expect(within(detail).getByText('ID')).toBeTruthy()
  })
})
