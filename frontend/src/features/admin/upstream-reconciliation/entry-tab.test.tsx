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

import { renderWithProviders } from '@/test-support/providers'

import { EntryTab } from './entry-tab'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  vi.restoreAllMocks()
})
afterAll(() => server.close())

const ACCOUNT = {
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
}

const SNAPSHOTS = [
  {
    id: 11,
    account_id: 1,
    usage_date: '2026-10-23',
    upstream_model: 'deepseek-chat',
    input_cache_hit_tokens: 100,
    input_cache_miss_tokens: 0,
    cache_write_tokens: null,
    output_tokens: 800,
    requests: 5,
    amount_minor: 1234,
    currency: 'CNY',
    source: 'manual',
    entered_at: '2026-10-23T10:00:00Z',
  },
  {
    id: 12,
    account_id: 1,
    usage_date: '2026-10-23',
    upstream_model: 'deepseek-reasoner',
    input_cache_hit_tokens: 20,
    input_cache_miss_tokens: 10,
    cache_write_tokens: 0,
    output_tokens: 900,
    requests: 3,
    amount_minor: 567,
    currency: 'CNY',
    source: 'manual',
    entered_at: '2026-10-23T10:00:00Z',
  },
]

interface SnapshotApiOptions {
  onSnapshot?: (url: URL) => void
  onPut?: (body: Record<string, unknown>) => void
  /** 保存请求在此 Promise 完成前不返回，用于断言保存进行中的 UI 状态。 */
  putGate?: Promise<void>
  /** 快照读取在此 Promise 完成前不返回，用于断言「载入中」的竞态。 */
  getGate?: Promise<void>
  /** 在 getGate 放行、响应即将返回时触发：用于等待响应真正抵达。 */
  onSnapshotDelivered?: () => void
}

function installSnapshotApi(options: SnapshotApiOptions = {}) {
  server.use(
    http.get(`${ORIGIN}/api/upstream-accounts`, () =>
      HttpResponse.json({
        items: [ACCOUNT],
        total: 1,
        page: 1,
        page_size: 20,
      }),
    ),
    http.get(`${ORIGIN}/api/upstream-snapshots`, async ({ request }) => {
      options.onSnapshot?.(new URL(request.url))
      await options.getGate
      options.onSnapshotDelivered?.()
      return HttpResponse.json({ items: SNAPSHOTS })
    }),
    http.put(`${ORIGIN}/api/upstream-snapshots`, async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>
      options.onPut?.(body)
      await options.putGate
      return HttpResponse.json({ updated: 1 })
    }),
  )
}

function deferred(): { promise: Promise<void>; resolve: () => void } {
  let resolve: () => void = () => undefined
  const promise = new Promise<void>((done) => {
    resolve = done
  })
  return { promise, resolve }
}

/** 选中账号后填入日期并加载当日快照。 */
async function loadDate(date: string) {
  await screen.findByRole('option', { name: 'DeepSeek' })
  fireEvent.change(screen.getByLabelText('Usage date'), {
    target: { value: date },
  })
  await userEvent.setup().click(screen.getByRole('button', { name: 'Load' }))
}

