import { useEffect, useState, type FormEvent } from 'react'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { passwordPolicyProblem } from '@/lib/password-policy'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert, PasswordField } from '@/shared/ui/form'

import { AuthHallShell } from './AuthHallShell'

const REDIRECT_DELAY_MS = 3000

type Phase = 'validating' | 'invalid' | 'unavailable' | 'form' | 'success'

export interface ResetPasswordPageProps {
  /** Token from the emailed `?token=` query parameter. */
  token?: string
  onNavigate?: (destination: string) => void
}

function defaultNavigate(destination: string): void {
  window.location.assign(destination)
}

/**
 * F-01-06 reset confirmation. Token validation is separate from consumption
 * (validate does not consume), so reloading the page never burns the link.
 */
export function ResetPasswordPage({
  token,
  onNavigate = defaultNavigate,
}: ResetPasswordPageProps) {
  const { t } = useI18n()
  const [phase, setPhase] = useState<Phase>(token ? 'validating' : 'invalid')
  const [newPassword, setNewPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [fieldErrors, setFieldErrors] = useState<{
    newPassword?: string
    confirmPassword?: string
  }>({})
  const [formError, setFormError] = useState<string | undefined>(undefined)
  const [submitting, setSubmitting] = useState(false)
  const [validateAttempt, setValidateAttempt] = useState(0)

  useEffect(() => {
    if (token === undefined || token === '') return
    let cancelled = false
    void (async () => {
      try {
        await apiClient.POST('/api/auth/password-reset/validate', {
          body: { token },
        })
        if (!cancelled) setPhase('form')
      } catch (caught) {
        if (cancelled) return
        // Only a definitive rejection means the link is dead. Offline, 429 and
        // 5xx are transient; calling those "invalid" tells the user to request
        // a new link for no reason.
        const retryable =
          caught instanceof ApiError &&
          (caught.kind === 'offline' || caught.status === 429 || caught.status >= 500)
        setPhase(retryable ? 'unavailable' : 'invalid')
      }
    })()
    return () => {
      cancelled = true
    }
  }, [token, validateAttempt])

  function retryValidation(): void {
    setPhase('validating')
    setValidateAttempt((attempt) => attempt + 1)
  }

  // Successful resets land on the sign-in page automatically; the button lets
  // the user skip the wait.
  useEffect(() => {
    if (phase !== 'success') return
    const timer = setTimeout(() => {
      onNavigate('/sign-in')
    }, REDIRECT_DELAY_MS)
    return () => {
      clearTimeout(timer)
    }
  }, [phase, onNavigate])

  async function handleSubmit(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    if (token === undefined) return

    const next: typeof fieldErrors = {}
    if (passwordPolicyProblem(newPassword) !== undefined) {
      next.newPassword = t('auth.validation.password_length')
    }
    if (newPassword !== confirmPassword) {
      next.confirmPassword = t('auth.validation.password_mismatch')
    }
    setFieldErrors(next)
    if (Object.keys(next).length > 0) return

    setFormError(undefined)
    setSubmitting(true)
    try {
      await apiClient.POST('/api/auth/password-reset/confirm', {
        body: { token, new_password: newPassword, confirm: confirmPassword },
      })
      setPhase('success')
    } catch {
      setFormError(t('auth.reset.error'))
    } finally {
      setSubmitting(false)
    }
  }

  if (phase === 'validating') {
    return (
      <AuthHallShell title={t('auth.reset.title')}>
        <div className="flex flex-col gap-[22px]" aria-busy="true">
          <div className="h-11 border-b border-border-default" />
          <div className="h-11 border-b border-border-default" />
          <div className="h-11 border-b border-border-default" />
        </div>
      </AuthHallShell>
    )
  }

  if (phase === 'invalid') {
    return (
      <AuthHallShell title={t('auth.reset.invalid_title')}>
        <div className="auth-hall-stack">
          <p className="auth-hall-pane-lead">{t('auth.reset.invalid_body')}</p>
          <Button
            className="auth-hall-submit w-full"
            onClick={() => onNavigate('/sign-in')}
          >
            {t('auth.reset.back')}
          </Button>
        </div>
      </AuthHallShell>
    )
  }

  if (phase === 'unavailable') {
    return (
      <AuthHallShell title={t('auth.reset.unavailable_title')}>
        <div className="auth-hall-stack">
          <p className="auth-hall-pane-lead">{t('auth.reset.unavailable_body')}</p>
          <Button className="auth-hall-submit w-full" onClick={retryValidation}>
            {t('auth.reset.retry')}
          </Button>
          <Button
            variant="ghost"
            className="auth-hall-back w-full"
            onClick={() => onNavigate('/sign-in')}
          >
            {t('auth.reset.back')}
          </Button>
        </div>
      </AuthHallShell>
    )
  }

  if (phase === 'success') {
    return (
      <AuthHallShell title={t('auth.reset.success_title')}>
        <div className="auth-hall-stack">
          <p className="auth-hall-pane-lead">{t('auth.reset.success_body')}</p>
          <Button
            className="auth-hall-submit w-full"
            onClick={() => onNavigate('/sign-in')}
          >
            {t('auth.reset.go_to_sign_in')}
          </Button>
        </div>
      </AuthHallShell>
    )
  }

  return (
    <AuthHallShell title={t('auth.reset.title')}>
      <form onSubmit={(event) => void handleSubmit(event)} noValidate>
        {formError !== undefined ? <FormAlert>{formError}</FormAlert> : null}
        <PasswordField
          variant="line"
          label={t('auth.reset.new_password')}
          showLabel={t('auth.sign_in.show_password')}
          hideLabel={t('auth.sign_in.hide_password')}
          autoComplete="new-password"
          value={newPassword}
          onChange={(event) => {
            setNewPassword(event.target.value)
          }}
          error={fieldErrors.newPassword}
          required
        />
        <PasswordField
          variant="line"
          label={t('auth.reset.confirm_password')}
          showLabel={t('auth.sign_in.show_password')}
          hideLabel={t('auth.sign_in.hide_password')}
          autoComplete="new-password"
          value={confirmPassword}
          onChange={(event) => {
            setConfirmPassword(event.target.value)
          }}
          error={fieldErrors.confirmPassword}
          required
        />
        <Button
          type="submit"
          className="auth-hall-submit w-full"
          disabled={submitting}
        >
          {submitting ? t('auth.reset.submitting') : t('auth.reset.submit')}
        </Button>
      </form>
    </AuthHallShell>
  )
}
