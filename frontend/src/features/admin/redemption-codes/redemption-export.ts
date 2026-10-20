/**
 * F-04-17 创建后导出（TXT / Markdown）——**纯前端本地生成**。
 *
 * 服务端只存码值哈希，导出端点也刻意不存在（`modules-04` F-04-17：
 * 「不走服务端导出接口，避免码二次传输留痕」）。因此这里只把创建响应里
 * 已拿到的一次性明文码序列化为 Blob，再交给浏览器下载。
 *
 * 抽成纯函数 + 可注入的保存函数，是为了在 jsdom 里断言文件名与正文，
 * 而不用去解析真实下载；UI 只负责按钮与禁用条件。
 */

import type { components } from '@/api/schema'

type CreateResponse = components['schemas']['RedemptionCodeCreateResponse']

export type RedemptionExportFormat = 'txt' | 'markdown'

/**
 * 单次导出行数上限（F-04-17 冻结 5000）。超过必须分批，避免一次生成
 * 几十 MB 的 Blob 卡死标签页；创建接口本身最多 100 条，这个闸门主要是
 * 给未来来源更广的导出路径留的硬边界。
 */
export const REDEMPTION_EXPORT_LIMIT = 5000

export interface RedemptionExportOptions {
  /** 是否在每行/每列带上批次名；默认开（F-04-17 选项默认值）。 */
  includeBatch?: boolean
  /** 是否带上单码面额；默认开。 */
  includeCredits?: boolean
}

export interface RedemptionExportFile {
  filename: string
  content: string
  mime: string
}

/** 保存动作的接缝；生产环境用浏览器下载，测试可注入 spy。 */
export type SaveFile = (blob: Blob, filename: string) => void

function pad2(value: number): string {
  return String(value).padStart(2, '0')
}

/** 文件名日期取用户本地时区，与 UI §7.4 的其他时间展示一致。 */
function compactDate(now: Date): string {
  return `${now.getFullYear()}${pad2(now.getMonth() + 1)}${pad2(now.getDate())}`
}

/**
 * 批次名是用户输入，直接拼进文件名会带上 `/`、`\`、`:` 等非法字符，
 * 部分浏览器会静默截断或拒绝下载。这里只做文件名层面的净化，不改动
 * 文件正文里的原始批次名。
 */
function safeFilenameSegment(value: string): string {
  const cleaned = value
    .trim()
    .replace(/[\\/:*?"<>|\s]+/g, '-')
    .replace(/^-+|-+$/g, '')
  return cleaned === '' ? 'batch' : cleaned
}

/** 表格单元格：竖线会破坏 Markdown 表格，换行会破坏 TXT 的「一行一码」。 */
function sanitizeCell(value: string): string {
  return value.replace(/\r?\n/g, ' ').replace(/\|/g, '\\|')
}

export function redemptionExportFilename(
  batchName: string | null | undefined,
  format: RedemptionExportFormat,
  now: Date = new Date(),
): string {
  const extension = format === 'markdown' ? 'md' : 'txt'
  const batch = safeFilenameSegment(batchName ?? '')
  return `redemption-codes-${batch}-${compactDate(now)}.${extension}`
}

export function buildRedemptionExportFile(
  result: CreateResponse,
  format: RedemptionExportFormat,
  options: RedemptionExportOptions = {},
  now: Date = new Date(),
): RedemptionExportFile {
  const includeBatch = options.includeBatch ?? true
  const includeCredits = options.includeCredits ?? true
  const batch = result.batch_name ?? ''

  if (format === 'markdown') {
    const header: string[] = ['Code']
    const separator: string[] = ['---']
    if (includeBatch) {
      header.push('Batch')
      separator.push('---')
    }
    if (includeCredits) {
      header.push('Credits')
      separator.push('---:')
    }

    const rows = result.codes.map((code) => {
      const cells = [code]
      if (includeBatch) cells.push(sanitizeCell(batch))
      if (includeCredits) cells.push(String(result.credits))
      return `| ${cells.join(' | ')} |`
    })

    return {
      filename: redemptionExportFilename(result.batch_name, format, now),
      mime: 'text/markdown;charset=utf-8',
      content: [`| ${header.join(' | ')} |`, `| ${separator.join(' | ')} |`, ...rows].join('\n') + '\n',
    }
  }

  const lines = result.codes.map((code) => {
    const parts = [code]
    if (includeBatch && batch.trim() !== '') parts.push(sanitizeCell(batch))
    if (includeCredits) parts.push(String(result.credits))
    return parts.join(' - ')
  })

  return {
    filename: redemptionExportFilename(result.batch_name, format, now),
    mime: 'text/plain;charset=utf-8',
    content: lines.join('\n') + '\n',
  }
}

/**
 * 触发一次浏览器下载。Object URL 延迟释放：部分浏览器在 click 同步返回
 * 后才真正读取 Blob，立即 revoke 会得到空文件。
 */
export function saveBlob(blob: Blob, filename: string): void {
  const objectUrl = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = objectUrl
  anchor.download = filename
  anchor.rel = 'noopener'
  document.body.appendChild(anchor)
  anchor.click()
  anchor.remove()
  setTimeout(() => {
    URL.revokeObjectURL(objectUrl)
  }, 0)
}

/** 返回实际使用的文件名（Toast / 测试播报使用）。 */
export function downloadRedemptionCodes(
  result: CreateResponse,
  format: RedemptionExportFormat,
  options: RedemptionExportOptions = {},
  save: SaveFile = saveBlob,
  now: Date = new Date(),
): string {
  const file = buildRedemptionExportFile(result, format, options, now)
  save(new Blob([file.content], { type: file.mime }), file.filename)
  return file.filename
}
