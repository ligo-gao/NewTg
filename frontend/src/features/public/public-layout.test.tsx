import { describe, expect, it } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { PublicLayout } from './public-layout'

describe('PublicLayout navigation', () => {
  it('wires the public nav and marks the active page', async () => {
    const { getByRole } = await renderWithProviders(
      <PublicLayout activePath="/about">
        <p>content</p>
      </PublicLayout>,
      ['public'],
    )

    const nav = getByRole('navigation', { name: 'Public pages' })
    expect(nav.textContent).toContain('About')
    expect(nav.textContent).toContain('Home')

    const about = getByRole('link', { name: 'About' })
    expect(about.getAttribute('href')).toBe('/about')
    expect(about.getAttribute('aria-current')).toBe('page')
    expect(getByRole('link', { name: 'Sign in' }).getAttribute('href')).toBe('/sign-in')
  })
})
