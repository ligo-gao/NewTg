import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import type { components } from '@/api/schema'
import { renderWithProviders } from '@/test-support/providers'
import { useToastStore } from '@/shared/ui/toast'

import { ModelPricingListPage } from './ModelPricingListPage'

type ModelPricing = components['schemas']['ModelPricing']

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())
beforeEach(() => {
  useToastStore.setState({ toasts: [] })
})

function pricingRow(overrides: Partial<ModelPricing> = {}): ModelPricing {
  return {
    id: 1,
    model: 'gpt-5',
    mode: 'per_token',
    billing_expr: null,
    prompt_price: 500,
    completion_price: 1500,
    cache_read_price: 50,
    cache_write_price: 150,
    author: 'Unknown',
    vendor: null,
    status: 1,
    free: false,
    created_at: '2026-10-01T10:00:00Z',
    updated_at: '2026-10-01T10:00:00Z',
    ...overrides,
  }
}

interface InstallOptions {
  permissions?: string[] | null
  items?: ModelPricing[]
  total?: number
  record?: (params: URLSearchParams) => void
  recordActor?: () => void
}

function installBaseHandlers({
  permissions = null,
  items = [pricingRow()],
  total = 1,
  record,
  recordActor,
}: InstallOptions = {}) {
  server.use(
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({ username: 'root', role: 'admin', status: 'enabled' }),
    ),
    http.get(`${ORIGIN}/api/users`, () => {
      recordActor?.()
      return HttpResponse.json({
        items: [
          {
            id: 1,
            username: 'root',
            display_name: null,
            email: null,
            role: 'admin',
            status: 'enabled',
            balance: 0,
            group: 'default',
            admin_permissions: permissions,
            totp_enabled: false,
            must_change_password: false,
            token_count: 0,
            created_at: '2026-10-01T10:00:00Z',
            updated_at: '2026-10-01T10:00:00Z',
          },
        ],
        total: 1,
        page: 1,
        page_size: 20,
      })
    }),
    http.get(`${ORIGIN}/api/model-pricing`, ({ request }) => {
      const params = new URL(request.url).searchParams
      record?.(params)
      const page = Number(params.get('p') ?? '1')
      return HttpResponse.json({
        items: items.map((item) => ({
          ...item,
          model: page === 1 ? item.model : `${item.model}-page-${page}`,
        })),
        total,
        page,
        page_size: Number(params.get('page_size') ?? '20'),
        quota_per_unit: 100,
      })
    }),
  )
}

