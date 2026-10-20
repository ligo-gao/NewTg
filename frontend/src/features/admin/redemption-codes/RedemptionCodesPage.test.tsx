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
afterEach(() => server.resetHandlers())
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
}: InstallOptions = {}) {
  server.use(
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
            fields: ['credits'],
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

  it('keeps the sheet open and shows a field error on a 422', async () => {
    installHandlers({ createStatus: 422 })
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
    expect(screen.queryByRole('dialog', { name: /Successfully created/ })).toBeNull()
  })

  it('asks for confirmation before deleting and reports a 409 without retrying', async () => {
    const deleted: number[] = []
    const patches: string[] = []
    installHandlers({
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
})
