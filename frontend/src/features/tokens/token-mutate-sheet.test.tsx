import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

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

function baseRoutes(
  handler: Parameters<typeof installFetch>[0],
  items = [makeToken({ id: 1, name: 'alpha' })],
) {
  return installFetch((request) => {
    if (request.url.pathname === '/api/token/auto-groups') {
      return jsonResponse(AUTO_GROUPS)
    }
    if (request.method === 'GET' && request.url.pathname === '/api/tokens') {
      return jsonResponse(makeListResponse(items))
    }
    return handler(request)
  })
}

async function openCreateSheet(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('button', { name: 'Create Token' }))
  return screen.findByRole('dialog', { name: 'Create Token' })
}

describe('token creation', () => {
  it('creates a single token and reveals its key exactly once', async () => {
    const user = userEvent.setup()
    const requests = baseRoutes((request) => {
      if (request.method === 'POST' && request.url.pathname === '/api/tokens') {
        return jsonResponse({
          created: [
            {
              ...makeToken({ id: 9, name: 'my-token', group: 'default' }),
              key: `nkt-${'A'.repeat(48)}`,
            },
          ],
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<TokenListPage />)
    await screen.findByText('alpha')
    const sheet = await openCreateSheet(user)

    await user.type(within(sheet).getByLabelText('Name'), 'my-token')
    await user.click(within(sheet).getByRole('button', { name: 'Create' }))

    await waitFor(() => {
      const create = findRequest(requests, 'POST', '/api/tokens')
      expect(create?.body).toMatchObject({
        name: 'my-token',
        group: 'default',
        quantity: 1,
        unlimited_quota: true,
      })
      expect(create?.headers.get('Idempotency-Key')).toBeTruthy()
    })

    const reveal = await screen.findByRole('dialog', { name: 'Save your API key' })
    expect(within(reveal).getByText(`nkt-${'A'.repeat(48)}`)).toBeTruthy()
    expect(
      within(reveal).getByText(/This is the only time you can view this key/),
    ).toBeTruthy()
    await user.click(
      within(reveal).getByRole('button', { name: "I've saved it, done" }),
    )
    await waitFor(() => {
      expect(
        screen.queryByRole('dialog', { name: 'Save your API key' }),
      ).toBeNull()
    })
  })

  it('shows every created key for a quantity batch', async () => {
    const user = userEvent.setup()
    baseRoutes((request) => {
      if (request.method === 'POST' && request.url.pathname === '/api/tokens') {
        return jsonResponse({
          created: [1, 2, 3].map((index) => ({
            ...makeToken({ id: 10 + index, name: `batch-${index}` }),
            key: `nkt-key-${index}`,
          })),
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<TokenListPage />)
    await screen.findByText('alpha')
    const sheet = await openCreateSheet(user)

    await user.type(within(sheet).getByLabelText('Name'), 'batch')
    const quantity = within(sheet).getByLabelText('Quantity')
    await user.clear(quantity)
    await user.type(quantity, '3')
    await user.click(within(sheet).getByRole('button', { name: 'Create' }))

    const reveal = await screen.findByRole('dialog', { name: 'Save your API key' })
    expect(within(reveal).getByText('3 total')).toBeTruthy()
    expect(within(reveal).getByText('nkt-key-1')).toBeTruthy()
    expect(within(reveal).getByText('nkt-key-3')).toBeTruthy()
  })

  it('validates the name and quantity before sending', async () => {
    const user = userEvent.setup()
    const requests = baseRoutes(() =>
      problemResponse(404, { type: 'about:blank', title: 'not found' }),
    )

    renderWithProviders(<TokenListPage />)
    await screen.findByText('alpha')
    const sheet = await openCreateSheet(user)

    await user.click(within(sheet).getByRole('button', { name: 'Create' }))
    expect(await within(sheet).findByText('Name is required')).toBeTruthy()

    await user.type(within(sheet).getByLabelText('Name'), 'valid')
    const quantity = within(sheet).getByLabelText('Quantity')
    await user.clear(quantity)
    await user.type(quantity, '101')
    await user.click(within(sheet).getByRole('button', { name: 'Create' }))
    expect(
      await within(sheet).findByText('Quantity must be between 1 and 100'),
    ).toBeTruthy()

    expect(findRequest(requests, 'POST', '/api/tokens')).toBeUndefined()
  })

  it('maps a server field error back to the form', async () => {
    const user = userEvent.setup()
    baseRoutes((request) => {
      if (request.method === 'POST' && request.url.pathname === '/api/tokens') {
        return problemResponse(400, {
          type: 'https://newtg.dev/problems/invalid_request',
          title: 'Invalid request',
          detail: 'token name is required',
          fields: ['name'],
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<TokenListPage />)
    await screen.findByText('alpha')
    const sheet = await openCreateSheet(user)
    await user.type(within(sheet).getByLabelText('Name'), 'valid')
    await user.click(within(sheet).getByRole('button', { name: 'Create' }))

    expect(await within(sheet).findByText('token name is required')).toBeTruthy()
  })

  it('edits the ordered auto-group list and submits the displayed order', async () => {
    const user = userEvent.setup()
    const requests = baseRoutes((request) => {
      if (request.method === 'POST' && request.url.pathname === '/api/tokens') {
        return jsonResponse({
          created: [{ ...makeToken({ id: 20, name: 'auto-token' }), key: 'nkt-key' }],
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<TokenListPage />)
    await screen.findByText('alpha')
    const sheet = await openCreateSheet(user)
    await user.type(within(sheet).getByLabelText('Name'), 'auto-token')
    await user.selectOptions(within(sheet).getByLabelText('Group'), 'auto')

    const addSelect = await within(sheet).findByLabelText('Add group')
    await user.selectOptions(addSelect, 'pro')
    await user.click(within(sheet).getByRole('button', { name: 'Add' }))
    await user.selectOptions(within(sheet).getByLabelText('Add group'), 'default')
    await user.click(within(sheet).getByRole('button', { name: 'Add' }))
    expect(within(sheet).getByText('2/5 selected')).toBeTruthy()

    await user.click(
      within(sheet).getByRole('button', { name: 'Move default up' }),
    )
    expect(
      within(sheet)
        .getAllByTestId('auto-group-item')
        .map((item) => item.getAttribute('data-group')),
    ).toEqual(['default', 'pro'])

    await user.click(within(sheet).getByRole('button', { name: 'Create' }))
    await waitFor(() => {
      expect(findRequest(requests, 'POST', '/api/tokens')?.body).toMatchObject({
        group: 'auto',
        auto_groups: ['default', 'pro'],
      })
    })
  })
})

describe('token editing', () => {
  it('saves with the server updated_at and refreshes on 409', async () => {
    const user = userEvent.setup()
    let patchCalls = 0
    const requests = baseRoutes(
      (request) => {
        if (request.method === 'GET' && request.url.pathname === '/api/tokens/1') {
          return jsonResponse(
            makeToken({ id: 1, name: 'alpha', updated_at: '2026-10-06T08:00:00Z' }),
          )
        }
        if (
          request.method === 'PATCH' &&
          request.url.pathname === '/api/tokens/1'
        ) {
          patchCalls += 1
          return problemResponse(409, {
            type: 'https://newtg.dev/problems/conflict_token',
            title: 'Conflict',
            detail: 'token was modified elsewhere',
          })
        }
        return problemResponse(404, { type: 'about:blank', title: 'not found' })
      },
      [makeToken({ id: 1, name: 'alpha', updated_at: '2026-10-06T08:00:00Z' })],
    )

    renderWithProviders(<TokenListPage />)
    await screen.findByText('alpha')
    await user.click(screen.getByRole('button', { name: 'Edit alpha' }))

    const sheet = await screen.findByRole('dialog', { name: 'Edit Token' })
    await waitFor(() => {
      expect(within(sheet).getByLabelText('Name')).toHaveProperty(
        'value',
        'alpha',
      )
    })
    await user.clear(within(sheet).getByLabelText('Name'))
    await user.type(within(sheet).getByLabelText('Name'), 'renamed')

    const refreshBefore = requests.filter(
      (request) => request.method === 'GET' && request.url.pathname === '/api/tokens/1',
    ).length

    await user.click(within(sheet).getByRole('button', { name: 'Save' }))

    await waitFor(() => {
      const patch = findRequest(requests, 'PATCH', '/api/tokens/1')
      expect(patch?.body).toMatchObject({
        name: 'renamed',
        updated_at: '2026-10-06T08:00:00Z',
      })
    })
    expect(patchCalls).toBe(1)

    const conflict = await within(sheet).findByRole('alert')
    expect(conflict.textContent).toContain(
      'Token was modified elsewhere, please refresh',
    )
    await user.click(within(conflict).getByRole('button', { name: 'Refresh' }))
    await waitFor(() => {
      const refreshAfter = requests.filter(
        (request) =>
          request.method === 'GET' && request.url.pathname === '/api/tokens/1',
      ).length
      expect(refreshAfter).toBeGreaterThan(refreshBefore)
    })
  })

  it('resets the key after confirmation and reveals the new key', async () => {
    const user = userEvent.setup()
    const requests = baseRoutes(
      (request) => {
        if (request.method === 'GET' && request.url.pathname === '/api/tokens/1') {
          return jsonResponse(makeToken({ id: 1, name: 'alpha' }))
        }
        if (
          request.method === 'POST' &&
          request.url.pathname === '/api/tokens/1:reset-key'
        ) {
          return jsonResponse({
            key: `nkt-${'Z'.repeat(48)}`,
            key_masked: `nkt-${'•'.repeat(24)}`,
          })
        }
        return problemResponse(404, { type: 'about:blank', title: 'not found' })
      },
      [makeToken({ id: 1, name: 'alpha' })],
    )

    renderWithProviders(<TokenListPage />)
    await screen.findByText('alpha')
    await user.click(screen.getByRole('button', { name: 'Edit alpha' }))
    const sheet = await screen.findByRole('dialog', { name: 'Edit Token' })

    await user.click(
      within(await screen.findByRole('dialog', { name: 'Edit Token' })).getByRole(
        'button',
        { name: 'Reset Key' },
      ),
    )
    const confirm = await screen.findByRole('dialog', { name: 'Reset key?' })
    await user.click(within(confirm).getByRole('button', { name: 'Reset Key' }))

    await waitFor(() => {
      expect(findRequest(requests, 'POST', '/api/tokens/1:reset-key')).toBeTruthy()
    })
    const reveal = await screen.findByRole('dialog', { name: 'Save your API key' })
    expect(within(reveal).getByText(`nkt-${'Z'.repeat(48)}`)).toBeTruthy()
    void sheet
  })
})
