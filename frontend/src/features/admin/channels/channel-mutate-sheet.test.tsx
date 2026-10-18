import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { ChannelListPage } from './channel-list-page'
import { ChannelMutateSheet } from './channel-mutate-sheet'
import {
  findRequest,
  installFetch,
  jsonResponse,
  makeChannel,
  makeListResponse,
  problemResponse,
  renderWithProviders,
} from './test-utils'

describe('ChannelMutateSheet', () => {
  it('creates a channel, sends the api key once, and only ever shows the mask', async () => {
    const user = userEvent.setup()
    const plaintext = 'sk-secret-key-1234'
    const mask = 'sk-…1234'
    const created = makeChannel({
      id: 7,
      name: 'new-channel',
      credentials_masked: { api_key: mask },
      credentials_set: true,
    })
    let createdFlag = false
    const requests = installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeListResponse(createdFlag ? [created] : []))
      }
      if (request.method === 'POST' && request.url.pathname === '/api/channels') {
        createdFlag = true
        return jsonResponse(created, 201)
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('No channels yet')

    const [newButton] = screen.getAllByRole('button', { name: 'New Channel' })
    expect(newButton).toBeDefined()
    await user.click(newButton as HTMLElement)

    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText('Name'), 'new-channel')
    await user.type(within(dialog).getByLabelText('API key'), plaintext)
    await user.type(within(dialog).getByLabelText('Supported models'), 'gpt-4{enter}')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))

    await waitFor(() => {
      expect(createdFlag).toBe(true)
    })

    const post = findRequest(requests, 'POST', '/api/channels')
    expect(post).toBeDefined()
    const body = post?.body as {
      credentials: { api_key: string }
      supported_models: string[]
    }
    expect(body.credentials.api_key).toBe(plaintext)
    expect(body.supported_models).toEqual(['gpt-4'])
    // The plaintext key must occur in exactly one place in the wire payload.
    expect(JSON.stringify(post?.body).split(plaintext)).toHaveLength(2)
    expect(post?.headers.get('Idempotency-Key')).toMatch(
      /^[0-9a-f-]{36}$/,
    )

    // The sheet closes and the refreshed list renders the masked value.
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).toBeNull()
    })
    expect(await screen.findByText('new-channel')).toBeTruthy()

    await user.click(
      screen.getByRole('button', { name: 'Expand details for new-channel' }),
    )
    expect(await screen.findByText(mask)).toBeTruthy()
    expect(screen.queryByText(plaintext)).toBeNull()
  })

  it('omits credentials when the edit form leaves the key blank', async () => {
    const user = userEvent.setup()
    const detail = makeChannel({
      id: 5,
      name: 'edit-me',
      version: '2026-10-03T00:00:00.000Z',
      credentials_masked: { api_key: 'sk-…abcd' },
      credentials_set: true,
    })
    const requests = installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels/5') {
        return jsonResponse(detail)
      }
      if (request.method === 'PATCH' && request.url.pathname === '/api/channels/5') {
        return jsonResponse({ ...detail, name: 'edit-me-2' })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(
      <ChannelMutateSheet
        open
        mode="edit"
        channelId={5}
        onOpenChange={() => {}}
        onSaved={() => {}}
      />,
    )

    const dialog = await screen.findByRole('dialog')
    const apiKey = await within(dialog).findByLabelText('API key')
    expect(apiKey.getAttribute('type')).toBe('password')
    expect(apiKey.getAttribute('placeholder')).toContain('sk-…abcd')
    expect(apiKey.getAttribute('placeholder')).toContain('Leave blank to keep unchanged')

    await user.click(within(dialog).getByRole('button', { name: 'Show API key' }))
    expect(apiKey.getAttribute('type')).toBe('text')

    const nameInput = within(dialog).getByLabelText('Name')
    await user.clear(nameInput)
    await user.type(nameInput, 'edit-me-2')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))

    await waitFor(() => {
      expect(
        findRequest(requests, 'PATCH', '/api/channels/5'),
      ).toBeDefined()
    })
    const patch = findRequest(requests, 'PATCH', '/api/channels/5')
    expect(patch?.body).toMatchObject({
      expected_version: '2026-10-03T00:00:00.000Z',
      name: 'edit-me-2',
    })
    expect(
      Object.prototype.hasOwnProperty.call(patch?.body, 'credentials'),
    ).toBe(false)
  })

  it('drives credential fields and base_url validation from the selected type', async () => {
    const user = userEvent.setup()
    installFetch(() =>
      problemResponse(404, { type: 'about:blank', title: 'not found' }),
    )

    renderWithProviders(
      <ChannelMutateSheet
        open
        mode="create"
        onOpenChange={() => {}}
        onSaved={() => {}}
      />,
    )

    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).queryByLabelText('Deployment')).toBeNull()

    const apiKey = within(dialog).getByLabelText('API key') as HTMLInputElement
    await user.type(apiKey, 'sk-before-switch')
    await user.selectOptions(
      within(dialog).getByLabelText('Type'),
      'azure_openai',
    )

    // Switching type resets credential inputs (modules-06 §3.2).
    expect(apiKey.value).toBe('')
    expect(within(dialog).getByLabelText('Deployment')).toBeTruthy()
    expect(within(dialog).getByLabelText('API version')).toBeTruthy()

    await user.type(within(dialog).getByLabelText('Name'), 'azure-1')
    await user.type(within(dialog).getByLabelText('API key'), 'sk-12345678')
    await user.type(within(dialog).getByLabelText('Supported models'), 'gpt-4{enter}')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))

    expect(
      await within(dialog).findByText(
        'Base URL is required for this channel type.',
      ),
    ).toBeTruthy()
  })

  it('shows the 409 version-conflict message inline', async () => {
    const user = userEvent.setup()
    const detail = makeChannel({
      id: 6,
      name: 'conflicted',
      version: '2026-10-04T00:00:00.000Z',
      credentials_masked: { api_key: 'sk-…wxyz' },
      credentials_set: true,
    })
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels/6') {
        return jsonResponse(detail)
      }
      if (request.method === 'PATCH' && request.url.pathname === '/api/channels/6') {
        return problemResponse(409, {
          type: 'https://newtg.dev/problems/channel_version_conflict',
          title: 'Channel version conflict',
          detail: 'the channel was modified by someone else; refresh and retry',
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(
      <ChannelMutateSheet
        open
        mode="edit"
        channelId={6}
        onOpenChange={() => {}}
        onSaved={() => {}}
      />,
    )

    const dialog = await screen.findByRole('dialog')
    await within(dialog).findByLabelText('API key')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))

    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toContain(
      'the channel was modified by someone else; refresh and retry',
    )
  })

  it('maps a 409 name conflict to the name field', async () => {
    const user = userEvent.setup()
    const detail = makeChannel({
      id: 8,
      name: 'dup',
      credentials_masked: { api_key: 'sk-…0001' },
      credentials_set: true,
    })
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels/8') {
        return jsonResponse(detail)
      }
      if (request.method === 'PATCH' && request.url.pathname === '/api/channels/8') {
        return problemResponse(409, {
          type: 'https://newtg.dev/problems/channel_name_conflict',
          title: 'Channel name already exists',
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(
      <ChannelMutateSheet
        open
        mode="edit"
        channelId={8}
        onOpenChange={() => {}}
        onSaved={() => {}}
      />,
    )

    const dialog = await screen.findByRole('dialog')
    await within(dialog).findByLabelText('API key')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))

    expect(
      await screen.findByText('A channel with this name already exists.'),
    ).toBeTruthy()
  })
})
