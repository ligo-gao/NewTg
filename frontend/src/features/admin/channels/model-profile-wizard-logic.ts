/**
 * 模型档案向导的纯数据层（Task 4）。
 *
 * 这层只做「上游事实 + 管理员默认值 → model_meta 写入体」的转换：不渲染、
 * 不发请求、不读全局状态，便于单测。UI 在 Task 5/6。
 *
 * 重要约束：能力 / 模态 / 参数 / 知识截止 *只能* 来自管理员输入或向导默认
 * 值。探针响应（`FetchChannelModelEntry`）没有这些字段，因此这里绝不从
 * 模型名或供应商推断它们。
 */
import type { components } from '@/api/schema'

import type { FetchChannelModelEntry } from './types'

type ModelMeta = components['schemas']['ModelMeta']
type ModelMetaCreateRequest = components['schemas']['ModelMetaCreateRequest']
type ModelMetaUpdateRequest = components['schemas']['ModelMetaUpdateRequest']
type Method = ModelMeta['endpoints'][string][number]
type Capability = ModelMeta['capabilities'][number]
type Modality = ModelMeta['modalities'][number]
type ParameterType = ModelMeta['parameters'][number]['type']

/**
 * 公开端点枚举，顺序与后端 `publicEndpointMapping`
 * （backend/internal/httpapi/handlers_public_pricing.go）一致。
 */
export const ENDPOINT_KEYS = [
  'chat.completions',
  'messages',
  'embeddings',
  'images.generations',
  'audio.speech',
  'audio.transcriptions',
  'rerank',
  'generateContent',
] as const

export type EndpointKey = (typeof ENDPOINT_KEYS)[number]

/** 与 ModelMutateSheet.tsx / model_meta 契约同序同值。 */
export const CAPABILITY_KEYS = [
  'text',
  'vision',
  'audio',
  'video',
  'function_calling',
  'tool_use',
  'reasoning',
  'embedding',
  'rerank',
  'image_generation',
  'speech',
  'transcription',
] as const satisfies readonly Capability[]

export const MODALITY_KEYS = [
  'text',
  'image',
  'audio',
  'video',
] as const satisfies readonly Modality[]

export const PARAMETER_TYPES = [
  'string',
  'number',
  'integer',
  'boolean',
  'array',
  'object',
] as const satisfies readonly ParameterType[]

/** 单次确认的模型行数上限（与后端批量写入的规模预期一致）。 */
export const MAX_WIZARD_ROWS = 200

/** 与后端 `maxModelMetaDisplayNameRunes` / `maxModelMetaDescriptionRunes` 同口径。 */
export const DISPLAY_NAME_MAX = 64
export const DESCRIPTION_MAX = 512

/**
 * 公开端点枚举 → model_meta `endpoints` 的路径。
 *
 * 这是 `publicEndpointMapping` 的反向表：后端把档案里的路径折叠成公开
 * 枚举，向导要把勾选的枚举写回路径。**新增公开端点时两处同步**——只改
 * 后端会让这里写出后端不认识的路径，只改这里会让新端点永远不出现在公开
 * 筛选里。
 */
const ENDPOINT_PATHS: Readonly<Record<EndpointKey, string>> = {
  'chat.completions': '/v1/chat/completions',
  messages: '/v1/messages',
  embeddings: '/v1/embeddings',
  'images.generations': '/v1/images/generations',
  'audio.speech': '/v1/audio/speech',
  'audio.transcriptions': '/v1/audio/transcriptions',
  rerank: '/v1/rerank',
  generateContent: '/v1beta/models/{model}:generateContent',
}

/**
 * 这 8 个公开端点都是 POST 路由。公开 facet 只看路径是否存在
 * （`deriveEndpointTypes`），方法只是 model_meta 展示的一部分。
 */
const ENDPOINT_METHODS: readonly Method[] = ['POST']

