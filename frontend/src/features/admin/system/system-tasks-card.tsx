import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Fragment, useState } from 'react'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'
import { StatusBadge, type StatusBadgeVariant } from '@/shared/ui/status-badge'
import { pushErrorToast, useToastStore } from '@/shared/ui/toast'

import {
  createTaskTriggerKey,
  getSystemTask,
  listSystemTasks,
  triggerSystemTask,
  type SystemTaskRun,
  type SystemTaskSummary,
} from './system-tasks-api'

/**
 * F-08-21 系统任务卡（W17-2）。
 *
 * 只读列表 + 行展开近 20 条运行历史 + 白名单任务的手动触发（二次确认）。
 * 10s 轮询，页面 hidden 时暂停（与系统状态页同一节奏）。
 *
 * 权限：端点要求 settings:manage；403 时整卡隐藏（与实例卡同一处理），
 * 不在前端伪造权限判断。触发按钮只对 `manual_trigger_allowed` 渲染，
 * 但真正的授权仍在服务端（非白名单 403）。
 */

const TASK_POLL_INTERVAL = 10_000

function formatTimestamp(value: string | null | undefined): string {
  if (value === null || value === undefined || value === '') return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '—'
  return date.toLocaleString()
}

function formatDuration(ms: number | null | undefined): string {
  if (ms === null || ms === undefined || !Number.isFinite(ms) || ms < 0) return '—'
  if (ms < 1000) return `${Math.round(ms)}ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`
  const minutes = Math.floor(ms / 60_000)
  const seconds = Math.round((ms % 60_000) / 1000)
  return `${minutes}m ${seconds}s`
}

function statusVariant(status: string | null | undefined): StatusBadgeVariant {
  switch (status) {
    case 'succeeded':
      return 'success'
    case 'failed':
      return 'danger'
    case 'running':
      return 'info'
    case 'pending':
      return 'warning'
    case 'skipped':
      return 'neutral'
    default:
      return 'neutral'
  }
}

function intervalLabel(
  seconds: number,
  t: (key: string, values?: Record<string, string | number>) => string,
): string {
  if (seconds <= 0) return t('settings.status.tasks.interval_manual')
  if (seconds < 60) return t('settings.status.tasks.interval_seconds', { seconds })
  if (seconds < 3600) return t('settings.status.tasks.interval_minutes', { minutes: Math.round(seconds / 60) })
  return t('settings.status.tasks.interval_hours', { hours: Math.round(seconds / 3600) })
}

function runStatusLabel(
  status: string,
  t: (key: string) => string,
): string {
  return t(`settings.status.tasks.run_status_${status}`)
}

