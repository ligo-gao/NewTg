import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useId, useMemo, useRef, useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert, TextField } from '@/shared/ui/form'

import {
  checkDeploymentName,
  createDeployment,
  createIdempotencyKey,
  estimateDeploymentPrice,
  fetchAvailableReplicas,
  fetchHardwareTypes,
  fetchLocations,
  type DeployPriceEstimation,
  type CreateDeploymentRequest,
} from './api'
import { EnvEditor, PriceEstimateCard } from './components'
import { SecretField } from './secret-field'
import {
  anyProviderField,
  errorMessage,
  formatAmount,
  isUnknownResult,
  isRateLimited,
  positiveInteger,
  retryAfterOf,
  rowsToEnv,
  splitCommand,
  type EnvRow,
} from './utils'

const STEP_KEYS = [
  'admin.deployments.create.step1',
  'admin.deployments.create.step2',
  'admin.deployments.create.step3',
  'admin.deployments.create.step4',
] as const

type NameState = 'idle' | 'checking' | 'available' | 'taken' | 'unknown'

interface NamedReplica {
  locationId: number
  name: string
  available: number
}

/**
 * 创建部署四步向导（F-08-55）。复核步骤必须勾选费用确认；提交使用页面层
 * 生成的 Idempotency-Key，同一意图的重试复用该 key，504 结果未知时禁止
 * 自动重试并要求先刷新。
 */
