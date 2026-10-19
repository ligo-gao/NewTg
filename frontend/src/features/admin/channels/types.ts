/**
 * Channel management types (modules-06 §1.3, §2–§5).
 *
 * `ChannelStatus` is the three-state machine frozen by modules-06 §5.1:
 * 1 enabled, 2 manual_disabled, 3 auto_disabled. There is deliberately no
 * `deleted` state — deletion removes the row.
 */

export type ChannelStatus = 1 | 2 | 3

export type ChannelSort = 'priority' | 'name' | 'response_time'

export interface ChannelCredentialsMasked {
  api_key: string
  deployment?: string
  api_version?: string
}

export interface Channel {
  id: number
  name: string
  type: string
  status: ChannelStatus
  priority: number
  weight: number
  supported_models: string[]
  /** `null` means "all groups" (modules-06 §3.2 F-06-04). */
  group_tags: string[] | null
  rate_limit: number | null
  auto_ban: boolean
  test_model: string | null
  remark: string | null
  tag: string | null
  fail_count: number
  last_test_at: string | null
  last_test_result: boolean | null
  created_at: string
  updated_at: string
  /** Optimistic-concurrency token derived from `updated_at` (R-P8). */
  version: string
  /**
   * Detail/POST/PATCH responses only (GC-6 detail exception). List rows carry
   * neither field, so list fixtures must not invent them.
   */
  credentials_masked?: ChannelCredentialsMasked
  credentials_set?: boolean
}

export interface ChannelListResponse {
  items: Channel[]
  total: number
  page: number
  page_size: number
}

export interface ChannelListParams {
  page: number
  pageSize: number
  keyword: string
  status: ChannelStatus | null
  types: string[]
  group: string
  model: string
  sort: ChannelSort
}

export interface ChannelCredentials {
  api_key: string
  deployment?: string
  api_version?: string
}

export interface CreateChannelInput {
  name: string
  type: string
  base_url?: string
  credentials: ChannelCredentials
  supported_models: string[]
  group_tags?: string[] | null
  priority?: number
  weight?: number
  rate_limit?: number | null
  auto_ban?: boolean
  test_model?: string | null
  remark?: string | null
  tag?: string | null
}

export interface UpdateChannelInput {
  /** Required by the backend on every PATCH (R-P8). */
  expected_version: string
  name?: string
  type?: string
  base_url?: string | null
  /** Omit or leave empty to keep the stored credential unchanged. */
  credentials?: ChannelCredentials | null
  supported_models?: string[]
  group_tags?: string[] | null
  priority?: number
  weight?: number
  rate_limit?: number | null
  auto_ban?: boolean
  test_model?: string | null
  remark?: string | null
  tag?: string | null
  status?: 1 | 2
}

export interface BatchOperationResult {
  succeeded: number[]
  failed: Array<{ id: number; reason: string }>
}

export type BatchStatusAction = 'enable' | 'disable'

export interface ChannelTestInput {
  model?: string
  stream?: boolean
}

export interface ChannelTestResult {
  success: boolean
  latency_ms: number
  model: string
  error_class?: string
}

// ---------------------------------------------------------------------------
// Channel type catalogue (R-P20)
// ---------------------------------------------------------------------------

/**
 * Protocol families from relay-02 §5 ("协议族"). The table there is the
 * authority for both membership and counts; the channel type selector renders
 * exactly these headings.
 */
export type ChannelTypeGroup =
  | 'openai'
  | 'anthropic'
  | 'gemini'
  | 'palm'
  | 'proprietary'
  | 'task'
  | 'flexible'
  | 'gateway'
  | 'legacy'

export const CHANNEL_TYPE_GROUP_ORDER: readonly ChannelTypeGroup[] = [
  'openai',
  'anthropic',
  'gemini',
  'palm',
  'proprietary',
  'task',
  'flexible',
  'gateway',
  'legacy',
]

interface ChannelTypeEntry {
  value: string
  group: ChannelTypeGroup
}

/**
 * The 59 named types of `contract.ChannelTypes` (backend/internal/contract/
 * channels.go), minus the empty sentinel. Order inside each family follows the
 * baseline numbering only as a stable display order. R-P20: the sentinel is
 * not a selectable type, so this list must contain exactly 59 entries.
 */
