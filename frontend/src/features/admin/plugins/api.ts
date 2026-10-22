/**
 * 任务插件管理传输层（modules-08 §3.10 / UI §6.10，F-08-28～F-08-37）。
 *
 * 读端点与除上传/试运行外的写端点都走生成的 `apiClient`，响应形状与
 * `api/schema.d.ts` 编译期绑定。上传（JSON 源文本）与 dryrun 例外：两者的
 * 422 响应在 Problem 扩展里携带 `issues[]`（`{key, fieldPath, locale, rule,
 * message}`），而共享的 `ApiError` 归一化只保留白名单字段，会把「文件:行 +
 * 消息」丢掉——编译错误必须能被复制，所以这两天走原始 fetch 并在本模块
 * 附加 issues。
 *
 * 上传与 dryrun 显式携带 Idempotency-Key（同一用户意图的重试复用同一 key，
 * web-00 §5.6）。其余写端点（activate/status/delete/市场）的生成契约没有声明
 * 该请求头，`apiClient` 中间件会按请求随机生成一个——调用方无从复用，这是
 * 契约缺口，已在 t8 报告登记。
 */
import type { ApiError } from '@/api/errors'
import { toApiError } from '@/api/client'
import { CSRF_HEADER, getCsrfToken } from '@/lib/csrf'
import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'

export type TaskPluginRuntimeStatus =
  components['schemas']['TaskPluginRuntimeStatus']
export type TaskPlugin = components['schemas']['TaskPlugin']
export type TaskPluginPage = components['schemas']['TaskPluginPage']
export type TaskPluginVersion = components['schemas']['TaskPluginVersion']
export type TaskPluginDetail = components['schemas']['TaskPluginDetail']
export type TaskPluginSourceText = components['schemas']['TaskPluginSourceText']
export type PluginIssue = components['schemas']['PluginIssue']
export type PluginRuntimePlugin = components['schemas']['PluginRuntimePlugin']
export type PluginRuntimeInstance = components['schemas']['PluginRuntimeInstance']
export type PluginRuntimeAggregate = components['schemas']['PluginRuntimeAggregate']
export type UploadTaskPluginResponse =
  components['schemas']['UploadTaskPluginResponse']
export type PluginDryRunResult = components['schemas']['PluginDryRunResult']
export type MarketplaceHome = components['schemas']['MarketplaceHome']
export type MarketplacePlugin = components['schemas']['MarketplacePlugin']
export type MarketplaceVersion = components['schemas']['MarketplaceVersion']
export type MarketplaceSource = components['schemas']['MarketplaceSource']
export type MarketplaceSourceList = components['schemas']['MarketplaceSourceList']
export type MarketplaceInstallResponse =
  components['schemas']['MarketplaceInstallResponse']
export type UpdateMarketplaceSourcesRequest =
  components['schemas']['UpdateMarketplaceSourcesRequest']
export type PluginActivateResponse = components['schemas']['PluginActivateResponse']
export type PluginStatusResponse = components['schemas']['PluginStatusResponse']
export type PluginVersionDeleteResponse =
  components['schemas']['PluginVersionDeleteResponse']

/** 422 里的结构化校验问题（编译错误按 `message` 原样展示，可复制）。 */
export interface PluginApiError extends ApiError {
  issues?: PluginIssue[]
}

export interface ListTaskPluginsParams {
  search?: string
  source?: 'builtin' | 'upload'
  enabled?: boolean
  runtimeStatus?: TaskPluginRuntimeStatus
  page?: number
  pageSize?: number
}

export async function listTaskPlugins(
  params: ListTaskPluginsParams,
  signal?: AbortSignal,
): Promise<TaskPluginPage> {
  return requireData(
    await apiClient.GET('/api/plugin/task', {
      params: {
        query: {
          search: params.search,
          source: params.source,
          enabled: params.enabled,
          runtime_status: params.runtimeStatus,
          p: params.page,
          page_size: params.pageSize,
        },
      },
      signal,
    }),
  )
}

export async function getTaskPlugin(
  key: string,
  includeSource: boolean,
  signal?: AbortSignal,
): Promise<TaskPluginDetail> {
  return requireData(
    await apiClient.GET('/api/plugin/task/{key}', {
      params: {
        path: { key },
        query: includeSource ? { include_source: '1' } : {},
      },
      signal,
    }),
  )
}

export async function listTaskPluginVersions(
  key: string,
  signal?: AbortSignal,
): Promise<TaskPluginVersion[]> {
  const data = requireData(
    await apiClient.GET('/api/plugin/task/{key}/versions', {
      params: { path: { key } },
      signal,
    }),
  )
  return data.items ?? []
}

export async function getTaskPluginSourceText(
  key: string,
  version: string | undefined,
  signal?: AbortSignal,
): Promise<TaskPluginSourceText> {
  return requireData(
    await apiClient.GET('/api/plugin/task/{key}/source', {
      params: { path: { key }, query: version === undefined ? {} : { version } },
      signal,
    }),
  )
}

export async function getTaskPluginRuntimeStatus(
  signal?: AbortSignal,
): Promise<PluginRuntimeAggregate> {
  return requireData(
    await apiClient.GET('/api/plugin/task/runtime/status', { signal }),
  )
}

export async function activateTaskPluginVersion(
  key: string,
  version: string,
  expectUpdatedAt: string | null,
): Promise<PluginActivateResponse> {
  return requireData(
    await apiClient.POST('/api/plugin/task/{key}/activate', {
      params: { path: { key } },
      body: { version, expect_updated_at: expectUpdatedAt },
    }),
  )
}

