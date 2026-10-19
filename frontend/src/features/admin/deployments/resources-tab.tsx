import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert, TextField } from '@/shared/ui/form'

import {
  estimateDeploymentPrice,
  fetchAvailableReplicas,
  fetchHardwareTypes,
  fetchLocations,
  type DeployPriceEstimation,
} from './api'
import { PriceEstimateCard, SectionCard } from './components'
import {
  anyProviderField,
  errorMessage,
  positiveInteger,
} from './utils'

/**
 * 资源与价格 Tab（F-08-53 / F-08-54）：硬件目录、地域目录、可用副本查询
 * 与价格估算器。所有数据实时来自 provider；价格卡常驻「provider 估算，
 * 非用户扣费」。
 */
export function ResourcesTab() {
  const { t } = useI18n()

  const hardwareQuery = useQuery({
    queryKey: ['deploy-hardware-types'],
    queryFn: ({ signal }) => fetchHardwareTypes(signal),
    retry: false,
  })
  const locationsQuery = useQuery({
    queryKey: ['deploy-locations'],
    queryFn: ({ signal }) => fetchLocations(signal),
    retry: false,
  })

  const hardware = hardwareQuery.data?.hardware_types ?? []
  const locations = locationsQuery.data?.locations ?? []

  const [queryHardware, setQueryHardware] = useState('')
  const [queryGpu, setQueryGpu] = useState('1')
  const [queryParams, setQueryParams] = useState<{ hardwareId: number; gpu: number } | null>(null)
  const replicasQuery = useQuery({
    queryKey: ['deploy-available-replicas', queryParams?.hardwareId, queryParams?.gpu],
    queryFn: ({ signal }) =>
      fetchAvailableReplicas(queryParams?.hardwareId ?? 0, queryParams?.gpu ?? 1, signal),
    enabled: queryParams !== null,
    retry: false,
  })

  const [estimatorHardware, setEstimatorHardware] = useState('')
  const [estimatorLocations, setEstimatorLocations] = useState<number[]>([])
  const [estimatorGpu, setEstimatorGpu] = useState('1')
  const [estimatorReplicas, setEstimatorReplicas] = useState('1')
  const [estimatorDuration, setEstimatorDuration] = useState('24')
  const [estimate, setEstimate] = useState<DeployPriceEstimation | null>(null)
  const [estimating, setEstimating] = useState(false)
  const [estimateError, setEstimateError] = useState<string | null>(null)

  const runEstimate = async (): Promise<void> => {
    const hardwareId = positiveInteger(estimatorHardware)
    const gpus = positiveInteger(estimatorGpu)
    const replicas = positiveInteger(estimatorReplicas)
    const duration = positiveInteger(estimatorDuration)
    if (
      hardwareId === null ||
      gpus === null ||
      replicas === null ||
      duration === null ||
      estimatorLocations.length === 0
    ) {
      setEstimate(null)
      setEstimateError(t('admin.deployments.resources.form_incomplete'))
      return
    }
    setEstimating(true)
    setEstimateError(null)
    try {
      const result = await estimateDeploymentPrice({
        hardware_id: hardwareId,
        location_ids: estimatorLocations,
        gpus_per_container: gpus,
        replica_count: replicas,
        duration_hours: duration,
      })
      setEstimate(result)
    } catch (cause) {
      setEstimate(null)
      setEstimateError(errorMessage(cause))
    } finally {
      setEstimating(false)
    }
  }

  return (
    <section
      className="space-y-4"
      aria-label={t('admin.deployments.page.tab_resources')}
      data-testid="resources-tab"
    >
      <SectionCard
        title={t('admin.deployments.resources.hardware_title')}
        testId="hardware-catalog"
      >
        {hardwareQuery.isPending ? (
          <div className="space-y-2" aria-hidden="true">
            {Array.from({ length: 3 }, (_, index) => (
              <div key={index} className="h-9 rounded bg-surface-subtle" />
            ))}
          </div>
        ) : null}
        {hardwareQuery.isError ? (
          <div className="space-y-2">
            <FormAlert tone="danger">{errorMessage(hardwareQuery.error)}</FormAlert>
            <Button variant="secondary" onClick={() => void hardwareQuery.refetch()}>
              {t('admin.deployments.common.retry')}
            </Button>
          </div>
        ) : null}
        {hardwareQuery.isSuccess && hardware.length === 0 ? (
          <p className="text-sm text-ink-3">{t('admin.deployments.resources.empty')}</p>
        ) : null}
        {hardware.length > 0 ? (
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="text-xs text-ink-3">
                <tr>
                  <th className="px-2 py-1 font-medium">{t('admin.deployments.resources.col_brand')}</th>
                  <th className="px-2 py-1 font-medium">{t('admin.deployments.resources.col_model')}</th>
                  <th className="px-2 py-1 font-medium">{t('admin.deployments.resources.col_gpu')}</th>
                  <th className="px-2 py-1 font-medium">{t('admin.deployments.resources.col_vram')}</th>
                  <th className="px-2 py-1 font-medium">{t('admin.deployments.resources.col_available')}</th>
                  <th className="px-2 py-1 font-medium">{t('admin.deployments.resources.col_locations')}</th>
                </tr>
              </thead>
              <tbody>
                {hardware.map((entry) => (
                  <tr key={String(entry.id)} className="border-t border-border-hairline">
                    <td className="px-2 py-1.5 text-ink-1">
                      {anyProviderField(entry, ['brand_name', 'brand']) ?? '—'}
                    </td>
                    <td className="px-2 py-1.5 font-mono text-xs text-ink-2">
                      {entry.name ?? '—'}
                    </td>
                    <td className="px-2 py-1.5 font-mono text-xs text-ink-2">
                      {anyProviderField(entry, ['gpu_count', 'gpus', 'gpu']) ?? '—'}
                    </td>
                    <td className="px-2 py-1.5 font-mono text-xs text-ink-2">
                      {anyProviderField(entry, ['vram', 'vram_gb', 'memory_gb']) ?? '—'}
                    </td>
                    <td className="px-2 py-1.5 font-mono text-xs text-ink-2">
                      {entry.available_replicas ?? 0}
                    </td>
                    <td className="px-2 py-1.5 font-mono text-xs text-ink-2">
                      {anyProviderField(entry, ['location_count', 'locations_count']) ?? '—'}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
      </SectionCard>

      <SectionCard
        title={t('admin.deployments.resources.location_title')}
        testId="location-catalog"
      >
        {locationsQuery.isPending ? (
          <div className="space-y-2" aria-hidden="true">
            {Array.from({ length: 3 }, (_, index) => (
              <div key={index} className="h-9 rounded bg-surface-subtle" />
            ))}
          </div>
        ) : null}
        {locationsQuery.isError ? (
          <div className="space-y-2">
            <FormAlert tone="danger">{errorMessage(locationsQuery.error)}</FormAlert>
            <Button variant="secondary" onClick={() => void locationsQuery.refetch()}>
              {t('admin.deployments.common.retry')}
            </Button>
          </div>
        ) : null}
        {locationsQuery.isSuccess && locations.length === 0 ? (
          <p className="text-sm text-ink-3">{t('admin.deployments.resources.empty')}</p>
        ) : null}
        {locations.length > 0 ? (
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="text-xs text-ink-3">
                <tr>
                  <th className="px-2 py-1 font-medium">{t('admin.deployments.resources.col_location_id')}</th>
                  <th className="px-2 py-1 font-medium">{t('admin.deployments.resources.col_location_name')}</th>
                  <th className="px-2 py-1 font-medium">{t('admin.deployments.resources.col_hardware')}</th>
                  <th className="px-2 py-1 font-medium">{t('admin.deployments.resources.col_replicas')}</th>
                </tr>
              </thead>
              <tbody>
                {locations.map((entry) => (
                  <tr key={String(entry.id)} className="border-t border-border-hairline">
                    <td className="px-2 py-1.5 font-mono text-xs text-ink-2">
                      {entry.id ?? '—'}
                    </td>
                    <td className="px-2 py-1.5 text-ink-1">{entry.name ?? '—'}</td>
                    <td className="px-2 py-1.5 text-xs text-ink-3">
                      {anyProviderField(entry, ['hardware', 'hardware_name']) ?? '—'}
                    </td>
                    <td className="px-2 py-1.5 font-mono text-xs text-ink-2">
                      {anyProviderField(entry, ['available_replicas', 'replicas']) ?? '—'}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
      </SectionCard>

      <SectionCard
        title={t('admin.deployments.resources.replica_title')}
        testId="replica-query"
      >
        <form
          className="flex flex-wrap items-end gap-2"
          onSubmit={(event) => {
            event.preventDefault()
            const id = positiveInteger(queryHardware)
            const gpu = positiveInteger(queryGpu)
            if (id !== null && gpu !== null) setQueryParams({ hardwareId: id, gpu })
          }}
        >
          <label className="flex flex-col gap-1 text-xs text-ink-2">
            {t('admin.deployments.resources.hardware')}
            <select
              aria-label={t('admin.deployments.resources.hardware')}
              value={queryHardware}
              onChange={(event) => setQueryHardware(event.target.value)}
              className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
            >
              <option value="">{t('admin.deployments.resources.select_hardware')}</option>
              {hardware.map((entry) => (
                <option key={String(entry.id)} value={String(entry.id)}>
                  {entry.name ?? String(entry.id)}
                </option>
              ))}
            </select>
          </label>
          <TextField
            label={t('admin.deployments.create.gpu')}
            type="number"
            inputMode="numeric"
            min={1}
            value={queryGpu}
            onChange={(event) => setQueryGpu(event.target.value)}
            className="w-24"
          />
          <Button type="submit" variant="secondary" className="h-9">
            {t('admin.deployments.resources.query')}
          </Button>
        </form>

        {replicasQuery.isError ? (
          <div className="mt-3 space-y-2">
            <FormAlert tone="danger">{errorMessage(replicasQuery.error)}</FormAlert>
            <Button variant="secondary" onClick={() => void replicasQuery.refetch()}>
              {t('admin.deployments.common.retry')}
            </Button>
          </div>
        ) : null}
        {replicasQuery.isSuccess ? (
          (replicasQuery.data?.replicas ?? []).length === 0 ? (
            <p className="mt-3 text-sm text-ink-3">
              {t('admin.deployments.resources.empty')}
            </p>
          ) : (
            <table className="mt-3 w-full text-left text-sm" data-testid="replica-result">
              <thead className="text-xs text-ink-3">
                <tr>
                  <th className="px-2 py-1 font-medium">{t('admin.deployments.resources.col_location_name')}</th>
                  <th className="px-2 py-1 font-medium">{t('admin.deployments.resources.col_replicas')}</th>
                  <th className="px-2 py-1 font-medium">{t('admin.deployments.resources.col_max_gpu')}</th>
                </tr>
              </thead>
              <tbody>
                {(replicasQuery.data?.replicas ?? []).map((entry) => (
                  <tr key={entry.location_id} className="border-t border-border-hairline">
                    <td className="px-2 py-1.5 text-ink-1">{entry.location_name}</td>
                    <td className="px-2 py-1.5 font-mono text-xs text-ink-2">
                      {entry.available_count}
                    </td>
                    <td className="px-2 py-1.5 font-mono text-xs text-ink-2">
                      {entry.max_gpus}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )
        ) : null}
      </SectionCard>

      <SectionCard
        title={t('admin.deployments.resources.price_title')}
        testId="price-estimator"
      >
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <label className="flex flex-col gap-1 text-xs text-ink-2">
            {t('admin.deployments.resources.hardware')}
            <select
              aria-label={t('admin.deployments.resources.hardware')}
              value={estimatorHardware}
              onChange={(event) => {
                setEstimatorHardware(event.target.value)
                setEstimate(null)
              }}
              className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
            >
              <option value="">{t('admin.deployments.resources.select_hardware')}</option>
              {hardware.map((entry) => (
                <option key={String(entry.id)} value={String(entry.id)}>
                  {entry.name ?? String(entry.id)}
                </option>
              ))}
            </select>
          </label>
          <TextField
            label={t('admin.deployments.create.gpu')}
            type="number"
            inputMode="numeric"
            min={1}
            value={estimatorGpu}
            onChange={(event) => {
              setEstimatorGpu(event.target.value)
              setEstimate(null)
            }}
          />
          <TextField
            label={t('admin.deployments.create.replicas')}
            type="number"
            inputMode="numeric"
            min={1}
            value={estimatorReplicas}
            onChange={(event) => {
              setEstimatorReplicas(event.target.value)
              setEstimate(null)
            }}
          />
          <TextField
            label={t('admin.deployments.create.duration_hours')}
            type="number"
            inputMode="numeric"
            min={1}
            value={estimatorDuration}
            onChange={(event) => {
              setEstimatorDuration(event.target.value)
              setEstimate(null)
            }}
          />
        </div>
        <fieldset className="mt-3">
          <legend className="text-xs text-ink-2">
            {t('admin.deployments.resources.location_multi')}
          </legend>
          <div className="mt-1 flex flex-wrap gap-3">
            {locations.map((entry) => {
              const id = typeof entry.id === 'number' ? entry.id : Number(entry.id)
              const checked = estimatorLocations.includes(id)
              return (
                <label key={String(entry.id)} className="flex items-center gap-1.5 text-sm text-ink-2">
                  <input
                    type="checkbox"
                    checked={checked}
                    onChange={(event) => {
                      setEstimatorLocations((current) =>
                        event.target.checked
                          ? [...current, id]
                          : current.filter((value) => value !== id),
                      )
                      setEstimate(null)
                    }}
                  />
                  {entry.name ?? String(entry.id)}
                </label>
              )
            })}
          </div>
        </fieldset>
        <div className="mt-3">
          <Button variant="secondary" onClick={() => void runEstimate()} disabled={estimating}>
            {estimating
              ? t('admin.deployments.price.estimating')
              : t('admin.deployments.price.estimate')}
          </Button>
        </div>
        <div className="mt-3">
          <PriceEstimateCard
            estimate={estimate}
            loading={estimating}
            error={estimateError}
            onRetry={() => void runEstimate()}
          />
        </div>
      </SectionCard>
    </section>
  )
}
