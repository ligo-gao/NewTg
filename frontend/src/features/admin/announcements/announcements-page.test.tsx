import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

import type {
  AnnouncementCategory,
  AnnouncementDetail,
  AnnouncementDisplayStatus,
  AnnouncementStatus,
  AnnouncementSummary,
} from '@/features/announcements/api'
import { renderWithProviders } from '@/test-support/providers'
import { useToastStore } from '@/shared/ui/toast'

import { AnnouncementsPage } from './announcements-page'
import type { AnnouncementsSearch } from './search'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

/** 最近一次列表请求；用于断言 URL search 契约。 */
let lastListUrl: URL | undefined

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
beforeEach(() => {
  // jsdom 不做布局，Dialog 的初始焦点候选过滤依赖非空 rect；
  // 确认框「初始焦点落在取消」的断言需要这个 mock。
  vi.spyOn(HTMLElement.prototype, 'getClientRects').mockImplementation(
    () => ({ length: 1 }) as unknown as DOMRectList,
  )
})
afterEach(() => {
  server.resetHandlers()
  useToastStore.setState({ toasts: [] })
  sessionStorage.clear()
  vi.restoreAllMocks()
  Object.defineProperty(navigator, 'onLine', { configurable: true, value: true })
  // 让仍挂载的组件从离线态恢复；也是下一个用例的干净起点。
  window.dispatchEvent(new Event('online'))
})
afterAll(() => server.close())

interface RowSeed {
  id: string
  title: string
  category: AnnouncementCategory
  status: AnnouncementStatus
  display_status: AnnouncementDisplayStatus
  content?: string
}

function summary(seed: RowSeed): AnnouncementSummary {
  return {
    id: seed.id,
    title: seed.title,
    category: seed.category,
    status: seed.status,
    display_status: seed.display_status,
    publish_at: '2026-10-01T09:00:00Z',
    expire_at:
      seed.display_status === 'expired' ? '2026-10-02T09:00:00Z' : null,
    published_at:
      seed.status === 'draft' ? null : '2026-10-01T09:05:00Z',
    offline_at: seed.status === 'offline' ? '2026-10-03T09:00:00Z' : null,
    created_by: 1,
    updated_by: 1,
    created_at: '2026-10-01T08:00:00Z',
    updated_at: '2026-10-01T09:05:00Z',
  }
}

function detailOf(row: AnnouncementSummary, content: string): AnnouncementDetail {
  return {
    ...row,
    content_markdown: content,
    content_html: `<p>${content}</p>`,
  }
}

const SEEDS: RowSeed[] = [
  {
    id: '11111111-1111-4111-8111-111111111111',
    title: 'Draft notice',
    category: 'notice',
    status: 'draft',
    display_status: 'draft',
    content: 'Draft body',
  },
  {
    id: '22222222-2222-4222-8222-222222222222',
    title: 'Live maintenance',
    category: 'maintenance',
    status: 'published',
    display_status: 'published',
    content: 'Live body',
  },
  {
    id: '33333333-3333-4333-8333-333333333333',
    title: 'Expired release',
    category: 'release',
    status: 'published',
    display_status: 'expired',
    content: 'Expired body',
  },
  {
    id: '44444444-4444-4444-8444-444444444444',
    title: 'Pulled notice',
    category: 'notice',
    status: 'offline',
    display_status: 'offline',
    content: 'Pulled body',
  },
]

interface Capture {
  listUrls: URL[]
  posts: { path: string; body: Record<string, unknown> | null; headers: Headers }[]
  puts: { id: string; body: Record<string, unknown> }[]
  deletes: string[]
}

interface InstallOptions {
  /** 覆盖列表响应；用于空态/错误/429/分页 clamp。 */
  listOverride?: (rows: AnnouncementSummary[], url: URL) => Response
  /** 覆盖写请求响应；用于 publish_now 回滚与 429。 */
  writeOverride?: (request: Request) => Response | undefined
}

