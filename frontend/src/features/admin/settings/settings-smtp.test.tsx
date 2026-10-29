import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import type { SettingsItem, SettingsSectionResponse } from './api'
import { tabConfig } from './sections'
import { SettingsSectionForm } from './settings-section-form'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  vi.restoreAllMocks()
  server.resetHandlers()
})
afterAll(() => server.close())

const CONFIG = tabConfig('notify').sections.find((entry) => entry.id === 'notify.smtp')!

function item(key: string, value: unknown, overrides: Partial<SettingsItem> = {}): SettingsItem {
  return {
    key,
    value,
    type: typeof value === 'number' ? 'number' : 'string',
    default: value,
    public: false,
    secret: false,
    dangerous: false,
    ...overrides,
  }
}

function smtpState(overrides: Partial<Record<string, unknown>> = {}): SettingsSectionResponse {
  const values: Record<string, unknown> = {
    'notify.smtp.host': 'smtp.example.com',
    'notify.smtp.port': 587,
    'notify.smtp.encryption': 'starttls',
    'notify.smtp.username': 'bot@example.com',
    'notify.smtp.from_email': 'bot@example.com',
    ...overrides,
  }
  return {
    section: 'notify.smtp',
    updated_at: '2026-10-19T10:00:00Z',
    items: Object.entries(values).map(([key, value]) => item(key, value)),
  }
}

interface Harness {
  tests: number
}

/** Installs the settings GET plus the SMTP probe; `onTest` overrides the answer. */
function installHandlers(
  state: SettingsSectionResponse = smtpState(),
  onTest?: () => Response | Promise<Response>,
): Harness {
  const harness: Harness = { tests: 0 }

  server.use(
    http.get(`${ORIGIN}/api/settings`, ({ request }) => {
      const section = new URL(request.url).searchParams.get('section')
      if (section !== 'notify.smtp') {
        return HttpResponse.json({ type: 'about:blank', title: 'unknown section' }, { status: 404 })
      }
      return HttpResponse.json<SettingsSectionResponse>(state)
    }),
    http.post(`${ORIGIN}/api/settings/test/smtp`, () => {
      harness.tests += 1
      if (onTest !== undefined) return onTest()
      return HttpResponse.json({ ok: true, steps: [] })
    }),
  )

  return harness
}

async function renderForm(harness: Harness = installHandlers()) {
  await renderWithProviders(
    <SettingsSectionForm config={CONFIG} readOnly={false} onDirtyChange={vi.fn()} />,
    ['settings'],
  )
  await screen.findByTestId('settings-section-notify.smtp')
  // The section shell renders a skeleton while the snapshot loads; the test
  // button only exists once the saved values are in.
  await screen.findByRole('button', { name: 'Test connection' })
  return harness
}

describe('SMTP settings section', () => {
  it('renders every executed stage in order with reason and SMTP code', async () => {
    const user = userEvent.setup()
    const harness = await renderForm(
      installHandlers(smtpState(), () =>
        HttpResponse.json({
          ok: false,
          steps: [
            { stage: 'dns', status: 'passed' },
            { stage: 'connect', status: 'passed' },
            { stage: 'tls', status: 'passed' },
            { stage: 'greeting', status: 'passed' },
            {
              stage: 'auth',
              status: 'failed',
              reason: 'auth_failed',
              smtp_code: 535,
            },
          ],
        }),
      ),
    )

    await user.click(screen.getByRole('button', { name: 'Test connection' }))

    expect(
      await screen.findByText(
        'Stopped at "Sign-in": Sign-in failed. Check the account and password.',
      ),
    ).toBeTruthy()
    expect(harness.tests).toBe(1)

    const steps = screen.getByRole('list', { name: 'Connection test steps' })
    const rows = Array.from(steps.querySelectorAll('li'))
    expect(rows).toHaveLength(5)
    expect(rows[0]?.textContent ?? '').toContain('DNS lookup')
    expect(rows[4]?.textContent ?? '').toContain('Sign-in')
    expect(rows[4]?.textContent ?? '').toContain('Code 535')
  })

  it('reports a usable configuration without claiming a mail was sent', async () => {
    const user = userEvent.setup()
    await renderForm(
      installHandlers(smtpState(), () =>
        HttpResponse.json({
          ok: true,
          steps: [
            { stage: 'dns', status: 'passed' },
            { stage: 'connect', status: 'passed' },
            { stage: 'tls', status: 'skipped' },
            { stage: 'greeting', status: 'passed' },
            { stage: 'auth', status: 'passed' },
            { stage: 'mail_from', status: 'passed' },
          ],
        }),
      ),
    )

    await user.click(screen.getByRole('button', { name: 'Test connection' }))

    expect(await screen.findByText('SMTP configuration is usable. No email was sent.')).toBeTruthy()
    expect(screen.getByText('Skipped')).toBeTruthy()
  })

  it('disables the button until the saved snapshot has a host and sender', async () => {
    await renderForm(installHandlers(smtpState({ 'notify.smtp.host': '', 'notify.smtp.username': '' })))

    const button = screen.getByRole('button', { name: 'Test connection' }) as HTMLButtonElement
    expect(button.disabled).toBe(true)
    expect(
      screen.getByText('Save the server address and sender address before testing.'),
    ).toBeTruthy()
  })

  it('explains a 422 when the saved configuration is incomplete', async () => {
    const user = userEvent.setup()
    await renderForm(
      installHandlers(smtpState(), () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/smtp_not_configured',
            title: 'SMTP is not configured',
            status: 422,
            detail: 'configure notify.smtp.host and notify.smtp.from_email before testing',
          },
          { status: 422 },
        ),
      ),
    )

    await user.click(screen.getByRole('button', { name: 'Test connection' }))

    expect(
      await screen.findByText('Save the SMTP server address and sender address first.'),
    ).toBeTruthy()
  })

  it('never renders a missing-key placeholder for an unknown failure reason', async () => {
    const user = userEvent.setup()
    await renderForm(
      installHandlers(smtpState(), () =>
        HttpResponse.json({
          ok: false,
          steps: [{ stage: 'greeting', status: 'failed', reason: 'quota_exceeded' }],
        }),
      ),
    )

    await user.click(screen.getByRole('button', { name: 'Test connection' }))

    await waitFor(() => {
      expect(screen.getByText('quota_exceeded')).toBeTruthy()
    })
    expect(document.body.textContent).not.toContain('settings.smtp.test.reason')
  })
})
