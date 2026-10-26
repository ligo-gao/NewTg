import { fireEvent } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import type { ModelsSearch } from './params'
import { Toolbar } from './toolbar'

const BASE: ModelsSearch = {
  q: '',
  cap: '',
  vendor: '',
  context: '',
  endpoint: '',
  measured: false,
  sort: 'measured',
  range: '7d',
  view: 'table',
  compare: '',
}

async function renderToolbar(
  search: ModelsSearch = BASE,
  resultCount = 12,
  activeFilterCount = 0,
  onChange = vi.fn(),
  onOpenFilters = vi.fn(),
) {
  const result = await renderWithProviders(
    <Toolbar
      search={search}
      resultCount={resultCount}
      activeFilterCount={activeFilterCount}
      onChange={onChange}
      onOpenFilters={onOpenFilters}
    />,
    ['public'],
  )
  return { onChange, onOpenFilters, ...result }
}

describe('Toolbar', () => {
  it('writes the trimmed search term on submit', async () => {
    const { getByRole, onChange } = await renderToolbar()
    const input = getByRole('searchbox')

    fireEvent.change(input, { target: { value: '  gpt  ' } })
    fireEvent.submit(getByRole('search'))
    expect(onChange).toHaveBeenLastCalledWith({ ...BASE, q: 'gpt' })

    fireEvent.change(input, { target: { value: 'gpt' } })
    fireEvent.keyDown(input, { key: 'Escape' })
    expect(onChange).toHaveBeenLastCalledWith({ ...BASE, q: '' })
  })

  it('switches the range and keeps every other parameter', async () => {
    const search: ModelsSearch = {
      ...BASE,
      q: 'vision',
      cap: 'vision',
      vendor: 'openai',
      context: '8k',
      endpoint: 'chat.completions',
      measured: true,
      sort: 'tps',
      range: '7d',
      view: 'cards',
      compare: 'a,b',
    }
    const { getByRole, onChange } = await renderToolbar(search)

    fireEvent.click(getByRole('button', { name: 'Last 24h' }))

    expect(onChange).toHaveBeenLastCalledWith({ ...search, range: '24h' })
  })

  it('writes sort and view changes', async () => {
    const { getByRole, onChange } = await renderToolbar()

    fireEvent.change(getByRole('combobox', { name: 'Sort by' }), {
      target: { value: 'latency' },
    })
    expect(onChange).toHaveBeenLastCalledWith({ ...BASE, sort: 'latency' })

    fireEvent.click(getByRole('button', { name: 'Cards' }))
    expect(onChange).toHaveBeenLastCalledWith({ ...BASE, view: 'cards' })
  })

  it('offers a price sort option and writes it back', async () => {
    const { getByRole, onChange } = await renderToolbar()
    const select = getByRole('combobox', { name: 'Sort by' })

    const option = Array.from(select.querySelectorAll('option')).find(
      (candidate) => candidate.value === 'price',
    )
    expect(option).toBeTruthy()

    fireEvent.change(select, { target: { value: 'price' } })
    expect(onChange).toHaveBeenLastCalledWith({ ...BASE, sort: 'price' })
  })

  it('shows the active filter count on the filter button', async () => {
    const { getByRole, getByText, onOpenFilters } = await renderToolbar(
      BASE,
      12,
      3,
    )

    expect(getByText('12 models')).toBeTruthy()
    fireEvent.click(getByRole('button', { name: 'Filters (3)' }))

    expect(onOpenFilters).toHaveBeenCalledTimes(1)
  })

  it('hides the filter button when no sheet can open', async () => {
    const { queryByRole } = await renderWithProviders(
      <Toolbar
        search={BASE}
        resultCount={12}
        activeFilterCount={3}
        onChange={vi.fn()}
      />,
      ['public'],
    )

    // The wide layout keeps the filter panel on screen, so a button that only
    // opens a sheet would be a dead control.
    expect(queryByRole('button', { name: 'Filters (3)' })).toBeNull()
    expect(queryByRole('button', { name: 'Filters' })).toBeNull()
  })
})