function install(options: InstallOptions = {}) {
  let rows = SEEDS.map(summary)
  const contents = new Map(SEEDS.map((seed) => [seed.id, seed.content ?? 'Body']))
  const capture: Capture = { listUrls: [], posts: [], puts: [], deletes: [] }

  server.use(
    http.get(`${ORIGIN}/api/announcements`, ({ request }) => {
      const url = new URL(request.url)
      capture.listUrls.push(url)
      lastListUrl = url
      if (options.listOverride !== undefined) {
        return options.listOverride(rows, url)
      }
      let items = rows
      const status = url.searchParams.get('status')
      if (status !== null) {
        items = items.filter((row) =>
          status === 'expired'
            ? row.display_status === 'expired'
            : row.display_status === status,
        )
      }
      const category = url.searchParams.get('category')
      if (category !== null) {
        items = items.filter((row) => row.category === category)
      }
      const page = Number(url.searchParams.get('p') ?? '1')
      const pageSize = Number(url.searchParams.get('page_size') ?? '20')
      const total = items.length
      const start = (page - 1) * pageSize
      return HttpResponse.json({
        items: items.slice(start, start + pageSize),
        total,
        page,
        page_size: pageSize,
      })
    }),
    http.get(`${ORIGIN}/api/announcements/:id`, ({ params }) => {
      const row = rows.find((entry) => entry.id === params.id)
      if (row === undefined) {
        return HttpResponse.json({ title: 'Not found' }, { status: 404 })
      }
      return HttpResponse.json(detailOf(row, contents.get(row.id) ?? 'Body'))
    }),
    http.post(`${ORIGIN}/api/announcements`, async ({ request }) => {
      const override = options.writeOverride?.(request)
      if (override !== undefined) return override
      const body = (await request.json()) as Record<string, unknown>
      capture.posts.push({
        path: 'create',
        body,
        headers: new Headers(request.headers),
      })
      const publishNow = body.publish_now === true
      const id = crypto.randomUUID()
      rows = [
        ...rows,
        summary({
          id,
          title: String(body.title ?? ''),
          category: (body.category as AnnouncementCategory) ?? 'notice',
          status: publishNow ? 'published' : 'draft',
          display_status: publishNow ? 'published' : 'draft',
        }),
      ]
      contents.set(id, String(body.content ?? ''))
      return HttpResponse.json(
        detailOf(rows[rows.length - 1] as AnnouncementSummary, String(body.content ?? '')),
        { status: 201 },
      )
    }),
    http.post(`${ORIGIN}/api/announcements/:id/:verb`, async ({ request, params }) => {
      const override = options.writeOverride?.(request)
      if (override !== undefined) return override
      const body = await request.clone().json().catch(() => null)
      capture.posts.push({
        path: String(params.verb),
        body: body as Record<string, unknown> | null,
        headers: new Headers(request.headers),
      })
      const row = rows.find((entry) => entry.id === params.id)
      if (row === undefined) {
        return HttpResponse.json({ title: 'Not found' }, { status: 404 })
      }
      if (params.verb === 'publish') {
        row.status = 'published'
        row.display_status = 'published'
        row.published_at = '2026-10-20T10:00:00Z'
      } else {
        row.status = 'offline'
        row.display_status = 'offline'
        row.offline_at = '2026-10-20T10:00:00Z'
      }
      return HttpResponse.json(detailOf(row, contents.get(row.id) ?? 'Body'))
    }),
    http.put(`${ORIGIN}/api/announcements/:id`, async ({ request, params }) => {
      const override = options.writeOverride?.(request)
      if (override !== undefined) return override
      const body = (await request.json()) as Record<string, unknown>
      capture.puts.push({ id: String(params.id), body })
      const row = rows.find((entry) => entry.id === params.id)
      if (row === undefined) {
        return HttpResponse.json({ title: 'Not found' }, { status: 404 })
      }
      if (typeof body.content === 'string') contents.set(row.id, body.content)
      if (typeof body.title === 'string') row.title = body.title
      if (row.status === 'offline' || row.status === 'draft') {
        const publishNow = body.publish_now === true
        row.status = publishNow ? 'published' : 'draft'
        row.display_status = publishNow ? 'published' : 'draft'
        if (publishNow) row.published_at = '2026-10-20T10:00:00Z'
      }
      return HttpResponse.json(
        detailOf(row, contents.get(row.id) ?? 'Body'),
      )
    }),
    http.delete(`${ORIGIN}/api/announcements/:id`, ({ params }) => {
      capture.deletes.push(String(params.id))
      rows = rows.filter((entry) => entry.id !== params.id)
      return new HttpResponse(null, { status: 204 })
    }),
  )

  return capture
}

