import { describe, expect, it } from 'vitest'

import { RECONCILIATION_TABS, normalizeReconciliationTab } from './tabs'

describe('normalizeReconciliationTab', () => {
  it('keeps a known tab', () => {
    expect(normalizeReconciliationTab('results')).toBe('results')
  })

  it('falls back to overview for an unknown value', () => {
    expect(normalizeReconciliationTab('nope')).toBe('overview')
  })

  it('falls back to overview when the param is missing', () => {
    expect(normalizeReconciliationTab(undefined)).toBe('overview')
  })

  it('keeps every tab the shell declares', () => {
    for (const tab of RECONCILIATION_TABS) {
      expect(normalizeReconciliationTab(tab.id)).toBe(tab.id)
    }
  })
})

describe('RECONCILIATION_TABS', () => {
  it('declares the four shell tabs in display order', () => {
    expect(RECONCILIATION_TABS.map((tab) => tab.id)).toEqual([
      'entry',
      'results',
      'overview',
      'accounts',
    ])
  })

  it('points every tab at the admin dictionary page namespace', () => {
    for (const tab of RECONCILIATION_TABS) {
      expect(tab.key.startsWith('admin.upstream_reconciliation.page.')).toBe(true)
    }
  })
})
