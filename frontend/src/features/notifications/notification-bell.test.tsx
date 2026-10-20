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
        return new HttpResponse(
          JSON.stringify({
            type: 'about:blank',
            title: 'Too Many Requests',
            status: options.listStatus,
          }),
          {
            status: options.listStatus,
            headers: {
              'Content-Type': 'application/problem+json',
              'Retry-After': '42',
            },
          },
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

  it('lists notifications with title, summary, body and paginates', async () => {
    const items = Array.from({ length: 22 }, (_, index) =>
      notification({
        id: index + 1,
        title: `Notice ${index + 1}`,
        summary: `Summary ${index + 1}`,
        body: `Body ${index + 1}`,
      }),
    )
    const log = install({ items })
    await renderWithProviders(<ConsoleHeader />, ['common'])

    const dialog = await openPanel()
    expect(within(dialog).getByText('Notice 1')).toBeTruthy()
    expect(within(dialog).getByText('Summary 1')).toBeTruthy()
    expect(within(dialog).getByText('Body 1')).toBeTruthy()
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
      '1 unread notifications',
    )
  })

  it('falls back to /announcements/{id} when link_url is absent', async () => {
    install({
      items: [notification({ id: 42, title: 'Announcement', link_url: undefined })],
    })
    const onNavigate = vi.fn()
    await renderWithProviders(
      <ConsoleHeader onNotificationNavigate={onNavigate} />,
      ['common'],
    )

    const dialog = await openPanel()
    const user = userEvent.setup()
    await user.click(within(dialog).getByText('Announcement'))

    await waitFor(() => {
      expect(onNavigate).toHaveBeenCalledWith('/announcements/42')
    })
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

  it('shows the server-driven Retry-After on 429', async () => {
    install({ listStatus: 429 })
    await renderWithProviders(<ConsoleHeader />, ['common'])

    const dialog = await openPanel()
    expect(
      within(dialog).getByText('Too many requests. Try again in 42s.'),
    ).toBeTruthy()
    expect(
      within(dialog).getByRole('button', { name: 'Try again' }),
    ).toHaveProperty('disabled', true)
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
