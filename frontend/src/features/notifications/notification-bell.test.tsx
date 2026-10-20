import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import type { UserNotification } from '@/features/notifications/api'
import { ConsoleHeader } from '@/routes/_console'
import { renderWithProviders } from '@/test-support/providers'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function notification(
  overrides: Partial<UserNotification> = {},
): UserNotification {
  return {
    id: 1,
    category: 'system.announcement',
    template_key: 'system.announcement',
    title: 'Scheduled maintenance',
    summary: 'The gateway will restart.',
    body: 'Maintenance window: **02:00 UTC**',
    created_at: '2026-10-20T08:00:00Z',
    read_at: null,
    ...overrides,
  }
}

interface RequestLog {
  listCalls: Array<{ page: number; pageSize: number }>
  readIds: number[]
}

interface InstallOptions {
  items?: UserNotification[]
  listStatus?: number
  /** `null` omits the `Retry-After` header entirely. */
  retryAfter?: number | null
  offline?: boolean
  readStatus?: number
}

function install(options: InstallOptions = {}): RequestLog {
  const state = {
    items: options.items ?? [],
  }
  const log: RequestLog = { listCalls: [], readIds: [] }

  server.use(
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({
        username: 'alice',
        role: 'user',
        status: 'enabled',
        permissions: [],
      }),
    ),
    http.get(`${ORIGIN}/api/notifications`, ({ request }) => {
      const url = new URL(request.url)
      const page = Number(url.searchParams.get('p') ?? '1')
      const pageSize = Number(url.searchParams.get('page_size') ?? '20')
      log.listCalls.push({ page, pageSize })
      if (options.offline === true) return HttpResponse.error()
      if (options.listStatus !== undefined) {
        const headers: Record<string, string> = {
          'Content-Type': 'application/problem+json',
        }
        const retryAfter =
          options.retryAfter === undefined ? 42 : options.retryAfter
        if (retryAfter !== null) headers['Retry-After'] = String(retryAfter)
        return new HttpResponse(
          JSON.stringify({
            type: 'about:blank',
            title: 'Too Many Requests',
            status: options.listStatus,
          }),
          { status: options.listStatus, headers },
        )
      }
      const start = (page - 1) * pageSize
      return HttpResponse.json({
        items: state.items.slice(start, start + pageSize),
        total: state.items.length,
        page,
        page_size: pageSize,
      })
    }),
    http.post(`${ORIGIN}/api/notifications/:id/read`, ({ params }) => {
      const id = Number(params.id)
      log.readIds.push(id)
      if (options.readStatus !== undefined) {
        return new HttpResponse(null, { status: options.readStatus })
      }
      state.items = state.items.map((item) =>
        item.id === id
          ? { ...item, read_at: '2026-10-20T09:00:00Z' }
          : item,
      )
      return new HttpResponse(null, { status: 204 })
    }),
  )
  return log
}

async function openPanel(): Promise<HTMLElement> {
  const user = userEvent.setup()
  const bell = await screen.findByRole('button', { name: /^Notifications/ })
  await user.click(bell)
  return screen.findByRole('dialog')
}

