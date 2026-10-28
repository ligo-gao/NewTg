import { render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { codePhrase, DegradedBanner } from './degraded-banner'

describe('codePhrase', () => {
  it('returns the template unchanged when it has no placeholder', () => {
    const { container } = render(
      <p>{codePhrase('No code in this sentence.', 'PROMETHEUS_URL')}</p>,
    )

    expect(container.textContent).toBe('No code in this sentence.')
    expect(container.querySelector('code')).toBeNull()
  })

  it('renders every placeholder as its own code element', () => {
    const { container } = render(
      <p>
        {codePhrase(
          'Check {code} in the deployment and also {code} again.',
          'PROMETHEUS_URL',
        )}
      </p>,
    )

    const codes = [...container.querySelectorAll('code')]
    expect(codes).toHaveLength(2)
    expect(codes.map((code) => code.textContent)).toEqual([
      'PROMETHEUS_URL',
      'PROMETHEUS_URL',
    ])
    expect(container.textContent).toBe(
      'Check PROMETHEUS_URL in the deployment and also PROMETHEUS_URL again.',
    )
  })
})

describe('DegradedBanner', () => {
  it('renders nothing while the overview has not arrived or the source is ok', async () => {
    const pending = await renderWithProviders(
      <DegradedBanner status={undefined} />,
      ['admin'],
    )

    expect(pending.container.innerHTML).toBe('')
    expect(screen.queryByRole('status')).toBeNull()
    pending.unmount()

    const healthy = await renderWithProviders(<DegradedBanner status="ok" />, [
      'admin',
    ])
    expect(healthy.container.innerHTML).toBe('')
    expect(screen.queryByRole('status')).toBeNull()
  })

  it('uses the query-failed copy when Prometheus itself returned an error', async () => {
    await renderWithProviders(<DegradedBanner status="error" />, ['admin'])

    const banner = screen.getByRole('status')
    expect(banner.getAttribute('data-testid')).toBe(
      'performance-degraded-banner',
    )
    expect(banner.textContent).toContain(
      'Monitoring query failed: instance and resource metrics are temporarily unavailable; model data is unaffected.',
    )
    expect(banner.textContent).toContain(
      'The backend reached the monitoring address, but Prometheus returned an error or timed out.',
    )
    // 新旧两条文案互斥：不能同时出现「未接入」的说法。
    expect(banner.textContent).not.toContain('Monitoring not connected')
    const code = within(banner).getByText('PROMETHEUS_URL')
    expect(code.tagName).toBe('CODE')
  })

  it.each(['unavailable', 'unknown'] as const)(
    'uses the not-connected copy when the source is %s',
    async (status) => {
      await renderWithProviders(<DegradedBanner status={status} />, ['admin'])

      const banner = screen.getByRole('status')
      expect(banner.textContent).toContain(
        'Monitoring not connected: instance and resource metrics are temporarily unavailable; model data is unaffected.',
      )
      expect(banner.textContent).toContain(
        'The page only reads from the backend, which queries Prometheus itself',
      )
      expect(banner.textContent).not.toContain('Monitoring query failed')
      const code = within(banner).getByText('PROMETHEUS_URL')
      expect(code.tagName).toBe('CODE')
    },
  )
})
