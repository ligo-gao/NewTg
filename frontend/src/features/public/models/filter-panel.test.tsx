import { fireEvent } from '@testing-library/react'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { ModelFilters } from './filter-panel'
import type { ModelsSearch } from './params'
import type { ModelFacets } from './taxonomy'

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

const FACETS: ModelFacets = {
  capabilities: [
    { value: 'text', label: 'text', count: 5 },
    { value: 'vision', label: 'vision', count: 3 },
    { value: 'tool_use', label: 'tool_use', count: 2 },
  ],
  vendors: [
    { value: 'openai', count: 3 },
    { value: 'anthropic', count: 2 },
  ],
  contexts: [
    { value: '8k', count: 1 },
    { value: '128k', count: 4 },
  ],
  endpoints: [{ value: 'chat.completions', count: 5 }],
}

/** The catalog's model_meta is empty: every facet comes back with no options. */
const EMPTY_FACETS: ModelFacets = {
  capabilities: [],
  vendors: [],
  contexts: [],
  endpoints: [],
}

type ChangeHandler = (next: ModelsSearch, replace?: boolean) => void

function Harness({
  initial,
  onChange,
  onClose,
  facets = FACETS,
}: {
  initial: ModelsSearch
  onChange: ChangeHandler
  onClose?: () => void
  facets?: ModelFacets
}) {
  const [search, setSearch] = useState(initial)
  return (
    <ModelFilters
      facets={facets}
      search={search}
      onChange={(next, replace) => {
        onChange(next, replace)
        setSearch(next)
      }}
      onClose={onClose}
    />
  )
}

async function renderFilters(
  initial: ModelsSearch = BASE,
  onClose?: () => void,
  facets: ModelFacets = FACETS,
) {
  const onChange = vi.fn<ChangeHandler>()
  const result = await renderWithProviders(
    <Harness
      initial={initial}
      onChange={onChange}
      onClose={onClose}
      facets={facets}
    />,
    ['public'],
  )
  const last = () => onChange.mock.lastCall?.[0] as ModelsSearch
  return { ...result, onChange, last }
}

describe('ModelFilters', () => {
  it('adds and removes one capability without clearing the rest', async () => {
    const initial: ModelsSearch = {
      ...BASE,
      cap: 'text',
      vendor: 'openai',
      measured: true,
      compare: 'a,b',
    }
    const { getByRole, last } = await renderFilters(initial)

    fireEvent.click(getByRole('checkbox', { name: 'Vision' }))
    expect(last().cap).toBe('text,vision')
    expect(last().vendor).toBe('openai')
    expect(last().measured).toBe(true)
    expect(last().compare).toBe('a,b')

    fireEvent.click(getByRole('checkbox', { name: 'Vision' }))
    expect(last().cap).toBe('text')
  })

  it('keeps an unknown capability as a removable selection', async () => {
    const { getByRole, last } = await renderFilters({
      ...BASE,
      cap: 'text,hologram',
    })

    const unknown = getByRole('checkbox', { name: 'hologram' }) as HTMLInputElement
    expect(unknown.checked).toBe(true)

    fireEvent.click(unknown)
    expect(last().cap).toBe('text')
  })

  it('resets every group, the search and the measured switch', async () => {
    const initial: ModelsSearch = {
      q: 'gpt',
      cap: 'text,vision',
      vendor: 'openai',
      context: '8k',
      endpoint: 'chat.completions',
      measured: true,
      sort: 'tps',
      range: '24h',
      view: 'cards',
      compare: 'a,b',
    }
    const { getByRole, last } = await renderFilters(initial)

    expect(
      (getByRole('switch', { name: 'Only models with measurements' }) as HTMLElement)
        .getAttribute('aria-checked'),
    ).toBe('true')

    fireEvent.click(getByRole('button', { name: 'Reset all' }))

    // Reset restores the defaults the route publishes on a bare `/models`
    // (sort=measured, range=7d, no compare selection); only the view
    // preference is display state and survives a reset.
    expect(last()).toEqual({
      q: '',
      cap: '',
      vendor: '',
      context: '',
      endpoint: '',
      measured: false,
      sort: 'measured',
      range: '7d',
      view: 'cards',
      compare: '',
    })
  })

  it('groups every checkbox under a fieldset legend', async () => {
    const { container } = await renderFilters()

    const groups = [...container.querySelectorAll('fieldset')]
    expect(groups.map((group) => group.querySelector('legend')?.textContent)).toEqual([
      'Capabilities',
      'Vendors',
      'Context window',
      'Call type',
    ])

    for (const group of groups) {
      const checkboxes = [...group.querySelectorAll('input[type="checkbox"]')]
      expect(checkboxes.length).toBeGreaterThan(0)
      for (const checkbox of checkboxes) {
        expect(checkbox.closest('fieldset')).toBe(group)
      }
    }
  })

  it('does not render a group with no options', async () => {
    const onlyVendors: ModelFacets = {
      ...EMPTY_FACETS,
      vendors: FACETS.vendors,
    }
    const { container, getByRole, queryByText } = await renderFilters(
      BASE,
      undefined,
      onlyVendors,
    )

    const groups = [...container.querySelectorAll('fieldset')]
    expect(groups).toHaveLength(1)
    expect(groups[0]?.querySelector('legend')?.textContent).toBe('Vendors')
    expect(getByRole('checkbox', { name: 'openai' })).toBeTruthy()
    expect(queryByText('Capabilities')).toBeNull()
  })

  it('renders one note instead of four empty groups when no profile exists', async () => {
    const { container, getByText, queryByText } = await renderFilters(
      BASE,
      undefined,
      EMPTY_FACETS,
    )

    expect(
      getByText('The operator has not filled in model profiles yet.'),
    ).toBeTruthy()
    expect(
      getByText(/No vendor, capability or context options are available/),
    ).toBeTruthy()
    expect(container.querySelectorAll('fieldset')).toHaveLength(0)
    for (const heading of [
      'Capabilities',
      'Vendors',
      'Context window',
      'Call type',
    ]) {
      expect(queryByText(heading)).toBeNull()
    }
  })

  it('always renders the data group', async () => {
    const { getByText, getByRole } = await renderFilters(
      BASE,
      undefined,
      EMPTY_FACETS,
    )

    expect(getByText('Data')).toBeTruthy()
    expect(
      getByRole('switch', { name: 'Only models with measurements' }),
    ).toBeTruthy()
  })

  it('disables reset when nothing is filtered', async () => {
    const idle = await renderFilters()
    expect(
      (idle.getByRole('button', { name: 'Reset all' }) as HTMLButtonElement)
        .disabled,
    ).toBe(true)
    idle.unmount()

    const filtered = await renderFilters({ ...BASE, cap: 'text' })
    expect(
      (filtered.getByRole('button', { name: 'Reset all' }) as HTMLButtonElement)
        .disabled,
    ).toBe(false)
  })

  it('calls onClose from the close button when provided', async () => {
    const withoutClose = await renderFilters()
    expect(withoutClose.queryByRole('button', { name: 'Close filters' })).toBeNull()
    withoutClose.unmount()

    const onClose = vi.fn()
    const { getByRole } = await renderFilters(BASE, onClose)
    fireEvent.click(getByRole('button', { name: 'Close filters' }))

    expect(onClose).toHaveBeenCalledTimes(1)
  })
})
