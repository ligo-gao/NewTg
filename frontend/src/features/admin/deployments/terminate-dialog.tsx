import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert, TextField } from '@/shared/ui/form'

import { createIdempotencyKey, terminateDeployment } from './api'
import { useDangerCancelFocus } from './components'
import type { DeploymentActionTarget } from './deployments-tab'
import { errorMessage, isUnknownResult, retryAfterOf } from './utils'

/** 终止部署（F-08-58）：不可逆，必须输入部署名称才能确认。 */
export function TerminateDialog({
  target,
  onClose,
}: {
  target: DeploymentActionTarget | null
  onClose: () => void
}) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [typed, setTyped] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<unknown>(null)
  // 504 后幂等键已释放，禁止再次提交（终止操作尤其不可重复）。
  const [unknownResult, setUnknownResult] = useState(false)
  const [idempotencyKey, setIdempotencyKey] = useState<string | null>(null)

  const close = (): void => {
    setTyped('')
    setSubmitting(false)
    setError(null)
    setUnknownResult(false)
    setIdempotencyKey(null)
    onClose()
  }

  const submit = async (): Promise<void> => {
    if (target === null || submitting || unknownResult || typed !== target.name) return
    const key = idempotencyKey ?? createIdempotencyKey()
    if (idempotencyKey === null) setIdempotencyKey(key)
    setSubmitting(true)
    setError(null)
    try {
      await terminateDeployment(target.id, key)
      await queryClient.invalidateQueries({ queryKey: ['deployments'] })
      await queryClient.invalidateQueries({ queryKey: ['deployment', target.id] })
      close()
    } catch (cause) {
      if (isUnknownResult(cause)) setUnknownResult(true)
      setError(cause)
    } finally {
      setSubmitting(false)
    }
  }

  const matched = target !== null && typed === target.name
  const retryAfter = retryAfterOf(error)
  useDangerCancelFocus(target !== null)

  return (
    <Dialog
      open={target !== null}
      onOpenChange={(next) => {
        if (!next) close()
      }}
      title={t('admin.deployments.terminate.title')}
      description={t('admin.deployments.terminate.warning')}
      role="alertdialog"
      footer={
        <>
          <Button
            variant="secondary"
            data-danger-cancel
            onClick={close}
            disabled={submitting}
          >
            {t('admin.deployments.confirm.cancel')}
          </Button>
          <Button
            variant="danger"
            disabled={!matched || submitting || unknownResult}
            onClick={() => void submit()}
          >
            {submitting
              ? t('admin.deployments.common.saving')
              : t('admin.deployments.terminate.confirm')}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <p className="text-body text-ink-2">
          {t('admin.deployments.terminate.type_name')}
        </p>
        <TextField
          label={t('admin.deployments.terminate.name_label')}
          value={typed}
          onChange={(event) => setTyped(event.target.value)}
        />
        <p className="font-mono text-meta text-ink-3">{target?.name}</p>
        {error !== null ? (
          <FormAlert tone="danger">
            {isUnknownResult(error)
              ? t('admin.deployments.common.unknown_result')
              : errorMessage(error)}
            {retryAfter !== undefined
              ? ` ${t('admin.deployments.settings.retry_after', { seconds: retryAfter })}`
              : ''}
          </FormAlert>
        ) : null}
        {unknownResult && target !== null ? (
          <Button
            variant="secondary"
            onClick={() => {
              const id = target.id
              close()
              void queryClient.invalidateQueries({ queryKey: ['deployments'] })
              void queryClient.invalidateQueries({ queryKey: ['deployment', id] })
            }}
          >
            {t('admin.deployments.common.close_and_refresh')}
          </Button>
        ) : null}
      </div>
    </Dialog>
  )
}