function Harness({ initial }: { initial?: AnnouncementsSearch }) {
  const [search, setSearch] = useState<AnnouncementsSearch>(initial ?? {})
  return (
    <AnnouncementsPage
      search={search}
      onSearchChange={(patch) => {
        setSearch((previous) => ({ ...previous, ...patch }))
      }}
    />
  )
}

async function renderPage(initial?: AnnouncementsSearch) {
  await renderWithProviders(<Harness initial={initial} />, ['common', 'admin'])
  await screen.findByText('Draft notice')
}

async function openCreate(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('button', { name: 'Publish announcement' }))
  return screen.getByRole('dialog')
}

async function fillRequired(
  user: ReturnType<typeof userEvent.setup>,
  title: string,
  content: string,
) {
  await user.type(screen.getByLabelText('Title'), title)
  fireEvent.change(screen.getByLabelText('Content (Markdown)'), {
    target: { value: content },
  })
}

describe('announcements admin page (F-08-04)', () => {
  beforeEach(() => {
    useToastStore.setState({ toasts: [] })
  })

  it('renders the status machine badges, categories and list columns', async () => {
    const capture = install()
    await renderPage()

    const rows = screen.getAllByTestId('announcement-row')
    expect(rows).toHaveLength(4)

    const badgeFor = (title: string): HTMLElement => {
      const row = rows.find((entry) => entry.textContent?.includes(title))
      if (row === undefined) throw new Error(`missing row ${title}`)
      return within(row).getByTestId('status-badge')
    }

    // 徽章口径由 UI §6.1.1 冻结：draft 中性描边、published 成功、
    // expired 警告（虚拟态）、offline 中性。
    expect(badgeFor('Draft notice').getAttribute('data-variant')).toBe('neutral-outline')
    expect(badgeFor('Draft notice').textContent).toContain('Draft')
    expect(badgeFor('Live maintenance').getAttribute('data-variant')).toBe('success')
    expect(badgeFor('Live maintenance').textContent).toContain('Published')
    expect(badgeFor('Expired release').getAttribute('data-variant')).toBe('warning')
    expect(badgeFor('Expired release').textContent).toContain('Expired')
    expect(badgeFor('Pulled notice').getAttribute('data-variant')).toBe('neutral')
    expect(badgeFor('Pulled notice').textContent).toContain('Offline')

    // 分类是 Tag（无状态圆点），不是 StatusBadge。
    expect(within(rows[1] as HTMLElement).getByTestId('tag').textContent).toBe(
      'Maintenance',
    )

    for (const header of screen.getAllByRole('columnheader')) {
      expect(header.getAttribute('scope')).toBe('col')
    }
    const lastRequest = capture.listUrls[capture.listUrls.length - 1]
    expect(lastRequest?.searchParams.get('p')).toBe('1')
    expect(lastRequest?.searchParams.get('page_size')).toBe('20')
  })

  it('sends status/category filters and resets to page 1', async () => {
    const capture = install()
    const user = userEvent.setup()
    await renderPage()

    await user.selectOptions(screen.getByRole('combobox', { name: 'Status' }), 'draft')
    await waitFor(() => {
      expect(screen.getAllByTestId('announcement-row')).toHaveLength(1)
    })
    expect(screen.getByText('Draft notice')).toBeTruthy()
    const filtered = capture.listUrls[capture.listUrls.length - 1]
    expect(filtered?.searchParams.get('status')).toBe('draft')
    expect(filtered?.searchParams.get('p')).toBe('1')

    await user.selectOptions(screen.getByRole('combobox', { name: 'Category' }), 'release')
    await waitFor(() => {
      // draft + release 没有交集：空态出现，筛选条件仍保留在控件上。
      expect(screen.getByTestId('announcement-empty')).toBeTruthy()
    })
    const both = capture.listUrls[capture.listUrls.length - 1]
    expect(both?.searchParams.get('status')).toBe('draft')
    expect(both?.searchParams.get('category')).toBe('release')
  })

  it('paginates and changes page_size through the URL search contract', async () => {
    install({
      listOverride: () =>
        HttpResponse.json({
          items: SEEDS.map(summary),
          total: 45,
          page: 1,
          page_size: 20,
        }),
    })
    const user = userEvent.setup()
    await renderPage()

    expect(screen.getByText('1–20 of 45')).toBeTruthy()
    await user.click(screen.getByRole('button', { name: 'Next' }))
    await waitFor(() => {
      expect(lastListUrl?.searchParams.get('p')).toBe('2')
    })

    await user.selectOptions(screen.getByLabelText('Rows per page'), '10')
    await waitFor(() => {
      expect(lastListUrl?.searchParams.get('page_size')).toBe('10')
      expect(lastListUrl?.searchParams.get('p')).toBe('1')
    })
  })

  it('shows the empty state with the primary publish action', async () => {
    install({ listOverride: () => HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }) })
    await renderWithProviders(<Harness />, ['common', 'admin'])

    const empty = await screen.findByTestId('announcement-empty')
    expect(empty.textContent).toContain('No announcements yet')
    expect(
      within(empty).getByRole('button', { name: 'Publish announcement' }),
    ).toBeTruthy()
  })

  it('keeps filters on a list error and retries in place', async () => {
    let failing = true
    install({
      listOverride: () =>
        failing
          ? new HttpResponse(
              JSON.stringify({ type: 'about:blank', title: 'Server error' }),
              { status: 500, headers: { 'Content-Type': 'application/problem+json' } },
            )
          : HttpResponse.json({ items: SEEDS.map(summary), total: 4, page: 1, page_size: 20 }),
    })
    await renderWithProviders(<Harness initial={{ status: 'draft' }} />, ['common', 'admin'])

    const errorBox = await screen.findByTestId('announcement-list-error')
    expect(errorBox.textContent).toContain('Failed to load announcements.')
    // 筛选控件仍在错误态之上，且保留了 draft。
    expect((screen.getByLabelText('Status') as HTMLSelectElement).value).toBe('draft')

    failing = false
    await userEvent.setup().click(
      within(errorBox).getByRole('button', { name: 'Retry' }),
    )
    await screen.findByText('Draft notice')
  })

  it('shows a 429 list error inline with the Retry-After hint', async () => {
    install({
      listOverride: () =>
        new HttpResponse(
          JSON.stringify({ type: 'about:blank', title: 'Too many requests' }),
          {
            status: 429,
            headers: {
              'Content-Type': 'application/problem+json',
              'Retry-After': '5',
            },
          },
        ),
    })
    await renderWithProviders(<Harness />, ['common', 'admin'])

    expect(
      (await screen.findByTestId('announcement-list-error')).textContent,
    ).toContain('Try again in 5 s.')
  })

  it('publishes a draft only after the alertdialog confirmation', async () => {
    const capture = install()
    const user = userEvent.setup()
    await renderPage()

    await user.click(
      screen.getByRole('button', { name: 'Publish Draft notice' }),
    )
    const dialog = await screen.findByRole('alertdialog')
    expect(dialog.textContent).toContain('Users will see it immediately')
    expect(capture.posts).toHaveLength(0)

    await user.click(within(dialog).getByRole('button', { name: 'Publish' }))
    await waitFor(() => {
      expect(capture.posts).toHaveLength(1)
    })
    expect(capture.posts[0]?.path).toBe('publish')
    // 发布携带幂等键（F-08-04 TC-05）。
    expect(capture.posts[0]?.headers.get('Idempotency-Key')).toBeTruthy()
    await waitFor(() => {
      expect(screen.getByText('Draft notice')).toBeTruthy()
      expect(
        within(
          screen
            .getAllByTestId('announcement-row')
            .find((row) => row.textContent?.includes('Draft notice')) as HTMLElement,
        ).getByTestId('status-badge').textContent,
      ).toContain('Published')
    })
  })

  it('takes a published announcement offline without a confirmation', async () => {
    const capture = install()
    const user = userEvent.setup()
    await renderPage()

    await user.click(
      screen.getByRole('button', { name: 'Take Live maintenance offline' }),
    )
    await waitFor(() => {
      expect(capture.posts).toHaveLength(1)
    })
    expect(screen.queryByRole('alertdialog')).toBeNull()
    expect(capture.posts[0]?.path).toBe('offline')
    expect(capture.posts[0]?.headers.get('Idempotency-Key')).toBeTruthy()
  })

  it('takes an expired announcement offline like a published one', async () => {
    const capture = install()
    const user = userEvent.setup()
    await renderPage()

    await user.click(
      screen.getByRole('button', { name: 'Take Expired release offline' }),
    )
    await waitFor(() => {
      expect(capture.posts).toHaveLength(1)
    })
    expect(capture.posts[0]?.path).toBe('offline')
    await waitFor(() => {
      const row = screen
        .getAllByTestId('announcement-row')
        .find((entry) => entry.textContent?.includes('Expired release'))
      expect(
        within(row as HTMLElement).getByTestId('status-badge').textContent,
      ).toContain('Offline')
    })
  })

  it('keeps the publish confirmation open and reports a 429', async () => {
    install({
      writeOverride: (request) =>
        new URL(request.url).pathname.endsWith('/publish')
          ? new HttpResponse(
              JSON.stringify({ title: 'Too many requests' }),
              {
                status: 429,
                headers: {
                  'Content-Type': 'application/problem+json',
                  'Retry-After': '5',
                },
              },
            )
          : undefined,
    })
    const user = userEvent.setup()
    await renderPage()

    await user.click(
      screen.getByRole('button', { name: 'Publish Draft notice' }),
    )
    const dialog = await screen.findByRole('alertdialog')
    await user.click(within(dialog).getByRole('button', { name: 'Publish' }))

    await waitFor(() => {
      expect(useToastStore.getState().toasts.at(-1)?.message).toBe(
        'Too many requests. Try again in 5 s.',
      )
    })
    // 限流不是「已处理」：确认框保留，用户可以稍后重试。
    expect(screen.getByRole('alertdialog')).toBeTruthy()
  })

  it('sets initial focus on Cancel for destructive confirmations', async () => {
    install()
    const user = userEvent.setup()
    await renderPage()

    await user.click(
      screen.getByRole('button', { name: 'Delete Pulled notice' }),
    )
    const deleteDialog = await screen.findByRole('alertdialog')
    expect(document.activeElement).toBe(
      within(deleteDialog).getByRole('button', { name: 'Cancel' }),
    )

    await user.click(within(deleteDialog).getByRole('button', { name: 'Cancel' }))
    await user.click(
      screen.getByRole('button', { name: 'Publish Draft notice' }),
    )
    const publishDialog = await screen.findByRole('alertdialog')
    expect(document.activeElement).toBe(
      within(publishDialog).getByRole('button', { name: 'Cancel' }),
    )
  })

  it('clamps the page back after deleting the last row on it', async () => {
    install({
      listOverride: (_rows, url) => {
        const requested = Number(url.searchParams.get('p') ?? '1')
        if (requested === 2) {
          return HttpResponse.json({
            items: [summary(SEEDS[3] as RowSeed)],
            total: 21,
            page: 2,
            page_size: 20,
          })
        }
        return HttpResponse.json({
          items: SEEDS.slice(0, 3).map(summary),
          total: 21,
          page: 1,
          page_size: 20,
        })
      },
    })
    const user = userEvent.setup()
    await renderWithProviders(<Harness initial={{ p: 2 }} />, ['common', 'admin'])
    await screen.findByText('Pulled notice')

    await user.click(screen.getByRole('button', { name: 'Delete Pulled notice' }))
    const dialog = await screen.findByRole('alertdialog')
    await user.click(
      within(dialog).getByRole('button', { name: 'Delete permanently' }),
    )

    await waitFor(() => {
      expect(lastListUrl?.searchParams.get('p')).toBe('1')
    })
  })

  it('deletes only after the destructive confirmation', async () => {
    const capture = install()
    const user = userEvent.setup()
    await renderPage()

    await user.click(screen.getByRole('button', { name: 'Delete Pulled notice' }))
    const dialog = await screen.findByRole('alertdialog')
    expect(dialog.textContent).toContain('cannot be undone')
    expect(capture.deletes).toHaveLength(0)

    await user.click(
      within(dialog).getByRole('button', { name: 'Delete permanently' }),
    )
    await waitFor(() => {
      expect(capture.deletes).toHaveLength(1)
    })
    await waitFor(() => {
      expect(screen.queryByText('Pulled notice')).toBeNull()
    })
  })

  it('keeps the loaded snapshot but disables every write while offline', async () => {
    install()
    await renderPage()

    window.dispatchEvent(new Event('offline'))
    await waitFor(() => {
      expect(
        (screen.getByRole('button', { name: 'Publish announcement' }) as HTMLButtonElement)
          .disabled,
      ).toBe(true)
    })
    // 列表快照仍可见。
    expect(screen.getByText('Live maintenance')).toBeTruthy()
    expect(
      (screen.getByRole('button', { name: 'Take Live maintenance offline' }) as HTMLButtonElement)
        .disabled,
    ).toBe(true)
    // 离线时编辑同样是写路径的入口，必须一并禁用（LOW）。
    expect(
      (screen.getByRole('button', { name: 'Edit Live maintenance' }) as HTMLButtonElement)
        .disabled,
    ).toBe(true)
    expect(screen.getByText(/You are offline/)).toBeTruthy()

    window.dispatchEvent(new Event('online'))
    await waitFor(() => {
      expect(
        (screen.getByRole('button', { name: 'Publish announcement' }) as HTMLButtonElement)
          .disabled,
      ).toBe(false)
    })
  })
})

