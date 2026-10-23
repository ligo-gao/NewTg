import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { waitFor } from '@testing-library/react'
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from 'vitest'

import { translate, type TranslateValues } from '@/app/providers/i18n'
import { useToastStore } from '@/shared/ui/toast'
import { renderWithProviders } from '@/test-support/providers'

import { AccountEmailDialog } from './AccountEmailDialog'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

/**
 * Copy is derived from the same translation function the component uses, so
 * the assertions never hard-code English wording (or the missing-key fallback
 * while the locale files are still catching up).
 */
function copy(key: string, values?: TranslateValues): string {
  return translate('en', key, values)
}

const emailSentBody = {
  email: 'new@example.com',
  email_verified: true,
  email_verified_at: '2026-10-23T00:00:00Z',
}

describe('AccountEmailDialog', () => {
  let errorSpy: ReturnType<typeof vi.spyOn>

  beforeEach(() => {
    useToastStore.setState({ toasts: [] })
    errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
  })

  afterEach(() => {
    expect(errorSpy).not.toHaveBeenCalled()
    errorSpy.mockRestore()
  })

  it('blocks sending and confirming while the new email is invalid', async () => {
    let sendCalls = 0
    let changeCalls = 0
    server.use(
      http.post(`${ORIGIN}/api/auth/email-code`, () => {
        sendCalls += 1
        return new HttpResponse(null, { status: 204 })
      }),
      http.put(`${ORIGIN}/api/email`, () => {
        changeCalls += 1
        return HttpResponse.json(emailSentBody)
      }),
    )

    const onOpenChange = vi.fn()
    const onChanged = vi.fn()
    const { getByLabelText, getByRole, findByText } = await renderWithProviders(
      <AccountEmailDialog
        open
        currentEmail="old@example.com"
        onOpenChange={onOpenChange}
        onChanged={onChanged}
      />,
      ['common', 'account', 'auth'],
    )
    const user = userEvent.setup()

    // The current address is shown read-only for context.
    expect((getByLabelText(copy('account.profile.current_email')) as HTMLInputElement).value).toBe(
      'old@example.com',
    )

    await user.type(getByLabelText(copy('account.profile.new_email')), 'not-an-email')

    await user.click(getByRole('button', { name: copy('account.profile.send_code') }))
    expect(await findByText(copy('account.profile.email_invalid'))).toBeTruthy()

    await user.type(getByLabelText(copy('account.profile.email_code')), '123456')
    await user.type(getByLabelText(copy('account.profile.password')), 'secret-pass')
    await user.click(
      getByRole('button', { name: copy('account.profile.confirm_change') }),
    )

    await waitFor(() => {
      expect(sendCalls).toBe(0)
      expect(changeCalls).toBe(0)
    })
    expect(onChanged).not.toHaveBeenCalled()
    expect(onOpenChange).not.toHaveBeenCalled()
  })

  it('shows a 60-second resend countdown after sending a code', async () => {
    let sendCalls = 0
    server.use(
      http.post(`${ORIGIN}/api/auth/email-code`, () => {
        sendCalls += 1
        return new HttpResponse(null, { status: 204 })
      }),
    )

    const { getByLabelText, getByRole } = await renderWithProviders(
      <AccountEmailDialog
        open
        currentEmail={null}
        onOpenChange={vi.fn()}
        onChanged={vi.fn()}
      />,
      ['common', 'account', 'auth'],
    )
    const user = userEvent.setup()

    await user.type(getByLabelText(copy('account.profile.new_email')), 'new@example.com')
    await user.click(getByRole('button', { name: copy('account.profile.send_code') }))

    await waitFor(() => {
      expect(sendCalls).toBe(1)
    })

    const resend = getByRole('button', {
      name: copy('account.profile.resend_in', { seconds: 60 }),
    }) as HTMLButtonElement
    expect(resend.disabled).toBe(true)

    await waitFor(() => {
      expect(
        useToastStore
          .getState()
          .toasts.some((toast) => toast.message === copy('account.profile.email_sent')),
      ).toBe(true)
    })
  })

  it('submits the change, reports the new address and closes on success', async () => {
    const bodies: unknown[] = []
    server.use(
      http.post(`${ORIGIN}/api/auth/email-code`, () => new HttpResponse(null, { status: 204 })),
      http.put(`${ORIGIN}/api/email`, async ({ request }) => {
        bodies.push(await request.json())
        return HttpResponse.json(emailSentBody)
      }),
    )

    const onOpenChange = vi.fn()
    const onChanged = vi.fn()
    const { getByLabelText, getByRole } = await renderWithProviders(
      <AccountEmailDialog
        open
        currentEmail="old@example.com"
        onOpenChange={onOpenChange}
        onChanged={onChanged}
      />,
      ['common', 'account', 'auth'],
    )
    const user = userEvent.setup()

    await user.type(getByLabelText(copy('account.profile.new_email')), 'new@example.com')
    await user.click(getByRole('button', { name: copy('account.profile.send_code') }))
    await user.type(getByLabelText(copy('account.profile.email_code')), '123456')
    await user.type(getByLabelText(copy('account.profile.password')), 'secret-pass')
    await user.type(getByLabelText(copy('account.profile.totp_code')), '654321')

    await user.click(
      getByRole('button', { name: copy('account.profile.confirm_change') }),
    )

    await waitFor(() => {
      expect(onChanged).toHaveBeenCalledWith('new@example.com')
    })
    expect(onOpenChange).toHaveBeenCalledWith(false)
    expect(bodies).toEqual([
      {
        new_email: 'new@example.com',
        code: '123456',
        current_password: 'secret-pass',
        totp_code: '654321',
      },
    ])
    expect(
      useToastStore
        .getState()
        .toasts.some((toast) => toast.message === copy('account.profile.email_saved')),
    ).toBe(true)
  })

  it('keeps the dialog open and reports an inline error when the change fails', async () => {
    server.use(
      http.post(`${ORIGIN}/api/auth/email-code`, () => new HttpResponse(null, { status: 204 })),
      http.put(`${ORIGIN}/api/email`, () =>
        HttpResponse.json(
          {
            status: 403,
            type: 'https://newtg.dev/problems/reauth_required',
            title: 'Reauthentication required',
          },
          { status: 403 },
        ),
      ),
    )

    const onOpenChange = vi.fn()
    const onChanged = vi.fn()
    const { getByLabelText, getByRole, findByText, findByRole } =
      await renderWithProviders(
        <AccountEmailDialog
          open
          currentEmail="old@example.com"
          onOpenChange={onOpenChange}
          onChanged={onChanged}
        />,
        ['common', 'account', 'auth'],
      )
    const user = userEvent.setup()

    await user.type(getByLabelText(copy('account.profile.new_email')), 'new@example.com')
    await user.click(getByRole('button', { name: copy('account.profile.send_code') }))
    await user.type(getByLabelText(copy('account.profile.email_code')), '123456')
    await user.type(getByLabelText(copy('account.profile.password')), 'wrong-pass')
    await user.click(
      getByRole('button', { name: copy('account.profile.confirm_change') }),
    )

    expect(await findByText(copy('account.profile.email_error'))).toBeTruthy()
    expect(await findByRole('dialog')).toBeTruthy()
    expect(onChanged).not.toHaveBeenCalled()
    expect(onOpenChange).not.toHaveBeenCalled()
  })
})
