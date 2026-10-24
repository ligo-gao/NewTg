import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import {
  afterAll,
  afterEach,
  beforeAll,
  describe,
  expect,
  it,
  vi,
} from 'vitest'

import type { Channel } from '@/features/admin/channels/types'
import { renderWithProviders } from '@/test-support/providers'

import type { UpstreamAccount } from './api'
import { AccountsTab } from './accounts-tab'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  vi.restoreAllMocks()
})
afterAll(() => server.close())

function makeAccount(overrides: Partial<UpstreamAccount> = {}): UpstreamAccount {
  return {
    id: 1,
    name: 'DeepSeek',
    provider: 'deepseek',
    currency: 'CNY',
    timezone: 'Asia/Shanghai',
    console_url: null,
    status: 1,
    note: '',
    created_at: '2026-10-20T08:00:00Z',
    updated_at: '2026-10-20T08:00:00Z',
    ...overrides,
  }
}

function makeChannel(overrides: Partial<Channel> = {}): Channel {
  return {
    id: 7,
    name: 'deepseek-main',
    type: 'deepseek',
    status: 1,
    priority: 1,
    weight: 1,
    supported_models: ['deepseek-chat'],
    group_tags: null,
    rate_limit: null,
    auto_ban: false,
    test_model: null,
    remark: null,
    tag: null,
    fail_count: 0,
    auto_disabled_reason: null,
    last_test_at: null,
    last_test_result: null,
    created_at: '2026-10-20T08:00:00Z',
    updated_at: '2026-10-20T08:00:00Z',
    version: '2026-10-20T08:00:00.000Z',
    upstream_account_id: null,
    ...overrides,
  }
}

function problemResponse(
  status: number,
  problem: { title: string; detail: string },
) {
  return HttpResponse.json(
    { type: 'about:blank', ...problem },
    { status, headers: { 'Content-Type': 'application/problem+json' } },
  )
}

interface ApiProblem {
  title: string
  detail: string
}

interface ApiOptions {
  accounts?: UpstreamAccount[]
  /** 服务端 total 与返回行数不同时用来构造账号截断提示。 */
  accountTotal?: number
  channels?: Channel[]
  /** 服务端 total 与返回行数不同时用来构造截断提示。 */
  channelTotal?: number
  accountPatchProblem?: ApiProblem
  deleteProblem?: ApiProblem
  channelPatchProblem?: ApiProblem
  /** 409 之前服务端已被并发写入者改成的样子；只有重取才能看到。 */
  channelAfterConflict?: Partial<Channel>
  /** 设置后渠道 PATCH 先挂起，直到测试 resolve，用来观察进行中的行状态。 */
  channelPatchGate?: Promise<void>
}

interface ApiRecords {
  accountList: URL[]
  channelList: URL[]
  accountCreate: { body: Record<string, unknown>; key: string | null }[]
  accountPatch: {
    id: number
    body: Record<string, unknown>
    key: string | null
  }[]
  accountDelete: { id: number; key: string | null }[]
  channelPatch: { id: number; body: Record<string, unknown> }[]
}

/**
 * 可变的内存数据源：写操作成功后真的改动数组，读取失效重取时便能看到新值
 * ——创建后新行出现、删除后行消失、归属保存后下拉回到服务端值，都由它
 * 驱动，而不是测试自己断言后又手动改 fixture。
 */
