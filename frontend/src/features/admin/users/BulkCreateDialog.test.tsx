import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor } from '@testing-library/react'
import { File as NodeFile } from 'node:buffer'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { BulkCreateDialog } from './BulkCreateDialog'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

let downloads: string[] = []

beforeEach(() => {
  downloads = []
  // jsdom 未实现 object URL；断言只关心文件名与被点击一次。
  Object.defineProperty(URL, 'createObjectURL', {
    configurable: true,
    writable: true,
    value: vi.fn(() => 'blob:mock'),
  })
  Object.defineProperty(URL, 'revokeObjectURL', {
    configurable: true,
    writable: true,
    value: vi.fn(),
  })
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(
    function captureDownload(this: HTMLAnchorElement) {
      downloads.push(this.download)
    },
  )
})

afterEach(() => {
  vi.restoreAllMocks()
})

function groupsHandler() {
  return http.get(`${ORIGIN}/api/groups`, () =>
    HttpResponse.json({
      items: [
        {
          id: 1,
          name: 'default',
          description: null,
          ratio: 100,
          user_selectable: true,
          is_system: true,
          status: 1,
          users_count: 0,
          pricing_overrides_count: 0,
          channels_count: 0,
          created_at: '2026-10-01T10:00:00Z',
          updated_at: '2026-10-01T10:00:00Z',
        },
      ],
      total: 1,
      page: 1,
      page_size: 100,
    }),
  )
}

async function renderDialog() {
  const onOpenChange = vi.fn()
  const onCreated = vi.fn()
  const result = await renderWithProviders(
    <BulkCreateDialog open onOpenChange={onOpenChange} onCreated={onCreated} />,
    ['common', 'admin'],
  )
  return { ...result, user: userEvent.setup(), onOpenChange, onCreated }
}

/**
 * Node's File keeps the multipart body readable by undici inside MSW; the
 * jsdom File holds a cross-realm stream that never settles when a handler
 * reads the body. The cast bridges the two DOM typings.
 */
function csvFile(): File {
  return new NodeFile(
    ['username,password,customer_type,group\n'],
    'users.csv',
    { type: 'text/csv' },
  ) as unknown as globalThis.File
}

