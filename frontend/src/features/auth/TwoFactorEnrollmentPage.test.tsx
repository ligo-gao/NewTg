import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { TwoFactorEnrollmentPage } from './TwoFactorEnrollmentPage'

const ORIGIN = 'http://localhost:3000'
const SETUP_URL = `${ORIGIN}/api/auth/2fa/enroll/setup`
const ENABLE_URL = `${ORIGIN}/api/auth/2fa/enroll/enable`
const CONFIRM_URL = `${ORIGIN}/api/auth/2fa/enroll/confirm`

/** 共享页脚读取的公开状态；绑定页本身不依赖这些开关。 */
const PUBLIC_STATUS = {
  system_initialized: true,
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

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
beforeEach(() => {
  server.use(
    http.get(`${ORIGIN}/api/status`, () => HttpResponse.json(PUBLIC_STATUS)),
    http.get(`${ORIGIN}/api/me`, () =>
      HttpResponse.json({ type: 'unauthorized', title: 'Unauthorized' }, { status: 401 }),
    ),
  )
})
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

async function renderEnrollment(overrides?: {
  ticket?: string
  redirect?: string
}) {
  const onNavigate = vi.fn()
  const result = await renderWithProviders(
    <TwoFactorEnrollmentPage
      ticket={overrides?.ticket ?? 'enroll-ticket'}
      redirect={overrides?.redirect ?? '/dashboard'}
      onNavigate={onNavigate}
    />,
    ['common', 'auth'],
  )
  return { ...result, user: userEvent.setup(), onNavigate }
}

async function typeCode(user: ReturnType<typeof userEvent.setup>, container: HTMLElement) {
  const boxes = container.querySelectorAll<HTMLInputElement>('[data-otp-input]')
  expect(boxes).toHaveLength(6)
  await user.click(boxes[0]!)
  await user.paste('123456')
}

function tickStored(getByLabelText: (name: string) => HTMLElement) {
  return getByLabelText('I have stored my backup codes in a safe place')
}

describe('TwoFactorEnrollmentPage', () => {
  it('walks scan → verify → codes and only enables after the backup codes are confirmed', async () => {
    let enableBody: Record<string, unknown> | undefined
    let confirmBody: Record<string, unknown> | undefined

    server.use(
      http.post(SETUP_URL, () =>
        HttpResponse.json({
          secret: 'JBSWY3DPEHPK3PXP',
          otpauth_url: 'otpauth://totp/NewTG:admin?secret=JBSWY3DPEHPK3PXP',
        }),
      ),
      http.post(ENABLE_URL, async ({ request }) => {
        enableBody = (await request.json()) as Record<string, unknown>
        return HttpResponse.json({
          status: 'backup_codes_ready',
          backup_codes: ['AAAA-1111', 'BBBB-2222', 'CCCC-3333', 'DDDD-4444'],
        })
      }),
      http.post(CONFIRM_URL, async ({ request }) => {
        confirmBody = (await request.json()) as Record<string, unknown>
        return HttpResponse.json({
          user: {
            id: 1,
            username: 'admin',
            role: 'admin',
            email_verified: true,
            must_change_password: false,
          },
        })
      }),
    )

    const { getByRole, getByText, getByLabelText, findByRole, container, user, onNavigate } =
      await renderEnrollment()

    // Scan step comes from the setup call, never from client-side generation.
    expect(await findByRole('heading', { name: 'Add this key to your authenticator' })).toBeTruthy()
    expect((getByLabelText('Setup key') as HTMLInputElement).value).toBe(
      'JBSWY3DPEHPK3PXP',
    )
    await user.click(getByRole('button', { name: 'Next' }))

    expect(getByRole('heading', { name: 'Verify your authenticator' })).toBeTruthy()
    await typeCode(user, container)
    await user.click(getByRole('button', { name: 'Verify' }))

    expect(await findByRole('heading', { name: 'Save your backup codes' })).toBeTruthy()
    expect(getByText('AAAA-1111')).toBeTruthy()
    expect(enableBody).toEqual({
      ticket: 'enroll-ticket',
      secret: 'JBSWY3DPEHPK3PXP',
      code: '123456',
    })

    // R-04 第 4 条：未勾选确认之前，Finish 必须保持禁用，且不得调用 confirm。
    const finish = getByRole('button', { name: 'Finish setup' }) as HTMLButtonElement
    expect(finish.disabled).toBe(true)
    expect(confirmBody).toBeUndefined()

    await user.click(tickStored(getByLabelText))
    expect(finish.disabled).toBe(false)
    await user.click(finish)

    await waitFor(() => {
      expect(confirmBody).toEqual({
        ticket: 'enroll-ticket',
        backup_codes_confirmed: true,
      })
    })
    await waitFor(() => {
      expect(onNavigate).toHaveBeenCalledWith('/dashboard')
    })
  })

  it('offers a backup code download before the confirmation checkbox', async () => {
    server.use(
      http.post(SETUP_URL, () =>
        HttpResponse.json({
          secret: 'JBSWY3DPEHPK3PXP',
          otpauth_url: 'otpauth://totp/NewTG:admin?secret=JBSWY3DPEHPK3PXP',
        }),
      ),
      http.post(ENABLE_URL, () =>
        HttpResponse.json({
          status: 'backup_codes_ready',
          backup_codes: ['AAAA-1111', 'BBBB-2222'],
        }),
      ),
    )

    const { getByRole, findByRole, container, user } = await renderEnrollment()

    expect(await findByRole('heading', { name: 'Add this key to your authenticator' })).toBeTruthy()
    await user.click(getByRole('button', { name: 'Next' }))
    await typeCode(user, container)
    await user.click(getByRole('button', { name: 'Verify' }))

    expect(await findByRole('heading', { name: 'Save your backup codes' })).toBeTruthy()
    expect(getByRole('button', { name: 'Download backup codes' })).toBeTruthy()
  })

  it('treats an invalid ticket as terminal and offers a way back to sign-in', async () => {
    server.use(
      http.post(SETUP_URL, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/two_factor_ticket_invalid',
            title: 'Invalid or expired verification ticket',
          },
          { status: 400 },
        ),
      ),
    )

    const { findByText, getByRole, user, onNavigate } = await renderEnrollment()

    expect(await findByText('Enrollment link expired')).toBeTruthy()
    await user.click(getByRole('button', { name: 'Back to sign in' }))
    expect(onNavigate).toHaveBeenCalledWith('/sign-in')
  })

  it('keeps the verify form on a wrong code and shows the error', async () => {
    server.use(
      http.post(SETUP_URL, () =>
        HttpResponse.json({
          secret: 'JBSWY3DPEHPK3PXP',
          otpauth_url: 'otpauth://totp/NewTG:admin?secret=JBSWY3DPEHPK3PXP',
        }),
      ),
      http.post(ENABLE_URL, () =>
        HttpResponse.json(
          { type: 'https://newtg.dev/problems/bad_request', title: 'Incorrect verification code' },
          { status: 400 },
        ),
      ),
    )

    const { getByRole, findByRole, user, container } = await renderEnrollment()

    expect(await findByRole('heading', { name: 'Add this key to your authenticator' })).toBeTruthy()
    await user.click(getByRole('button', { name: 'Next' }))
    await typeCode(user, container)
    await user.click(getByRole('button', { name: 'Verify' }))

    expect(await findByRole('alert')).toBeTruthy()
    expect(getByRole('heading', { name: 'Verify your authenticator' })).toBeTruthy()
  })

  it('never calls the API without a ticket', async () => {
    const { findByText, getByRole } = await renderEnrollment({ ticket: '' })

    expect(await findByText('Enrollment link expired')).toBeTruthy()
    expect(getByRole('button', { name: 'Back to sign in' })).toBeTruthy()
  })
})
