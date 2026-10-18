import { useState, type ReactNode } from 'react'
import { useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { useToastStore } from '@/shared/ui/toast'

export interface SignOutOthersDialogProps {
  /** Number of sessions that will be revoked (excludes the current one). */
  count: number
  trigger: ReactNode
}

/** F-01-14 "sign out everywhere else" confirmation. */
export function SignOutOthersDialog({ count, trigger }: SignOutOthersDialogProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const [open, setOpen] = useState(false)
  const [submitting, setSubmitting] = useState(false)

  async function confirm(): Promise<void> {
    setSubmitting(true)
    try {
      await apiClient.POST('/api/sessions/revoke-others', {})
      push({ tone: 'success', message: t('account.sessions.revoked_others') })
      await queryClient.invalidateQueries({ queryKey: ['sessions'] })
      setOpen(false)
    } catch {
      push({ tone: 'error', message: t('account.sessions.revoke_error') })
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <>
      <span
        onClick={() => {
          setOpen(true)
        }}
      >
        {trigger}
      </span>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title={t('account.sessions.others_title')}
        description={t('account.sessions.others_body')}
        footer={
          <>
            <Button variant="secondary" onClick={() => setOpen(false)}>
              {t('common.action.cancel')}
            </Button>
            <Button
              variant="danger"
              disabled={submitting || count === 0}
              onClick={() => void confirm()}
            >
              {t('account.sessions.others_confirm')}
            </Button>
          </>
        }
      >
        <p className="text-sm text-ink-2">
          {t('account.sessions.others_body')} ({count})
        </p>
      </Dialog>
    </>
  )
}
