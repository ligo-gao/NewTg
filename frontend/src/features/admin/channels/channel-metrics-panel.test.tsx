import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { ChannelMetricsPanel } from './channel-metrics-panel'
import {
  findRequest,
  installFetch,
  jsonResponse,
  makeChannel,
  problemResponse,
  renderWithProviders,
} from './test-utils'
import type {
  ChannelMetrics,
  ChannelMetricsPoint,
  ChannelMetricsWindow,
} from './types'

/**
 * W16-1：渠道指标详情面板（F-06-11）。覆盖趋势渲染、24h/7d 切换、
 * 空态（无用量 ≠ 0%）、错误态 + 重试、离线态。
 */

function makePoints(window: ChannelMetricsWindow, empty = false): ChannelMetricsPoint[] {
  const count = window === '24h' ? 24 : 168
  return Array.from({ length: count }, (_, index) => ({
    bucket_start: new Date(Date.UTC(2026, 9, 21, index % 24)).toISOString(),
    request_count: empty ? 0 : 4,
    success_count: empty ? 0 : 3,
    failure_count: empty ? 0 : 1,
    success_rate: empty ? null : 0.75,
    avg_latency_ms: empty ? null : 240,
    avg_first_token_ms: empty ? null : 80,
    avg_cost: empty ? null : 1.5,
  }))
}

function makeMetrics(
  window: ChannelMetricsWindow,
  overrides: Partial<ChannelMetrics> = {},
  empty = false,
): ChannelMetrics {
  return {
    channel_id: 1,
    window,
    bucket_seconds: 3600,
    points: makePoints(window, empty),
    summary: {
      request_count: empty ? 0 : 12,
      success_count: empty ? 0 : 9,
      failure_count: empty ? 0 : 3,
      success_rate: empty ? null : 0.75,
      avg_latency_ms: empty ? null : 240,
      avg_first_token_ms: empty ? null : 80,
      avg_cost: empty ? null : 1.5,
      cost_total: empty ? 0 : 18,
      used_credits: empty ? 0 : 180,
      used_credits_24h: empty ? 0 : 18,
      request_count_24h: empty ? 0 : 12,
    },
    last_test_at: null,
    last_test_result: null,
    ...overrides,
  }
}

describe('ChannelMetricsPanel', () => {
  it('renders the summary and the success-rate sparkline', async () => {
    const requests = installFetch((request) => {
      if (request.url.pathname === '/api/channels/1/metrics') {
        return jsonResponse(makeMetrics('24h'))
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelMetricsPanel channel={makeChannel({ id: 1 })} />)

    // The panel is a self-contained card (region) so the dossier can render it
    // full width beneath the three-card row.
    const panel = screen.getByRole('region', { name: 'Usage metrics' })
    expect(await within(panel).findByText('Requests')).toBeTruthy()
    expect(await screen.findByText('Requests')).toBeTruthy()
    expect(screen.getByText('12')).toBeTruthy()
    expect(screen.getByText('75.0%')).toBeTruthy()
    expect(screen.getByText('240 ms')).toBeTruthy()
    expect(within(panel).getByText('Avg first token')).toBeTruthy()
    expect(within(panel).getByText('Credits (24h)')).toBeTruthy()
    const svg = screen.getByTestId('channel-metrics-sparkline')
    const line = svg.querySelector('path')
    expect(line?.getAttribute('d')).toBeTruthy()
    expect(line?.getAttribute('d')).toContain('M')

    const request = findRequest(requests, 'GET', '/api/channels/1/metrics')
    expect(request?.url.searchParams.get('window')).toBe('24h')
  })

  it('switches to 7d and refetches with the new window', async () => {
    const user = userEvent.setup()
    const requests = installFetch((request) => {
      const window = request.url.searchParams.get('window')
      return jsonResponse(makeMetrics(window === '7d' ? '7d' : '24h'))
    })

    renderWithProviders(<ChannelMetricsPanel channel={makeChannel({ id: 1 })} />)
    expect(await screen.findByText('Usage metrics')).toBeTruthy()

    await user.click(screen.getByRole('tab', { name: '7d' }))

    await waitFor(() => {
      const latest = findRequest(requests, 'GET', '/api/channels/1/metrics')
      expect(latest?.url.searchParams.get('window')).toBe('7d')
    })
    expect(screen.getByRole('tab', { name: '7d' }).getAttribute('aria-selected')).toBe('true')
  })

  it('shows the empty state instead of 0% when the channel has no traffic', async () => {
    installFetch(() => jsonResponse(makeMetrics('24h', {}, true)))

    renderWithProviders(<ChannelMetricsPanel channel={makeChannel({ id: 1 })} />)

    expect(await screen.findByTestId('channel-metrics-empty')).toBeTruthy()
    expect(screen.getByText('No usage data yet.')).toBeTruthy()
    expect(screen.queryByTestId('channel-metrics-sparkline')).toBeNull()
    expect(screen.queryByText('0.0%')).toBeNull()
  })

  it('shows an inline error and retries', async () => {
    const user = userEvent.setup()
    let calls = 0
    installFetch(() => {
      calls += 1
      if (calls === 1) {
        return problemResponse(500, {
          type: 'https://newtg.dev/problems/metrics_unavailable',
          title: 'Channel metrics unavailable',
        })
      }
      return jsonResponse(makeMetrics('24h'))
    })

    renderWithProviders(<ChannelMetricsPanel channel={makeChannel({ id: 1 })} />)

    expect(await screen.findByText('Failed to load channel metrics.')).toBeTruthy()
    await user.click(screen.getByRole('button', { name: 'Retry' }))
    await waitFor(() => {
      expect(screen.getByText('Usage metrics')).toBeTruthy()
      expect(screen.getByText('75.0%')).toBeTruthy()
    })
  })

  it('shows the offline state on a transport failure', async () => {
    installFetch(() => {
      throw new TypeError('Failed to fetch')
    })

    renderWithProviders(<ChannelMetricsPanel channel={makeChannel({ id: 1 })} />)

    expect(
      await screen.findByText(
        'You are offline. Metrics will load when the connection returns.',
      ),
    ).toBeTruthy()
  })
})