const ENDPOINT_KEY_SET: ReadonlySet<string> = new Set(ENDPOINT_KEYS)
const CAPABILITY_KEY_SET: ReadonlySet<string> = new Set(CAPABILITY_KEYS)
const MODALITY_KEY_SET: ReadonlySet<string> = new Set(MODALITY_KEYS)
const PARAMETER_TYPE_SET: ReadonlySet<string> = new Set(PARAMETER_TYPES)

function isEndpointKey(value: string): value is EndpointKey {
  return ENDPOINT_KEY_SET.has(value)
}

function isCapability(value: string): value is Capability {
  return CAPABILITY_KEY_SET.has(value)
}

function isModality(value: string): value is Modality {
  return MODALITY_KEY_SET.has(value)
}

function isParameterType(value: string): value is ParameterType {
  return PARAMETER_TYPE_SET.has(value)
}

export interface ParameterRow {
  name: string
  type: string
  description: string
  defaultValue: string
  /**
   * 既有档案的参数可能带 `required`；编辑时保留它，避免「只改一个字段却把
   * 必填标记悄悄改成 false」。`parseParameterLines` 不产出这个字段（文本
   * 格式没有必填标记），写入时回落 false。
   */
  required?: boolean
}

export interface WizardDefaults {
  vendorName: string
  endpoints: EndpointKey[]
  capabilities: string[]
  modalities: string[]
  parameters: ParameterRow[]
  knowledgeCutoff: string
  tags: string[]
}

export interface WizardRow {
  id: string
  displayName: string
  description: string
  contextLength: string
  maxOutput: string
  releasedAt: string
  endpoints: EndpointKey[]
  vendorName: string
  capabilities: string[]
  modalities: string[]
  parameters: ParameterRow[]
  knowledgeCutoff: string
  tags: string[]
  factsSource: string
  selected: boolean
  overwrite: boolean
}

/**
 * 把勾选的公开端点枚举写成 model_meta 的 `{path: [methods]}`。
 *
 * 按 `ENDPOINT_KEYS` 的固定顺序输出（与后端 facet 顺序一致），重复的 key
 * 自然折叠。内部构造直接产出契约类型；公开签名按 brief 保持宽松的
 * `Record<string, string[]>`（`Method[]` 可赋值给 `string[]`）。
 */
function endpointObjectOf(keys: EndpointKey[]): ModelMeta['endpoints'] {
  const out: ModelMeta['endpoints'] = {}
  for (const key of ENDPOINT_KEYS) {
    if (keys.includes(key)) {
      out[ENDPOINT_PATHS[key]] = [...ENDPOINT_METHODS]
    }
  }
  return out
}

export function endpointsObject(keys: EndpointKey[]): Record<string, string[]> {
  return endpointObjectOf(keys)
}

/**
 * 既有档案 → 行内参数表。`description` / `default` / `required` 在契约里是
 * 附加键（`unknown`），逐项做类型守卫；对象全部新建，行编辑不会反向改到
 * 查询缓存里的档案。
 */
function profileParameters(profile: ModelMeta | undefined): ParameterRow[] {
  if (profile === undefined) return []
  return profile.parameters.map((parameter) => ({
    name: parameter.name,
    type: parameter.type,
    description:
      typeof parameter.description === 'string' ? parameter.description : '',
    defaultValue: typeof parameter.default === 'string' ? parameter.default : '',
    ...(parameter.required === true ? { required: true } : {}),
  }))
}

/**
 * 把探针条目投影成向导行。
 *
 * - `selected`：创建态全选（没有既有档案）；编辑态只预选 `preselected`
 *   里的模型，避免向导默认改写既有档案。
 * - `overwrite` 恒为 false：覆盖必须由操作者逐行勾选。
 * - 能力 / 模态 / 参数 / 知识截止 / 标签：既有档案取本地值（深拷贝），
 *   没有档案的行留空。**绝不从模型名推断**；既有值只是让第 3 步的勾选
 *   变成叠加/取消，而不是拿空集合盲替换。
 */
