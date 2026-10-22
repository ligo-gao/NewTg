/**
 * W17-2 系统任务 API（billing-06 §13.5；modules-08 F-08-21 系统任务卡）。
 *
 * 端点只有列表/详情/触发三个（契约没有 cancel）。触发必须带
 * `Idempotency-Key`：同一把管理员意图在重试时复用同一个 key，服务端 24h
 * 内重放首次 202，不产生第二条运行记录。
 */
import {
  CSRF_HEADER,
  getCsrfToken,
  isCsrfFailure,
  refreshCsrfCookie,
  toApiError,
} from '@/api/client'
import { parseProblem } from '@/api/errors'

export interface SystemTaskSummary {
  task_type: string
  description?: string
  enabled: boolean
  interval_seconds: number
  last_run_at: string | null
  last_status: 'succeeded' | 'failed' | 'skipped' | null
  next_due_at: string | null
  running: boolean
  holder_id?: string
  last_duration_ms: number | null
  manual_trigger_allowed: boolean
  concurrency?: string
}

export interface SystemTaskRun {
  id: number
  task_type: string
  status: 'pending' | 'running' | 'succeeded' | 'failed'
  node_id?: string
  started_at: string | null
  finished_at: string | null
  error?: string
  // 只包含计数/枚举的汇总；渲染时按 JSON 展示但不做深链路解析。
  result?: Record<string, unknown> | null
  trigger: 'schedule' | 'manual' | 'wakeup'
}

export interface SystemTaskPage {
  items: SystemTaskSummary[]
  total: number
  page: number
  page_size: number
}

export interface SystemTaskRunPage {
  items: SystemTaskRun[]
  total: number
  page: number
  page_size: number
}

export interface SystemTaskDetail {
  summary: SystemTaskSummary
  runs: SystemTaskRunPage
}

export interface SystemTaskTriggerResult {
  run_id: number
  task_type: string
  trigger: 'manual'
}

function apiUrl(path: string): string {
  if (typeof location === 'undefined') return path
  return new URL(path, location.origin).toString()
}

async function dispatch(path: string, init: RequestInit): Promise<Response> {
  try {
    return await fetch(apiUrl(path), init)
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error
    // 传输失败统一映射成 offline（web-00 §5.2），与其它管理面一致。
    throw parseProblem({ status: 0 })
  }
}

async function readJSON<T>(response: Response): Promise<T> {
  if (!response.ok) throw await toApiError(response)
  return (await response.json()) as T
}

export function listSystemTasks(signal?: AbortSignal): Promise<SystemTaskPage> {
  return dispatch('/api/system/tasks?p=1&page_size=100', {
    method: 'GET',
    credentials: 'same-origin',
    headers: { Accept: 'application/json' },
    signal,
  }).then((response) => readJSON<SystemTaskPage>(response))
}

export function getSystemTask(name: string, signal?: AbortSignal): Promise<SystemTaskDetail> {
  const encoded = encodeURIComponent(name)
  return dispatch(`/api/system/tasks/${encoded}?p=1&page_size=20`, {
    method: 'GET',
    credentials: 'same-origin',
    headers: { Accept: 'application/json' },
    signal,
  }).then((response) => readJSON<SystemTaskDetail>(response))
}

/** 为一次管理员意图生成幂等键；重试时必须复用同一个值。 */
export function createTaskTriggerKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`
}

export async function triggerSystemTask(
  name: string,
  idempotencyKey: string,
): Promise<SystemTaskTriggerResult> {
  const headers = new Headers({
    Accept: 'application/json',
    'Idempotency-Key': idempotencyKey,
  })
  const csrfToken = getCsrfToken()
  if (csrfToken !== undefined) headers.set(CSRF_HEADER, csrfToken)

  const init: RequestInit = {
    method: 'POST',
    headers,
    credentials: 'same-origin',
  }
  let response = await dispatch(`/api/system/tasks/${encodeURIComponent(name)}:trigger`, init)
  // 与契约客户端一致：CSRF 过期时刷新一次 cookie 并原样重放（同一个幂等键，
  // 服务端不会因为重放产生第二条运行）。
  if (response.ok === false && (await isCsrfFailure(response))) {
    await refreshCsrfCookie()
    headers.delete(CSRF_HEADER)
    const refreshed = getCsrfToken()
    if (refreshed !== undefined) headers.set(CSRF_HEADER, refreshed)
    response = await dispatch(`/api/system/tasks/${encodeURIComponent(name)}:trigger`, init)
  }
  return readJSON<SystemTaskTriggerResult>(response)
}
