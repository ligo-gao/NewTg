import { useState } from 'react'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

type AdminUser = components['schemas']['AdminUser']

export interface AccountRecoveryDialogsProps {
  user: AdminUser
  onClose: () => void
}

type Stage = 'menu' | 'confirm-2fa' | 'confirm-password' | 'result'

/**
 * F-07-12 account recovery. The temporary password is held only in this
 * component's state: closing the dialog unmounts it, matching the
 * "shown once" contract (never copied into a toast or cache).
 */
export function AccountRecoveryDialogs({
  user,
  onClose,
}: AccountRecoveryDialogsProps) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)
  const [stage, setStage] = useState<Stage>('menu')
  const [tempPassword, setTempPassword] = useState('')
  const [copied, setCopied] = useState(false)
  const [error, setError] = useState<string | undefined>(undefined)
  const [submitting, setSubmitting] = useState(false)

  async function resetTwoFa(): Promise<void> {
    setError(undefined)
    setSubmitting(true)
    try {
      await apiClient.POST('/api/users/{id}/reset-2fa', {
        params: { path: { id: user.id } },
      })
      push({ tone: 'success', message: t('admin.recovery.reset_2fa_success') })
      onClose()
    } catch {
      setError(t('admin.recovery.error'))
    } finally {
      setSubmitting(false)
    }
  }

  async function resetPassword(): Promise<void> {
    setError(undefined)
    setSubmitting(true)
    try {
      const data = requireData(
        await apiClient.POST('/api/users/{id}/reset-password', {
          params: { path: { id: user.id } },
        }),
      )
      setTempPassword(data.temp_password)
      setStage('result')
    } catch {
      setError(t('admin.recovery.error'))
    } finally {
      setSubmitting(false)
    }
  }

  async function copy(): Promise<void> {
    try {
      await navigator.clipboard.writeText(tempPassword)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      // Clipboard unavailable: the value remains selectable.
    }
  }

  const title =
    stage === 'confirm-2fa'
      ? t('admin.recovery.reset_2fa_title')
      : stage === 'confirm-password'
        ? t('admin.recovery.reset_password_title')
        : stage === 'result'
          ? t('admin.recovery.result_title')
          : t('admin.recovery.menu')

  const description =
    stage === 'confirm-2fa'
      ? t('admin.recovery.reset_2fa_body', { username: user.username })
      : stage === 'confirm-password'
        ? t('admin.recovery.reset_password_body', { username: user.username })
        : stage === 'result'
          ? t('admin.recovery.result_body', { username: user.username })
          : undefined

  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose()
      }}
      title={title}
      description={description}
      footer={
        stage === 'menu' ? (
          <Button variant="secondary" onClick={onClose}>
            {t('common.action.close')}
          </Button>
        ) : stage === 'result' ? (
          <Button onClick={onClose}>{t('admin.recovery.saved_close')}</Button>
        ) : (
          <>
            <Button variant="secondary" onClick={() => setStage('menu')}>
              {t('common.action.cancel')}
            </Button>
            <Button
              variant={stage === 'confirm-2fa' ? 'danger' : 'primary'}
              disabled={submitting}
              onClick={() =>
                void (stage === 'confirm-2fa' ? resetTwoFa() : resetPassword())
              }
            >
              {stage === 'confirm-2fa'
                ? t('admin.recovery.reset_2fa_confirm')
                : t('admin.recovery.reset_password_confirm')}
            </Button>
          </>
        )
      }
    >
      <div className="flex flex-col gap-3">
        {error !== undefined ? <FormAlert>{error}</FormAlert> : null}

        {stage === 'menu' ? (
          <div className="flex flex-col gap-2">
            <Button
              variant="secondary"
              disabled={!user.totp_enabled}
              onClick={() => setStage('confirm-2fa')}
            >
              {t('admin.recovery.reset_2fa')}
            </Button>
            <Button variant="secondary" onClick={() => setStage('confirm-password')}>
              {t('admin.recovery.reset_password')}
            </Button>
          </div>
        ) : stage === 'result' ? (
          <>
            <div className="flex items-center gap-2">
              <code
                aria-label={t('admin.recovery.result_title')}
                className="flex-1 select-all rounded-lg bg-surface-subtle px-3 py-2 font-mono text-base text-ink-1"
              >
                {tempPassword}
              </code>
              <Button variant="secondary" onClick={() => void copy()}>
                {copied ? t('common.action.copied') : t('common.action.copy')}
              </Button>
            </div>
            <p className="text-xs text-warning">{t('admin.recovery.only_once')}</p>
          </>
        ) : (
          <p className="text-sm text-ink-2">
            {stage === 'confirm-2fa'
              ? t('admin.recovery.reset_2fa_body', { username: user.username })
              : t('admin.recovery.reset_password_body', { username: user.username })}
          </p>
        )}
      </div>
    </Dialog>
  )
}
