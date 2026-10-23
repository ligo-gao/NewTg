import { useEffect, useId, useState, type FormEvent } from 'react'

import { apiClient } from '@/api/client'
import { useI18n } from '@/shared/lib/i18n-context'
import { Dialog } from '@/shared/ui/base/dialog'
import { Button } from '@/shared/ui/button'
import { FormAlert, PasswordField, TextField } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

export interface AccountEmailDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  currentEmail: string | null
  onChanged: (email: string) => void
}

/** Pragmatic client-side shape check; the server stays authoritative. */
const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
const CODE_PATTERN = /^\d{6}$/
/** Matches the endpoint's per-address cooldown (60s). */
const RESEND_SECONDS = 60

/**
 * F-01 email change dialog.
 *
 * The flow is two steps in one surface: send a one-time code to the new address
 * (`purpose=bind`), then re-authenticate with the current password (plus TOTP
 * when 2FA is on) while submitting that code. Nothing is persisted until the
 * final PUT succeeds, so a dismissed dialog leaves the account untouched.
 */
export function AccountEmailDialog({
  open,
  onOpenChange,
  currentEmail,
  onChanged,
}: AccountEmailDialogProps) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)
  const formId = useId()

  const [email, setEmail] = useState('')
  const [code, setCode] = useState('')
  const [password, setPassword] = useState('')
  const [totpCode, setTotpCode] = useState('')
  const [error, setError] = useState<string | undefined>(undefined)
  const [sending, setSending] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [countdown, setCountdown] = useState(0)

  // Reset on every open *and* close: a reopened dialog must never show the
  // previous email/code/password, nor keep a stale cooldown running.
  useEffect(() => {
    setEmail('')
    setCode('')
    setPassword('')
    setTotpCode('')
    setError(undefined)
    setSending(false)
    setSubmitting(false)
    setCountdown(0)
  }, [open])

  useEffect(() => {
    if (countdown <= 0) return
    const timer = setInterval(() => {
      setCountdown((remaining) => (remaining <= 1 ? 0 : remaining - 1))
    }, 1000)
    return () => {
      clearInterval(timer)
    }
  }, [countdown])

  async function sendCode(): Promise<void> {
    setError(undefined)
    const nextEmail = email.trim()
    if (!EMAIL_PATTERN.test(nextEmail)) {
      setError(t('account.profile.email_invalid'))
      return
    }

    setSending(true)
    try {
      await apiClient.POST('/api/auth/email-code', {
        body: { email: nextEmail, purpose: 'bind' },
      })
      setCountdown(RESEND_SECONDS)
      push({ tone: 'success', message: t('account.profile.email_sent') })
    } catch {
      setError(t('account.profile.email_error'))
    } finally {
      setSending(false)
    }
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    setError(undefined)

    const nextEmail = email.trim()
    if (!EMAIL_PATTERN.test(nextEmail)) {
      setError(t('account.profile.email_invalid'))
      return
    }
    if (!CODE_PATTERN.test(code.trim())) {
      setError(t('account.profile.email_code_invalid'))
      return
    }
    if (password === '') {
      setError(t('account.profile.email_password_required'))
      return
    }

    setSubmitting(true)
    try {
      await apiClient.PUT('/api/email', {
        body: {
          new_email: nextEmail,
          code: code.trim(),
          current_password: password,
          ...(totpCode.trim() === '' ? {} : { totp_code: totpCode.trim() }),
        },
      })
      push({ tone: 'success', message: t('account.profile.email_saved') })
      onChanged(nextEmail)
      onOpenChange(false)
    } catch {
      setError(t('account.profile.email_error'))
    } finally {
      setSubmitting(false)
    }
  }

  const busy = sending || submitting

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (submitting) return
        onOpenChange(next)
      }}
      title={t('account.profile.email_dialog_title')}
      description={t('account.profile.email_dialog_desc')}
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
          <Button type="submit" form={formId} disabled={busy}>
            {t('account.profile.confirm_change')}
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

        <TextField
          label={t('account.profile.current_email')}
          value={currentEmail ?? '—'}
          readOnly
        />

        <TextField
          label={t('account.profile.new_email')}
          type="email"
          autoComplete="email"
          inputMode="email"
          value={email}
          onChange={(event) => {
            setEmail(event.target.value)
          }}
        />

        <div className="flex items-start gap-2">
          <div className="min-w-0 flex-1">
            <TextField
              label={t('account.profile.email_code')}
              placeholder={t('account.profile.email_code_ph')}
              autoComplete="one-time-code"
              inputMode="numeric"
              maxLength={6}
              value={code}
              onChange={(event) => {
                setCode(event.target.value)
              }}
            />
          </div>
          <Button
            variant="secondary"
            className="mt-[26px] shrink-0"
            disabled={busy || countdown > 0}
            onClick={() => {
              void sendCode()
            }}
          >
            {countdown > 0
              ? t('account.profile.resend_in', { seconds: countdown })
              : t('account.profile.send_code')}
          </Button>
        </div>

        <PasswordField
          label={t('account.profile.password')}
          showLabel={t('auth.sign_in.show_password')}
          hideLabel={t('auth.sign_in.hide_password')}
          placeholder={t('account.profile.email_password_ph')}
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

        <p className="text-xs leading-5 text-ink-3">
          {t('account.profile.email_notice')}
        </p>
      </form>
    </Dialog>
  )
}
