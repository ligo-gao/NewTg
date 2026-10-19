import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert } from '@/shared/ui/form'

export interface PreviewDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** email 才展示 subject；inapp 只预览正文。 */
  channel: 'email' | 'inapp'
  subject: string
  body: string
  loading: boolean
  error: string | null
  onRetry: () => void
}

/**
 * 纯文本预览对话框（§6.9「预览用样例值、不落投递记录」）。
 *
 * 刻意不引入 Markdown 渲染器：设计把正文定义为 Markdown，但本波以
 * `white-space: pre-wrap` 的纯文本分块展示（登记：富文本编辑器/渲染器待
 * 后续批次），因此预览不会执行任何标记逻辑，符合 P3「无裸 HTML 面」。
 */
export function PreviewDialog({
  open,
  onOpenChange,
  channel,
  subject,
  body,
  loading,
  error,
  onRetry,
}: PreviewDialogProps) {
  const { t } = useI18n()
  const subjectVisible = channel === 'email' && subject !== ''

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={t('settings.notification_templates.preview.title')}
      description={t('settings.notification_templates.preview.saved_only')}
      footer={
        <div className="flex justify-end gap-2">
          {error !== null ? (
            <Button variant="secondary" onClick={onRetry}>
              {t('settings.notification_templates.preview.retry')}
            </Button>
          ) : null}
          <Button variant="secondary" onClick={() => onOpenChange(false)}>
            {t('settings.notification_templates.preview.close')}
          </Button>
        </div>
      }
    >
      {loading ? (
        <div className="space-y-2" aria-hidden="true">
          <div className="h-4 w-40 rounded bg-surface-subtle" />
          <div className="h-24 w-full rounded-lg bg-surface-subtle" />
        </div>
      ) : null}

      {error !== null ? <FormAlert tone="danger">{error}</FormAlert> : null}

      {!loading && error === null ? (
        <div className="space-y-4" data-testid="notification-preview">
          {subjectVisible ? (
            <section>
              <h3 className="text-xs font-medium text-ink-3">
                {t('settings.notification_templates.preview.subject')}
              </h3>
              <p
                data-testid="preview-subject"
                className="mt-1 rounded-lg border border-border-hairline bg-surface-subtle px-3 py-2 text-sm text-ink-1"
              >
                {subject}
              </p>
            </section>
          ) : null}
          <section>
            <h3 className="text-xs font-medium text-ink-3">
              {t('settings.notification_templates.preview.body')}
            </h3>
            <p
              data-testid="preview-body"
              className="mt-1 whitespace-pre-wrap break-words rounded-lg border border-border-hairline bg-surface-subtle px-3 py-2 font-mono text-[13px] leading-6 text-ink-1"
            >
              {body}
            </p>
          </section>
          {channel === 'inapp' ? (
            <p className="text-xs text-ink-3">
              {t('settings.notification_templates.preview.inapp_note')}
            </p>
          ) : null}
        </div>
      ) : null}
    </Dialog>
  )
}
