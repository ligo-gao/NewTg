import { useEffect, useState } from 'react'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'
import { TextField } from '@/shared/ui/form'

import {
  confirmPaymentCompliance,
  type PaymentComplianceStatus,
} from './api'

export interface PaymentComplianceGateProps {
  open: boolean
  /**
   * Frozen phrase from the settings dictionary. It must be identical in every
   * locale and match the server constant; the server re-validates it.
   */
  expectedText: string
  /**
   * Dismissing the gate must leave the payment tab. The shell wires this to a
   * non-payment tab so the protected sections never render for an unconfirmed
   * administrator.
   */
  onCancel: () => void
  onConfirmed: (status: PaymentComplianceStatus) => void
}

function errorMessage(error: unknown, fallback: string): string {
  if (error instanceof ApiError) {
    if (error.kind === 'validation') return fallback
    return error.detail ?? error.title
  }
  return error instanceof Error ? error.message : fallback
}

/**
 * Payment settings compliance gate (modules-08 §3.4 / G-M6C-19).
 *
 * Shown only inside `/admin/settings?tab=payment` for an administrator who has
 * not confirmed yet. The primary action stays disabled until the input exactly
 * matches the frozen phrase; the server performs the authoritative comparison
 * and records the confirmation (who/when) in `audit_logs`.
 *
 * Cancelling/Escape/backdrop never reveals the payment sections: the shell
 * switches to a different tab instead.
 */
export function PaymentComplianceGate({
  open,
  expectedText,
  onCancel,
  onConfirmed,
}: PaymentComplianceGateProps) {
  const { t } = useI18n()
  const [text, setText] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Every open is a fresh acknowledgement: never submit a previously typed
  // value without the administrator seeing it again.
  useEffect(() => {
    if (open) {
      setText('')
      setError(null)
      setSubmitting(false)
    }
  }, [open])

  const matches = text === expectedText

  async function submit(): Promise<void> {
    if (!matches || submitting) return
    setSubmitting(true)
    setError(null)
    try {
      const status = await confirmPaymentCompliance(text)
      onConfirmed(status)
    } catch (caught) {
      setError(
        errorMessage(caught, t('settings.payment_compliance.error_mismatch')),
      )
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        // A request in flight must settle before the gate can be dismissed.
        if (!next && !submitting) onCancel()
      }}
      title={t('settings.payment_compliance.title')}
      description={t('settings.payment_compliance.description')}
      role="alertdialog"
      dismissible={!submitting}
      footer={
        <>
          <Button variant="ghost" disabled={submitting} onClick={onCancel}>
            {t('settings.payment_compliance.cancel')}
          </Button>
          <Button disabled={!matches || submitting} onClick={() => void submit()}>
            {submitting
              ? t('settings.payment_compliance.confirming')
              : t('settings.payment_compliance.confirm')}
          </Button>
        </>
      }
    >
      <p>{t('settings.payment_compliance.responsibility')}</p>
      <div className="mt-4">
        <TextField
          label={t('settings.payment_compliance.input_label')}
          hint={t('settings.payment_compliance.instruction', { text: expectedText })}
          value={text}
          onChange={(event) => {
            setText(event.target.value)
            if (error !== null) setError(null)
          }}
          autoComplete="off"
          spellCheck={false}
          disabled={submitting}
        />
      </div>
      {error !== null ? (
        <p role="alert" className="mt-3 text-sm text-danger">
          {error}
        </p>
      ) : null}
    </Dialog>
  )
}
