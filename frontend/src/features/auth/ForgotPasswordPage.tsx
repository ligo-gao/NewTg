import { useState, type FormEvent } from 'react'

import { apiClient } from '@/api/client'
import { useI18n } from '@/shared/lib/i18n-context'
import { useCountdown } from '@/shared/lib/use-countdown'
import { Button } from '@/shared/ui/button'
import { FormAlert, TextField } from '@/shared/ui/form'

import { AuthPageShell } from './AuthPageShell'

const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
const RESEND_SECONDS = 30

export interface ForgotPasswordPageProps {
  onNavigate?: (destination: string) => void
}

function defaultNavigate(destination: string): void {
  window.location.assign(destination)
}

/**
 * F-01-05 reset-email request. The response is always 204 so the page shows
 * the same success state whether or not the address exists (anti-enumeration).
 */
export function ForgotPasswordPage({
  onNavigate = defaultNavigate,
}: ForgotPasswordPageProps) {
  const { t } = useI18n()
  const [email, setEmail] = useState('')
  const [fieldError, setFieldError] = useState<string | undefined>(undefined)
  const [formError, setFormError] = useState<string | undefined>(undefined)
  const [sent, setSent] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const countdown = useCountdown()

  async function send(): Promise<void> {
    setFormError(undefined)
    if (!EMAIL_PATTERN.test(email)) {
      setFieldError(t('auth.validation.email'))
      return
    }
    setFieldError(undefined)
    setSubmitting(true)
    try {
      await apiClient.POST('/api/auth/password-reset/request', {
        body: { email: email.trim() },
      })
      setSent(true)
      countdown.start(RESEND_SECONDS)
    } catch {
      setFormError(t('auth.forgot.error'))
    } finally {
      setSubmitting(false)
    }
  }

  function handleSubmit(event: FormEvent<HTMLFormElement>): void {
    event.preventDefault()
    void send()
  }

  return (
    <AuthPageShell title={t('auth.forgot.title')}>
      {sent ? (
        <div className="flex flex-col gap-4" aria-live="polite">
          <p className="text-sm font-medium text-success">
            {t('auth.forgot.sent_title')}
          </p>
          <p className="text-sm text-ink-2">{t('auth.forgot.sent_description')}</p>
          <p className="text-[13px] text-ink-3">
            {t('auth.forgot.sent_enumeration')}
          </p>
          <Button
            variant="secondary"
            className="h-11 w-full"
            disabled={submitting || countdown.running}
            onClick={() => void send()}
          >
            {countdown.running
              ? t('auth.forgot.resend_in', { seconds: countdown.seconds })
              : t('auth.forgot.resend')}
          </Button>
          <Button
            variant="ghost"
            className="h-10 w-full"
            onClick={() => onNavigate('/sign-in')}
          >
            {t('auth.forgot.back')}
          </Button>
        </div>
      ) : (
        <form className="flex flex-col gap-4" onSubmit={handleSubmit} noValidate>
          <p className="text-[13px] text-ink-2">{t('auth.forgot.description')}</p>
          {formError !== undefined ? <FormAlert>{formError}</FormAlert> : null}
          <TextField
            label={t('auth.forgot.email')}
            type="email"
            autoComplete="email"
            value={email}
            onChange={(event) => {
              setEmail(event.target.value)
            }}
            error={fieldError}
            required
          />
          <Button type="submit" className="h-11 w-full" disabled={submitting}>
            {submitting
              ? t('auth.forgot.submitting')
              : t('auth.forgot.submit')}
          </Button>
          <Button
            variant="ghost"
            className="h-10 w-full"
            onClick={() => onNavigate('/sign-in')}
          >
            {t('auth.forgot.back')}
          </Button>
        </form>
      )}
    </AuthPageShell>
  )
}
