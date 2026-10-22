import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { ChannelListPage } from './channel-list-page'
import {
  findRequest,
  installFetch,
  jsonResponse,
  makeChannel,
  makeListResponse,
  problemResponse,
  renderWithProviders,
} from './test-utils'
import type { ChannelMetrics } from './types'

/**
 * W16-1：渠道列表内嵌 24h 概要（F-06-11 列表列）与展开区趋势面板。
 * 概要是**可选**字段：日志库不可用时列显示 —，列表本身仍可用。
 */

function metricsFixture(): ChannelMetrics {
  return {
    channel_id: 1,
    window: '24h',
    bucket_seconds: 3600,
    points: Array.from({ length: 24 }, (_, index) => ({
      bucket_start: new Date(Date.UTC(2026, 9, 22, index)).toISOString(),
      request_count: 2,
      success_count: 1,
      failure_count: 1,
      success_rate: 0.5,
      avg_latency_ms: 120,
      avg_first_token_ms: null,
      avg_cost: 2,
    })),
    summary: {
      request_count: 8,
      success_count: 6,
      failure_count: 2,
      success_rate: 0.75,
      avg_latency_ms: 120,
      avg_first_token_ms: null,
      avg_cost: 2,
      cost_total: 16,
      used_credits: 160,
      used_credits_24h: 16,
      request_count_24h: 8,
    },
    last_test_at: null,
    last_test_result: null,
  }
}

describe('ChannelListPage metrics overview', () => {
  it('renders the 24h success rate and latency columns, and — when omitted', async () => {
    const items = [
      makeChannel({
        id: 1,
        name: 'alpha',
        metrics: { request_count: 10, success_rate: 0.8, avg_latency_ms: 150 },
      }),
      makeChannel({ id: 2, name: 'beta' }),
    ]
    installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeListResponse(items))
      }
      if (request.url.pathname === '/api/channels/1/metrics') {
        return jsonResponse(metricsFixture())
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    expect(await screen.findByText('alpha')).toBeTruthy()

    const table = screen.getByTestId('channels-table')
    expect(within(table).getByRole('columnheader', { name: 'Success (24h)' })).toBeTruthy()
    expect(within(table).getByRole('columnheader', { name: 'Latency (24h)' })).toBeTruthy()
    expect(screen.getByTestId('channel-metrics-rate-1').textContent).toBe('80.0%')
    expect(screen.getByTestId('channel-metrics-latency-1').textContent).toBe('150 ms')
    expect(screen.getByTestId('channel-metrics-rate-2').textContent).toBe('—')
    expect(screen.getByTestId('channel-metrics-latency-2').textContent).toBe('—')
  })

  it('loads the trend panel when the row is expanded', async () => {
    const user = userEvent.setup()
    const items = [makeChannel({ id: 1, name: 'alpha' })]
    const requests = installFetch((request) => {
      if (request.method === 'GET' && request.url.pathname === '/api/channels') {
        return jsonResponse(makeListResponse(items))
      }
      if (request.url.pathname === '/api/channels/1/metrics') {
        return jsonResponse(metricsFixture())
      }
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    })

    renderWithProviders(<ChannelListPage />)
    expect(await screen.findByText('alpha')).toBeTruthy()

    await user.click(screen.getByRole('button', { name: 'Expand details for alpha' }))

    const panel = await screen.findByTestId('channel-metrics-1')
    expect(within(panel).getByText('Usage metrics')).toBeTruthy()
    expect(within(panel).getByTestId('channel-metrics-sparkline')).toBeTruthy()
    await waitFor(() => {
      const request = findRequest(requests, 'GET', '/api/channels/1/metrics')
      expect(request?.url.searchParams.get('window')).toBe('24h')
    })
  })
})
