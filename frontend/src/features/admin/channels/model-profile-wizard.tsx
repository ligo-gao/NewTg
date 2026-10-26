/**
 * 补全模型档案向导：第 1 步「选模型」与第 2 步「批量默认值」。
 *
 * 步骤 3（逐行确认）、步骤 4（结果）与写库循环属于 Task 6，这里只呈现四个
 * 步骤的导航骨架，3/4 步保持禁用。上游事实只来自探测响应；能力 / 模态 /
 * 参数 / 知识截止 *只能* 由管理员在向导里填写，绝不从模型名推断。
 */
import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import type { components } from '@/api/schema'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'

import { fetchChannelModels } from './api'
import {
  CAPABILITY_KEYS,
  ENDPOINT_KEYS,
  MODALITY_KEYS,
  applyDefaults,
  buildRows,
  parseParameterLines,
  type EndpointKey,
  type WizardDefaults,
  type WizardRow,
} from './model-profile-wizard-logic'
import type { FetchChannelModelEntry, FetchChannelModelsInput } from './types'

type ModelMeta = components['schemas']['ModelMeta']

/** `/api/models`、`/api/vendors` 的每页上限（契约上限 100）。 */
const PAGE_SIZE = 100
/** 目录是百级规模；封顶避免上游异常 total 时无限翻页。 */
const MAX_PAGES = 10

const MODEL_CATALOG_QUERY_KEY = ['admin', 'channel-model-profile-wizard', 'models'] as const
const VENDOR_QUERY_KEY = ['admin', 'channel-model-profile-wizard', 'vendors'] as const

/** 「本地没有，提交时新建」在下拉里的哨兵值（不会与真实供应商名冲突）。 */
const CREATE_VENDOR = '__create__'

const EMPTY_CATALOG: ReadonlyMap<string, ModelMeta> = new Map()

const EMPTY_VENDOR_DIRECTORY: VendorDirectory = { ids: new Map(), names: [] }

interface VendorDirectory {
  /** `lower(trim(name))` → id，与 Task 6 的建商去重口径一致。 */
  ids: Map<string, number>
  /** 原始大小写，仅用于下拉展示。 */
  names: string[]
}

async function loadModelCatalog(): Promise<Map<string, ModelMeta>> {
  const catalog = new Map<string, ModelMeta>()
  let loaded = 0
  for (let page = 1; page <= MAX_PAGES; page += 1) {
    const data = requireData(
      await apiClient.GET('/api/models', {
        params: { query: { p: page, page_size: PAGE_SIZE } },
      }),
    )
    const items = data.items ?? []
    for (const item of items) catalog.set(item.model_name, item)
    loaded += items.length
    if (items.length === 0 || loaded >= data.total) break
  }
  return catalog
}

async function loadVendorDirectory(): Promise<VendorDirectory> {
  const ids = new Map<string, number>()
  const names: string[] = []
  let loaded = 0
  for (let page = 1; page <= MAX_PAGES; page += 1) {
    const data = requireData(
      await apiClient.GET('/api/vendors', {
        params: { query: { p: page, page_size: PAGE_SIZE } },
      }),
    )
    const items = data.items ?? []
    for (const vendor of items) {
      const name = vendor.name.trim()
      const key = name.toLowerCase()
      if (key === '') continue
      if (!ids.has(key)) names.push(name)
      ids.set(key, vendor.id)
    }
    loaded += items.length
    if (items.length === 0 || loaded >= data.total) break
  }
  names.sort((left, right) => left.localeCompare(right))
  return { ids, names }
}

function emptyWizardDefaults(): WizardDefaults {
  return {
    vendorName: '',
    endpoints: [],
    capabilities: [],
    modalities: [],
    parameters: [],
    knowledgeCutoff: '',
    tags: [],
  }
}

