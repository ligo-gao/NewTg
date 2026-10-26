/**
 * 补全模型档案向导：第 1 步「选模型」与第 2 步「批量默认值」。
 *
 * 步骤 3（逐行确认）、步骤 4（结果）与写库循环属于 Task 6，这里只呈现四个
 * 步骤的导航骨架，3/4 步保持禁用。上游事实只来自探测响应；能力 / 模态 /
 * 参数 / 知识截止 *只能* 由管理员在向导里填写，绝不从模型名推断。
 *
 * 本文件只保留容器职责：探测、行状态、步骤导航与页脚。目录读取在
 * `model-profile-wizard-queries.ts`，两步的界面各在自己的 step 文件里。
 */
import { useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'

import { ApiError } from '@/api/errors'
import type { components } from '@/api/schema'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'

import { fetchChannelModels } from './api'
import {
  applyDefaults,
  buildRows,
  parseParameterLines,
  type ParameterRow,
  type WizardDefaults,
  type WizardRow,
} from './model-profile-wizard-logic'
import {
  loadModelCatalog,
  loadVendorDirectory,
  type VendorDirectory,
} from './model-profile-wizard-queries'
import {
  CREATE_VENDOR,
  StepDefaults,
  emptyDefaultsForm,
  type DefaultsFormState,
} from './model-profile-wizard-step-defaults'
import { StepModels } from './model-profile-wizard-step-models'
import type { FetchChannelModelEntry, FetchChannelModelsInput } from './types'

type ModelMeta = components['schemas']['ModelMeta']

const MODEL_CATALOG_QUERY_KEY = [
  'admin',
  'channel-model-profile-wizard',
  'models',
] as const
const VENDOR_QUERY_KEY = [
  'admin',
  'channel-model-profile-wizard',
  'vendors',
] as const

const EMPTY_CATALOG: Map<string, ModelMeta> = new Map()

const EMPTY_VENDOR_DIRECTORY: VendorDirectory = {
  ids: new Map(),
  nameById: new Map(),
  names: [],
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
type WizardStep = 1 | 2

function sameStrings(
  left: readonly string[],
  right: readonly string[],
): boolean {
  return (
    left.length === right.length &&
    left.every((item, index) => item === right[index])
  )
}

function sameParameters(
  left: readonly ParameterRow[],
  right: readonly ParameterRow[],
): boolean {
  return (
    left.length === right.length &&
    left.every((row, index) => {
      const other = right[index]
      return (
        other !== undefined &&
        row.name === other.name &&
        row.type === other.type &&
        row.description === other.description &&
        row.defaultValue === other.defaultValue
      )
    })
  )
}

/** 批量默认值是否真的改写了这一行（决定 `appliedCount` 是否出现）。 */
function rowWasWritten(current: WizardRow, next: WizardRow): boolean {
  return (
    current.vendorName !== next.vendorName ||
    current.knowledgeCutoff !== next.knowledgeCutoff ||
    !sameStrings(current.endpoints, next.endpoints) ||
    !sameStrings(current.capabilities, next.capabilities) ||
    !sameStrings(current.modalities, next.modalities) ||
    !sameStrings(current.tags, next.tags) ||
    !sameParameters(current.parameters, next.parameters)
  )
}

/** 下拉选中的值（供应商 id / 新建哨兵）→ 写入体需要的供应商名。 */
function vendorNameOf(
  form: DefaultsFormState,
  directory: VendorDirectory,
): string {
  if (form.vendorChoice === '') return ''
  if (form.vendorChoice === CREATE_VENDOR) return form.newVendorName.trim()
  return directory.nameById.get(Number(form.vendorChoice)) ?? ''
}

function WizardLoading({ label }: { label: string }) {
  return (
    <div
      role="status"
      className="flex min-h-[240px] items-center justify-center gap-2 text-body text-ink-2"
    >
      <span
        aria-hidden="true"
        className="size-4 animate-spin rounded-full border-2 border-border-default border-t-brand"
      />
      {label}
    </div>
  )
}

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
  // null 表示非 ApiError：渲染时再取「读取失败」的本地化文案，避免把 `t`
  // 塞进探测 effect 的依赖里（语言切换不应重发上游请求）。
  const [probeError, setProbeError] = useState<string | null>(null)
  const [entries, setEntries] = useState<FetchChannelModelEntry[]>([])
  const [rows, setRows] = useState<WizardRow[]>([])
  const [nonce, setNonce] = useState(0)
  const [query, setQuery] = useState('')
  const [fillChannelModels, setFillChannelModels] = useState(true)
  const [form, setForm] = useState<DefaultsFormState>(emptyDefaultsForm)
  const [appliedCount, setAppliedCount] = useState<number | null>(null)
  // 只为每个探测结果建一次行：目录查询的后台重取不应把管理员已套用的
  // 默认值 / 勾选状态冲掉。目录「失败 → 成功」算一次新的建行时机，因此
  // 状态也进入 key，恢复后只重建一次。
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
    setProbeError(null)

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
          error instanceof ApiError ? error.message : null,
        )
        setProbeState('error')
      }
    }

    void load()
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nonce, requestKey])

  const catalogStatus = catalogQuery.isPending
    ? 'pending'
    : catalogQuery.isError
      ? 'error'
      : 'success'
  const catalogSettled = catalogStatus !== 'pending'
  const catalogReady = catalogStatus === 'success'

  // 行由「探测条目 + 既有档案 + 表单当前模型」共同决定，三者都就绪后才建，
  // 否则会把已有档案误标成「待建档」。目录读取失败时先用空目录建行，好让
  // 第 1 步显示错误横幅；恢复成功后（key 里的状态变化）再重建一次。
  useEffect(() => {
    if (probeState !== 'ready' || !catalogSettled) return
    const buildKey = `${nonce}\u0000${channelModelsKey}\u0000${catalogStatus}`
    if (builtKeyRef.current === buildKey) return
    builtKeyRef.current = buildKey
    setRows(
      buildRows(entries, catalogQuery.data ?? EMPTY_CATALOG, channelModels),
    )
    // `channelModels` 以稳定 key 参与依赖，避免父组件内联数组造成重建循环。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [probeState, catalogStatus, entries, catalogQuery.data, channelModelsKey, nonce])

  const catalog = catalogQuery.data ?? EMPTY_CATALOG
  const vendorDirectory = vendorsQuery.data ?? EMPTY_VENDOR_DIRECTORY

  const selectedCount = rows.filter((row) => row.selected).length
  // 目录未成功加载前不放开第 2 步：档案状态还不可信，写库必须失败关闭。
  const stepTwoEnabled = selectedCount > 0 && catalogReady

  function toggleRow(id: string): void {
    setRows((current) =>
      current.map((row) =>
        row.id === id ? { ...row, selected: !row.selected } : row,
      ),
    )
  }

  function selectVisible(ids: string[]): void {
    const visible = new Set(ids)
    setRows((current) =>
      current.map((row) =>
        visible.has(row.id) ? { ...row, selected: true } : row,
      ),
    )
  }

  function clearAll(): void {
    setRows((current) => current.map((row) => ({ ...row, selected: false })))
  }

  function applyBatchDefaults(): void {
    const selected = new Set(
      rows.filter((row) => row.selected).map((row) => row.id),
    )
    const batch: WizardDefaults = {
      ...form.defaults,
      vendorName: vendorNameOf(form, vendorDirectory),
      parameters: parseParameterLines(form.parameterText),
      tags: form.tagsText
        .split(',')
        .map((tag) => tag.trim())
        .filter((tag) => tag !== ''),
    }
    let written = 0
    const nextRows = rows.map((row) => {
      if (!selected.has(row.id)) return row
      const next = applyDefaults([row], batch, form.emptyOnly)[0] ?? row
      if (rowWasWritten(row, next)) written += 1
      return next
    })
    setRows(nextRows)
    setAppliedCount(written === 0 ? null : written)
  }

  /**
   * 写回是**并集**：勾选只表示「要给谁补档案」，不表示「渠道只服务这些
   * 模型」。因此渠道原有但上游没公布的模型永远保留，空选也不会清空列表。
   */
  function backToForm(): void {
    if (fillChannelModels) {
      onApplyModels(
        Array.from(
          new Set([
            ...channelModels,
            ...rows.filter((row) => row.selected).map((row) => row.id),
          ]),
        ),
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
        <p className="text-body text-danger">
          {probeError ?? t('common.channels.profileWizard.loadError')}
        </p>
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
    return <WizardLoading label={t('common.channels.profileWizard.loading')} />
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
    return <WizardLoading label={t('common.channels.profileWizard.loading')} />
  }

  return (
    <div className="flex flex-col gap-4">
      <nav
        aria-label={t('common.channels.profileWizard.title')}
        className="grid grid-cols-4 border-b border-border-hairline"
      >
        {([1, 2, 3, 4] as const).map((index) => {
          const enabled = index === 1 || (index === 2 && stepTwoEnabled)
          const current = step === index
          return (
            <button
              key={index}
              type="button"
              disabled={!enabled}
              aria-current={current ? 'step' : undefined}
              onClick={() => {
                if (enabled) setStep(index)
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
        <StepModels
          rows={rows}
          catalog={catalog}
          query={query}
          onQueryChange={setQuery}
          onToggleRow={toggleRow}
          onSelectVisible={selectVisible}
          onClearAll={clearAll}
          catalogFailed={catalogStatus === 'error'}
          onRetryCatalog={() => {
            void catalogQuery.refetch()
          }}
        />
      ) : (
        <StepDefaults
          value={form}
          onChange={setForm}
          vendorDirectory={vendorDirectory}
          selectedCount={selectedCount}
          appliedCount={appliedCount}
          onApply={applyBatchDefaults}
        />
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
              disabled={!stepTwoEnabled}
            >
              {t('common.channels.profileWizard.next')}
            </Button>
          ) : null}
        </div>
      </div>
    </div>
  )
}