describe('BulkCreateDialog', () => {
  it('downloads the CSV template', async () => {
    server.use(
      http.get(`${ORIGIN}/api/users/bulk/template`, () =>
        HttpResponse.text('username,password,customer_type,group\n', {
          headers: {
            'Content-Type': 'text/csv; charset=utf-8',
            'Content-Disposition': 'attachment; filename="newtg-users-template.csv"',
          },
        }),
      ),
    )

    const { user } = await renderDialog()
    await user.click(screen.getByRole('button', { name: 'Download template' }))

    await waitFor(() => {
      expect(downloads).toEqual(['newtg-users-template.csv'])
    })
  })

  it('imports a CSV and reports created count', async () => {
    let submittedBody = ''
    server.use(
      http.post(`${ORIGIN}/api/users/bulk/import`, async ({ request }) => {
        const bytes = await request.arrayBuffer()
        submittedBody = new TextDecoder().decode(bytes)
        return HttpResponse.json({ created: 3 }, { status: 201 })
      }),
    )

    const { user, onCreated, onOpenChange } = await renderDialog()
    await user.upload(screen.getByLabelText('CSV file'), csvFile())
    await user.click(screen.getByRole('button', { name: 'Import' }))

    await waitFor(() => {
      expect(onCreated).toHaveBeenCalledTimes(1)
    })
    expect(onOpenChange).toHaveBeenCalledWith(false)
    // The body is multipart: both parts must reach the server. The file's
    // payload is not byte-asserted because jsdom hands MSW a cross-realm File
    // that serializes as a placeholder string; the field itself still ships.
    expect(submittedBody).toContain('name="file"')
    expect(submittedBody).toContain('name="must_change_password"')
    expect(submittedBody).toContain('true')
    // Success is announced; no row-error list is rendered.
    expect(screen.queryByText(/Fix these rows/)).toBeNull()
  })

  it('lists per-row validation errors and keeps the dialog open', async () => {
    server.use(
      http.post(`${ORIGIN}/api/users/bulk/import`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/validation_failed',
            title: 'Validation failed',
            status: 422,
            errors: [
              { line: 2, field: 'password', code: 'password_invalid', username: 'alice' },
              {
                line: 4,
                field: 'customer_type',
                code: 'customer_type_invalid',
                username: 'bob',
              },
            ],
          },
          { status: 422 },
        ),
      ),
    )

    const { user, onCreated, onOpenChange } = await renderDialog()
    await user.upload(screen.getByLabelText('CSV file'), csvFile())
    await user.click(screen.getByRole('button', { name: 'Import' }))

    expect(await screen.findByText('Line 2')).toBeTruthy()
    expect(screen.getByText('Line 4')).toBeTruthy()
    expect(screen.getByText(/@alice/)).toBeTruthy()
    expect(screen.getByText(/Invalid password/)).toBeTruthy()
    expect(screen.getByText(/Invalid customer type/)).toBeTruthy()
    expect(onCreated).not.toHaveBeenCalled()
    expect(onOpenChange).not.toHaveBeenCalledWith(false)
    // The dialog itself is still on screen.
    expect(screen.getByRole('dialog', { name: 'Bulk create users' })).toBeTruthy()
  })

  it('lists header errors returned as 400 instead of a generic failure', async () => {
    // handlers_bulk_users.go maps a malformed header (and a wrong field count)
    // to 400 with the same {errors:[…]} body 422 uses.
    server.use(
      http.post(`${ORIGIN}/api/users/bulk/import`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/validation_failed',
            title: 'Validation failed',
            status: 400,
            errors: [{ line: 1, field: 'header', code: 'header_invalid' }],
          },
          { status: 400 },
        ),
      ),
    )

    const { user, onCreated } = await renderDialog()
    await user.upload(screen.getByLabelText('CSV file'), csvFile())
    await user.click(screen.getByRole('button', { name: 'Import' }))

    expect(await screen.findByText('Line 1')).toBeTruthy()
    expect(screen.getByText(/Invalid CSV header/)).toBeTruthy()
    // Not the retryable-looking generic error.
    expect(screen.queryByText(/The import failed/)).toBeNull()
    expect(onCreated).not.toHaveBeenCalled()
    expect(screen.getByRole('dialog', { name: 'Bulk create users' })).toBeTruthy()
  })

  it('shows the generic form error for a server failure', async () => {
    server.use(
      http.post(`${ORIGIN}/api/users/bulk/import`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/internal',
            title: 'Internal server error',
            status: 500,
          },
          { status: 500 },
        ),
      ),
    )

    const { user, onCreated } = await renderDialog()
    await user.upload(screen.getByLabelText('CSV file'), csvFile())
    await user.click(screen.getByRole('button', { name: 'Import' }))

    expect(await screen.findByText(/The import failed/)).toBeTruthy()
    expect(onCreated).not.toHaveBeenCalled()
    expect(screen.queryByText(/Fix these rows/)).toBeNull()
    expect(screen.getByRole('dialog', { name: 'Bulk create users' })).toBeTruthy()
  })

  it('maps a 413 upload to the file-too-large message', async () => {
    server.use(
      http.post(`${ORIGIN}/api/users/bulk/import`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/invalid_request',
            title: 'File too large',
            status: 413,
          },
          { status: 413 },
        ),
      ),
    )

    const { user, onCreated } = await renderDialog()
    await user.upload(screen.getByLabelText('CSV file'), csvFile())
    await user.click(screen.getByRole('button', { name: 'Import' }))

    expect(await screen.findByText(/File exceeds 1 MiB/)).toBeTruthy()
    expect(onCreated).not.toHaveBeenCalled()
  })

  it('shows the generic form error when the request itself fails', async () => {
    server.use(
      http.post(`${ORIGIN}/api/users/bulk/import`, () => HttpResponse.error()),
    )

    const { user, onCreated } = await renderDialog()
    await user.upload(screen.getByLabelText('CSV file'), csvFile())
    await user.click(screen.getByRole('button', { name: 'Import' }))

    expect(await screen.findByText(/The import failed/)).toBeTruthy()
    expect(onCreated).not.toHaveBeenCalled()
    // The submit control is released again after the failure.
    expect(
      (screen.getByRole('button', { name: 'Import' }) as HTMLButtonElement).disabled,
    ).toBe(false)
  })

  it('sends must_change_password=false when the checkbox is cleared', async () => {
    let submittedBody = ''
    server.use(
      http.post(`${ORIGIN}/api/users/bulk/import`, async ({ request }) => {
        const bytes = await request.arrayBuffer()
        submittedBody = new TextDecoder().decode(bytes)
        return HttpResponse.json({ created: 1 }, { status: 201 })
      }),
    )

    const { user, onCreated } = await renderDialog()
    await user.upload(screen.getByLabelText('CSV file'), csvFile())
    await user.click(
      screen.getByRole('checkbox', {
        name: 'Require a password change at first sign-in',
      }),
    )
    await user.click(screen.getByRole('button', { name: 'Import' }))

    await waitFor(() => {
      expect(onCreated).toHaveBeenCalledTimes(1)
    })
    expect(submittedBody).toContain(
      'name="must_change_password"\r\n\r\nfalse',
    )
    expect(submittedBody).not.toContain(
      'name="must_change_password"\r\n\r\ntrue',
    )
  })

  it('generates users and downloads credentials only once', async () => {
    let body: unknown
    server.use(
      groupsHandler(),
      http.post(`${ORIGIN}/api/users/bulk/generate`, async ({ request }) => {
        body = await request.json()
        return HttpResponse.text(
          'username,password,customer_type,group\nNewTG_user0001,secret,general,default\n',
          {
            headers: {
              'Content-Type': 'text/csv; charset=utf-8',
              'Content-Disposition':
                'attachment; filename="newtg-users-20261027T000000Z.csv"',
              'Cache-Control': 'no-store',
            },
          },
        )
      }),
    )

    const { user, onCreated } = await renderDialog()
    await user.click(screen.getByRole('tab', { name: 'Generate' }))

    await user.type(screen.getByLabelText('Number of users'), '3')
    await user.selectOptions(screen.getByLabelText('Customer type'), 'enterprise')
    await user.type(screen.getByLabelText('Group'), 'default')
    await user.click(screen.getByRole('button', { name: 'Generate' }))

    await waitFor(() => {
      expect(downloads).toEqual(['newtg-users-20261027T000000Z.csv'])
    })
    expect(body).toEqual({ count: 3, customer_type: 'enterprise', group: 'default' })
    expect(onCreated).toHaveBeenCalledTimes(1)
    // The one-shot nature is stated, and no "download again" control exists.
    expect(screen.getAllByText(/only copy|cannot view it again/).length).toBeGreaterThan(0)
    expect(screen.queryByRole('button', { name: /download again/i })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Generate' })).toBeNull()
  })

  it('keeps generate submit disabled without a group', async () => {
    server.use(groupsHandler())

    const { user } = await renderDialog()
    await user.click(screen.getByRole('tab', { name: 'Generate' }))

    const submit = screen.getByRole('button', { name: 'Generate' })
    expect((submit as HTMLButtonElement).disabled).toBe(true)

    await user.type(screen.getByLabelText('Number of users'), '2')
    await user.selectOptions(screen.getByLabelText('Customer type'), 'general')
    expect((screen.getByRole('button', { name: 'Generate' }) as HTMLButtonElement).disabled).toBe(
      true,
    )

    await user.type(screen.getByLabelText('Group'), 'default')
    expect((screen.getByRole('button', { name: 'Generate' }) as HTMLButtonElement).disabled).toBe(
      false,
    )
  })

  // F2: the generate endpoint is gated by users:create, but GET /api/groups is
  // gated by groups:manage. An operator with only users:create gets a 403 here
  // and must still be able to name a group by hand (the server validates it).
  it('accepts a typed group when the group list is forbidden', async () => {
    server.use(
      http.get(`${ORIGIN}/api/groups`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/forbidden',
            title: 'Forbidden',
            status: 403,
          },
          { status: 403 },
        ),
      ),
    )

    const { user } = await renderDialog()
    await user.click(screen.getByRole('tab', { name: 'Generate' }))

    expect(await screen.findByText(/Could not load the group list/)).toBeTruthy()

    await user.type(screen.getByLabelText('Number of users'), '2')
    await user.selectOptions(screen.getByLabelText('Customer type'), 'general')
    const groupInput = screen.getByLabelText('Group') as HTMLInputElement
    await user.type(groupInput, 'vip')

    expect(groupInput.value).toBe('vip')
    expect((screen.getByRole('button', { name: 'Generate' }) as HTMLButtonElement).disabled).toBe(
      false,
    )
  })

  // F3: the server has already created the accounts when the response arrives;
  // the CSV is the only copy of their passwords. A download failure must not
  // put the form back — a retry would create a second batch.
  it('reports a created batch when the download fails instead of re-offering the form', async () => {
    server.use(
      groupsHandler(),
      http.post(`${ORIGIN}/api/users/bulk/generate`, () =>
        HttpResponse.text(
          'username,password,customer_type,group\nNewTG_user0001,secret,general,default\n',
          {
            headers: {
              'Content-Type': 'text/csv; charset=utf-8',
              'Content-Disposition':
                'attachment; filename="newtg-users-20261027T000000Z.csv"',
              'Cache-Control': 'no-store',
            },
          },
        ),
      ),
    )
    // The save itself throws after the response was received.
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {
      throw new Error('download blocked')
    })

    const { user, onCreated } = await renderDialog()
    await user.click(screen.getByRole('tab', { name: 'Generate' }))

    await user.type(screen.getByLabelText('Number of users'), '2')
    await user.selectOptions(screen.getByLabelText('Customer type'), 'general')
    await user.type(screen.getByLabelText('Group'), 'default')
    await user.click(screen.getByRole('button', { name: 'Generate' }))

    expect(await screen.findByText(/accounts were created/i)).toBeTruthy()
    expect(onCreated).toHaveBeenCalledTimes(1)
    // The generate form is gone: there is no path to a second batch.
    expect(screen.queryByRole('button', { name: 'Generate' })).toBeNull()
    expect(screen.queryByLabelText('Number of users')).toBeNull()
  })
})
