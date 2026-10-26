import { useState } from 'react'
import { fireEvent } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { StrategyPreview } from './StrategyPreview'
import type { TimeConfigDraft } from './pricing-utils'

const CONFIG: TimeConfigDraft = {
  enabled: true,
  timezone: 'Asia/Shanghai',
  holidayPolicy: 'none',
  defaultMultiplier: '50',
  windows: [
    {
      name: 'Morning peak',
      days: [1, 2, 3, 4, 5],
      start: '09:00',
      end: '12:00',
      multiplier: '100',
    },
  ],
}

const PRICES = {
  prompt: 200,
  completion: 800,
  cacheRead: 4,
  cacheWrite: 250,
}

describe('StrategyPreview', () => {
  function PeakHarness() {
    const [at, setAt] = useState('2026-10-26T10:00')
    return (
      <StrategyPreview
        mode="per_token"
        prices={PRICES}
        config={CONFIG}
        at={at}
        onAtChange={setAt}
        quotaPerUnit={100}
      />
    )
  }

  it('computes peak and off-peak totals locally', async () => {
    const { getByLabelText, getByTestId, getByText } = await renderWithProviders(
      <PeakHarness />,
      ['common', 'admin'],
    )

    // 128K × 200 + 4K × 800 + 64K × 4, all at 1.0x = 29.056 credits.
    expect(getByText('Morning peak')).toBeTruthy()
    expect(getByTestId('strategy-preview-total-credits').textContent).toContain('29.056')

    fireEvent.change(getByLabelText('Request time'), {
      target: { value: '2026-10-26T21:00' },
    })

    expect(getByText('Default rate')).toBeTruthy()
    expect(getByTestId('strategy-preview-total-credits').textContent).toContain('14.528')
  })

  it('scales a per-request price by the active multiplier', async () => {
    const { getByTestId } = await renderWithProviders(
      <StrategyPreview
        mode="per_request"
        prices={{ ...PRICES, prompt: 100 }}
        config={CONFIG}
        at="2026-10-26T21:00"
        onAtChange={() => undefined}
        quotaPerUnit={100}
      />,
      ['common', 'admin'],
    )

    expect(getByTestId('strategy-preview-total-credits').textContent).toContain('50')
    expect(getByTestId('strategy-preview-total-yuan').textContent).toContain('0.5000')
  })

  it('uses the base price when time pricing is disabled', async () => {
    const { getByTestId, getByText } = await renderWithProviders(
      <StrategyPreview
        mode="per_token"
        prices={PRICES}
        config={{ ...CONFIG, enabled: false }}
        at="2026-10-26T10:00"
        onAtChange={() => undefined}
        quotaPerUnit={100}
      />,
      ['common', 'admin'],
    )

    expect(getByText('Time pricing off')).toBeTruthy()
    expect(getByTestId('strategy-preview-total-credits').textContent).toContain('29.056')
  })

  it('shows peak and off-peak columns and highlights the active tier', async () => {
    const { getByTestId } = await renderWithProviders(
      <PeakHarness />,
      ['common', 'admin'],
    )
    const table = getByTestId('strategy-preview-compare')
    expect(table.textContent).toContain('Peak')
    expect(table.textContent).toContain('Off-peak')
    expect(table.querySelector('[data-active="true"]')?.textContent).toContain('Peak')
  })

  it('charges unset cache reads at the miss price', async () => {
    const { getByTestId } = await renderWithProviders(
      <StrategyPreview
        mode="per_token"
        prices={{ prompt: 200, completion: 800, cacheRead: null, cacheWrite: null }}
        config={CONFIG}
        at="2026-10-26T10:00"
        onAtChange={() => undefined}
        quotaPerUnit={100}
      />,
      ['common', 'admin'],
    )
    // 128K×200 + 64K×200 + 4K×800 = 41.6 credits
    expect(getByTestId('strategy-preview-total-credits').textContent).toContain('41.6')
    expect(getByTestId('strategy-preview-compare').textContent).toContain('Cache hit')
  })

  it('collapses to a single flat column when time pricing is off', async () => {
    const { getByTestId } = await renderWithProviders(
      <StrategyPreview
        mode="per_token"
        prices={PRICES}
        config={{ ...CONFIG, enabled: false }}
        at="2026-10-26T10:00"
        onAtChange={() => undefined}
        quotaPerUnit={100}
      />,
      ['common', 'admin'],
    )
    const table = getByTestId('strategy-preview-compare')
    expect(table.textContent).toContain('Flat')
    expect(table.textContent).not.toContain('Peak')
  })
})