export function buildRows(
  entries: FetchChannelModelEntry[],
  existing: Map<string, ModelMeta>,
  preselected: string[],
): WizardRow[] {
  const preselectedIds = new Set(preselected)
  return entries.map((entry) => {
    const profile = existing.get(entry.id)
    return {
      id: entry.id,
      displayName: entry.display_name,
      description: entry.description,
      contextLength: entry.context_length == null ? '' : String(entry.context_length),
      maxOutput: entry.max_output == null ? '' : String(entry.max_output),
      releasedAt: entry.released_at,
      endpoints: entry.endpoints.filter(isEndpointKey),
      vendorName: profile?.vendor_name ?? entry.vendor_suggestion,
      capabilities: profile === undefined ? [] : [...profile.capabilities],
      modalities: profile === undefined ? [] : [...profile.modalities],
      parameters: profileParameters(profile),
      knowledgeCutoff: profile?.knowledge_cutoff ?? '',
      tags: profile === undefined ? [] : [...profile.tags],
      factsSource: entry.facts_source,
      selected: preselectedIds.has(entry.id) || profile === undefined,
      overwrite: false,
    }
  })
}

function applyString(current: string, fallback: string, emptyOnly: boolean): string {
  if (!emptyOnly) return fallback
  return current.trim() === '' ? fallback : current
}

function applyList<T>(current: T[], fallback: T[], emptyOnly: boolean): T[] {
  const source = emptyOnly && current.length > 0 ? current : fallback
  return [...source]
}

function applyParameterList(
  current: ParameterRow[],
  fallback: ParameterRow[],
  emptyOnly: boolean,
): ParameterRow[] {
  const source = emptyOnly && current.length > 0 ? current : fallback
  return source.map((row) => ({ ...row }))
}

/**
 * 把批量默认值套用到行。
 *
 * `emptyOnly` 为 true 时只填空白字段（字符串空 / 数组空 / 参数表空），为
 * false 时整体替换。`defaults.vendorName` 为空串时不覆盖任何行——空供应商
 * 没有意义，而 buildRows 可能已经带了上游建议。
 */
export function applyDefaults(
  rows: WizardRow[],
  defaults: WizardDefaults,
  emptyOnly: boolean,
): WizardRow[] {
  return rows.map((row) => ({
    ...row,
    endpoints: applyList(row.endpoints, defaults.endpoints, emptyOnly),
    capabilities: applyList(row.capabilities, defaults.capabilities, emptyOnly),
    modalities: applyList(row.modalities, defaults.modalities, emptyOnly),
    parameters: applyParameterList(row.parameters, defaults.parameters, emptyOnly),
    knowledgeCutoff: applyString(row.knowledgeCutoff, defaults.knowledgeCutoff, emptyOnly),
    tags: applyList(row.tags, defaults.tags, emptyOnly),
    vendorName:
      defaults.vendorName === ''
        ? row.vendorName
        : applyString(row.vendorName, defaults.vendorName, emptyOnly),
  }))
}

/**
 * 解析 `name:type[:描述]` 文本（一行一个参数）。
 *
 * 缺少冒号、名字为空、类型不在契约枚举里的行整行丢弃；描述可以再含冒号。
 * `defaultValue` 不在这套文本格式里，恒为空串。
 */
export function parseParameterLines(text: string): ParameterRow[] {
  const rows: ParameterRow[] = []
  for (const rawLine of text.split('\n')) {
    const line = rawLine.trim()
    if (line === '') continue
    const parts = line.split(':')
    const name = (parts[0] ?? '').trim()
    const type = (parts[1] ?? '').trim()
    if (name === '' || !PARAMETER_TYPE_SET.has(type)) continue
    rows.push({
      name,
      type,
      description: parts.slice(2).join(':').trim(),
      defaultValue: '',
    })
  }
  return rows
}

