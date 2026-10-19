import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { Breadcrumb } from './breadcrumb'

describe('Breadcrumb (F-09-36)', () => {
  it('renders nothing for a depth-1 page', async () => {
    const { queryByRole } = await renderWithProviders(
      <Breadcrumb items={[{ label: 'Home', path: '/', current: true }]} />,
      ['public'],
    )

    expect(queryByRole('navigation')).toBeNull()
  })

  it('renders a nav landmark with the current page marked and not clickable', async () => {
    const { getByRole, getByText, queryByRole } = await renderWithProviders(
      <Breadcrumb
        items={[
          { label: 'Home', path: '/' },
          { label: 'Pricing', path: '/pricing' },
          { label: 'GPT-4o', current: true },
        ]}
      />,
      ['public'],
    )

    const nav = getByRole('navigation', { name: 'Breadcrumb' })
    expect(nav.textContent).toContain('Pricing')

    const current = getByText('GPT-4o')
    expect(current.getAttribute('aria-current')).toBe('page')
    // 末级是当前页，不能是链接。
    expect(current.closest('a')).toBeNull()
    expect(queryByRole('link', { name: 'GPT-4o' })).toBeNull()

    expect(getByRole('link', { name: 'Home' }).getAttribute('href')).toBe('/')
    expect(getByRole('link', { name: 'Pricing' }).getAttribute('href')).toBe('/pricing')
  })

  it('renders disabled middle levels as non-link text with a hint', async () => {
    const { getByText, queryByRole } = await renderWithProviders(
      <Breadcrumb
        items={[
          { label: 'Home', path: '/' },
          { label: 'Pricing', disabled: true, disabledHint: 'This page is not open' },
          { label: 'GPT-4o', current: true },
        ]}
      />,
      ['public'],
    )

    const middle = getByText('Pricing')
    expect(middle.getAttribute('aria-disabled')).toBe('true')
    expect(middle.getAttribute('title')).toBe('This page is not open')
    expect(queryByRole('link', { name: 'Pricing' })).toBeNull()
  })

  it('folds middle levels into an expandable ellipsis when the chain is longer than three', async () => {
    const { getByLabelText, getByRole } = await renderWithProviders(
      <Breadcrumb
        items={[
          { label: 'Home', path: '/' },
          { label: 'Pricing', path: '/pricing' },
          { label: 'Author', path: '/authors/acme' },
          { label: 'GPT-4o', current: true },
        ]}
      />,
      ['public'],
    )

    expect(getByLabelText('Show hidden breadcrumb levels')).toBeTruthy()
    // 被折叠的层级仍在文档中，展开后可点击。
    expect(getByRole('link', { name: 'Author' }).getAttribute('href')).toBe('/authors/acme')
    expect(getByRole('link', { name: 'Home' })).toBeTruthy()
  })
})
