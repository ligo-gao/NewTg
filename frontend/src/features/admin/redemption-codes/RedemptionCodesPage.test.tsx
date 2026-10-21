import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

import type { components } from '@/api/schema'
import { renderWithProviders } from '@/test-support/providers'
import { useToastStore } from '@/shared/ui/toast'

import { RedemptionCodesPage } from './RedemptionCodesPage'
import type { RedemptionSearch } from './redemption-search'

type RedemptionCode = components['schemas']['RedemptionCode']

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  vi.unstubAllGlobals()
})
afterAll(() => server.close())
beforeEach(() => {
  useToastStore.setState({ toasts: [] })
  // 每个用例都先装一套默认处理器；用例内再 `server.use` 覆盖时 MSW 以最后
  // 注册者优先，因此不需要额外的清理分支。
  installHandlers()
})

function codeRow(overrides: Partial<RedemptionCode> = {}): RedemptionCode {
  return {
    id: 12,
    batch_name: 'launch',
    credits: 10000,
    status: 'unused',
    code_mask: '••••••',
    created_by: 1,
    used_by: null,
    used_at: null,
    expires_at: null,
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
    ...overrides,
  }
}

interface InstallOptions {
  items?: RedemptionCode[]
  total?: number
  record?: (params: URLSearchParams) => void
  createStatus?: number
  createBody?: Record<string, unknown>
  onDelete?: (id: number) => void
  onPatch?: (id: string, body: string) => Response | undefined
  onBatchDelete?: (body: string) => void
  /** 调用者的生效权限（决定导出文件是否包含 used_by，R-49）。 */
  permissions?: string[]
}

function installHandlers({
  items = [codeRow()],
  total = 1,
  record,
  createStatus = 201,
  createBody,
  onDelete,
  onPatch,
  onBatchDelete,
  permissions = ['redemption:manage', 'users:read'],
}: InstallOptions = {}) {
  server.use(
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({
        username: 'root',
        role: 'admin',
        status: 'enabled',
        permissions,
      }),
    ),
    http.get(`${ORIGIN}/api/redemption-codes`, ({ request }) => {
      const params = new URL(request.url).searchParams
      record?.(params)
      return HttpResponse.json({
        items,
        total,
        page: Number(params.get('p') ?? '1'),
        page_size: Number(params.get('page_size') ?? '20'),
      })
    }),
    http.post(`${ORIGIN}/api/redemption-codes`, async ({ request }) => {
      const body = (await request.json()) as { count: number }
      if (createStatus !== 201) {
        return HttpResponse.json(
          createBody ?? {
            type: 'about:blank',
            title: 'Validation failed',
            detail: 'Invalid request',
          },
          { status: createStatus, headers: { 'Content-Type': 'application/problem+json' } },
        )
      }
      return HttpResponse.json(
        createBody ?? {
          batch_name: 'launch',
          credits: 100,
          count: body.count,
          expires_at: null,
          codes: ['ABCDEFGH12345678ABCDEFGH12', 'ABCDEFGH12345678ABCDEFGH34'],
        },
        { status: 201 },
      )
    }),
    http.patch(`${ORIGIN}/api/redemption-codes/:id`, async ({ request, params }) => {
      const custom = onPatch?.(String(params.id), await request.text())
      if (custom !== undefined) return custom
      return HttpResponse.json({ id: Number(params.id), status: 'disabled' })
    }),
    http.delete(`${ORIGIN}/api/redemption-codes/:id`, ({ params }) => {
      onDelete?.(Number(params.id))
      return new HttpResponse(null, { status: 204 })
    }),
    http.post(`${ORIGIN}/api/redemption-codes/batch-delete`, async ({ request }) => {
      onBatchDelete?.(await request.text())
      return HttpResponse.json({ succeeded: 1, failed: [] })
    }),
  )
}

const DEFAULT_SEARCH: RedemptionSearch = { p: 1 }