/** `parseParameterLines` 的逆操作。 */
export function formatParameterLines(rows: ParameterRow[]): string {
  return rows
    .filter((row) => row.name.trim() !== '')
    .map((row) => {
      const name = row.name.trim()
      const type = row.type.trim()
      const description = row.description.trim()
      return description === '' ? `${name}:${type}` : `${name}:${type}:${description}`
    })
    .join('\n')
}

function parseOptionalInt(value: string): number | null {
  const trimmed = value.trim()
  if (trimmed === '') return null
  const parsed = Number(trimmed)
  if (!Number.isSafeInteger(parsed) || parsed < 0) return null
  return parsed
}

/**
 * 行内校验。写库逻辑（`parseOptionalInt` / `parameterPayload`）遇到非法输入
 * 会静默丢掉字段，所以第 3 步必须自己把这些情况报出来，不能依赖写入体。
 *
 * 返回的是字段名而不是文案：文案归 UI（i18n），纯逻辑层不持有翻译。
 */
export type RowIssue =
  | 'displayName'
  | 'description'
  | 'contextLength'
  | 'maxOutput'
  | 'knowledgeCutoff'
  | 'parameters'

/** rune 口径与后端 `utf8.RuneCountInString` 一致（`[...str]` 按码点切分）。 */
export function runeLength(value: string): number {
  return [...value].length
}

function optionalIntIsValid(value: string): boolean {
  const trimmed = value.trim()
  if (trimmed === '') return true
  return /^\d+$/.test(trimmed) && Number.isSafeInteger(Number(trimmed))
}

/**
 * `knowledge_cutoff` 的契约是 `YYYY-MM`（后端 `validKnowledgeCutoff` 与
 * `model_meta_knowledge_cutoff_chk` 同口径），第 2 步的 month 输入也产出
 * 这个形状；写成别的形状后端会 422。
 */
const KNOWLEDGE_CUTOFF_PATTERN = /^\d{4}-(0[1-9]|1[0-2])$/

export function knowledgeCutoffIsValid(value: string): boolean {
  const trimmed = value.trim()
  return trimmed === '' || KNOWLEDGE_CUTOFF_PATTERN.test(trimmed)
}

/**
 * 参数行：完全空白的行视为「尚未添加」，不算错误；有名字就必须有合法类型，
 * 有类型就必须有名字。类型非法时**不改写**成别的类型，只报错。
 */
export function parametersAreValid(rows: ParameterRow[]): boolean {
  return rows.every((row) => {
    const name = row.name.trim()
    const type = row.type.trim()
    if (name === '' && type === '') return true
    return name !== '' && isParameterType(type)
  })
}

export function rowIssues(row: WizardRow): RowIssue[] {
  const issues: RowIssue[] = []
  if (runeLength(row.displayName.trim()) > DISPLAY_NAME_MAX) {
    issues.push('displayName')
  }
  if (runeLength(row.description.trim()) > DESCRIPTION_MAX) {
    issues.push('description')
  }
  if (!optionalIntIsValid(row.contextLength)) issues.push('contextLength')
  if (!optionalIntIsValid(row.maxOutput)) issues.push('maxOutput')
  if (!knowledgeCutoffIsValid(row.knowledgeCutoff)) issues.push('knowledgeCutoff')
  if (!parametersAreValid(row.parameters)) issues.push('parameters')
  return issues
}

export function rowIsValid(row: WizardRow): boolean {
  return rowIssues(row).length === 0
}

/** 第 3 步摘要与提交循环共用的「这一行会做什么」。 */
export type RowAction = 'create' | 'update' | 'skip'

/**
 * 锁定档案（`sync_official === false`）永远跳过；已有档案默认也跳过，
 * 只有管理员逐行勾了「覆盖」才更新。绝不由模型名或渠道类型推断。
 */
export function rowActionOf(
  row: WizardRow,
  profile: ModelMeta | undefined,
): RowAction {
  if (profile === undefined) return 'create'
  if (!profile.sync_official) return 'skip'
  return row.overwrite ? 'update' : 'skip'
}

