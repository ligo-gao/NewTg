import { useEffect, useState, type FormEvent } from 'react'

import { apiClient } from '@/api/client'
import { passwordPolicyProblem } from '@/lib/password-policy'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert, PasswordField } from '@/shared/ui/form'

import { AuthPageShell } from './AuthPageShell'

const REDIRECT_DELAY_MS = 3000

type Phase = 'validating' | 'invalid' | 'form' | 'success'

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

  useEffect(() => {
    if (token === undefined || token === '') return
    let cancelled = false
    void (async () => {
      try {
        await apiClient.POST('/api/auth/password-reset/validate', {
          body: { token },
        })
        if (!cancelled) setPhase('form')
      } catch {
        if (!cancelled) setPhase('invalid')
      }
    })()
    return () => {
      cancelled = true
    }
  }, [token])

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
      <AuthPageShell title={t('auth.reset.title')}>
        <div className="space-y-3" aria-busy="true">
          <div className="h-10 rounded-lg bg-surface-subtle" />
          <div className="h-10 rounded-lg bg-surface-subtle" />
          <div className="h-11 rounded-lg bg-surface-subtle" />
        </div>
      </AuthPageShell>
    )
  }

  if (phase === 'invalid') {
    return (
      <AuthPageShell title={t('auth.reset.invalid_title')}>
        <p className="text-sm text-ink-2">{t('auth.reset.invalid_body')}</p>
        <Button className="mt-4 h-11 w-full" onClick={() => onNavigate('/sign-in')}>
          {t('auth.reset.back')}
        </Button>
      </AuthPageShell>
    )
  }

  if (phase === 'success') {
    return (
      <AuthPageShell title={t('auth.reset.success_title')}>
        <p className="text-sm text-ink-2">{t('auth.reset.success_body')}</p>
        <Button className="mt-4 h-11 w-full" onClick={() => onNavigate('/sign-in')}>
          {t('auth.reset.go_to_sign_in')}
        </Button>
      </AuthPageShell>
    )
  }

  return (
    <AuthPageShell title={t('auth.reset.title')}>
      <form
        className="flex flex-col gap-4"
        onSubmit={(event) => void handleSubmit(event)}
        noValidate
      >
        {formError !== undefined ? <FormAlert>{formError}</FormAlert> : null}
        <PasswordField
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
        <Button type="submit" className="h-11 w-full" disabled={submitting}>
          {submitting ? t('auth.reset.submitting') : t('auth.reset.submit')}
        </Button>
      </form>
    </AuthPageShell>
  )
}
