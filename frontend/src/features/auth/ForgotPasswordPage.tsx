import { useState, type FormEvent } from 'react'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import type { components } from '@/api/schema'
import { usePublicStatus } from '@/lib/public-status'
import { useI18n } from '@/shared/lib/i18n-context'
import { useCountdown } from '@/shared/lib/use-countdown'
import { Button } from '@/shared/ui/button'
import { FormAlert, TextField } from '@/shared/ui/form'

import { AuthPageShell } from './AuthPageShell'
import { TurnstileWidget } from './TurnstileWidget'

const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
// Server cooldown is 60s (`passwordResetResendAfter`, handlers_password.go:58)
// even though the UI copy historically said 30s; a shorter local timer lets the
// user press a button that is guaranteed to 429.
const RESEND_SECONDS = 60

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
  const [turnstileToken, setTurnstileToken] = useState<string | undefined>(undefined)
  const [turnstileReset, setTurnstileReset] = useState(0)

  const statusQuery = usePublicStatus()
  const turnstile = statusQuery.data?.auth.turnstile
  const turnstileEnabled = turnstile?.enabled ?? false

  async function send(): Promise<void> {
    setFormError(undefined)
    if (!EMAIL_PATTERN.test(email)) {
      setFieldError(t('auth.validation.email'))
      return
    }
    if (turnstileEnabled && (turnstileToken === undefined || turnstileToken === '')) {
      setFormError(t('auth.turnstile.required'))
      return
    }
    setFieldError(undefined)
    setSubmitting(true)
    try {
      const body: components['schemas']['PasswordResetRequest'] = {
        email: email.trim(),
      }
      if (turnstileEnabled) body.turnstile_token = turnstileToken
      await apiClient.POST('/api/auth/password-reset/request', { body })
      setSent(true)
      countdown.start(RESEND_SECONDS)
    } catch (caught) {
      if (caught instanceof ApiError && caught.status === 429) {
        // The 429 carries the remaining cooldown; without starting the timer
        // the resend button would be immediately clickable again.
        countdown.start(caught.retry_after ?? RESEND_SECONDS)
        setFormError(t('auth.forgot.rate_limited'))
      } else {
        setFormError(t('auth.forgot.error'))
      }
    } finally {
      if (turnstileEnabled) {
        // Single-use token; restart the challenge for the next resend.
        setTurnstileToken(undefined)
        setTurnstileReset((value) => value + 1)
      }
      setSubmitting(false)
    }
  }

  function handleSubmit(event: FormEvent<HTMLFormElement>): void {
    event.preventDefault()
    void send()
  }

  if (statusQuery.isError) {
    return (
      <AuthPageShell title={t('auth.forgot.title')}>
        <FormAlert>{t('auth.forgot.status_error')}</FormAlert>
        <Button
          variant="secondary"
          className="mt-4 h-10"
          onClick={() => void statusQuery.refetch()}
        >
          {t('common.action.retry')}
        </Button>
      </AuthPageShell>
    )
  }

  if (statusQuery.isPending || statusQuery.data === undefined) {
    // The request path needs the Turnstile switch; rendering early would let
    // the visitor submit without the token the server now requires.
    return (
      <AuthPageShell title={t('auth.forgot.title')}>
        <div className="space-y-3" aria-busy="true">
          <div className="h-10 rounded-lg bg-surface-subtle" />
          <div className="h-11 rounded-lg bg-surface-subtle" />
        </div>
      </AuthPageShell>
    )
  }

  return (
    <AuthPageShell title={t('auth.forgot.title')}>
      {sent ? (
        <div className="flex flex-col gap-4" aria-live="polite">
          {formError !== undefined ? <FormAlert>{formError}</FormAlert> : null}
          <p className="text-body font-medium text-success">
            {t('auth.forgot.sent_title')}
          </p>
          <p className="text-body text-ink-2">{t('auth.forgot.sent_description')}</p>
          <p className="text-meta text-ink-3">
            {t('auth.forgot.sent_enumeration')}
          </p>
          {turnstileEnabled ? (
            <TurnstileWidget
              siteKey={turnstile?.site_key ?? ''}
              onToken={setTurnstileToken}
              resetSignal={turnstileReset}
            />
          ) : null}
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
          <p className="text-meta text-ink-2">{t('auth.forgot.description')}</p>
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
          {turnstileEnabled ? (
            <TurnstileWidget
              siteKey={turnstile?.site_key ?? ''}
              onToken={setTurnstileToken}
              resetSignal={turnstileReset}
            />
          ) : null}
          <Button type="submit" className="h-11 w-full" disabled={submitting || countdown.running}>
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
