import { HttpResponse, http } from 'msw'
import { setupServer } from 'msw/node'
import userEvent from '@testing-library/user-event'
import { screen, waitFor, within } from '@testing-library/react'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'

import enSettings from '@/locales/en/settings.json'
import frSettings from '@/locales/fr/settings.json'
import jaSettings from '@/locales/ja/settings.json'
import ruSettings from '@/locales/ru/settings.json'
import viSettings from '@/locales/vi/settings.json'
import zhCNSettings from '@/locales/zh-CN/settings.json'
import zhTWSettings from '@/locales/zh-TW/settings.json'
import { renderWithProviders } from '@/test-support/providers'

import { PaymentComplianceGate } from './payment-compliance-gate'
import type { PaymentComplianceStatus } from './api'

const ORIGIN = 'http://localhost:3000'
const EXPECTED = enSettings.payment_compliance.expected_text

const server = setupServer()
beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => server.resetHandlers())
afterAll(() => server.close())

function Harness({
  onCancel = () => undefined,
  onConfirmed = () => undefined,
}: {
  onCancel?: () => void
  onConfirmed?: (status: PaymentComplianceStatus) => void
}) {
  return (
    <PaymentComplianceGate
      open
      expectedText={EXPECTED}
      onCancel={onCancel}
      onConfirmed={onConfirmed}
    />
  )
}

describe('PaymentComplianceGate', () => {
  it('keeps Confirm disabled until the exact phrase is entered', async () => {
    let posted = 0
    const onConfirmed = vi.fn()
    server.use(
      http.post(`${ORIGIN}/api/settings/payment-compliance`, async ({ request }) => {
        posted += 1
        const body = (await request.json()) as { confirmation?: string }
        expect(body.confirmation).toBe(EXPECTED)
        return HttpResponse.json({
          confirmed: true,
          confirmed_at: '2026-10-19T09:30:00Z',
        })
      }),
    )
    await renderWithProviders(<Harness onConfirmed={onConfirmed} />, [
      'common',
      'settings',
    ])
    const user = userEvent.setup()

    const gate = screen.getByRole('alertdialog')
    const confirmButton = within(gate).getByRole('button', { name: 'Confirm' })
    const input = within(gate).getByLabelText('Confirmation text')
    expect((confirmButton as HTMLButtonElement).disabled).toBe(true)

    await user.type(input, 'wrong phrase')
    expect((confirmButton as HTMLButtonElement).disabled).toBe(true)
    await user.click(confirmButton)
    expect(posted).toBe(0)

    await user.clear(input)
    await user.type(input, EXPECTED)
    await waitFor(() =>
      expect((confirmButton as HTMLButtonElement).disabled).toBe(false),
    )
    await user.click(confirmButton)

    await waitFor(() => expect(onConfirmed).toHaveBeenCalledTimes(1))
    expect(onConfirmed.mock.calls[0]?.[0]).toEqual({
      confirmed: true,
      confirmed_at: '2026-10-19T09:30:00Z',
    })
    expect(posted).toBe(1)
  })

  it('shows the server rejection inline and does not confirm', async () => {
    const onConfirmed = vi.fn()
    server.use(
      http.post(`${ORIGIN}/api/settings/payment-compliance`, () =>
        HttpResponse.json(
          {
            type: 'https://newtg.dev/problems/bad_request',
            title: 'Invalid confirmation',
            status: 400,
            detail: 'confirmation text does not match',
            fields: ['confirmation'],
          },
          { status: 400 },
        ),
      ),
    )
    await renderWithProviders(<Harness onConfirmed={onConfirmed} />, [
      'common',
      'settings',
    ])
    const user = userEvent.setup()
    const gate = screen.getByRole('alertdialog')

    await user.type(within(gate).getByLabelText('Confirmation text'), EXPECTED)
    await user.click(within(gate).getByRole('button', { name: 'Confirm' }))

    expect(
      await within(gate).findByText(
        'The text does not match. Type the exact phrase and try again.',
      ),
    ).toBeTruthy()
    expect(onConfirmed).not.toHaveBeenCalled()
  })

  it('cancels without calling the confirmation API', async () => {
    let posted = 0
    const onCancel = vi.fn()
    server.use(
      http.post(`${ORIGIN}/api/settings/payment-compliance`, () => {
        posted += 1
        return HttpResponse.json({ confirmed: false, confirmed_at: null })
      }),
    )
    await renderWithProviders(<Harness onCancel={onCancel} />, [
      'common',
      'settings',
    ])
    const user = userEvent.setup()
    const gate = screen.getByRole('alertdialog')

    await user.click(within(gate).getByRole('button', { name: 'Cancel' }))
    expect(onCancel).toHaveBeenCalledTimes(1)
    expect(posted).toBe(0)

    // Escape follows the same cancel path (dismissible while idle).
    await user.keyboard('{Escape}')
    expect(onCancel).toHaveBeenCalledTimes(2)
    expect(posted).toBe(0)
  })

  it('supports keyboard submission with Enter', async () => {
    const onConfirmed = vi.fn()
    server.use(
      http.post(`${ORIGIN}/api/settings/payment-compliance`, () =>
        HttpResponse.json({
          confirmed: true,
          confirmed_at: '2026-10-19T09:30:00Z',
        }),
      ),
    )
    await renderWithProviders(<Harness onConfirmed={onConfirmed} />, [
      'common',
      'settings',
    ])
    const user = userEvent.setup()
    const gate = screen.getByRole('alertdialog')

    const input = within(gate).getByLabelText('Confirmation text')
    input.focus()
    await user.keyboard(EXPECTED)
    const confirmButton = within(gate).getByRole('button', { name: 'Confirm' })
    await waitFor(() =>
      expect((confirmButton as HTMLButtonElement).disabled).toBe(false),
    )
    confirmButton.focus()
    await user.keyboard('{Enter}')
    await waitFor(() => expect(onConfirmed).toHaveBeenCalledTimes(1))
  })

  it('freezes the same confirmation phrase across all seven locales', () => {
    for (const dictionary of [
      enSettings,
      zhCNSettings,
      zhTWSettings,
      frSettings,
      ruSettings,
      jaSettings,
      viSettings,
    ]) {
      expect(dictionary.payment_compliance.expected_text).toBe(EXPECTED)
    }
  })
})
