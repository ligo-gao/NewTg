import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { ChannelDetailPanel } from './channel-detail-panel'
import {
  installFetch,
  makeChannel,
  problemResponse,
  renderWithProviders,
} from './test-utils'

/**
 * The expanded row is the only place that shows every model, plus routing,
 * groups and 24h health. Existing scalar fields must keep rendering alongside.
 *
 * Layout (channels-preview.html): a full-width models card, a three-card row
 * (routing, channel info, 24h health), then a full-width metrics card.
 */

function stubMetrics(): void {
  installFetch((request) => {
    if (request.url.pathname === '/api/channels/7/metrics') {
      return problemResponse(404, { type: 'about:blank', title: 'not found' })
    }
    return problemResponse(404, { type: 'about:blank', title: 'not found' })
  })
}

describe('ChannelDetailPanel', () => {
  it('renders every model as a chip under the models heading', () => {
    stubMetrics()
    renderWithProviders(
      <ChannelDetailPanel
        channel={makeChannel({
          id: 7,
          supported_models: ['gpt-4', 'gpt-4o', 'o3-mini'],
        })}
      />,
    )

    const heading = screen.getByRole('heading', { name: 'All models (3)' })
    const section = heading.closest('section')
    expect(section).not.toBeNull()
    const models = within(section as HTMLElement)

    expect(models.getByText('gpt-4')).toBeTruthy()
    expect(models.getByText('gpt-4o')).toBeTruthy()
    expect(models.getByText('o3-mini')).toBeTruthy()
  })

  it('lays out the models card, the three-card row and the full-width metrics card', () => {
    stubMetrics()
    renderWithProviders(
      <ChannelDetailPanel
        channel={makeChannel({
          id: 7,
          supported_models: ['gpt-4', 'gpt-4o'],
        })}
      />,
    )

    const modelsCard = screen
      .getByRole('heading', { name: 'All models (2)' })
      .closest('section')
    const routingCard = screen
      .getByRole('heading', { name: 'Routing & groups' })
      .closest('section')
    const infoCard = screen
      .getByRole('heading', { name: 'Channel info' })
      .closest('section')
    const healthCard = screen
      .getByRole('heading', { name: '24h health' })
      .closest('section')
    const metricsCard = screen.getByTestId('channel-metrics-7')

    expect(modelsCard).not.toBeNull()
    expect(routingCard).not.toBeNull()
    expect(infoCard).not.toBeNull()
    expect(healthCard).not.toBeNull()

    // The three smaller cards share one row; the metrics card is a sibling of
    // that row and of the models card, i.e. it spans the dossier full width.
    const dossier = modelsCard?.parentElement
    expect(dossier).not.toBeNull()
    expect(routingCard?.parentElement).toBe(infoCard?.parentElement)
    expect(healthCard?.parentElement).toBe(routingCard?.parentElement)
    expect(metricsCard.parentElement).toBe(dossier)
  })

  it('renders the performance test action in the models card and invokes it', async () => {
    const user = userEvent.setup()
    const onPerformanceTest = vi.fn()
    stubMetrics()
    renderWithProviders(
      <ChannelDetailPanel
        channel={makeChannel({ id: 7, supported_models: ['gpt-4'] })}
        onPerformanceTest={onPerformanceTest}
      />,
    )

    const modelsCard = screen
      .getByRole('heading', { name: 'All models (1)' })
      .closest('section')
    expect(modelsCard).not.toBeNull()
    const button = within(modelsCard as HTMLElement).getByRole('button', {
      name: 'Performance test',
    })

    await user.click(button)
    expect(onPerformanceTest).toHaveBeenCalledTimes(1)
  })

  it('omits the performance test action when no callback is provided', () => {
    stubMetrics()
    renderWithProviders(
      <ChannelDetailPanel channel={makeChannel({ id: 7, supported_models: ['gpt-4'] })} />,
    )

    expect(
      screen.queryByRole('button', { name: 'Performance test' }),
    ).toBeNull()
  })

  it('shows priority, weight and groups in the routing section', () => {
    stubMetrics()
    renderWithProviders(
      <ChannelDetailPanel
        channel={makeChannel({
          id: 7,
          priority: 42,
          weight: 7,
          group_tags: ['vip', 'default'],
        })}
      />,
    )

    const heading = screen.getByRole('heading', { name: 'Routing & groups' })
    const section = heading.closest('section')
    expect(section).not.toBeNull()
    const routing = within(section as HTMLElement)

    expect(routing.getByText('Priority')).toBeTruthy()
    expect(routing.getByText('42')).toBeTruthy()
    expect(routing.getByText('Weight')).toBeTruthy()
    expect(routing.getByText('7')).toBeTruthy()
    expect(routing.getByText('Groups')).toBeTruthy()
    expect(routing.getByText('vip')).toBeTruthy()
    expect(routing.getByText('default')).toBeTruthy()
  })

  it('falls back to "All groups" when group_tags is empty', () => {
    stubMetrics()
    renderWithProviders(
      <ChannelDetailPanel channel={makeChannel({ id: 7, group_tags: null })} />,
    )

    expect(screen.getByText('All groups')).toBeTruthy()
  })

  it('renders the 24h health heading, last test and the metrics panel', () => {
    stubMetrics()
    renderWithProviders(
      <ChannelDetailPanel
        channel={makeChannel({
          id: 7,
          last_test_at: new Date().toISOString(),
          last_test_result: true,
        })}
      />,
    )

    const heading = screen.getByRole('heading', { name: '24h health' })
    const section = heading.closest('section')
    expect(section).not.toBeNull()
    const health = within(section as HTMLElement)

    expect(health.getByText('Last test')).toBeTruthy()
    expect(health.getByText(/just now/)).toBeTruthy()
    expect(screen.getByTestId('channel-metrics-7')).toBeTruthy()
  })

  it('shows the channel 24h success and latency when the summary carries them', () => {
    stubMetrics()
    renderWithProviders(
      <ChannelDetailPanel
        channel={makeChannel({
          id: 7,
          metrics: {
            request_count: 12,
            success_rate: 0.75,
            avg_latency_ms: 240,
          },
        })}
      />,
    )

    const heading = screen.getByRole('heading', { name: '24h health' })
    const section = heading.closest('section')
    expect(section).not.toBeNull()
    const health = within(section as HTMLElement)

    expect(health.getByText('Success (24h)')).toBeTruthy()
    expect(health.getByText('75.0%')).toBeTruthy()
    expect(health.getByText('Latency (24h)')).toBeTruthy()
    expect(health.getByText('240 ms')).toBeTruthy()
  })

  it('shows last test only when the channel has no 24h summary', () => {
    stubMetrics()
    renderWithProviders(<ChannelDetailPanel channel={makeChannel({ id: 7 })} />)

    const heading = screen.getByRole('heading', { name: '24h health' })
    const section = heading.closest('section')
    const health = within(section as HTMLElement)

    expect(health.getByText('Last test')).toBeTruthy()
    expect(health.queryByText('Success (24h)')).toBeNull()
    expect(health.queryByText('Latency (24h)')).toBeNull()
  })

  it('keeps the existing detail fields, including the auto-disable reason', () => {
    stubMetrics()
    renderWithProviders(
      <ChannelDetailPanel
        channel={makeChannel({
          id: 7,
          rate_limit: 60,
          auto_ban: false,
          fail_count: 3,
          tag: 'prod',
          status: 3,
          auto_disabled_reason: 'channel_keyword',
          updated_at: '2026-10-01T00:00:00Z',
        })}
      />,
    )

    const heading = screen.getByRole('heading', { name: 'Channel info' })
    const section = heading.closest('section')
    expect(section).not.toBeNull()
    const info = within(section as HTMLElement)

    expect(info.getByText('ID')).toBeTruthy()
    expect(info.getByText('7')).toBeTruthy()
    expect(info.getByText('Rate limit')).toBeTruthy()
    expect(info.getByText('60')).toBeTruthy()
    expect(info.getByText('Auto-disable')).toBeTruthy()
    expect(info.getByText('No')).toBeTruthy()
    expect(info.getByText('Failures')).toBeTruthy()
    expect(info.getByText('3')).toBeTruthy()
    expect(info.getByText('Tag')).toBeTruthy()
    expect(info.getByText('prod')).toBeTruthy()
    expect(info.getByText('Updated')).toBeTruthy()
    expect(info.getByText('Auto-disable reason')).toBeTruthy()
    expect(info.getByText(/Matched a disable keyword/)).toBeTruthy()
    expect(info.queryByText(/channel_keyword/)).toBeNull()
  })
})
