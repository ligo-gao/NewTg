import { within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { PageHeader, type PageHeaderCrumb } from './page-header'

const CRUMBS: readonly PageHeaderCrumb[] = [
  { label: 'Console', to: '/dashboard' },
  { label: 'API tokens' },
]

describe('PageHeader', () => {
  it('renders the title and the description', async () => {
    const { getByRole, getByText } = await renderWithProviders(
      <PageHeader
        crumbs={CRUMBS}
        title="API tokens"
        description="Manage the keys that call the gateway."
      />,
      ['common'],
    )

    expect(
      getByRole('heading', { level: 1, name: 'API tokens' }),
    ).toBeTruthy()
    expect(
      getByText('Manage the keys that call the gateway.'),
    ).toBeTruthy()
  })

  it('renders ancestor crumbs as links and the current crumb as text', async () => {
    const { getByRole } = await renderWithProviders(
      <PageHeader crumbs={CRUMBS} title="API tokens" />,
      ['common'],
    )

    const nav = getByRole('navigation', { name: 'Console' })
    const ancestor = within(nav).getByRole('link', { name: 'Console' })
    expect(ancestor.getAttribute('href')).toBe('/dashboard')
    expect(ancestor.hasAttribute('aria-current')).toBe(false)

    const current = within(nav).getByText('API tokens')
    expect(current.tagName).toBe('SPAN')
    expect(current.getAttribute('aria-current')).toBe('page')
    expect(nav.querySelectorAll('[aria-current="page"]')).toHaveLength(1)
    expect(
      within(nav).queryByRole('link', { name: 'API tokens' }),
    ).toBeNull()
  })

  it('marks only the final crumb as the current page', async () => {
    const { getByRole } = await renderWithProviders(
      <PageHeader
        crumbs={[
          { label: 'Console' },
          { label: 'Workspace' },
          { label: 'Dashboard' },
        ]}
        title="Dashboard"
      />,
      ['common'],
    )

    const nav = getByRole('navigation', { name: 'Console' })
    const current = nav.querySelectorAll('[aria-current="page"]')
    expect(current).toHaveLength(1)
    expect(current[0]).toBe(within(nav).getByText('Dashboard'))

    // Ancestors without a link are plain text, not additional current pages.
    for (const label of ['Console', 'Workspace']) {
      expect(within(nav).getByText(label).hasAttribute('aria-current')).toBe(
        false,
      )
    }
  })

  it('omits the description paragraph when none is given', async () => {
    const { getByTestId } = await renderWithProviders(
      <PageHeader crumbs={CRUMBS} title="API tokens" />,
      ['common'],
    )

    expect(getByTestId('page-header').querySelector('p')).toBeNull()
  })

  it('renders the actions slot', async () => {
    const { getByRole } = await renderWithProviders(
      <PageHeader
        crumbs={CRUMBS}
        title="API tokens"
        actions={
          <button type="button">
            Create token
          </button>
        }
      />,
      ['common'],
    )

    expect(getByRole('button', { name: 'Create token' })).toBeTruthy()
  })

  it('names the breadcrumb nav "Console" in English', async () => {
    const { getByRole } = await renderWithProviders(
      <PageHeader crumbs={CRUMBS} title="API tokens" />,
      ['common'],
    )

    expect(getByRole('navigation', { name: 'Console' })).toBeTruthy()
  })
})