function installApi(options: ApiOptions = {}): ApiRecords {
  const accounts = [...(options.accounts ?? [])]
  const channels = [...(options.channels ?? [])]
  const records: ApiRecords = {
    accountList: [],
    channelList: [],
    accountCreate: [],
    accountPatch: [],
    accountDelete: [],
    channelPatch: [],
  }
  let nextAccountId = accounts.reduce((max, a) => Math.max(max, a.id), 0) + 1

  server.use(
    http.get(`${ORIGIN}/api/upstream-accounts`, ({ request }) => {
      const url = new URL(request.url)
      records.accountList.push(url)
      return HttpResponse.json({
        items: accounts,
        total: options.accountTotal ?? accounts.length,
        page: 1,
        page_size: Number(url.searchParams.get('page_size') ?? 20),
      })
    }),
    http.get(`${ORIGIN}/api/channels`, ({ request }) => {
      records.channelList.push(new URL(request.url))
      return HttpResponse.json({
        items: channels,
        total: options.channelTotal ?? channels.length,
        page: 1,
        page_size: 100,
      })
    }),
    http.post(`${ORIGIN}/api/upstream-accounts`, async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>
      records.accountCreate.push({
        body,
        key: request.headers.get('Idempotency-Key'),
      })
      const created = makeAccount({
        id: nextAccountId,
        name: String(body.name),
        provider: String(body.provider),
        currency: String(body.currency ?? 'CNY'),
        timezone: String(body.timezone ?? 'Asia/Shanghai'),
        console_url:
          typeof body.console_url === 'string' ? body.console_url : null,
        note: typeof body.note === 'string' ? body.note : '',
      })
      nextAccountId += 1
      accounts.push(created)
      return HttpResponse.json(created, { status: 201 })
    }),
    http.patch(`${ORIGIN}/api/upstream-accounts/:id`, async ({ request, params }) => {
      const id = Number(params.id)
      const body = (await request.json()) as Record<string, unknown>
      records.accountPatch.push({
        id,
        body,
        key: request.headers.get('Idempotency-Key'),
      })
      if (options.accountPatchProblem !== undefined) {
        return problemResponse(409, options.accountPatchProblem)
      }
      const index = accounts.findIndex((account) => account.id === id)
      if (index === -1) {
        return problemResponse(404, {
          title: 'Not found',
          detail: 'no such account',
        })
      }
      const current = accounts[index]
      if (current === undefined) throw new Error('missing account')
      const next: UpstreamAccount = {
        ...current,
        ...(typeof body.name === 'string' ? { name: body.name } : {}),
        ...(typeof body.provider === 'string' ? { provider: body.provider } : {}),
        ...(typeof body.currency === 'string' ? { currency: body.currency } : {}),
        ...(typeof body.timezone === 'string' ? { timezone: body.timezone } : {}),
        ...('console_url' in body
          ? { console_url: body.console_url as string | null }
          : {}),
        ...(typeof body.note === 'string' ? { note: body.note } : {}),
        ...(body.status === 1 || body.status === 2
          ? { status: body.status as 1 | 2 }
          : {}),
        updated_at: '2026-10-21T00:00:00Z',
      }
      accounts[index] = next
      return HttpResponse.json(next)
    }),
    http.delete(`${ORIGIN}/api/upstream-accounts/:id`, ({ request, params }) => {
      const id = Number(params.id)
      records.accountDelete.push({
        id,
        key: request.headers.get('Idempotency-Key'),
      })
      if (options.deleteProblem !== undefined) {
        return problemResponse(409, options.deleteProblem)
      }
      const index = accounts.findIndex((account) => account.id === id)
      if (index !== -1) accounts.splice(index, 1)
      return new HttpResponse(null, { status: 204 })
    }),
    http.patch(`${ORIGIN}/api/channels/:id`, async ({ request, params }) => {
      const id = Number(params.id)
      const body = (await request.json()) as Record<string, unknown>
      records.channelPatch.push({ id, body })
      if (options.channelPatchGate !== undefined) {
        await options.channelPatchGate
      }
      if (options.channelPatchProblem !== undefined) {
        // 模拟并发写入者：冲突响应发出前，服务端已经改成了别的值。
        const conflictedIndex = channels.findIndex((channel) => channel.id === id)
        const conflicted = channels[conflictedIndex]
        if (conflictedIndex !== -1 && conflicted !== undefined) {
          channels[conflictedIndex] = {
            ...conflicted,
            ...options.channelAfterConflict,
          }
        }
        return problemResponse(409, options.channelPatchProblem)
      }
      const index = channels.findIndex((channel) => channel.id === id)
      if (index === -1) {
        return problemResponse(404, {
          title: 'Not found',
          detail: 'no such channel',
        })
      }
      const current = channels[index]
      if (current === undefined) throw new Error('missing channel')
      const next: Channel = {
        ...current,
        upstream_account_id: body.upstream_account_id as number | null,
        version: '2026-10-21T00:00:00.000Z',
      }
      channels[index] = next
      return HttpResponse.json(next)
    }),
  )
  return records
}

