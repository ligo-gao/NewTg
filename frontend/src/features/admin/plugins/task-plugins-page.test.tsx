import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor } from '@testing-library/react'
import { useState } from 'react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { TaskPluginsPage, type TaskPluginsTabId } from './task-plugins-page'

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const UPDATED_AT = '2026-10-19T10:00:00Z'

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

  it('hides write-only tabs and the upload shortcut without settings:manage', async () => {
    installActor([])
    installSettings()
    installEmptyList()

    await renderWithProviders(<Harness />, ['common', 'admin'])

    expect(
      await screen.findByText(
        'Read-only: your account lacks the settings:manage permission.',
      ),
    ).toBeTruthy()
    expect(screen.queryByRole('tab', { name: 'Upload' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Upload plugin' })).toBeNull()
    expect(screen.getByRole('tab', { name: 'Marketplace' })).toBeTruthy()
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
