import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { waitFor } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { SetupWizard } from './SetupWizard'

const ORIGIN = 'http://localhost:3000'

const server = setupServer()

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

describe('SetupWizard', () => {
  it('posts the frozen flat body and navigates to the dashboard on success', async () => {
    let body: Record<string, unknown> | undefined
    server.use(
      http.post(`${ORIGIN}/api/setup`, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>
        return HttpResponse.json(
          {
            user: {
              id: 1,
              username: 'admin',
              email: 'admin@example.com',
              role: 'admin',
              status: 'enabled',
              email_verified: false,
            },
          },
          { status: 201 },
        )
      }),
    )

    const onNavigate = vi.fn()
    const { getByLabelText, getByRole } = await renderWithProviders(
      <SetupWizard onNavigate={onNavigate} />,
      ['common', 'auth'],
    )
    const user = userEvent.setup()

    await user.clear(getByLabelText('Site name'))
    await user.type(getByLabelText('Site name'), 'NewTG Test')
    await user.type(getByLabelText('Server address'), 'https://gateway.example')
    await user.click(getByRole('button', { name: 'Next' }))

    await user.type(getByLabelText('Username'), 'admin')
    await user.type(getByLabelText('Email'), 'admin@example.com')
    await user.type(getByLabelText('Password'), 'correct horse')
    await user.type(getByLabelText('Confirm password'), 'correct horse')
    await user.click(getByRole('button', { name: 'Next' }))

    await user.click(getByRole('button', { name: 'Finish setup' }))

    await waitFor(() => {
      expect(onNavigate).toHaveBeenCalledWith('/dashboard')
    })

    // Contract freeze: the setup body is flat (no nesting, no email_code/agree_terms).
    expect(body).toEqual({
      site_title: 'NewTG Test',
      server_address: 'https://gateway.example',
      username: 'admin',
      email: 'admin@example.com',
      password: 'correct horse',
      confirmPassword: 'correct horse',
    })
  })

  it('blocks step 1 until both site fields are valid', async () => {
    const { getByLabelText, getByRole } = await renderWithProviders(
      <SetupWizard onNavigate={() => {}} />,
      ['common', 'auth'],
    )
    const user = userEvent.setup()

    const next = getByRole('button', { name: 'Next' }) as HTMLButtonElement
    expect(next.disabled).toBe(true)

    await user.type(getByLabelText('Site name'), 'NewTG')
    await user.type(getByLabelText('Server address'), 'not-a-url')
    expect(next.disabled).toBe(true)

    await user.clear(getByLabelText('Server address'))
    await user.type(getByLabelText('Server address'), 'https://gateway.example')
    expect(next.disabled).toBe(false)
  })
})
