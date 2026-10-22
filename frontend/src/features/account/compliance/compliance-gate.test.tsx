import { useState } from 'react'
import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { renderWithProviders } from '@/test-support/providers'

import { ComplianceGate } from './compliance-gate'
import { clearComplianceAcknowledgement } from './compliance-session'
import { useComplianceGate } from './use-compliance-gate'

interface HarnessProps {
  onConfirm?: () => void
  onOpenChange?: (open: boolean) => void
  termsHref?: string
  privacyHref?: string
}

/** Controlled host so a cancelled gate can be reopened in the same test. */
function Harness({
  onConfirm = () => undefined,
  onOpenChange = () => undefined,
  termsHref,
  privacyHref,
}: HarnessProps) {
  const [open, setOpen] = useState(true)
  return (
    <div>
      <button type="button" onClick={() => setOpen(true)}>
        Reopen gate
      </button>
      <ComplianceGate
        open={open}
        termsHref={termsHref}
        privacyHref={privacyHref}
        onOpenChange={(next) => {
          setOpen(next)
          onOpenChange(next)
        }}
        onConfirm={onConfirm}
      />
    </div>
  )
}

beforeEach(() => {
  clearComplianceAcknowledgement()
})

describe('ComplianceGate', () => {
  it('keeps Continue disabled until the acknowledgement is ticked', async () => {
    const onConfirm = vi.fn()
    await renderWithProviders(<Harness onConfirm={onConfirm} />, [
      'common',
      'account',
    ])
    const user = userEvent.setup()

    expect(screen.getByRole('alertdialog')).toBeTruthy()
    const checkbox = screen.getByRole('checkbox', {
      name: /I have read and agree/i,
    })
    const continueButton = screen.getByRole('button', {
      name: 'Agree & Continue',
    })
    expect((checkbox as HTMLInputElement).checked).toBe(false)
    expect((continueButton as HTMLButtonElement).disabled).toBe(true)

    // A disabled button cannot submit, and the callback is only wired to it.
    await user.click(continueButton)
    expect(onConfirm).not.toHaveBeenCalled()

    await user.click(checkbox)
    expect((checkbox as HTMLInputElement).checked).toBe(true)
    expect((continueButton as HTMLButtonElement).disabled).toBe(false)

    await user.click(continueButton)
    expect(onConfirm).toHaveBeenCalledTimes(1)
  })

  it('resets the checkbox when the gate is cancelled and reopened', async () => {
    await renderWithProviders(<Harness />, ['common', 'account'])
    const user = userEvent.setup()

    await user.click(
      screen.getByRole('checkbox', { name: /I have read and agree/i }),
    )
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('alertdialog')).toBeNull()

    await user.click(screen.getByRole('button', { name: 'Reopen gate' }))
    const checkbox = screen.getByRole('checkbox', {
      name: /I have read and agree/i,
    })
    expect((checkbox as HTMLInputElement).checked).toBe(false)
    expect(
      (
        screen.getByRole('button', {
          name: 'Agree & Continue',
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true)
  })

  it('links to the bundled legal documents by default', async () => {
    await renderWithProviders(<Harness />, ['common', 'account'])

    expect(
      screen.getByRole('link', { name: 'Terms of Service' }).getAttribute('href'),
    ).toBe('/legal/terms')
    expect(
      screen.getByRole('link', { name: 'Privacy Policy' }).getAttribute('href'),
    ).toBe('/legal/privacy')
  })

  it('rejects unsafe legal link targets instead of rendering them as anchors', async () => {
    await renderWithProviders(
      <Harness
        termsHref="javascript:alert(1)"
        privacyHref="//evil.example/privacy"
      />,
      ['common', 'account'],
    )

    expect(screen.getByText('Terms of Service')).toBeTruthy()
    expect(screen.getByText('Privacy Policy')).toBeTruthy()
    expect(
      screen.queryByRole('link', { name: 'Terms of Service' }),
    ).toBeNull()
    expect(
      screen.queryByRole('link', { name: 'Privacy Policy' }),
    ).toBeNull()
    expect(document.querySelector('a[href^="javascript:"]')).toBeNull()
    expect(document.querySelector('a[href^="//evil.example"]')).toBeNull()
  })

  it('cancels on the Cancel button and on Escape without confirming', async () => {
    const onConfirm = vi.fn()
    const onOpenChange = vi.fn()
    await renderWithProviders(
      <Harness onConfirm={onConfirm} onOpenChange={onOpenChange} />,
      ['common', 'account'],
    )
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(onOpenChange).toHaveBeenLastCalledWith(false)
    expect(onConfirm).not.toHaveBeenCalled()

    await user.click(screen.getByRole('button', { name: 'Reopen gate' }))
    await user.keyboard('{Escape}')
    expect(onOpenChange).toHaveBeenLastCalledWith(false)
    expect(onConfirm).not.toHaveBeenCalled()
  })

  it('supports keyboard operation: Space toggles, Enter continues', async () => {
    const onConfirm = vi.fn()
    await renderWithProviders(<Harness onConfirm={onConfirm} />, [
      'common',
      'account',
    ])
    const user = userEvent.setup()

    const checkbox = screen.getByRole('checkbox', {
      name: /I have read and agree/i,
    })
    checkbox.focus()
    await user.keyboard(' ')
    expect((checkbox as HTMLInputElement).checked).toBe(true)

    const continueButton = screen.getByRole('button', {
      name: 'Agree & Continue',
    })
    continueButton.focus()
    await user.keyboard('{Enter}')
    expect(onConfirm).toHaveBeenCalledTimes(1)
  })
})

/**
 * Session persistence contract: acknowledge once, then later payments in the
 * same tab skip the gate. Cancelling must never run the held payment action.
 */
function GateHarness() {
  const { gateOpen, requireCompliance, confirmCompliance, cancelCompliance } =
    useComplianceGate()
  const [payments, setPayments] = useState(0)
  return (
    <div>
      <button
        type="button"
        onClick={() => {
          requireCompliance(() => {
            setPayments((count) => count + 1)
          })
        }}
      >
        Pay
      </button>
      <span data-testid="payments">{payments}</span>
      <ComplianceGate
        open={gateOpen}
        onOpenChange={(next) => {
          if (!next) cancelCompliance()
        }}
        onConfirm={confirmCompliance}
      />
    </div>
  )
}

describe('useComplianceGate', () => {
  it('never submits on cancel, and remembers the acknowledgement for the session', async () => {
    await renderWithProviders(<GateHarness />, ['common', 'account'])
    const user = userEvent.setup()
    const payments = screen.getByTestId('payments')

    await user.click(screen.getByRole('button', { name: 'Pay' }))
    expect(screen.getByRole('alertdialog')).toBeTruthy()
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(payments.textContent).toBe('0')

    await user.click(screen.getByRole('button', { name: 'Pay' }))
    await user.click(
      screen.getByRole('checkbox', { name: /I have read and agree/i }),
    )
    await user.click(screen.getByRole('button', { name: 'Agree & Continue' }))
    expect(payments.textContent).toBe('1')

    // Same tab session: the gate is skipped and the action runs immediately.
    await user.click(screen.getByRole('button', { name: 'Pay' }))
    expect(screen.queryByRole('alertdialog')).toBeNull()
    expect(payments.textContent).toBe('2')
  })

  it('asks again when session storage is unavailable', async () => {
    const getItem = vi
      .spyOn(Storage.prototype, 'getItem')
      .mockImplementation(() => null)
    const setItem = vi
      .spyOn(Storage.prototype, 'setItem')
      .mockImplementation(() => undefined)
    try {
      await renderWithProviders(<GateHarness />, ['common', 'account'])
      const user = userEvent.setup()

      await user.click(screen.getByRole('button', { name: 'Pay' }))
      await user.click(
        screen.getByRole('checkbox', { name: /I have read and agree/i }),
      )
      await user.click(screen.getByRole('button', { name: 'Agree & Continue' }))

      await user.click(screen.getByRole('button', { name: 'Pay' }))
      expect(screen.getByRole('alertdialog')).toBeTruthy()
    } finally {
      getItem.mockRestore()
      setItem.mockRestore()
    }
  })
})
