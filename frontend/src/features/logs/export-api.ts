import { toApiError } from '@/api/client'
import { parseProblem } from '@/api/errors'
import type { operations } from '@/api/schema'
import { downloadBlob } from '@/shared/lib/download'

import type { UsageLogSearch } from './search'

/**
 * F-05-07 日志导出（modules-05 §7 / UI §7.4）。
 *
 * 生成端是服务端流式接口，OpenAPI 冻结稿尚未收录该路径（同
 * `features/public/api.ts` 的处置），因此这里用受控的原始 fetch，而不是
 * `apiClient.GET`——后者只接受 `schema.d.ts` 里已存在的路径。所有失败仍经
 * `toApiError`/`parseProblem` 归一成同一套 `ApiError` 词汇，页面不会出现
 * 第二套错误模型。
 *
 * 文件内容绝不进入 React Query 缓存：响应直接读成 Blob 并立即触发一次性
 * 下载，然后 revoke object URL（隐私面 + 大文件内存）。
 */

export type ExportFormat = 'txt' | 'markdown'
export type ExportScope = 'filtered' | 'all'

/** 与列表参数同源，避免导出与屏幕上的筛选逐渐漂移。 */
export type ExportQuery = NonNullable<
  operations['listUsageLogs']['parameters']['query']
>

export interface ExportRequest {
  search: UsageLogSearch
  scope: ExportScope
  format: ExportFormat
  /** 仅在调用方已持有 `log.prompt.view` 时才应为 true；服务端仍会重校验。 */
  revealSensitive: boolean
  /** 页面权限门：非管理员绝不发送 username 管理参数。 */
  canFilterByUsername: boolean
  signal?: AbortSignal
}

/** 422 `EXPORT_ROW_LIMIT`：必须在写出任何字节前拒绝（不存在部分文件）。 */
export class ExportRowLimitError extends Error {
  constructor() {
    super('EXPORT_ROW_LIMIT')
    this.name = 'ExportRowLimitError'
  }
}

/** 204：范围内 0 行。服务端不生成空文件，前端按空态提示而不是下载。 */
export class ExportEmptyError extends Error {
  constructor() {
    super('EXPORT_EMPTY')
    this.name = 'ExportEmptyError'
  }
}

function asIso(value: string | undefined): string | undefined {
  if (value === undefined || value === '') return undefined
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp)) return undefined
  return new Date(timestamp).toISOString()
}

/**
 * 把屏上筛选序列化成导出查询。分页参数刻意不发送：导出覆盖整个范围，
 * 不是当前页。
 */
export function buildExportQuery(
  request: Pick<ExportRequest, 'search' | 'scope' | 'format' | 'revealSensitive' | 'canFilterByUsername'>,
): ExportQuery {
  const { search } = request
  const query: ExportQuery = {}

  if (search.range === 'custom') {
    const start = asIso(search.start)
    const end = asIso(search.end)
    if (start !== undefined && end !== undefined) {
      query.start = start
      query.end = end
    }
  } else {
    query.range = search.range
  }

  if (search.model !== undefined) query.model = search.model
  if (search.group !== undefined) query.group = search.group
  if (search.token_id !== undefined) query.token_id = search.token_id
  if (search.request_id !== undefined) query.request_id = search.request_id
  if (search.status !== undefined) query.status = search.status
  if (request.canFilterByUsername && search.username !== undefined) {
    query.username = search.username
  }

  return query
}

function toSearchParams(
  request: Pick<ExportRequest, 'search' | 'scope' | 'format' | 'revealSensitive' | 'canFilterByUsername'>,
): URLSearchParams {
  const params = new URLSearchParams()
  const query = buildExportQuery(request)
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined) continue
    params.set(key, String(value))
  }
  params.set('scope', request.scope)
  params.set('format', request.format)
  // 缺省即打码；显式发送 0/1 让「本次导出口径」在请求里可审计。
  params.set('reveal_sensitive', request.revealSensitive ? '1' : '0')
  return params
}

function apiUrl(path: string): string {
  const base =
    typeof location !== 'undefined' ? location.origin : 'http://localhost'
  return new URL(path, base).toString()
}

