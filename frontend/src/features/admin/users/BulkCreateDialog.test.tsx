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
    await user.upload(
      screen.getByLabelText('CSV file'),
      // Node's File keeps the multipart body readable by undici inside MSW;
      // the jsdom File holds a cross-realm stream that never settles. The cast
      // bridges the two DOM typings; both behave as a file input payload.
      new NodeFile(['username,password,customer_type,group\n'], 'users.csv', {
        type: 'text/csv',
      }) as unknown as globalThis.File,
    )
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
    await user.upload(
      screen.getByLabelText('CSV file'),
      new File(['username,password,customer_type,group\n'], 'users.csv', {
        type: 'text/csv',
      }),
    )
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
    await user.selectOptions(screen.getByLabelText('Group'), 'default')
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

    await waitFor(() => {
      expect(screen.getByRole('option', { name: 'default' })).toBeTruthy()
    })
    await user.selectOptions(screen.getByLabelText('Group'), 'default')
    expect((screen.getByRole('button', { name: 'Generate' }) as HTMLButtonElement).disabled).toBe(
      false,
    )
  })
})
