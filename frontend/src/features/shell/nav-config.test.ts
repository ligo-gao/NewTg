import { describe, expect, it } from 'vitest'

import type { Me } from '@/lib/session'
import en from '@/locales/en/common.json'
import fr from '@/locales/fr/common.json'
import ja from '@/locales/ja/common.json'
import ru from '@/locales/ru/common.json'
import vi from '@/locales/vi/common.json'
import zhCN from '@/locales/zh-CN/common.json'
import zhTW from '@/locales/zh-TW/common.json'

import {
  activeNavItem,
  NAV_GROUPS,
  visibleNavGroups,
  type NavGroup,
} from './nav-config'

const FULL_ADMIN = { role: 'admin', permissions: ['*'] } as unknown as Me
const USER = { role: 'user', permissions: [] } as unknown as Me
const LIMITED_ADMIN = {
  role: 'admin',
  permissions: ['users:read'],
} as unknown as Me

function itemIds(groups: readonly NavGroup[]): string[] {
  return groups.flatMap((group) => group.items.map((item) => item.id))
}

const ALL_ITEMS = NAV_GROUPS.flatMap((group) => group.items)

describe('nav-config', () => {
  it('shows every group to a full admin', () => {
    const ids = itemIds(visibleNavGroups(FULL_ADMIN))
    expect(ids).toEqual(
      expect.arrayContaining([
        'overview',
        'tokens',
        'channels',
        'settings',
        'profile',
        'about',
      ]),
    )
  })

  it('hides the admin and system groups from an ordinary user', () => {
    const ids = itemIds(visibleNavGroups(USER))
    expect(ids).toContain('tokens')
    expect(ids).toContain('profile')
    expect(ids).not.toContain('channels')
    expect(ids).not.toContain('settings')
    expect(ids).not.toContain('users')
  })

  it('honors fine-grained permissions for admins', () => {
    const ids = itemIds(visibleNavGroups(LIMITED_ADMIN))
    expect(ids).toContain('users')
    expect(ids).not.toContain('channels')
    expect(ids).not.toContain('models')
    expect(ids).not.toContain('groups')
  })

  it('picks the longest matching route', () => {
    expect(activeNavItem('/dashboard/usage-logs')?.id).toBe('usage-logs')
    expect(activeNavItem('/dashboard/audit-logs')?.id).toBe('audit-logs')
    expect(activeNavItem('/dashboard')?.id).toBe('overview')
    expect(activeNavItem('/admin/settings')?.id).toBe('settings')
    expect(activeNavItem('/admin/settings/announcements')?.id).toBe('settings')
    expect(activeNavItem('/tokens')?.id).toBe('tokens')
  })

  it('has a resolvable label key for every item and group in all seven locales', () => {
    const dictionaries = [en, zhCN, zhTW, fr, ru, ja, vi] as ReadonlyArray<
      Record<string, unknown>
    >
    for (const dictionary of dictionaries) {
      for (const item of ALL_ITEMS) {
        const key = item.labelKey.replace(/^common\./, '')
        expect(dictionary[key], item.labelKey).toBeTruthy()
      }
      for (const group of NAV_GROUPS) {
        const key = group.labelKey.replace(/^common\./, '')
        expect(dictionary[key], group.labelKey).toBeTruthy()
      }
    }
  })
})
