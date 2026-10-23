import { useQuery } from '@tanstack/react-query'
import { useEffect, useState } from 'react'

import { toApiError } from '@/api/client'
import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'
import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'

import { SystemCard } from './system-card'
import { SystemTasksCard } from './system-tasks-card'
import { SystemUpdateCard } from './system-update-card'

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
  mfa?: MfaOverview
  polling?: PollingInfo
}

/** F-08-21 全局 + 管理面 MFA 覆盖率（只读聚合，不含用户标识/种子）。 */
export interface MfaOverview {
  available?: boolean
  totp_enabled_users?: number
  total_users?: number
  mfa_coverage_pct?: number
  totp_enabled_admins?: number
  total_admins?: number
  admin_mfa_coverage_pct?: number
}

/** 轮询状态卡的只读数据；周期由服务端下发，避免前端第二份常量。 */
export interface PollingInfo {
  interval_seconds?: number
  heartbeat_interval_seconds?: number
  update_check_background_polling?: boolean
  server_time?: string
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

export function SystemStatusPage() {
  const { t } = useI18n()
  // 自动刷新暂停是浏览器本地状态：手动暂停与标签页隐藏都暂停轮询
  // （UI §6.7）。隐藏时暂停不是「用户暂停」，文案分开显示。
  const [manualPaused, setManualPaused] = useState(false)
  const [tabHidden, setTabHidden] = useState(false)

  useEffect(() => {
    const syncVisibility = () => setTabHidden(document.visibilityState === 'hidden')
    syncVisibility()
    document.addEventListener('visibilitychange', syncVisibility)
    return () => document.removeEventListener('visibilitychange', syncVisibility)
  }, [])

  const autoRefresh = !manualPaused && !tabHidden
  const pollingQuery = useQuery({
    queryKey: ['system-status'],
    queryFn: ({ signal }) => fetchSystemStatus(signal),
    refetchInterval: autoRefresh ? 10_000 : false,
  })
  // The instance registry is a separate permission (`settings:manage`): an
  // admin without it must not see the instance card at all. Other failures
  // (503 registry unavailable) fall back to the status payload below.
  const instancesQuery = useQuery({
    queryKey: ['instances'],
    queryFn: ({ signal }) => fetchInstances(signal),
    retry: false,
    refetchInterval: autoRefresh ? 10_000 : false,
  })

  const query = pollingQuery

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
  const polling = data?.polling ?? {}
  const pollingInterval =
    typeof polling.interval_seconds === 'number' ? polling.interval_seconds : 10
  const heartbeatInterval =
    typeof polling.heartbeat_interval_seconds === 'number'
      ? polling.heartbeat_interval_seconds
      : 30
  const mfa = data?.mfa
  const mfaAvailable = mfa?.available === true
  const mfaUsers = mfa?.totp_enabled_users ?? 0
  const mfaTotalUsers = mfa?.total_users ?? 0
  const mfaAdmins = mfa?.totp_enabled_admins ?? 0
  const mfaTotalAdmins = mfa?.total_admins ?? 0
  const mfaMissingUsers = mfaAvailable ? Math.max(0, mfaTotalUsers - mfaUsers) : 0
  const mfaMissingAdmins = mfaAvailable ? Math.max(0, mfaTotalAdmins - mfaAdmins) : 0
  const mfaPct = mfa?.mfa_coverage_pct ?? 0
  const mfaAdminPct = mfa?.admin_mfa_coverage_pct ?? 0

  return (
    <div className="mx-auto flex max-w-[1520px] flex-col gap-4" data-testid="system-status-page">
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
          <Button variant="secondary" onClick={() => setManualPaused((value) => !value)}>
            {manualPaused
              ? t('settings.status.polling_resume')
              : t('settings.status.polling_pause')}
          </Button>
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
          <SystemCard title={t('settings.status.version_title')} testId="system-status-version">
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
          </SystemCard>

          {/* F-08-42：更新检查紧贴版本卡——一张给「当前版本」，一张给
              「有没有更新」；按需取数，不参与页面 10s 轮询。 */}
          <SystemUpdateCard currentVersion={data?.version} />

          <SystemCard title={t('settings.status.polling_title')} testId="system-status-polling">
            <ul className="flex flex-wrap gap-x-6 gap-y-2 text-sm">
              <li className="flex items-center gap-2">
                <span className="text-ink-2">{t('settings.status.polling_interval')}</span>
                <span className="font-mono text-ink-1">
                  {t('settings.status.polling_interval_value', {
                    seconds: pollingInterval,
                  })}
                </span>
              </li>
              <li className="flex items-center gap-2">
                <span className="text-ink-2">
                  {t('settings.status.polling_heartbeat_interval')}
                </span>
                <span className="font-mono text-ink-1">
                  {t('settings.status.polling_heartbeat_value', { seconds: heartbeatInterval })}
                </span>
              </li>
              <li className="flex items-center gap-2">
                <span className="text-ink-2">
                  {t('settings.status.polling_update_background')}
                </span>
                <StatusBadge variant="neutral">
                  {polling.update_check_background_polling
                    ? t('settings.status.polling_active')
                    : t('settings.status.polling_disabled_value')}
                </StatusBadge>
              </li>
              <li className="flex items-center gap-2">
                <StatusBadge variant={autoRefresh ? 'success' : 'warning'}>
                  {tabHidden
                    ? t('settings.status.polling_paused_hidden')
                    : manualPaused
                      ? t('settings.status.polling_paused_manual')
                      : t('settings.status.polling_active')}
                </StatusBadge>
              </li>
            </ul>
            <p className="mt-2 text-xs text-ink-3">{t('settings.status.polling_note')}</p>
          </SystemCard>

          <SystemCard title={t('settings.status.dependencies_title')} testId="system-status-dependencies">
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
          </SystemCard>

          {/* F-08-21：全局 + 管理面双口径覆盖率。只展示聚合计数；低于 100%
              时给出黄色提示（不展示任何用户标识或种子）。 */}
          <SystemCard title={t('settings.status.mfa_title')} testId="system-status-mfa">
            {!mfaAvailable ? (
              <p className="text-sm text-ink-3">{t('settings.status.mfa_unavailable')}</p>
            ) : (
              <div className="grid grid-cols-1 gap-4 text-sm sm:grid-cols-2">
                <div>
                  <p className="text-xs text-ink-3">{t('settings.status.mfa_global')}</p>
                  <p className="mt-1 flex items-center gap-2">
                    <span className="font-mono text-ink-1">{mfaPct.toFixed(1)}%</span>
                    <span className="text-ink-2">
                      {t('settings.status.mfa_enabled_of_total', {
                        enabled: mfaUsers,
                        total: mfaTotalUsers,
                      })}
                    </span>
                  </p>
                  {mfaMissingUsers > 0 ? (
                    <p className="mt-1 text-xs text-warning">
                      {t('settings.status.mfa_warning_users', { count: mfaMissingUsers })}
                    </p>
                  ) : null}
                </div>
                <div>
                  <p className="text-xs text-ink-3">{t('settings.status.mfa_admin')}</p>
                  <p className="mt-1 flex items-center gap-2">
                    <span className="font-mono text-ink-1">{mfaAdminPct.toFixed(1)}%</span>
                    <span className="text-ink-2">
                      {mfaTotalAdmins === 0
                        ? t('settings.status.mfa_no_admins')
                        : t('settings.status.mfa_enabled_of_total', {
                            enabled: mfaAdmins,
                            total: mfaTotalAdmins,
                          })}
                    </span>
                  </p>
                  {mfaMissingAdmins > 0 ? (
                    <p className="mt-1 text-xs text-warning">
                      {t('settings.status.mfa_warning_admins', { count: mfaMissingAdmins })}
                      {' '}
                      <a
                        href="/admin/users?filter=admin&totp=disabled"
                        className="text-brand underline-offset-2 hover:underline"
                      >
                        {t('settings.status.mfa_manage_admins')}
                      </a>
                    </p>
                  ) : null}
                </div>
              </div>
            )}
          </SystemCard>

          {!instancesForbidden ? (
            <SystemCard title={t('settings.status.instances_title')} testId="system-status-instances">
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
            </SystemCard>
          ) : null}

          {/* F-08-21 系统任务卡（W17-2）：只读列表 + 白名单手动触发。
              卡片自行处理 403（无 settings:manage 时整卡隐藏）。 */}
          <SystemTasksCard />
        </>
      ) : null}
    </div>
  )
}
