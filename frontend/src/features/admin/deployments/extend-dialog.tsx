import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert, TextField } from '@/shared/ui/form'

import {
  createIdempotencyKey,
  estimateDeploymentPrice,
  extendDeployment,
  fetchDeployment,
  type DeployPriceEstimation,
} from './api'
import { PriceEstimateCard } from './components'
import type { DeploymentActionTarget } from './deployments-tab'
import {
  errorMessage,
  isUnknownResult,
  positiveInteger,
  retryAfterOf,
} from './utils'

/**
 * 续期（F-08-58）：先估算、再确认。服务端在续期前也会强制取报价，但页面
 * 必须在确认框里展示金额，否则管理员是「盲签字」。
 */
export function ExtendDialog({
  target,
  onClose,
}: {
  target: DeploymentActionTarget | null
  onClose: () => void
}) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const open = target !== null
  const detailQuery = useQuery({
    queryKey: ['deployment', target?.id],
    queryFn: ({ signal }) => fetchDeployment(target?.id ?? '', signal),
    enabled: open,
  })
  const detail = detailQuery.data

  const [hours, setHours] = useState('24')
  const [estimate, setEstimate] = useState<DeployPriceEstimation | null>(null)
  const [estimating, setEstimating] = useState(false)
  const [estimateError, setEstimateError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<unknown>(null)
  // 504 = 结果未知。后端在非 2xx（含 504）时已释放幂等键，继续用同一 key
  // 重试会真的再执行一次（重复加时长/计费），所以此时必须锁死提交，直到
  // 用户关闭并刷新确认 provider 状态；重新打开会生成新 key。
  const [unknownResult, setUnknownResult] = useState(false)
  // 同一意图的重试复用同一 key；只有成功/关闭后才丢弃。
  const [idempotencyKey, setIdempotencyKey] = useState<string | null>(null)

  const reset = (): void => {
    setHours('24')
    setEstimate(null)
    setEstimateError(null)
    setError(null)
    setSubmitting(false)
    setEstimating(false)
    setUnknownResult(false)
    setIdempotencyKey(null)
  }

  const close = (): void => {
    reset()
    onClose()
  }

  const runEstimate = async (): Promise<void> => {
    if (detail === undefined) return
    const duration = positiveInteger(hours)
    if (duration === null) return
    const locationIds = (detail.locations ?? [])
      .map((entry) => (typeof entry.id === 'number' ? entry.id : 0))
      .filter((id) => id > 0)
    if (detail.hardware_id === undefined || locationIds.length === 0) {
      setEstimateError(t('admin.deployments.price.unavailable'))
      return
    }
    setEstimating(true)
    setEstimateError(null)
    setEstimate(null)
    try {
      const result = await estimateDeploymentPrice({
        hardware_id: detail.hardware_id,
        location_ids: locationIds,
        gpus_per_container: detail.gpus_per_container ?? 1,
        replica_count: Math.max(1, detail.total_containers ?? 1),
        duration_hours: duration,
      })
      setEstimate(result)
    } catch (cause) {
      setEstimateError(errorMessage(cause))
    } finally {
      setEstimating(false)
    }
  }

  const submit = async (): Promise<void> => {
    if (target === null || estimate === null || submitting || unknownResult) return
    const duration = positiveInteger(hours)
    if (duration === null) return
    const key = idempotencyKey ?? createIdempotencyKey()
    if (idempotencyKey === null) setIdempotencyKey(key)
    setSubmitting(true)
    setError(null)
    try {
      await extendDeployment(target.id, duration, key)
      await queryClient.invalidateQueries({ queryKey: ['deployments'] })
      await queryClient.invalidateQueries({ queryKey: ['deployment', target.id] })
      close()
    } catch (cause) {
      // 504 结果未知：不自动重试、不换 key；刷新确认 provider 状态后再操作。
      if (isUnknownResult(cause)) setUnknownResult(true)
      setError(cause)
    } finally {
      setSubmitting(false)
    }
  }

  const durationValue = positiveInteger(hours)
  const unknown = isUnknownResult(error)
  const retryAfter = retryAfterOf(error)

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) close()
      }}
      title={t('admin.deployments.extend.title')}
      description={target?.name}
      footer={
        <>
          <Button variant="secondary" onClick={close} disabled={submitting}>
            {t('admin.deployments.confirm.cancel')}
          </Button>
          <Button
            onClick={() => void submit()}
            disabled={
              estimate === null || submitting || durationValue === null || unknownResult
            }
          >
            {submitting
              ? t('admin.deployments.common.saving')
              : t('admin.deployments.extend.submit')}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <TextField
          label={t('admin.deployments.extend.hours')}
          type="number"
          inputMode="numeric"
          min={1}
          value={hours}
          disabled={submitting}
          onChange={(event) => {
            setHours(event.target.value)
            // 参数变化后旧报价失效，必须重新估算。
            setEstimate(null)
            setEstimateError(null)
          }}
        />
        <Button
          variant="secondary"
          onClick={() => void runEstimate()}
          disabled={
            estimating ||
            durationValue === null ||
            detail === undefined
          }
        >
          {estimating
            ? t('admin.deployments.price.estimating')
            : t('admin.deployments.price.estimate')}
        </Button>
        <PriceEstimateCard
          estimate={estimate}
          loading={estimating}
          error={estimateError}
          onRetry={() => void runEstimate()}
        />
        {estimate !== null && durationValue !== null ? (
          <p className="text-body text-ink-2">
            {t('admin.deployments.extend.confirm_text', {
              n: durationValue,
              amount: estimate.estimated_cost,
              currency: estimate.currency,
            })}
          </p>
        ) : null}
        {error !== null ? (
          <FormAlert tone="danger">
            {unknown
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