export async function setTaskPluginStatus(
  key: string,
  enabled: boolean,
  expectUpdatedAt: string | null,
): Promise<PluginStatusResponse> {
  return requireData(
    await apiClient.POST('/api/plugin/task/{key}/status', {
      params: { path: { key } },
      body: { enabled, expect_updated_at: expectUpdatedAt },
    }),
  )
}

export async function deleteTaskPluginVersion(
  key: string,
  version: string,
  expectUpdatedAt: string | null,
): Promise<PluginVersionDeleteResponse> {
  return requireData(
    await apiClient.DELETE('/api/plugin/task/{key}/versions/{version}', {
      params: { path: { key, version } },
      body: { expect_updated_at: expectUpdatedAt },
    }),
  )
}

export async function getPluginMarketplace(
  refresh: boolean,
  signal?: AbortSignal,
): Promise<MarketplaceHome> {
  return requireData(
    await apiClient.GET('/api/plugin/task/marketplace', {
      params: { query: { refresh: refresh ? '1' : '0' } },
      signal,
    }),
  )
}

export async function listPluginMarketplaceSources(
  signal?: AbortSignal,
): Promise<MarketplaceSourceList> {
  return requireData(
    await apiClient.GET('/api/plugin/task/marketplace/sources', { signal }),
  )
}

export async function replacePluginMarketplaceSources(
  body: UpdateMarketplaceSourcesRequest,
): Promise<MarketplaceSourceList> {
  return requireData(
    await apiClient.PUT('/api/plugin/task/marketplace/sources', { body }),
  )
}

export async function checkPluginMarketplaceSource(
  id: string,
): Promise<MarketplaceSource> {
  return requireData(
    await apiClient.POST('/api/plugin/task/marketplace/sources/{id}/check', {
      params: { path: { id } },
    }),
  )
}

export async function installPluginFromMarketplace(
  body: {
    source_id: string
    key: string
    version: string
    activate: boolean
  },
): Promise<MarketplaceInstallResponse> {
  return requireData(
    await apiClient.POST('/api/plugin/task/marketplace/install', { body }),
  )
}

// ---------------------------------------------------------------------------
// 原始 fetch 路径（需要保留 Problem.issues）
// ---------------------------------------------------------------------------

function apiUrl(path: string): string {
  if (typeof location === 'undefined') return path
  return new URL(path, location.origin).toString()
}

function writeHeaders(idempotencyKey: string, json: boolean): Record<string, string> {
  const headers: Record<string, string> = {
    Accept: 'application/json',
    'Idempotency-Key': idempotencyKey,
  }
  if (json) headers['Content-Type'] = 'application/json'
  const csrfToken = getCsrfToken()
  if (csrfToken !== undefined) headers[CSRF_HEADER] = csrfToken
  return headers
}

async function readIssues(response: Response): Promise<PluginIssue[] | undefined> {
  try {
    const body: unknown = await response.clone().json()
    if (typeof body !== 'object' || body === null) return undefined
    const raw = (body as Record<string, unknown>).issues
    if (!Array.isArray(raw)) return undefined
    const issues = raw.filter(
      (item): item is PluginIssue =>
        typeof item === 'object' &&
        item !== null &&
        typeof (item as Record<string, unknown>).message === 'string',
    )
    return issues.length > 0 ? issues : undefined
  } catch {
    return undefined
  }
}

/** 归一化错误并附上 422 的结构化 issues（非 422 时没有该扩展）。 */
async function pluginError(response: Response): Promise<PluginApiError> {
  const issues = await readIssues(response)
  const error = await toApiError(response)
  return issues === undefined ? error : Object.assign(error, { issues })
}

async function postJson(
  path: string,
  body: unknown,
  idempotencyKey: string,
  signal?: AbortSignal,
): Promise<unknown> {
  const response = await fetch(apiUrl(path), {
    method: 'POST',
    credentials: 'same-origin',
    headers: writeHeaders(idempotencyKey, true),
    body: JSON.stringify(body),
    signal,
  })
  if (!response.ok) throw await pluginError(response)
  return response.json() as Promise<unknown>
}

/**
 * 上传插件源（JSON `{source_text}`）。
 *
 * 服务端先编译校验再入库：失败 422 `plugin_compile_failed` 且零写入，因此
 * 这个调用就是「校验 + 安装」的同一步——界面不提供绕过编译的入口（S1）。
 */
export async function uploadTaskPlugin(
  sourceText: string,
  idempotencyKey: string,
): Promise<UploadTaskPluginResponse> {
  const data = (await postJson(
    '/api/plugin/task',
    { source_text: sourceText },
    idempotencyKey,
  )) as UploadTaskPluginResponse
  return data
}

export interface DryRunInput {
  key: string
  version: string | undefined
  endpoint: string
  payload: unknown
  allowOutbound: boolean
}

/** 沙箱试运行（零副作用；`allow_outbound` 当前必定 422，界面不会发送 true）。 */
export async function dryRunTaskPlugin(
  input: DryRunInput,
  idempotencyKey: string,
  signal?: AbortSignal,
): Promise<PluginDryRunResult> {
  const data = (await postJson(
    `/api/plugin/task/${encodeURIComponent(input.key)}/dryrun`,
    {
      version: input.version,
      endpoint: input.endpoint,
      payload: input.payload,
      allow_outbound: input.allowOutbound,
    },
    idempotencyKey,
    signal,
  )) as PluginDryRunResult
  return data
}

/** 每次用户提交生成一次；重试同一意图时由页面层复用。 */
export function createIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`
}