function emptyEntry(id: string): FetchChannelModelEntry {
  return {
    id,
    display_name: '',
    description: '',
    context_length: null,
    max_output: null,
    released_at: '',
    endpoints: [],
    vendor_suggestion: '',
    facts_source: '',
  }
}

export interface ModelProfileWizardProps {
  /** 与 FetchModelsDialog 同一个探测请求体。 */
  request: FetchChannelModelsInput
  /** 表单当前的 supported_models。编辑态据此预选。 */
  channelModels: string[]
  onBack: () => void
  onApplyModels: (models: string[]) => void
}

type ProbeState = 'loading' | 'ready' | 'error'
type ProfileState = 'new' | 'existing' | 'locked'
type FactKind = 'auto' | 'suggest' | 'empty'
type WizardStep = 1 | 2

const PROFILE_BADGE_CLASSES: Record<ProfileState, string> = {
  new: 'border-border-default bg-surface-subtle text-ink-2',
  existing: 'border-info-line bg-info-bg text-info',
  locked: 'border-border-default bg-surface-subtle text-ink-3',
}

const FACT_CHIP_CLASSES: Record<FactKind, string> = {
  auto: 'border-info-line bg-info-bg text-info',
  suggest: 'border-warning-line bg-warning-bg text-warning',
  empty: 'border-border-default bg-surface-subtle text-ink-3',
}

const INPUT_CLASSES =
  'h-9 w-full rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none'

