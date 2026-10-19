import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { useState } from 'react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'
import { ToastViewport, useToastStore } from '@/shared/ui/toast'

import { NotificationTemplatesPage } from './notification-templates-page'
import { NOTIFICATION_CATEGORIES } from './registry'
import type { NotificationTemplatesSearch } from './search'
import type { NotificationDelivery, NotificationTemplate } from './types'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const ME_ADMIN = {
  username: 'admin',
  email: 'admin@example.com',
  role: 'admin',
  status: 'enabled',
  permissions: ['*'],
}

function template(
  overrides: Partial<NotificationTemplate> & Pick<NotificationTemplate, 'key' | 'channel' | 'locale'>,
): NotificationTemplate {
  return {
    category: overrides.key,
    subject: 'Subject text',
    body: 'Body text',
    enabled: true,
    customized: false,
    updated_at: '2026-10-19T10:00:00Z',
    ...overrides,
  }
}

const ROWS: NotificationTemplate[] = [
  template({
    key: 'billing.balance_low',
    channel: 'email',
    locale: 'en',
    subject: 'Low balance',
    body: 'Your balance is {{.balance}}.',
    customized: true,
    updated_at: '2026-10-19T08:00:00Z',
  }),
  template({ key: 'billing.balance_low', channel: 'email', locale: 'fr', body: 'French body' }),
  template({ key: 'billing.balance_low', channel: 'inapp', locale: 'en', subject: '', body: 'Balance low' }),
  template({ key: 'billing.balance_low', channel: 'inapp', locale: 'zh-CN', subject: '', body: 'Balance low' }),
  template({ key: 'billing.quota_exhausted', channel: 'email', locale: 'en', body: 'Quota exhausted' }),
  template({ key: 'auth.verify_code', channel: 'email', locale: 'en', body: 'Code {{.code}}' }),
  template({ key: 'system.announcement', channel: 'inapp', locale: 'en', subject: '', body: 'Announcement' }),
  template({ key: 'ops.redemption_expiring', channel: 'email', locale: 'fr', body: 'Codes expiring', enabled: false }),
]

const DELIVERIES: NotificationDelivery[] = [
  {
    id: 1,
    category: 'billing.balance_low',
    channel: 'email',
    recipient: 'alice@example.com',
    locale: 'en',
    status: 'sent',
    created_at: '2026-10-19T09:00:00Z',
  },
  {
    id: 2,
    category: 'ops.channel_auto_disabled',
    channel: 'email',
    recipient: 'root@example.com',
    locale: 'en',
    status: 'failed',
    error: 'connection refused',
    created_at: '2026-10-19T08:00:00Z',
  },
  {
    id: 3,
    category: 'billing.balance_low',
    channel: 'webhook',
    recipient: 'https://hooks.example.com/x',
    locale: 'en',
    status: 'bounced',
    created_at: '2026-10-19T07:00:00Z',
  },
]

interface PutLog {
  key: string
  body: Record<string, unknown>
}

