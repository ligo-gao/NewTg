import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { NotifyEntryCard } from './notify-entry-card'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const ITEMS = [
  { key: 'billing.balance_low', category: 'billing.balance_low', channel: 'email', locale: 'en', subject: 's', body: 'b', enabled: true, customized: true, updated_at: '2026-10-19T10:00:00Z' },
  { key: 'billing.balance_low', category: 'billing.balance_low', channel: 'inapp', locale: 'fr', subject: '', body: 'b', enabled: true, customized: false, updated_at: '2026-10-19T10:00:00Z' },
  { key: 'auth.verify_code', category: 'auth.verify_code', channel: 'email', locale: 'en', subject: 's', body: 'b', enabled: true, customized: false, updated_at: '2026-10-19T10:00:00Z' },
]

function install() {
  server.use(
    http.get(`${ORIGIN}/api/notification-templates`, () => HttpResponse.json({ items: ITEMS })),
  )
}

describe('NotifyEntryCard', () => {
  it('summarises category, modified-row and locale coverage and links onward', async () => {
    const user = userEvent.setup()
    install()
    const onManage = vi.fn()
    await renderWithProviders(
      <NotifyEntryCard canManage onManage={onManage} />,
      ['common', 'settings'],
    )

    expect(await screen.findByText('Notification templates')).toBeTruthy()
    // 16 类来自冻结注册表；已修改与语言覆盖来自真实返回行。
    expect(await screen.findByText('16')).toBeTruthy()
    expect(screen.getByText('1')).toBeTruthy()
    expect(screen.getByText('2')).toBeTruthy()

    await user.click(screen.getByRole('button', { name: 'Manage templates' }))
    expect(onManage).toHaveBeenCalledTimes(1)
  })

  it('renders nothing without settings:manage', async () => {
    install()
    const { container } = await renderWithProviders(
      <NotifyEntryCard canManage={false} />,
      ['common', 'settings'],
    )
    expect(container.querySelector('[data-testid="settings-notify-entry"]')).toBeNull()
  })
})