describe('console notification bell', () => {
  it('shows the unread badge and a live-region count', async () => {
    install({
      items: [
        notification({ id: 1 }),
        notification({ id: 2, title: 'Release 4' }),
        notification({
          id: 3,
          title: 'Already read',
          read_at: '2026-10-19T00:00:00Z',
        }),
      ],
    })

    await renderWithProviders(<ConsoleHeader />, ['common'])

    const bell = await screen.findByRole('button', {
      name: 'Notifications (2 unread)',
    })
    expect(bell.textContent).toContain('2')
    expect(screen.getByRole('status').textContent).toBe(
      '2 unread notifications',
    )
  })

  it('renders an empty state when there are no notifications', async () => {
    install({ items: [] })
    await renderWithProviders(<ConsoleHeader />, ['common'])

    const dialog = await openPanel()
    expect(within(dialog).getByText('You have no notifications.')).toBeTruthy()
  })

  it('lists notifications with title and summary, never the Markdown body', async () => {
    const items = Array.from({ length: 22 }, (_, index) =>
      notification({
        id: index + 1,
        title: `Notice ${index + 1}`,
        summary: `Summary ${index + 1}`,
        body: `Read **this** and [view details](/announcements/${index + 1})`,
      }),
    )
    const log = install({ items })
    await renderWithProviders(<ConsoleHeader />, ['common'])

    const dialog = await openPanel()
    expect(within(dialog).getByText('Notice 1')).toBeTruthy()
    expect(within(dialog).getByText('Summary 1')).toBeTruthy()
    // body 含 Markdown 字面量，面板只展示 title + summary，绝不外泄标记。
    expect(within(dialog).queryByText(/\*\*this\*\*/)).toBeNull()
    expect(within(dialog).queryByText(/\[view details\]/)).toBeNull()
    // 第一页只取 20 条，第 21 条在第二页。
    expect(within(dialog).queryByText('Notice 21')).toBeNull()
    expect(within(dialog).getByText('Page 1 of 2')).toBeTruthy()
    // 顶栏的未读探测（100 条）先于面板列表（20 条）发出。
    expect(log.listCalls).toContainEqual({ page: 1, pageSize: 20 })

    const user = userEvent.setup()
    await user.click(within(dialog).getByRole('button', { name: 'Next' }))

    expect(await within(dialog).findByText('Notice 21')).toBeTruthy()
    await waitFor(() => {
      expect(log.listCalls).toContainEqual({ page: 2, pageSize: 20 })
    })
    expect(within(dialog).getByText('Page 2 of 2')).toBeTruthy()
  })

  it('marks an item read, decrements the badge and follows link_url', async () => {
    const log = install({
      items: [
        notification({
          id: 11,
          title: 'Read me',
          link_url: 'https://example.com/notice/11',
        }),
        notification({ id: 12, title: 'Still unread' }),
      ],
    })
    const onNavigate = vi.fn()
    await renderWithProviders(
      <ConsoleHeader onNotificationNavigate={onNavigate} />,
      ['common'],
    )

    const dialog = await openPanel()
    const user = userEvent.setup()
    await user.click(within(dialog).getByText('Read me'))

    await waitFor(() => {
      expect(log.readIds).toEqual([11])
    })
    await waitFor(() => {
      expect(onNavigate).toHaveBeenCalledWith('https://example.com/notice/11')
    })
    // 幂等端点成功后，本地缓存把该条置为已读，徽章随之递减。
    await screen.findByRole('button', {
      name: 'Notifications (1 unread)',
    })
    expect(screen.getByRole('status').textContent).toBe(
      '1 unread notification',
    )
  })

  it('uses singular copy for exactly one unread notification', async () => {
    install({ items: [notification({ id: 1 })] })
    await renderWithProviders(<ConsoleHeader />, ['common'])

    const bell = await screen.findByRole('button', {
      name: 'Notifications (1 unread)',
    })
    expect(bell.textContent).toContain('1')
    expect(screen.getByRole('status').textContent).toBe(
      '1 unread notification',
    )

    const dialog = await openPanel()
    // 总数文案同样用单数，避免「1 notifications」。
    expect(await within(dialog).findByText('1 notification')).toBeTruthy()
  })

  it('caps both the badge and the live-region count at 99+', async () => {
    install({
      items: Array.from({ length: 100 }, (_, index) =>
        notification({ id: index + 1, title: `Notice ${index + 1}` }),
      ),
    })
    await renderWithProviders(<ConsoleHeader />, ['common'])

    const bell = await screen.findByRole('button', {
      name: 'Notifications (99+ unread)',
    })
    expect(bell.textContent).toContain('99+')
    // aria-live 与徽章同口径：不播报 100，因为真实未读数可能更多。
    expect(screen.getByRole('status').textContent).toBe(
      '99+ unread notifications',
    )
  })

  it('keeps the panel open and shows an inline error when marking read fails', async () => {
    const log = install({
      items: [
        notification({
          id: 51,
          title: 'Cannot mark me',
          link_url: 'https://example.com/notice/51',
        }),
      ],
      readStatus: 500,
    })
    const onNavigate = vi.fn()
    await renderWithProviders(
      <ConsoleHeader onNotificationNavigate={onNavigate} />,
      ['common'],
    )

    const dialog = await openPanel()
    const user = userEvent.setup()
    await user.click(within(dialog).getByText('Cannot mark me'))

    await waitFor(() => {
      expect(log.readIds).toEqual([51])
    })
    // 失败必须可见：面板保留、行内错误出现、不跳转。
    expect(screen.getByRole('dialog')).toBeTruthy()
    expect(
      within(dialog).getByText('Could not mark the notification as read.'),
    ).toBeTruthy()
    expect(onNavigate).not.toHaveBeenCalled()
  })

  it('does not navigate when there is no usable target', async () => {
    install({
      items: [
        notification({ id: 42, title: 'No target', link_url: undefined }),
      ],
    })
    const onNavigate = vi.fn()
    await renderWithProviders(
      <ConsoleHeader onNotificationNavigate={onNavigate} />,
      ['common'],
    )

    const dialog = await openPanel()
    const user = userEvent.setup()
    await user.click(within(dialog).getByText('No target'))

    // 无 announcement_id、无白名单 link_url：不凭空拼路径，也不关闭面板。
    await waitFor(() => {
      expect(screen.getByRole('dialog')).toBeTruthy()
    })
    expect(onNavigate).not.toHaveBeenCalled()
  })

  it('rejects a backslash protocol-relative link_url', async () => {
    install({
      items: [
        notification({
          id: 44,
          title: 'Backslash target',
          // `/\evil.example` 会被浏览器归一成 `//evil.example`（站外）。
          link_url: '/\\evil.example/path',
        }),
      ],
    })
    const onNavigate = vi.fn()
    await renderWithProviders(
      <ConsoleHeader onNotificationNavigate={onNavigate} />,
      ['common'],
    )

    const dialog = await openPanel()
    const user = userEvent.setup()
    await user.click(within(dialog).getByText('Backslash target'))

    await waitFor(() => {
      expect(screen.getByRole('dialog')).toBeTruthy()
    })
    expect(onNavigate).not.toHaveBeenCalled()
  })

  it('prefers payload announcement_id over link_url for the detail deep link', async () => {
    install({
      items: [
        notification({
          id: 43,
          title: 'Announcement deep link',
          announcement_id: '0b6f3b1e-2d3c-4f0a-9a6d-1f2e3d4c5b6a',
          // 发布时刻的快照可能指向旧路径；公告归因键必须优先。
          link_url: '/legacy/notice',
        }),
      ],
    })
    const onNavigate = vi.fn()
    await renderWithProviders(
      <ConsoleHeader onNotificationNavigate={onNavigate} />,
      ['common'],
    )

    const dialog = await openPanel()
    const user = userEvent.setup()
    await user.click(within(dialog).getByText('Announcement deep link'))

    await waitFor(() => {
      expect(onNavigate).toHaveBeenCalledWith(
        '/announcements/0b6f3b1e-2d3c-4f0a-9a6d-1f2e3d4c5b6a',
      )
    })
  })

  it('supports marking every unread notification as read', async () => {
    const log = install({
      items: [
        notification({ id: 21, title: 'One' }),
        notification({ id: 22, title: 'Two' }),
      ],
    })
    await renderWithProviders(<ConsoleHeader />, ['common'])

    const dialog = await openPanel()
    const user = userEvent.setup()
    await user.click(
      within(dialog).getByRole('button', { name: 'Mark all as read' }),
    )

    await waitFor(() => {
      expect(log.readIds.sort((a, b) => a - b)).toEqual([21, 22])
    })
    // 面板打开时顶栏被 Sheet 置为 inert；先关闭再读铃铛的可访问名称。
    await user.click(within(dialog).getByRole('button', { name: 'Close' }))
    await screen.findByRole('button', { name: 'Notifications' })
    expect(screen.getByRole('status').textContent).toBe(
      'No unread notifications',
    )
  })

  it('counts down the server Retry-After and re-enables retry when it expires', async () => {
    install({ listStatus: 429, retryAfter: 1 })
    await renderWithProviders(<ConsoleHeader />, ['common'])

    const dialog = await openPanel()
    expect(
      await within(dialog).findByText('Too many requests. Try again in 1s.'),
    ).toBeTruthy()
    const retry = within(dialog).getByRole('button', { name: 'Try again' })
    expect(retry).toHaveProperty('disabled', true)

    // 到期自动恢复：按钮可点，文案不再声称还需等待。
    await waitFor(
      () => {
        expect(retry).toHaveProperty('disabled', false)
      },
      { timeout: 4000 },
    )
    expect(
      within(dialog).getByText('Too many requests. Try again.'),
    ).toBeTruthy()
  })

  it('allows manual retry when the 429 has no Retry-After header', async () => {
    install({ listStatus: 429, retryAfter: null })
    await renderWithProviders(<ConsoleHeader />, ['common'])

    const dialog = await openPanel()
    expect(
      await within(dialog).findByText('Too many requests. Try again.'),
    ).toBeTruthy()
    // 缺失时不发明时长、也不倒计时：立即允许手动重试。
    expect(
      within(dialog).getByRole('button', { name: 'Try again' }),
    ).toHaveProperty('disabled', false)
  })

  it('disables "mark all as read" when nothing is unread', async () => {
    install({
      items: [
        notification({
          id: 61,
          title: 'Already read',
          read_at: '2026-10-19T00:00:00Z',
        }),
      ],
    })
    await renderWithProviders(<ConsoleHeader />, ['common'])

    const dialog = await openPanel()
    expect(within(dialog).getByText('Already read')).toBeTruthy()
    await waitFor(() => {
      expect(
        within(dialog).getByRole('button', { name: 'Mark all as read' }),
      ).toHaveProperty('disabled', true)
    })
  })

  it('offers a retry when the network is offline', async () => {
    install({ offline: true })
    await renderWithProviders(<ConsoleHeader />, ['common'])

    const dialog = await openPanel()
    expect(
      within(dialog).getByText(
        'You are offline. Reconnect and try again.',
      ),
    ).toBeTruthy()
    expect(
      within(dialog).getByRole('button', { name: 'Try again' }),
    ).toHaveProperty('disabled', false)
  })
})
