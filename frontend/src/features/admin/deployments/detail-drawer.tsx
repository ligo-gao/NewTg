import { useQuery } from '@tanstack/react-query'
import { useCallback, useEffect, useRef, useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { FormAlert } from '@/shared/ui/form'

import {
  fetchDeployment,
  fetchDeploymentContainer,
  fetchDeploymentContainers,
  fetchDeploymentLogs,
  type DeployContainer,
  type DeployDeploymentDetail,
} from './api'
import { DeploymentStatusBadge } from './status'
import type { DeploymentActionTarget } from './deployments-tab'
import { useOnlineStatus } from './use-online'
import {
  errorMessage,
  formatDateTime,
  formatRemainingMinutes,
  formatRuntimeSeconds,
} from './utils'

type DetailTab = 'overview' | 'containers' | 'logs'

const LOG_LEVELS = ['', 'debug', 'info', 'warn', 'error']
const LOG_STREAMS = ['', 'stdout', 'stderr']

function Field({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="text-xs text-ink-3">{label}</dt>
      <dd className="font-mono text-sm text-ink-1">{value}</dd>
    </div>
  )
}

function Overview({ detail }: { detail: DeployDeploymentDetail }) {
  const { t, locale } = useI18n()
  const config = detail.container_config
  const locations = (detail.locations ?? [])
    .map((entry) => (typeof entry.name === 'string' ? entry.name : String(entry.id ?? '')))
    .filter((entry) => entry !== '')
    .join(', ')
  return (
    <div className="space-y-4">
      <dl className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <Field
          label={t('admin.deployments.detail.hardware')}
          value={[detail.brand_name, detail.hardware_name].filter(Boolean).join(' ') || '—'}
        />
        <Field label={t('admin.deployments.detail.location')} value={locations || '—'} />
        <Field
          label={t('admin.deployments.detail.gpu')}
          value={String(detail.gpus_per_container ?? detail.total_gpus ?? '—')}
        />
        <Field
          label={t('admin.deployments.detail.replicas')}
          value={String(detail.total_containers ?? '—')}
        />
        <Field
          label={t('admin.deployments.detail.remaining')}
          value={formatRemainingMinutes(detail.compute_minutes_remaining)}
        />
        <Field
          label={t('admin.deployments.detail.paid')}
          // 详情契约没有币种字段；不臆造 USD，按 provider 返回的数值原样展示。
          value={detail.amount_paid === undefined ? '—' : String(detail.amount_paid)}
        />
        <Field
          label={t('admin.deployments.detail.created')}
          value={formatDateTime(detail.created_at, locale)}
        />
        <Field
          label={t('admin.deployments.detail.started')}
          value={formatDateTime(detail.started_at, locale)}
        />
        <Field
          label={t('admin.deployments.detail.finished')}
          value={formatDateTime(detail.finished_at, locale)}
        />
      </dl>

      <div className="rounded-lg border border-border-default bg-surface-subtle p-3">
        <p className="text-xs font-medium text-ink-3">
          {t('admin.deployments.detail.container_config')}
        </p>
        <dl className="mt-2 grid grid-cols-1 gap-2 text-sm sm:grid-cols-2">
          <Field
            label={t('admin.deployments.detail.image')}
            value={config?.image_url ?? '—'}
          />
          <Field
            label={t('admin.deployments.detail.entrypoint')}
            value={
              config?.entrypoint !== undefined && config.entrypoint.length > 0
                ? config.entrypoint.join(' ')
                : '—'
            }
          />
          <Field
            label={t('admin.deployments.detail.traffic_port')}
            value={config?.traffic_port === undefined ? '—' : String(config.traffic_port)}
          />
          <Field
            label={t('admin.deployments.detail.env')}
            value={
              config?.env_variables === undefined
                ? '—'
                : Object.keys(config.env_variables).join(', ') || '—'
            }
          />
        </dl>
        {/* 秘密环境变量永不返回（§3.20.5）：这里只给出口径提示。 */}
        <p className="mt-2 text-xs text-ink-3">
          {t('admin.deployments.detail.secrets_hidden')}
        </p>
      </div>
    </div>
  )
}

function ContainerTable({
  containers,
  onOpen,
}: {
  containers: DeployContainer[]
  onOpen: (containerId: string) => void
}) {
  const { t } = useI18n()
  if (containers.length === 0) {
    return (
      <p className="rounded-xl border border-dashed border-border-default px-4 py-8 text-center text-sm text-ink-3">
        {t('admin.deployments.detail.no_containers')}
      </p>
    )
  }
  return (
    <div className="overflow-x-auto rounded-xl border border-border-default">
      <table className="w-full text-left text-sm">
        <thead className="bg-surface-subtle text-xs text-ink-3">
          <tr>
            <th className="px-3 py-2 font-medium">
              {t('admin.deployments.detail.container_id')}
            </th>
            <th className="px-3 py-2 font-medium">
              {t('admin.deployments.detail.container_status')}
            </th>
            <th className="px-3 py-2 font-medium">
              {t('admin.deployments.detail.container_hardware')}
            </th>
            <th className="px-3 py-2 font-medium">
              {t('admin.deployments.detail.container_gpu')}
            </th>
            <th className="px-3 py-2 font-medium">
              {t('admin.deployments.detail.container_runtime')}
            </th>
            <th className="px-3 py-2 font-medium">
              {t('admin.deployments.detail.container_url')}
            </th>
          </tr>
        </thead>
        <tbody>
          {containers.map((container) => (
            <tr
              key={container.container_id}
              className="border-t border-border-hairline"
            >
              <td className="px-3 py-2">
                <button
                  type="button"
                  className="font-mono text-xs text-brand underline"
                  onClick={() => onOpen(container.container_id)}
                >
                  {container.container_id}
                </button>
              </td>
              <td className="px-3 py-2">
                <DeploymentStatusBadge status={container.status} />
              </td>
              <td className="px-3 py-2 text-xs text-ink-2">
                {container.hardware ?? '—'}
              </td>
              <td className="px-3 py-2 font-mono text-xs text-ink-2">
                {container.gpus_per_container}
              </td>
              <td className="px-3 py-2 font-mono text-xs text-ink-2">
                {formatRuntimeSeconds(container.runtime_seconds)}
              </td>
              <td className="px-3 py-2">
                {container.public_url !== undefined && container.public_url !== '' ? (
                  <a
                    href={container.public_url}
                    target="_blank"
                    rel="noopener noreferrer"
                    title={t('admin.deployments.detail.open_url')}
                    className="text-xs text-brand underline"
                  >
                    {t('admin.deployments.detail.container_url')}
                  </a>
                ) : (
                  '—'
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function LogViewer({
  deploymentId,
  containers,
}: {
  deploymentId: string
  containers: DeployContainer[]
}) {
  const { t } = useI18n()
  const [containerId, setContainerId] = useState('')
  const [level, setLevel] = useState('')
  const [stream, setStream] = useState('')
  const [reloadToken, setReloadToken] = useState(0)
  const [logs, setLogs] = useState<string[]>([])
  const [nextToken, setNextToken] = useState('')
  const [hasMore, setHasMore] = useState(false)
  const [loading, setLoading] = useState(false)
  const [loadingMore, setLoadingMore] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // 请求序号：筛选变化/抽屉关闭后，晚到的旧响应不得覆盖新结果。
  const requestId = useRef(0)

  // 容器列表到达后默认选中第一个：日志页签不需要用户先做一次额外选择。
  useEffect(() => {
    if (containerId === '' && containers.length > 0) {
      setContainerId(containers[0]?.container_id ?? '')
    }
  }, [containerId, containers])

  const load = useCallback(
    async (token: string, append: boolean) => {
      if (containerId === '') return
      const current = ++requestId.current
      if (append) setLoadingMore(true)
      else setLoading(true)
      setError(null)
      try {
        const page = await fetchDeploymentLogs(deploymentId, {
          containerId,
          level: level === '' ? undefined : level,
          stream: stream === '' ? undefined : stream,
          pageToken: token === '' ? undefined : token,
          limit: 100,
        })
        if (current !== requestId.current) return
        setLogs((previous) => (append ? [...previous, ...page.logs] : page.logs))
        setNextToken(page.next_page_token)
        setHasMore(page.has_more)
      } catch (cause) {
        if (current === requestId.current) setError(errorMessage(cause))
      } finally {
        if (current === requestId.current) {
          setLoading(false)
          setLoadingMore(false)
        }
      }
    },
    [containerId, deploymentId, level, stream],
  )

  useEffect(() => {
    setLogs([])
    setNextToken('')
    setHasMore(false)
    if (containerId !== '') void load('', false)
  }, [load, containerId, reloadToken])

  useEffect(() => {
    return () => {
      // 卸载时让在途响应失效。
      requestId.current += 1
    }
  }, [])

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-end gap-2">
        <label className="flex flex-col gap-1 text-xs text-ink-2">
          {t('admin.deployments.detail.log_container')}
          <select
            aria-label={t('admin.deployments.detail.log_container')}
            value={containerId}
            onChange={(event) => setContainerId(event.target.value)}
            className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
          >
            {containers.map((container) => (
              <option key={container.container_id} value={container.container_id}>
                {container.container_id}
              </option>
            ))}
          </select>
        </label>
        <label className="flex flex-col gap-1 text-xs text-ink-2">
          {t('admin.deployments.detail.log_level')}
          <select
            aria-label={t('admin.deployments.detail.log_level')}
            value={level}
            onChange={(event) => setLevel(event.target.value)}
            className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
          >
            {LOG_LEVELS.map((entry) => (
              <option key={entry === '' ? 'all' : entry} value={entry}>
                {entry === '' ? t('admin.deployments.detail.log_all') : entry}
              </option>
            ))}
          </select>
        </label>
        <label className="flex flex-col gap-1 text-xs text-ink-2">
          {t('admin.deployments.detail.log_stream')}
          <select
            aria-label={t('admin.deployments.detail.log_stream')}
            value={stream}
            onChange={(event) => setStream(event.target.value)}
            className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1"
          >
            {LOG_STREAMS.map((entry) => (
              <option key={entry === '' ? 'all' : entry} value={entry}>
                {entry === '' ? t('admin.deployments.detail.log_all') : entry}
              </option>
            ))}
          </select>
        </label>
        <Button
          variant="secondary"
          className="h-9"
          onClick={() => setReloadToken((current) => current + 1)}
          disabled={loading}
        >
          {t('admin.deployments.detail.log_refresh')}
        </Button>
      </div>

      <p className="text-xs text-warning" role="note">
        {t('admin.deployments.detail.log_warning')}
      </p>

      {error !== null ? (
        <FormAlert tone="danger">{error}</FormAlert>
      ) : null}

      {loading ? (
        <p className="text-sm text-ink-2" role="status">
          {t('admin.deployments.common.loading')}
        </p>
      ) : null}

      {!loading && logs.length === 0 && error === null ? (
        <p className="rounded-xl border border-dashed border-border-default px-4 py-8 text-center text-sm text-ink-3">
          {t('admin.deployments.detail.empty_logs')}
        </p>
      ) : null}

      {logs.length > 0 ? (
        // 纯文本渲染：React 转义每一行，日志里的 HTML 不会被解释。
        // 不做自动滚动到底（§6.13）：用户用「继续加载」控制阅读位置。
        <div
          role="log"
          aria-live="off"
          data-testid="deployment-log"
          className="max-h-[420px] overflow-auto rounded-lg border border-border-default bg-surface-subtle p-3 font-mono text-xs text-ink-1"
        >
          {logs.map((line, index) => (
            <div key={index} className="whitespace-pre-wrap break-all">
              {line}
            </div>
          ))}
        </div>
      ) : null}

      {hasMore ? (
        <Button
          variant="secondary"
          disabled={loadingMore}
          onClick={() => void load(nextToken, true)}
        >
          {t('admin.deployments.detail.load_more')}
        </Button>
      ) : logs.length > 0 ? (
        <p className="text-xs text-ink-3">{t('admin.deployments.detail.no_more')}</p>
      ) : null}
    </div>
  )
}

function ContainerDetailDialog({
  deploymentId,
  containerId,
  onClose,
}: {
  deploymentId: string
  containerId: string | null
  onClose: () => void
}) {
  const { t, locale } = useI18n()
  const query = useQuery({
    queryKey: ['deployment-container', deploymentId, containerId],
    queryFn: ({ signal }) =>
      fetchDeploymentContainer(deploymentId, containerId ?? '', signal),
    enabled: containerId !== null,
  })
  const container = query.data

  return (
    <Dialog
      open={containerId !== null}
      onOpenChange={(next) => {
        if (!next) onClose()
      }}
      title={t('admin.deployments.detail.container_detail_title')}
      description={containerId ?? undefined}
      variant="sheet"
      panelClassName="max-w-[640px] lg:max-w-[600px]"
    >
      {query.isPending ? (
        <p className="text-sm text-ink-2" role="status">
          {t('admin.deployments.common.loading')}
        </p>
      ) : null}
      {query.isError ? (
        <div className="space-y-2">
          <FormAlert tone="danger">{errorMessage(query.error)}</FormAlert>
          <Button variant="secondary" onClick={() => void query.refetch()}>
            {t('admin.deployments.common.retry')}
          </Button>
        </div>
      ) : null}
      {container !== undefined ? (
        <div className="space-y-4">
          <dl className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <Field label={t('admin.deployments.detail.container_id')} value={container.container_id} />
            <Field label={t('admin.deployments.detail.container_status')} value={container.status} />
            <Field label={t('admin.deployments.detail.container_hardware')} value={container.hardware ?? '—'} />
            <Field label={t('admin.deployments.detail.container_gpu')} value={String(container.gpus_per_container)} />
            <Field label={t('admin.deployments.detail.container_runtime')} value={formatRuntimeSeconds(container.runtime_seconds)} />
            <Field label={t('admin.deployments.detail.created')} value={formatDateTime(container.created_at, locale)} />
          </dl>
          <div>
            <p className="text-xs text-ink-3">{t('admin.deployments.detail.container_url')}</p>
            {container.public_url !== undefined && container.public_url !== '' ? (
              <a
                href={container.public_url}
                target="_blank"
                rel="noopener noreferrer"
                className="break-all text-sm text-brand underline"
              >
                {container.public_url}
              </a>
            ) : (
              <p className="text-sm text-ink-2">—</p>
            )}
            <p className="text-xs text-ink-3">{t('admin.deployments.detail.open_url')}</p>
          </div>
          <div>
            <p className="text-xs text-ink-3">{t('admin.deployments.detail.container_events')}</p>
            {(container.events ?? []).length === 0 ? (
              <p className="text-sm text-ink-3">—</p>
            ) : (
              <ul className="space-y-1 text-xs text-ink-2">
                {(container.events ?? []).map((event, index) => (
                  <li key={index} className="font-mono">
                    {event.time !== undefined
                      ? `${formatDateTime(event.time, locale)} · `
                      : ''}
                    {event.message ?? ''}
                  </li>
                ))}
              </ul>
            )}
          </div>
        </div>
      ) : null}
    </Dialog>
  )
}

export interface DeploymentDetailDrawerProps {
  deploymentId: string | null
  deploymentName?: string
  canManage: boolean
  enabled: boolean | undefined
  onClose: () => void
  onAction: (target: DeploymentActionTarget) => void
}

/**
 * 详情抽屉（F-08-56）：概览 / 容器 / 日志三页签；容器详情为嵌套抽屉。
 * 全部数据实时来自 provider；秘密值只出现「已配置/不回显」口径。
 */
export function DeploymentDetailDrawer({
  deploymentId,
  deploymentName,
  canManage,
  enabled,
  onClose,
  onAction,
}: DeploymentDetailDrawerProps) {
  const { t } = useI18n()
  const online = useOnlineStatus()
  const [tab, setTab] = useState<DetailTab>('overview')
  const [containerId, setContainerId] = useState<string | null>(null)

  useEffect(() => {
    setTab('overview')
    setContainerId(null)
  }, [deploymentId])

  const detailQuery = useQuery({
    queryKey: ['deployment', deploymentId],
    queryFn: ({ signal }) => fetchDeployment(deploymentId ?? '', signal),
    enabled: deploymentId !== null,
  })

  const containersQuery = useQuery({
    queryKey: ['deployment-containers', deploymentId],
    queryFn: ({ signal }) => fetchDeploymentContainers(deploymentId ?? '', signal),
    enabled: deploymentId !== null,
    retry: false,
  })

  const detail = detailQuery.data
  const containers = containersQuery.data?.containers ?? []
  const status = detail?.status ?? ''
  const writable =
    canManage &&
    online &&
    enabled !== false &&
    status.trim().toLowerCase() !== 'destroyed' &&
    deploymentId !== null
  const trigger = (kind: DeploymentActionTarget['kind']): void => {
    if (deploymentId === null) return
    onAction({
      kind,
      id: deploymentId,
      name: deploymentName ?? deploymentId,
      status,
    })
  }

  const tabs: { id: DetailTab; key: string }[] = [
    { id: 'overview', key: 'admin.deployments.detail.tab_overview' },
    { id: 'containers', key: 'admin.deployments.detail.tab_containers' },
    { id: 'logs', key: 'admin.deployments.detail.tab_logs' },
  ]

  return (
    <>
      <Dialog
        open={deploymentId !== null}
        onOpenChange={(next) => {
          if (!next) onClose()
        }}
        title={deploymentName ?? deploymentId ?? ''}
        description={deploymentId ?? undefined}
        variant="sheet"
        panelClassName="max-w-[720px] lg:max-w-[680px]"
      >
        <div className="flex flex-col gap-4">
          <div className="flex flex-wrap items-center justify-between gap-2">
            {detail !== undefined ? <DeploymentStatusBadge status={detail.status} /> : <span />}
            {!online && canManage ? (
              <p className="text-xs text-ink-3">
                {t('admin.deployments.common.offline')}
              </p>
            ) : null}
            {writable ? (
              <div className="flex flex-wrap gap-1">
                <Button variant="secondary" className="h-7 px-2 text-xs" onClick={() => trigger('extend')}>
                  {t('admin.deployments.actions.extend')}
                </Button>
                <Button variant="ghost" className="h-7 px-2 text-xs" onClick={() => trigger('rename')}>
                  {t('admin.deployments.actions.rename')}
                </Button>
                <Button variant="ghost" className="h-7 px-2 text-xs" onClick={() => trigger('update')}>
                  {t('admin.deployments.actions.update')}
                </Button>
                <Button variant="ghost" className="h-7 px-2 text-xs text-danger" onClick={() => trigger('terminate')}>
                  {t('admin.deployments.actions.terminate')}
                </Button>
              </div>
            ) : null}
          </div>

          <div
            role="tablist"
            aria-label={t('admin.deployments.detail.tabs_label')}
            className="flex gap-1 border-b border-border-hairline"
          >
            {tabs.map((entry) => {
              const selected = entry.id === tab
              return (
                <button
                  key={entry.id}
                  type="button"
                  role="tab"
                  aria-selected={selected}
                  onClick={() => setTab(entry.id)}
                  className={[
                    'h-9 rounded-t-lg px-3 text-[13px]',
                    selected
                      ? 'border-b-2 border-brand font-medium text-ink-1'
                      : 'text-ink-2 hover:bg-surface-hover hover:text-ink-1',
                  ].join(' ')}
                >
                  {t(entry.key)}
                  {entry.id === 'containers' && containers.length > 0
                    ? ` (${containers.length})`
                    : ''}
                </button>
              )
            })}
          </div>

          {detailQuery.isPending ? (
            <div className="space-y-2" aria-hidden="true">
              {Array.from({ length: 5 }, (_, index) => (
                <div key={index} className="h-6 rounded bg-surface-subtle" />
              ))}
            </div>
          ) : null}

          {detailQuery.isError ? (
            <div className="space-y-2">
              <FormAlert tone="danger">{errorMessage(detailQuery.error)}</FormAlert>
              <Button variant="secondary" onClick={() => void detailQuery.refetch()}>
                {t('admin.deployments.common.retry')}
              </Button>
            </div>
          ) : null}

          {detail !== undefined && tab === 'overview' ? <Overview detail={detail} /> : null}
          {tab === 'containers' ? (
            containersQuery.isPending ? (
              <p className="text-sm text-ink-2" role="status">
                {t('admin.deployments.common.loading')}
              </p>
            ) : containersQuery.isError ? (
              <div className="space-y-2">
                <FormAlert tone="danger">{errorMessage(containersQuery.error)}</FormAlert>
                <Button variant="secondary" onClick={() => void containersQuery.refetch()}>
                  {t('admin.deployments.common.retry')}
                </Button>
              </div>
            ) : (
              <ContainerTable containers={containers} onOpen={setContainerId} />
            )
          ) : null}
          {tab === 'logs' ? (
            containersQuery.isPending ? (
              <p className="text-sm text-ink-2" role="status">
                {t('admin.deployments.common.loading')}
              </p>
            ) : containers.length === 0 ? (
              <p className="rounded-xl border border-dashed border-border-default px-4 py-8 text-center text-sm text-ink-3">
                {t('admin.deployments.detail.no_containers')}
              </p>
            ) : deploymentId !== null ? (
              <LogViewer deploymentId={deploymentId} containers={containers} />
            ) : null
          ) : null}
        </div>
      </Dialog>

      <ContainerDetailDialog
        deploymentId={deploymentId ?? ''}
        containerId={containerId}
        onClose={() => setContainerId(null)}
      />
    </>
  )
}