/**
 * 从 `Content-Disposition` 读取服务端文件名。支持 RFC 5987 的
 * `filename*=UTF-8''…` 与普通 `filename="…"`；路径分隔符/控制字符一律
 * 丢弃，防止响应头把下载写到意料之外的目录名。
 */
export function filenameFromContentDisposition(
  header: string | null,
): string | undefined {
  if (header === null || header === '') return undefined

  const extended = /filename\*\s*=\s*UTF-8''([^;]+)/i.exec(header)
  if (extended?.[1] !== undefined) {
    try {
      return sanitizeFilename(decodeURIComponent(extended[1].trim()))
    } catch {
      // 非法的百分号编码回退到普通 filename。
    }
  }

  const plain = /filename\s*=\s*"?([^";]+)"?/i.exec(header)
  if (plain?.[1] === undefined) return undefined
  return sanitizeFilename(plain[1].trim())
}

function sanitizeFilename(value: string): string | undefined {
  // 逐字符过滤而不是用带控制字符范围的 regex（后者会触发
  // no-control-regex，且意图更难读）：去掉路径分隔符、保留字符与 C0 控制码。
  const cleaned = Array.from(value)
    .filter((character) => {
      const code = character.charCodeAt(0)
      if (code < 0x20 || code === 0x7f) return false
      return !'\\/:*?"<>|'.includes(character)
    })
    .join('')
    .trim()
  return cleaned === '' ? undefined : cleaned
}

function fallbackFilename(scope: ExportScope, format: ExportFormat): string {
  // 服务端总是给出附件名；这里只兜底缺失响应头的情况。时间取用户本地时区
  // （UI §7.4），与后端 UTC 文件名在语义上一致。
  const now = new Date()
  const pad = (value: number): string => String(value).padStart(2, '0')
  const stamp = `${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}-${pad(now.getHours())}${pad(now.getMinutes())}`
  return `usage-logs-${scope}-${stamp}.${format === 'markdown' ? 'md' : 'txt'}`
}

function isAbortError(error: unknown): boolean {
  return (
    error instanceof DOMException && error.name === 'AbortError'
  ) || (error instanceof Error && error.name === 'AbortError')
}

/**
 * 请求导出并触发下载。返回实际使用的文件名（测试/状态播报使用）。
 *
 * 状态映射（UI §7.5）：204 → `ExportEmptyError`；422 且带
 * `EXPORT_ROW_LIMIT` → `ExportRowLimitError`；其余非 2xx → `ApiError`。
 */
export async function downloadUsageLogs(
  request: ExportRequest,
): Promise<{ filename: string }> {
  const params = toSearchParams(request)

  let response: Response
  try {
    response = await fetch(
      apiUrl(`/api/usage-logs/export?${params.toString()}`),
      {
        method: 'GET',
        credentials: 'same-origin',
        headers: { Accept: 'text/plain, text/markdown, application/problem+json' },
        signal: request.signal,
      },
    )
  } catch (error) {
    if (isAbortError(error)) throw error
    // 传输失败与契约客户端的 `offline` 同一口径。
    throw parseProblem({ status: 0 })
  }

  if (response.status === 204) {
    // 204 无正文，不能走 toApiError（会读出一个空 problem）。
    throw new ExportEmptyError()
  }

  if (response.status === 422) {
    let problem: unknown
    try {
      problem = await response.clone().json()
    } catch {
      problem = undefined
    }
    if (problem !== null && typeof problem === 'object') {
      const record = problem as Record<string, unknown>
      const detail = record.detail
      if (
        record.type === 'https://newtg.dev/problems/export_row_limit' ||
        (typeof detail === 'string' && detail.includes('EXPORT_ROW_LIMIT'))
      ) {
        throw new ExportRowLimitError()
      }
    }
  }

  if (!response.ok) {
    throw await toApiError(response)
  }

  const filename =
    filenameFromContentDisposition(
      response.headers.get('Content-Disposition'),
    ) ?? fallbackFilename(request.scope, request.format)

  const blob = await response.blob()
  downloadBlob(blob, filename)
  return { filename }
}
