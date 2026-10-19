export type PlaygroundRole = 'user' | 'assistant'

export interface PlaygroundUsage {
  promptTokens: number
  completionTokens: number
  /** 来自 `usage_logs.cost`；查询失败时 null（界面显示待定，不估算）。 */
  credits: number | null
}

export interface PlaygroundMessage {
  id: string
  role: PlaygroundRole
  content: string
  /** 该条回复使用的模型；历史消息的模型标签不因切换选择而改变。 */
  model?: string
  /** 用户中途停止时保留已生成内容并标记。 */
  stopped?: boolean
  /** 流被截断/传输中断（未收到 `[DONE]`）：保留内容但不得当成成功回复。 */
  interrupted?: boolean
  usage?: PlaygroundUsage
}

const KEY_PREFIX = 'newtg.playground.v1'

/**
 * 会话存储键：`用户 + 模型` 双重隔离。
 *
 * 为什么按模型再分一层：Playground 允许比较不同模型的回答，若所有模型共用
 * 一条会话，切换模型后历史消息会以“当前模型”身份继续发送给上游，模型的
 * 归属就不再可追溯。按用户隔离则保证多账号共用浏览器时不串话。
 * 只写 sessionStorage（关闭标签页即丢弃），不落库、不写 localStorage。
 */
export function playgroundSessionKey(username: string, model: string): string {
  return `${KEY_PREFIX}.${username}.${model}`
}

function isMessage(value: unknown): value is PlaygroundMessage {
  if (typeof value !== 'object' || value === null) return false
  const record = value as Record<string, unknown>
  return (
    typeof record.id === 'string' &&
    (record.role === 'user' || record.role === 'assistant') &&
    typeof record.content === 'string'
  )
}

export function loadPlaygroundMessages(key: string): PlaygroundMessage[] {
  if (typeof sessionStorage === 'undefined') return []
  try {
    const raw = sessionStorage.getItem(key)
    if (raw === null) return []
    const parsed: unknown = JSON.parse(raw)
    if (!Array.isArray(parsed)) return []
    return parsed.filter(isMessage)
  } catch {
    // 损坏的本地数据不能把整页打挂：当作空会话并等待下一次写入覆盖。
    return []
  }
}

export function savePlaygroundMessages(
  key: string,
  messages: readonly PlaygroundMessage[],
): void {
  if (typeof sessionStorage === 'undefined') return
  try {
    sessionStorage.setItem(key, JSON.stringify(messages))
  } catch {
    // 配额/隐私模式写入失败不影响当前页面继续对话。
  }
}

export function clearPlaygroundMessages(key: string): void {
  if (typeof sessionStorage === 'undefined') return
  try {
    sessionStorage.removeItem(key)
  } catch {
    // 同上：清空只是本地便利，失败不阻断。
  }
}