describe('announcement drawer', () => {
  it('counts content by UTF-8 bytes and shows the character count', async () => {
    install()
    const user = userEvent.setup()
    await renderPage()

    await user.click(screen.getByRole('button', { name: 'Publish announcement' }))
    fireEvent.change(screen.getByLabelText('Content (Markdown)'), {
      // Two Cyrillic characters: 2 characters, 4 UTF-8 bytes.
      target: { value: '\u0434\u0430' },
    })
    expect(screen.getByTestId('announcement-content-counter').textContent).toBe(
      '4 / 16384 bytes · 2 characters',
    )
  })

  it('rejects content above 16384 UTF-8 bytes but accepts the exact boundary', async () => {
    const capture = install()
    const user = userEvent.setup()
    await renderPage()

    await openCreate(user)
    await user.type(screen.getByLabelText('Title'), 'Byte limit')
    fireEvent.change(screen.getByLabelText('Content (Markdown)'), {
      target: { value: 'a'.repeat(16385) },
    })
    await user.click(screen.getByRole('button', { name: 'Save draft' }))
    expect(
      await screen.findByText('The content must be at most 16384 UTF-8 bytes.'),
    ).toBeTruthy()
    await waitFor(() => {
      expect(capture.posts).toHaveLength(0)
    })

    fireEvent.change(screen.getByLabelText('Content (Markdown)'), {
      target: { value: 'a'.repeat(16384) },
    })
    await user.click(screen.getByRole('button', { name: 'Save draft' }))
    await waitFor(() => {
      expect(capture.posts).toHaveLength(1)
    })
  })

  it('saves a draft with publish_now=false', async () => {
    const capture = install()
    const user = userEvent.setup()
    await renderPage()

    await openCreate(user)
    await fillRequired(user, 'Maintenance window', 'We will be brief.')
    await user.click(screen.getByRole('button', { name: 'Save draft' }))

    await waitFor(() => {
      expect(capture.posts).toHaveLength(1)
    })
    expect(capture.posts[0]?.body).toMatchObject({
      title: 'Maintenance window',
      content: 'We will be brief.',
      category: 'notice',
      publish_now: false,
    })
    // 保存草稿不是发布确认：不弹 alertdialog。
    expect(screen.queryByRole('alertdialog')).toBeNull()
    await waitFor(() => {
      expect(useToastStore.getState().toasts[0]?.message).toBe('Draft saved.')
    })
  })

  it('publishes through the single atomic path after confirmation', async () => {
    const capture = install()
    const user = userEvent.setup()
    await renderPage()

    await openCreate(user)
    await fillRequired(user, 'Release notes', 'Version 2 is ready.')
    await user.click(screen.getByRole('button', { name: 'Save and publish' }))

    const dialog = await screen.findByRole('alertdialog')
    expect(dialog.textContent).toContain('Users will see it immediately')
    await user.click(within(dialog).getByRole('button', { name: 'Publish' }))

    await waitFor(() => {
      expect(capture.posts).toHaveLength(1)
    })
    expect(capture.posts[0]?.body).toMatchObject({
      title: 'Release notes',
      publish_now: true,
    })
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).toBeNull()
    })
  })

  it('keeps the form and nothing else when publish_now rolls back with 422', async () => {
    const capture = install({
      writeOverride: (request) =>
        request.method === 'POST'
          ? new HttpResponse(
              JSON.stringify({
                type: 'https://newtg.dev/problems/validation_failed',
                title: 'Validation failed',
                status: 422,
                fields: ['content'],
              }),
              {
                status: 422,
                headers: { 'Content-Type': 'application/problem+json' },
              },
            )
          : undefined,
    })
    const user = userEvent.setup()
    await renderPage()
    const listCallsBefore = capture.listUrls.length

    await openCreate(user)
    await fillRequired(user, 'Will roll back', 'Still invalid.')
    await user.click(screen.getByRole('button', { name: 'Save and publish' }))
    await user.click(
      within(await screen.findByRole('alertdialog')).getByRole('button', {
        name: 'Publish',
      }),
    )

    expect(
      await screen.findByText('Saving and publishing failed. No changes were saved.'),
    ).toBeTruthy()
    // 抽屉仍打开且内容保留；列表没有因失败被刷新（不留草稿行）。
    expect(screen.getByRole('dialog')).toBeTruthy()
    expect((screen.getByLabelText('Title') as HTMLInputElement).value).toBe(
      'Will roll back',
    )
    expect(capture.listUrls.length).toBe(listCallsBefore)
  })

  it('edits a published announcement in place with a read-only effective time', async () => {
    const capture = install()
    const user = userEvent.setup()
    await renderPage()

    await user.click(
      screen.getByRole('button', { name: 'Edit Live maintenance' }),
    )
    const sheet = await screen.findByRole('dialog')
    await within(sheet).findByText(/Saving updates it in place/)

    // N-35：不提供可编辑的生效时间输入，只展示服务端值。
    expect(screen.getByTestId('announcement-publish-at-locked')).toBeTruthy()
    expect(screen.queryByLabelText('Takes effect')).toBeNull()

    const title = screen.getByLabelText('Title')
    await user.clear(title)
    await user.type(title, 'Live maintenance v2')
    await user.click(screen.getByRole('button', { name: 'Save changes' }))

    await waitFor(() => {
      expect(capture.puts).toHaveLength(1)
    })
    const body = capture.puts[0]?.body
    expect(body).toMatchObject({
      title: 'Live maintenance v2',
      publish_now: false,
    })
    // 生效时间被忽略（既不出现在请求里，也不会被服务端重写）。
    expect(body).not.toHaveProperty('publish_at')
  })

  it('shows the saved values when the same row is reopened', async () => {
    const capture = install()
    const user = userEvent.setup()
    await renderPage()

    await user.click(
      screen.getByRole('button', { name: 'Edit Live maintenance' }),
    )
    await within(await screen.findByRole('dialog')).findByText(/Saving updates it in place/)
    const title = screen.getByLabelText('Title')
    await user.clear(title)
    await user.type(title, 'Live maintenance v2')
    await user.click(screen.getByRole('button', { name: 'Save changes' }))
    await waitFor(() => {
      expect(capture.puts).toHaveLength(1)
    })
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).toBeNull()
    })

    // 重开同一行：详情缓存必须已被服务端返回值覆盖（staleTime: Infinity
    // 不会自动重取），否则这里仍是编辑前的标题。
    await user.click(
      screen.getByRole('button', { name: 'Edit Live maintenance v2' }),
    )
    await within(await screen.findByRole('dialog')).findByText(/Saving updates it in place/)
    expect((screen.getByLabelText('Title') as HTMLInputElement).value).toBe(
      'Live maintenance v2',
    )
  })

  it('explains why writes are disabled when the connection drops mid-edit', async () => {
    install()
    const user = userEvent.setup()
    await renderPage()
    await openCreate(user)

    window.dispatchEvent(new Event('offline'))

    await waitFor(() => {
      expect(screen.getByRole('dialog').textContent).toContain(
        'You are offline. Publishing changes is disabled until the connection returns.',
      )
    })
  })

  it('links the content error and focuses the first invalid field', async () => {
    const capture = install()
    const user = userEvent.setup()
    await renderPage()

    await openCreate(user)
    await user.type(screen.getByLabelText('Title'), 'Title without content')
    await user.click(screen.getByRole('button', { name: 'Save draft' }))

    const textarea = screen.getByLabelText(
      'Content (Markdown)',
    ) as HTMLTextAreaElement
    await waitFor(() => {
      expect(document.activeElement).toBe(textarea)
    })
    const errorId = textarea.getAttribute('aria-describedby')
    expect(errorId).toBeTruthy()
    expect(document.getElementById(errorId as string)?.textContent).toContain(
      'Enter the announcement content.',
    )
    await waitFor(() => {
      expect(capture.posts).toHaveLength(0)
    })
  })

  it('returns an offline announcement to draft when saved', async () => {
    const capture = install()
    const user = userEvent.setup()
    await renderPage()

    await user.click(
      screen.getByRole('button', { name: 'Edit Pulled notice' }),
    )
    const sheet = await screen.findByRole('dialog')
    await within(sheet).findByText(/returns it to draft/)

    await user.click(screen.getByRole('button', { name: 'Save draft' }))
    await waitFor(() => {
      expect(capture.puts).toHaveLength(1)
    })
    expect(capture.puts[0]?.body).toMatchObject({ publish_now: false })
    await waitFor(() => {
      const row = screen
        .getAllByTestId('announcement-row')
        .find((entry) => entry.textContent?.includes('Pulled notice'))
      expect(
        within(row as HTMLElement).getByTestId('status-badge').textContent,
      ).toContain('Draft')
    })
  })

  it('shows a 429 inline and preserves the form for a retry', async () => {
    install({
      writeOverride: (request) =>
        request.method === 'POST'
          ? new HttpResponse(
              JSON.stringify({ title: 'Too many requests' }),
              {
                status: 429,
                headers: {
                  'Content-Type': 'application/problem+json',
                  'Retry-After': '5',
                },
              },
            )
          : undefined,
    })
    const user = userEvent.setup()
    await renderPage()

    await openCreate(user)
    await fillRequired(user, 'Rate limited', 'Keep me.')
    await user.click(screen.getByRole('button', { name: 'Save draft' }))

    expect(await screen.findByText(/Try again in 5 s\./)).toBeTruthy()
    expect((screen.getByLabelText('Title') as HTMLInputElement).value).toBe(
      'Rate limited',
    )
  })
})
