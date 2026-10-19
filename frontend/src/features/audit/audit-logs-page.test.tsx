import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it } from 'vitest'

import { AuditLogsPage } from './audit-logs-page'
import {
  DEFAULT_AUDIT_LOG_SEARCH,
  type AuditLogSearch,
} from './search'
import {
  type FetchRecord,
  installFetch,
  jsonResponse,
  latestRequest,
  problemResponse,
  renderWithProviders,
  requestsFor,
} from '@/features/logs/test-utils'

const ME_USER = {
  username: 'alice',
  role: 'user' as const,
  status: 'enabled' as const,
  permissions: [] as string[],
}

const ME_ADMIN = {
  ...ME_USER,
  username: 'root',
  role: 'admin' as const,
  permissions: ['logs:read'] as string[],
}

const LOGIN_ROW = {
  id: 1,
  user_id: 2,
  username: 'alice',
  category: 'login' as const,
  ip: '203.0.113.5',
  user_agent:
    'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/125.0 Safari/537.36',
  method: 'POST',
  route: '/api/auth/login',
  status_code: 200,
  result: 'success' as const,
  detail: { action: 'password_login', login_method: 'password' },
  before: null,
  after: null,
  created_at: '2026-10-14T09:41:22Z',
}

const OPERATION_ROW = {
  id: 2,
  user_id: 2,
  username: 'alice',
  category: 'operation' as const,
  ip: '203.0.113.5',
  user_agent: 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) Firefox/128.0',
  method: 'POST',
  route: '/api/tokens',
  status_code: 403,
  result: 'failed' as const,
  detail: {
    action: 'token_create',
    resource_type: 'token',
    resource_id: 7,
    failure_reason: 'quota_exceeded',
  },
  before: { name: 'old-name', status: 'enabled', api_key: 'sk-live-secret' },
  after: { name: 'new-name', status: 'disabled', api_key: 'sk-live-secret' },
  created_at: '2026-10-13T08:00:00Z',
}

function page(items: unknown[], total = items.length, pageNumber = 1, pageSize = 20) {
  return { items, total, page: pageNumber, page_size: pageSize }
}

interface Scenario {
  me?: object
  list?: (request: FetchRecord) => Response
}

function installScenario(scenario: Scenario): FetchRecord[] {
  return installFetch((request) => {
    if (request.url.pathname === '/api/me') {
      return jsonResponse(scenario.me ?? ME_USER)
    }
    if (request.url.pathname === '/api/audit-logs') {
      if (scenario.list !== undefined) return scenario.list(request)
      return jsonResponse(page([]))
    }
    return problemResponse(404, { type: 'about:blank', title: 'not found' })
  })
}

function PageHarness({
  initial,
  onSearch,
}: {
  initial?: Partial<AuditLogSearch>
  onSearch?: (next: AuditLogSearch) => void
}) {
  const [search, setSearch] = useState<AuditLogSearch>({
    ...DEFAULT_AUDIT_LOG_SEARCH,
    ...initial,
  })
  return (
    <AuditLogsPage
      search={search}
      onSearchChange={(next) => {
        onSearch?.(next)
        setSearch(next)
      }}
    />
  )
}

