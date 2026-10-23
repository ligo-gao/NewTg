import { describe, expect, it } from 'vitest'

import type { Me } from '@/lib/session'

import { COMMANDS, isCommandAllowed, type CommandDefinition } from './commands'
import { NAV_GROUPS } from './nav-config'

const FULL_ADMIN = { role: 'admin', permissions: ['*'] } as unknown as Me
const USER = { role: 'user', permissions: [] } as unknown as Me
const USERS_READ_ADMIN = {
  role: 'admin',
  permissions: ['users:read'],
} as unknown as Me

function routeCommands(): CommandDefinition[] {
  return COMMANDS.filter((command) => command.kind === 'route')
}

function routeTo(to: string): CommandDefinition | undefined {
  return routeCommands().find((command) => command.to === to)
}

function requireRouteTo(to: string): CommandDefinition {
  const command = routeTo(to)
  if (command === undefined) {
    throw new Error(`missing route command for ${to}`)
  }
  return command
}

describe('command registry', () => {
  it('covers every navigation destination', () => {
    for (const group of NAV_GROUPS) {
      for (const item of group.items) {
        expect(routeTo(item.to), `missing route command for ${item.to}`).toBeDefined()
      }
    }
  })

  it('allows every route command for a full admin', () => {
    for (const command of routeCommands()) {
      expect(
        isCommandAllowed(command, FULL_ADMIN),
        `full admin denied ${command.id}`,
      ).toBe(true)
    }
  })

  it('denies an ordinary user every admin command but keeps session pages', () => {
    for (const command of COMMANDS) {
      if (command.requiredRole !== 'admin') continue
      expect(
        isCommandAllowed(command, USER),
        `ordinary user allowed ${command.id}`,
      ).toBe(false)
    }

    for (const to of ['/dashboard', '/tokens', '/about', '/pricing', '/docs']) {
      expect(
        isCommandAllowed(requireRouteTo(to), USER),
        `ordinary user denied ${to}`,
      ).toBe(true)
    }
  })

  it('limits a scoped admin to the surfaces their permission covers', () => {
    expect(
      isCommandAllowed(requireRouteTo('/admin/users'), USERS_READ_ADMIN),
    ).toBe(true)

    for (const to of [
      '/admin/settings',
      '/admin/system/performance',
      '/redemption-codes',
    ]) {
      expect(
        isCommandAllowed(requireRouteTo(to), USERS_READ_ADMIN),
        `scoped admin allowed ${to}`,
      ).toBe(false)
    }
  })

  it('fails closed for every gated command when `me` is missing', () => {
    for (const command of COMMANDS) {
      const gated =
        command.requiredRole !== undefined ||
        command.requiredPermission !== undefined
      if (!gated) continue

      expect(isCommandAllowed(command, null), `null me allowed ${command.id}`).toBe(
        false,
      )
      expect(
        isCommandAllowed(command, undefined),
        `undefined me allowed ${command.id}`,
      ).toBe(false)
    }
  })
})
