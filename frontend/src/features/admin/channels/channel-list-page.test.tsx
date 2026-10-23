import { fireEvent, screen, waitFor, within } from '@testing-library/react'
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

/** Runs a per-row action from the row's kebab menu. */
async function chooseRowAction(
  user: ReturnType<typeof userEvent.setup>,
  rowTestId: string,
  name: string,
): Promise<void> {
  const row = screen.getByTestId(rowTestId)
  await user.click(within(row).getByRole('button', { name: /^More actions for / }))
  await user.click(screen.getByRole('menuitem', { name }))
}

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
    // Groups and the routing numbers share one cell.
    const alphaRow = screen.getByTestId('channel-row-1')
    expect(alphaRow.textContent).toContain('Priority 10')
    expect(alphaRow.textContent).toContain('Weight 1')
    expect(within(screen.getByTestId('channel-row-2')).getByText('vip')).toBeTruthy()
    expect(
      screen
        .getByRole('columnheader', { name: 'Routing & groups' })
        .getAttribute('aria-sort'),
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

    await chooseRowAction(user, 'channel-row-12', 'Test connection')
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

    await chooseRowAction(user, 'channel-row-1', 'Delete')

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
      if (request.method === 'PATCH' && request.url.pathname === '/api/channels/1') {
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

    // The kebab stays focusable (aria-disabled) while the row is busy, so the
    // menu has to be opened before the mutation starts to observe the close.
    await user.click(
      screen.getByRole('button', { name: 'More actions for alpha' }),
    )
    expect(screen.getByRole('menu')).toBeTruthy()

    // fireEvent.click (no pointerdown) starts the mutation without the menu's
    // outside-pointerdown handler closing it first.
    fireEvent.click(
      screen.getByRole('switch', { name: 'Disable channel alpha' }),
    )

    expect(await screen.findByTestId('channel-row-busy-1')).toBeTruthy()

    // The menu closes when the row turns busy, restoring focus to the kebab.
    expect(screen.queryByRole('menu')).toBeNull()
    const kebab = screen.getByRole('button', {
      name: 'More actions for alpha',
    })
    expect(kebab.getAttribute('aria-disabled')).toBe('true')
    expect(document.activeElement).toBe(kebab)
    expect(
      (screen.getByRole('switch', { name: 'Disable channel alpha' }) as HTMLButtonElement)
        .disabled,
    ).toBe(true)
    expect(
      (screen.getByRole('button', { name: 'Enable selected' }) as HTMLButtonElement)
        .disabled,
    ).toBe(true)
    expect(
      (screen.getByRole('button', { name: 'Disable selected' }) as HTMLButtonElement)
        .disabled,
    ).toBe(true)
    expect(
      (screen.getByRole('button', { name: 'Delete selected' }) as HTMLButtonElement)
        .disabled,
    ).toBe(true)

    await waitFor(() => {
      expect(release).not.toBeNull()
    })
    const resolve = release as ((response: Response) => void) | null
    resolve?.(jsonResponse(makeChannel({ id: 1, name: 'alpha' })))

    await waitFor(() => {
      expect(screen.queryByTestId('channel-row-busy-1')).toBeNull()
    })
    expect(kebab.getAttribute('aria-disabled')).toBe('false')
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
      within(alphaRow).getByRole('button', { name: 'More actions for alpha' }),
    )
    await user.click(screen.getByRole('menuitem', { name: 'Test connection' }))
    expect(await screen.findByTestId('channel-row-busy-1')).toBeTruthy()

    // Only row 1 is busy, so row 2 can start its own mutation.
    await user.click(
      within(betaRow).getByRole('button', { name: 'More actions for beta' }),
    )
    await user.click(screen.getByRole('menuitem', { name: 'Test connection' }))
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

  it('toggles the dossier from the models count instead of listing models inline', async () => {
    const user = userEvent.setup()
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(
          makeListResponse([
            makeChannel({
              id: 7,
              name: 'model-fold',
              supported_models: ['gpt-4', 'gpt-4o'],
            }),
          ]),
        )
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('model-fold')

    // The summary row no longer enumerates models; it only shows a count.
    const row = screen.getByTestId('channel-row-7')
    expect(within(row).queryByText('gpt-4')).toBeNull()

    const toggle = within(row).getByRole('button', {
      name: 'Models: 2 · model-fold',
    })
    expect(toggle.getAttribute('aria-expanded')).toBe('false')
    expect(toggle.getAttribute('aria-controls')).toBeNull()
    expect(document.getElementById('channel-detail-7')).toBeNull()

    await user.click(toggle)

    const detail = await screen.findByTestId('channel-detail-7')
    expect(detail.id).toBe('channel-detail-7')
    expect(detail.getAttribute('colspan')).toBe('7')
    expect(
      within(row)
        .getByRole('button', { name: 'Models: 2 · model-fold' })
        .getAttribute('aria-expanded'),
    ).toBe('true')
    expect(
      within(row)
        .getByRole('button', { name: 'Models: 2 · model-fold' })
        .getAttribute('aria-controls'),
    ).toBe('channel-detail-7')
    // The expanded dossier is the one place the models are enumerated.
    expect(within(detail).getByText('gpt-4')).toBeTruthy()
    expect(within(detail).getByText('gpt-4o')).toBeTruthy()

    await user.click(
      within(row).getByRole('button', { name: 'Models: 2 · model-fold' }),
    )
    await waitFor(() => {
      expect(document.getElementById('channel-detail-7')).toBeNull()
    })
    expect(
      within(row)
        .getByRole('button', { name: 'Models: 2 · model-fold' })
        .getAttribute('aria-expanded'),
    ).toBe('false')
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

  it('shows the localised auto-disable reason and last-updated time in the detail panel', async () => {
    const user = userEvent.setup()
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(
          makeListResponse([
            makeChannel({
              id: 4,
              name: 'auto-banned',
              status: 3,
              auto_disabled_reason: 'channel_keyword',
              updated_at: '2026-10-01T00:00:00Z',
            }),
          ]),
        )
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('auto-banned')
    await user.click(
      screen.getByRole('button', { name: 'Expand details for auto-banned' }),
    )

    const detail = await screen.findByTestId('channel-detail-4')
    expect(within(detail).getByText('Auto-disable reason')).toBeTruthy()
    // Localised copy, never the raw reason code (R-M4-05).
    expect(within(detail).getByText(/Matched a disable keyword/)).toBeTruthy()
    expect(within(detail).getByText(/Last updated/)).toBeTruthy()
    expect(within(detail).queryByText(/channel_keyword/)).toBeNull()
  })

  it('hides the auto-disable reason row when the reason is empty', async () => {
    const user = userEvent.setup()
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(
          makeListResponse([
            makeChannel({
              id: 5,
              name: 'reasonless',
              status: 3,
              auto_disabled_reason: null,
            }),
          ]),
        )
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('reasonless')
    await user.click(
      screen.getByRole('button', { name: 'Expand details for reasonless' }),
    )

    const detail = await screen.findByTestId('channel-detail-5')
    expect(within(detail).queryByText('Auto-disable reason')).toBeNull()
  })

  it('passes the auto-disable reason to the list badge tooltip', async () => {
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(
          makeListResponse([
            makeChannel({
              id: 6,
              name: 'timed-out',
              status: 3,
              auto_disabled_reason: 'channel_timeout',
              updated_at: '2026-10-01T00:00:00Z',
            }),
          ]),
        )
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('timed-out')

    const title = screen.getByTestId('channel-status-badge').getAttribute('title')
    expect(title).toContain('Response timed out')
    expect(title).toContain('Last updated')
  })

  it('locks every selected row for the duration of a bulk status write', async () => {
    const user = userEvent.setup()
    const items = [
      makeChannel({ id: 1, name: 'alpha' }),
      makeChannel({ id: 2, name: 'beta' }),
    ]
    let release: ((response: Response) => void) | null = null
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeListResponse(items))
      }
      if (request.method === 'PATCH' && request.url.pathname === '/api/channels/batch') {
        return new Promise<Response>((resolve) => {
          release = resolve
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('alpha')
    await user.click(screen.getByLabelText('Select alpha'))
    await user.click(screen.getByLabelText('Select beta'))

    await user.click(screen.getByRole('button', { name: 'Enable selected' }))
    await waitFor(() => {
      expect(release).not.toBeNull()
    })

    // Both selected rows are locked, so neither can race the batch PATCH.
    for (const name of ['alpha', 'beta']) {
      expect(
        (
          screen.getByRole('switch', {
            name: `Disable channel ${name}`,
          }) as HTMLButtonElement
        ).disabled,
      ).toBe(true)
      expect(
        screen
          .getByRole('button', { name: `More actions for ${name}` })
          .getAttribute('aria-disabled'),
      ).toBe('true')
    }

    const resolve = release as ((response: Response) => void) | null
    resolve?.(jsonResponse({ succeeded: [1, 2], failed: [] }))

    await waitFor(() => {
      expect(screen.queryByTestId('bulk-bar')).toBeNull()
    })
    await waitFor(() => {
      expect(
        (
          screen.getByRole('switch', {
            name: 'Disable channel alpha',
          }) as HTMLButtonElement
        ).disabled,
      ).toBe(false)
    })
    expect(
      screen
        .getByRole('button', { name: 'More actions for alpha' })
        .getAttribute('aria-disabled'),
    ).toBe('false')
  })

  it('labels and colours the merged 24h health cell', async () => {
    const items = [
      makeChannel({
        id: 1,
        name: 'alpha',
        metrics: { request_count: 10, success_rate: 0.8, avg_latency_ms: 150 },
      }),
      makeChannel({ id: 2, name: 'beta' }),
      makeChannel({
        id: 3,
        name: 'gamma',
        metrics: { request_count: 10, success_rate: 0.995, avg_latency_ms: 90 },
      }),
      makeChannel({
        id: 4,
        name: 'delta',
        metrics: { request_count: 10, success_rate: 0.97, avg_latency_ms: 120 },
      }),
    ]
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeListResponse(items))
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('alpha')

    // The two values share one column, so each carries its own accessible name.
    expect(
      screen.getByTestId('channel-metrics-rate-1').getAttribute('aria-label'),
    ).toBe('Success (24h): 80.0%')
    expect(
      screen.getByTestId('channel-metrics-latency-1').getAttribute('aria-label'),
    ).toBe('Latency (24h): 150 ms')
    expect(
      screen.getByTestId('channel-metrics-rate-2').getAttribute('aria-label'),
    ).toBe('Success (24h): —')
    expect(
      screen.getByTestId('channel-metrics-latency-2').getAttribute('aria-label'),
    ).toBe('Latency (24h): —')

    // Threshold colours: danger below 95%, warning 95–99%, success at 99%+.
    expect(screen.getByTestId('channel-metrics-rate-1').className).toContain(
      'text-danger',
    )
    expect(screen.getByTestId('channel-metrics-rate-3').className).toContain(
      'text-success',
    )
    expect(screen.getByTestId('channel-metrics-rate-4').className).toContain(
      'text-warning',
    )
    // The rate is the cell's emphasis.
    expect(screen.getByTestId('channel-metrics-rate-1').className).toContain(
      'font-semibold',
    )
    // Latency is secondary ink with tabular numerals.
    expect(screen.getByTestId('channel-metrics-latency-1').className).toContain(
      'text-ink-2',
    )
    expect(screen.getByTestId('channel-metrics-latency-1').className).toContain(
      'tabular-nums',
    )
    // No data renders one muted dash plus an explicit "No data" line.
    expect(screen.getByTestId('channel-metrics-rate-2').className).toContain(
      'text-ink-3',
    )
    expect(screen.getByTestId('channel-metrics-latency-2').className).toContain(
      'hidden',
    )
    expect(screen.getByText('No data')).toBeTruthy()
  })

  it('points the channel-name disclosure at the detail row only while expanded', async () => {
    const user = userEvent.setup()
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(
          makeListResponse([makeChannel({ id: 9, name: 'disclosure' })]),
        )
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('disclosure')

    const expand = screen.getByRole('button', {
      name: 'Expand details for disclosure',
    })
    expect(expand.getAttribute('aria-expanded')).toBe('false')
    expect(expand.getAttribute('aria-controls')).toBeNull()

    await user.click(expand)

    const collapse = await screen.findByRole('button', {
      name: 'Collapse details for disclosure',
    })
    expect(collapse.getAttribute('aria-expanded')).toBe('true')
    expect(collapse.getAttribute('aria-controls')).toBe('channel-detail-9')
  })

  it('shows the cross-protocol forwarding note without an editable switch', async () => {
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeListResponse([]))
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('No channels yet')

    const note = screen.getByRole('note')
    expect(note.textContent).toContain('x-newtg-protocol')
    expect(note.textContent).toContain('relay.cross_protocol_forward_enabled')
    // M4 has no settings API: the panel must not offer a control that would
    // silently do nothing.
    expect(screen.queryByRole('switch')).toBeNull()
    expect(within(note).queryByRole('button')).toBeNull()
  })

  it('renders the page header with breadcrumbs and the New Channel action', async () => {
    const user = userEvent.setup()
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeListResponse([]))
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('No channels yet')

    const header = screen.getByTestId('page-header')
    expect(within(header).getByRole('heading', { name: 'Channels' })).toBeTruthy()
    expect(
      within(header).getByText(
        'Manage upstream channels, models and routing health.',
      ),
    ).toBeTruthy()

    const nav = within(header).getByRole('navigation', { name: 'Console' })
    expect(within(nav).getByText('Console')).toBeTruthy()
    expect(within(nav).getByText('Management')).toBeTruthy()
    expect(within(nav).getByText('Channels').getAttribute('aria-current')).toBe(
      'page',
    )

    // New Channel moved into the header; it opens the create sheet.
    await user.click(
      within(header).getByRole('button', { name: 'New Channel' }),
    )
    expect(await screen.findByRole('dialog')).toBeTruthy()
  })

  it('marks the select-all checkbox indeterminate for a partial selection', async () => {
    const user = userEvent.setup()
    const items = [
      makeChannel({ id: 1, name: 'alpha' }),
      makeChannel({ id: 2, name: 'beta' }),
    ]
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeListResponse(items))
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('alpha')

    const selectAll = screen.getByLabelText(
      'Select all channels on this page',
    ) as HTMLInputElement
    expect(selectAll.indeterminate).toBe(false)

    await user.click(screen.getByLabelText('Select alpha'))
    expect(selectAll.indeterminate).toBe(true)
    expect(selectAll.checked).toBe(false)

    await user.click(screen.getByLabelText('Select beta'))
    await waitFor(() => {
      expect(selectAll.indeterminate).toBe(false)
    })
    expect(selectAll.checked).toBe(true)
  })

  it('opens the performance dialog from the dossier and refreshes on completion', async () => {
    const user = userEvent.setup()
    const channel = makeChannel({
      id: 21,
      name: 'perf-channel',
      supported_models: ['m1'],
    })
    const requests = installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeListResponse([channel]))
      }
      if (
        request.method === 'POST' &&
        request.url.pathname === '/api/channels/21/performance-tests'
      ) {
        return jsonResponse({
          id: 1,
          channel_id: 21,
          model: 'm1',
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
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('perf-channel')

    await user.click(
      screen.getByRole('button', { name: 'Expand details for perf-channel' }),
    )
    const detail = await screen.findByTestId('channel-detail-21')
    const before = requests.filter(
      (request) => request.method === 'GET' && request.url.pathname === '/api/channels',
    ).length

    await user.click(
      within(detail).getByRole('button', { name: 'Performance test' }),
    )
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Start' }))

    await waitFor(() => {
      expect(
        findRequest(requests, 'POST', '/api/channels/21/performance-tests'),
      ).toBeDefined()
    })

    // Completion invalidates the list query so the dossier re-reads the row.
    await waitFor(() => {
      expect(
        requests.filter(
          (request) =>
            request.method === 'GET' && request.url.pathname === '/api/channels',
        ).length,
      ).toBeGreaterThan(before)
    })
  })

  it('marks the table busy and avoids misleading totals while loading', async () => {
    let release: ((response: Response) => void) | null = null
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return new Promise<Response>((resolve) => {
          release = resolve
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)

    const table = screen.getByTestId('channels-table')
    expect(table.getAttribute('aria-busy')).toBe('true')
    // The first page has not arrived, so neither the total nor the page
    // indicator may claim "0 channels" / "Page 1 of 1".
    expect(screen.getByTestId('channels-total').textContent).toBe('—')
    expect(screen.getByTestId('channels-page-indicator').textContent).toBe('—')
    expect(screen.queryByText('0 channels')).toBeNull()

    await waitFor(() => {
      expect(release).not.toBeNull()
    })
    const resolve = release as ((response: Response) => void) | null
    resolve?.(jsonResponse(makeListResponse([])))

    await screen.findByText('No channels yet')
    expect(table.getAttribute('aria-busy')).toBe('false')
    expect(screen.getByTestId('channels-total').textContent).toBe('0 channels')
  })
})
