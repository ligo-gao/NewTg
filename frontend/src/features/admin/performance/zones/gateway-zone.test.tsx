import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import type { PerformanceStats, PerfOverview } from '../api'
import { GatewayZone } from './gateway-zone'

type Gateway = PerfOverview['gateways'][number]
type MemoryStats = PerformanceStats['memory_stats']

const MB = 1024 * 1024

function gateway(overrides: Partial<Gateway> = {}): Gateway {
  return {
    instance_id: 'gw-b',
    role: 'gateway',
    version: '1.2.3',
    last_seen: '2026-10-26T09:12:00Z',
    heartbeat_fresh: true,
    metrics: {
      goroutines: 842,
      heap_mb: 186,
      p95_ms: 1900,
      error_rate: 0.0002,
      inflight: 14,
    },
    metrics_status: 'ok',
    ...overrides,
  }
}

const LOCAL_STATS: MemoryStats = {
  alloc_bytes: 340 * MB,
  sys_bytes: 512 * MB,
  num_gc: 128,
  num_goroutine: 214,
}

async function renderZone(options: {
  gateways?: Gateway[]
  sourceDown?: boolean
  localStats?: MemoryStats | undefined
} = {}) {
  const result = await renderWithProviders(
    <GatewayZone
      gateways={options.gateways ?? [gateway()]}
      sourceDown={options.sourceDown ?? false}
      localStats={
        'localStats' in options ? options.localStats : LOCAL_STATS
      }
    />,
    ['admin'],
  )
  return result
}

describe('GatewayZone', () => {
  it('renders the zone rail with the visible title and replica count', async () => {
    await renderZone({
      gateways: [
        gateway({ instance_id: 'gw-b' }),
        gateway({ instance_id: 'gw-a' }),
      ],
    })

    expect(
      screen.getByRole('heading', { name: 'Gateway instances' }),
    ).toBeTruthy()
    expect(screen.getByText('2 replicas online')).toBeTruthy()
    expect(
      screen.getByRole('region', { name: 'Gateway instances' }),
    ).toBeTruthy()
  })

  it('renders the instance table with a metrics column sorted by instance id', async () => {
    await renderZone({
      gateways: [
        gateway({ instance_id: 'gw-b' }),
        gateway({ instance_id: 'gw-a' }),
      ],
    })

    const headers = screen
      .getAllByRole('columnheader')
      .map((header) => header.textContent)
    expect(headers).toEqual([
      'Instance',
      'Role',
      'Version',
      'Last heartbeat',
      'Metrics',
    ])

    const rows = screen.getAllByTestId('gateway-row')
    expect(rows).toHaveLength(2)
    expect(within(rows[0] as HTMLElement).getByText('gw-a')).toBeTruthy()
    expect(within(rows[1] as HTMLElement).getByText('gw-b')).toBeTruthy()
    expect(screen.queryByTestId('gateway-local')).toBeNull()
  })

  it('renders the metric list with every present sub-field', async () => {
    await renderZone()

    const row = screen.getAllByTestId('gateway-row')[0] as HTMLElement
    expect(row.textContent).toContain(
      'Ingress P95 1.9s · 5xx rate 0.02% · In-flight 14 · Goroutines 842 · Heap 186.0 MB',
    )
  })

  it('keeps a metric entry visible when only that sub-field is null', async () => {
    await renderZone({
      gateways: [
        gateway({
          metrics: {
            goroutines: 842,
            heap_mb: null,
            p95_ms: null,
            error_rate: null,
            inflight: 14,
          },
          // 五项不齐时后端标 unknown，但它仍不是「没有 metrics」；这一格必须
          // 仍显示子项与 `—`，而不是退回整列的 metrics_missing。
          metrics_status: 'ok',
        }),
      ],
    })

    const row = screen.getAllByTestId('gateway-row')[0] as HTMLElement
    expect(row.textContent).toContain('Ingress P95 —')
    expect(row.textContent).toContain('5xx rate —')
    expect(row.textContent).toContain('In-flight 14')
    expect(row.textContent).toContain('Heap —')
    expect(within(row).queryByText('Metrics unavailable')).toBeNull()
  })

  it.each([
    ['metrics null', gateway({ metrics: null, metrics_status: 'unknown' })],
    [
      'metrics_status unknown',
      gateway({ metrics_status: 'unknown' }),
    ],
  ])('shows metrics unavailable for %s', async (_label, fixture) => {
    await renderZone({ gateways: [fixture] })

    const row = screen.getAllByTestId('gateway-row')[0] as HTMLElement
    expect(within(row).getByText('Metrics unavailable')).toBeTruthy()
  })

  it('badges a stale heartbeat and renders — for a missing last_seen', async () => {
    await renderZone({
      gateways: [
        gateway({
          heartbeat_fresh: false,
          last_seen: null,
        }),
      ],
    })

    const row = screen.getAllByTestId('gateway-row')[0] as HTMLElement
    expect(within(row).getByText('Heartbeat stale')).toBeTruthy()
    expect(within(row).getByText('—')).toBeTruthy()
  })

  it('keeps the heartbeat table and appends the local process row when the source is down', async () => {
    await renderZone({ sourceDown: true })

    expect(screen.getAllByTestId('gateway-row')).toHaveLength(1)

    const local = screen.getByTestId('gateway-local')
    expect(
      within(local).getByText('Local process (this replica only)'),
    ).toBeTruthy()
    expect(local.textContent).toContain('GET /api/performance/stats')
    expect(within(local).getByText('Goroutines')).toBeTruthy()
    expect(within(local).getByText('214')).toBeTruthy()
    expect(within(local).getByText('340.0 MB')).toBeTruthy()
  })

  it('shows only the local title and note when local stats are unavailable', async () => {
    await renderZone({ sourceDown: true, localStats: undefined })

    const local = screen.getByTestId('gateway-local')
    expect(
      within(local).getByText('Local process (this replica only)'),
    ).toBeTruthy()
    expect(local.textContent).toContain('GET /api/performance/stats')
    expect(within(local).queryByText('Goroutines')).toBeNull()
    expect(within(local).queryByText('214')).toBeNull()
  })
})
