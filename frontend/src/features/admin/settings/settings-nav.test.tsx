import { fireEvent, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import type { SettingsTab } from './sections'
import { SettingsNav } from './settings-nav'

interface HarnessProps {
  initial?: SettingsTab
  dirtySections?: ReadonlySet<string>
}

function Harness({ initial = 'site', dirtySections }: HarnessProps) {
  const [tab, setTab] = useState<SettingsTab>(initial)
  return <SettingsNav tab={tab} onTabChange={setTab} dirtySections={dirtySections} />
}

async function renderNav(props: HarnessProps = {}) {
  const result = await renderWithProviders(<Harness {...props} />, [
    'common',
    'settings',
  ])
  const nav = screen.getByRole('navigation', { name: 'Settings sections' })
  return { ...result, nav }
}

describe('SettingsNav', () => {
  it('renders the six groups and seven items in the documented order', async () => {
    const { nav } = await renderNav()

    const headings = Array.from(nav.querySelectorAll('div')).map(
      (element) => element.textContent,
    )
    expect(headings).toContain('Site & brand')
    expect(headings).toContain('Authentication')
    expect(headings).toContain('Billing & payments')
    expect(headings).toContain('Security & limits')
    expect(headings).toContain('Notifications')
    expect(headings).toContain('Request policy')

    expect(
      within(nav)
        .getAllByRole('button')
        .map((button) => button.textContent),
    ).toEqual([
      'Site',
      'Authentication',
      'Billing',
      'Payments',
      'Rate limits & security',
      'Notifications',
      'Request policies',
    ])
  })

  it('marks the active tab with aria-current and never uses tab roles', async () => {
    const { nav } = await renderNav()

    expect(
      within(nav).getByRole('button', { name: 'Site' }).getAttribute('aria-current'),
    ).toBe('page')
    expect(
      within(nav)
        .getByRole('button', { name: 'Payments' })
        .hasAttribute('aria-current'),
    ).toBe(false)
    expect(screen.queryByRole('tab')).toBeNull()
    expect(screen.queryByRole('tablist')).toBeNull()
  })

  it('reports the clicked tab and leaves switching to the shell', async () => {
    const user = userEvent.setup()
    const onTabChange = vi.fn()
    await renderWithProviders(
      <SettingsNav tab="site" onTabChange={onTabChange} />,
      ['common', 'settings'],
    )

    await user.click(screen.getByRole('button', { name: 'Billing' }))
    expect(onTabChange).toHaveBeenCalledWith('billing')
  })

  it('shows a warning dot only on tabs with a dirty section', async () => {
    const { nav } = await renderNav({ dirtySections: new Set(['pay']) })

    const payment = within(nav).getByRole('button', { name: 'Payments' })
    const dot = within(payment).getByTestId('settings-nav-dirty')
    expect(dot.getAttribute('aria-hidden')).toBe('true')
    expect(dot.className).toContain('bg-warning')

    expect(
      within(within(nav).getByRole('button', { name: 'Site' })).queryByTestId(
        'settings-nav-dirty',
      ),
    ).toBeNull()
  })

  it('moves focus and selection vertically with the arrow, Home and End keys', async () => {
    const { nav } = await renderNav()

    const site = within(nav).getByRole('button', { name: 'Site' })
    site.focus()
    fireEvent.keyDown(nav, { key: 'ArrowDown' })
    const auth = within(nav).getByRole('button', { name: 'Authentication' })
    expect(auth.getAttribute('aria-current')).toBe('page')
    expect(document.activeElement).toBe(auth)

    fireEvent.keyDown(nav, { key: 'End' })
    expect(
      within(nav)
        .getByRole('button', { name: 'Request policies' })
        .getAttribute('aria-current'),
    ).toBe('page')

    fireEvent.keyDown(nav, { key: 'Home' })
    expect(
      within(nav).getByRole('button', { name: 'Site' }).getAttribute('aria-current'),
    ).toBe('page')

    fireEvent.keyDown(nav, { key: 'ArrowUp' })
    expect(
      within(nav)
        .getByRole('button', { name: 'Request policies' })
        .getAttribute('aria-current'),
    ).toBe('page')
  })
})
