import { describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { PriceFields } from './PriceFields'

const BASE_VALUES = {
  prompt: '',
  completion: '',
  cacheRead: '',
  cacheWrite: '',
}

describe('PriceFields', () => {
  it('renders the two input tiers with the hit/miss ratio', async () => {
    const onChange = vi.fn()
    const { getByLabelText, getByText } = await renderWithProviders(
      <PriceFields
        mode="per_token"
        unit="credits"
        values={{ prompt: '200', cacheRead: '4', completion: '800', cacheWrite: '' }}
        errors={{}}
        quotaPerUnit={100}
        onChange={onChange}
        onUnitChange={() => undefined}
      />,
      ['common', 'admin'],
    )
    expect(getByLabelText('Input price (cache miss)')).toBeTruthy()
    expect(getByLabelText('Input price (cache hit)')).toBeTruthy()
    expect(getByText('= 2.00% of the miss price')).toBeTruthy()
  })

  it('warns when the hit price is higher than the miss price but keeps inputs editable', async () => {
    const { getByText, getByLabelText } = await renderWithProviders(
      <PriceFields
        mode="per_token"
        unit="credits"
        values={{ prompt: '100', cacheRead: '150', completion: '800', cacheWrite: '' }}
        errors={{}}
        quotaPerUnit={100}
        onChange={() => undefined}
        onUnitChange={() => undefined}
      />,
      ['common', 'admin'],
    )
    expect(getByText(/higher than the cache-miss price/)).toBeTruthy()
    expect(getByLabelText('Input price (cache hit)')).toBeTruthy()
  })

  it('renders the same four fields for any model', async () => {
    const { getByLabelText } = await renderWithProviders(
      <PriceFields
        mode="per_token"
        unit="credits"
        values={BASE_VALUES}
        errors={{}}
        quotaPerUnit={100}
        onChange={() => undefined}
        onUnitChange={() => undefined}
      />,
      ['common', 'admin'],
    )
    for (const label of [
      'Input price (cache miss)',
      'Input price (cache hit)',
      'Output price',
      'Cache write price (optional)',
    ]) {
      expect(getByLabelText(label)).toBeTruthy()
    }
  })

  it('explains the empty hit price per context', async () => {
    const model = await renderWithProviders(
      <PriceFields
        mode="per_token"
        unit="credits"
        values={BASE_VALUES}
        errors={{}}
        quotaPerUnit={100}
        onChange={() => undefined}
        onUnitChange={() => undefined}
      />,
      ['common', 'admin'],
    )
    expect(model.getByText('Empty = charged at the miss price')).toBeTruthy()
    model.unmount()

    const group = await renderWithProviders(
      <PriceFields
        mode="per_token"
        unit="credits"
        values={BASE_VALUES}
        errors={{}}
        quotaPerUnit={100}
        context="group"
        onChange={() => undefined}
        onUnitChange={() => undefined}
      />,
      ['common', 'admin'],
    )
    expect(group.getByText('Empty = 0 (free)')).toBeTruthy()
  })

  it('renders only the request price for per-request mode', async () => {
    const { getByLabelText, queryByLabelText, queryByText } = await renderWithProviders(
      <PriceFields
        mode="per_request"
        unit="credits"
        // 切模式不清值：残留的缓存价不能产生与按次计价无关的警告。
        values={{ prompt: '100', completion: '800', cacheRead: '150', cacheWrite: '' }}
        errors={{}}
        quotaPerUnit={100}
        onChange={() => undefined}
        onUnitChange={() => undefined}
      />,
      ['common', 'admin'],
    )
    expect(getByLabelText('Price per request')).toBeTruthy()
    expect(queryByLabelText('Input price (cache hit)')).toBeNull()
    expect(queryByText(/higher than the cache-miss price/)).toBeNull()
  })
})
