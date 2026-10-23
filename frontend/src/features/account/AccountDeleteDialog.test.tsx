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

import { AccountDeleteDialog } from './AccountDeleteDialog'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function copy(key: string, values?: TranslateValues): string {
  return translate('en', key, values)
}

describe('AccountDeleteDialog', () => {
  let errorSpy: ReturnType<typeof vi.spyOn>

  beforeEach(() => {
    useToastStore.setState({ toasts: [] })
    errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
  })

  afterEach(() => {
    expect(errorSpy).not.toHaveBeenCalled()
    errorSpy.mockRestore()
  })

  it('keeps confirm disabled until the username matches and a password is typed', async () => {
    const { getByLabelText, getByRole } = await renderWithProviders(
      <AccountDeleteDialog
        open
        username="alice"
        onOpenChange={vi.fn()}
        onDeleted={vi.fn()}
      />,
      ['common', 'account', 'auth'],
    )
    const user = userEvent.setup()

    const confirm = getByRole('button', {
      name: copy('account.profile.delete_confirm'),
    }) as HTMLButtonElement
    expect(confirm.disabled).toBe(true)

    const usernameInput = getByLabelText(
      copy('account.profile.delete_confirm_label', { username: 'alice' }),
    )

    await user.type(usernameInput, 'alic')
    expect(confirm.disabled).toBe(true)

    await user.type(usernameInput, 'e')
    // Username now matches, but the password is still empty.
    expect(confirm.disabled).toBe(true)

    await user.type(getByLabelText(copy('account.profile.delete_password')), 'secret-pass')
    await waitFor(() => {
      expect(confirm.disabled).toBe(false)
    })
  })

  it('deletes the account, calls onDeleted and closes on success', async () => {
    const bodies: unknown[] = []
    server.use(
      http.delete(`${ORIGIN}/api/account`, async ({ request }) => {
        bodies.push(await request.json())
        return new HttpResponse(null, { status: 204 })
      }),
    )

    const onOpenChange = vi.fn()
    const onDeleted = vi.fn()
    const { getByLabelText, getByRole } = await renderWithProviders(
      <AccountDeleteDialog
        open
        username="alice"
        onOpenChange={onOpenChange}
        onDeleted={onDeleted}
      />,
      ['common', 'account', 'auth'],
    )
    const user = userEvent.setup()

    await user.type(
      getByLabelText(copy('account.profile.delete_confirm_label', { username: 'alice' })),
      'alice',
    )
    await user.type(getByLabelText(copy('account.profile.delete_password')), 'secret-pass')
    await user.type(getByLabelText(copy('account.profile.totp_code')), '654321')

    await user.click(
      getByRole('button', { name: copy('account.profile.delete_confirm') }),
    )

    await waitFor(() => {
      expect(onDeleted).toHaveBeenCalledTimes(1)
    })
    expect(onOpenChange).toHaveBeenCalledWith(false)
    expect(bodies).toEqual([
      {
        confirm_username: 'alice',
        password: 'secret-pass',
        totp_code: '654321',
      },
    ])
    expect(
      useToastStore
        .getState()
        .toasts.some((toast) => toast.message === copy('account.profile.delete_saved')),
    ).toBe(true)
  })

  it('keeps the dialog open and shows an inline error when deletion fails', async () => {
    server.use(
      http.delete(`${ORIGIN}/api/account`, () =>
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
    const onDeleted = vi.fn()
    const { getByLabelText, getByRole, findByText, findByRole } =
      await renderWithProviders(
        <AccountDeleteDialog
          open
          username="alice"
          onOpenChange={onOpenChange}
          onDeleted={onDeleted}
        />,
        ['common', 'account', 'auth'],
      )
    const user = userEvent.setup()

    await user.type(
      getByLabelText(copy('account.profile.delete_confirm_label', { username: 'alice' })),
      'alice',
    )
    await user.type(getByLabelText(copy('account.profile.delete_password')), 'wrong-pass')
    await user.click(
      getByRole('button', { name: copy('account.profile.delete_confirm') }),
    )

    expect(await findByText(copy('account.profile.delete_error'))).toBeTruthy()
    expect(await findByRole('alertdialog')).toBeTruthy()
    expect(onDeleted).not.toHaveBeenCalled()
    expect(onOpenChange).not.toHaveBeenCalled()
  })
})
