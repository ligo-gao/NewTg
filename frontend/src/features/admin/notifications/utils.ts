/**
 * 通知页的小工具。纯函数，便于单测与抽屉/表格复用。
 */

/**
 * 收件人邮箱中间打码（UI §6.9 投递记录列）。
 * `alice@example.com` → `a***e@example.com`，短本地部分退化为 `a***@…`。
 * 非邮箱值（如 webhook 收件人）同样保留首尾、隐藏中间。
 */
export function maskRecipient(value: string): string {
  const at = value.lastIndexOf('@')
  if (at > 0) {
    const local = value.slice(0, at)
    const domain = value.slice(at)
    if (local.length <= 2) return `${local.slice(0, 1)}***${domain}`
    return `${local.slice(0, 1)}***${local.slice(-1)}${domain}`
  }
  if (value.length <= 2) return `${value.slice(0, 1)}***`
  return `${value.slice(0, 1)}***${value.slice(-1)}`
}

/** 按 Unicode 码点计数，与服务端 `utf8.RuneCountInString` 的上限口径一致。 */
export function runeLength(value: string): number {
  return [...value].length
}

export interface InsertResult {
  value: string
  caret: number
}

/** 在光标处插入 `{{.name}}`；无有效光标时追加到末尾。 */
export function insertAtCursor(
  value: string,
  caret: number | null,
  token: string,
): InsertResult {
  const position =
    caret === null || caret < 0 || caret > value.length ? value.length : caret
  return {
    value: `${value.slice(0, position)}${token}${value.slice(position)}`,
    caret: position + token.length,
  }
}

/**
 * CTA 链接的服务端口径（render.go validCTAURL）：字面 http(s)，或整串恰好
 * 是一个变量（`{{.reset_url}}`）。前端镜像它以便字段级提示。
 */
export function isValidCtaUrl(value: string): boolean {
  const trimmed = value.trim()
  if (trimmed === '') return true
  if (/^https?:\/\//i.test(trimmed)) return true
  return /^\{\{\s*\.[a-z][a-z0-9_]*\s*\}\}$/.test(trimmed)
}
