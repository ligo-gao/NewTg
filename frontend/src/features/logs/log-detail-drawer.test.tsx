import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import type { LogsCapabilities } from './capabilities'
import { LogDetailDrawer } from './log-detail-drawer'
import {
  installFetch,
  jsonResponse,
  problemResponse,
  renderWithProviders,
} from './test-utils'

const DETAIL = {
  id: 1,
  request_id: '9f3a8d2e-1b2c-4d5e-8f90-112233445566',
  token_id: 1,
  token_name: 'prod-key-1',
  channel_id: 12,
  upstream: 'OpenRouter-GPT',
  model: 'gpt-4o',
  group: 'default',
  prompt_tokens: 1204,
  completion_tokens: 356,
  cache_read_tokens: 812,
  cache_write_tokens: 0,
  pre_deducted: 512,
  cost: 45,
  refunded: 467,
  latency_ms: 1200,
  ttft: 300,
  stream: true,
  status: 'success' as const,
  error_class: null,
  error_message: null,
  retry_chain: null,
  created_at: '2026-10-14T09:41:22Z',
  other: {},
}

const ADMIN: LogsCapabilities = {
  loading: false,
  isAdmin: true,
  canFilterByUsername: true,
  canViewPrompt: false,
  canViewSensitive: false,
}

const USER: LogsCapabilities = {
  ...ADMIN,
  isAdmin: false,
  canFilterByUsername: false,
}

function installDetail(detail: object) {
  return installFetch((request) => {
    if (request.url.pathname === '/api/usage-logs/1') {
      return jsonResponse(detail)
    }
    return problemResponse(404, { type: 'about:blank', title: 'not found' })
  })
}

function renderDrawer(capabilities: LogsCapabilities) {
  return renderWithProviders(
    <LogDetailDrawer
      logId={1}
      onOpenChange={() => undefined}
      capabilities={capabilities}
      revealSensitive={false}
    />,
  )
}

describe('log detail drawer role gating', () => {
  it('shows the username field to an admin', async () => {
    installDetail({ ...DETAIL, username: 'alice' })
    renderDrawer(ADMIN)

    const dialog = await screen.findByRole('dialog')
    expect(await within(dialog).findByText('General')).toBeTruthy()
    expect(within(dialog).getByText('User')).toBeTruthy()
    expect(within(dialog).getByText('alice')).toBeTruthy()
  })

  it('never shows the username field to a non-admin', async () => {
    // The payload carries the admin-only field; the UI must still not render
    // it without the admin role.
    installDetail({ ...DETAIL, username: 'alice' })
    renderDrawer(USER)

    const dialog = await screen.findByRole('dialog')
    expect(await within(dialog).findByText('General')).toBeTruthy()
    expect(within(dialog).queryByText('User')).toBeNull()
    expect(within(dialog).queryByText('alice')).toBeNull()
  })
})
