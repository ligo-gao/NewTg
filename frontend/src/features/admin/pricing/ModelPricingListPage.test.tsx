import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { useQuery } from '@tanstack/react-query'
import userEvent from '@testing-library/user-event'
import { waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

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
  usersStatus?: number
  /** 模拟调用者自己的行不在返回页（F-A）。 */
  noActorRow?: boolean
  record?: (params: URLSearchParams) => void
  recordActor?: () => void
}

function installBaseHandlers({
  permissions = null,
  items = [pricingRow()],
  total = 1,
  usersStatus,
  noActorRow = false,
  record,
  recordActor,
}: InstallOptions = {}) {
  server.use(
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({ username: 'root', role: 'admin', status: 'enabled' }),
    ),
    http.get(`${ORIGIN}/api/users`, () => {
      recordActor?.()
      if (usersStatus !== undefined) {
        return HttpResponse.json(
          { type: 'about:blank', title: 'Forbidden' },
          { status: usersStatus },
        )
      }
      return HttpResponse.json({
        items: noActorRow
          ? [
              {
                id: 2,
                username: 'another-admin',
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
            ]
          : [
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
        page_size: 100,
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

  it('refreshes and reports a gone record when a delete races another delete', async () => {
    let listCalls = 0
    installBaseHandlers({
      permissions: null,
      record: () => {
        listCalls += 1
      },
    })
    server.use(
      http.delete(`${ORIGIN}/api/model-pricing/gpt-5`, () =>
        HttpResponse.json(
          { type: 'about:blank', title: 'Not Found' },
          { status: 404 },
        ),
      ),
    )

    const { findByRole, findByText } = await renderWithProviders(
      <ModelPricingListPage />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    expect(await findByText('gpt-5')).toBeTruthy()
    await user.click(await findByRole('button', { name: 'Delete' }))
    const dialog = await findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Delete' }))

    await waitFor(() => {
      const messages = useToastStore.getState().toasts.map((toast) => toast.message)
      expect(messages).toContain(
        'This record no longer exists. The list has been refreshed.',
      )
    })
    // 404 与 409 一样失效列表，避免幽灵行与重复 404。
    await waitFor(() => {
      expect(listCalls).toBeGreaterThanOrEqual(2)
    })
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
    const freeTags = getAllByTestId('tag').filter((tag) => tag.textContent === 'Free')
    expect(freeTags).toHaveLength(1)
    expect(freeTags[0]?.getAttribute('aria-label')).toBe('Free')
    expect((await findByText('partial-zero')).closest('tr')?.textContent).toContain('0')
  })

  it('skips the list request entirely when pricing:read is absent', async () => {
    let actorLoaded = false
    let listCalls = 0
    installBaseHandlers({
      permissions: ['users:read'],
      record: () => {
        listCalls += 1
      },
      recordActor: () => {
        actorLoaded = true
      },
    })

    const { findByText, queryByRole } = await renderWithProviders(
      <ModelPricingListPage />,
      ['common', 'admin'],
    )

    expect(await findByText('Access denied')).toBeTruthy()
    await waitFor(() => {
      expect(actorLoaded).toBe(true)
    })
    // F8：能力已知且无读权限时，不在挂载阶段发出注定 403 的列表请求。
    expect(listCalls).toBe(0)
    expect(queryByRole('table')).toBeNull()
  })

  it('still loads read-only rows when the actor row cannot be read', async () => {
    installBaseHandlers({ usersStatus: 403 })

    const { findByText, queryByRole } = await renderWithProviders(
      <ModelPricingListPage />,
      ['common', 'admin'],
    )

    // 能力无法判定：允许服务端裁决读权限（列表成功），写操作隐藏。
    expect(await findByText('gpt-5')).toBeTruthy()
    expect(queryByRole('button', { name: 'New pricing' })).toBeNull()
  })

  it('does not treat a missing actor row as an empty permission set (F-A)', async () => {
    installBaseHandlers({ noActorRow: true, permissions: null })

    const { findByText, queryByRole } = await renderWithProviders(
      <ModelPricingListPage />,
      ['common', 'admin'],
    )

    // 自己的行不在第一页：能力未知，允许读请求通过服务端裁决；写操作隐藏。
    expect(await findByText('gpt-5')).toBeTruthy()
    expect(queryByRole('button', { name: 'New pricing' })).toBeNull()
  })

  it('invalidates the model selector and group-override caches on delete (F5)', async () => {
    installBaseHandlers({ permissions: null })
    let modelProbeCalls = 0
    let overrideProbeCalls = 0

    function InvalidationProbe() {
      useQuery({
        queryKey: ['admin-pricing-models'],
        queryFn: () => {
          modelProbeCalls += 1
          return null
        },
      })
      useQuery({
        queryKey: ['admin-group-pricing'],
        queryFn: () => {
          overrideProbeCalls += 1
          return null
        },
      })
      return null
    }

    server.use(
      http.delete(`${ORIGIN}/api/model-pricing/gpt-5`, () =>
        new HttpResponse(null, { status: 204 }),
      ),
    )

    const { findByRole, findByText } = await renderWithProviders(
      <>
        <ModelPricingListPage />
        <InvalidationProbe />
      </>,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    expect(await findByText('gpt-5')).toBeTruthy()
    await waitFor(() => {
      expect(modelProbeCalls).toBe(1)
      expect(overrideProbeCalls).toBe(1)
    })

    await user.click(await findByRole('button', { name: 'Delete' }))
    const dialog = await findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Delete' }))

    await waitFor(() => {
      expect(modelProbeCalls).toBeGreaterThanOrEqual(2)
      expect(overrideProbeCalls).toBeGreaterThanOrEqual(2)
    })
  })

  it('filters rows to the plugin billing-expression models from the F-08-37 deep link', async () => {
    installBaseHandlers({
      items: [
        pricingRow({ id: 1, model: 'sora-2' }),
        pricingRow({ id: 2, model: 'gpt-5' }),
        pricingRow({ id: 3, model: 'kling-other' }),
      ],
      total: 3,
    })
    // 插件计费变体是设置面数据（T5）；前端只读取，不新增端点（R-50）。
    server.use(
      http.get(`${ORIGIN}/api/settings`, () =>
        HttpResponse.json({
          section: 'billing_setting',
          updated_at: null,
          items: [
            {
              key: 'billing_setting.plugin_billing_expr',
              value: {
                'kling::sora-2': 'u("seconds")',
                'kling::kling-other': 'u("seconds")',
                'suno::other': 'u("duration")',
              },
              type: 'object',
              default: {},
              public: false,
              secret: false,
              dangerous: false,
            },
          ],
        }),
      ),
    )
    const onClearPlugin = vi.fn()

    const { findByText, queryByText, getByText, getByRole } = await renderWithProviders(
      <ModelPricingListPage plugin="kling" onClearPlugin={onClearPlugin} />,
      ['common', 'admin'],
    )

    expect(await findByText('sora-2')).toBeTruthy()
    expect(getByText('kling-other')).toBeTruthy()
    // 非该插件的模型不出现。
    expect(queryByText('gpt-5')).toBeNull()
    expect(getByText('Filtered by plugin: kling')).toBeTruthy()

    await userEvent.setup().click(getByRole('button', { name: 'Clear plugin filter' }))
    expect(onClearPlugin).toHaveBeenCalledTimes(1)
  })

  it('distinguishes "plugin has no expressions" from a plain empty catalog', async () => {
    installBaseHandlers({ items: [pricingRow()], total: 1 })
    server.use(
      http.get(`${ORIGIN}/api/settings`, () =>
        HttpResponse.json({
          section: 'billing_setting',
          updated_at: null,
          items: [
            {
              key: 'billing_setting.plugin_billing_expr',
              value: { 'suno::other': 'u("duration")' },
              type: 'object',
              default: {},
              public: false,
              secret: false,
              dangerous: false,
            },
          ],
        }),
      ),
    )
    const onClearPlugin = vi.fn()

    const { findByText, getByText, queryByText, getByRole } = await renderWithProviders(
      <ModelPricingListPage plugin="kling" onClearPlugin={onClearPlugin} />,
      ['common', 'admin'],
    )

    expect(
      await findByText('This plugin has no billing expressions configured.'),
    ).toBeTruthy()
    // 插件过滤命中 0 行时不能提示「还没有定价，去新增」——数据其实存在，
    // 只是不属于这个插件。
    expect(queryByText('Models without pricing cannot be requested by users.')).toBeNull()
    await userEvent.setup().click(getByRole('button', { name: 'Clear filters' }))
    expect(onClearPlugin).toHaveBeenCalledTimes(1)
    expect(getByText('Filtered by plugin: kling')).toBeTruthy()
  })

  // R-55：GET /api/settings 要求 settings:manage（R-26），但 /admin 只判
  // role === 'admin'。没有该能力的 admin 仍可能带 ?plugin= 深链进来。
  // 403 表示「插件过滤不可用」而非「设置读取失败」：不能把整页拖进错误态，
  // 也不能谎称插件没有配置——隐藏筛选入口、退回普通列表、不提示错误。
  it('treats a 403 settings read as an unavailable plugin filter, not an error', async () => {
    installBaseHandlers({
      items: [pricingRow({ id: 1, model: 'sora-2' }), pricingRow({ id: 2, model: 'gpt-5' })],
      total: 2,
    })
    server.use(
      http.get(`${ORIGIN}/api/settings`, () =>
        HttpResponse.json({ type: 'about:blank', title: 'Forbidden' }, { status: 403 }),
      ),
    )
    const onClearPlugin = vi.fn()

    const {
      findByText,
      getByText,
      getByLabelText,
      queryByText,
      queryByRole,
    } = await renderWithProviders(
      <ModelPricingListPage plugin="kling" onClearPlugin={onClearPlugin} />,
      ['common', 'admin'],
    )

    // 未过滤的完整列表照常渲染（gpt-5 不属于 kling 也要出现）。
    expect(await findByText('gpt-5')).toBeTruthy()
    expect(getByText('sora-2')).toBeTruthy()
    // 筛选入口（深链横幅与清除按钮）整体隐藏。
    expect(queryByText('Filtered by plugin: kling')).toBeNull()
    expect(queryByRole('button', { name: 'Clear plugin filter' })).toBeNull()
    // 不显示「无法读取插件计费配置」的错误空态。
    expect(queryByText('Could not read the plugin billing configuration.')).toBeNull()
    // 页面其余筛选功能不受影响。
    expect(getByLabelText('Model prefix')).toBeTruthy()
  })
})
