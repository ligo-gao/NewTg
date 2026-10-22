import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'

import { toApiError } from '@/api/client'
import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'
import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'

import { SystemTasksCard } from './system-tasks-card'

/**
 * FE-08 system status (`GET /api/system/status`, m6c-task-1 contract):
 * `{version, uptime, instances[], dependencies{db, redis, worker}}`.
 *
 * The backend is landing the endpoint in a parallel task, so every field is
 * read defensively: an older build may omit one and the page must downgrade to
 * "unknown" instead of crashing.
 */

export type DependencyState = 'healthy' | 'degraded' | 'unknown'

export interface SystemStatusResponse {
  version?: string
  uptime?: number | string
  instances?: unknown[]
  dependencies?: Record<string, unknown>
}

/**
 * `GET /api/instances` (F-08-21) is the authoritative per-replica source. The
 * system-status `instances` array is kept as a fallback for older builds and
 * as the degraded path when the registry is temporarily unavailable.
 */
export interface InstanceListResponse {
  items?: unknown[]
  current_instance_id?: string
}

export interface InstanceRow {
  name: string
  role: string
  status: string
  version: string
  lastSeen: string
}

const HEALTHY_TOKENS = ['ok', 'up', 'healthy', 'ready', 'alive', 'connected', 'active']
const DEGRADED_TOKENS = [
  'down',
  'error',
  'unhealthy',
  'degraded',
  'not ready',
  'failed',
  'stale',
  'offline',
]

function dependencyState(value: unknown): DependencyState {
  if (typeof value === 'boolean') return value ? 'healthy' : 'degraded'
  if (typeof value === 'string') {
    const token = value.toLowerCase()
    if (HEALTHY_TOKENS.includes(token)) return 'healthy'
    if (DEGRADED_TOKENS.includes(token)) return 'degraded'
    return 'unknown'
  }
  if (typeof value === 'object' && value !== null) {
    const record = value as Record<string, unknown>
    if (typeof record.ok === 'boolean') return record.ok ? 'healthy' : 'degraded'
    if (typeof record.healthy === 'boolean') return record.healthy ? 'healthy' : 'degraded'
    if (record.status !== undefined) return dependencyState(record.status)
  }
  return 'unknown'
}

function badgeVariant(state: DependencyState): StatusBadgeVariant {
  if (state === 'healthy') return 'success'
  if (state === 'degraded') return 'danger'
  return 'neutral'
}

function readString(record: Record<string, unknown>, key: string): string {
  const value = record[key]
  return typeof value === 'string' ? value : ''
}

function normalizeInstance(value: unknown, index: number): InstanceRow {
  const record =
    typeof value === 'object' && value !== null ? (value as Record<string, unknown>) : {}
  return {
    name:
      readString(record, 'instance_id') ||
      readString(record, 'node_name') ||
      readString(record, 'name') ||
      `#${index + 1}`,
    role: readString(record, 'role'),
    status: readString(record, 'status'),
    version: readString(record, 'version'),
    lastSeen: readString(record, 'last_seen') || readString(record, 'last_heartbeat'),
  }
}

export function formatUptime(
  value: number | string | undefined,
  formats: { days: string; seconds: string },
): string {
  const totalSeconds =
    typeof value === 'number' ? value : value === undefined ? Number.NaN : Number(value)
  if (!Number.isFinite(totalSeconds) || totalSeconds < 0) return '—'
  const seconds = Math.floor(totalSeconds)
  if (seconds < 60) return formats.seconds.replace('{seconds}', String(seconds))
  const days = Math.floor(seconds / 86_400)
  const hours = Math.floor((seconds % 86_400) / 3_600)
  const minutes = Math.floor((seconds % 3_600) / 60)
  return formats.days
    .replace('{days}', String(days))
    .replace('{hours}', String(hours))
    .replace('{minutes}', String(minutes))
}

export async function fetchSystemStatus(signal?: AbortSignal): Promise<SystemStatusResponse> {
  const url =
    typeof location === 'undefined'
      ? '/api/system/status'
      : new URL('/api/system/status', location.origin).toString()
  const response = await fetch(url, {
    method: 'GET',
    credentials: 'same-origin',
    headers: { Accept: 'application/json' },
    signal,
  })
  if (!response.ok) throw await toApiError(response)
  return (await response.json()) as SystemStatusResponse
}

export async function fetchInstances(signal?: AbortSignal): Promise<InstanceListResponse> {
  const url =
    typeof location === 'undefined'
      ? '/api/instances'
      : new URL('/api/instances', location.origin).toString()
  const response = await fetch(url, {
    method: 'GET',
    credentials: 'same-origin',
    headers: { Accept: 'application/json' },
    signal,
  })
  if (!response.ok) throw await toApiError(response)
  return (await response.json()) as InstanceListResponse
}

function Card({
  title,
  children,
  testId,
}: {
  title: string
  children: ReactNode
  testId?: string
}) {
  return (
    <section
      data-testid={testId}
      className="rounded-xl border border-border-default bg-surface-card p-5"
    >
      <h2 className="mb-3 text-[15px] font-semibold leading-[22px] text-ink-1">{title}</h2>
      {children}
    </section>
  )
}