export const CHANNEL_TYPES: readonly ChannelTypeEntry[] = [
  // OpenAI compatible — 24 (relay-02 §5 family tally)
  { value: 'openai', group: 'openai' },
  { value: 'azure_openai', group: 'openai' },
  { value: 'openai_max', group: 'openai' },
  { value: 'ohmygpt', group: 'openai' },
  { value: 'custom', group: 'openai' },
  { value: 'ails', group: 'openai' },
  { value: 'aiproxy', group: 'openai' },
  { value: 'api2gpt', group: 'openai' },
  { value: 'aigc2d', group: 'openai' },
  { value: 'ali', group: 'openai' },
  { value: '360', group: 'openai' },
  { value: 'openrouter', group: 'openai' },
  { value: 'fastgpt', group: 'openai' },
  { value: 'zhipu_v4', group: 'openai' },
  { value: 'perplexity', group: 'openai' },
  { value: 'lingyiwanwu', group: 'openai' },
  { value: 'siliconflow', group: 'openai' },
  { value: 'mistral', group: 'openai' },
  { value: 'deepseek', group: 'openai' },
  { value: 'volcengine', group: 'openai' },
  { value: 'baidu_v2', group: 'openai' },
  { value: 'xinference', group: 'openai' },
  { value: 'xai', group: 'openai' },
  { value: 'submodel', group: 'openai' },
  // Anthropic native — 2
  { value: 'anthropic', group: 'anthropic' },
  { value: 'moonshot', group: 'anthropic' },
  // Gemini native (incl. Vertex) — 2
  { value: 'gemini', group: 'gemini' },
  { value: 'vertex_ai', group: 'gemini' },
  // Google PaLM legacy — 1
  { value: 'palm', group: 'palm' },
  // Proprietary protocols — 15
  { value: 'ollama', group: 'proprietary' },
  { value: 'baidu', group: 'proprietary' },
  { value: 'zhipu', group: 'proprietary' },
  { value: 'xunfei', group: 'proprietary' },
  { value: 'tencent', group: 'proprietary' },
  { value: 'aws', group: 'proprietary' },
  { value: 'cohere', group: 'proprietary' },
  { value: 'minimax', group: 'proprietary' },
  { value: 'dify', group: 'proprietary' },
  { value: 'jina', group: 'proprietary' },
  { value: 'cloudflare', group: 'proprietary' },
  { value: 'mokaai', group: 'proprietary' },
  { value: 'coze', group: 'proprietary' },
  { value: 'replicate', group: 'proprietary' },
  { value: 'codex', group: 'proprietary' },
  // Task platforms — 9
  { value: 'midjourney', group: 'task' },
  { value: 'midjourney_plus', group: 'task' },
  { value: 'sunoapi', group: 'task' },
  { value: 'kling', group: 'task' },
  { value: 'jimeng', group: 'task' },
  { value: 'vidu', group: 'task' },
  { value: 'doubao_video', group: 'task' },
  { value: 'sora', group: 'task' },
  { value: 'task_plugin', group: 'task' },
  // Flexible integration — 3
  { value: 'advanced_custom', group: 'flexible' },
  { value: 'vllm', group: 'flexible' },
  { value: 'sglang', group: 'flexible' },
  // Gateway compatible — 2
  { value: 'sub2api', group: 'gateway' },
  { value: 'newapi', group: 'gateway' },
  // Legacy / unregistered (no adapter) — 1
  { value: 'aiproxy_library', group: 'legacy' },
]

export function channelTypesInGroup(group: ChannelTypeGroup): string[] {
  return CHANNEL_TYPES.filter((entry) => entry.group === group).map(
    (entry) => entry.value,
  )
}

export function isChannelType(value: string): boolean {
  return CHANNEL_TYPES.some((entry) => entry.value === value)
}

// ---------------------------------------------------------------------------
// Native protocol channels (relay-02 §2.5)
// ---------------------------------------------------------------------------

export interface NativeChannelTypeInfo {
  /** i18n key for the human label shown inside the type selector. */
  labelKey: string
  /** i18n key for the one-line explanation shown under the selector. */
  descriptionKey: string
}

/**
 * The four channel types whose upstream speaks an Anthropic/Gemini native
 * protocol (relay-02 §2.5): `AKAnthropicNative` = types 14 (`anthropic`) and
 * 25 (`moonshot`, an Anthropic-compatible `/v1/messages` upstream), and the
 * Gemini family = type 24 (`gemini`) plus type 41 (`vertex_ai`). Vertex has
 * its own adapter key and is grouped with Gemini for display, but that adapter
 * is not implemented in this release (requests get ErrEndpointUnsupported) and
 * it is not a cross-protocol target.
 */
export const NATIVE_CHANNEL_TYPES: Readonly<
  Record<string, NativeChannelTypeInfo>
> = {
  anthropic: {
    labelKey: 'common.channels.nativeType.anthropic.label',
    descriptionKey: 'common.channels.nativeType.anthropic.description',
  },
  moonshot: {
    labelKey: 'common.channels.nativeType.moonshot.label',
    descriptionKey: 'common.channels.nativeType.moonshot.description',
  },
  gemini: {
    labelKey: 'common.channels.nativeType.gemini.label',
    descriptionKey: 'common.channels.nativeType.gemini.description',
  },
  vertex_ai: {
    labelKey: 'common.channels.nativeType.vertex_ai.label',
    descriptionKey: 'common.channels.nativeType.vertex_ai.description',
  },
}

export function nativeChannelTypeInfo(
  type: string,
): NativeChannelTypeInfo | undefined {
  return Object.prototype.hasOwnProperty.call(NATIVE_CHANNEL_TYPES, type)
    ? NATIVE_CHANNEL_TYPES[type]
    : undefined
}

// ---------------------------------------------------------------------------
// Cross-protocol forwarding (relay-02 §2.5 / M4 Task 5)
// ---------------------------------------------------------------------------

/**
 * Header that selects the *upstream* protocol on `POST /v1/chat/completions`
 * (M4 Task 5, `backend/internal/relay/crossproto`). Values are the native
 * protocol names; an absent header keeps the channel's own protocol.
 */
export const CROSS_PROTOCOL_HEADER = 'x-newtg-protocol'

export const CROSS_PROTOCOL_VALUES = ['openai', 'anthropic', 'gemini'] as const

/**
 * ConfigCenter key owned by module 08 (site settings). M4 has no settings API,
 * so the console can only document it — never read or write it.
 */
export const CROSS_PROTOCOL_SWITCH_KEY = 'relay.cross_protocol_forward_enabled'

/**
 * Types whose upstream endpoint cannot be derived (relay-02 §7.1 "必填" group;
 * mirrored by `requiredBaseURLTypes` in backend/internal/httpapi/
 * handlers_channel.go). The create form requires `base_url` for exactly these.
 */
export const REQUIRED_BASE_URL_TYPES: ReadonlySet<string> = new Set([
  'azure_openai',
  'custom',
  'sunoapi',
  'volcengine',
  'doubao_video',
  'newapi',
  'task_plugin',
  'vllm',
  'sglang',
])
