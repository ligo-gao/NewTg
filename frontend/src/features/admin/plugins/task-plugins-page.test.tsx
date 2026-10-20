import { QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { render, screen, waitFor, within } from '@testing-library/react'
import { useState } from 'react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { I18nProvider } from '@/app/providers/i18n'
import { createQueryClient } from '@/app/providers/query'
import { routeTree } from '@/routeTree.gen'
import { renderWithProviders } from '@/test-support/providers'

import { TaskPluginsPage, type TaskPluginsTabId } from './task-plugins-page'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const UPDATED_AT = '2026-10-19T10:00:00Z'

const PUBLIC_STATUS = {
  system_initialized: true,
  auth: {
    signup_enabled: true,
    password_login_enabled: true,
    email_verification_required: false,
    turnstile: { enabled: false, site_key: '' },
  },
  password_login_encryption_enabled: false,
  languages: ['en'],
  default_language: 'en',
}

function installActor(permissions: string[]) {
  server.use(
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({
        username: 'root',
        role: 'admin',
        status: 'enabled',
        permissions,
      }),
    ),
  )
}

function installSettings(values: Record<string, unknown> = {}) {
  server.use(
    http.get(`${ORIGIN}/api/settings`, () =>
      HttpResponse.json({
        section: 'plugin',
        updated_at: UPDATED_AT,
        items: [
          { key: 'plugin.marketplace.enabled', value: true, type: 'bool', dangerous: false },
          { key: 'plugin.disabled_factory_keys', value: [], type: 'list', dangerous: true },
        ].map((item) => ({
          ...item,
          default: item.value,
          public: false,
          secret: false,
          ...(item.key in values ? { value: values[item.key] } : {}),
        })),
      }),
    ),
  )
}

function installEmptyList() {
  server.use(
    http.get(`${ORIGIN}/api/plugin/task`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
    ),
  )
}

function plugin(key: string, name: string) {
  return {
    key,
    name,
    icon: null,
    source: 'builtin',
    builtin: true,
    enabled: true,
    active_version: '1.0.0',
    description: null,
    runtime_status: 'registered',
    bound_channels: 0,
    updated_at: UPDATED_AT,
  }
}

function installShell(permissions: string[]) {
  installActor(permissions)
  installSettings()
  installEmptyList()
  server.use(
    http.get(`${ORIGIN}/api/announcements/active`, () =>
      HttpResponse.json({ items: [] }),
    ),
    http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(PUBLIC_STATUS)),
    http.get(`${ORIGIN}/api/users`, () =>
      HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
    ),
  )
}

/** 与应用同构的路由树：beforeLoad 守卫与非组件逻辑只能在这里验证。 */
function renderAt(path: string) {
  const queryClient = createQueryClient()
  const router = createRouter({
    routeTree,
    context: { queryClient },
    history: createMemoryHistory({ initialEntries: [path] }),
    scrollRestoration: false,
  })
  render(
    <QueryClientProvider client={queryClient}>
      <I18nProvider modules={['common', 'admin']}>
        <RouterProvider router={router} />
      </I18nProvider>
    </QueryClientProvider>,
  )
  return router
}

function Harness() {
  const [tab, setTab] = useState<TaskPluginsTabId>('installed')
  return <TaskPluginsPage tab={tab} onTabChange={setTab} />
}

describe('task plugins page shell (§6.10)', () => {
  it('exposes six tabs and supports arrow-key navigation', async () => {
    installActor(['settings:manage'])
    installSettings()
    installEmptyList()

    await renderWithProviders(<Harness />, ['common', 'admin'])

    const installed = await screen.findByRole('tab', { name: 'Installed' })
    expect(installed.getAttribute('aria-selected')).toBe('true')
    await waitFor(() => expect(screen.getAllByRole('tab')).toHaveLength(6))

    installed.focus()
    await userEvent.keyboard('{ArrowRight}')
    expect(
      await screen.findByRole('tab', { name: 'Upload', selected: true }),
    ).toBeTruthy()
    await userEvent.keyboard('{ArrowLeft}')
    expect(
      await screen.findByRole('tab', { name: 'Installed', selected: true }),
    ).toBeTruthy()
  })

  it('redirects direct links to /403 without settings:manage (R-M6D-16)', async () => {
    window.scrollTo = () => undefined
    installShell([])

    const router = renderAt('/admin/system/task-plugins?tab=upload')

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/403')
    })
    expect(router.state.location.href).toContain(
      'redirect=%2Fadmin%2Fsystem%2Ftask-plugins%3Ftab%3Dupload',
    )
  })

  it('links the pricing row to model pricing with the plugin key', async () => {
    window.scrollTo = () => undefined
    installActor(['settings:manage'])
    installSettings()
    server.use(
      http.get(`${ORIGIN}/api/plugin/task`, () =>
        HttpResponse.json({
          items: [plugin('kling', 'Kling')],
          total: 1,
          page: 1,
          page_size: 20,
        }),
      ),
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(PUBLIC_STATUS)),
      http.get(`${ORIGIN}/api/users`, () =>
        HttpResponse.json({ items: [], total: 0, page: 1, page_size: 20 }),
      ),
      http.get(`${ORIGIN}/api/announcements/active`, () =>
        HttpResponse.json({ items: [] }),
      ),
    )

    renderAt('/admin/system/task-plugins?tab=pricing')

    const row = await screen.findByTestId('pricing-row')
    const link = within(row).getByRole('link', { name: /View pricing/ })
    expect(link.getAttribute('href')).toBe(
      '/admin/pricing/model-pricing?plugin=kling',
    )
    expect(within(row).queryAllByRole('textbox')).toHaveLength(0)
  })

  it('hides the marketplace tab when plugin.marketplace.enabled is false', async () => {
    installActor(['settings:manage'])
    installSettings({ 'plugin.marketplace.enabled': false })
    installEmptyList()

    await renderWithProviders(<Harness />, ['common', 'admin'])

    await screen.findByRole('tab', { name: 'Installed' })
    await waitFor(() => {
      expect(screen.queryByRole('tab', { name: 'Marketplace' })).toBeNull()
    })
  })
})
