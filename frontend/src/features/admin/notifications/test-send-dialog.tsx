import { useEffect, useState } from 'react'

import { ApiError } from '@/api/errors'
import { useI18n, type TranslateFn } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert, TextField } from '@/shared/ui/form'
import { useCountdown } from '@/shared/lib/use-countdown'
import { useToastStore } from '@/shared/ui/toast'

import { testTemplate } from './api'
import { useOnlineStatus } from './use-online'
import type { NotificationTemplate } from './types'

export interface TestSendDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  template: NotificationTemplate | null
  /** 当前管理员邮箱（GET /api/me）；为空时收件人默认为空。 */
  defaultRecipient: string
  /** true=已配置；false=未配置（禁用并说明）；null=尚不确定（不禁用）。 */
  smtpConfigured: boolean | null
}

type ResultTone = 'success' | 'danger' | 'warning'

interface TestResult {
  tone: ResultTone
  message: string
}

/**
 * 把服务端返回的发送错误归类（UI §6.9「结果就地展示成功/失败分类」）。
 * 分类只影响文案，原始 `last_error` 仍完整展示，便于复制排障。
 */
function classifyFailure(message: string | null, t: TranslateFn): string {
  if (message === null || message.trim() === '') {
    return t('settings.notification_templates.test.error_unknown')
  }
  if (/timeout|timed out|deadline exceeded/i.test(message)) {
    return t('settings.notification_templates.test.error_timeout')
  }
  if (/auth|535|credentials?|login/i.test(message)) {
    return t('settings.notification_templates.test.error_auth')
  }
  if (/connection refused|connect:|dial tcp|no such host|network/i.test(message)) {
    return t('settings.notification_templates.test.error_connection')
  }
  return t('settings.notification_templates.test.error_unknown')
}

export function TestSendDialog({
  open,
  onOpenChange,
  template,
  defaultRecipient,
  smtpConfigured,
}: TestSendDialogProps) {
  const { t } = useI18n()
  const push = useToastStore((state) => state.push)
  const online = useOnlineStatus()
  const countdown = useCountdown()

  const [recipient, setRecipient] = useState(defaultRecipient)
  const [submitting, setSubmitting] = useState(false)
  const [result, setResult] = useState<TestResult | null>(null)
  const [recipientError, setRecipientError] = useState<string | undefined>(undefined)

  // 每次打开都回到「当前管理员邮箱」的默认值，清空上一次结果与倒计时。
  useEffect(() => {
    if (!open) return
    setRecipient(defaultRecipient)
    setSubmitting(false)
    setResult(null)
    setRecipientError(undefined)
    countdown.stop()
    // countdown 的 start/stop 是稳定引用；这里只想在 open 变化时重置。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, defaultRecipient])

  const disabled =
    !online ||
    submitting ||
    countdown.running ||
    smtpConfigured === false ||
    template === null

  async function submit(): Promise<void> {
    if (template === null) return
    const address = recipient.trim()
    if (address === '') {
      setRecipientError(t('settings.notification_templates.test.recipient_required'))
      return
    }
    setRecipientError(undefined)
    setSubmitting(true)
    setResult(null)
    try {
      // variables 传空对象：服务端会用注册表样例值补齐（§3.9.6d）。
      const response = await testTemplate(template.key, {
        locale: template.locale,
        channel: 'email',
        variables: {},
        recipient: address,
      })
      if (response.status === 'sent') {
        setResult({
          tone: 'success',
          message: t('settings.notification_templates.test.success'),
        })
      } else {
        setResult({
          tone: 'danger',
          message: t('settings.notification_templates.test.failed', {
            reason: classifyFailure(response.error, t),
          }),
        })
      }
    } catch (error) {
      if (error instanceof ApiError) {
        if (error.kind === 'rate_limited') {
          const seconds = error.retry_after ?? 60
          countdown.start(seconds)
          setResult({
            tone: 'warning',
            message: t('settings.notification_templates.test.rate_limited', { seconds }),
          })
        } else if (error.kind === 'offline') {
          setResult({
            tone: 'danger',
            message: t('settings.notification_templates.test.offline'),
          })
        } else if (error.type.endsWith('smtp_not_configured')) {
          setResult({
            tone: 'danger',
            message: t('settings.notification_templates.test.smtp_missing'),
          })
        } else if (error.type.endsWith('not_email_template')) {
          setResult({
            tone: 'danger',
            message: t('settings.notification_templates.test.email_only'),
          })
        } else {
          setResult({
            tone: 'danger',
            message: t('settings.notification_templates.test.failed', {
              reason: classifyFailure(error.detail ?? error.title, t),
            }),
          })
        }
      } else {
        setResult({
          tone: 'danger',
          message: t('settings.notification_templates.test.failed', {
            reason: t('settings.notification_templates.test.error_unknown'),
          }),
        })
        push({
          tone: 'error',
          message: t('settings.notification_templates.test.error_unknown'),
        })
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={t('settings.notification_templates.test.title', {
        key: template?.key ?? '',
      })}
      description={t('settings.notification_templates.test.recipient_hint')}
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={() => onOpenChange(false)}>
            {t('settings.notification_templates.test.close')}
          </Button>
          <Button onClick={() => void submit()} disabled={disabled}>
            {submitting
              ? t('settings.notification_templates.test.sending')
              : countdown.running
                ? t('settings.notification_templates.test.retry_in', {
                    seconds: countdown.seconds,
                  })
                : t('settings.notification_templates.test.submit')}
          </Button>
        </div>
      }
    >
      <div className="space-y-3">
        {smtpConfigured === false ? (
          <FormAlert tone="warning">
            {t('settings.notification_templates.test.smtp_missing')}
          </FormAlert>
        ) : null}
        {!online ? (
          <p className="text-meta text-ink-3">
            {t('settings.notification_templates.page.offline')}
          </p>
        ) : null}

        <TextField
          label={t('settings.notification_templates.test.recipient')}
          type="email"
          value={recipient}
          error={recipientError}
          disabled={submitting}
          onChange={(event) => {
            setRecipient(event.target.value)
            setRecipientError(undefined)
          }}
        />

        {result !== null ? (
          <FormAlert tone={result.tone}>{result.message}</FormAlert>
        ) : null}
      </div>
    </Dialog>
  )
}
