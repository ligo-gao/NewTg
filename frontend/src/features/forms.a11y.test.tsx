import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import { screen } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'

import { SignInPage } from '@/features/auth/SignInPage'
import { TopupPage } from '@/features/account/topup/topup-page'
import { PasswordField, TextField } from '@/shared/ui/form'
import { renderWithProviders } from '@/test-support/providers'

/**
 * UI-5 form accessibility:
 *  - every input/select/textarea on representative M1–M6 pages has an
 *    associated accessible name;
 *  - validation errors are wired through `aria-invalid` + `aria-describedby`;
 *  - icon-only controls (password reveal) expose an accessible name.
 */

const ORIGIN = 'http://localhost:3000'
const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

const PUBLIC_STATUS = {
  auth: {
    signup_enabled: true,
    password_login_enabled: true,
    email_verification_required: false,
    turnstile: { enabled: false, site_key: '' },
  },
  password_login_encryption_enabled: false,
  languages: ['en'],
  default_language: 'en',
}

const TOPUP_CONFIG = {
  amount_options: [1000, 5000, 10000],
  min_topup: 1000,
  max_topup: 500000,
  quota_per_unit: 100,
  credits_ratio: 1,
  enabled_providers: ['stripe', 'alipay'],
  providers: {
    stripe: { publishable_key: 'pk_test_123', min_topup: 1000 },
    alipay: { min_topup: 1000 },
  },
}

const WALLET = {
  balance: 5000,
  held_credits: 0,
  total_consumed: 0,
  total_requests: 0,
  threshold: 1000,
  health: 'healthy' as const,
  group: 'default',
}

function controlName(control: Element): string {
  const ariaLabel = control.getAttribute('aria-label')
  if (ariaLabel !== null && ariaLabel !== '') return ariaLabel

  const labelledBy = control.getAttribute('aria-labelledby')
  if (labelledBy !== null) {
    const text = labelledBy
      .split(/\s+/)
      .map((id) => document.getElementById(id)?.textContent ?? '')
      .join(' ')
      .trim()
    if (text !== '') return text
  }

  const labels = (control as HTMLInputElement).labels
  if (labels !== null && labels !== undefined && labels.length > 0) {
    const text = Array.from(labels)
      .map((label) => label.textContent ?? '')
      .join(' ')
      .trim()
    if (text !== '') return text
  }

  return control.getAttribute('title') ?? ''
}

function expectEveryControlNamed(): void {
  const controls = Array.from(
    document.querySelectorAll<HTMLElement>('input, select, textarea'),
  ).filter((control) => {
    const type = control.getAttribute('type')
    return type !== 'hidden' && type !== 'submit' && type !== 'button'
  })

  expect(controls.length).toBeGreaterThan(0)
  for (const control of controls) {
    expect(
      controlName(control),
      `unlabelled <${control.tagName.toLowerCase()}> ${control.outerHTML}`,
    ).not.toBe('')
  }
}

describe('form label association (UI-5)', () => {
  it('every control on the sign-in page has an accessible name', async () => {
    server.use(
      http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(PUBLIC_STATUS)),
      http.get(`${ORIGIN}/api/me`, () =>
        HttpResponse.json({ type: 'unauthorized', title: 'Unauthorized' }, { status: 401 }),
      ),
    )
    const { container } = await renderWithProviders(
      <SignInPage onNavigate={() => {}} />,
      ['common', 'auth'],
    )

    await screen.findByLabelText('Username or email')
    expectEveryControlNamed()
    expect(container.querySelectorAll('input').length).toBeGreaterThanOrEqual(2)
  })

  it('every control on the top-up page has an accessible name', async () => {
    server.use(
      http.get(`${ORIGIN}/api/topup/config`, () => HttpResponse.json(TOPUP_CONFIG)),
      http.get(`${ORIGIN}/api/wallet/summary`, () => HttpResponse.json(WALLET)),
    )
    await renderWithProviders(<TopupPage />, ['common', 'account'])

    await screen.findByRole('radio', { name: '¥10' })
    expectEveryControlNamed()
  })

  it('wires error text through aria-invalid and aria-describedby', async () => {
    await renderWithProviders(
      <TextField label="Amount" error="Amount is required" />,
      [],
    )

    const input = screen.getByLabelText('Amount')
    expect(input.getAttribute('aria-invalid')).toBe('true')
    const describedBy = input.getAttribute('aria-describedby')
    expect(describedBy).toBeTruthy()
    expect(document.getElementById(describedBy ?? '')?.textContent).toBe(
      'Amount is required',
    )
  })

  it('gives the password reveal icon button an accessible name', async () => {
    await renderWithProviders(
      <PasswordField
        label="Password"
        showLabel="Show password"
        hideLabel="Hide password"
      />,
      [],
    )

    expect(screen.getByRole('button', { name: 'Show password' })).toBeTruthy()
  })
})