describe('RedemptionCodesPage', () => {
  it('renders the list with the fixed code mask and never the plaintext', async () => {
    await renderWithProviders(
      <RedemptionCodesPage search={DEFAULT_SEARCH} onSearchChange={() => {}} />,
      ['common', 'admin'],
    )

    expect(await screen.findByText('launch')).toBeTruthy()
    expect(screen.getByText('••••••')).toBeTruthy()
    expect(screen.getByText('10,000')).toBeTruthy()
    expect(screen.getAllByText('Unused').length).toBeGreaterThan(0)
  })

  it('sends filters through the URL search callback and resets the page', async () => {
    const onSearchChange = vi.fn()
    await renderWithProviders(
      <RedemptionCodesPage search={{ p: 2 }} onSearchChange={onSearchChange} />,
      ['common', 'admin'],
    )

    const user = userEvent.setup()
    await screen.findByText('launch')

    await user.selectOptions(screen.getByLabelText('Status'), 'disabled')
    await user.type(screen.getByLabelText('Batch Name'), ' autumn ')
    await user.type(screen.getByLabelText('Min Credits'), '50')
    await user.click(screen.getByRole('button', { name: 'Search' }))

    expect(onSearchChange).toHaveBeenCalledWith({
      p: 1,
      status: 'disabled',
      batch: 'autumn',
      min: 50,
    })
  })

  it('reads the page size from the URL and resets to page 1 when it changes', async () => {
    const requests: URLSearchParams[] = []
    installHandlers({ total: 100, record: (params) => requests.push(params) })
    const onSearchChange = vi.fn()
    await renderWithProviders(
      <RedemptionCodesPage search={{ p: 2, size: 50 }} onSearchChange={onSearchChange} />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()
    await screen.findByText('launch')

    // 刷新/分享链接后仍用同一分页形状：size 来自 URL，而不是组件内 state。
    await waitFor(() => {
      expect(requests[0]?.get('page_size')).toBe('50')
    })
    expect(requests[0]?.get('p')).toBe('2')

    const select = screen.getByLabelText('Rows per page') as HTMLSelectElement
    expect(select.value).toBe('50')
    await user.selectOptions(select, '10')
    expect(onSearchChange).toHaveBeenCalledWith({ p: 1, size: 10 }, { replace: true })
  })

  it('shows the desktop-only gate below 1024px without requesting the list', async () => {
    const record = vi.fn()
    installHandlers({ record })
    vi.stubGlobal(
      'matchMedia',
      vi.fn().mockImplementation((query: string) => ({
        matches: false,
        media: query,
        onchange: null,
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        addListener: vi.fn(),
        removeListener: vi.fn(),
        dispatchEvent: vi.fn(),
      })),
    )

    await renderWithProviders(
      <RedemptionCodesPage search={DEFAULT_SEARCH} onSearchChange={() => {}} />,
      ['common', 'admin'],
    )

    expect(await screen.findByText('Please use a desktop browser.')).toBeTruthy()
    expect(record).not.toHaveBeenCalled()
  })

  it('returns to the first page after a successful generation', async () => {
    const onSearchChange = vi.fn()
    installHandlers({ total: 100 })
    await renderWithProviders(
      <RedemptionCodesPage search={{ p: 3 }} onSearchChange={onSearchChange} />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()
    await screen.findByText('launch')

    await user.click(screen.getAllByRole('button', { name: 'Generate Codes' })[0]!)
    const sheet = await screen.findByRole('dialog', { name: 'Generate Codes' })
    await user.type(within(sheet).getByLabelText('Credits per Code'), '100')
    await user.click(within(sheet).getByRole('button', { name: 'Generate' }))

    await screen.findByRole('dialog', { name: /Successfully created/ })
    expect(onSearchChange).toHaveBeenCalledWith({ p: 1 }, { replace: true })
  })

  it('refetches the list when a row action reports 404 (state changed elsewhere)', async () => {
    let listCalls = 0
    installHandlers({
      record: () => {
        listCalls += 1
      },
      onPatch: () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'Not Found' },
          { status: 404, headers: { 'Content-Type': 'application/problem+json' } },
        ),
    })

    await renderWithProviders(
      <RedemptionCodesPage search={DEFAULT_SEARCH} onSearchChange={() => {}} />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()
    await screen.findByText('launch')
    expect(listCalls).toBe(1)

    await user.click(screen.getByRole('button', { name: 'Disable' }))
    await waitFor(() => {
      expect(listCalls).toBeGreaterThan(1)
    })
  })

  it('prefills the localized default batch name', async () => {
    await renderWithProviders(
      <RedemptionCodesPage search={DEFAULT_SEARCH} onSearchChange={() => {}} />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()
    await screen.findByText('launch')

    await user.click(screen.getAllByRole('button', { name: 'Generate Codes' })[0]!)
    const sheet = await screen.findByRole('dialog', { name: 'Generate Codes' })
    const input = within(sheet).getByLabelText('Batch Name') as HTMLInputElement

    expect(input.value).toMatch(/^Batch-\d{8}$/)
  })

  it('shows the empty state and offers code generation', async () => {
    installHandlers({ items: [], total: 0 })
    await renderWithProviders(
      <RedemptionCodesPage search={DEFAULT_SEARCH} onSearchChange={() => {}} />,
      ['common', 'admin'],
    )

    expect(await screen.findByText('No redemption codes yet.')).toBeTruthy()
    expect(
      screen.getAllByRole('button', { name: 'Generate Codes' }).length,
    ).toBeGreaterThan(0)
  })

  it('creates codes, shows the one-time plaintext result and copies it', async () => {
    installHandlers()
    await renderWithProviders(
      <RedemptionCodesPage search={DEFAULT_SEARCH} onSearchChange={() => {}} />,
      ['common', 'admin'],
    )
    // userEvent.setup() 会安装自己的 clipboard 桩，因此必须在它之后再
    // 覆盖 navigator.clipboard，断言才能观察到组件的 writeText 调用。
    const user = userEvent.setup()
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    })
    await screen.findByText('launch')

    await user.click(screen.getAllByRole('button', { name: 'Generate Codes' })[0]!)
    const sheet = await screen.findByRole('dialog', { name: 'Generate Codes' })
    await user.type(within(sheet).getByLabelText('Credits per Code'), '100')
    await user.click(within(sheet).getByRole('button', { name: 'Generate' }))

    const result = await screen.findByRole('dialog', {
      name: 'Successfully created 10 redemption codes',
    })
    expect(within(result).getByText('ABCDEFGH12345678ABCDEFGH12')).toBeTruthy()
    expect(
      within(result).getAllByText('Codes are shown only once. Save them now.').length,
    ).toBeGreaterThan(0)

    await user.click(within(result).getByRole('button', { name: 'Copy All' }))
    expect(writeText).toHaveBeenCalledWith(
      'ABCDEFGH12345678ABCDEFGH12\nABCDEFGH12345678ABCDEFGH34',
    )

    await user.click(within(result).getByRole('button', { name: 'Done' }))
    await waitFor(() => {
      expect(screen.queryByText('ABCDEFGH12345678ABCDEFGH12')).toBeNull()
    })
  })

  it('maps a contract 400 detail back to localized copy without leaking the internal text', async () => {
    // 创建接口在契约里只有 400（没有 `fields` 扩展），detail 是内部英文。
    installHandlers({
      createStatus: 400,
      createBody: {
        type: 'about:blank',
        title: 'Invalid request',
        detail: 'billing: invalid redemption batch: credits must be > 0, got 0',
      },
    })
    await renderWithProviders(
      <RedemptionCodesPage search={DEFAULT_SEARCH} onSearchChange={() => {}} />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()
    await screen.findByText('launch')

    await user.click(screen.getAllByRole('button', { name: 'Generate Codes' })[0]!)
    const sheet = await screen.findByRole('dialog', { name: 'Generate Codes' })
    await user.type(within(sheet).getByLabelText('Credits per Code'), '100')
    await user.click(within(sheet).getByRole('button', { name: 'Generate' }))

    expect(
      await within(sheet).findByText('Credits must be a positive integer.'),
    ).toBeTruthy()
    expect(within(sheet).queryByText(/billing: invalid redemption batch/)).toBeNull()
    expect(screen.queryByRole('dialog', { name: /Successfully created/ })).toBeNull()
  })

  it('falls back to the generic message for an unrecognized validation detail', async () => {
    installHandlers({
      createStatus: 400,
      createBody: {
        type: 'about:blank',
        title: 'Invalid request',
        detail: 'request body must be a JSON object',
      },
    })
    await renderWithProviders(
      <RedemptionCodesPage search={DEFAULT_SEARCH} onSearchChange={() => {}} />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()
    await screen.findByText('launch')

    await user.click(screen.getAllByRole('button', { name: 'Generate Codes' })[0]!)
    const sheet = await screen.findByRole('dialog', { name: 'Generate Codes' })
    await user.type(within(sheet).getByLabelText('Credits per Code'), '100')
    await user.click(within(sheet).getByRole('button', { name: 'Generate' }))

    expect(
      await within(sheet).findByText('Could not generate codes. Please try again.'),
    ).toBeTruthy()
    expect(within(sheet).queryByText(/request body must be/)).toBeNull()
  })

  it('asks for confirmation before deleting and reports a 409 without retrying', async () => {
    const deleted: number[] = []
    const patches: string[] = []
    let listCalls = 0
    installHandlers({
      record: () => {
        listCalls += 1
      },
      onDelete: (id) => deleted.push(id),
      onPatch: (id, body) => {
        patches.push(`${id}:${body}`)
        return HttpResponse.json(
          { type: 'about:blank', title: 'Conflict', detail: 'already used' },
          { status: 409, headers: { 'Content-Type': 'application/problem+json' } },
        )
      },
    })

    await renderWithProviders(
      <RedemptionCodesPage search={DEFAULT_SEARCH} onSearchChange={() => {}} />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()
    await screen.findByText('launch')

    await user.click(screen.getByRole('button', { name: 'Delete' }))
    const confirm = await screen.findByRole('alertdialog', {
      name: 'Delete this redemption code?',
    })
    // 删除必须经过一次显式确认，未确认前不得发出请求。
    expect(deleted).toEqual([])
    await user.click(within(confirm).getByRole('button', { name: 'Delete' }))
    await waitFor(() => {
      expect(deleted).toEqual([12])
    })

    await user.click(screen.getByRole('button', { name: 'Disable' }))
    await waitFor(() => {
      expect(patches).toEqual(['12:{"status":"disabled"}'])
    })
    const messages = useToastStore.getState().toasts.map((toast) => toast.message)
    expect(messages).toContain('Redeemed codes cannot be modified.')
    // 409 是确定性拒绝：一次请求即为最终结果，不做自动重试。
    expect(patches).toHaveLength(1)
    // 但行状态已被别处改变，必须重查列表，否则界面继续展示过期状态。
    await waitFor(() => {
      expect(listCalls).toBeGreaterThan(1)
    })
  })

  it('batch-deletes the selected rows after confirmation', async () => {
    let payload = ''
    installHandlers({
      items: [codeRow({ id: 12 }), codeRow({ id: 13 })],
      total: 2,
      onBatchDelete: (body) => {
        payload = body
      },
    })

    await renderWithProviders(
      <RedemptionCodesPage search={DEFAULT_SEARCH} onSearchChange={() => {}} />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()
    expect((await screen.findAllByText('launch')).length).toBe(2)

    await user.click(screen.getByRole('checkbox', { name: 'Select redemption code 13' }))
    await user.click(screen.getByRole('button', { name: 'Delete selected (1)' }))
    const confirm = await screen.findByRole('alertdialog', {
      name: 'Delete 1 redemption codes?',
    })
    await user.click(within(confirm).getByRole('button', { name: 'Delete' }))

    await waitFor(() => {
      expect(JSON.parse(payload)).toEqual({ ids: [13] })
    })
  })

  it('disables row mutations for redeemed codes', async () => {
    installHandlers({
      items: [codeRow({ status: 'used', used_by: 7, used_at: '2026-10-02T10:00:00Z' })],
      total: 1,
    })

    await renderWithProviders(
      <RedemptionCodesPage search={DEFAULT_SEARCH} onSearchChange={() => {}} />,
      ['common', 'admin'],
    )

    expect((await screen.findAllByText('Used')).length).toBeGreaterThan(0)
    await screen.findByText('••••••')
    expect((screen.getByRole('button', { name: 'Enable' }) as HTMLButtonElement).disabled).toBe(true)
    expect((screen.getByRole('button', { name: 'Delete' }) as HTMLButtonElement).disabled).toBe(true)
  })

  it('exports metadata only and trims used_by without users:read (R-49)', async () => {
    const user = userEvent.setup()
    const blobs: Blob[] = []
    // jsdom 没有实现 Object URL：直接补两个方法（不要整体替换 URL
    // 构造器，否则 openapi-fetch 的 `new URL()` 会失效）。
    Object.defineProperty(URL, 'createObjectURL', {
      configurable: true,
      writable: true,
      value: vi.fn((blob: Blob) => {
        blobs.push(blob)
        return 'blob:redemption'
      }),
    })
    Object.defineProperty(URL, 'revokeObjectURL', {
      configurable: true,
      writable: true,
      value: vi.fn(),
    })
    installHandlers({
      items: [
        codeRow({ id: 12, batch_name: 'launch', credits: 100, status: 'used', used_by: 7, used_at: '2026-10-02T10:00:00Z' }),
      ],
      total: 1,
      // 只有兑换码管理权限：导出文件不得包含 used_by。
      permissions: ['redemption:manage'],
    })

    await renderWithProviders(
      <RedemptionCodesPage search={DEFAULT_SEARCH} onSearchChange={() => {}} />,
      ['common', 'admin'],
    )

    const exportButton = (await screen.findByRole('button', {
      name: 'Export metadata',
    })) as HTMLButtonElement
    expect(exportButton.disabled).toBe(true)

    await screen.findByText('••••••')
    await user.click(screen.getByRole('checkbox', { name: 'Select redemption code 12' }))
    expect(exportButton.disabled).toBe(false)
    await user.click(exportButton)

    const dialog = await screen.findByRole('dialog', { name: 'Export metadata (1 rows)' })
    expect(
      within(dialog).getByText(/lacks the users:read permission/),
    ).toBeTruthy()
    await user.click(within(dialog).getByRole('button', { name: 'Download' }))

    await waitFor(() => expect(blobs).toHaveLength(1))
    const text = await new Promise<string>((resolve, reject) => {
      const reader = new FileReader()
      reader.onload = () => resolve(String(reader.result))
      reader.onerror = () => reject(reader.error)
      reader.readAsText(blobs[0]!)
    })
    expect(text).toContain('ID\tBatch\tCredits\tStatus\tCreated at\tUsed at')
    expect(text).not.toContain('Used by')
    expect(text).not.toContain('\t7')
    expect(useToastStore.getState().toasts.at(-1)?.message).toContain(
      'redemption-codes-metadata-',
    )
  })
})