describe('ModelPricingListPage', () => {
  it('requests the next offset page through the pagination control', async () => {
    let lastParams: URLSearchParams | undefined
    installBaseHandlers({
      total: 45,
      record: (params) => {
        lastParams = params
      },
    })

    const { findByText, getByRole } = await renderWithProviders(
      <ModelPricingListPage />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    expect(await findByText('gpt-5')).toBeTruthy()
    await user.click(getByRole('button', { name: 'Next' }))

    expect(await findByText('gpt-5-page-2')).toBeTruthy()
    await waitFor(() => {
      expect(lastParams?.get('p')).toBe('2')
    })
  })

  it('applies filters and resets to the first page', async () => {
    const recorded: URLSearchParams[] = []
    installBaseHandlers({
      total: 45,
      record: (params) => recorded.push(new URLSearchParams(params)),
    })

    const { findByText, getByLabelText, getByRole } = await renderWithProviders(
      <ModelPricingListPage />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    expect(await findByText('gpt-5')).toBeTruthy()
    await user.click(getByRole('button', { name: 'Next' }))
    await waitFor(() => expect(recorded.at(-1)?.get('p')).toBe('2'))

    await user.selectOptions(getByLabelText('Billing mode'), 'per_request')
    await user.type(getByLabelText('Model prefix'), 'gpt')
    await user.selectOptions(getByLabelText('Status'), 'disabled')
    await user.click(getByRole('button', { name: 'Search' }))

    await waitFor(() => {
      const applied = recorded.at(-1)
      expect(applied?.get('mode')).toBe('per_request')
      expect(applied?.get('status')).toBe('disabled')
      expect(applied?.get('keyword')).toBe('gpt')
      expect(applied?.get('p')).toBe('1')
    })
  })

  it('hides every write action when the actor only holds pricing:read', async () => {
    let actorLoaded = false
    installBaseHandlers({
      permissions: ['pricing:read'],
      recordActor: () => {
        actorLoaded = true
      },
    })

    const { findByText, getByRole, queryByRole } = await renderWithProviders(
      <ModelPricingListPage />,
      ['common', 'admin'],
    )

    expect(await findByText('gpt-5')).toBeTruthy()
    await waitFor(() => {
      expect(actorLoaded).toBe(true)
    })
    expect(queryByRole('button', { name: 'New pricing' })).toBeNull()
    expect(queryByRole('button', { name: 'Edit' })).toBeNull()
    expect(queryByRole('button', { name: 'Disable' })).toBeNull()
    expect(queryByRole('button', { name: 'Delete' })).toBeNull()
    // 只读视图仍然渲染。
    expect(getByRole('table')).toBeTruthy()
  })

  it('confirms a disable and surfaces an optimistic-lock 409', async () => {
    let listCalls = 0
    installBaseHandlers({
      permissions: null,
      record: () => {
        listCalls += 1
      },
    })
    const bodies: Record<string, unknown>[] = []

    server.use(
      http.patch(`${ORIGIN}/api/model-pricing/gpt-5`, async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>)
        return HttpResponse.json(
          {
            type: 'about:blank',
            title: 'Conflict',
            detail: 'updated_at mismatch',
          },
          { status: 409 },
        )
      }),
    )

    const { findByRole, findByText } = await renderWithProviders(
      <ModelPricingListPage />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    expect(await findByText('gpt-5')).toBeTruthy()
    // 行操作打开确认框；下面把确认按钮限定在 dialog 内，避免与行上的
    // 两个 "Disable" 标签混淆。
    await user.click(await findByRole('button', { name: 'Disable' }))
    const dialog = await within(document.body).findByRole('dialog')
    const confirm = within(dialog).getByRole('button', { name: 'Disable' })
    expect(dialog.textContent).toContain(
      'Disabling prevents users from calling this model. Continue?',
    )
    await user.click(confirm)

    await waitFor(() => {
      expect(bodies[0]).toEqual({
        updated_at: '2026-10-01T10:00:00Z',
        status: 2,
      })
    })
    await waitFor(() => {
      const messages = useToastStore.getState().toasts.map((toast) => toast.message)
      expect(messages).toContain(
        'This record was changed by someone else. The list has been refreshed.',
      )
    })
    // 列表已失效并重新拉取，而确认框保持打开（dialog 会把背景设为
    // inert，因此断言请求次数而不是查 DOM 表格）。
    await waitFor(() => {
      expect(listCalls).toBeGreaterThanOrEqual(2)
    })
  })

  it('escapes the DELETE updated_at query parameter', async () => {
    installBaseHandlers({
      permissions: null,
      items: [pricingRow({ updated_at: '2026-10-01T10:00:00+08:00' })],
    })
    let deleteUrl = ''

    server.use(
      http.delete(`${ORIGIN}/api/model-pricing/gpt-5`, ({ request }) => {
        deleteUrl = request.url
        return new HttpResponse(null, { status: 204 })
      }),
    )

    const { findByRole, findByText, getByRole } = await renderWithProviders(
      <ModelPricingListPage />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    expect(await findByText('gpt-5')).toBeTruthy()
    await user.click(await findByRole('button', { name: 'Delete' }))
    const dialog = await within(document.body).findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Delete' }))

    await waitFor(() => {
      expect(deleteUrl).not.toBe('')
    })
    const url = new URL(deleteUrl)
    // 字面 + 会以空格到达服务端；序列化器必须输出 %2B。
    expect(deleteUrl).toContain('%2B08%3A00')
    expect(url.searchParams.get('updated_at')).toBe('2026-10-01T10:00:00+08:00')
    expect(getByRole('table')).toBeTruthy()
  })

  it('shows a free tag only for the server-marked free row and renders single zeros', async () => {
    installBaseHandlers({
      items: [
        pricingRow({ model: 'embed-3', prompt_price: 0, completion_price: 0, cache_read_price: 0, cache_write_price: 0, free: true }),
        pricingRow({ id: 2, model: 'partial-zero', prompt_price: 0, completion_price: 0, cache_read_price: 0, cache_write_price: 0, free: false }),
      ],
      total: 2,
    })

    const { findByText, getAllByTestId } = await renderWithProviders(
      <ModelPricingListPage />,
      ['common', 'admin'],
    )

    expect(await findByText('embed-3')).toBeTruthy()
    // 只有服务端标记的行挂 Free Tag；第二行仍然渲染字面 0 单元格。
    expect(getAllByTestId('tag').filter((tag) => tag.textContent === 'Free')).toHaveLength(1)
    expect((await findByText('partial-zero')).closest('tr')?.textContent).toContain('0')
  })
})