async function renderTab(canManage = true) {
  await renderWithProviders(<AccountsTab canManage={canManage} />, [
    'common',
    'admin',
  ])
}

function accountRow(name: string): HTMLElement {
  const row = screen
    .getAllByTestId('account-row')
    .find((element) => within(element).queryByText(name) !== null)
  if (row === undefined) throw new Error(`missing account row ${name}`)
  return row
}

function bindingRow(name: string): HTMLElement {
  const row = screen
    .getAllByTestId('binding-row')
    .find((element) => within(element).queryByText(name) !== null)
  if (row === undefined) throw new Error(`missing binding row ${name}`)
  return row
}

describe('upstream accounts tab', () => {
  it('creates an account with the loaded defaults and lists it', async () => {
    const records = installApi({ accounts: [makeAccount()] })
    await renderTab()
    const user = userEvent.setup()

    await screen.findByTestId('account-row')
    await user.click(screen.getByRole('button', { name: 'New account' }))

    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText('Name'), 'Moonshot')
    await user.type(within(dialog).getByLabelText('Provider'), 'moonshot')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(records.accountCreate).toHaveLength(1))
    expect(records.accountCreate[0]?.body).toEqual({
      name: 'Moonshot',
      provider: 'moonshot',
      currency: 'CNY',
      timezone: 'Asia/Shanghai',
    })
    // 每次用户意图一个幂等键：服务端据此重放而不是建出第二个账号。
    expect(records.accountCreate[0]?.key).toBeTruthy()

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    const accountsCard = screen.getByTestId('accounts-card')
    await waitFor(() =>
      expect(within(accountsCard).getAllByTestId('account-row')).toHaveLength(2),
    )
    expect(within(accountsCard).getByText('Moonshot')).toBeTruthy()

    // 账号列表不按状态过滤：停用账号也要出现在这里与归属下拉中。
    expect(records.accountList.length).toBeGreaterThan(1)
    for (const url of records.accountList) {
      expect(url.searchParams.has('status')).toBe(false)
      // 归属下拉要看到尽可能多的账号，固定请求服务端上限的一页。
      expect(url.searchParams.get('page_size')).toBe('100')
    }
  })

  it('edits an account, sending only the changed fields and the version token', async () => {
    const records = installApi({ accounts: [makeAccount()] })
    await renderTab()
    const user = userEvent.setup()

    const row = await screen.findByTestId('account-row')
    await user.click(within(row).getByRole('button', { name: 'Edit' }))

    const dialog = await screen.findByRole('dialog')
    const name = within(dialog).getByLabelText('Name')
    await user.clear(name)
    await user.type(name, 'DeepSeek CN')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(records.accountPatch).toHaveLength(1))
    expect(records.accountPatch[0]?.id).toBe(1)
    expect(records.accountPatch[0]?.body).toEqual({
      expected_updated_at: '2026-10-20T08:00:00Z',
      name: 'DeepSeek CN',
    })
    expect(records.accountPatch[0]?.key).toBeTruthy()

    await waitFor(() =>
      expect(within(accountRow('DeepSeek CN')).getByText('DeepSeek CN')).toBeTruthy(),
    )
  })

  it('clears the console URL with an explicit null in the PATCH body', async () => {
    const records = installApi({
      accounts: [makeAccount({ console_url: 'https://console.example' })],
    })
    await renderTab()
    const user = userEvent.setup()

    const row = await screen.findByTestId('account-row')
    await user.click(within(row).getByRole('button', { name: 'Edit' }))

    const dialog = await screen.findByRole('dialog')
    const consoleUrl = within(dialog).getByLabelText('Console URL')
    await user.clear(consoleUrl)
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(records.accountPatch).toHaveLength(1))
    // 「缺省 = 不修改、null = 清空」：清空字段必须显式提交 null。
    expect(records.accountPatch[0]?.body).toEqual({
      expected_updated_at: '2026-10-20T08:00:00Z',
      console_url: null,
    })
  })

  it('keeps the edit form open and shows the problem detail on a 409', async () => {
    const records = installApi({
      accounts: [makeAccount()],
      accountPatchProblem: {
        title: 'Conflict',
        detail: 'account was changed by another admin',
      },
    })
    await renderTab()
    const user = userEvent.setup()

    const row = await screen.findByTestId('account-row')
    await user.click(within(row).getByRole('button', { name: 'Edit' }))

    const dialog = await screen.findByRole('dialog')
    const name = within(dialog).getByLabelText('Name')
    await user.clear(name)
    await user.type(name, 'DeepSeek CN')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(records.accountPatch).toHaveLength(1))
    expect(
      await within(dialog).findByText('account was changed by another admin'),
    ).toBeTruthy()
    // 冲突后不关闭抽屉：管理员需要带着当前草稿重试或放弃。
    expect(screen.getByRole('dialog')).toBe(dialog)
  })

  it('toggles an account between active and disabled', async () => {
    const records = installApi({ accounts: [makeAccount()] })
    await renderTab()
    const user = userEvent.setup()

    const row = await screen.findByTestId('account-row')
    await user.click(within(row).getByRole('button', { name: 'Disable' }))

    await waitFor(() => expect(records.accountPatch).toHaveLength(1))
    expect(records.accountPatch[0]?.body).toEqual({
      expected_updated_at: '2026-10-20T08:00:00Z',
      status: 2,
    })

    await waitFor(() =>
      expect(
        within(accountRow('DeepSeek')).getByRole('button', { name: 'Enable' }),
      ).toBeTruthy(),
    )
    await user.click(
      within(accountRow('DeepSeek')).getByRole('button', { name: 'Enable' }),
    )

    await waitFor(() => expect(records.accountPatch).toHaveLength(2))
    // 第二次 PATCH 用重取后的 updated_at，而不是第一次提交时的旧值。
    expect(records.accountPatch[1]?.body).toEqual({
      expected_updated_at: '2026-10-21T00:00:00Z',
      status: 1,
    })
    // 每次意图一个幂等键：两次切换不能复用同一个 key。
    expect(records.accountPatch[0]?.key).toBeTruthy()
    expect(records.accountPatch[1]?.key).toBeTruthy()
    expect(records.accountPatch[0]?.key).not.toBe(records.accountPatch[1]?.key)
  })

  it('refetches the account after a failed status toggle', async () => {
    const records = installApi({
      accounts: [makeAccount()],
      accountPatchProblem: {
        title: 'Conflict',
        detail: 'account was changed by another admin',
      },
    })
    await renderTab()
    const user = userEvent.setup()

    const row = await screen.findByTestId('account-row')
    // 记下点击时的重取次数：失败后必须再取一次。
    const listCallsAtClick = records.accountList.length
    await user.click(within(row).getByRole('button', { name: 'Disable' }))

    expect(
      await screen.findByText('account was changed by another admin'),
    ).toBeTruthy()
    // 409 后重取：行拿到服务端的 updated_at，重试才有意义。
    await waitFor(() =>
      expect(records.accountList.length).toBeGreaterThan(listCallsAtClick),
    )
  })

  it('deletes an account after the confirmation is accepted', async () => {
    const records = installApi({
      accounts: [
        makeAccount(),
        makeAccount({ id: 2, name: 'Moonshot', provider: 'moonshot' }),
      ],
    })
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    await renderTab()
    const user = userEvent.setup()

    await screen.findAllByTestId('account-row')
    await user.click(
      within(accountRow('DeepSeek')).getByRole('button', { name: 'Delete' }),
    )

    await waitFor(() => expect(records.accountDelete).toHaveLength(1))
    expect(records.accountDelete[0]?.id).toBe(1)
    await waitFor(() => expect(screen.getAllByTestId('account-row')).toHaveLength(1))

    await user.click(
      within(accountRow('Moonshot')).getByRole('button', { name: 'Delete' }),
    )

    await waitFor(() => expect(records.accountDelete).toHaveLength(2))
    expect(records.accountDelete[1]?.id).toBe(2)
    await waitFor(() => expect(screen.queryByTestId('account-row')).toBeNull())

    // 每次意图一个幂等键：两次删除的 key 都必须存在且互不相同。
    expect(records.accountDelete[0]?.key).toBeTruthy()
    expect(records.accountDelete[1]?.key).toBeTruthy()
    expect(records.accountDelete[0]?.key).not.toBe(records.accountDelete[1]?.key)
  })

  it('sends nothing when the delete confirmation is declined', async () => {
    const records = installApi({ accounts: [makeAccount()] })
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    await renderTab()
    const user = userEvent.setup()

    const row = await screen.findByTestId('account-row')
    await user.click(within(row).getByRole('button', { name: 'Delete' }))

    expect(confirmSpy).toHaveBeenCalledWith(
      'Delete account DeepSeek? This cannot be undone.',
    )
    expect(records.accountDelete).toHaveLength(0)
    expect(screen.getByTestId('account-row')).toBeTruthy()
  })

  it('renders the server 409 on delete and keeps the row', async () => {
    installApi({
      accounts: [makeAccount()],
      deleteProblem: {
        title: 'Conflict',
        detail: 'account is still bound to a channel',
      },
    })
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    await renderTab()
    const user = userEvent.setup()

    const row = await screen.findByTestId('account-row')
    await user.click(within(row).getByRole('button', { name: 'Delete' }))

    expect(
      await screen.findByText('account is still bound to a channel'),
    ).toBeTruthy()
    expect(screen.getByTestId('account-row')).toBeTruthy()
  })

  it('marks the channel name as the binding table’s row header', async () => {
    installApi({ channels: [makeChannel({ name: 'deepseek-main' })] })
    await renderTab()

    await screen.findByTestId('binding-row')
    const header = screen.getByRole('rowheader', { name: 'deepseek-main' })
    expect(header.tagName).toBe('TH')
    expect(header.getAttribute('scope')).toBe('row')
  })

  it('disables delete for an account the loaded channels reference', async () => {
    installApi({
      accounts: [
        makeAccount(),
        makeAccount({ id: 2, name: 'Moonshot', provider: 'moonshot' }),
      ],
      channels: [makeChannel({ id: 7, upstream_account_id: 1 })],
    })
    await renderTab()

    await screen.findByTestId('binding-row')
    const boundRow = accountRow('DeepSeek')
    const unboundRow = accountRow('Moonshot')

    const boundDelete = within(boundRow).getByRole('button', {
      name: 'Delete',
    })
    expect((boundDelete as HTMLButtonElement).disabled).toBe(true)
    const hint = within(boundRow).getByText(
      'Bound to a channel — unbind it first.',
    )
    // 提示与按钮用 id 关联，读屏才能把「为什么禁用」读出来。
    expect(hint.id).toBe('account-delete-hint-1')
    expect(boundDelete.getAttribute('aria-describedby')).toBe(
      'account-delete-hint-1',
    )

    const unboundDelete = within(unboundRow).getByRole('button', {
      name: 'Delete',
    })
    expect((unboundDelete as HTMLButtonElement).disabled).toBe(false)
    expect(unboundDelete.getAttribute('aria-describedby')).toBeNull()
  })

  it('binds a channel with its version token and the numeric account id', async () => {
    const records = installApi({
      accounts: [
        makeAccount(),
        makeAccount({ id: 2, name: 'Moonshot', provider: 'moonshot' }),
      ],
      channels: [makeChannel({ id: 7, upstream_account_id: null })],
    })
    await renderTab()

    const row = await screen.findByTestId('binding-row')
    const select = within(row).getByLabelText('Upstream account: deepseek-main')
    await screen.findByRole('option', { name: 'Moonshot' })

    fireEvent.change(select, { target: { value: '2' } })

    await waitFor(() => expect(records.channelPatch).toHaveLength(1))
    expect(records.channelPatch[0]).toEqual({
      id: 7,
      body: {
        expected_version: '2026-10-20T08:00:00.000Z',
        upstream_account_id: 2,
      },
    })
    expect(await screen.findByText('Channel attribution saved.')).toBeTruthy()
    await waitFor(() => expect((select as HTMLSelectElement).value).toBe('2'))
  })

  it('sends null when the binding is cleared', async () => {
    const records = installApi({
      accounts: [makeAccount()],
      channels: [makeChannel({ id: 7, upstream_account_id: 1 })],
    })
    await renderTab()

    const row = await screen.findByTestId('binding-row')
    const select = within(row).getByLabelText('Upstream account: deepseek-main')
    await waitFor(() => expect((select as HTMLSelectElement).value).toBe('1'))

    fireEvent.change(select, { target: { value: '' } })

    await waitFor(() => expect(records.channelPatch).toHaveLength(1))
    expect(records.channelPatch[0]?.body).toEqual({
      expected_version: '2026-10-20T08:00:00.000Z',
      upstream_account_id: null,
    })
  })

  it('shows the problem detail on a binding conflict and keeps the server value', async () => {
    installApi({
      accounts: [
        makeAccount(),
        makeAccount({ id: 2, name: 'Moonshot', provider: 'moonshot' }),
      ],
      channels: [makeChannel({ id: 7, upstream_account_id: 1 })],
      channelPatchProblem: {
        title: 'Conflict',
        detail: 'channel was updated by another admin',
      },
      // 并发写入者在冲突响应前把归属改成了账号 2：只有重取才看得到它。
      channelAfterConflict: {
        upstream_account_id: 2,
        version: '2026-10-22T00:00:00.000Z',
      },
    })
    await renderTab()

    const row = await screen.findByTestId('binding-row')
    const select = within(row).getByLabelText(
      'Upstream account: deepseek-main',
    ) as HTMLSelectElement
    await waitFor(() => expect(select.value).toBe('1'))

    fireEvent.change(select, { target: { value: '2' } })

    expect(
      await screen.findByText(
        'Could not save the channel attribution. channel was updated by another admin',
      ),
    ).toBeTruthy()
    // 非乐观：下拉落到服务端重取回来的值（2），而不是提交时的 1。
    await waitFor(() => expect(select.value).toBe('2'))
  })

  it('notes that the channel list is truncated', async () => {
    const records = installApi({
      accounts: [makeAccount()],
      channels: [
        makeChannel({ id: 7, name: 'deepseek-main' }),
        makeChannel({ id: 8, name: 'deepseek-backup' }),
      ],
      channelTotal: 120,
    })
    await renderTab()

    expect(
      await screen.findByText('Showing the first 2 of 120 channels.'),
    ).toBeTruthy()
    // 绑定卡片只读一页 100 行；批量操作属于渠道控制台。
    expect(records.channelList[0]?.searchParams.get('p')).toBe('1')
    expect(records.channelList[0]?.searchParams.get('page_size')).toBe('100')
  })

  it('notes that the account list is truncated in both cards', async () => {
    const records = installApi({
      accounts: [
        makeAccount(),
        makeAccount({ id: 2, name: 'Moonshot', provider: 'moonshot' }),
      ],
      accountTotal: 130,
      channels: [makeChannel({ id: 7 })],
    })
    await renderTab()

    // 账号被截断时两张卡片都要说明：归属下拉里少一个账号，管理员「修正」
    // 归属就会悄悄覆盖真实的账号指向。
    const notes = await screen.findAllByText(
      'Showing the first 2 of 130 accounts.',
    )
    expect(notes).toHaveLength(2)
    expect(
      within(screen.getByTestId('accounts-card')).getByText(
        'Showing the first 2 of 130 accounts.',
      ),
    ).toBeTruthy()
    expect(
      within(screen.getByTestId('binding-card')).getByText(
        'Showing the first 2 of 130 accounts.',
      ),
    ).toBeTruthy()
    expect(records.accountList[0]?.searchParams.get('page_size')).toBe('100')
  })

  it('keeps other rows usable while one binding is pending', async () => {
    let releaseGate: (() => void) | undefined
    const gate = new Promise<void>((resolve) => {
      releaseGate = resolve
    })
    const records = installApi({
      accounts: [makeAccount()],
      channels: [
        makeChannel({ id: 7, name: 'deepseek-main' }),
        makeChannel({ id: 8, name: 'deepseek-backup' }),
      ],
      channelPatchGate: gate,
    })
    await renderTab()

    await screen.findAllByTestId('binding-row')
    const first = within(bindingRow('deepseek-main')).getByLabelText(
      'Upstream account: deepseek-main',
    ) as HTMLSelectElement
    const second = within(bindingRow('deepseek-backup')).getByLabelText(
      'Upstream account: deepseek-backup',
    ) as HTMLSelectElement

    fireEvent.change(first, { target: { value: '1' } })

    // A 行挂起时，B 行仍然可操作。
    await waitFor(() => expect(first.disabled).toBe(true))
    expect(second.disabled).toBe(false)

    fireEvent.change(second, { target: { value: '1' } })

    // 两行各自挂起：B 的请求不会让 A 提前「解锁」。
    await waitFor(() => expect(records.channelPatch).toHaveLength(2))
    expect(first.disabled).toBe(true)
    expect(second.disabled).toBe(true)

    releaseGate?.()

    // 两个请求都落定后各自恢复。
    await waitFor(() => expect(first.disabled).toBe(false))
    await waitFor(() => expect(second.disabled).toBe(false))
    // 每行只发一次 PATCH——挂起期间行保持禁用，不能带着旧 version 重发。
    expect(records.channelPatch.filter((patch) => patch.id === 7)).toHaveLength(1)
    expect(records.channelPatch.filter((patch) => patch.id === 8)).toHaveLength(1)
  })

  it('renders the read-only notice and disables every write control', async () => {
    installApi({ accounts: [makeAccount()], channels: [makeChannel()] })
    await renderTab(false)

    expect(
      await screen.findByText(
        'Read-only: your account lacks the settings:manage permission.',
      ),
    ).toBeTruthy()
    await screen.findByTestId('binding-row')

    expect(
      (screen.getByRole('button', { name: 'New account' }) as HTMLButtonElement)
        .disabled,
    ).toBe(true)

    const row = accountRow('DeepSeek')
    for (const label of ['Edit', 'Disable', 'Delete']) {
      expect(
        (within(row).getByRole('button', { name: label }) as HTMLButtonElement)
          .disabled,
      ).toBe(true)
    }

    const select = within(screen.getByTestId('binding-row')).getByLabelText(
      'Upstream account: deepseek-main',
    ) as HTMLSelectElement
    expect(select.disabled).toBe(true)
  })
})
