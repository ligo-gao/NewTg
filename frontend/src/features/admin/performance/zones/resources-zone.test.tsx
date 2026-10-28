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
    lock_wait: 2.5,
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
  it('renders the zone rail as the section heading', async () => {
    await renderZone()

    expect(
      screen.getByRole('heading', { name: 'Server resources' }),
    ).toBeTruthy()
    expect(
      screen.getByRole('region', { name: 'Server resources' }),
    ).toBeTruthy()
  })

  it('renders the host, PostgreSQL and Redis cards with their readings', async () => {
    await renderZone()

    expect(screen.getAllByTestId('resource-card')).toHaveLength(3)

    const host = card('host')
    expect(host.textContent).toContain('38.0%')
    expect(host.textContent).toContain('62.0%')
    expect(host.textContent).toContain('71.0%')
    // 网络与命令速率是「每秒」量：读数必须带 /s 单位。
    expect(host.textContent).toContain('4.2 MB/s')
    expect(host.textContent).toContain('1.1 MB/s')

    const postgres = card('postgres')
    expect(postgres.textContent).toContain('11 / 25')
    expect(postgres.textContent).toContain('342')
    expect(postgres.textContent).toContain('12.4 GB')
    expect(postgres.textContent).toContain('Lock waits')
    expect(postgres.textContent).toContain('2.5')

    const redis = card('redis')
    expect(redis.textContent).toContain('38')
    expect(redis.textContent).toContain('96.2%')
    expect(redis.textContent).toContain('1,204/s')
  })

  it('renders a zero or non-finite lock-wait value instead of hiding the row', async () => {
    const zero = await renderZone({
      resources: {
        ...RESOURCES,
        postgres: { ...RESOURCES.postgres, lock_wait: 0 },
      },
    })
    expect(card('postgres').textContent).toContain('Lock waits')
    expect(card('postgres').textContent).toContain('0.0')
    zero.unmount()

    await renderZone({
      resources: {
        ...RESOURCES,
        postgres: { ...RESOURCES.postgres, lock_wait: Number.NaN },
      },
    })
    expect(card('postgres').textContent).toContain('Lock waits')
    expect(card('postgres').textContent).toContain('—')
  })

  it('renders null readings as an em dash instead of a fake zero', async () => {
    await renderZone({
      resources: {
        ...RESOURCES,
        host: { ...RESOURCES.host, cpu: null },
        postgres: { ...RESOURCES.postgres, max: null },
        redis: { ...RESOURCES.redis, up: null },
      },
    })

    // null = 无样本：渲染 `—`，同卡其它读数照常显示。
    const host = card('host')
    expect(host.textContent).toContain('—')
    expect(host.textContent).toContain('62.0%')
    expect(host.textContent).not.toContain('0.0%')

    // 单边未知：backends 已知、max 未知 → `11 / —`，不补 0，也不整行吞掉。
    expect(card('postgres').textContent).toContain('11 / —')

    // redis.up = null 是「未知」不是「故障」：整卡不渲染徽标。
    expect(within(card('redis')).queryByTestId('status-badge')).toBeNull()
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
