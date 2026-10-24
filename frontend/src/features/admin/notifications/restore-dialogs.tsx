import { useEffect, useState } from 'react'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert } from '@/shared/ui/form'
import { useToastStore } from '@/shared/ui/toast'

import { restoreAllTemplates, restoreTemplate } from './api'
import type {
  NotificationTemplate,
  NotificationTemplateChannel,
  NotificationLocale,
} from './types'

function errorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.detail ?? error.title
  return error instanceof Error ? error.message : String(error)
}

export interface RestoreSingleDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  template: NotificationTemplate | null
  onRestored: (restored: NotificationTemplate) => void
}

/**
 * 单行恢复默认确认（§6.9：丢弃本语言手工修改，恢复内置文案）。
 *
 * 危险确认固定 role=alertdialog 且焦点落到「取消」（initialFocus="first"，
 * footer 里取消按钮在前）。恢复只回写内容，不改变启用状态。
 */
export function RestoreSingleDialog({
  open,
  onOpenChange,
  template,
  onRestored,
}: RestoreSingleDialogProps) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!open) {
      setBusy(false)
      setError(null)
    }
  }, [open])

  async function confirm(): Promise<void> {
    if (template === null) return
    setBusy(true)
    setError(null)
    try {
      const restored = await restoreTemplate(template.key, {
        locale: template.locale,
        channel: template.channel,
      })
      push({ tone: 'success', message: t('settings.notification_templates.restore.single_success') })
      onRestored(restored)
      onOpenChange(false)
    } catch (caught) {
      setError(errorMessage(caught))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog
      open={open && template !== null}
      onOpenChange={onOpenChange}
      title={t('settings.notification_templates.restore.single_title')}
      description={`${template?.key ?? ''} · ${template?.locale ?? ''} · ${template?.channel ?? ''}`}
      role="alertdialog"
      initialFocus="first"
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={() => onOpenChange(false)} disabled={busy}>
            {t('settings.notification_templates.restore.cancel')}
          </Button>
          <Button variant="danger" onClick={() => void confirm()} disabled={busy}>
            {busy
              ? t('settings.notification_templates.restore.working')
              : t('settings.notification_templates.restore.single_confirm')}
          </Button>
        </div>
      }
    >
      <div className="space-y-3">
        {error !== null ? <FormAlert tone="danger">{error}</FormAlert> : null}
        <p className="text-body text-ink-2">
          {t('settings.notification_templates.restore.single_body')}
        </p>
      </div>
    </Dialog>
  )
}

export interface RestoreAllDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** 当前筛选命中的行数；确认时必须输入同一个数字。 */
  count: number
  categories: readonly string[]
  channel?: NotificationTemplateChannel
  locale?: NotificationLocale
  modifiedOnly: boolean
  onRestored: (restored: number) => void
}

/**
 * 批量恢复默认确认。设计要求额外输入受影响行数，避免误点把大面积模板
 * 重置（§6.9「批量恢复 → 额外要求输入受影响行数确认」）。
 */
export function RestoreAllDialog({
  open,
  onOpenChange,
  count,
  categories,
  channel,
  locale,
  modifiedOnly,
  onRestored,
}: RestoreAllDialogProps) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)
  const [typed, setTyped] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!open) {
      setTyped('')
      setBusy(false)
      setError(null)
    }
  }, [open])

  const matches = typed.trim() === String(count) && count > 0

  async function confirm(): Promise<void> {
    if (!matches) return
    setBusy(true)
    setError(null)
    try {
      const result = await restoreAllTemplates({
        categories: [...categories],
        channel,
        locale,
        modifiedOnly,
      })
      push({
        tone: 'success',
        message: t('settings.notification_templates.restore.all_success', {
          count: result.restored,
        }),
      })
      onRestored(result.restored)
      onOpenChange(false)
    } catch (caught) {
      setError(errorMessage(caught))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={t('settings.notification_templates.restore.all_title')}
      role="alertdialog"
      initialFocus="first"
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={() => onOpenChange(false)} disabled={busy}>
            {t('settings.notification_templates.restore.cancel')}
          </Button>
          <Button variant="danger" onClick={() => void confirm()} disabled={busy || !matches}>
            {busy
              ? t('settings.notification_templates.restore.working')
              : t('settings.notification_templates.restore.all_confirm')}
          </Button>
        </div>
      }
    >
      <div className="space-y-3">
        {error !== null ? <FormAlert tone="danger">{error}</FormAlert> : null}
        <p className="text-body text-ink-2">
          {t('settings.notification_templates.restore.all_body', { count })}
        </p>
        <label className="flex flex-col gap-1.5">
          <span className="text-meta font-medium text-ink-1">
            {t('settings.notification_templates.restore.all_input_label')}
          </span>
          <input
            value={typed}
            inputMode="numeric"
            disabled={busy}
            onChange={(event) => setTyped(event.target.value)}
            aria-label={t('settings.notification_templates.restore.all_input_label')}
            className="h-10 w-32 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-50"
          />
        </label>
        {typed !== '' && !matches ? (
          <p role="alert" className="text-meta text-danger">
            {t('settings.notification_templates.restore.all_input_mismatch', { count })}
          </p>
        ) : null}
      </div>
    </Dialog>
  )
}