describe('audit logs page', () => {
  it('renders events for a user without a username column and never sends username', async () => {
    const requests = installScenario({
      list: () => jsonResponse(page([LOGIN_ROW, OPERATION_ROW], 2)),
    })
    renderWithProviders(
      <PageHarness initial={{ username: 'someone-else' }} />,
    )

    await screen.findByText('password_login')
    const table = screen.getByRole('table')
    expect(within(table).getByText('Login')).toBeTruthy()
    expect(within(table).getByText('Operation')).toBeTruthy()
    expect(screen.queryByRole('columnheader', { name: 'Username' })).toBeNull()
    expect(screen.queryByLabelText('Username')).toBeNull()
    await waitFor(() => {
      expect(latestRequest(requests, '/api/audit-logs')).toBeDefined()
    })
    expect(
      requestsFor(requests, '/api/audit-logs').every(
        (request) => !request.url.searchParams.has('username'),
      ),
    ).toBe(true)
  })

  it('shows the username column and forwards the filter for logs:read admins', async () => {
    const requests = installScenario({
      me: ME_ADMIN,
      list: () => jsonResponse(page([LOGIN_ROW])),
    })
    renderWithProviders(<PageHarness initial={{ username: 'alice' }} />)

    await screen.findByText('password_login')
    expect(await screen.findByRole('columnheader', { name: 'Username' })).toBeTruthy()
    expect(screen.getByLabelText('Username')).toBeTruthy()
    await waitFor(() => {
      expect(
        latestRequest(requests, '/api/audit-logs')?.url.searchParams.get(
          'username',
        ),
      ).toBe('alice')
    })
  })

  it('syncs category, result and range filters to the request', async () => {
    const user = userEvent.setup()
    const changes: AuditLogSearch[] = []
    const requests = installScenario({
      list: () => jsonResponse(page([LOGIN_ROW])),
    })
    renderWithProviders(<PageHarness onSearch={(next) => changes.push(next)} />)

    await screen.findByText('password_login')
    await user.selectOptions(screen.getByLabelText('Category'), 'operation')
    await user.selectOptions(screen.getByLabelText('Result'), 'failed')
    await user.click(screen.getByRole('radio', { name: '7d' }))

    await waitFor(() => {
      expect(changes.at(-1)?.category).toBe('operation')
      expect(changes.at(-1)?.result).toBe('failed')
      expect(changes.at(-1)?.range).toBe('7d')
      const request = latestRequest(requests, '/api/audit-logs')
      expect(request?.url.searchParams.get('category')).toBe('operation')
      expect(request?.url.searchParams.get('result')).toBe('failed')
      expect(request?.url.searchParams.get('range')).toBe('7d')
    })
  })

  it('opens a category-specific detail dialog with a safe before/after diff', async () => {
    const user = userEvent.setup()
    installScenario({ list: () => jsonResponse(page([OPERATION_ROW])) })
    renderWithProviders(<PageHarness />)

    await screen.findByText('token_create')
    await user.click(screen.getByRole('button', { name: 'View' }))

    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('Audit detail')).toBeTruthy()
    expect(within(dialog).getByText('General')).toBeTruthy()
    expect(within(dialog).getByText('Category fields')).toBeTruthy()
    expect(within(dialog).getByText('token')).toBeTruthy()
    expect(within(dialog).getByText('7')).toBeTruthy()
    expect(within(dialog).getByText('name')).toBeTruthy()
    expect(within(dialog).getByText('old-name')).toBeTruthy()
    expect(within(dialog).getByText('new-name')).toBeTruthy()
    // Secrets are never rendered, even if the backend snapshot leaks a key.
    expect(dialog.textContent).not.toContain('sk-live-secret')
    expect(dialog.textContent).not.toContain('api_key')
  })

  it('renders login-specific detail fields', async () => {
    const user = userEvent.setup()
    installScenario({ list: () => jsonResponse(page([LOGIN_ROW])) })
    renderWithProviders(<PageHarness />)

    await screen.findByText('password_login')
    await user.click(screen.getByRole('button', { name: 'View' }))

    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('Login method')).toBeTruthy()
    expect(within(dialog).getByText('password')).toBeTruthy()
  })

  it('paginates and renders the default empty state', async () => {
    const user = userEvent.setup()
    const requests = installScenario({
      list: () => jsonResponse(page([LOGIN_ROW], 42, 1, 20)),
    })
    renderWithProviders(<PageHarness />)

    await screen.findByText('password_login')
    expect(screen.getByText('Page 1 of 3')).toBeTruthy()
    await user.click(screen.getByRole('button', { name: 'Next page' }))
    await waitFor(() => {
      expect(
        latestRequest(requests, '/api/audit-logs')?.url.searchParams.get('p'),
      ).toBe('2')
    })
  })

  it('renders the empty state with the default-range hint', async () => {
    installScenario({ list: () => jsonResponse(page([])) })
    renderWithProviders(<PageHarness />)

    expect(await screen.findByText('No audit logs')).toBeTruthy()
    expect(
      screen.getByText('Showing the last 30 days by default'),
    ).toBeTruthy()
  })

  it('renders a 429 message with the Retry-After countdown', async () => {
    installScenario({
      list: () =>
        problemResponse(
          429,
          { type: 'rate_limited', title: 'Too many requests' },
          { 'Retry-After': '12' },
        ),
    })
    renderWithProviders(<PageHarness />)

    expect(
      await screen.findByText('Too many requests, retry in 12s'),
    ).toBeTruthy()
    // The filter toolbar stays mounted while the list is rate-limited.
    expect(screen.getByLabelText('Category')).toBeTruthy()
  })

  it('keeps in-flight username text when another filter changes', async () => {
    const user = userEvent.setup()
    const requests = installScenario({
      me: ME_ADMIN,
      list: () => jsonResponse(page([LOGIN_ROW])),
    })
    renderWithProviders(<PageHarness />)

    await screen.findByText('password_login')
    const input = screen.getByLabelText('Username') as HTMLInputElement
    await user.type(input, 'alice')
    await user.selectOptions(screen.getByLabelText('Category'), 'login')
    expect(input.value).toBe('alice')

    await waitFor(
      () => {
        const request = latestRequest(requests, '/api/audit-logs')
        expect(request?.url.searchParams.get('username')).toBe('alice')
        expect(request?.url.searchParams.get('category')).toBe('login')
      },
      { timeout: 2500 },
    )
  })
})
