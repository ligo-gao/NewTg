import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { PricingBand } from './PricingBand'
import type { TimeConfigDraft } from './pricing-utils'

const CONFIG: TimeConfigDraft = {
  enabled: true,
  timezone: 'Asia/Shanghai',
  holidayPolicy: 'none',
  defaultMultiplier: '100',
  windows: [
    {
      name: 'Morning peak',
      days: [1, 2, 3, 4, 5],
      start: '09:00',
      end: '12:00',
      multiplier: '100',
    },
    {
      name: 'Cheap',
      days: [1, 2, 3, 4, 5],
      start: '12:00',
      end: '18:00',
      multiplier: '50',
    },
    {
      name: 'Evening peak',
      days: [1, 2, 3, 4, 5],
      start: '18:00',
      end: '20:00',
      multiplier: '150',
    },
  ],
}

describe('PricingBand', () => {
  it('marks peak and off-peak hours from the structured windows', async () => {
    const { getByRole } = await renderWithProviders(
      <PricingBand config={CONFIG} selected="2026-10-26T10:00" onSelect={() => undefined} />,
      ['common', 'admin'],
    )

    const peak = getByRole('button', {
      name: 'Monday 19:00 · Evening peak · 150%',
    })
    const off = getByRole('button', { name: 'Monday 13:00 · Cheap · 50%' })
    const base = getByRole('button', {
      name: 'Monday 09:00 · Morning peak · 100%',
    })
    expect(peak.getAttribute('data-kind')).toBe('peak')
    expect(off.getAttribute('data-kind')).toBe('off')
    expect(base.getAttribute('data-kind')).toBe('base')
  })

  it('marks the selected hour and reports the clicked time', async () => {
    const onSelect = vi.fn()
    const { getByRole } = await renderWithProviders(
      <PricingBand config={CONFIG} selected="2026-10-26T10:00" onSelect={onSelect} />,
      ['common', 'admin'],
    )
    const user = userEvent.setup()

    expect(
      getByRole('button', { name: 'Monday 10:00 · Morning peak · 100%' }).getAttribute(
        'aria-current',
      ),
    ).toBe('true')

    await user.click(
      getByRole('button', { name: 'Monday 09:00 · Morning peak · 100%' }),
    )
    expect(onSelect).toHaveBeenCalledWith('2026-10-26T09:00')
  })
})
