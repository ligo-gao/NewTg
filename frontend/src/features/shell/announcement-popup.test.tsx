import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { fireEvent, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import type { ActiveAnnouncement } from '@/features/announcements/api'
import { renderWithProviders } from '@/test-support/providers'

import { AnnouncementPopup } from './announcement-popup'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  server.resetHandlers()
  sessionStorage.clear()
})
afterAll(() => server.close())

function active(overrides: Partial<ActiveAnnouncement> = {}): ActiveAnnouncement {
  return {
    id: '11111111-1111-4111-8111-111111111111',
    title: 'Scheduled maintenance',
    category: 'maintenance',
    publish_at: '2026-10-20T08:00:00Z',
    expire_at: null,
    content_markdown: '**Heads up**',
    content_html: '<p><strong>Heads up</strong></p>',
    read: false,
    ...overrides,
  }
}

interface Capture {
  activeCalls: number
  reads: string[]
}

function install(items: ActiveAnnouncement[], options: { activeStatus?: number } = {}): Capture {
  const capture: Capture = { activeCalls: 0, reads: [] }
  server.use(
    http.get(`${ORIGIN}/api/announcements/active`, () => {
      capture.activeCalls += 1
      if (options.activeStatus !== undefined) {
        return new HttpResponse(
          JSON.stringify({ title: 'Unavailable' }),
          {
            status: options.activeStatus,
            headers: { 'Content-Type': 'application/problem+json' },
          },
        )
      }
      return HttpResponse.json({ items })
    }),
    http.post(`${ORIGIN}/api/announcements/:id/read`, ({ params }) => {
      capture.reads.push(String(params.id))
      return new HttpResponse(null, { status: 204 })
    }),
  )
  return capture
}

describe('announcement popup (console shell)', () => {
  it('shows an unread announcement once and records the read on confirmation', async () => {
    const capture = install([active()])
    const user = userEvent.setup()
    await renderWithProviders(<AnnouncementPopup />, ['common'])

    const dialog = await screen.findByRole('alertdialog')
    expect(dialog.getAttribute('aria-modal')).toBe('true')
    expect(dialog.textContent).toContain('Scheduled maintenance')
    // 弹层渲染服务端净化后的 HTML，而不是 Markdown 原文。
    const content = screen.getByTestId('announcement-popup-content')
    expect(content.innerHTML).toBe('<p><strong>Heads up</strong></p>')
    expect(dialog.textContent).toContain('Maintenance')

    // 公告不允许用 Escape 静默消失。
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(screen.queryByRole('alertdialog')).toBeTruthy()

    await user.click(screen.getByRole('button', { name: 'Got it' }))
    await waitFor(() => {
      expect(capture.reads).toEqual(['11111111-1111-4111-8111-111111111111'])
    })
    await waitFor(() => {
      expect(screen.queryByRole('alertdialog')).toBeNull()
    })
  })

  it('queues multiple unread announcements one at a time', async () => {
    const capture = install([
      active(),
      active({ id: '22222222-2222-4222-8222-222222222222', title: 'Release 2', category: 'release', read: false }),
    ])
    const user = userEvent.setup()
    await renderWithProviders(<AnnouncementPopup />, ['common'])

    expect((await screen.findByRole('alertdialog')).textContent).toContain(
      'Scheduled maintenance',
    )
    await user.click(screen.getByRole('button', { name: 'Got it' }))
    await waitFor(() => {
      expect(screen.getByRole('alertdialog').textContent).toContain('Release 2')
    })
    await user.click(screen.getByRole('button', { name: 'Got it' }))
    await waitFor(() => {
      expect(capture.reads).toHaveLength(2)
      expect(screen.queryByRole('alertdialog')).toBeNull()
    })
  })

  it('does not show announcements that are already read', async () => {
    const capture = install([active({ read: true })])
    await renderWithProviders(<AnnouncementPopup />, ['common'])

    await waitFor(() => {
      expect(capture.activeCalls).toBe(1)
    })
    expect(screen.queryByRole('alertdialog')).toBeNull()
    expect(capture.reads).toHaveLength(0)
  })

  it('renders nothing and sends no read when there are no announcements', async () => {
    const capture = install([])
    await renderWithProviders(<AnnouncementPopup />, ['common'])

    await waitFor(() => {
      expect(capture.activeCalls).toBe(1)
    })
    expect(screen.queryByRole('alertdialog')).toBeNull()
    expect(capture.reads).toHaveLength(0)
  })

  it('remembers a dismissed announcement for the rest of the session', async () => {
    // 服务端仍报告未读（模拟标记已读请求尚未落库），本地会话记忆必须
    // 阻止同一个公告在组件重挂载后再弹一次。
    install([active()])
    const user = userEvent.setup()
    const first = await renderWithProviders(<AnnouncementPopup />, ['common'])
    await user.click(
      await screen.findByRole('button', { name: 'Got it' }),
    )
    first.unmount()

    await renderWithProviders(<AnnouncementPopup />, ['common'])
    await waitFor(() => {
      expect(screen.queryByRole('alertdialog')).toBeNull()
    })
  })

  it('does not retry a failing active request', async () => {
    const capture = install([], { activeStatus: 500 })
    await renderWithProviders(<AnnouncementPopup />, ['common'])

    await waitFor(() => {
      expect(capture.activeCalls).toBe(1)
    })
    expect(screen.queryByRole('alertdialog')).toBeNull()
  })
})
