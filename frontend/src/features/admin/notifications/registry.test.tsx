import { describe, expect, it } from 'vitest'

import {
  NOTIFICATION_CATEGORIES,
  categoryLabelKey,
  isLockedTemplate,
  scanTemplateVariables,
  variablesFor,
} from './registry'
import {
  categoriesFromSearch,
  categoriesToSearch,
  parseNotificationTemplatesSearch,
} from './search'
import { insertAtCursor, isValidCtaUrl, maskRecipient, runeLength } from './utils'

describe('notification registry', () => {
  it('freezes the 16 registered categories', () => {
    expect(NOTIFICATION_CATEGORIES).toHaveLength(16)
    expect(new Set(NOTIFICATION_CATEGORIES.map((entry) => entry.key)).size).toBe(16)
    expect(NOTIFICATION_CATEGORIES.some((entry) => entry.key === 'billing.balance_low')).toBe(true)
    expect(categoryLabelKey('auth.verify_code')).toBe(
      'settings.notification_templates.categories.auth_verify_code',
    )
  })

  it('keeps the P4 locked set in sync with the backend registry', () => {
    for (const key of [
      'auth.verify_code',
      'auth.password_reset',
      'auth.welcome',
      'security.alert',
      'system.smtp_test',
      'system.announcement',
    ]) {
      expect(isLockedTemplate(key)).toBe(true)
    }
    expect(isLockedTemplate('billing.balance_low')).toBe(false)
    expect(isLockedTemplate('ops.channel_auto_disabled')).toBe(false)
  })

  it('exposes the per-key channel variable whitelist', () => {
    expect(variablesFor('billing.balance_low', 'email')).toHaveLength(5)
    expect(variablesFor('billing.balance_low', 'inapp')).toHaveLength(3)
    expect(variablesFor('system.announcement', 'inapp').map((entry) => entry.name)).toEqual([
      'title',
      'summary',
      'published_at',
      'link_url',
    ])
  })

  it('flags unknown variables and unsupported constructs', () => {
    expect(scanTemplateVariables('Hello {{.balance}}', 'billing.balance_low', 'email')).toEqual({
      unknown: [],
      invalid: [],
    })
    expect(scanTemplateVariables('Hello {{.username}}', 'billing.balance_low', 'inapp')).toEqual({
      unknown: ['username'],
      invalid: [],
    })
    expect(scanTemplateVariables('Hello {{if .x}}', 'billing.balance_low', 'email').invalid).toEqual([
      '{{if .x}}',
    ])
    expect(scanTemplateVariables('Unclosed {{', 'billing.balance_low', 'email').invalid).toEqual([
      'Unclosed {{',
    ])
  })
})

describe('notification search contract', () => {
  it('normalises the URL search and drops unknown values', () => {
    expect(
      parseNotificationTemplatesSearch({
        tab: 'deliveries',
        category: 'ops.redemption_expiring,unknown,auth.verify_code',
        channel: 'inapp',
        locale: 'fr',
        modified: true,
        page: '2',
        d_status: 'failed',
        d_page: 3,
      }),
    ).toEqual({
      tab: 'deliveries',
      category: 'auth.verify_code,ops.redemption_expiring',
      channel: 'inapp',
      locale: 'fr',
      modified: true,
      page: 2,
      d_status: 'failed',
      d_page: 3,
    })
  })

  it('round-trips the multi-value category filter', () => {
    expect(categoriesFromSearch('system.announcement,auth.verify_code')).toEqual([
      'auth.verify_code',
      'system.announcement',
    ])
    expect(categoriesToSearch(['system.announcement', 'auth.verify_code'])).toBe(
      'auth.verify_code,system.announcement',
    )
    expect(categoriesToSearch([])).toBeUndefined()
  })
})

describe('notification utils', () => {
  it('masks recipients in the middle', () => {
    expect(maskRecipient('alice@example.com')).toBe('a***e@example.com')
    expect(maskRecipient('ab@example.com')).toBe('a***@example.com')
    expect(maskRecipient('https://hooks.example.com/x')).toBe('h***x')
  })

  it('inserts a variable at the cursor', () => {
    expect(insertAtCursor('Hello world', 5, '{{.name}}')).toEqual({
      value: 'Hello{{.name}} world',
      caret: 14,
    })
    expect(insertAtCursor('', null, '{{.x}}')).toEqual({ value: '{{.x}}', caret: 6 })
  })

  it('counts code points and accepts the documented CTA URLs', () => {
    expect(runeLength('abc')).toBe(3)
    expect(runeLength('🜁')).toBe(1)
    expect(isValidCtaUrl('https://example.com/x')).toBe(true)
    expect(isValidCtaUrl('{{.reset_url}}')).toBe(true)
    expect(isValidCtaUrl('javascript:alert(1)')).toBe(false)
  })
})