/**
 * 行内供应商的四种状态。第 3 步与写库循环共用，避免「下拉显示未选中、
 * 提交却用了某个名字」这类不一致。
 *
 * - `unset`：没有供应商（或管理员清空）。
 * - `listed`：本地目录命中（`name` 用目录里的原始大小写）。
 * - `new`：归一化后等于第 2 步显式填写的新建名——提交时会创建。
 * - `unresolved`：探测建议值不在目录里，且不是第 2 步显式新建的那个——
 *   **不创建**，本次不设置。
 *
 * 供应商创建的唯一入口是第 2 步的「新建供应商」，本函数只判断归属。
 */
export type VendorState =
  | { kind: 'unset' }
  | { kind: 'listed'; name: string; id: number }
  | { kind: 'new'; name: string }
  | { kind: 'unresolved'; name: string }

export interface VendorLookup {
  ids: ReadonlyMap<string, number>
  nameById: ReadonlyMap<number, string>
}

export function vendorStateOf(
  vendorName: string,
  directory: VendorLookup,
  newVendorName: string,
): VendorState {
  const name = vendorName.trim()
  if (name === '') return { kind: 'unset' }
  const key = name.toLowerCase()
  const id = directory.ids.get(key)
  if (id !== undefined) {
    return { kind: 'listed', name: directory.nameById.get(id) ?? name, id }
  }
  const pending = newVendorName.trim()
  if (pending !== '' && pending.toLowerCase() === key) {
    return { kind: 'new', name }
  }
  return { kind: 'unresolved', name }
}

export type RowResultAction = 'created' | 'updated' | 'skipped' | 'failed'

export interface RowResult {
  id: string
  action: RowResultAction
  /** 失败原因（已本地化）；成功 / 跳过行没有。 */
  message?: string
  /**
   * 校验失败为 false：输入没改，重试必然再失败。供应商创建失败、409 等
   * 可重试。
   */
  retryable: boolean
}

function normalizeTags(tags: string[]): string[] {
  return tags.map((tag) => tag.trim()).filter((tag) => tag !== '')
}

/**
 * 参数行的写入体：形状与 ModelMutateSheet 一致，`required` 恒 false（向导
 * 的文本格式不支持必填标记），`default` / `description` 只在非空时携带。
 *
 * 类型必须落在契约枚举内，否则整行丢弃——与 `parseParameterLines` /
 * `filter(isEndpointKey)` 同策略：不替操作者改写他没选的类型。
 */
function parameterPayload(
  rows: ParameterRow[],
): NonNullable<ModelMetaCreateRequest['parameters']> {
  const out: NonNullable<ModelMetaCreateRequest['parameters']> = []
  for (const row of rows) {
    const name = row.name.trim()
    const type = row.type.trim()
    if (name === '' || !isParameterType(type)) continue
    const entry: { name: string; type: ParameterType } & Record<string, unknown> = {
      name,
      type,
      // 既有档案的必填标记原样保留；文本格式解析出来的行没有该字段。
      required: row.required ?? false,
    }
    const defaultValue = row.defaultValue.trim()
    if (defaultValue !== '') entry.default = defaultValue
    const description = row.description.trim()
    if (description !== '') entry.description = description
    out.push(entry)
  }
  return out
}

/**
 * 新建档案的请求体：只放非空字段，其余交给服务端默认值（status=1、
 * sync_official=true、name_rule=0）。`vendor_id` 始终携带，null 表示不归属。
 */
