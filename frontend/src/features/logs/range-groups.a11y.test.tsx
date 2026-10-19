import { fireEvent, screen, waitFor } from '@testing-library/react'
import { useState } from 'react'
import { describe, expect, it } from 'vitest'

import {
  DEFAULT_AUDIT_LOG_SEARCH,
  type AuditLogSearch,
} from '@/features/audit/search'
import { AuditLogsPage } from '@/features/audit/audit-logs-page'
import {
  installFetch,
  jsonResponse,
  problemResponse,
  renderWithProviders,
} from '@/features/logs/test-utils'
import type { UsageLogSearch } from '@/features/logs/search'
import { DEFAULT_USAGE_LOG_SEARCH } from '@/features/logs/search'
import { UsageLogsPage } from '@/features/logs/usage-logs-page'

/**
 * UI-5: the time-range controls on the log pages are radiogroups, so they must
 * support the WAI-ARIA arrow-key pattern (move focus + selection) rather than
 * being pointer-only.
 */

const ME = {
  username: 'root',
  role: 'admin' as const,
  status: 'enabled' as const,
  permissions: ['logs:read'] as string[],
}

function page(items: unknown[]) {
  return { items, total: items.length, page: 1, page_size: 20 }
}

function installOnePage(item: unknown) {
  return installFetch((request) => {
    if (request.url.pathname === '/api/me') return jsonResponse(ME)
    if (
      request.url.pathname === '/api/audit-logs' ||
      request.url.pathname === '/api/usage-logs'
    ) {
      return jsonResponse(page([item]))
    }
    return problemResponse(404, { type: 'about:blank', title: 'not found' })
  })
}

const AUDIT_ROW = {
  id: 1,
  user_id: 2,
  username: 'alice',
  category: 'login' as const,
  ip: '203.0.113.5',
  user_agent: 'Mozilla/5.0',
  method: 'POST',
  route: '/api/auth/login',
  status_code: 200,
  result: 'success' as const,
  detail: {},
  before: null,
  after: null,
  created_at: '2026-10-14T09:41:22Z',
}

const USAGE_ROW = {
  id: 1,
  request_id: '9f3a8d2e-1b2c-4d5e-8f90-112233445566',
  token_id: 1,
  token_name: 'prod-key-1',
  channel_id: 12,
  upstream: 'OpenRouter-GPT',
  model: 'gpt-4o',
  group: 'default',
  prompt_tokens: 1,
  completion_tokens: 1,
  cache_read_tokens: 0,
  cache_write_tokens: 0,
  pre_deducted: 2,
  cost: 1,
  refunded: 1,
  latency_ms: 1000,
  ttft: 200,
  stream: true,
  status: 'success' as const,
  error_class: null,
  error_message: null,
  retry_chain: null,
  created_at: '2026-10-14T09:41:22Z',
}

function AuditHarness() {
  // R-07 例外（2026-10-19 评审裁决）：审计日志默认范围按设计稿 modules-05
  // 保持 30d，本规格验证的是范围单选组的键盘行为，因此 harness 显式传入
  // 24h 作为起始值——断言本身未做任何改动。
  const [search, setSearch] = useState<AuditLogSearch>({
    ...DEFAULT_AUDIT_LOG_SEARCH,
    range: '24h',
  })
  return <AuditLogsPage search={search} onSearchChange={setSearch} />
}

function UsageHarness() {
  const [search, setSearch] = useState<UsageLogSearch>(DEFAULT_USAGE_LOG_SEARCH)
  return <UsageLogsPage search={search} onSearchChange={setSearch} />
}

describe('log page range radiogroups (UI-5)', () => {
  it('audit logs: ArrowRight moves focus and selection', async () => {
    installOnePage(AUDIT_ROW)
    renderWithProviders(<AuditHarness />)

    const first = await screen.findByRole('radio', { name: '24h' })
    await waitFor(() => expect(first.getAttribute('aria-checked')).toBe('true'))
    first.focus()

    fireEvent.keyDown(first, { key: 'ArrowRight' })
    const second = screen.getByRole('radio', { name: '7d' })
    expect(document.activeElement).toBe(second)
    expect(second.getAttribute('aria-checked')).toBe('true')
  })

  it('usage logs: ArrowRight moves focus and selection', async () => {
    installOnePage(USAGE_ROW)
    renderWithProviders(<UsageHarness />)

    const first = await screen.findByRole('radio', { name: '24h' })
    await waitFor(() => expect(first.getAttribute('aria-checked')).toBe('true'))
    first.focus()

    fireEvent.keyDown(first, { key: 'ArrowRight' })
    const second = screen.getByRole('radio', { name: '7d' })
    expect(document.activeElement).toBe(second)
    expect(second.getAttribute('aria-checked')).toBe('true')
  })
})
