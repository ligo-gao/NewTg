/**
 * 通知模板 / 投递记录 transport。
 *
 * 这些端点**尚未进入 OpenAPI 契约批次**（`api/schema.d.ts` 里没有
 * `/api/notification-templates`），因此本模块按后端真实处理器
 * （backend/internal/httpapi/handlers_notification_templates.go）的冻结路径
 * 直接走 fetch，并复用与契约客户端相同的错误语义：
 * - `toApiError` 把非 2xx 归一成 `ApiError`（含 kind / retry_after）；
 * - 写请求带 CSRF token，并镜像一次 `GET /api/me` 的 CSRF 恢复重放。
 * 待契约批次把端点纳入生成客户端后，本文件整体替换为 `apiClient` 调用
 * （登记在 m6c-notify-ui-report.md 的待办）。
 *
 * 真实参数名与 brief 的差异：列表筛选用 `modified=true`（不是
 * `modified_only`）；投递记录分页用 `p`（不是 `page`）。
 */
import {
  CSRF_HEADER,
  WRITE_METHODS,
  getCsrfToken,
  isCsrfFailure,
  refreshCsrfCookie,
  toApiError,
} from '@/api/client'
import { parseProblem } from '@/api/errors'

import type {
  DeliveryListParams,
  DeliveryListResponse,
  NotificationTemplate,
  NotificationTemplateChannel,
  NotificationTemplateListResponse,
  NotificationLocale,
  PreviewTemplateInput,
  PreviewTemplateResponse,
  TemplateListParams,
  TestSendInput,
  TestSendResponse,
  UpdateTemplateInput,
} from './types'

/** Node 测试环境要求绝对 URL；浏览器中与同源相对路径等价。 */
function apiUrl(path: string): string {
  if (typeof location === 'undefined') return path
  return new URL(path, location.origin).toString()
}

interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT'
  body?: unknown
  signal?: AbortSignal
}

async function dispatch(path: string, init: RequestInit): Promise<Response> {
  try {
    return await fetch(apiUrl(path), init)
  } catch (error) {
    // 调用方主动取消不是传输失败，必须原样抛出让 React Query 丢弃结果。
    if (error instanceof DOMException && error.name === 'AbortError') throw error
    throw parseProblem({ status: 0 })
  }
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const method = options.method ?? 'GET'
  const headers = new Headers({ Accept: 'application/json' })

  if (WRITE_METHODS.has(method)) {
    const csrfToken = getCsrfToken()
    if (csrfToken !== undefined) headers.set(CSRF_HEADER, csrfToken)
    // 刻意不发 Idempotency-Key：PUT 由 expect_updated_at 提供幂等性，
    // 测试发送按 §3.9.6d 明确豁免去重（重复点击应当真的再发一封）。
  }

  let body: string | undefined
  if (options.body !== undefined) {
    headers.set('Content-Type', 'application/json')
    body = JSON.stringify(options.body)
  }

  const init: RequestInit = {
    method,
    headers,
    body,
    credentials: 'same-origin',
    signal: options.signal,
  }

  let response = await dispatch(path, init)

  // 与契约客户端一致的 CSRF 恢复：刷新会话 cookie 后重放一次写请求。
  if (!response.ok && WRITE_METHODS.has(method) && (await isCsrfFailure(response))) {
    await refreshCsrfCookie()
    headers.delete(CSRF_HEADER)
    const refreshedToken = getCsrfToken()
    if (refreshedToken !== undefined) headers.set(CSRF_HEADER, refreshedToken)
    response = await dispatch(path, init)
  }

  if (!response.ok) throw await toApiError(response)
  return (await response.json()) as T
}

function templatePath(key: string): string {
  return `/api/notification-templates/${encodeURIComponent(key)}`
}

export function listTemplates(
  params: TemplateListParams = {},
  signal?: AbortSignal,
): Promise<NotificationTemplateListResponse> {
  const query = new URLSearchParams()
  if (params.category !== undefined) query.set('category', params.category)
  if (params.channel !== undefined) query.set('channel', params.channel)
  if (params.locale !== undefined) query.set('locale', params.locale)
  // 服务端只认 `modified=true|1`。
  if (params.modifiedOnly === true) query.set('modified', 'true')
  const suffix = query.size > 0 ? `?${query.toString()}` : ''
  return request<NotificationTemplateListResponse>(`/api/notification-templates${suffix}`, { signal })
}

/** 单行详情。locale 省略时服务端按 en 处理并走回退链。 */
export function getTemplate(
  key: string,
  locale: NotificationLocale,
  channel: NotificationTemplateChannel,
  signal?: AbortSignal,
): Promise<NotificationTemplate> {
  const query = new URLSearchParams({ locale, channel })
  return request<NotificationTemplate>(`${templatePath(key)}?${query.toString()}`, {
    signal,
  })
}

export function updateTemplate(
  key: string,
  input: UpdateTemplateInput,
  signal?: AbortSignal,
): Promise<NotificationTemplate> {
  return request<NotificationTemplate>(templatePath(key), {
    method: 'PUT',
    body: input,
    signal,
  })
}

export function previewTemplate(
  key: string,
  input: PreviewTemplateInput,
  signal?: AbortSignal,
): Promise<PreviewTemplateResponse> {
  return request<PreviewTemplateResponse>(`${templatePath(key)}:preview`, {
    method: 'POST',
    body: input,
    signal,
  })
}

export function testTemplate(
  key: string,
  input: TestSendInput,
  signal?: AbortSignal,
): Promise<TestSendResponse> {
  return request<TestSendResponse>(`${templatePath(key)}:test`, {
    method: 'POST',
    body: input,
    signal,
  })
}

export function listDeliveries(
  params: DeliveryListParams,
  signal?: AbortSignal,
): Promise<DeliveryListResponse> {
  const query = new URLSearchParams({
    p: String(params.page),
    page_size: String(params.pageSize),
  })
  if (params.category !== undefined) query.set('category', params.category)
  if (params.channel !== undefined) query.set('channel', params.channel)
  if (params.status !== undefined) query.set('status', params.status)
  return request<DeliveryListResponse>(`/api/notification-deliveries?${query.toString()}`, {
    signal,
  })
}

export type TemplateField = 'subject' | 'body' | 'cta_text' | 'cta_url' | 'enabled' | 'variables'

/**
 * 422 的字段归属。
 *
 * 后端 Problem 只带 `type` + `detail`（没有 `fields`/`errors` 扩展），所以这里
 * 按**封闭的 problem type** 与 detail 前缀做客户端映射；未命中的错误保持
 * 表单级提示，绝不猜测字段。
 */
export function fieldForProblem(error: { type?: string; detail?: string }): TemplateField | null {
  const type = error.type ?? ''
  if (type.endsWith('locked_template')) return 'enabled'
  if (type.endsWith('missing_variable') || type.endsWith('invalid_variable')) {
    return 'variables'
  }
  if (type.endsWith('invalid_template')) {
    const detail = error.detail ?? ''
    if (detail.includes('subject')) return 'subject'
    if (detail.includes('body')) return 'body'
    if (detail.includes('CTA')) return 'cta_text'
  }
  return null
}
