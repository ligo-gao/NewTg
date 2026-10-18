import { useState, type FormEvent } from 'react'
import { useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { passwordPolicyProblem } from '@/lib/password-policy'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert, PasswordField } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

import { SessionsCard } from './SessionsCard'
import { TwoFaCard } from './TwoFaCard'

export interface SecurityPageProps {
  /**
   * True when the session is restricted (`403 password_change_required`) or the
   * login response carried `must_change_password`. Only the password form is
   * rendered: every other console endpoint is blocked server-side.
   */
  forcePasswordChange?: boolean
  onNavigate?: (destination: string) => void
}

function defaultNavigate(destination: string): void {
  window.location.assign(destination)
}

/** F-01-16/F-01-10/F-01-13 security page plus the forced-change screen. */
export function SecurityPage({
  forcePasswordChange = false,
  onNavigate = defaultNavigate,
}: SecurityPageProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [fieldErrors, setFieldErrors] = useState<{
    newPassword?: string
    confirmPassword?: string
  }>({})
  const [formError, setFormError] = useState<string | undefined>(undefined)
  const [submitting, setSubmitting] = useState(false)

  async function changePassword(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
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
      await apiClient.POST('/api/password', {
        body: { current_password: currentPassword, new_password: newPassword },
      })
      push({ tone: 'success', message: t('account.security.password_changed') })
      setCurrentPassword('')
      setNewPassword('')
      setConfirmPassword('')
      // The server revoked every other session, so the mounted sessions list
      // is now wrong. Without this it keeps showing the killed devices for a
      // staleTime window while the toast says they were signed out.
      await queryClient.invalidateQueries({ queryKey: ['sessions'] })
      await queryClient.invalidateQueries({ queryKey: ['me'] })
      if (forcePasswordChange) {
        onNavigate('/dashboard')
      }
    } catch {
      setFormError(t('account.security.password_error'))
    } finally {
      setSubmitting(false)
    }
  }

  const passwordCard = (
    <section className="rounded-xl border border-border-hairline bg-surface-card p-5">
      <h2 className="text-base font-semibold text-ink-1">
        {forcePasswordChange
          ? t('account.security.force_title')
          : t('account.security.password_title')}
      </h2>
      <p className="mt-1 text-[13px] text-ink-2">
        {forcePasswordChange
          ? t('account.security.force_description')
          : t('account.security.password_description')}
      </p>

      <form
        className="mt-4 flex max-w-[420px] flex-col gap-3"
        onSubmit={(event) => void changePassword(event)}
        noValidate
      >
        {formError !== undefined ? <FormAlert>{formError}</FormAlert> : null}
        <PasswordField
          label={t('account.security.current_password')}
          showLabel={t('auth.sign_in.show_password')}
          hideLabel={t('auth.sign_in.hide_password')}
          autoComplete="current-password"
          value={currentPassword}
          onChange={(event) => {
            setCurrentPassword(event.target.value)
          }}
          required
        />
        <PasswordField
          label={t('account.security.new_password')}
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
          label={t('account.security.confirm_password')}
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
          className="self-start"
          disabled={submitting || currentPassword === '' || newPassword === ''}
        >
          {submitting
            ? t('account.security.changing_password')
            : forcePasswordChange
              ? t('account.security.force_submit')
              : t('account.security.change_password')}
        </Button>
      </form>
    </section>
  )

  if (forcePasswordChange) {
    return (
      <div className="mx-auto flex max-w-[880px] flex-col gap-4">
        <h1 className="text-xl font-semibold text-ink-1">
          {t('account.security.force_title')}
        </h1>
        {passwordCard}
      </div>
    )
  }

  return (
    <div className="mx-auto flex max-w-[880px] flex-col gap-4">
      <h1 className="text-xl font-semibold text-ink-1">{t('account.security.title')}</h1>
      {passwordCard}
      <TwoFaCard />
      <SessionsCard />
    </div>
  )
}
