import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { TokenListPage } from './token-list-page'
import {
  findRequest,
  installFetch,
  jsonResponse,
  makeListResponse,
  makeToken,
  problemResponse,
  renderWithProviders,
} from './test-utils'

const AUTO_GROUPS = {
  groups: [
    { name: 'default', ratio: 1 },
    { name: 'pro', ratio: 1.5 },
  ],
  max_count: 5,
}

function autoGroupsResponse(): Response {
  return jsonResponse(AUTO_GROUPS)
}

function tokenRoute(handler: Parameters<typeof installFetch>[0]) {
  return installFetch((request) => {
    if (request.url.pathname === '/api/token/auto-groups') {
      return autoGroupsResponse()
    }
    return handler(request)
  })
}

describe('TokenListPage', () => {
  it('renders tokens with the fixed masked key and never leaks plaintext', async () => {
    const requests = tokenRoute((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/tokens') {
        return jsonResponse(
          makeListResponse([
            makeToken({ id: 1, name: 'alpha' }),
            makeToken({ id: 2, name: 'beta', status: 'disabled' }),
          ]),
        )
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<TokenListPage />)

    expect(await screen.findByText('alpha')).toBeTruthy()
    expect(screen.getByText('beta')).toBeTruthy()
    expect(within(screen.getByTestId('token-row-1')).getByText('Enabled')).toBeTruthy()
    expect(within(screen.getByTestId('token-row-2')).getByText('Disabled')).toBeTruthy()
    expect(screen.getAllByTestId('masked-key')[0]?.textContent).toBe(
      `nkt-${'•'.repeat(24)}`,
    )
    // The projection can never contain a plaintext key: no `nkt-` followed by
    // the 48-character Base62 payload from the contract is present.
    expect(document.body.textContent ?? '').not.toMatch(/nkt-[A-Za-z0-9]{20,}/)
    expect(findRequest(requests, 'GET', '/api/tokens')?.url.searchParams.get('p')).toBe(
      '1',
    )
  })

  it('sends filter params, sorts the current page, and paginates', async () => {
    const user = userEvent.setup()
    const items = [
      makeToken({ id: 1, name: 'beta', status: 'disabled' }),
      makeToken({ id: 2, name: 'alpha' }),
    ]
    const requests = tokenRoute((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/tokens') {
        return jsonResponse(makeListResponse(items, { total: 40 }))
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<TokenListPage />)
    await screen.findByText('beta')

    await user.type(screen.getByLabelText('Search by name or key'), 'alp')
    await waitFor(() => {
      expect(
        findRequest(requests, 'GET', '/api/tokens')?.url.searchParams.get('keyword'),
      ).toBe('alp')
    })

    await user.selectOptions(screen.getByLabelText('Status'), 'disabled')
    await user.selectOptions(screen.getByLabelText('Quota'), 'low')
    await waitFor(() => {
      const latest = findRequest(requests, 'GET', '/api/tokens')
      expect(latest?.url.searchParams.get('status')).toBe('disabled')
      expect(latest?.url.searchParams.get('quota')).toBe('low')
      expect(latest?.url.searchParams.get('p')).toBe('1')
    })

    // Sorting is page-local: the endpoint freezes `created_at DESC`, so the
    // table offers deterministic ordering over the loaded offset page only.
    const nameHeader = screen.getByRole('columnheader', { name: 'Name' })
    await user.click(within(nameHeader).getByRole('button'))
    const rows = screen.getAllByTestId(/token-row-/)
    expect(rows.map((row) => row.getAttribute('data-testid'))).toEqual([
      'token-row-2',
      'token-row-1',
    ])
    expect(nameHeader.getAttribute('aria-sort')).toBe('ascending')

    await user.selectOptions(screen.getByLabelText('Rows per page'), '10')
    await user.click(screen.getByRole('button', { name: 'Next page' }))
    await waitFor(() => {
      const latest = findRequest(requests, 'GET', '/api/tokens')
      expect(latest?.url.searchParams.get('page_size')).toBe('10')
      expect(latest?.url.searchParams.get('p')).toBe('2')
    })
  })

  it('runs batch enable and surfaces per-row partial failures', async () => {
    const user = userEvent.setup()
    const items = [
      makeToken({ id: 1, name: 'alpha', status: 'disabled' }),
      makeToken({ id: 2, name: 'beta', status: 'disabled' }),
    ]
    const requests = tokenRoute((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/tokens') {
        return jsonResponse(makeListResponse(items))
      }
      if (
        request.method === 'POST' &&
        request.url.pathname === '/api/tokens:batch-enable'
      ) {
        return jsonResponse({
          succeeded: 1,
          failed: [{ id: 2, name: 'beta', detail: 'backend exploded' }],
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<TokenListPage />)
    await screen.findByText('alpha')

    await user.click(screen.getByLabelText('Select alpha'))
    await user.click(screen.getByLabelText('Select beta'))
    expect(screen.getByText('2 selected')).toBeTruthy()

    await user.click(screen.getByRole('button', { name: 'Enable selected' }))

    await waitFor(() => {
      const batch = findRequest(requests, 'POST', '/api/tokens:batch-enable')
      expect(batch?.body).toEqual({ ids: [1, 2] })
    })
    const failures = await screen.findByTestId('batch-failures')
    expect(failures.textContent).toContain('Succeeded 1, failed 1')
    expect(failures.textContent).toContain('beta')
    expect(failures.textContent).toContain('backend exploded')
  })

  it('disables a token through the row power action', async () => {
    const user = userEvent.setup()
    const token = makeToken({ id: 7, name: 'alpha', status: 'enabled' })
    const requests = tokenRoute((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/tokens') {
        return jsonResponse(makeListResponse([token]))
      }
      if (
        request.method === 'POST' &&
        request.url.pathname === '/api/tokens/7:disable'
      ) {
        return jsonResponse({ ...token, status: 'disabled' })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<TokenListPage />)
    await screen.findByText('alpha')

    await user.click(screen.getByRole('button', { name: 'Disable alpha' }))
    await waitFor(() => {
      expect(findRequest(requests, 'POST', '/api/tokens/7:disable')).toBeTruthy()
    })
    expect(await screen.findByText('Token disabled')).toBeTruthy()
  })

  it('confirms deletion and carries the optimistic-lock version', async () => {
    const user = userEvent.setup()
    const token = makeToken({
      id: 3,
      name: 'alpha',
      updated_at: '2026-10-05T12:00:00Z',
    })
    const requests = tokenRoute((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/tokens') {
        return jsonResponse(makeListResponse([token]))
      }
      if (request.method === 'DELETE' && request.url.pathname === '/api/tokens/3') {
        return new Response(null, { status: 204 })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<TokenListPage />)
    await screen.findByText('alpha')

    await user.click(screen.getByRole('button', { name: 'Delete alpha' }))
    const dialog = await screen.findByRole('dialog', { name: 'Delete token?' })
    expect(dialog.textContent).toContain('alpha')
    await user.click(within(dialog).getByRole('button', { name: 'Delete' }))

    await waitFor(() => {
      const remove = findRequest(requests, 'DELETE', '/api/tokens/3')
      expect(remove?.url.searchParams.get('updated_at')).toBe('2026-10-05T12:00:00Z')
    })
    expect(await screen.findByText('Token deleted')).toBeTruthy()
  })

  it('shows the usage column from the list projection', async () => {
    tokenRoute((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/tokens') {
        return jsonResponse(
          makeListResponse([
            makeToken({ id: 1, name: 'unlimited', unlimited_quota: true }),
            makeToken({
              id: 2,
              name: 'metered',
              unlimited_quota: false,
              remain_quota: 5000,
              quota_percent: 50,
            }),
          ]),
        )
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<TokenListPage />)
    await screen.findByText('metered')

    expect(within(screen.getByTestId('token-row-1')).getByText('Unlimited')).toBeTruthy()
    const metered = screen.getByTestId('token-row-2')
    expect(within(metered).getByText('5,000')).toBeTruthy()
    const progress = within(metered).getByRole('progressbar')
    expect(progress.getAttribute('aria-valuenow')).toBe('50')
  })

  it('renders empty, error, and loading states', async () => {
    installFetch((request) => {
      if (request.url.pathname === '/api/token/auto-groups') return autoGroupsResponse()
      return jsonResponse(makeListResponse([]))
    })
    const view = renderWithProviders(<TokenListPage />)
    expect(await screen.findByText('No API tokens yet')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Create Token' })).toBeTruthy()
    view.unmount()

    const requests = installFetch((request) => {
      if (request.url.pathname === '/api/token/auto-groups') return autoGroupsResponse()
      return problemResponse(500, {
        type: 'https://newtg.dev/problems/internal_error',
        title: 'Internal Server Error',
      })
    })
    renderWithProviders(<TokenListPage />)
    expect(await screen.findByText('Failed to load tokens.')).toBeTruthy()
    const before = requests.length
    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }))
    await waitFor(() => {
      expect(requests.length).toBeGreaterThan(before)
    })
  })

  it('shows a loading skeleton while the first page is in flight', async () => {
    installFetch(
      () =>
        new Promise<Response>(() => {
          // Never resolves: keeps the query pending for the assertion.
        }),
    )

    renderWithProviders(<TokenListPage />)
    expect(await screen.findByTestId('tokens-loading')).toBeTruthy()
  })

  it('shows copy-key guidance instead of writing the masked key to the clipboard', async () => {
    const user = userEvent.setup()
    const writeText = vi.fn(async () => undefined)
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    })
    tokenRoute((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/tokens') {
        return jsonResponse(makeListResponse([makeToken({ id: 1, name: 'alpha' })]))
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<TokenListPage />)
    await screen.findByText('alpha')
    await user.click(screen.getByRole('button', { name: 'Copy key for alpha' }))

    expect(await screen.findByRole('alert')).toBeTruthy()
    expect(writeText).not.toHaveBeenCalled()
    expect(
      screen.getByText(/The full key is shown only once at creation/),
    ).toBeTruthy()
  })
})