export function SystemStatusPage() {
  const { t } = useI18n()
  const query = useQuery({
    queryKey: ['system-status'],
    queryFn: ({ signal }) => fetchSystemStatus(signal),
  })
  // The instance registry is a separate permission (`settings:manage`): an
  // admin without it must not see the instance card at all. Other failures
  // (503 registry unavailable) fall back to the status payload below.
  const instancesQuery = useQuery({
    queryKey: ['instances'],
    queryFn: ({ signal }) => fetchInstances(signal),
    retry: false,
  })

  const data = query.data
  const dependencies = data?.dependencies ?? {}
  const instancesForbidden =
    instancesQuery.error instanceof ApiError && instancesQuery.error.status === 403
  // Prefer the dedicated registry; fall back to the status payload so a
  // registry outage still shows the current instance.
  const instanceSource = instancesQuery.data?.items ?? data?.instances
  // The endpoint is being built in a parallel backend task; never assume the
  // collection shape (F10), otherwise a partial response blanks the page.
  const instances = (Array.isArray(instanceSource) ? instanceSource : []).map(normalizeInstance)
  const lastUpdated = query.dataUpdatedAt > 0 ? new Date(query.dataUpdatedAt).toLocaleTimeString() : null

  return (
    <div className="mx-auto flex max-w-[1200px] flex-col gap-4" data-testid="system-status-page">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold text-ink-1">{t('settings.status.title')}</h1>
          <p className="mt-1 text-sm text-ink-2">{t('settings.status.subtitle')}</p>
        </div>
        <div className="flex items-center gap-3">
          {lastUpdated !== null ? (
            <span className="text-xs text-ink-3">
              {t('settings.status.last_updated', { time: lastUpdated })}
            </span>
          ) : null}
          <Button variant="secondary" onClick={() => void query.refetch()} disabled={query.isFetching}>
            {t('settings.status.refresh')}
          </Button>
        </div>
      </header>

      {query.isPending ? (
        <p className="text-sm text-ink-2" role="status">
          {t('settings.status.loading')}
        </p>
      ) : null}

      {query.isError ? (
        <div className="space-y-3">
          <FormAlert tone="danger">
            {t('settings.status.load_error')}{' '}
            {query.error instanceof ApiError ? (query.error.detail ?? query.error.title) : ''}
          </FormAlert>
          <Button variant="secondary" onClick={() => void query.refetch()}>
            {t('settings.status.retry')}
          </Button>
        </div>
      ) : null}

      {query.isSuccess ? (
        <>
          <Card title={t('settings.status.version_title')} testId="system-status-version">
            <dl className="grid grid-cols-1 gap-3 text-sm sm:grid-cols-2">
              <div>
                <dt className="text-xs text-ink-3">{t('settings.status.version')}</dt>
                <dd className="font-mono text-ink-1">{data?.version ?? '—'}</dd>
              </div>
              <div>
                <dt className="text-xs text-ink-3">{t('settings.status.uptime')}</dt>
                <dd className="font-mono text-ink-1">
                  {formatUptime(data?.uptime, {
                    days: t('settings.status.uptime_value'),
                    seconds: t('settings.status.uptime_seconds'),
                  })}
                </dd>
              </div>
            </dl>
          </Card>

          <Card title={t('settings.status.dependencies_title')} testId="system-status-dependencies">
            <ul className="flex flex-wrap gap-4">
              {(['db', 'redis', 'worker'] as const).map((dependency) => {
                const state = dependencyState(dependencies[dependency])
                return (
                  <li key={dependency} className="flex items-center gap-2 text-sm">
                    <span className="text-ink-2">{t(`settings.status.dependency_${dependency}`)}</span>
                    <StatusBadge variant={badgeVariant(state)}>
                      {t(`settings.status.${state}`)}
                    </StatusBadge>
                  </li>
                )
              })}
            </ul>
          </Card>

          {!instancesForbidden ? (
            <Card title={t('settings.status.instances_title')} testId="system-status-instances">
            {instances.length === 0 ? (
              <p className="text-sm text-ink-3">{t('settings.status.instances_empty')}</p>
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full min-w-[640px] text-left text-sm">
                  <thead className="text-xs text-ink-3">
                    <tr>
                      <th scope="col" className="py-2 pr-4 font-medium">
                        {t('settings.status.instance_name')}
                      </th>
                      <th scope="col" className="py-2 pr-4 font-medium">
                        {t('settings.status.instance_role')}
                      </th>
                      <th scope="col" className="py-2 pr-4 font-medium">
                        {t('settings.status.instance_status')}
                      </th>
                      <th scope="col" className="py-2 pr-4 font-medium">
                        {t('settings.status.instance_version')}
                      </th>
                      <th scope="col" className="py-2 font-medium">
                        {t('settings.status.instance_last_seen')}
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {instances.map((instance) => (
                      <tr key={instance.name} className="border-t border-border-hairline">
                        <td className="py-2 pr-4 font-mono text-ink-1">{instance.name}</td>
                        <td className="py-2 pr-4 text-ink-2">{instance.role || '—'}</td>
                        <td className="py-2 pr-4 text-ink-2">{instance.status || '—'}</td>
                        <td className="py-2 pr-4 font-mono text-ink-2">{instance.version || '—'}</td>
                        <td className="py-2 font-mono text-ink-2">{instance.lastSeen || '—'}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
            </Card>
          ) : null}

          {/* F-08-21 系统任务卡（W17-2）：只读列表 + 白名单手动触发。
              卡片自行处理 403（无 settings:manage 时整卡隐藏）。 */}
          <SystemTasksCard />
        </>
      ) : null}
    </div>
  )
}
