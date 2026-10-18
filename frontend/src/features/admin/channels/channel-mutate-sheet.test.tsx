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
  it('creates a channel, sends the api key once, and never re-displays the plaintext', async () => {
    const user = userEvent.setup()
    const plaintext = 'sk-secret-key-1234'
    const mask = 'sk-…1234'
    // The POST/detail DTO carries the mask; the list DTO deliberately does not
    // (GC-6), so the list fixture must stay mask-free.
    const created = makeChannel({
      id: 7,
      name: 'new-channel',
      credentials_masked: { api_key: mask },
      credentials_set: true,
    })
    const listed = makeChannel({
      id: 7,
      name: 'new-channel',
      credentials_set: true,
    })
    let createdFlag = false
    const requests = installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeListResponse(createdFlag ? [listed] : []))
      }
      if (request.method === 'POST' && request.url.pathname === '/api/channels') {
        createdFlag = true
        return jsonResponse(created, 201)
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    await screen.findByText('No channels yet')

    const newButton = screen.getByRole('button', { name: 'New Channel' })
    await user.click(newButton)

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

    // The sheet closes and the refreshed list renders the masked state; the
    // plaintext never comes back from the server.
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).toBeNull()
    })
    expect(await screen.findByText('new-channel')).toBeTruthy()

    await user.click(
      screen.getByRole('button', { name: 'Expand details for new-channel' }),
    )
    expect(await screen.findByText('Set')).toBeTruthy()
    expect(screen.queryByText(plaintext)).toBeNull()
  })

  it('reuses the create idempotency key on retry and mints a new one after an edit', async () => {
    const user = userEvent.setup()
    const requests = installFetch((request) => {
      if (request.method === 'POST' && request.url.pathname === '/api/channels') {
        return problemResponse(500, {
          type: 'https://newtg.dev/problems/internal_error',
          title: 'Internal Server Error',
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    const onOpenChange = () => {}
    renderWithProviders(
      <ChannelMutateSheet
        open
        mode="create"
        onOpenChange={onOpenChange}
        onSaved={() => {}}
      />,
    )

    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText('Name'), 'retry-me')
    await user.type(within(dialog).getByLabelText('API key'), 'sk-12345678')
    await user.type(within(dialog).getByLabelText('Supported models'), 'gpt-4{enter}')

    const save = within(dialog).getByRole('button', { name: 'Save' })
    await user.click(save)
    await waitFor(() => {
      expect(
        requests.filter(
          (request) =>
            request.method === 'POST' &&
            request.url.pathname === '/api/channels',
        ),
      ).toHaveLength(1)
    })

    // Retrying the same payload reuses the key: the backend replays instead
    // of creating a second channel.
    await user.click(save)
    await waitFor(() => {
      const posts = requests.filter(
        (request) =>
          request.method === 'POST' &&
          request.url.pathname === '/api/channels',
      )
      expect(posts).toHaveLength(2)
      expect(posts[0]?.headers.get('Idempotency-Key')).toBe(
        posts[1]?.headers.get('Idempotency-Key'),
      )
    })

    // Editing the payload changes the intent, so the next submit mints a new
    // key rather than replaying the old (different) request.
    const nameInput = within(dialog).getByLabelText('Name')
    await user.clear(nameInput)
    await user.type(nameInput, 'retry-me-2')
    await user.click(save)
    await waitFor(() => {
      const posts = requests.filter(
        (request) =>
          request.method === 'POST' &&
          request.url.pathname === '/api/channels',
      )
      expect(posts).toHaveLength(3)
      expect(posts[2]?.headers.get('Idempotency-Key')).not.toBe(
        posts[0]?.headers.get('Idempotency-Key'),
      )
    })
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

  it('requires a new key when the azure deployment changes on edit', async () => {
    const user = userEvent.setup()
    const detail = makeChannel({
      id: 21,
      name: 'azure-edit',
      type: 'azure_openai',
      credentials_masked: {
        api_key: 'sk-…abcd',
        deployment: 'dep-1',
        api_version: '2024-10-21',
      },
      credentials_set: true,
    })
    const requests = installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels/21') {
        return jsonResponse(detail)
      }
      if (request.method === 'PATCH' && request.url.pathname === '/api/channels/21') {
        return jsonResponse(detail)
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(
      <ChannelMutateSheet
        open
        mode="edit"
        channelId={21}
        onOpenChange={() => {}}
        onSaved={() => {}}
      />,
    )

    const dialog = await screen.findByRole('dialog')
    const deployment = await within(dialog).findByLabelText('Deployment')
    await user.clear(deployment)
    await user.type(deployment, 'dep-2')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))

    expect(
      await within(dialog).findByText(
        'Enter a new API key to change the deployment or API version.',
      ),
    ).toBeTruthy()
    expect(findRequest(requests, 'PATCH', '/api/channels/21')).toBeUndefined()
  })

  it('requires full azure credentials when switching type on edit', async () => {
    const user = userEvent.setup()
    const detail = makeChannel({
      id: 22,
      name: 'switcher',
      type: 'openai',
      credentials_masked: { api_key: 'sk-…wxyz' },
      credentials_set: true,
    })
    const requests = installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels/22') {
        return jsonResponse(detail)
      }
      if (request.method === 'PATCH' && request.url.pathname === '/api/channels/22') {
        return jsonResponse({ ...detail, type: 'azure_openai' })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(
      <ChannelMutateSheet
        open
        mode="edit"
        channelId={22}
        onOpenChange={() => {}}
        onSaved={() => {}}
      />,
    )

    const dialog = await screen.findByRole('dialog')
    await within(dialog).findByLabelText('API key')
    await user.selectOptions(
      within(dialog).getByLabelText('Type'),
      'azure_openai',
    )
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))

    expect(await within(dialog).findByText('API key is required.')).toBeTruthy()
    expect(
      await within(dialog).findByText('Deployment is required for Azure OpenAI.'),
    ).toBeTruthy()
    expect(
      await within(dialog).findByText('API version is required for Azure OpenAI.'),
    ).toBeTruthy()
    expect(findRequest(requests, 'PATCH', '/api/channels/22')).toBeUndefined()

    // Supplying the complete set turns the same submission into a valid PATCH.
    await user.type(within(dialog).getByLabelText('API key'), 'sk-azure-123')
    await user.type(within(dialog).getByLabelText('Deployment'), 'dep-1')
    await user.type(within(dialog).getByLabelText('API version'), '2024-10-21')
    await user.type(
      within(dialog).getByLabelText('Base URL'),
      'https://example.openai.azure.com',
    )
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))

    await waitFor(() => {
      expect(
        findRequest(requests, 'PATCH', '/api/channels/22'),
      ).toBeDefined()
    })
    expect(
      findRequest(requests, 'PATCH', '/api/channels/22')?.body,
    ).toMatchObject({
      type: 'azure_openai',
      credentials: {
        api_key: 'sk-azure-123',
        deployment: 'dep-1',
        api_version: '2024-10-21',
      },
    })
  })

  it('recovers from a 409 version conflict by reloading the baseline version', async () => {
    const user = userEvent.setup()
    const detail = makeChannel({
      id: 6,
      name: 'conflicted',
      version: '2026-10-04T00:00:00.000Z',
      credentials_masked: { api_key: 'sk-…wxyz' },
      credentials_set: true,
    })
    const latest = {
      ...detail,
      version: '2026-10-05T12:00:00.000Z',
    }
    let detailCalls = 0
    let patchCalls = 0
    const requests = installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels/6') {
        detailCalls += 1
        return jsonResponse(detailCalls === 1 ? detail : latest)
      }
      if (request.method === 'PATCH' && request.url.pathname === '/api/channels/6') {
        patchCalls += 1
        if (patchCalls === 1) {
          return problemResponse(409, {
            type: 'https://newtg.dev/problems/channel_version_conflict',
            title: 'Channel version conflict',
            detail: 'the channel was modified by someone else; refresh and retry',
          })
        }
        return jsonResponse(latest)
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

    // Keep an unsaved edit so the reload can be checked for re-application.
    const nameInput = within(dialog).getByLabelText('Name') as HTMLInputElement
    await user.clear(nameInput)
    await user.type(nameInput, 'conflicted-renamed')

    const save = within(dialog).getByRole('button', {
      name: 'Save',
    }) as HTMLButtonElement
    await user.click(save)

    const alert = await screen.findByRole('alert')
    expect(alert.textContent).toContain(
      'the channel was modified by someone else; refresh and retry',
    )
    // The stale expected_version makes every retry fail: Save is locked until
    // the baseline is reloaded on purpose.
    expect(save.disabled).toBe(true)

    await user.click(within(dialog).getByRole('button', { name: 'Reload latest' }))

    await waitFor(() => {
      expect(detailCalls).toBe(2)
    })
    await waitFor(() => {
      expect(save.disabled).toBe(false)
    })
    // The unsaved edit survives the reload.
    expect(nameInput.value).toBe('conflicted-renamed')
    expect(within(dialog).queryByRole('alert')).toBeNull()

    await user.click(save)
    await waitFor(() => {
      expect(patchCalls).toBe(2)
    })
    expect(findRequest(requests, 'PATCH', '/api/channels/6')?.body).toMatchObject({
      expected_version: '2026-10-05T12:00:00.000Z',
      name: 'conflicted-renamed',
    })
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

  it('maps 422 errors[] entries onto the matching form fields', async () => {
    const user = userEvent.setup()
    const detail = makeChannel({
      id: 31,
      name: 'validated',
      credentials_masked: { api_key: 'sk-…9876' },
      credentials_set: true,
    })
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels/31') {
        return jsonResponse(detail)
      }
      if (request.method === 'PATCH' && request.url.pathname === '/api/channels/31') {
        return problemResponse(422, {
          type: 'https://newtg.dev/problems/validation_failed',
          title: 'Validation failed',
          detail: 'two fields are invalid',
          errors: [
            { field: 'name', message: 'Name is reserved by policy.' },
            { field: 'supported_models', message: 'Model gpt-4 is not allowed.' },
          ],
        })
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(
      <ChannelMutateSheet
        open
        mode="edit"
        channelId={31}
        onOpenChange={() => {}}
        onSaved={() => {}}
      />,
    )

    const dialog = await screen.findByRole('dialog')
    await within(dialog).findByLabelText('API key')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))

    expect(await within(dialog).findByText('Name is reserved by policy.')).toBeTruthy()
    expect(
      within(dialog).getByText('Model gpt-4 is not allowed.'),
    ).toBeTruthy()
  })
})
