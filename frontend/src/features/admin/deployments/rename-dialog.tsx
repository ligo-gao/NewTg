import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert, TextField } from '@/shared/ui/form'

import { checkDeploymentName, createIdempotencyKey, renameDeployment } from './api'
import type { DeploymentActionTarget } from './deployments-tab'
import { errorMessage, isUnknownResult, retryAfterOf } from './utils'

type NameState = 'idle' | 'checking' | 'available' | 'taken' | 'unknown'

/** 改名（F-08-57）：输入 → 失焦查重 → 保存；服务端提交前再查一次。 */
export function RenameDialog({
  target,
  onClose,
}: {
  target: DeploymentActionTarget | null
  onClose: () => void
}) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [name, setName] = useState('')
  const [nameState, setNameState] = useState<NameState>('idle')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [idempotencyKey, setIdempotencyKey] = useState<string | null>(null)

  const check = async (): Promise<void> => {
    const candidate = name.trim()
    if (candidate === '' || candidate === target?.name) {
      setNameState('idle')
      return
    }
    setNameState('checking')
    try {
      const result = await checkDeploymentName(candidate)
      setNameState(result.available ? 'available' : 'taken')
    } catch {
      // 网络失败不阻止继续填写；提交时服务端仍会查重并返回 409。
      setNameState('unknown')
    }
  }

  const close = (): void => {
    setName('')
    setNameState('idle')
    setError(null)
    setSubmitting(false)
    setIdempotencyKey(null)
    onClose()
  }

  const submit = async (): Promise<void> => {
    if (target === null || submitting) return
    const candidate = name.trim()
    if (candidate === '' || candidate === target.name) return
    const key = idempotencyKey ?? createIdempotencyKey()
    if (idempotencyKey === null) setIdempotencyKey(key)
    setSubmitting(true)
    setError(null)
    try {
      await renameDeployment(target.id, candidate, key)
      await queryClient.invalidateQueries({ queryKey: ['deployments'] })
      await queryClient.invalidateQueries({ queryKey: ['deployment', target.id] })
      close()
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 409) setNameState('taken')
      setError(cause)
    } finally {
      setSubmitting(false)
    }
  }

  const nameHint =
    nameState === 'checking'
      ? t('admin.deployments.create.name_checking')
      : nameState === 'available'
        ? t('admin.deployments.create.name_available')
        : nameState === 'taken'
          ? t('admin.deployments.create.name_taken')
          : nameState === 'unknown'
            ? t('admin.deployments.create.name_unknown')
            : undefined

  const retryAfter = retryAfterOf(error)

  return (
    <Dialog
      open={target !== null}
      onOpenChange={(next) => {
        if (!next) close()
      }}
      title={t('admin.deployments.rename.title')}
      description={target?.name}
      footer={
        <>
          <Button variant="secondary" onClick={close} disabled={submitting}>
            {t('admin.deployments.confirm.cancel')}
          </Button>
          <Button
            onClick={() => void submit()}
            disabled={
              submitting ||
              name.trim() === '' ||
              name.trim() === target?.name ||
              nameState === 'taken'
            }
          >
            {submitting
              ? t('admin.deployments.common.saving')
              : t('admin.deployments.rename.save')}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <TextField
          label={t('admin.deployments.rename.name')}
          value={name}
          onChange={(event) => {
            setName(event.target.value)
            setNameState('idle')
          }}
          onBlur={() => void check()}
          hint={nameHint}
          autoFocus
        />
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
      </div>
    </Dialog>
  )
}
