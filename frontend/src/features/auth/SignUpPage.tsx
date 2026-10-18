import { useState, type FormEvent } from 'react'
import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import { sanitizeAuthRedirect } from '@/lib/auth-redirect'
import { passwordPolicyProblem } from '@/lib/password-policy'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert, PasswordField, TextField } from '@/shared/ui/form'

import { AuthPageShell } from './AuthPageShell'
import { EmailCodeField } from './EmailCodeField'

const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/

export interface SignUpPageProps {
  redirect?: string
  aff?: string
  onNavigate?: (destination: string) => void
}

interface FieldErrors {
  username?: string
  email?: string
  password?: string
  confirmPassword?: string
  emailCode?: string
  agree?: string
}

function defaultNavigate(destination: string): void {
  window.location.assign(destination)
}

/** F-01-01 registration (username + email + code). */
export function SignUpPage({
  redirect,
  aff,
  onNavigate = defaultNavigate,
}: SignUpPageProps) {
  const { t } = useI18n()
  const [username, setUsername] = useState('')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [emailCode, setEmailCode] = useState('')
  const [agree, setAgree] = useState(false)
  const [errors, setErrors] = useState<FieldErrors>({})
  const [formError, setFormError] = useState<string | undefined>(undefined)
  const [submitting, setSubmitting] = useState(false)

  const status = useQuery({
    queryKey: ['public-status'],
    queryFn: async () => requireData(await apiClient.GET('/api/status')),
  })
  const verificationRequired = status.data?.auth.email_verification_required ?? false

  if (status.data !== undefined && !status.data.auth.signup_enabled) {
    return (
      <AuthPageShell title={t('auth.sign_up.title')}>
        <FormAlert tone="warning">{t('auth.sign_up.disabled')}</FormAlert>
        <a
          href="/"
          className="mt-4 inline-block text-[13px] font-medium text-brand hover:underline"
        >
          {t('auth.sign_in.title', { site: t('auth.shell.brand') })}
        </a>
      </AuthPageShell>
    )
  }

  function validate(): boolean {
    const next: FieldErrors = {}
    if (username.trim() === '') next.username = t('auth.validation.required')
    if (!EMAIL_PATTERN.test(email)) next.email = t('auth.validation.email')
    const problem = passwordPolicyProblem(password)
    if (problem !== undefined) next.password = t('auth.validation.password_length')
    if (password !== confirmPassword) {
      next.confirmPassword = t('auth.validation.password_mismatch')
    }
    if (verificationRequired && emailCode.length !== 6) {
      next.emailCode = t('auth.validation.required')
    }
    if (!agree) next.agree = t('auth.validation.required')
    setErrors(next)
    return Object.keys(next).length === 0
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault()
    setFormError(undefined)
    if (!validate()) return

    setSubmitting(true)
    try {
      const body: components['schemas']['RegisterRequest'] = {
        username: username.trim(),
        email: email.trim(),
        password,
        confirmPassword,
        agree_terms: agree,
      }
      if (verificationRequired) body.email_code = emailCode
      if (aff !== undefined && aff !== '') body.aff = aff

      const data = requireData(await apiClient.POST('/api/auth/register', { body }))

      if (data.user.must_change_password) {
        onNavigate('/security?force=password')
        return
      }
      onNavigate(sanitizeAuthRedirect(redirect))
    } catch (caught) {
      if (caught instanceof ApiError && caught.status === 409) {
        setFormError(t('auth.sign_up.conflict'))
      } else if (caught instanceof ApiError && caught.status === 403) {
        setFormError(t('auth.sign_up.disabled'))
      } else if (caught instanceof ApiError && caught.status === 422) {
        setFormError(t('auth.sign_up.domain_not_allowed'))
      } else {
        setFormError(t('auth.sign_up.error'))
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <AuthPageShell
      title={t('auth.sign_up.title')}
      subtitle={
        <span>
          {t('auth.sign_up.have_account')}{' '}
          <a className="font-medium text-brand hover:underline" href="/sign-in">
            {t('auth.sign_up.sign_in')}
          </a>
        </span>
      }
    >
      <form
        className="flex flex-col gap-4"
        onSubmit={(event) => void handleSubmit(event)}
        noValidate
      >
        {formError !== undefined ? <FormAlert>{formError}</FormAlert> : null}

        <TextField
          label={t('auth.sign_up.username')}
          autoComplete="username"
          value={username}
          onChange={(event) => {
            setUsername(event.target.value)
          }}
          error={errors.username}
          required
        />

        <TextField
          label={t('auth.sign_up.email')}
          type="email"
          autoComplete="email"
          value={email}
          onChange={(event) => {
            setEmail(event.target.value)
          }}
          error={errors.email}
          required
        />

        <EmailCodeField
          purpose="register"
          email={email}
          value={emailCode}
          onChange={(next) => {
            setEmailCode(next)
            setErrors((current) => ({ ...current, emailCode: undefined }))
          }}
        />
        {errors.emailCode !== undefined ? (
          <p className="-mt-2 text-xs text-danger">{errors.emailCode}</p>
        ) : null}

        <PasswordField
          label={t('auth.sign_up.password')}
          showLabel={t('auth.sign_in.show_password')}
          hideLabel={t('auth.sign_in.hide_password')}
          autoComplete="new-password"
          value={password}
          onChange={(event) => {
            setPassword(event.target.value)
          }}
          error={errors.password}
          required
        />

        <PasswordField
          label={t('auth.sign_up.confirm_password')}
          showLabel={t('auth.sign_in.show_password')}
          hideLabel={t('auth.sign_in.hide_password')}
          autoComplete="new-password"
          value={confirmPassword}
          onChange={(event) => {
            setConfirmPassword(event.target.value)
          }}
          error={errors.confirmPassword}
          required
        />

        <label className="flex items-start gap-2 text-[13px] text-ink-2">
          <input
            type="checkbox"
            className="mt-0.5 size-4 accent-brand"
            checked={agree}
            onChange={(event) => {
              setAgree(event.target.checked)
            }}
          />
          <span>
            {t('auth.sign_up.agree_prefix')}{' '}
            <a
              className="font-medium text-brand hover:underline"
              href="/legal/terms"
              target="_blank"
              rel="noopener noreferrer"
            >
              {t('auth.sign_up.terms')}
            </a>{' '}
            {t('auth.sign_up.and')}{' '}
            <a
              className="font-medium text-brand hover:underline"
              href="/legal/privacy"
              target="_blank"
              rel="noopener noreferrer"
            >
              {t('auth.sign_up.privacy')}
            </a>
          </span>
        </label>
        {errors.agree !== undefined ? (
          <p className="text-xs text-danger">{errors.agree}</p>
        ) : null}

        <Button type="submit" className="h-11 w-full" disabled={submitting}>
          {submitting
            ? t('auth.sign_up.submitting')
            : t('auth.sign_up.submit')}
        </Button>
      </form>
    </AuthPageShell>
  )
}
