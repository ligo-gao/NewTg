/**
 * 补全模型档案向导：四个步骤的容器。
 *
 * 第 1 步「选模型」、第 2 步「批量默认值」、第 3 步「逐行确认」、第 4 步
 * 「结果」。上游事实只来自探测响应；能力 / 模态 / 参数 / 知识截止 *只能*
 * 由管理员在向导里填写，绝不从模型名推断。
 *
 * 本文件只保留容器职责：探测、行状态、目录读取、写库循环与步骤导航。
 * 各步的界面在自己的 step 文件里。写库只有这一份入口：`for...of` 串行、
 * 逐行 try/catch，一行失败不影响其它行。
 */
import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'

import { apiClient } from '@/api/client'
import { ApiError } from '@/api/errors'
import { requireData } from '@/api/result'
import { useI18n, type TranslateFn } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'

import { fetchChannelModels } from './api'
import {
  MAX_WIZARD_ROWS,
  applyDefaults,
  buildCreateBody,
  buildRows,
  buildUpdateBody,
  parseParameterLines,
  rowActionOf,
  rowIsValid,
  type ParameterRow,
  type RowResult,
  type WizardDefaults,
  type WizardRow,
} from './model-profile-wizard-logic'
import {
  loadModelCatalog,
  loadVendorDirectory,
  MODEL_CATALOG_LIMIT,
  type ModelCatalog,
  type VendorDirectory,
} from './model-profile-wizard-queries'
import {
  CREATE_VENDOR,
  StepDefaults,
  emptyDefaultsForm,
  type DefaultsFormState,
} from './model-profile-wizard-step-defaults'
import { StepConfirm } from './model-profile-wizard-step-confirm'
import { StepModels } from './model-profile-wizard-step-models'
import { ModelProfileWizardResult } from './model-profile-wizard-result'
import type { FetchChannelModelEntry, FetchChannelModelsInput } from './types'

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

const EMPTY_CATALOG: ModelCatalog = {
  byName: new Map(),
  total: 0,
  loaded: 0,
  truncated: false,
}

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
type WizardStep = 1 | 2 | 3 | 4

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

/**
 * 写库行的动作（提交时由 `rowActionOf` 决定，skip 不走到这里）。
 *
 * 409 的语义按动作区分：创建撞的是「同名档案已存在」，不是乐观锁；更新才是
 * 「被他人修改」。前者刷新目录后确认即可，重试原地必再冲突，所以不可重试。
 */
type WriteAction = 'create' | 'update'

/**
 * 确定性失败的 4xx 重试必然再失败（典型：Vertex 风格 id 含 `@`，行内又改不了
 * id），只有超时、限流与服务端错误值得重试。
 */
function isRetryableStatus(status: number): boolean {
  return status === 408 || status === 429 || status >= 500
}

interface WriteFailure {
  message: string
  retryable: boolean
  /** 创建 409：档案可能已被别人建好，刷新目录让管理员确认。 */
  refetchCatalog?: boolean
}

function writeFailureOf(
  error: unknown,
  action: WriteAction,
  t: TranslateFn,
): WriteFailure {
  if (error instanceof ApiError) {
    if (error.status === 409) {
      return action === 'create'
        ? {
            message: t('common.channels.profileWizard.conflictCreate'),
            retryable: false,
            refetchCatalog: true,
          }
        : {
            message: t('common.channels.profileWizard.conflict'),
            retryable: true,
          }
    }
    const base = error.detail ?? error.title
    const message =
      error.fields !== undefined && error.fields.length > 0
        ? `${base} (${error.fields.join(', ')})`
        : base
    return { message, retryable: isRetryableStatus(error.status) }
  }
  // 网络错误 / 超时没有状态码：重试是合理出路。
  return {
    message: error instanceof Error ? error.message : String(error),
    retryable: true,
  }
}

interface VendorFailure {
  message: string
  retryable: boolean
}