export function ModelProfileWizard({
  request,
  channelModels,
  onBack,
  onApplyModels,
}: ModelProfileWizardProps) {
  const { t } = useI18n()
  const requestKey = [
    request.channel_id ?? '',
    request.type,
    request.base_url ?? '',
    request.credentials?.api_key ?? '',
    request.credentials?.deployment ?? '',
    request.credentials?.api_version ?? '',
  ].join('\u0000')
  const channelModelsKey = channelModels.join('\u0000')

  const [step, setStep] = useState<WizardStep>(1)
  const [probeState, setProbeState] = useState<ProbeState>('loading')
  const [probeError, setProbeError] = useState('')
  const [entries, setEntries] = useState<FetchChannelModelEntry[]>([])
  const [rows, setRows] = useState<WizardRow[]>([])
  const [nonce, setNonce] = useState(0)
  const [query, setQuery] = useState('')
  const [fillChannelModels, setFillChannelModels] = useState(true)
  const [defaults, setDefaults] = useState<WizardDefaults>(emptyWizardDefaults)
  const [parameterText, setParameterText] = useState('')
  const [tagsText, setTagsText] = useState('')
  const [emptyOnly, setEmptyOnly] = useState(true)
  const [appliedCount, setAppliedCount] = useState<number | null>(null)
  const [vendorChoice, setVendorChoice] = useState('')
  const [newVendorName, setNewVendorName] = useState('')
  // 只为每个探测结果建一次行：目录查询的后台重取不应把管理员已套用的
  // 默认值 / 勾选状态冲掉。
  const builtKeyRef = useRef<string | null>(null)

  const catalogQuery = useQuery({
    queryKey: MODEL_CATALOG_QUERY_KEY,
    queryFn: loadModelCatalog,
    staleTime: 30_000,
  })
  const vendorsQuery = useQuery({
    queryKey: VENDOR_QUERY_KEY,
    queryFn: loadVendorDirectory,
    staleTime: 30_000,
  })

  // 探测依赖稳定 key：父组件的 request 是内联对象，直接用对象会让每次渲染
  // 都重发一次上游请求（与 FetchModelsDialog 同款处理）。
  useEffect(() => {
    let cancelled = false
    setProbeState('loading')
    setProbeError('')

    async function load() {
      try {
        const response = await fetchChannelModels(request)
        if (cancelled) return
        // 旧后端没有 entries：按 id 兜底成「只有模型名」的条目。
        setEntries(response.entries ?? response.models.map(emptyEntry))
        setProbeState('ready')
      } catch (error) {
        if (cancelled) return
        setProbeError(
          error instanceof ApiError
            ? error.message
            : t('common.channels.profileWizard.loadError'),
        )
        setProbeState('error')
      }
    }

    void load()
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nonce, requestKey, t])

  const catalogSettled = !catalogQuery.isPending

  // 行由「探测条目 + 既有档案 + 表单当前模型」共同决定，三者都就绪后才建，
  // 否则会把已有档案误标成「待建档」。目录查询失败时降级为空目录，仍允许
  // 管理员继续；本任务不写库。
  useEffect(() => {
    if (probeState !== 'ready' || !catalogSettled) return
    const buildKey = `${nonce}\u0000${channelModelsKey}`
    if (builtKeyRef.current === buildKey) return
    builtKeyRef.current = buildKey
    setRows(
      buildRows(entries, catalogQuery.data ?? new Map(), channelModels),
    )
    // `channelModels` 以稳定 key 参与依赖，避免父组件内联数组造成重建循环。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [probeState, catalogSettled, entries, catalogQuery.data, channelModelsKey])

  const catalog = catalogQuery.data ?? EMPTY_CATALOG
  const vendorDirectory = vendorsQuery.data ?? EMPTY_VENDOR_DIRECTORY

  const filteredRows = useMemo(() => {
    const needle = query.trim().toLowerCase()
    if (needle === '') return rows
    return rows.filter((row) => row.id.toLowerCase().includes(needle))
  }, [rows, query])

  const selectedCount = rows.filter((row) => row.selected).length

  function profileStateOf(row: WizardRow): ProfileState {
    const profile = catalog.get(row.id)
    if (profile === undefined) return 'new'
    return profile.sync_official ? 'existing' : 'locked'
  }

  function displayNameOf(row: WizardRow): string {
    if (row.displayName.trim() !== '') return row.displayName
    return catalog.get(row.id)?.display_name ?? ''
  }

  /**
   * 事实来源只由「上游给没给值」推导，与 `facts_source`（上游目录种类）无关：
   * 自动 = 显示名 / 描述 / 上下文 / 最大输出 / 发布日期至少一项；
   * 建议 = 探测到了端点映射；两者都没有 = 待填。
   */
  function factKindsOf(row: WizardRow): FactKind[] {
    const kinds: FactKind[] = []
    const hasFacts =
      row.displayName.trim() !== '' ||
      row.description.trim() !== '' ||
      row.contextLength !== '' ||
      row.maxOutput !== '' ||
      row.releasedAt !== ''
    if (hasFacts) kinds.push('auto')
    if (row.endpoints.length > 0) kinds.push('suggest')
    if (kinds.length === 0) kinds.push('empty')
    return kinds
  }

  function toggleRow(id: string): void {
    setRows((current) =>
      current.map((row) =>
        row.id === id ? { ...row, selected: !row.selected } : row,
      ),
    )
  }

  function selectVisible(): void {
    const visible = new Set(filteredRows.map((row) => row.id))
    setRows((current) =>
      current.map((row) =>
        visible.has(row.id) ? { ...row, selected: true } : row,
      ),
    )
  }

  function clearAll(): void {
    setRows((current) => current.map((row) => ({ ...row, selected: false })))
  }

  function toggleEndpoint(key: EndpointKey): void {
    setDefaults((current) => ({
      ...current,
      endpoints: current.endpoints.includes(key)
        ? current.endpoints.filter((item) => item !== key)
        : [...current.endpoints, key],
    }))
  }

  function toggleCapability(key: string): void {
    setDefaults((current) => ({
      ...current,
      capabilities: current.capabilities.includes(key)
        ? current.capabilities.filter((item) => item !== key)
        : [...current.capabilities, key],
    }))
  }

  function toggleModality(key: string): void {
    setDefaults((current) => ({
      ...current,
      modalities: current.modalities.includes(key)
        ? current.modalities.filter((item) => item !== key)
        : [...current.modalities, key],
    }))
  }

  function applyBatchDefaults(): void {
    const selected = new Set(
      rows.filter((row) => row.selected).map((row) => row.id),
    )
    const vendorName =
      vendorChoice === CREATE_VENDOR ? newVendorName.trim() : vendorChoice
    const batch: WizardDefaults = {
      ...defaults,
      vendorName,
      parameters: parseParameterLines(parameterText),
      tags: tagsText
        .split(',')
        .map((tag) => tag.trim())
        .filter((tag) => tag !== ''),
    }
    setRows((current) =>
      current.map((row) => {
        if (!selected.has(row.id)) return row
        return applyDefaults([row], batch, emptyOnly)[0] ?? row
      }),
    )
    setAppliedCount(selected.size)
  }

  function backToForm(): void {
    if (fillChannelModels) {
      onApplyModels(
        rows.filter((row) => row.selected).map((row) => row.id),
      )
    }
    onBack()
  }

  const backButton = (
    <Button variant="secondary" onClick={onBack}>
      {t('common.channels.profileWizard.back')}
    </Button>
  )

  if (probeState === 'error') {
    return (
      <div
        role="alert"
        className="flex min-h-[240px] flex-col items-center justify-center gap-3 text-center"
      >
        <p className="text-body text-danger">{probeError}</p>
        <div className="flex flex-wrap items-center justify-center gap-2">
          <Button
            variant="secondary"
            onClick={() => setNonce((value) => value + 1)}
          >
            {t('common.channels.error.retry')}
          </Button>
          {backButton}
        </div>
      </div>
    )
  }

  if (probeState === 'loading' || !catalogSettled) {
    return (
      <div className="flex min-h-[240px] items-center justify-center gap-2 text-body text-ink-2">
        <span className="size-4 animate-spin rounded-full border-2 border-border-default border-t-brand" />
        {t('common.channels.profileWizard.loading')}
      </div>
    )
  }

  if (entries.length === 0) {
    return (
      <div className="flex min-h-[240px] flex-col items-center justify-center gap-3 text-center">
        <p className="text-body text-ink-3">
          {t('common.channels.profileWizard.empty')}
        </p>
        {backButton}
      </div>
    )
  }

  // 条目已就绪、行尚未建好（目录查询刚结束）时仍显示加载态，避免先闪一帧空表。
  if (rows.length === 0) {
    return (
      <div className="flex min-h-[240px] items-center justify-center gap-2 text-body text-ink-2">
        <span className="size-4 animate-spin rounded-full border-2 border-border-default border-t-brand" />
        {t('common.channels.profileWizard.loading')}
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-4">
      <nav
        aria-label={t('common.channels.profileWizard.title')}
        className="grid grid-cols-4 border-b border-border-hairline"
      >
        {([1, 2, 3, 4] as const).map((index) => {
          const enabled = index <= 2
          const current = step === index
          return (
            <button
              key={index}
              type="button"
              disabled={!enabled}
              aria-current={current ? 'step' : undefined}
              onClick={() => {
                if (index === 1 || index === 2) setStep(index)
              }}
              className={[
                'flex items-center gap-2 border-b-2 px-1 py-2.5 text-left text-meta transition-colors',
                current
                  ? 'border-brand font-semibold text-ink-1'
                  : 'border-transparent text-ink-3',
                enabled ? 'cursor-pointer' : 'cursor-default',
              ].join(' ')}
            >
              <span
                className={[
                  'grid size-5 shrink-0 place-items-center rounded-full border text-badge tabular-nums',
                  current
                    ? 'border-brand bg-brand text-on-brand'
                    : 'border-border-default bg-surface-card text-ink-3',
                ].join(' ')}
              >
                {index}
              </span>
              <span className="truncate">
                {t(`common.channels.profileWizard.step${index}`)}
              </span>
            </button>
          )
        })}
      </nav>

      {step === 1 ? (
        <section className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <input
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder={t('common.channels.profileWizard.search')}
              aria-label={t('common.channels.profileWizard.search')}
              className={`${INPUT_CLASSES} min-w-0 flex-1`}
            />
            <Button variant="ghost" onClick={selectVisible}>
              {t('common.channels.profileWizard.selectAll')}
            </Button>
            <Button variant="ghost" onClick={clearAll}>
              {t('common.channels.profileWizard.clearAll')}
            </Button>
          </div>

          <p className="text-meta text-ink-3">
            {t('common.channels.profileWizard.selectedCount', {
              count: selectedCount,
            })}
          </p>

          <div className="overflow-x-auto rounded-lg border border-border-hairline">
            <table className="w-full min-w-[620px] border-collapse text-meta">
              <thead>
                <tr className="bg-surface-subtle text-left text-badge font-medium text-ink-3">
                  <th scope="col" className="w-9 px-2 py-2">
                    <span className="sr-only">
                      {t('common.channels.profileWizard.selectAll')}
                    </span>
                  </th>
                  <th scope="col" className="px-2 py-2">
                    {t('common.channels.profileWizard.model')}
                  </th>
                  <th scope="col" className="px-2 py-2">
                    {t('common.channels.profileWizard.displayName')}
                  </th>
                  <th scope="col" className="px-2 py-2">
                    {t('common.channels.profileWizard.profileState')}
                  </th>
                  <th scope="col" className="px-2 py-2">
                    {t('common.channels.profileWizard.context')}
                  </th>
                  <th scope="col" className="px-2 py-2">
                    {t('common.channels.profileWizard.maxOutput')}
                  </th>
                  <th scope="col" className="px-2 py-2">
                    {t('common.channels.profileWizard.factsSource')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {filteredRows.length === 0 ? (
                  <tr>
                    <td colSpan={7} className="px-4 py-8 text-center text-ink-3">
                      {t('common.channels.fetchModels.empty')}
                    </td>
                  </tr>
                ) : (
                  filteredRows.map((row) => {
                    const state = profileStateOf(row)
                    const displayName = displayNameOf(row)
                    return (
                      <tr
                        key={row.id}
                        className="border-t border-border-hairline hover:bg-surface-hover"
                      >
                        <td className="px-2 py-2">
                          <input
                            type="checkbox"
                            checked={row.selected}
                            onChange={() => toggleRow(row.id)}
                            aria-label={row.id}
                            className="size-4 rounded border-border-default"
                          />
                        </td>
                        <td className="px-2 py-2">
                          <span className="font-mono text-ink-1">{row.id}</span>
                        </td>
                        <td className="max-w-[160px] truncate px-2 py-2 text-ink-2">
                          {displayName === '' ? '—' : displayName}
                        </td>
                        <td className="px-2 py-2">
                          <span
                            className={`inline-flex h-[21px] items-center rounded border px-1.5 text-badge whitespace-nowrap ${PROFILE_BADGE_CLASSES[state]}`}
                          >
                            {t(`common.channels.profileWizard.state.${state}`)}
                          </span>
                        </td>
                        <td className="px-2 py-2 tabular-nums text-ink-2">
                          {row.contextLength === '' ? '—' : row.contextLength}
                        </td>
                        <td className="px-2 py-2 tabular-nums text-ink-2">
                          {row.maxOutput === '' ? '—' : row.maxOutput}
                        </td>
                        <td className="px-2 py-2">
                          <span className="flex flex-wrap gap-1">
                            {factKindsOf(row).map((kind) => (
                              <span
                                key={kind}
                                className={`inline-flex h-[21px] items-center gap-1 rounded border px-1.5 text-badge whitespace-nowrap ${FACT_CHIP_CLASSES[kind]}`}
                              >
                                <span
                                  aria-hidden="true"
                                  className="h-3 w-0.5 rounded-full bg-current"
                                />
                                {t(`common.channels.profileWizard.fact.${kind}`)}
                              </span>
                            ))}
                          </span>
                        </td>
                      </tr>
                    )
                  })
                )}
              </tbody>
            </table>
          </div>
        </section>
      ) : (
        <section className="flex flex-col gap-4">
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="flex flex-col gap-1.5 sm:col-span-2">
              <label
                htmlFor="profile-wizard-vendor"
                className="text-meta text-ink-2"
              >
                {t('common.channels.profileWizard.vendor')}
              </label>
              <select
                id="profile-wizard-vendor"
                value={vendorChoice}
                onChange={(event) => setVendorChoice(event.target.value)}
                className={INPUT_CLASSES}
              >
                <option value="">
                  {t('common.channels.form.none')}
                </option>
                {vendorDirectory.names.length === 0 ? null : (
                  <optgroup
                    label={t(
                      'common.channels.profileWizard.vendorExisting',
                    )}
                  >
                    {vendorDirectory.names.map((name) => (
                      <option key={name} value={name}>
                        {name}
                      </option>
                    ))}
                  </optgroup>
                )}
                <option value={CREATE_VENDOR}>
                  {t('common.channels.profileWizard.vendorCreate')}
                </option>
              </select>
              {vendorChoice === CREATE_VENDOR ? (
                <input
                  value={newVendorName}
                  onChange={(event) => setNewVendorName(event.target.value)}
                  aria-label={t('common.channels.profileWizard.vendor')}
                  placeholder={t('common.channels.profileWizard.vendor')}
                  className={INPUT_CLASSES}
                />
              ) : null}
            </div>

            <fieldset className="flex flex-col gap-2">
              <legend className="text-meta text-ink-2">
                {t('common.channels.profileWizard.endpoints')}
              </legend>
              <div className="flex flex-wrap gap-1.5">
                {ENDPOINT_KEYS.map((key) => (
                  <ChipToggle
                    key={key}
                    checked={defaults.endpoints.includes(key)}
                    label={key}
                    onToggle={() => toggleEndpoint(key)}
                  />
                ))}
              </div>
            </fieldset>

            <fieldset className="flex flex-col gap-2">
              <legend className="text-meta text-ink-2">
                {t('common.channels.profileWizard.capabilities')}
              </legend>
              <div className="flex flex-wrap gap-1.5">
                {CAPABILITY_KEYS.map((key) => (
                  <ChipToggle
                    key={key}
                    checked={defaults.capabilities.includes(key)}
                    label={t(`admin.models.capability_${key}`)}
                    onToggle={() => toggleCapability(key)}
                  />
                ))}
              </div>
            </fieldset>

            <fieldset className="flex flex-col gap-2">
              <legend className="text-meta text-ink-2">
                {t('common.channels.profileWizard.modalities')}
              </legend>
              <div className="flex flex-wrap gap-1.5">
                {MODALITY_KEYS.map((key) => (
                  <ChipToggle
                    key={key}
                    checked={defaults.modalities.includes(key)}
                    label={t(`admin.models.modality_${key}`)}
                    onToggle={() => toggleModality(key)}
                  />
                ))}
              </div>
            </fieldset>

            <div className="flex flex-col gap-1.5">
              <label
                htmlFor="profile-wizard-cutoff"
                className="text-meta text-ink-2"
              >
                {t('common.channels.profileWizard.knowledgeCutoff')}
              </label>
              <input
                id="profile-wizard-cutoff"
                type="month"
                value={defaults.knowledgeCutoff}
                onChange={(event) =>
                  setDefaults((current) => ({
                    ...current,
                    knowledgeCutoff: event.target.value,
                  }))
                }
                className={INPUT_CLASSES}
              />
            </div>

            <div className="flex flex-col gap-1.5 sm:col-span-2">
              <label
                htmlFor="profile-wizard-tags"
                className="text-meta text-ink-2"
              >
                {t('common.channels.profileWizard.tags')}
              </label>
              <input
                id="profile-wizard-tags"
                value={tagsText}
                onChange={(event) => setTagsText(event.target.value)}
                className={INPUT_CLASSES}
              />
            </div>

            <div className="flex flex-col gap-1.5 sm:col-span-2">
              <label
                htmlFor="profile-wizard-parameters"
                className="text-meta text-ink-2"
              >
                {t('common.channels.profileWizard.parameters')}
              </label>
              <textarea
                id="profile-wizard-parameters"
                value={parameterText}
                onChange={(event) => setParameterText(event.target.value)}
                placeholder={t(
                  'common.channels.profileWizard.parametersHint',
                )}
                className="min-h-20 w-full rounded-lg border border-border-default bg-surface-card px-3 py-2 font-mono text-body text-ink-1 placeholder:font-sans placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              />
              <p className="text-meta text-ink-3">
                {t('common.channels.profileWizard.parametersHint')}
              </p>
            </div>
          </div>

          <div className="flex flex-wrap items-center gap-3 border-t border-border-hairline pt-3">
            <label className="flex items-center gap-2 text-meta text-ink-1">
              <input
                type="checkbox"
                checked={emptyOnly}
                onChange={(event) => setEmptyOnly(event.target.checked)}
                className="size-4 rounded border-border-default"
              />
              {t('common.channels.profileWizard.emptyOnly')}
            </label>
            <Button
              variant="secondary"
              onClick={applyBatchDefaults}
              disabled={selectedCount === 0}
            >
              {t('common.channels.profileWizard.applyDefaults')}
            </Button>
            {appliedCount === null ? null : (
              <span aria-live="polite" className="text-meta text-ink-3">
                {t('common.channels.profileWizard.defaultsApplied', {
                  count: appliedCount,
                })}
              </span>
            )}
          </div>
        </section>
      )}

      <div className="flex flex-wrap items-center gap-3 border-t border-border-hairline pt-4">
        <label className="flex items-center gap-2 text-meta text-ink-2">
          <input
            type="checkbox"
            checked={fillChannelModels}
            onChange={(event) => setFillChannelModels(event.target.checked)}
            className="size-4 rounded border-border-default"
          />
          {t('common.channels.profileWizard.fillChannelModels')}
        </label>
        <span className="text-meta text-ink-3">
          {t('common.channels.profileWizard.selectedCount', {
            count: selectedCount,
          })}
        </span>
        <div className="ml-auto flex flex-wrap gap-2">
          {step === 1 ? (
            <Button variant="secondary" onClick={backToForm}>
              {t('common.channels.profileWizard.back')}
            </Button>
          ) : (
            <Button variant="secondary" onClick={() => setStep(1)}>
              {t('common.channels.profileWizard.prev')}
            </Button>
          )}
          {step === 1 ? (
            <Button
              onClick={() => setStep(2)}
              disabled={selectedCount === 0}
            >
              {t('common.channels.profileWizard.next')}
            </Button>
          ) : null}
        </div>
      </div>
    </div>
  )
}

interface ChipToggleProps {
  checked: boolean
  label: string
  onToggle: () => void
}

function ChipToggle({ checked, label, onToggle }: ChipToggleProps) {
  return (
    <label
      className={[
        'inline-flex h-7 cursor-pointer items-center gap-1.5 rounded-full border px-2.5 text-badge transition-colors focus-within:shadow-[var(--focus-ring)]',
        checked
          ? 'border-brand-line bg-brand-subtle font-medium text-brand'
          : 'border-border-default bg-surface-card text-ink-2 hover:bg-surface-hover',
      ].join(' ')}
    >
      <input
        type="checkbox"
        checked={checked}
        onChange={onToggle}
        className="sr-only"
      />
      {label}
    </label>
  )
}