export function CreateDeploymentDrawer({
  open,
  onClose,
}: {
  open: boolean
  onClose: () => void
}) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const idPrefix = useId()

  const [step, setStep] = useState(0)
  const [errors, setErrors] = useState<Record<string, string>>({})

  const [name, setName] = useState('')
  const [nameState, setNameState] = useState<NameState>('idle')
  // 名称查重请求序号：名称变化后，旧响应不得覆盖新名称的可用性结论。
  const nameCheckRequest = useRef(0)
  const [imageUrl, setImageUrl] = useState('')
  const [registryUsername, setRegistryUsername] = useState('')
  const [registrySecret, setRegistrySecret] = useState<string | null>('')
  const [entrypoint, setEntrypoint] = useState('')
  const [args, setArgs] = useState('')
  const [trafficPort, setTrafficPort] = useState('')
  const [envRows, setEnvRows] = useState<EnvRow[]>([])
  const [secretEnvRows, setSecretEnvRows] = useState<EnvRow[]>([])

  const [hardwareId, setHardwareId] = useState('')
  const [gpuCount, setGpuCount] = useState('1')
  const [locationIds, setLocationIds] = useState<number[]>([])

  const [replicaCount, setReplicaCount] = useState('1')
  const [durationHours, setDurationHours] = useState('24')
  const [estimate, setEstimate] = useState<DeployPriceEstimation | null>(null)
  const [estimating, setEstimating] = useState(false)
  const [estimateError, setEstimateError] = useState<string | null>(null)
  const [estimateNonce, setEstimateNonce] = useState(0)

  const [consent, setConsent] = useState(false)
  const [allowWithoutPrice, setAllowWithoutPrice] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [submitError, setSubmitError] = useState<unknown>(null)
  const [unknownResult, setUnknownResult] = useState(false)
  const [idempotencyKey, setIdempotencyKey] = useState<string | null>(null)

  const hardwareQuery = useQuery({
    queryKey: ['deploy-hardware-types'],
    queryFn: ({ signal }) => fetchHardwareTypes(signal),
    enabled: open,
  })
  const locationsQuery = useQuery({
    queryKey: ['deploy-locations'],
    queryFn: ({ signal }) => fetchLocations(signal),
    enabled: open,
  })

  const hardware = hardwareQuery.data?.hardware_types ?? []
  const locations = locationsQuery.data?.locations ?? []
  const hardwareNumber = positiveInteger(hardwareId)
  const gpuNumber = positiveInteger(gpuCount)

  const replicasQuery = useQuery({
    queryKey: ['deploy-available-replicas', hardwareId, gpuNumber],
    queryFn: ({ signal }) =>
      fetchAvailableReplicas(hardwareNumber ?? 0, gpuNumber ?? 1, signal),
    enabled: open && hardwareNumber !== null && gpuNumber !== null,
    retry: false,
  })

  const replicaMap = useMemo(() => {
    const map = new Map<number, NamedReplica>()
    for (const entry of replicasQuery.data?.replicas ?? []) {
      map.set(entry.location_id, {
        locationId: entry.location_id,
        name: entry.location_name,
        available: entry.available_count,
      })
    }
    return map
  }, [replicasQuery.data])

  const replicaNumber = positiveInteger(replicaCount)
  // 余量不足的地域禁用（F-08-53）；已选但后续变得不可用的地域自动取消，
  // 避免提交一个服务端必然拒绝的组合。
  useEffect(() => {
    if (replicaNumber === null || replicasQuery.data === undefined) return
    setLocationIds((current) =>
      current.filter((id) => (replicaMap.get(id)?.available ?? 0) >= replicaNumber),
    )
  }, [replicaMap, replicaNumber, replicasQuery.data])

  const reset = (): void => {
    setStep(0)
    setErrors({})
    setName('')
    setNameState('idle')
    setImageUrl('')
    setRegistryUsername('')
    setRegistrySecret('')
    setEntrypoint('')
    setArgs('')
    setTrafficPort('')
    setEnvRows([])
    setSecretEnvRows([])
    setHardwareId('')
    setGpuCount('1')
    setLocationIds([])
    setReplicaCount('1')
    setDurationHours('24')
    setEstimate(null)
    setEstimateError(null)
    setEstimating(false)
    setConsent(false)
    setAllowWithoutPrice(false)
    setSubmitting(false)
    setSubmitError(null)
    setUnknownResult(false)
    setIdempotencyKey(null)
    nameCheckRequest.current += 1
  }

  const close = (): void => {
    reset()
    onClose()
  }

  // 价格估算：参数变化 400ms 后重算；旧请求用 AbortController 取消，
  // 避免晚到的旧报价覆盖新参数（TC-F-08-54-01 参数变化触发重算）。
  useEffect(() => {
    if (!open) return
    if (
      hardwareNumber === null ||
      gpuNumber === null ||
      locationIds.length === 0 ||
      replicaNumber === null
    ) {
      setEstimate(null)
      setEstimateError(null)
      return
    }
    const duration = positiveInteger(durationHours)
    if (duration === null) {
      setEstimate(null)
      return
    }
    const controller = new AbortController()
    // 参数已变化：旧报价立即作废，避免在 400ms 防抖窗口里展示过期金额。
    setEstimate(null)
    const timer = globalThis.setTimeout(() => {
      setEstimating(true)
      setEstimateError(null)
      estimateDeploymentPrice(
        {
          hardware_id: hardwareNumber,
          location_ids: locationIds,
          gpus_per_container: gpuNumber,
          replica_count: replicaNumber,
          duration_hours: duration,
        },
        controller.signal,
      )
        .then((result) => {
          if (controller.signal.aborted) return
          setEstimate(result)
          setEstimateError(null)
        })
        .catch((cause: unknown) => {
          if (controller.signal.aborted) return
          setEstimate(null)
          setEstimateError(errorMessage(cause))
        })
        .finally(() => {
          if (!controller.signal.aborted) setEstimating(false)
        })
    }, 400)
    return () => {
      controller.abort()
      globalThis.clearTimeout(timer)
    }
  }, [open, hardwareNumber, gpuNumber, locationIds, replicaNumber, durationHours, estimateNonce])

  const checkName = async (): Promise<void> => {
    const candidate = name.trim()
    if (candidate === '') {
      setNameState('idle')
      return
    }
    const request = ++nameCheckRequest.current
    setNameState('checking')
    try {
      const result = await checkDeploymentName(candidate)
      if (request !== nameCheckRequest.current) return
      setNameState(result.available ? 'available' : 'taken')
    } catch {
      if (request !== nameCheckRequest.current) return
      // 网络失败允许继续填写；提交时服务端仍会查重并返回 409。
      setNameState('unknown')
    }
  }

  const validateStep = (index: number): Record<string, string> => {
    const next: Record<string, string> = {}
    if (index === 0) {
      if (name.trim() === '') next.name = t('admin.deployments.create.field_required')
      else if (nameState === 'taken') next.name = t('admin.deployments.create.name_taken')
      if (imageUrl.trim() === '') next.image = t('admin.deployments.create.field_required')
      if (trafficPort.trim() !== '') {
        const port = Number(trafficPort)
        if (!Number.isInteger(port) || port <= 0 || port > 65535) {
          next.port = t('admin.deployments.create.field_positive')
        }
      }
    }
    if (index === 1) {
      if (hardwareNumber === null) next.hardware = t('admin.deployments.create.field_required')
      // GPU 数必填且 ≥1：清空后不得静默按 1 提交。
      if (gpuNumber === null) next.gpu = t('admin.deployments.create.field_positive')
      if (locationIds.length === 0) next.locations = t('admin.deployments.create.locations_required')
    }
    if (index === 2) {
      if (replicaNumber === null) next.replicas = t('admin.deployments.create.field_positive')
      if (positiveInteger(durationHours) === null) {
        next.duration = t('admin.deployments.create.field_positive')
      }
    }
    if (index === 3) {
      if (!consent) next.consent = t('admin.deployments.create.consent_required')
      if (estimate === null && !allowWithoutPrice) {
        next.price = t('admin.deployments.create.estimate_required')
      }
    }
    return next
  }

  const focusFirstError = (stepIndex: number, fieldErrors: Record<string, string>): void => {
    const field = Object.keys(fieldErrors)[0]
    if (field === undefined) return
    const id =
      field === 'name'
        ? `${idPrefix}-name`
        : field === 'image'
          ? `${idPrefix}-image`
          : field === 'port'
            ? `${idPrefix}-port`
            : field === 'hardware'
              ? `${idPrefix}-hardware`
              : field === 'gpu'
                ? `${idPrefix}-gpu`
                : field === 'replicas'
                ? `${idPrefix}-replicas`
                : field === 'duration'
                  ? `${idPrefix}-duration`
                  : field === 'consent'
                    ? `${idPrefix}-consent`
                    : undefined
    const element = id === undefined ? null : document.getElementById(id)
    element?.focus()
    if (stepIndex === 1 && field === 'locations') {
      document.getElementById(`${idPrefix}-locations`)?.focus()
    }
  }

  const goTo = (next: number): void => {
    if (next > step) {
      const stepErrors = validateStep(step)
      setErrors(stepErrors)
      if (Object.keys(stepErrors).length > 0) {
        focusFirstError(step, stepErrors)
        return
      }
    }
    setErrors({})
    setStep(next)
  }

  const buildBody = (): CreateDeploymentRequest | null => {
    if (hardwareNumber === null || gpuNumber === null || replicaNumber === null) {
      return null
    }
    const duration = positiveInteger(durationHours)
    if (duration === null) return null
    const body: CreateDeploymentRequest = {
      name: name.trim(),
      hardware_id: hardwareNumber,
      location_ids: locationIds,
      gpus_per_container: gpuNumber,
      replica_count: replicaNumber,
      duration_hours: duration,
      image_url: imageUrl.trim(),
    }
    if (registryUsername.trim() !== '') body.registry_username = registryUsername.trim()
    if (registrySecret !== null && registrySecret !== '') body.registry_secret = registrySecret
    if (entrypoint.trim() !== '') body.entrypoint = splitCommand(entrypoint)
    if (args.trim() !== '') body.args = splitCommand(args)
    if (trafficPort.trim() !== '') body.traffic_port = Number(trafficPort)
    const env = rowsToEnv(envRows)
    if (Object.keys(env).length > 0) body.env_variables = env
    const secretEnv = rowsToEnv(secretEnvRows)
    if (Object.keys(secretEnv).length > 0) body.secret_env_variables = secretEnv
    return body
  }

  const submit = async (): Promise<void> => {
    // 复核是最后一步：逐项重新校验，错误定位回对应步骤。
    for (let index = 0; index < 4; index += 1) {
      const stepErrors = validateStep(index)
      if (Object.keys(stepErrors).length > 0) {
        setErrors(stepErrors)
        if (index !== step) setStep(index)
        focusFirstError(index, stepErrors)
        return
      }
    }
    const body = buildBody()
    if (body === null || submitting) return
    // 同一用户操作重试复用同一 key；结果未知时不允许在这里再用它重试。
    const key = idempotencyKey ?? createIdempotencyKey()
    if (idempotencyKey === null) setIdempotencyKey(key)
    setSubmitting(true)
    setSubmitError(null)
    setUnknownResult(false)
    try {
      await createDeployment(body, key)
      await queryClient.invalidateQueries({ queryKey: ['deployments'] })
      close()
    } catch (cause) {
      if (isUnknownResult(cause)) setUnknownResult(true)
      setSubmitError(cause)
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

  const selectedHardware = hardware.find(
    (entry) => String(entry.id ?? '') === hardwareId,
  )
  const locationNames = locationIds
    .map((id) => replicasQuery.data?.replicas.find((entry) => entry.location_id === id)?.location_name ?? String(id))
    .join(', ')
  const retryAfter = retryAfterOf(submitError)
  const rateLimited = isRateLimited(submitError)

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next && !submitting) close()
      }}
      title={t('admin.deployments.create.title')}
      variant="sheet"
      panelClassName="max-w-[720px] lg:max-w-[680px]"
      dismissible={!submitting}
      footer={
        <>
          <Button
            variant="secondary"
            onClick={() => goTo(Math.max(0, step - 1))}
            disabled={step === 0 || submitting}
          >
            {t('admin.deployments.common.previous')}
          </Button>
          {step < 3 ? (
            <Button onClick={() => goTo(step + 1)} disabled={submitting}>
              {t('admin.deployments.common.next')}
            </Button>
          ) : (
            <Button
              onClick={() => void submit()}
              disabled={submitting || unknownResult}
            >
              {submitting
                ? t('admin.deployments.create.submitting')
                : t('admin.deployments.create.submit')}
            </Button>
          )}
        </>
      }
    >
      <div className="flex flex-col gap-4">
        <div
          role="tablist"
          aria-label={t('admin.deployments.create.steps_label')}
          className="flex flex-wrap gap-1 border-b border-border-hairline"
        >
          {STEP_KEYS.map((key, index) => {
            const selected = index === step
            return (
              <button
                key={key}
                type="button"
                role="tab"
                aria-selected={selected}
                tabIndex={selected ? 0 : -1}
                id={`${idPrefix}-step-${index}`}
                onClick={() => goTo(index)}
                onKeyDown={(event) => {
                  if (event.key !== 'ArrowRight' && event.key !== 'ArrowLeft') return
                  event.preventDefault()
                  const next = event.key === 'ArrowRight'
                    ? Math.min(3, index + 1)
                    : Math.max(0, index - 1)
                  document.getElementById(`${idPrefix}-step-${next}`)?.focus()
                  goTo(next)
                }}
                className={[
                  'h-9 rounded-t-lg px-3 text-meta',
                  selected
                    ? 'border-b-2 border-brand font-medium text-ink-1'
                    : 'text-ink-2 hover:bg-surface-hover hover:text-ink-1',
                ].join(' ')}
              >
                {`${index + 1}. ${t(key)}`}
              </button>
            )
          })}
        </div>

        {step === 0 ? (
          <div className="space-y-3">
            <TextField
              id={`${idPrefix}-name`}
              label={t('admin.deployments.create.name')}
              value={name}
              onChange={(event) => {
                setName(event.target.value)
                setNameState('idle')
                nameCheckRequest.current += 1
              }}
              onBlur={() => void checkName()}
              hint={nameHint}
              error={errors.name}
              disabled={submitting}
            />
            {nameState === 'checking' ? (
              <p className="text-meta text-ink-3" role="status">
                {t('admin.deployments.create.name_checking')}
              </p>
            ) : null}
            <TextField
              id={`${idPrefix}-image`}
              label={t('admin.deployments.create.image_url')}
              value={imageUrl}
              onChange={(event) => setImageUrl(event.target.value)}
              error={errors.image}
              disabled={submitting}
            />
            <TextField
              label={t('admin.deployments.create.registry_username')}
              value={registryUsername}
              onChange={(event) => setRegistryUsername(event.target.value)}
              disabled={submitting}
            />
            <SecretField
              id={`${idPrefix}-registry-secret`}
              label={t('admin.deployments.create.registry_secret')}
              value={registrySecret}
              showBadge={false}
              disabled={submitting}
              onChange={setRegistrySecret}
            />
            <TextField
              label={t('admin.deployments.create.entrypoint')}
              value={entrypoint}
              onChange={(event) => setEntrypoint(event.target.value)}
              disabled={submitting}
            />
            <TextField
              label={t('admin.deployments.create.args')}
              value={args}
              onChange={(event) => setArgs(event.target.value)}
              disabled={submitting}
            />
            <TextField
              id={`${idPrefix}-port`}
              label={t('admin.deployments.create.traffic_port')}
              type="number"
              inputMode="numeric"
              min={1}
              max={65535}
              value={trafficPort}
              onChange={(event) => setTrafficPort(event.target.value)}
              error={errors.port}
              disabled={submitting}
            />
            <EnvEditor
              idPrefix={`${idPrefix}-env`}
              label={t('admin.deployments.create.env')}
              rows={envRows}
              onChange={setEnvRows}
              disabled={submitting}
            />
            <EnvEditor
              idPrefix={`${idPrefix}-secret-env`}
              label={t('admin.deployments.create.secret_env')}
              rows={secretEnvRows}
              onChange={setSecretEnvRows}
              secret
              disabled={submitting}
            />
          </div>
        ) : null}

        {step === 1 ? (
          <div className="space-y-3">
            {hardwareQuery.isPending ? (
              <p className="text-body text-ink-2" role="status">
                {t('admin.deployments.common.loading')}
              </p>
            ) : null}
            {hardwareQuery.isError ? (
              <div className="space-y-2">
                <FormAlert tone="danger">{errorMessage(hardwareQuery.error)}</FormAlert>
                <Button variant="secondary" onClick={() => void hardwareQuery.refetch()}>
                  {t('admin.deployments.common.retry')}
                </Button>
              </div>
            ) : null}
            {hardware.length === 0 && hardwareQuery.isSuccess ? (
              <p className="text-body text-ink-3">
                {t('admin.deployments.resources.empty')}
              </p>
            ) : null}
            <label className="flex flex-col gap-1 text-meta font-medium text-ink-1">
              {t('admin.deployments.create.hardware')}
              <select
                id={`${idPrefix}-hardware`}
                aria-label={t('admin.deployments.create.hardware')}
                value={hardwareId}
                disabled={submitting}
                onChange={(event) => {
                  setHardwareId(event.target.value)
                  setLocationIds([])
                }}
                className="h-10 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1"
              >
                <option value="">{t('admin.deployments.resources.select_hardware')}</option>
                {hardware.map((entry) => (
                  <option key={String(entry.id)} value={String(entry.id)}>
                    {`${anyProviderField(entry, ['brand_name', 'brand']) ?? ''} ${entry.name ?? String(entry.id)}`.trim()}
                  </option>
                ))}
              </select>
              {errors.hardware !== undefined ? (
                <span className="text-meta text-danger">{errors.hardware}</span>
              ) : null}
            </label>
            <TextField
              id={`${idPrefix}-gpu`}
              label={t('admin.deployments.create.gpu')}
              type="number"
              inputMode="numeric"
              min={1}
              value={gpuCount}
              onChange={(event) => {
                setGpuCount(event.target.value)
                setLocationIds([])
              }}
              error={errors.gpu}
              disabled={submitting}
            />
            <div className="flex flex-col gap-1" id={`${idPrefix}-locations`} tabIndex={-1}>
              <p className="text-meta font-medium text-ink-1">
                {t('admin.deployments.create.locations')}
              </p>
              <p className="text-meta text-ink-3">
                {t('admin.deployments.create.locations_hint')}
              </p>
              {errors.locations !== undefined ? (
                <span className="text-meta text-danger">{errors.locations}</span>
              ) : null}
              {replicasQuery.isPending && hardwareNumber !== null ? (
                <p className="text-meta text-ink-3" role="status">
                  {t('admin.deployments.common.loading')}
                </p>
              ) : null}
              {locations.map((location) => {
                const id = typeof location.id === 'number' ? location.id : Number(location.id)
                const available = replicaMap.get(id)?.available ?? 0
                const insufficient = replicaNumber === null || available < replicaNumber
                const checked = locationIds.includes(id)
                return (
                  <label
                    key={String(location.id)}
                    className={[
                      'flex items-center justify-between gap-2 text-body',
                      insufficient ? 'text-ink-3' : 'text-ink-1',
                    ].join(' ')}
                  >
                    <span className="flex items-center gap-2">
                      <input
                        type="checkbox"
                        checked={checked}
                        disabled={insufficient || submitting}
                        onChange={(event) => {
                          setLocationIds((current) =>
                            event.target.checked
                              ? [...current, id]
                              : current.filter((entry) => entry !== id),
                          )
                        }}
                      />
                      {location.name ?? String(location.id)}
                    </span>
                    <span className="font-mono text-meta">
                      {replicaMap.get(id)?.available ?? 0}
                    </span>
                  </label>
                )
              })}
              {locations.length === 0 && locationsQuery.isSuccess ? (
                <p className="text-body text-ink-3">
                  {t('admin.deployments.resources.empty')}
                </p>
              ) : null}
            </div>
          </div>
        ) : null}

        {step === 2 ? (
          <div className="space-y-3">
            <TextField
              id={`${idPrefix}-replicas`}
              label={t('admin.deployments.create.replicas')}
              type="number"
              inputMode="numeric"
              min={1}
              value={replicaCount}
              onChange={(event) => setReplicaCount(event.target.value)}
              error={errors.replicas}
              disabled={submitting}
            />
            <TextField
              id={`${idPrefix}-duration`}
              label={t('admin.deployments.create.duration_hours')}
              type="number"
              inputMode="numeric"
              min={1}
              value={durationHours}
              onChange={(event) => setDurationHours(event.target.value)}
              error={errors.duration}
              disabled={submitting}
            />
            <PriceEstimateCard
              estimate={estimate}
              loading={estimating}
              error={estimateError}
              onRetry={() => setEstimateNonce((current) => current + 1)}
            />
          </div>
        ) : null}

        {step === 3 ? (
          <div className="space-y-3">
            <dl className="grid grid-cols-1 gap-2 text-body sm:grid-cols-2">
              <div>
                <dt className="text-meta text-ink-3">{t('admin.deployments.create.name')}</dt>
                <dd className="font-mono text-ink-1">{name}</dd>
              </div>
              <div>
                <dt className="text-meta text-ink-3">{t('admin.deployments.create.image_url')}</dt>
                <dd className="break-all font-mono text-meta text-ink-1">{imageUrl}</dd>
              </div>
              <div>
                <dt className="text-meta text-ink-3">{t('admin.deployments.create.hardware')}</dt>
                <dd className="text-ink-1">
                  {[anyProviderField(selectedHardware ?? {}, ['brand_name', 'brand']), selectedHardware?.name]
                    .filter(Boolean)
                    .join(' ') || '—'}
                </dd>
              </div>
              <div>
                <dt className="text-meta text-ink-3">{t('admin.deployments.create.locations')}</dt>
                <dd className="text-ink-1">{locationNames || '—'}</dd>
              </div>
              <div>
                <dt className="text-meta text-ink-3">{t('admin.deployments.create.replicas')}</dt>
                <dd className="font-mono text-ink-1">
                  {replicaCount} × {gpuCount}
                </dd>
              </div>
              <div>
                <dt className="text-meta text-ink-3">{t('admin.deployments.create.duration_hours')}</dt>
                <dd className="font-mono text-ink-1">{durationHours}</dd>
              </div>
              <div className="sm:col-span-2">
                <dt className="text-meta text-ink-3">{t('admin.deployments.price.title')}</dt>
                <dd className="font-mono text-ink-1">
                  {estimate === null
                    ? t('admin.deployments.price.unavailable')
                    : formatAmount(estimate.estimated_cost, estimate.currency)}
                </dd>
              </div>
            </dl>
            <PriceEstimateCard
              estimate={estimate}
              loading={estimating}
              error={estimateError}
            />
            <label className="flex items-start gap-2 text-body text-ink-2">
              <input
                id={`${idPrefix}-consent`}
                type="checkbox"
                checked={consent}
                onChange={(event) => setConsent(event.target.checked)}
                disabled={submitting}
              />
              <span>{t('admin.deployments.create.consent')}</span>
            </label>
            {errors.consent !== undefined ? (
              <p className="text-meta text-danger">{errors.consent}</p>
            ) : null}
            {estimate === null && estimateError !== null ? (
              <label className="flex items-start gap-2 text-body text-ink-2">
                <input
                  type="checkbox"
                  checked={allowWithoutPrice}
                  onChange={(event) => setAllowWithoutPrice(event.target.checked)}
                />
                <span>{t('admin.deployments.create.allow_without_price')}</span>
              </label>
            ) : null}
            {errors.price !== undefined ? (
              <p className="text-meta text-danger">{errors.price}</p>
            ) : null}
          </div>
        ) : null}

        {submitError !== null ? (
          <FormAlert tone="danger">
            {unknownResult
              ? t('admin.deployments.common.unknown_result')
              : rateLimited
                ? `${t('admin.deployments.settings.rate_limited')}${
                    retryAfter !== undefined
                      ? ` ${t('admin.deployments.settings.retry_after', { seconds: retryAfter })}`
                      : ''
                  }`
                : errorMessage(submitError)}
          </FormAlert>
        ) : null}
        {unknownResult ? (
          <Button
            variant="secondary"
            onClick={() => {
              close()
              void queryClient.invalidateQueries({ queryKey: ['deployments'] })
            }}
          >
            {t('admin.deployments.create.close_and_refresh')}
          </Button>
        ) : null}
      </div>
    </Dialog>
  )
}
