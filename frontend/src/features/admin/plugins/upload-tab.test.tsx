import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { fireEvent, screen, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { UploadTab } from './upload-tab'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const UPDATED_AT = '2026-10-19T10:00:00Z'

function fileInput(): HTMLInputElement {
  return screen.getByTestId('plugin-file-input') as HTMLInputElement
}

async function chooseFile(file: File): Promise<void> {
  fireEvent.change(fileInput(), { target: { files: [file] } })
}

function renderTab(overrides: Partial<Parameters<typeof UploadTab>[0]> = {}) {
  return renderWithProviders(<UploadTab canManage online {...overrides} />, [
    'common',
    'admin',
  ])
}

describe('upload tab (F-08-30)', () => {
  it('prechecks the .js suffix and the 2 MiB limit before uploading', async () => {
    let requests = 0
    server.use(
      http.post(`${ORIGIN}/api/plugin/task`, () => {
        requests += 1
        return HttpResponse.json({}, { status: 201 })
      }),
    )
    await renderTab()

    await chooseFile(new File(['console.log(1)'], 'plugin.txt', { type: 'text/plain' }))
    expect(await screen.findByText('Only .js files are accepted.')).toBeTruthy()

    const oversized = new File(['x'], 'big.js', { type: 'application/javascript' })
    Object.defineProperty(oversized, 'size', { value: 2 * 1024 * 1024 + 1 })
    await chooseFile(oversized)
    expect(await screen.findByText('The file exceeds the 2 MiB limit.')).toBeTruthy()

    expect(requests).toBe(0)
    // URL 导入端点未实现：界面不渲染该入口。
    expect(screen.queryByText(/from URL|From URL|Import from URL/i)).toBeNull()
  })

  it('shows a copyable compile-error panel and never offers a bypass', async () => {
    let body: unknown = null
    let idempotencyKey: string | null = null
    server.use(
      http.post(`${ORIGIN}/api/plugin/task`, async ({ request }) => {
        body = await request.json()
        idempotencyKey = request.headers.get('Idempotency-Key')
        return HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/plugin_compile_failed',
            title: 'Plugin compile failed',
            status: 422,
            detail: 'the plugin source failed compilation or validation',
            issues: [
              {
                key: '',
                fieldPath: 'plugin.js:3',
                locale: '',
                rule: 'syntax',
                message: 'unexpected token',
              },
            ],
          },
          { status: 422, headers: { 'Content-Type': 'application/problem+json' } },
        )
      }),
    )

    await renderTab()
    await chooseFile(new File(['export {}'], 'broken.js'))
    await userEvent.click(await screen.findByRole('button', { name: 'Confirm install' }))

    const panel = await screen.findByTestId('plugin-compile-error')
    expect(panel.textContent).toContain(
      'Plugin failed to compile. Nothing was saved: plugin.js:3: unexpected token',
    )
    expect(panel.getAttribute('role')).toBe('alert')
    expect(body).toMatchObject({ source_text: 'export {}' })
    expect(idempotencyKey).not.toBeNull()
    // 没有任何「仍然安装」绕过入口。
    expect(screen.queryByRole('button', { name: /install anyway|still install/i })).toBeNull()
  })

  it('installs successfully, previews the version and states it is not activated', async () => {
    server.use(
      http.post(`${ORIGIN}/api/plugin/task`, () =>
        HttpResponse.json(
          {
            created: true,
            plugin: {
              key: 'mj',
              name: 'MJ',
              icon: null,
              source: 'upload',
              builtin: false,
              enabled: true,
              active_version: null,
              description: null,
              runtime_status: 'registered',
              bound_channels: 0,
              updated_at: UPDATED_AT,
            },
            version: {
              id: 'v1',
              key: 'mj',
              version: '0.9.1',
              api_version: 'v1',
              source_hash: 'b'.repeat(64),
              icon: null,
              enabled: true,
              active: false,
              uploaded_by: 1,
              created_at: UPDATED_AT,
            },
          },
          { status: 201 },
        ),
      ),
    )

    await renderTab()
    await chooseFile(new File(['source'], 'mj.js'))
    await userEvent.click(await screen.findByRole('button', { name: 'Confirm install' }))

    const preview = await screen.findByTestId('plugin-upload-preview')
    expect(preview.textContent).toContain('mj')
    expect(preview.textContent).toContain('0.9.1')
    expect(
      screen.getByText(
        'The version was installed but is not activated. Activate it from the change history when you are ready.',
      ),
    ).toBeTruthy()
  })

  it('accepts keyboard activation of the dropzone', async () => {
    server.use(http.post(`${ORIGIN}/api/plugin/task`, () => HttpResponse.json({}, { status: 201 })))
    await renderTab()
    const dropzone = screen.getByTestId('plugin-dropzone')
    expect(dropzone.getAttribute('tabindex')).toBe('0')
    const click = vi.spyOn(fileInput(), 'click').mockImplementation(() => undefined)
    fireEvent.keyDown(dropzone, { key: 'Enter' })
    await waitFor(() => expect(click).toHaveBeenCalledTimes(1))
    click.mockRestore()
  })
})
