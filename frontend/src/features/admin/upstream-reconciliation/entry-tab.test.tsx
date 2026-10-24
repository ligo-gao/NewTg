import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { EntryTab } from './entry-tab'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
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
    http.get(`${ORIGIN}/api/upstream-snapshots`, ({ request }) => {
      options.onSnapshot?.(new URL(request.url))
      return HttpResponse.json({ items: SNAPSHOTS })
    }),
    http.put(`${ORIGIN}/api/upstream-snapshots`, async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>
      options.onPut?.(body)
      return HttpResponse.json({ updated: 1 })
    }),
  )
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
})