export function buildCreateBody(
  row: WizardRow,
  vendorId: number | null,
): ModelMetaCreateRequest {
  const body: ModelMetaCreateRequest = {
    model_name: row.id.trim(),
    vendor_id: vendorId,
  }

  const displayName = row.displayName.trim()
  if (displayName !== '') body.display_name = displayName

  const description = row.description.trim()
  if (description !== '') body.description = description

  const tags = normalizeTags(row.tags)
  if (tags.length > 0) body.tags = tags

  const endpoints = endpointObjectOf(row.endpoints)
  if (Object.keys(endpoints).length > 0) body.endpoints = endpoints

  const capabilities = row.capabilities.filter(isCapability)
  if (capabilities.length > 0) body.capabilities = capabilities

  const modalities = row.modalities.filter(isModality)
  if (modalities.length > 0) body.modalities = modalities

  const parameters = parameterPayload(row.parameters)
  if (parameters.length > 0) body.parameters = parameters

  const contextLength = parseOptionalInt(row.contextLength)
  if (contextLength !== null) body.context_length = contextLength

  const maxOutput = parseOptionalInt(row.maxOutput)
  if (maxOutput !== null) body.max_output = maxOutput

  const knowledgeCutoff = row.knowledgeCutoff.trim()
  if (knowledgeCutoff !== '') body.knowledge_cutoff = knowledgeCutoff

  const releasedAt = row.releasedAt.trim()
  if (releasedAt !== '') body.released_at = releasedAt

  return body
}

function cloneEndpoints(endpoints: ModelMeta['endpoints']): ModelMeta['endpoints'] {
  const out: ModelMeta['endpoints'] = {}
  for (const [path, methods] of Object.entries(endpoints)) out[path] = [...methods]
  return out
}

/**
 * 更新档案的请求体。
 *
 * `pick(rowValue, existingValue)`：向导字段为空串 / 空数组 / null 时一律回
 * 落到既有值，绝不把已填好的列清空。`updated_at` 抄既有值（乐观锁）；
 * `icon` / `status` / `sync_official` / `name_rule` 向导不维护，始终从既有
 * 值来。`model_name` 刻意不发送：编辑路径上它不可改，行由路径 id 定位。
 *
 * 是否覆盖既有档案由调用方按 `row.overwrite` 决定，这里只负责生成安全
 * 的请求体。
 */
export function buildUpdateBody(
  row: WizardRow,
  existing: ModelMeta,
  vendorId: number | null,
): ModelMetaUpdateRequest {
  const displayName = row.displayName.trim()
  const description = row.description.trim()
  const knowledgeCutoff = row.knowledgeCutoff.trim()
  const releasedAt = row.releasedAt.trim()
  const tags = normalizeTags(row.tags)
  const endpoints = endpointObjectOf(row.endpoints)
  const capabilities = row.capabilities.filter(isCapability)
  const modalities = row.modalities.filter(isModality)
  const parameters = parameterPayload(row.parameters)

  return {
    updated_at: existing.updated_at,
    display_name: displayName === '' ? (existing.display_name ?? null) : displayName,
    description: description === '' ? (existing.description ?? null) : description,
    icon: existing.icon ?? null,
    vendor_id: vendorId ?? existing.vendor_id ?? null,
    tags: tags.length > 0 ? tags : [...existing.tags],
    endpoints:
      Object.keys(endpoints).length > 0 ? endpoints : cloneEndpoints(existing.endpoints),
    capabilities: capabilities.length > 0 ? capabilities : [...existing.capabilities],
    modalities: modalities.length > 0 ? modalities : [...existing.modalities],
    parameters:
      parameters.length > 0
        ? parameters
        : existing.parameters.map((parameter) => ({ ...parameter })),
    context_length: parseOptionalInt(row.contextLength) ?? existing.context_length ?? null,
    max_output: parseOptionalInt(row.maxOutput) ?? existing.max_output ?? null,
    knowledge_cutoff:
      knowledgeCutoff === '' ? (existing.knowledge_cutoff ?? null) : knowledgeCutoff,
    released_at: releasedAt === '' ? (existing.released_at ?? null) : releasedAt,
    status: existing.status,
    sync_official: existing.sync_official,
    name_rule: existing.name_rule,
  }
}
