import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import type { PerfOverview } from '../api'
import { ResourcesZone } from './resources-zone'

type Resources = PerfOverview['resources']

const MB = 1024 * 1024

const RESOURCES: Resources = {
  host: {
    cpu: 0.38,
    mem: 0.62,
    disk: 0.71,
    net_in: 4.2 * MB,
    net_out: 1.1 * MB,
  },
  postgres: {
    backends: 11,
    max: 25,
    tps: 342,
    size_bytes: 12.4 * 1024 * MB,
    lock_wait: 0,
  },
  redis: {
    up: 1,
    clients: 38,
    mem_ratio: 0.36,
    hit_rate: 0.962,
    cmd_rate: 1204,
  },
}

async function renderZone(options: {
  resources?: Resources
  sourceDown?: boolean
} = {}) {
  const result = await renderWithProviders(
    <ResourcesZone
      resources={options.resources ?? RESOURCES}
      sourceDown={options.sourceDown ?? false}
    />,
    ['admin'],
  )
  return result
}

function card(resource: string): HTMLElement {
  const element = document.querySelector<HTMLElement>(
    `[data-resource="${resource}"]`,
  )
  if (element === null) throw new Error(`missing resource card ${resource}`)
  return element
}

describe('ResourcesZone', () => {
  it('renders the host, PostgreSQL and Redis cards with their readings', async () => {
    await renderZone()

    expect(screen.getAllByTestId('resource-card')).toHaveLength(3)

    const host = card('host')
    expect(host.textContent).toContain('38.0%')
    expect(host.textContent).toContain('62.0%')
    expect(host.textContent).toContain('71.0%')
    expect(host.textContent).toContain('4.2 MB')
    expect(host.textContent).toContain('1.1 MB')

    const postgres = card('postgres')
    expect(postgres.textContent).toContain('11 / 25')
    expect(postgres.textContent).toContain('342')
    expect(postgres.textContent).toContain('12.4 GB')

    const redis = card('redis')
    expect(redis.textContent).toContain('38')
    expect(redis.textContent).toContain('96.2%')
    expect(redis.textContent).toContain('1,204')
  })

  it('marks Redis up with a success badge that carries both colour and text', async () => {
    await renderZone()

    const badge = within(card('redis')).getByTestId('status-badge')
    expect(badge.textContent).toBe('Up')
    expect(badge.getAttribute('data-variant')).toBe('success')
  })

  it('marks Redis down with a danger badge', async () => {
    await renderZone({
      resources: {
        ...RESOURCES,
        redis: { ...RESOURCES.redis, up: 0 },
      },
    })

    const badge = within(card('redis')).getByTestId('status-badge')
    expect(badge.textContent).toBe('Down')
    expect(badge.getAttribute('data-variant')).toBe('danger')
  })

  it('replaces the whole grid with a dashed empty state when the source is down', async () => {
    await renderZone({ sourceDown: true })

    expect(screen.queryAllByTestId('resource-card')).toHaveLength(0)
    const empty = screen.getByTestId('resources-unavailable')
    expect(
      within(empty).getByText('Resource metrics unavailable'),
    ).toBeTruthy()
    expect(empty.textContent).toContain('PROMETHEUS_URL')
    // 虚线空态：不显示任何 0 或假读数。
    expect(empty.className).toContain('border-dashed')
    expect(screen.queryByText('38.0%')).toBeNull()
  })
})