function install(rows: NotificationTemplate[] = ROWS, deliveries: NotificationDelivery[] = DELIVERIES) {
  const puts: PutLog[] = []
  const detailRequests: URLSearchParams[] = []
  const testRequests: Record<string, unknown>[] = []

  server.use(
    http.get(`${ORIGIN}/api/me`, () => HttpResponse.json(ME_ADMIN)),
    http.get(`${ORIGIN}/api/settings`, ({ request }) => {
      const section = new URL(request.url).searchParams.get('section') ?? ''
      return HttpResponse.json({
        section,
        updated_at: '2026-10-19T10:00:00Z',
        items: [
          { key: 'notify.smtp.host', value: 'smtp.example.com', type: 'string', default: '', public: false, secret: false, dangerous: false },
          { key: 'notify.smtp.from_email', value: 'no-reply@example.com', type: 'string', default: '', public: false, secret: false, dangerous: false },
        ],
      })
    }),
    http.get(`${ORIGIN}/api/notification-templates`, ({ request }) => {
      const query = new URL(request.url).searchParams
      let items = rows
      const channel = query.get('channel')
      const locale = query.get('locale')
      if (channel !== null) items = items.filter((row) => row.channel === channel)
      if (locale !== null) items = items.filter((row) => row.locale === locale)
      if (query.get('modified') === 'true') items = items.filter((row) => row.customized)
      return HttpResponse.json({ items })
    }),
    http.get(`${ORIGIN}/api/notification-templates/:key`, ({ request, params }) => {
      const query = new URL(request.url).searchParams
      detailRequests.push(query)
      const row = rows.find(
        (entry) =>
          entry.key === params.key &&
          entry.channel === query.get('channel') &&
          entry.locale === query.get('locale'),
      )
      return row === undefined
        ? HttpResponse.json({ type: 'about:blank', title: 'not found' }, { status: 404 })
        : HttpResponse.json(row)
    }),
    http.put(`${ORIGIN}/api/notification-templates/:key`, async ({ request, params }) => {
      const body = (await request.json()) as Record<string, unknown>
      puts.push({ key: String(params.key), body })
      const row = rows.find(
        (entry) =>
          entry.key === params.key &&
          entry.channel === body.channel &&
          entry.locale === body.locale,
      )
      if (row === undefined) {
        return HttpResponse.json({ type: 'about:blank', title: 'not found' }, { status: 404 })
      }
      return HttpResponse.json({
        ...row,
        subject: body.subject,
        body: body.body,
        cta_text: body.cta_text,
        cta_url: body.cta_url,
        enabled: body.enabled,
        customized: true,
        updated_at: '2026-10-19T12:00:00Z',
      })
    }),
    http.post(`${ORIGIN}/api/notification-templates/:key`, async ({ request, params }) => {
      const rawKey = String(params.key)
      const body = (await request.json()) as Record<string, unknown>
      if (rawKey.endsWith(':preview')) {
        return HttpResponse.json({ subject: 'Rendered subject', body: 'Rendered body' })
      }
      if (rawKey.endsWith(':test')) {
        testRequests.push(body)
        return HttpResponse.json({ id: 1, status: 'sent', error: null }, { status: 202 })
      }
      return HttpResponse.json({ type: 'about:blank', title: 'not found' }, { status: 404 })
    }),
    http.get(`${ORIGIN}/api/notification-deliveries`, ({ request }) => {
      const query = new URL(request.url).searchParams
      let items = deliveries
      const status = query.get('status')
      const channel = query.get('channel')
      const category = query.get('category')
      if (status !== null) items = items.filter((row) => row.status === status)
      if (channel !== null) items = items.filter((row) => row.channel === channel)
      if (category !== null) items = items.filter((row) => row.category === category)
      return HttpResponse.json({
        items,
        total: items.length,
        page: Number(query.get('p') ?? 1),
        page_size: Number(query.get('page_size') ?? 20),
      })
    }),
  )

  return { puts, detailRequests, testRequests }
}

function Harness({ initial }: { initial?: NotificationTemplatesSearch }) {
  const [search, setSearch] = useState<NotificationTemplatesSearch>(initial ?? {})
  return (
    <NotificationTemplatesPage
      search={search}
      onSearchChange={(patch) => {
        setSearch((previous) => {
          const next: Record<string, unknown> = { ...previous, ...patch }
          for (const key of Object.keys(next)) {
            if (next[key] === undefined) delete next[key]
          }
          return next as NotificationTemplatesSearch
        })
      }}
    />
  )
}

async function renderPage(initial?: NotificationTemplatesSearch) {
  const logs = install()
  await renderWithProviders(
    <>
      <Harness initial={initial} />
      <ToastViewport />
    </>,
    ['common', 'settings'],
  )
  return logs
}

