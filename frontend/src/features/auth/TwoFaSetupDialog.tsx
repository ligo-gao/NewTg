import { useEffect, useState } from 'react'
import { QRCodeSVG } from 'qrcode.react'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert } from '@/shared/ui/form'

import { OtpInput } from './OtpInput'

export interface TwoFaSetupDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** Called after the user stored the backup codes and closed the wizard. */
  onEnabled?: () => void
}

type Step = 'scan' | 'verify' | 'codes'

/**
 * F-01-10 enable wizard. The API only reveals backup codes when `enable`
 * succeeds, so the order is scan → verify → save codes (the design's visual
 * step numbering is preserved, only the data dependency fixes the order).
 *
 * The `otpauth://` URI is rendered as an SVG QR code (`qrcode.react`, pinned
 * ISC dependency); the manual setup key remains as the fallback for
 * authenticators that cannot scan.
 */
export function TwoFaSetupDialog({
  open,
  onOpenChange,
  onEnabled,
}: TwoFaSetupDialogProps) {
  const { t } = useI18n()
  const [step, setStep] = useState<Step>('scan')
  const [secret, setSecret] = useState('')
  const [otpauthUrl, setOtpauthUrl] = useState('')
  const [code, setCode] = useState('')
  const [backupCodes, setBackupCodes] = useState<string[]>([])
  const [stored, setStored] = useState(false)
  const [loading, setLoading] = useState(false)
  const [alreadyEnabled, setAlreadyEnabled] = useState(false)
  const [error, setError] = useState<string | undefined>(undefined)

  useEffect(() => {
    if (!open) return
    let cancelled = false
    setStep('scan')
    setSecret('')
    setOtpauthUrl('')
    setCode('')
    setBackupCodes([])
    setStored(false)
    setAlreadyEnabled(false)
    setError(undefined)
    setLoading(true)

    void (async () => {
      try {
        const data = requireData(await apiClient.POST('/api/2fa/setup', {}))
        if (cancelled) return
        setSecret(data.secret)
        setOtpauthUrl(data.otpauth_url)
      } catch (caught) {
        if (cancelled) return
        if (caught instanceof ApiError && caught.status === 409) {
          setAlreadyEnabled(true)
        } else {
          setError(t('auth.two_fa_setup.error'))
        }
      } finally {
        if (!cancelled) setLoading(false)
      }
    })()

    return () => {
      cancelled = true
    }
    // `t` is stable per locale; the wizard must only (re)load when reopened.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  async function enable(): Promise<void> {
    setError(undefined)
    setLoading(true)
    try {
      const data = requireData(
        await apiClient.POST('/api/2fa/enable', { body: { secret, code } }),
      )
      setBackupCodes(data.backup_codes)
      setStep('codes')
    } catch {
      setError(t('auth.two_fa_setup.error'))
    } finally {
      setLoading(false)
    }
  }

  async function copy(value: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(value)
    } catch {
      // Clipboard permission denial is non-fatal: the value stays selectable.
    }
  }

  const title = alreadyEnabled
    ? t('auth.two_fa_setup.already_title')
    : t('auth.two_fa_setup.title')

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={title}
      // The backup codes are one-time material: this step must not vanish on
      // an accidental Escape/backdrop click before the user saves them.
      dismissible={step !== 'codes'}
      footer={
        alreadyEnabled ? (
          <Button onClick={() => onOpenChange(false)}>{t('common.action.close')}</Button>
        ) : step === 'scan' ? (
          <>
            <Button variant="secondary" onClick={() => onOpenChange(false)}>
              {t('common.action.cancel')}
            </Button>
            <Button disabled={loading || secret === ''} onClick={() => setStep('verify')}>
              {t('auth.two_fa_setup.next')}
            </Button>
          </>
        ) : step === 'verify' ? (
          <>
            <Button variant="secondary" onClick={() => setStep('scan')}>
              {t('auth.two_fa_setup.back')}
            </Button>
            <Button disabled={loading || code.length !== 6} onClick={() => void enable()}>
              {t('auth.two_fa_setup.verify')}
            </Button>
          </>
        ) : (
          <Button
            disabled={!stored}
            onClick={() => {
              onEnabled?.()
              onOpenChange(false)
            }}
          >
            {t('auth.two_fa_setup.finish')}
          </Button>
        )
      }
    >
      <div className="flex flex-col gap-3">
        {error !== undefined ? <FormAlert>{error}</FormAlert> : null}

        {alreadyEnabled ? (
          <p className="text-ink-2">{t('auth.two_fa_setup.already_body')}</p>
        ) : loading && secret === '' ? (
          <p className="text-ink-3">{t('auth.two_fa_setup.loading')}</p>
        ) : step === 'scan' ? (
          <>
            <h3 className="text-body font-semibold text-ink-1">
              {t('auth.two_fa_setup.scan_title')}
            </h3>
            <p className="text-meta text-ink-2">
              {t('auth.two_fa_setup.scan_description')}
            </p>
            <div className="flex items-center gap-4">
              <div className="shrink-0 rounded-lg border border-border-hairline bg-surface-card p-2">
                <QRCodeSVG
                  value={otpauthUrl}
                  size={160}
                  level="M"
                  marginSize={2}
                  title={t('auth.two_fa_setup.scan_title')}
                />
              </div>
              <p className="text-meta text-ink-2">
                {t('auth.two_fa_setup.scan_manual_hint')}
              </p>
            </div>
            <label className="text-meta font-medium text-ink-1" htmlFor="twofa-secret">
              {t('auth.two_fa_setup.secret')}
            </label>
            <div className="flex items-center gap-2">
              <input
                id="twofa-secret"
                readOnly
                value={secret}
                className="h-10 w-full rounded-lg border border-border-default bg-surface-subtle px-3 font-mono text-meta text-ink-1"
              />
              <Button variant="secondary" onClick={() => void copy(secret)}>
                {t('common.action.copy')}
              </Button>
            </div>
            <p className="break-all font-mono text-badge text-ink-3">{otpauthUrl}</p>
          </>
        ) : step === 'verify' ? (
          <>
            <h3 className="text-body font-semibold text-ink-1">
              {t('auth.two_fa_setup.verify_title')}
            </h3>
            <p className="text-meta text-ink-2">
              {t('auth.two_fa_setup.verify_description')}
            </p>
            <OtpInput
              value={code}
              onChange={setCode}
              ariaLabel={t('auth.two_factor.group_label')}
              disabled={loading}
            />
          </>
        ) : (
          <>
            <h3 className="text-body font-semibold text-ink-1">
              {t('auth.two_fa_setup.codes_title')}
            </h3>
            <p className="text-meta text-ink-2">
              {t('auth.two_fa_setup.codes_description')}
            </p>
            <ul className="grid grid-cols-2 gap-2 rounded-lg bg-surface-subtle p-3 font-mono text-meta">
              {backupCodes.map((value, index) => (
                <li key={value}>
                  <button
                    type="button"
                    aria-label={`${t('auth.two_fa_setup.codes_title')} ${index + 1}`}
                    className="w-full rounded px-1 py-0.5 text-left hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
                    onClick={() => void copy(value)}
                  >
                    {value}
                  </button>
                </li>
              ))}
            </ul>
            <label className="flex items-start gap-2 text-meta text-ink-2">
              <input
                type="checkbox"
                className="mt-0.5 size-4 accent-brand"
                checked={stored}
                onChange={(event) => setStored(event.target.checked)}
              />
              <span>{t('auth.two_fa_setup.stored')}</span>
            </label>
          </>
        )}
      </div>
    </Dialog>
  )
}
