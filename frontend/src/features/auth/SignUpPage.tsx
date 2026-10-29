import { useState, type FormEvent } from 'react'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import { sanitizeAuthRedirect } from '@/lib/auth-redirect'
import { passwordPolicyProblem } from '@/lib/password-policy'
import { usePublicStatus } from '@/lib/public-status'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert, PasswordField, TextField } from '@/shared/ui/form'

import { AuthHallShell } from './AuthHallShell'
import { EmailCodeField } from './EmailCodeField'
import { TurnstileWidget } from './TurnstileWidget'

const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
const USERNAME_PATTERN = /^[a-zA-Z0-9_-]{3,32}$/

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
  const [turnstileToken, setTurnstileToken] = useState<string | undefined>(undefined)
  const [turnstileReset, setTurnstileReset] = useState(0)

  const status = usePublicStatus()
  const turnstile = status.data?.auth.turnstile
  const turnstileEnabled = turnstile?.enabled ?? false

  if (status.isError) {
    return (
      <AuthHallShell title={t('auth.sign_up.title')}>
        <div className="flex flex-col gap-3">
          <FormAlert>{t('auth.sign_up.status_error')}</FormAlert>
          <Button
            variant="secondary"
            className="h-10"
            onClick={() => void status.refetch()}
          >
            {t('common.action.retry')}
          </Button>
        </div>
      </AuthHallShell>
    )
  }

  if (status.isPending || status.data === undefined) {
    // Do not render the form before `email_verification_required` is known: a
    // fast submit would omit `email_code` and get a guaranteed 400.
    return (
      <AuthHallShell title={t('auth.sign_up.title')}>
        <div className="flex flex-col gap-[22px]" aria-busy="true">
          <div className="h-11 border-b border-border-default" />
          <div className="h-11 border-b border-border-default" />
          <div className="h-11 border-b border-border-default" />
          <div className="h-11 border-b border-border-default" />
          <div className="h-11 border-b border-border-default" />
        </div>
      </AuthHallShell>
    )
  }

  const verificationRequired = status.data.auth.email_verification_required

  if (!status.data.auth.signup_enabled) {
    return (
      <AuthHallShell title={t('auth.sign_up.title')}>
        <FormAlert tone="warning">{t('auth.sign_up.disabled')}</FormAlert>
        <p className="auth-hall-signup">
          <a className="auth-hall-link" href="/sign-in">
            {t('auth.sign_in.title', { site: t('auth.shell.brand') })}
          </a>
        </p>
      </AuthHallShell>
    )
  }

  function validate(): boolean {
    const next: FieldErrors = {}
    if (!USERNAME_PATTERN.test(username.trim())) {
      next.username = t('auth.validation.username')
    }
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
    if (turnstileEnabled && (turnstileToken === undefined || turnstileToken === '')) {
      setFormError(t('auth.turnstile.required'))
      return
    }

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
      if (turnstileEnabled) body.turnstile_token = turnstileToken

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
      } else if (
        caught instanceof ApiError &&
        caught.status === 400 &&
        caught.title === 'Invalid or expired verification code'
      ) {
        // 400 也可能是弱口令等服务端校验；只有验证码这一条有明确文案，
        // 其余仍走通用兜底，避免把口令问题误报成验证码问题。
        setFormError(t('auth.sign_up.invalid_code'))
      } else {
        setFormError(t('auth.sign_up.error'))
      }
    } finally {
      if (turnstileEnabled) {
        // Single-use token: it was consumed by the attempt regardless of the
        // outcome, so drop it and restart the challenge.
        setTurnstileToken(undefined)
        setTurnstileReset((value) => value + 1)
      }
      setSubmitting(false)
    }
  }

  return (
    <AuthHallShell title={t('auth.sign_up.title')}>
      <form onSubmit={(event) => void handleSubmit(event)} noValidate>
        {formError !== undefined ? <FormAlert>{formError}</FormAlert> : null}

        <TextField
          variant="line"
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
          variant="line"
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

        {verificationRequired ? (
          <>
            <EmailCodeField
              variant="line"
              purpose="register"
              email={email}
              value={emailCode}
              onChange={(next) => {
                setEmailCode(next)
                setErrors((current) => ({ ...current, emailCode: undefined }))
              }}
            />
            {errors.emailCode !== undefined ? (
              <p className="-mt-2 text-meta text-danger">{errors.emailCode}</p>
            ) : null}
          </>
        ) : null}

        <PasswordField
          variant="line"
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
          variant="line"
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

        <label className="auth-hall-terms">
          <input
            type="checkbox"
            className="accent-brand"
            checked={agree}
            onChange={(event) => {
              setAgree(event.target.checked)
            }}
          />
          <span>
            {t('auth.sign_up.agree_prefix')}{' '}
            <a href="/legal/terms" target="_blank" rel="noopener noreferrer">
              {t('auth.sign_up.terms')}
            </a>{' '}
            {t('auth.sign_up.and')}{' '}
            <a href="/legal/privacy" target="_blank" rel="noopener noreferrer">
              {t('auth.sign_up.privacy')}
            </a>
          </span>
        </label>
        {errors.agree !== undefined ? (
          <p className="text-meta text-danger">{errors.agree}</p>
        ) : null}

        {turnstileEnabled ? (
          <TurnstileWidget
            siteKey={turnstile?.site_key ?? ''}
            onToken={setTurnstileToken}
            resetSignal={turnstileReset}
          />
        ) : null}

        <Button
          type="submit"
          className="auth-hall-submit w-full"
          disabled={submitting}
        >
          {submitting
            ? t('auth.sign_up.submitting')
            : t('auth.sign_up.submit')}
        </Button>

        <p className="auth-hall-signup">
          {t('auth.sign_up.have_account')}{' '}
          <a className="auth-hall-link" href="/sign-in">
            {t('auth.sign_up.sign_in')}
          </a>
        </p>
      </form>
    </AuthHallShell>
  )
}