/**
 * 建商失败原因。409 是「同名供应商已经存在」——不是乐观锁冲突，并且刷新
 * 目录后重试就能命中，所以单独给一句文案且仍可重试；其余 4xx 不可重试。
 */
function vendorFailureOf(
  error: unknown,
  name: string,
  t: TranslateFn,
): VendorFailure {
  if (error instanceof ApiError && error.status === 409) {
    return {
      message: t('common.channels.profileWizard.vendorExists', { name }),
      retryable: true,
    }
  }
  return {
    message: t('common.channels.profileWizard.vendorCreateFailed', {
      name,
      message: writeFailureOf(error, 'create', t).message,
    }),
    retryable:
      error instanceof ApiError ? isRetryableStatus(error.status) : true,
  }
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
  const queryClient = useQueryClient()
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
  // 提交进度与逐行结果。`null` 进度 = 未在写库；`null` 结果 = 还没提交过。
  const [progress, setProgress] = useState<{
    done: number
    total: number
  } | null>(null)
  const [results, setResults] = useState<RowResult[] | null>(null)
  // 只为每个探测结果建一次行：目录查询的后台重取不应把管理员已套用的
  // 默认值 / 勾选状态 / 第 3 步的编辑冲掉。目录「从未成功 → 成功」算一次
  // 新的建行时机（恢复后只重建一次）；进入过成功之后再失败**不重建**。
  const builtKeyRef = useRef<string | null>(null)
  const catalogReadyOnceRef = useRef(false)
  // 本轮已建成的供应商 id（归一化名 → id）。结果页的「只重试失败行」可能
  // 发生在目录刷新完成之前，重试必须复用它们，否则会再建一次撞唯一索引。
  const createdVendorIdsRef = useRef<Map<string, number>>(new Map())

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
  // 一旦成功拿到过目录就再也不会「不可用」：成功后的后台重取失败只影响
  // 新鲜度，不应把已解锁的流程重新锁上，也不应重建行。
  const catalogReady =
    catalogStatus === 'success' ||
    (catalogStatus === 'error' && catalogReadyOnceRef.current)
  const catalogFailed = catalogStatus === 'error' && !catalogReady

  // 行由「探测条目 + 既有档案 + 表单当前模型」共同决定，三者都就绪后才建，
  // 否则会把已有档案误标成「待建档」。目录读取失败（从未成功）时先用空目录
  // 建行，好让第 1 步显示错误横幅；恢复成功后只重建一次。
  useEffect(() => {
    if (probeState !== 'ready' || !catalogSettled) return
    if (catalogStatus === 'success') catalogReadyOnceRef.current = true
    // 成功过之后再失败沿用 `success` 相位，key 不变 ⇒ 不重建（否则会冲掉
    // 第 2 步默认值与第 3 步编辑）。
    const phase =
      catalogStatus === 'error' && catalogReadyOnceRef.current
        ? 'success'
        : catalogStatus
    const buildKey = `${nonce}\u0000${channelModelsKey}\u0000${phase}`
    if (builtKeyRef.current === buildKey) return
    builtKeyRef.current = buildKey
    const catalogData = catalogQuery.data ?? EMPTY_CATALOG
    setRows(
      buildRows(entries, catalogData.byName, channelModels, {
        catalogTruncated: catalogData.truncated,
      }),
    )
    // `channelModels` 以稳定 key 参与依赖，避免父组件内联数组造成重建循环。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [probeState, catalogStatus, entries, catalogQuery.data, channelModelsKey, nonce])

  const catalogData = catalogQuery.data ?? EMPTY_CATALOG
  const catalog = catalogData.byName
  const catalogTruncated = catalogData.truncated
  const vendorDirectory = vendorsQuery.data ?? EMPTY_VENDOR_DIRECTORY
  // 唯一允许创建的供应商：第 2 步显式选择「新建供应商」后填的名字。
  // 探测建议值（vendor_suggestion）不在这条路径上，绝不隐式建商。
  const explicitVendorName =
    form.vendorChoice === CREATE_VENDOR ? form.newVendorName.trim() : ''

  const selectedCount = rows.filter((row) => row.selected).length
  const selectedRows = rows.filter((row) => row.selected)
  // 真正会写库的行数：未勾「覆盖」的既有档案与锁定档案都 skip，不能把
  // 纯勾选数写进按钮（全选 137 行可能只更新 3 个）。
  const writableCount = selectedRows.filter(
    (row) => rowActionOf(row, catalog.get(row.id)) !== 'skip',
  ).length
  const submitting = progress !== null
  const tooMany = selectedCount > MAX_WIZARD_ROWS
  // 目录未成功加载前不放开第 2 步：档案状态还不可信，写库必须失败关闭。
  const stepTwoEnabled = selectedCount > 0 && catalogReady

  function updateRow(id: string, patch: Partial<WizardRow>): void {
    setRows((current) =>
      current.map((row) => (row.id === id ? { ...row, ...patch } : row)),
    )
  }

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
    // 第 2 步显式选择的供应商不受「只填空白字段」限制：供应商是操作者的
    // 明确选择，而不是可被探测建议值顶掉的默认值。否则带 vendor_suggestion
    // 的行会保留建议值，显式填写的新建名永远不会被使用（供应商也就不会创建）。
    // 名字为空时不写入，避免把建议值无意义地清空。
    const forceVendor = form.vendorChoice !== '' && batch.vendorName !== ''
    let written = 0
    const nextRows = rows.map((row) => {
      if (!selected.has(row.id)) return row
      const applied = applyDefaults([row], batch, form.emptyOnly)[0] ?? row
      const next = forceVendor ? { ...applied, vendorName: batch.vendorName } : applied
      if (rowWasWritten(row, next)) written += 1
      return next
    })
    setRows(nextRows)
    setAppliedCount(written === 0 ? null : written)
  }

  /**
   * 结果页「返回渠道表单」：写回是**并集**——勾选只表示「要给谁补档案」，
   * 不表示「渠道只服务这些模型」。因此渠道原有但上游没公布的模型永远保留，
   * 空选也不会清空列表。
   *
   * 只有结果页走这里；第 1 步的「返回」只调 `onBack()`（见 `backToForm`），
   * 管理员还没看过确认页，不应仅因默认勾选就把模型并集写回 `supported_models`。
   */
  function applyModelsAndBack(): void {
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

  /** 第 1 步 / 探测错误与空态的「返回」：不改渠道模型列表。 */
  function backToForm(): void {
    onBack()
  }

  function invalidateCatalog(): void {
    // 不 await：结果页要立刻出现，刷新在后台完成即可。除向导自己的目录外，
    // 也沿用 admin 模型页的失效范围，让管理台其它入口看到刚写入的档案与
    // 新建的供应商。
    void queryClient.invalidateQueries({ queryKey: MODEL_CATALOG_QUERY_KEY })
    void queryClient.invalidateQueries({ queryKey: VENDOR_QUERY_KEY })
    void queryClient.invalidateQueries({ queryKey: ['admin-models'] })
    void queryClient.invalidateQueries({ queryKey: ['admin-vendors'] })
    void queryClient.invalidateQueries({ queryKey: ['admin-models-missing'] })
    void queryClient.invalidateQueries({ queryKey: ['admin-vendor-options'] })
  }

  /**
   * 串行写库。供应商先解析：
   *
   * - 本地目录命中（或本轮刚建成的缓存）→ 直接用 id；
   * - 归一化后等于第 2 步显式填写的新建名 → 创建（同名只试一次）；
   * - 其余不在目录里的名字（典型是探测建议值）→ **不创建**，该行
   *   `vendor_id` 传 null（更新时沿用既有值）。
   *
   * 再逐行写档案；单行失败只记该行，不中断其它行。
   */
  async function writeRows(targets: WizardRow[]): Promise<Map<string, RowResult>> {
    const output = new Map<string, RowResult>()
    const vendorIds = new Map<string, number>()
    const vendorErrors = new Map<string, VendorFailure>()
    const explicitKey = explicitVendorName.toLowerCase()

    // 只给「真的会写库且通过校验」的行解析供应商：跳过 / 非法行不应产生
    // 供应商这类副作用。
    const writable = targets.filter(
      (row) =>
        rowActionOf(row, catalog.get(row.id)) !== 'skip' && rowIsValid(row),
    )
    for (const row of writable) {
      const name = row.vendorName.trim()
      if (name === '') continue
      const key = name.toLowerCase()
      if (vendorIds.has(key) || vendorErrors.has(key)) continue
      const known =
        vendorDirectory.ids.get(key) ?? createdVendorIdsRef.current.get(key)
      if (known !== undefined) {
        vendorIds.set(key, known)
        continue
      }
      // 只有第 2 步显式新建的那个名字才允许创建。
      if (explicitKey === '' || key !== explicitKey) continue
      try {
        const created = requireData(
          await apiClient.POST('/api/vendors', {
            // 新建供应商沿用模型表单的默认值：启用、无描述 / 图标。
            body: { name, status: 1 },
          }),
        )
        vendorIds.set(key, created.id)
        createdVendorIdsRef.current.set(key, created.id)
      } catch (error) {
        // 记录失败原因并让同名的后续行复用，避免对同一个名字反复创建。
        vendorErrors.set(key, vendorFailureOf(error, name, t))
        if (error instanceof ApiError && error.status === 409) {
          // 别人刚建好同名供应商：刷新目录，重试时就能按名字命中。
          void vendorsQuery.refetch()
        }
      }
    }

    for (const [index, row] of targets.entries()) {
      setProgress({ done: index + 1, total: targets.length })
      const profile = catalog.get(row.id)
      const action = rowActionOf(row, profile)
      if (action === 'skip') {
        output.set(row.id, {
          id: row.id,
          action: 'skipped',
          retryable: false,
        })
        continue
      }
      if (!rowIsValid(row)) {
        output.set(row.id, {
          id: row.id,
          action: 'failed',
          message: t('common.channels.profileWizard.rowInvalid'),
          retryable: false,
        })
        continue
      }

      const name = row.vendorName.trim()
      const key = name.toLowerCase()
      const vendorError = name === '' ? undefined : vendorErrors.get(key)
      if (vendorError !== undefined) {
        output.set(row.id, {
          id: row.id,
          action: 'failed',
          message: vendorError.message,
          retryable: vendorError.retryable,
        })
        continue
      }
      const vendorId = name === '' ? null : (vendorIds.get(key) ?? null)

      try {
        if (profile === undefined) {
          await apiClient.POST('/api/models', {
            body: buildCreateBody(row, vendorId),
          })
          output.set(row.id, {
            id: row.id,
            action: 'created',
            retryable: false,
          })
        } else {
          await apiClient.PUT('/api/models/{id}', {
            params: { path: { id: profile.id } },
            body: buildUpdateBody(row, profile, vendorId),
          })
          output.set(row.id, {
            id: row.id,
            action: 'updated',
            retryable: false,
          })
        }
      } catch (error) {
        const failure = writeFailureOf(error, action, t)
        // 创建 409 说明档案可能刚被别人建好：刷新目录，结果页的提示会让
        // 管理员确认，而不是拿「刷新后重试仍冲突」的死路。
        if (failure.refetchCatalog === true) void catalogQuery.refetch()
        output.set(row.id, {
          id: row.id,
          action: 'failed',
          message: failure.message,
          retryable: failure.retryable,
        })
      }
    }
    return output
  }

  function mergeResults(
    targets: WizardRow[],
    written: Map<string, RowResult>,
  ): RowResult[] {
    return targets.map(
      (row) =>
        written.get(row.id) ?? {
          id: row.id,
          action: 'failed',
          message: t('common.channels.profileWizard.rowInvalid'),
          retryable: false,
        },
    )
  }

  async function submitAll(): Promise<void> {
    if (submitting || tooMany || writableCount === 0) return
    const targets = selectedRows
    setResults(null)
    setProgress({ done: 0, total: targets.length })
    try {
      const written = await writeRows(targets)
      setResults(mergeResults(targets, written))
      setStep(4)
      invalidateCatalog()
    } finally {
      setProgress(null)
    }
  }

  async function retryFailed(): Promise<void> {
    if (results === null || submitting) return
    const retryable = new Set(
      results
        .filter((result) => result.action === 'failed' && result.retryable)
        .map((result) => result.id),
    )
    const targets = selectedRows.filter((row) => retryable.has(row.id))
    if (targets.length === 0) return
    setProgress({ done: 0, total: targets.length })
    try {
      const written = await writeRows(targets)
      setResults((current) =>
        current === null
          ? null
          : current.map((result) => written.get(result.id) ?? result),
      )
      invalidateCatalog()
    } finally {
      setProgress(null)
    }
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
          const enabled = !submitting
            ? index === 1 ||
              ((index === 2 || index === 3) && stepTwoEnabled) ||
              (index === 4 && results !== null)
            : false
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
          catalogFailed={catalogFailed}
          catalogTruncated={catalogTruncated}
          catalogLimit={MODEL_CATALOG_LIMIT}
          onRetryCatalog={() => {
            void catalogQuery.refetch()
          }}
        />
      ) : step === 2 ? (
        <StepDefaults
          value={form}
          onChange={setForm}
          vendorDirectory={vendorDirectory}
          selectedCount={selectedCount}
          appliedCount={appliedCount}
          onApply={applyBatchDefaults}
        />
      ) : step === 3 ? (
        <StepConfirm
          rows={selectedRows}
          catalog={catalog}
          vendorDirectory={vendorDirectory}
          newVendorName={explicitVendorName}
          tooMany={tooMany}
          onUpdateRow={updateRow}
        />
      ) : (
        <ModelProfileWizardResult
          results={results ?? []}
          busy={submitting}
          onRetryFailed={() => {
            void retryFailed()
          }}
          onBackToForm={applyModelsAndBack}
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
        {progress === null ? null : (
          <span aria-live="polite" className="text-meta text-ink-3">
            {t('common.channels.profileWizard.submitting', {
              done: progress.done,
              total: progress.total,
            })}
          </span>
        )}
        {step === 3 && writableCount === 0 ? (
          <span role="status" className="text-meta text-warning">
            {t('common.channels.profileWizard.nothingToWrite')}
          </span>
        ) : null}
        <div className="ml-auto flex flex-wrap gap-2">
          {step === 1 ? (
            <Button variant="secondary" onClick={backToForm}>
              {t('common.channels.profileWizard.back')}
            </Button>
          ) : step === 2 || step === 3 ? (
            <Button
              variant="secondary"
              onClick={() => setStep(step === 2 ? 1 : 2)}
              disabled={submitting}
            >
              {t('common.channels.profileWizard.prev')}
            </Button>
          ) : null}
          {step === 1 ? (
            <Button onClick={() => setStep(2)} disabled={!stepTwoEnabled}>
              {t('common.channels.profileWizard.next')}
            </Button>
          ) : step === 2 ? (
            <Button onClick={() => setStep(3)} disabled={!stepTwoEnabled}>
              {t('common.channels.profileWizard.next')}
            </Button>
          ) : step === 3 ? (
            <Button
              onClick={() => {
                void submitAll()
              }}
              disabled={submitting || tooMany || writableCount === 0}
            >
              {t('common.channels.profileWizard.submit', {
                count: writableCount,
              })}
            </Button>
          ) : null}
        </div>
      </div>
    </div>
  )
}