describe('snapshot entry tab', () => {
  it('prefills from the server and PUTs only dirty rows', async () => {
    let putBody: Record<string, unknown> | null = null
    let snapshotCalls = 0
    const seenParams: URLSearchParams[] = []
    installSnapshotApi({
      onSnapshot: (url) => {
        snapshotCalls += 1
        seenParams.push(url.searchParams)
      },
      onPut: (body) => {
        putBody = body
      },
    })

    await renderWithProviders(<EntryTab canManage />, ['common', 'admin'])
    const user = userEvent.setup()

    await loadDate('2026-10-23')

    const rows = await screen.findAllByTestId('entry-model-row')
    expect(rows).toHaveLength(2)
    const first = rows[0]
    if (first === undefined) throw new Error('missing first row')

    // 空值保持为空（NULL），显式 0 保持为 0；金额前置两位小数。
    expect(
      (within(first).getByLabelText('Cache hit tokens') as HTMLInputElement).value,
    ).toBe('100')
    expect(
      (within(first).getByLabelText('Cache miss tokens') as HTMLInputElement)
        .value,
    ).toBe('0')
    expect(
      (within(first).getByLabelText('Cache write tokens') as HTMLInputElement)
        .value,
    ).toBe('')
    expect(
      (within(first).getByLabelText('Amount (¥)') as HTMLInputElement).value,
    ).toBe('12.34')

    const output = within(first).getByLabelText('Output tokens')
    await user.clear(output)
    await user.type(output, '1200')
    expect(screen.getByText('1 unsaved row(s)')).toBeTruthy()

    await user.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(putBody).not.toBeNull())
    expect(putBody).toEqual({
      account_id: 1,
      items: [
        {
          usage_date: '2026-10-23',
          upstream_model: 'deepseek-chat',
          input_cache_hit_tokens: 100,
          input_cache_miss_tokens: 0,
          cache_write_tokens: null,
          output_tokens: 1200,
          requests: 5,
          amount_minor: 1234,
        },
      ],
    })
    // 读取按账号 + 单日区间发出，保存后快照查询被失效重取。
    expect(seenParams[0]?.get('account_id')).toBe('1')
    expect(seenParams[0]?.get('from')).toBe('2026-10-23')
    expect(seenParams[0]?.get('to')).toBe('2026-10-23')
    await waitFor(() => expect(snapshotCalls).toBe(2))
  })

  it('rejects a negative token count before calling the API', async () => {
    let putBody: Record<string, unknown> | null = null
    installSnapshotApi({
      onPut: (body) => {
        putBody = body
      },
    })

    await renderWithProviders(<EntryTab canManage />, ['common', 'admin'])
    const user = userEvent.setup()
    await loadDate('2026-10-23')

    const rows = await screen.findAllByTestId('entry-model-row')
    const first = rows[0]
    if (first === undefined) throw new Error('missing first row')

    fireEvent.change(within(first).getByLabelText('Output tokens'), {
      target: { value: '-5' },
    })
    await user.click(screen.getByRole('button', { name: 'Save' }))

    expect(
      await screen.findByText('Token counts and amounts cannot be negative.'),
    ).toBeTruthy()
    expect(putBody).toBeNull()
  })

  it('shows a read-only notice and disables the editor when canManage is false', async () => {
    installSnapshotApi()
    await renderWithProviders(<EntryTab canManage={false} />, [
      'common',
      'admin',
    ])

    expect(
      await screen.findByText(
        'Read-only: your account lacks the settings:manage permission.',
      ),
    ).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Reset' })).toBeNull()

    await loadDate('2026-10-23')
    const rows = await screen.findAllByTestId('entry-model-row')
    const first = rows[0]
    if (first === undefined) throw new Error('missing first row')
    expect(
      (within(first).getByLabelText('Output tokens') as HTMLInputElement)
        .disabled,
    ).toBe(true)
  })

  it('renders loaded model names as read-only text', async () => {
    installSnapshotApi()
    await renderWithProviders(<EntryTab canManage />, ['common', 'admin'])
    await loadDate('2026-10-23')

    const rows = await screen.findAllByTestId('entry-model-row')
    const first = rows[0]
    if (first === undefined) throw new Error('missing first row')

    // 模型名不是输入框：重命名只会 upsert 出一条新行，旧行无法删除。
    const modelCell = first.querySelector('td')
    if (modelCell === null) throw new Error('missing model cell')
    expect(modelCell.querySelector('input')).toBeNull()
    expect(within(first).queryByRole('textbox', { name: 'Model' })).toBeNull()
    expect(within(first).getByText('deepseek-chat')).toBeTruthy()

    // 其余单元格仍是可编辑输入框。
    expect(within(first).getByLabelText('Output tokens').tagName).toBe('INPUT')
    expect(within(first).getByLabelText('Amount (¥)').tagName).toBe('INPUT')
  })

  it('keeps accumulated keystrokes in one numeric input', async () => {
    installSnapshotApi()
    await renderWithProviders(<EntryTab canManage />, ['common', 'admin'])
    const user = userEvent.setup()
    await loadDate('2026-10-23')

    const rows = await screen.findAllByTestId('entry-model-row')
    const first = rows[0]
    if (first === undefined) throw new Error('missing first row')
    const output = within(first).getByLabelText('Output tokens')

    await user.clear(output)
    await user.type(output, '1200')

    // 每次按键都重渲染，行不能被重新挂载（否则焦点与输入会丢失）。
    expect(screen.getAllByTestId('entry-model-row')[0]).toBe(first)
    expect((output as HTMLInputElement).value).toBe('1200')
    expect(document.activeElement).toBe(output)
  })

  it('disables the cell inputs and the window controls while a save is in flight', async () => {
    const gate = deferred()
    installSnapshotApi({ putGate: gate.promise })
    await renderWithProviders(<EntryTab canManage />, ['common', 'admin'])
    const user = userEvent.setup()
    await loadDate('2026-10-23')

    const rows = await screen.findAllByTestId('entry-model-row')
    const first = rows[0]
    if (first === undefined) throw new Error('missing first row')
    const output = within(first).getByLabelText('Output tokens') as HTMLInputElement
    const amount = within(first).getByLabelText('Amount (¥)') as HTMLInputElement
    const loadButton = screen.getByRole('button', {
      name: 'Load',
    }) as HTMLButtonElement
    const dateInput = screen.getByLabelText('Usage date') as HTMLInputElement
    const accountSelect = screen.getByLabelText(
      'Upstream account',
    ) as HTMLSelectElement

    await user.clear(output)
    await user.type(output, '1200')
    await user.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(output.disabled).toBe(true))
    expect(amount.disabled).toBe(true)
    // 保存期间换窗口会让基线取自提交时的行、而表格留在别的数据上，
    // 下一次保存就会静默回滚这次成功的写入。
    expect(loadButton.disabled).toBe(true)
    expect(dateInput.disabled).toBe(true)
    expect(accountSelect.disabled).toBe(true)

    gate.resolve()
    await waitFor(() => expect(output.disabled).toBe(false))
    expect(amount.disabled).toBe(false)
    expect(loadButton.disabled).toBe(false)
    expect(dateInput.disabled).toBe(false)
    expect(accountSelect.disabled).toBe(false)
  })

  it('keeps unsaved edits when the discard confirmation is declined', async () => {
    let snapshotCalls = 0
    installSnapshotApi({
      onSnapshot: () => {
        snapshotCalls += 1
      },
    })
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)

    await renderWithProviders(<EntryTab canManage />, ['common', 'admin'])
    const user = userEvent.setup()
    await loadDate('2026-10-23')
    expect(snapshotCalls).toBe(1)

    const rows = await screen.findAllByTestId('entry-model-row')
    const first = rows[0]
    if (first === undefined) throw new Error('missing first row')
    const output = within(first).getByLabelText('Output tokens')

    await user.clear(output)
    await user.type(output, '1200')
    await user.click(screen.getByRole('button', { name: 'Load' }))

    expect(confirmSpy).toHaveBeenCalledWith(
      'Discard unsaved snapshot edits?',
    )
    // 拒绝后：不发新 GET，行与编辑值保持原样。
    expect(snapshotCalls).toBe(1)
    expect(screen.getAllByTestId('entry-model-row')[0]).toBe(first)
    expect((output as HTMLInputElement).value).toBe('1200')
    expect(screen.getByText('1 unsaved row(s)')).toBeTruthy()
  })

  it('keeps a row added while a load is in flight', async () => {
    // 读请求在飞行中时新增一行：响应抵达不得静默清掉这一行（脏数据优先）。
    const gate = deferred()
    let delivered = false
    installSnapshotApi({
      getGate: gate.promise,
      onSnapshotDelivered: () => {
        delivered = true
      },
    })
    await renderWithProviders(<EntryTab canManage />, ['common', 'admin'])
    const user = userEvent.setup()
    await screen.findByRole('option', { name: 'DeepSeek' })
    fireEvent.change(screen.getByLabelText('Usage date'), {
      target: { value: '2026-10-23' },
    })

    await user.click(screen.getByRole('button', { name: 'Load' }))
    await user.type(screen.getByLabelText('Add model'), 'deepseek-new')
    await user.click(screen.getByRole('button', { name: 'Add' }))

    gate.resolve()
    await waitFor(() => expect(delivered).toBe(true))
    // 必须等到响应真正落到缓存并重渲染（加载提示消失）：在响应抵达前，
    // 表格本来就只剩新增的这一行，三个断言会在「守卫被删掉」时同样成立。
    await waitFor(() => expect(screen.queryByText('Loading snapshot…')).toBeNull())

    const rows = screen.getAllByTestId('entry-model-row')
    expect(rows).toHaveLength(1)
    expect(within(rows[0] as HTMLElement).getByText('deepseek-new')).toBeTruthy()
    expect(screen.queryByText('deepseek-chat')).toBeNull()
  })

  it('keeps the account selection when its discard confirmation is declined', async () => {
    installSnapshotApi()
    vi.spyOn(window, 'confirm').mockReturnValue(false)

    await renderWithProviders(<EntryTab canManage />, ['common', 'admin'])
    const user = userEvent.setup()
    await loadDate('2026-10-23')

    const rows = await screen.findAllByTestId('entry-model-row')
    const first = rows[0]
    if (first === undefined) throw new Error('missing first row')
    const output = within(first).getByLabelText('Output tokens') as HTMLInputElement
    await user.clear(output)
    await user.type(output, '1200')

    const select = screen.getByLabelText('Upstream account') as HTMLSelectElement
    fireEvent.change(select, { target: { value: '' } })

    // 拒绝丢弃后，受控 select 回到原值，行与编辑值不变。
    await waitFor(() => expect(select.value).toBe('1'))
    expect(output.value).toBe('1200')
    expect(screen.getByText('1 unsaved row(s)')).toBeTruthy()
  })

  it('keeps the date and edits when its discard confirmation is declined', async () => {
    installSnapshotApi()
    vi.spyOn(window, 'confirm').mockReturnValue(false)

    await renderWithProviders(<EntryTab canManage />, ['common', 'admin'])
    const user = userEvent.setup()
    await loadDate('2026-10-23')

    const rows = await screen.findAllByTestId('entry-model-row')
    const first = rows[0]
    if (first === undefined) throw new Error('missing first row')
    const output = within(first).getByLabelText('Output tokens') as HTMLInputElement
    await user.clear(output)
    await user.type(output, '1200')

    const dateInput = screen.getByLabelText('Usage date') as HTMLInputElement
    fireEvent.change(dateInput, { target: { value: '2026-10-22' } })

    await waitFor(() => expect(dateInput.value).toBe('2026-10-23'))
    expect(output.value).toBe('1200')
    expect(screen.getByText('1 unsaved row(s)')).toBeTruthy()
  })

  it('reloads and drops edits when the discard confirmation is accepted', async () => {
    let snapshotCalls = 0
    installSnapshotApi({
      onSnapshot: () => {
        snapshotCalls += 1
      },
    })
    vi.spyOn(window, 'confirm').mockReturnValue(true)

    await renderWithProviders(<EntryTab canManage />, ['common', 'admin'])
    const user = userEvent.setup()
    await loadDate('2026-10-23')

    const rows = await screen.findAllByTestId('entry-model-row')
    const first = rows[0]
    if (first === undefined) throw new Error('missing first row')
    const output = within(first).getByLabelText('Output tokens') as HTMLInputElement

    await user.clear(output)
    await user.type(output, '1200')
    await user.click(screen.getByRole('button', { name: 'Load' }))

    await waitFor(() => expect(snapshotCalls).toBe(2))
    await waitFor(() => {
      const reloaded = screen.getAllByTestId('entry-model-row')
      const reloadedFirst = reloaded[0]
      if (reloadedFirst === undefined) throw new Error('missing reloaded row')
      expect(
        (
          within(reloadedFirst).getByLabelText(
            'Output tokens',
          ) as HTMLInputElement
        ).value,
      ).toBe('800')
    })
    expect(screen.getByText('0 unsaved row(s)')).toBeTruthy()
  })

  it('keeps duplicate text and reports it instead of adding a second row', async () => {
    installSnapshotApi()
    await renderWithProviders(<EntryTab canManage />, ['common', 'admin'])
    const user = userEvent.setup()
    await loadDate('2026-10-23')
    await screen.findAllByTestId('entry-model-row')

    const addInput = screen.getByLabelText('Add model') as HTMLInputElement
    const addButton = screen.getByRole('button', { name: 'Add' })

    await user.type(addInput, 'deepseek-chat')
    await user.click(addButton)

    expect(
      await screen.findByText('This model is already in the table'),
    ).toBeTruthy()
    expect(addInput.value).toBe('deepseek-chat')
    expect(screen.getAllByTestId('entry-model-row')).toHaveLength(2)

    // 修改输入即清除提示；新模型仍能正常添加。
    await user.clear(addInput)
    await user.type(addInput, 'deepseek-coder')
    expect(
      screen.queryByText('This model is already in the table'),
    ).toBeNull()
    await user.click(addButton)

    expect(screen.getAllByTestId('entry-model-row')).toHaveLength(3)
    expect(screen.getByText('deepseek-coder')).toBeTruthy()
  })

  it('clears the duplicate-model message on Load and on Reset', async () => {
    installSnapshotApi()
    await renderWithProviders(<EntryTab canManage />, ['common', 'admin'])
    const user = userEvent.setup()
    await loadDate('2026-10-23')
    await screen.findAllByTestId('entry-model-row')

    const addButton = screen.getByRole('button', { name: 'Add' })
    await user.type(screen.getByLabelText('Add model'), 'deepseek-chat')
    await user.click(addButton)
    expect(
      await screen.findByText('This model is already in the table'),
    ).toBeTruthy()

    // 提示描述的是上一次尝试，重新载入后不该还留在屏幕上。
    await user.click(screen.getByRole('button', { name: 'Load' }))
    await waitFor(() =>
      expect(
        screen.queryByText('This model is already in the table'),
      ).toBeNull(),
    )

    // 输入框保留着重复的模型名，再点一次即可复现提示。Reset 只在有未保存
    // 编辑时可用，所以先改一个单元格。
    const first = screen.getAllByTestId('entry-model-row')[0]
    if (first === undefined) throw new Error('missing first row')
    await user.type(within(first).getByLabelText('Output tokens'), '9')
    await user.click(addButton)
    expect(
      await screen.findByText('This model is already in the table'),
    ).toBeTruthy()

    await user.click(screen.getByRole('button', { name: 'Reset' }))
    await waitFor(() =>
      expect(
        screen.queryByText('This model is already in the table'),
      ).toBeNull(),
    )
  })

  it('disables Add until both an account and a date are selected', async () => {
    installSnapshotApi()
    await renderWithProviders(<EntryTab canManage />, ['common', 'admin'])
    const user = userEvent.setup()

    const addInput = screen.getByLabelText('Add model') as HTMLInputElement
    const addButton = screen.getByRole('button', {
      name: 'Add',
    }) as HTMLButtonElement

    // 账号尚未加载/选中。
    expect(screen.getByLabelText('Upstream account')).toBeInstanceOf(
      HTMLSelectElement,
    )
    expect(addButton.disabled).toBe(true)
    expect(addInput.disabled).toBe(true)

    await screen.findByRole('option', { name: 'DeepSeek' })
    const dateInput = screen.getByLabelText('Usage date')
    const accountSelect = screen.getByLabelText(
      'Upstream account',
    ) as HTMLSelectElement
    await waitFor(() => expect(accountSelect.value).toBe('1'))

    // 日期为空时同样不可添加。
    fireEvent.change(dateInput, { target: { value: '' } })
    await waitFor(() => expect(addButton.disabled).toBe(true))
    expect(addInput.disabled).toBe(true)

    // 账号 + 日期齐备、且模型名非空后才可添加。
    fireEvent.change(dateInput, { target: { value: '2026-10-24' } })
    await user.type(addInput, 'deepseek-chat')
    await waitFor(() => expect(addButton.disabled).toBe(false))
    expect(addInput.disabled).toBe(false)
  })
})