function TaskRuns({ taskName }: { taskName: string }) {
  const { t } = useI18n()
  const query = useQuery({
    queryKey: ['system-task', taskName],
    queryFn: ({ signal }) => getSystemTask(taskName, signal),
    retry: false,
  })

  if (query.isPending) {
    return (
      <p className="px-4 py-3 text-sm text-ink-2" role="status">
        {t('settings.status.tasks.runs_loading')}
      </p>
    )
  }
  if (query.isError) {
    return (
      <p className="px-4 py-3 text-sm text-danger" role="alert">
        {t('settings.status.tasks.runs_error')}
      </p>
    )
  }
  const runs = query.data?.runs.items ?? []
  if (runs.length === 0) {
    return <p className="px-4 py-3 text-sm text-ink-3">{t('settings.status.tasks.runs_empty')}</p>
  }
  return (
    <div className="px-4 py-3">
      <h4 className="mb-2 text-xs font-medium text-ink-3">{t('settings.status.tasks.runs_title')}</h4>
      <div className="overflow-x-auto">
        <table className="w-full min-w-[560px] text-left text-sm">
          <thead className="text-xs text-ink-3">
            <tr>
              <th scope="col" className="py-1 pr-3 font-medium">{t('settings.status.tasks.run_started')}</th>
              <th scope="col" className="py-1 pr-3 font-medium">{t('settings.status.tasks.run_status')}</th>
              <th scope="col" className="py-1 pr-3 font-medium">{t('settings.status.tasks.run_trigger')}</th>
              <th scope="col" className="py-1 pr-3 font-medium">{t('settings.status.tasks.run_duration')}</th>
              <th scope="col" className="py-1 font-medium">{t('settings.status.tasks.run_error')}</th>
            </tr>
          </thead>
          <tbody>
            {runs.map((run: SystemTaskRun) => (
              <tr key={run.id} className="border-t border-border-hairline">
                <td className="py-1 pr-3 font-mono text-ink-2">{formatTimestamp(run.started_at)}</td>
                <td className="py-1 pr-3">
                  <StatusBadge variant={statusVariant(run.status)}>{runStatusLabel(run.status, t)}</StatusBadge>
                </td>
                <td className="py-1 pr-3 text-ink-2">{t(`settings.status.tasks.trigger_${run.trigger}`)}</td>
                <td className="py-1 pr-3 font-mono text-ink-2">
                  {run.started_at !== null && run.finished_at !== null
                    ? formatDuration(new Date(run.finished_at).getTime() - new Date(run.started_at).getTime())
                    : '—'}
                </td>
                <td className="py-1 font-mono text-xs text-danger">{run.error ?? '—'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

export function SystemTasksCard() {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)
  const [expanded, setExpanded] = useState<string | null>(null)
  const [pending, setPending] = useState<SystemTaskSummary | null>(null)
  // 一次确认只生成一个幂等键；重试（网络抖动）复用同一个值。
  const [triggerKey, setTriggerKey] = useState('')

  const query = useQuery({
    queryKey: ['system-tasks'],
    queryFn: ({ signal }) => listSystemTasks(signal),
    retry: false,
    // F-08-21：10s 轮询，hidden 暂停。
    refetchInterval: () => (typeof document !== 'undefined' && document.hidden ? false : TASK_POLL_INTERVAL),
  })

  const mutation = useMutation({
    mutationFn: ({ name, key }: { name: string; key: string }) => triggerSystemTask(name, key),
    onSuccess: async (result) => {
      setPending(null)
      push({ tone: 'success', message: t('settings.status.tasks.trigger_success', { run_id: result.run_id }) })
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['system-tasks'] }),
        queryClient.invalidateQueries({ queryKey: ['system-task', result.task_type] }),
      ])
    },
    onError: (error: unknown) => {
      setPending(null)
      if (error instanceof ApiError) {
        if (error.status === 409) {
          push({ tone: 'error', message: t('settings.status.tasks.trigger_already_running') })
          return
        }
        if (error.status === 403) {
          push({ tone: 'error', message: t('settings.status.tasks.trigger_forbidden') })
          return
        }
      }
      pushErrorToast(error as ApiError)
    },
  })

  // 无权限：整卡隐藏（服务端 403 是唯一真源）。
  if (query.error instanceof ApiError && query.error.status === 403) {
    return null
  }

  const tasks = query.data?.items ?? []

  return (
    <section
      data-testid="system-tasks-card"
      className="rounded-xl border border-border-default bg-surface-card p-5"
    >
      <div className="mb-3 flex items-center justify-between">
        <div>
          <h2 className="text-[15px] font-semibold leading-[22px] text-ink-1">
            {t('settings.status.tasks.title')}
          </h2>
          <p className="mt-1 text-xs text-ink-3">{t('settings.status.tasks.subtitle')}</p>
        </div>
        <Button variant="secondary" onClick={() => void query.refetch()} disabled={query.isFetching}>
          {t('settings.status.tasks.refresh')}
        </Button>
      </div>

      {query.isPending ? (
        <p className="text-sm text-ink-2" role="status">{t('settings.status.tasks.loading')}</p>
      ) : null}

      {query.isError ? (
        <div className="space-y-2">
          <FormAlert tone="danger">{t('settings.status.tasks.load_error')}</FormAlert>
          <Button variant="secondary" onClick={() => void query.refetch()}>
            {t('settings.status.tasks.retry')}
          </Button>
        </div>
      ) : null}

      {query.isSuccess ? (
        tasks.length === 0 ? (
          <p className="text-sm text-ink-3">{t('settings.status.tasks.empty')}</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[920px] text-left text-sm">
              <thead className="text-xs text-ink-3">
                <tr>
                  <th scope="col" className="py-2 pr-4 font-medium">{t('settings.status.tasks.task')}</th>
                  <th scope="col" className="py-2 pr-4 font-medium">{t('settings.status.tasks.enabled')}</th>
                  <th scope="col" className="py-2 pr-4 font-medium">{t('settings.status.tasks.interval')}</th>
                  <th scope="col" className="py-2 pr-4 font-medium">{t('settings.status.tasks.last_run')}</th>
                  <th scope="col" className="py-2 pr-4 font-medium">{t('settings.status.tasks.last_status')}</th>
                  <th scope="col" className="py-2 pr-4 font-medium">{t('settings.status.tasks.next_due')}</th>
                  <th scope="col" className="py-2 pr-4 font-medium">{t('settings.status.tasks.holder')}</th>
                  <th scope="col" className="py-2 pr-4 font-medium">{t('settings.status.tasks.duration')}</th>
                  <th scope="col" className="py-2 font-medium">{t('settings.status.tasks.actions')}</th>
                </tr>
              </thead>
              <tbody>
                {tasks.map((task) => {
                  const isExpanded = expanded === task.task_type
                  return (
                    <Fragment key={task.task_type}>
                      <tr
                        key={task.task_type}
                        data-testid={`system-task-row-${task.task_type}`}
                        className="border-t border-border-hairline"
                      >
                        <td className="py-2 pr-4">
                          <button
                            type="button"
                            className="text-left font-mono text-ink-1 hover:underline"
                            onClick={() => setExpanded(isExpanded ? null : task.task_type)}
                            aria-expanded={isExpanded}
                          >
                            {task.task_type}
                          </button>
                          {task.description !== undefined && task.description !== '' ? (
                            <p className="text-xs text-ink-3">{task.description}</p>
                          ) : null}
                        </td>
                        <td className="py-2 pr-4">
                          <StatusBadge variant={task.enabled ? 'success' : 'neutral'}>
                            {task.enabled ? t('settings.status.tasks.on') : t('settings.status.tasks.off')}
                          </StatusBadge>
                        </td>
                        <td className="py-2 pr-4 text-ink-2">{intervalLabel(task.interval_seconds, t)}</td>
                        <td className="py-2 pr-4 font-mono text-ink-2">{formatTimestamp(task.last_run_at)}</td>
                        <td className="py-2 pr-4">
                          {task.running ? (
                            <StatusBadge variant="info">{t('settings.status.tasks.run_status_running')}</StatusBadge>
                          ) : task.last_status !== null ? (
                            <StatusBadge variant={statusVariant(task.last_status)}>
                              {runStatusLabel(task.last_status, t)}
                            </StatusBadge>
                          ) : (
                            <span className="text-ink-3">—</span>
                          )}
                        </td>
                        <td className="py-2 pr-4 font-mono text-ink-2">{formatTimestamp(task.next_due_at)}</td>
                        <td className="py-2 pr-4 font-mono text-xs text-ink-2">{task.holder_id ?? '—'}</td>
                        <td className="py-2 pr-4 font-mono text-ink-2">{formatDuration(task.last_duration_ms)}</td>
                        <td className="py-2">
                          <div className="flex items-center gap-2">
                            <Button
                              variant="ghost"
                              onClick={() => setExpanded(isExpanded ? null : task.task_type)}
                            >
                              {isExpanded
                                ? t('settings.status.tasks.hide_details')
                                : t('settings.status.tasks.details')}
                            </Button>
                            {task.manual_trigger_allowed ? (
                              <Button
                                variant="secondary"
                                onClick={() => {
                                  setPending(task)
                                  setTriggerKey(createTaskTriggerKey())
                                }}
                              >
                                {t('settings.status.tasks.trigger')}
                              </Button>
                            ) : null}
                          </div>
                        </td>
                      </tr>
                      {isExpanded ? (
                        <tr key={`${task.task_type}-runs`} className="border-t border-border-hairline bg-surface-subtle/40">
                          <td colSpan={9} className="p-0">
                            <TaskRuns taskName={task.task_type} />
                          </td>
                        </tr>
                      ) : null}

                    </Fragment>
                  )
                })}
              </tbody>
            </table>
          </div>
        )
      ) : null}

      {pending !== null ? (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4"
          role="dialog"
          aria-modal="true"
          data-testid="system-task-trigger-dialog"
        >
          <div className="w-full max-w-md rounded-xl border border-border-default bg-surface-card p-5">
            <h3 className="text-base font-semibold text-ink-1">
              {t('settings.status.tasks.trigger_confirm_title')}
            </h3>
            <p className="mt-2 text-sm text-ink-2">
              {t('settings.status.tasks.trigger_confirm_body', { task: pending.task_type })}
            </p>
            <div className="mt-4 flex justify-end gap-2">
              <Button variant="ghost" onClick={() => setPending(null)} disabled={mutation.isPending}>
                {t('settings.status.tasks.cancel')}
              </Button>
              <Button
                variant="primary"
                disabled={mutation.isPending}
                onClick={() => mutation.mutate({ name: pending.task_type, key: triggerKey })}
              >
                {t('settings.status.tasks.confirm')}
              </Button>
            </div>
          </div>
        </div>
      ) : null}
    </section>
  )
}
