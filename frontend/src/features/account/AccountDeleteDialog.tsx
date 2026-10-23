import { useEffect, useId, useState, type FormEvent } from 'react'

import { apiClient } from '@/api/client'
import { useI18n } from '@/shared/lib/i18n-context'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'
import { FormAlert, PasswordField, TextField } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

export interface AccountDeleteDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  username: string
  onDeleted: () => void
}

/**
 * F-01 irreversible account deletion dialog (R-04: hard delete, no grace
 * period). Both re-authentication factors are collected here — the typed
 * username guards against a mis-click, the password (plus TOTP when enabled)
 * is the server-side reauth gate. Nothing is sent until both are present.
 */
export function AccountDeleteDialog({
  open,
  onOpenChange,
  username,
  onDeleted,
}: AccountDeleteDialogProps) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)
  const formId = useId()

  const [confirmUsername, setConfirmUsername] = useState('')
  const [password, setPassword] = useState('')
  const [totpCode, setTotpCode] = useState('')
  const [error, setError] = useState<string | undefined>(undefined)
  const [submitting, setSubmitting] = useState(false)

  // A reopened dialog always starts from an empty, unconfirmed state: keeping
  // a typed username/password across opens would defeat the confirmation.
  useEffect(() => {
    setConfirmUsername('')
    setPassword('')
    setTotpCode('')
    setError(undefined)
    setSubmitting(false)
  }, [open])

  const canConfirm =
    confirmUsername === username && password !== '' && !submitting

  async function handleSubmit(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    if (!canConfirm) return
    setError(undefined)
    setSubmitting(true)
    try {
      await apiClient.DELETE('/api/account', {
        body: {
          confirm_username: confirmUsername,
          password,
          ...(totpCode.trim() === '' ? {} : { totp_code: totpCode.trim() }),
        },
      })
      push({ tone: 'success', message: t('account.profile.delete_saved') })
      onOpenChange(false)
      onDeleted()
    } catch {
      setError(t('account.profile.delete_error'))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (submitting) return
        onOpenChange(next)
      }}
      role="alertdialog"
      initialFocus="first"
      title={t('account.profile.delete_dialog_title')}
      description={t('account.profile.delete_dialog_desc')}
      footer={
        <>
          <Button
            variant="secondary"
            disabled={submitting}
            onClick={() => {
              onOpenChange(false)
            }}
          >
            {t('common.action.cancel')}
          </Button>
          <Button
            type="submit"
            form={formId}
            variant="danger"
            disabled={!canConfirm}
          >
            {t('account.profile.delete_confirm')}
          </Button>
        </>
      }
    >
      <form
        id={formId}
        className="flex flex-col gap-3"
        noValidate
        onSubmit={(event) => {
          void handleSubmit(event)
        }}
      >
        {error !== undefined ? <FormAlert>{error}</FormAlert> : null}

        <FormAlert tone="danger">{t('account.profile.delete_warning')}</FormAlert>

        <TextField
          label={t('account.profile.delete_confirm_label', { username })}
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          value={confirmUsername}
          onChange={(event) => {
            setConfirmUsername(event.target.value)
          }}
        />

        <PasswordField
          label={t('account.profile.delete_password')}
          showLabel={t('auth.sign_in.show_password')}
          hideLabel={t('auth.sign_in.hide_password')}
          autoComplete="current-password"
          value={password}
          onChange={(event) => {
            setPassword(event.target.value)
          }}
        />

        <TextField
          label={t('account.profile.totp_code')}
          placeholder={t('account.profile.totp_ph')}
          autoComplete="one-time-code"
          inputMode="numeric"
          maxLength={6}
          value={totpCode}
          onChange={(event) => {
            setTotpCode(event.target.value)
          }}
        />
      </form>
    </Dialog>
  )
}
