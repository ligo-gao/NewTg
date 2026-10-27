import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { DegradedBanner } from './degraded-banner'

describe('DegradedBanner', () => {
  it('renders nothing when not visible', async () => {
    const { container } = await renderWithProviders(
      <DegradedBanner visible={false} />,
      ['admin'],
    )

    expect(container.innerHTML).toBe('')
    expect(screen.queryByRole('status')).toBeNull()
  })

  it('announces the degraded copy with the Prometheus code phrase', async () => {
    await renderWithProviders(<DegradedBanner visible />, ['admin'])

    const banner = screen.getByRole('status')
    expect(banner.textContent).toContain(
      'Monitoring not connected: instance and resource metrics are temporarily unavailable; model data is unaffected.',
    )
    const code = within(banner).getByText('PROMETHEUS_URL')
    expect(code.tagName).toBe('CODE')
  })
})