function rowOf(key: string, channel: string, locale: string): HTMLElement {
  const channelLabel = channel === 'email' ? 'Email' : 'In-app'
  const rows = screen.getAllByTestId('notification-template-row')
  const found = rows.find((row) => {
    const cells = within(row).getAllByRole('cell')
    return (
      cells[0]?.textContent === key &&
      cells[2]?.textContent === channelLabel &&
      cells[3]?.textContent === locale
    )
  })
  if (found === undefined) throw new Error(`row not found: ${key}/${channel}/${locale}`)
  return found
}

beforeEach(() => {
  server.resetHandlers()
  useToastStore.setState({ toasts: [] })
})

describe('NotificationTemplatesPage', () => {
  it('renders rows, counts, modified/default and locked states', async () => {
    await renderPage()

    expect(await screen.findByText(/8 rows/)).toBeTruthy()
    expect(screen.getByText(/1 modified/)).toBeTruthy()
    expect(screen.getAllByTestId('notification-template-row')).toHaveLength(8)

    const modifiedRow = rowOf('billing.balance_low', 'email', 'en')
    expect(within(modifiedRow).getByText('Modified')).toBeTruthy()
    expect(within(rowOf('auth.verify_code', 'email', 'en')).getByText('Default')).toBeTruthy()
    expect(within(modifiedRow).queryByText('Default')).toBeNull()

    // 锁定模板：无开关，显示锁形文案。
    expect(
      within(rowOf('auth.verify_code', 'email', 'en')).getByText('🔒 Enabled (locked)'),
    ).toBeTruthy()
    expect(within(rowOf('ops.redemption_expiring', 'email', 'fr')).getByText('Disabled')).toBeTruthy()
  })

  it('filters by category multi-select, channel and locale through the URL contract', async () => {
    const user = userEvent.setup()
    await renderPage()
    await screen.findByText(/8 rows/)

    const category = screen.getByLabelText('Category')
    await user.selectOptions(category, ['auth.verify_code', 'system.announcement'])
    await waitFor(() => expect(screen.getByText(/2 rows/)).toBeTruthy())

    await user.deselectOptions(category, ['auth.verify_code', 'system.announcement'])
    await waitFor(() => expect(screen.getByText(/8 rows/)).toBeTruthy())

    await user.selectOptions(screen.getByLabelText('Channel'), 'inapp')
    await waitFor(() => expect(screen.getByText(/3 rows/)).toBeTruthy())

    await user.selectOptions(screen.getByLabelText('Channel'), '')
    await user.selectOptions(screen.getByLabelText('Locale'), 'fr')
    await waitFor(() => expect(screen.getByText(/2 rows/)).toBeTruthy())
  })

  it('saves the edited body with the optimistic token and explicit channel', async () => {
    const user = userEvent.setup()
    const { puts } = await renderPage()
    await screen.findByText(/8 rows/)

    await user.click(within(rowOf('billing.balance_low', 'email', 'en')).getByRole('button', { name: 'Edit' }))
    const dialog = await screen.findByRole('dialog')
    const body = within(dialog).getByLabelText('Body')
    await user.clear(body)
    await user.type(body, 'Updated body')
    await user.click(within(dialog).getByRole('button', { name: 'Save changes' }))

    await waitFor(() => expect(puts).toHaveLength(1))
    expect(puts[0]?.body).toMatchObject({
      locale: 'en',
      channel: 'email',
      body: 'Updated body',
      enabled: true,
      expect_updated_at: '2026-10-19T08:00:00Z',
    })
  })

  it('hides the enabled switch for locked templates', async () => {
    const user = userEvent.setup()
    await renderPage()
    await screen.findByText(/8 rows/)

    await user.click(within(rowOf('auth.verify_code', 'email', 'en')).getByRole('button', { name: 'Edit' }))
    const dialog = await screen.findByRole('dialog')
    expect(await within(dialog).findByText('Locked templates cannot be disabled.')).toBeTruthy()
    expect(within(dialog).queryByRole('switch')).toBeNull()
  })

  it('blocks saving when the template contains an unregistered variable', async () => {
    const user = userEvent.setup()
    await renderPage()
    await screen.findByText(/8 rows/)

    await user.click(within(rowOf('billing.balance_low', 'email', 'en')).getByRole('button', { name: 'Edit' }))
    const dialog = await screen.findByRole('dialog')
    const body = within(dialog).getByLabelText('Body')
    fireEvent.change(body, { target: { value: 'Hello {{.nope}}' } })

    expect(
      await within(dialog).findByText(/unregistered variable: nope/),
    ).toBeTruthy()
    expect((within(dialog).getByRole('button', { name: 'Save changes' }) as HTMLButtonElement).disabled).toBe(true)
  })

  it('guards a locale switch when there are unsaved changes', async () => {
    const user = userEvent.setup()
    const { detailRequests } = await renderPage()
    await screen.findByText(/8 rows/)

    await user.click(within(rowOf('billing.balance_low', 'email', 'en')).getByRole('button', { name: 'Edit' }))
    const dialog = await screen.findByRole('dialog')
    fireEvent.change(within(dialog).getByLabelText('Body'), { target: { value: 'Unsaved' } })
    await user.selectOptions(within(dialog).getByLabelText('Locale'), 'fr')

    expect(await screen.findByText('You have unsaved changes')).toBeTruthy()
    await user.click(screen.getByRole('button', { name: 'Discard and switch' }))

    await waitFor(() =>
      expect(detailRequests.some((query) => query.get('locale') === 'fr')).toBe(true),
    )
    expect(
      await within(screen.getByTestId('notification-template-drawer')).findByDisplayValue(
        'French body',
      ),
    ).toBeTruthy()
  })

  it('focuses the first editable field when the drawer opens', async () => {
    const user = userEvent.setup()
    await renderPage()
    await screen.findByText(/8 rows/)

    await user.click(
      within(rowOf('billing.balance_low', 'email', 'en')).getByRole('button', { name: 'Edit' }),
    )
    const drawer = await screen.findByRole('dialog')
    const subject = await within(drawer).findByLabelText('Subject')
    await waitFor(() => expect(document.activeElement).toBe(subject))
  })

  it('focuses the body for in-app templates (no subject field)', async () => {
    const user = userEvent.setup()
    await renderPage()
    await screen.findByText(/8 rows/)

    await user.click(
      within(rowOf('system.announcement', 'inapp', 'en')).getByRole('button', { name: 'Edit' }),
    )
    const drawer = await screen.findByRole('dialog')
    const body = await within(drawer).findByLabelText('Body')
    await waitFor(() => expect(document.activeElement).toBe(body))
  })

  it('routes invalid_template errors containing constructs to the variables panel', async () => {
    const user = userEvent.setup()
    await renderPage()
    await screen.findByText(/8 rows/)
    server.use(
      http.put(`${ORIGIN}/api/notification-templates/:key`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/invalid_template',
            title: 'Invalid template',
            detail: 'notify: unknown variable {{.drifted}}',
          },
          { status: 422 },
        ),
      ),
    )

    await user.click(
      within(rowOf('billing.balance_low', 'email', 'en')).getByRole('button', { name: 'Edit' }),
    )
    const drawer = await screen.findByRole('dialog')
    const body = within(drawer).getByLabelText('Body')
    await user.clear(body)
    await user.type(body, 'Changed body')
    await user.click(within(drawer).getByRole('button', { name: 'Save changes' }))

    expect(await within(drawer).findByText(/unknown variable \{\{\.drifted\}\}/)).toBeTruthy()
  })

  it('disables the row Test action when SMTP is not configured', async () => {
    install()
    server.use(
      http.get(`${ORIGIN}/api/settings`, () =>
        HttpResponse.json({
          section: 'notify.smtp',
          updated_at: '2026-10-19T10:00:00Z',
          items: [
            { key: 'notify.smtp.host', value: '', type: 'string', default: '', public: false, secret: false, dangerous: false },
            { key: 'notify.smtp.from_email', value: '', type: 'string', default: '', public: false, secret: false, dangerous: false },
          ],
        }),
      ),
    )
    await renderWithProviders(<Harness />, ['common', 'settings'])

    await screen.findByText(/8 rows/)
    const test = within(rowOf('billing.balance_low', 'email', 'en')).getByRole('button', {
      name: 'Test',
    })
    await waitFor(() => expect((test as HTMLButtonElement).disabled).toBe(true))
    expect(test.getAttribute('title')).toMatch(/SMTP is not configured/)
  })

  it('confirms before disabling a template and focuses cancel', async () => {
    const user = userEvent.setup()
    await renderPage()
    await screen.findByText(/8 rows/)

    await user.click(
      within(rowOf('billing.quota_exhausted', 'email', 'en')).getByRole('button', { name: 'Edit' }),
    )
    const drawer = await screen.findByRole('dialog')
    await user.click(within(drawer).getByRole('switch', { name: 'Enabled' }))

    const confirm = await screen.findByRole('alertdialog')
    const cancel = within(confirm).getByRole('button', { name: 'Cancel' })
    // 危险确认必须把焦点落到「取消」（设计 §6.9）；不用 toHaveFocus，
    // 本项目未安装 @testing-library/jest-dom。
    await waitFor(() => expect(document.activeElement).toBe(cancel))

    await user.click(within(confirm).getByRole('button', { name: 'Disable template' }))
    await waitFor(() =>
      expect(
        (within(drawer).getByRole('switch', { name: 'Enabled' }) as HTMLInputElement).checked,
      ).toBe(false),
    )
  })

  it('shows the conflict dialog when the optimistic token is stale', async () => {
    const user = userEvent.setup()
    await renderPage()
    await screen.findByText(/8 rows/)
    server.use(
      http.put(`${ORIGIN}/api/notification-templates/:key`, () =>
        HttpResponse.json(
          { type: 'https://newtg.dev/problems/conflict', title: 'Conflict', detail: 'stale' },
          { status: 409 },
        ),
      ),
    )

    await user.click(within(rowOf('billing.balance_low', 'email', 'en')).getByRole('button', { name: 'Edit' }))
    const dialog = await screen.findByRole('dialog')
    const body = within(dialog).getByLabelText('Body')
    await user.clear(body)
    await user.type(body, 'Changed')
    await user.click(within(dialog).getByRole('button', { name: 'Save changes' }))

    expect(await screen.findByText('Template changed elsewhere')).toBeTruthy()
  })

  it('renders the saved-content preview', async () => {
    const user = userEvent.setup()
    await renderPage()
    await screen.findByText(/8 rows/)

    await user.click(within(rowOf('billing.balance_low', 'email', 'en')).getByRole('button', { name: 'Preview' }))
    expect(await screen.findByText('Rendered subject')).toBeTruthy()
    expect(screen.getByText('Rendered body')).toBeTruthy()
  })

  it('sends a test email from the row action', async () => {
    const user = userEvent.setup()
    const { testRequests } = await renderPage()
    await screen.findByText(/8 rows/)

    await user.click(within(rowOf('billing.balance_low', 'email', 'en')).getByRole('button', { name: 'Test' }))
    const dialog = await screen.findByRole('dialog')
    expect((within(dialog).getByLabelText('Recipient') as HTMLInputElement).value).toBe(
      'admin@example.com',
    )
    await user.click(within(dialog).getByRole('button', { name: 'Send test' }))

    expect(await within(dialog).findByText('Test email sent. Check your inbox.')).toBeTruthy()
    expect(testRequests[0]).toMatchObject({ recipient: 'admin@example.com', channel: 'email' })
  })

  it('shows an inline countdown on a 429 test send', async () => {
    const user = userEvent.setup()
    await renderPage()
    await screen.findByText(/8 rows/)
    server.use(
      http.post(`${ORIGIN}/api/notification-templates/:key`, () =>
        HttpResponse.json(
          { type: 'https://newtg.dev/problems/rate_limited', title: 'Too many requests' },
          { status: 429, headers: { 'Retry-After': '5' } },
        ),
      ),
    )

    await user.click(within(rowOf('billing.balance_low', 'email', 'en')).getByRole('button', { name: 'Test' }))
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Send test' }))

    expect(
      await within(dialog).findByText('Too many test sends. Try again in 5 seconds.'),
    ).toBeTruthy()
    expect(within(dialog).getByRole('button', { name: /Retry in \d+s/ })).toBeTruthy()
  })

  it('renders masked deliveries and filters by status', async () => {
    const user = userEvent.setup()
    await renderPage({ tab: 'deliveries' })

    expect(await screen.findByText('a***e@example.com')).toBeTruthy()
    expect(screen.getByText('r***t@example.com')).toBeTruthy()
    const deliveryRows = screen.getAllByTestId('notification-delivery-row')
    expect(deliveryRows).toHaveLength(3)
    expect(within(deliveryRows[0]!).getByText('Sent')).toBeTruthy()
    expect(within(deliveryRows[1]!).getByText('Failed')).toBeTruthy()
    expect(within(deliveryRows[2]!).getByText('Bounced')).toBeTruthy()
    expect(screen.getByText('connection refused')).toBeTruthy()

    await user.selectOptions(screen.getByLabelText('Status'), 'failed')
    await waitFor(() =>
      expect(screen.getAllByTestId('notification-delivery-row')).toHaveLength(1),
    )
    expect(screen.queryByText('a***e@example.com')).toBeNull()
  })

  it('is read-only for admins without settings:manage', async () => {
    const user = userEvent.setup()
    install()
    server.use(
      http.get(`${ORIGIN}/api/me`, () =>
        HttpResponse.json({ ...ME_ADMIN, permissions: ['users:read'] }),
      ),
    )
    await renderWithProviders(<Harness />, ['common', 'settings'])

    expect(
      await screen.findByText(/your account lacks the settings:manage permission/i),
    ).toBeTruthy()
    await screen.findByText(/8 rows/)
    // 写类操作不渲染（而非仅禁用）：Test 入口直接缺席，Edit/Preview 保留为只读。
    expect(
      within(rowOf('billing.balance_low', 'email', 'en')).queryByRole('button', { name: 'Test' }),
    ).toBeNull()
    expect(
      within(rowOf('billing.balance_low', 'email', 'en')).getByRole('button', { name: 'Edit' }),
    ).toBeTruthy()

    await user.click(within(rowOf('billing.balance_low', 'email', 'en')).getByRole('button', { name: 'Edit' }))
    const dialog = await screen.findByRole('dialog')
    expect(await within(dialog).findByText(/settings:manage permission/)).toBeTruthy()
    expect(within(dialog).queryByRole('button', { name: 'Save changes' })).toBeNull()
  })

  it('paginates templates 20 rows at a time', async () => {
    const user = userEvent.setup()
    const rows: NotificationTemplate[] = []
    for (let index = 0; index < 25; index += 1) {
      const category = NOTIFICATION_CATEGORIES[index % NOTIFICATION_CATEGORIES.length]!
      const locale = index < NOTIFICATION_CATEGORIES.length ? 'en' : 'fr'
      rows.push(template({ key: category.key, channel: category.channels[0] ?? 'email', locale }))
    }
    install(rows)
    await renderWithProviders(<Harness />, ['common', 'settings'])

    expect(await screen.findByText('Page 1 of 2')).toBeTruthy()
    expect(screen.getAllByTestId('notification-template-row')).toHaveLength(20)
    await user.click(screen.getByRole('button', { name: 'Next' }))
    expect(await screen.findByText('Page 2 of 2')).toBeTruthy()
    expect(screen.getAllByTestId('notification-template-row')).toHaveLength(5)
  })
})
