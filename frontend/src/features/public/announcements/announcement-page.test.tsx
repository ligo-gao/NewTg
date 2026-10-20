import { fireEvent, waitFor } from '@testing-library/react'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { AnnouncementPage } from './announcement-page'

const ORIGIN = 'http://localhost:3000'
const ENDPOINT = `${ORIGIN}/api/announcements/public/:id`

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const ANNOUNCEMENT = {
  id: '0b6f3b1e-2d3c-4f0a-9a6d-1f2e3d4c5b6a',
  title: 'Scheduled maintenance',
  category: 'maintenance',
  content_html:
    '<h2>Maintenance window</h2><p>The gateway restarts at <strong>02:00 UTC</strong>.</p>',
  published_at: '2026-10-01T10:00:00Z',
  expire_at: null,
}

function announce(fixture: Record<string, unknown> = ANNOUNCEMENT) {
  return http.get(ENDPOINT, () => HttpResponse.json(fixture))
}

function problem(status: number, title: string, headers?: Record<string, string>) {
  return HttpResponse.json(
    { type: 'about:blank', title },
    { status, headers: { 'Content-Type': 'application/problem+json', ...headers } },
  )
}

describe('AnnouncementPage (F-08-04 public detail)', () => {
  it('renders the title, server-sanitized HTML, category and published time', async () => {
    server.use(announce())

    const { findByRole, getByText, container } = await renderWithProviders(
      <AnnouncementPage id={ANNOUNCEMENT.id} />,
      ['public'],
    )

    const heading = await findByRole('heading', {
      level: 1,
      name: 'Scheduled maintenance',
    })
    expect(heading).toBeTruthy()

    // 正文按 HTML 渲染（服务端已净化），而不是显示标签源码。
    expect(getByText('Maintenance window')).toBeTruthy()
    expect(container.textContent).toContain('The gateway restarts at 02:00 UTC.')
    expect(container.querySelector('strong')?.textContent).toBe('02:00 UTC')

    // 分类标签带可访问名称，时间使用语义化 <time datetime>。
    expect(getByText('Maintenance')).toBeTruthy()
    const time = container.querySelector('time')
    expect(time?.getAttribute('datetime')).toBe('2026-10-01T10:00:00Z')

    // 页面标题使用公告标题（SEO）。
    await waitFor(() => {
      expect(document.title).toBe('Scheduled maintenance')
    })
    expect(
      document.head.querySelector('meta[name="description"]')?.getAttribute('content'),
    ).toContain('The gateway restarts at')
  })

  it('renders a 404 empty state with home and pricing escape links', async () => {
    server.use(http.get(ENDPOINT, () => problem(404, 'Not Found')))

    const { findByRole, getByRole } = await renderWithProviders(
      <AnnouncementPage id="missing" />,
      ['public'],
    )

    expect(
      await findByRole('heading', { level: 1, name: 'Announcement not found' }),
    ).toBeTruthy()
    expect(getByRole('link', { name: 'Back to home' }).getAttribute('href')).toBe('/')
    expect(getByRole('link', { name: 'View pricing' }).getAttribute('href')).toBe(
      '/pricing',
    )
  })

  it('starts a Retry-After countdown on 429 and disables retry until it ends', async () => {
    server.use(
      http.get(ENDPOINT, () =>
        problem(429, 'Too Many Requests', { 'Retry-After': '30' }),
      ),
    )

    const { findByText, getByRole } = await renderWithProviders(
      <AnnouncementPage id={ANNOUNCEMENT.id} />,
      ['public'],
    )

    // 倒计时取服务端 Retry-After 的原值，不写死默认时长。
    expect(await findByText('Too many requests. Try again in 30 s.')).toBeTruthy()
    const retry = getByRole('button', { name: 'Retry' })
    expect((retry as HTMLButtonElement).disabled).toBe(true)
  })

  it('shows the offline message and a retry action on a transport failure', async () => {
    server.use(http.get(ENDPOINT, () => HttpResponse.error()))

    const { findByText, getByRole } = await renderWithProviders(
      <AnnouncementPage id={ANNOUNCEMENT.id} />,
      ['public'],
    )

    expect(
      await findByText('You are offline. Check your connection and try again.'),
    ).toBeTruthy()
    expect(getByRole('button', { name: 'Retry' })).toBeTruthy()
  })

  it('refetches from the inline retry action after a server error', async () => {
    let fail = true
    server.use(
      http.get(ENDPOINT, () =>
        fail ? problem(503, 'Service Unavailable') : HttpResponse.json(ANNOUNCEMENT),
      ),
    )

    const { findByText, findByRole, getByRole } = await renderWithProviders(
      <AnnouncementPage id={ANNOUNCEMENT.id} />,
      ['public'],
    )

    expect(await findByText('Announcement unavailable')).toBeTruthy()
    expect(await findByText('Failed to load this announcement.')).toBeTruthy()

    fail = false
    fireEvent.click(getByRole('button', { name: 'Retry' }))

    expect(
      await findByRole('heading', { level: 1, name: 'Scheduled maintenance' }),
    ).toBeTruthy()
  })

  it('degrades an empty title and empty content to localized placeholders', async () => {
    server.use(announce({ ...ANNOUNCEMENT, title: '', content_html: '' }))

    const { findByRole, findByText } = await renderWithProviders(
      <AnnouncementPage id={ANNOUNCEMENT.id} />,
      ['public'],
    )

    expect(
      await findByRole('heading', { level: 1, name: 'Untitled announcement' }),
    ).toBeTruthy()
    expect(await findByText('This announcement has no content.')).toBeTruthy()
  })

  it('marks an announcement whose expire_at has passed as ended without refetching', async () => {
    server.use(announce({ ...ANNOUNCEMENT, expire_at: '2020-01-01T00:00:00Z' }))

    const { findByRole, findByText } = await renderWithProviders(
      <AnnouncementPage id={ANNOUNCEMENT.id} />,
      ['public'],
    )

    expect(await findByRole('heading', { level: 1 })).toBeTruthy()
    expect(await findByText('This announcement has ended')).toBeTruthy()
  })

  it('restores the previous document title when the route unmounts', async () => {
    server.use(announce())
    document.title = 'Before announcement'

    const { findByRole, unmount } = await renderWithProviders(
      <AnnouncementPage id={ANNOUNCEMENT.id} />,
      ['public'],
    )
    await findByRole('heading', { level: 1, name: 'Scheduled maintenance' })
    await waitFor(() => {
      expect(document.title).toBe('Scheduled maintenance')
    })

    unmount()

    // 切走后不得残留公告标题；恢复为进入本页之前的标题。
    await waitFor(() => {
      expect(document.title).toBe('Before announcement')
    })
  })
})
