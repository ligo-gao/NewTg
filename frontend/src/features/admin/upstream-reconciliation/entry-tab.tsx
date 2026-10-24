import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useMemo, useRef, useState } from 'react'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert, TextField } from '@/shared/ui/form'

import {
  createIdempotencyKey,
  listAccounts,
  listSnapshots,
  upsertSnapshots,
  type UpstreamSnapshot,
  type UpstreamSnapshotWrite,
} from './api'

/** 结果页与录入页共享同一读取前缀；保存成功后按前缀失效。 */
const SNAPSHOTS_QUERY_KEY = ['upstream-snapshots'] as const
const ACCOUNTS_QUERY_KEY = ['upstream-accounts'] as const

/** 五个计数维度，顺序即表格列顺序。 */
const COUNT_FIELDS = [
  'input_cache_hit_tokens',
  'input_cache_miss_tokens',
  'cache_write_tokens',
  'output_tokens',
  'requests',
] as const

type CountField = (typeof COUNT_FIELDS)[number]

const COUNT_LABEL_KEYS: Record<CountField, string> = {
  input_cache_hit_tokens: 'admin.upstream_reconciliation.entry.col_hit',
  input_cache_miss_tokens: 'admin.upstream_reconciliation.entry.col_miss',
  cache_write_tokens: 'admin.upstream_reconciliation.entry.col_cache_write',
  output_tokens: 'admin.upstream_reconciliation.entry.col_output',
  requests: 'admin.upstream_reconciliation.entry.col_requests',
}

type EntryValidationError =
  | 'error_model'
  | 'error_negative'
  | 'error_integer'
  | 'error_amount'

const VALIDATION_KEYS: Record<EntryValidationError, string> = {
  error_model: 'admin.upstream_reconciliation.entry.error_model',
  error_negative: 'admin.upstream_reconciliation.entry.error_negative',
  error_integer: 'admin.upstream_reconciliation.entry.error_integer',
  error_amount: 'admin.upstream_reconciliation.entry.error_amount',
}

/**
 * 一行编辑状态。计数保持 `number | null`：空输入是 NULL（上游未报告该维度），
 * `0` 是真实的零，两者在写入契约里语义不同，不能合并。
 * 金额以元为单位的原始文本保存，只在往返边界上换算成整数分。
 */
interface EntryRow {
  upstream_model: string
  input_cache_hit_tokens: number | null
  input_cache_miss_tokens: number | null
  cache_write_tokens: number | null
  output_tokens: number | null
  requests: number | null
  amount_yuan: string
}

type BuildResult =
  | { items: UpstreamSnapshotWrite[] }
  | { error: EntryValidationError }

const CELL_INPUT_CLASSES =
  'h-9 w-28 rounded-lg border border-border-default bg-surface-card px-2 text-right font-mono text-meta text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50'

const MODEL_CELL_CLASSES = 'font-mono text-meta text-ink-1'

/** 可编辑的计数维度补丁；模型名固定于新增时，不在此类型内。 */
type CountPatch = Partial<Record<CountField, number | null>>

function emptyRow(): EntryRow {
  return {
    upstream_model: '',
    input_cache_hit_tokens: null,
    input_cache_miss_tokens: null,
    cache_write_tokens: null,
    output_tokens: null,
    requests: null,
    amount_yuan: '',
  }
}

function fromSnapshot(snapshot: UpstreamSnapshot): EntryRow {
  return {
    upstream_model: snapshot.upstream_model,
    input_cache_hit_tokens: snapshot.input_cache_hit_tokens ?? null,
    input_cache_miss_tokens: snapshot.input_cache_miss_tokens ?? null,
    cache_write_tokens: snapshot.cache_write_tokens ?? null,
    output_tokens: snapshot.output_tokens ?? null,
    requests: snapshot.requests ?? null,
    amount_yuan: formatYuan(snapshot.amount_minor),
  }
}

/**
 * 分 -> 元只用于**展示**：`toFixed` 是格式化，不参与任何金额运算；反向换算
 * 一律 `Math.round(yuan * 100)`，避免浮点误差进入写入载荷。
 */
function formatYuan(minor: number | null | undefined): string {
  if (minor === null || minor === undefined) return ''
  return (minor / 100).toFixed(2)
}

/** 元 -> 整数分。`null` = 留空；`undefined` = 非法（负数或超出安全整数）。 */
function parseMinor(raw: string): number | null | undefined {
  const text = raw.trim()
  if (text === '') return null
  const yuan = Number(text)
  if (!Number.isFinite(yuan) || yuan < 0) return undefined
  const minor = Math.round(yuan * 100)
  return Number.isSafeInteger(minor) ? minor : undefined
}

function parseCount(raw: string): number | null {
  const text = raw.trim()
  if (text === '') return null
  const value = Number(text)
  return Number.isFinite(value) ? value : null
}

function isRowDirty(row: EntryRow, base: EntryRow | undefined): boolean {
  if (base === undefined) {
    // 新增行：全空视为尚未开始填写，不进入脏计数，也不提交。
    return (
      row.upstream_model.trim() !== '' ||
      row.amount_yuan.trim() !== '' ||
      COUNT_FIELDS.some((field) => row[field] !== null)
    )
  }
  return (
    row.upstream_model !== base.upstream_model ||
    row.amount_yuan !== base.amount_yuan ||
    COUNT_FIELDS.some((field) => row[field] !== base[field])
  )
}

/**
 * 只把脏行编入 PUT 载荷。空行跳过；负数、小数计数与非法金额在调用 API 前
 * 拦下（后端也会校验，但前端不该为一个明显错误发一次写请求）。
 */
function buildItems(
  rows: readonly EntryRow[],
  baseline: readonly EntryRow[],
  usageDate: string,
): BuildResult {
  const items: UpstreamSnapshotWrite[] = []
  for (let index = 0; index < rows.length; index += 1) {
    const row = rows[index]
    if (row === undefined) continue
    if (!isRowDirty(row, baseline[index])) continue

    const model = row.upstream_model.trim()
    if (model === '') return { error: 'error_model' }

    for (const field of COUNT_FIELDS) {
      const value = row[field]
      if (value === null) continue
      if (value < 0) return { error: 'error_negative' }
      if (!Number.isInteger(value)) return { error: 'error_integer' }
    }

    const amountMinor = parseMinor(row.amount_yuan)
    if (amountMinor === undefined) {
      const yuan = Number(row.amount_yuan)
      return {
        error: Number.isFinite(yuan) && yuan < 0 ? 'error_negative' : 'error_amount',
      }
    }

    items.push({
      usage_date: usageDate,
      upstream_model: model,
      input_cache_hit_tokens: row.input_cache_hit_tokens,
      input_cache_miss_tokens: row.input_cache_miss_tokens,
      cache_write_tokens: row.cache_write_tokens,
      output_tokens: row.output_tokens,
      requests: row.requests,
      amount_minor: amountMinor,
    })
  }
  return { items }
}

function todayISO(): string {
  const now = new Date()
  const local = new Date(now.getTime() - now.getTimezoneOffset() * 60_000)
  return local.toISOString().slice(0, 10)
}

function problemMessage(error: unknown): string {
  if (error instanceof ApiError) return error.detail ?? error.title
  return error instanceof Error ? error.message : String(error)
}

/**
 * 快照录入 Tab（任务 13）。
 *
 * 管理员选中上游账号与自然日后加载当日快照，按上游模型逐行填写五个计数与
 * 账单金额。编辑保留 `null`/`0` 区分，仅脏行进入 PUT；金额在往返边界上
 * 以整数分传输。服务端仍是最终校验方，这里只做「明显错误不发请求」的前置
 * 拦截。
 */
export function EntryTab({ canManage }: { canManage: boolean }) {
  const { t } = useI18n()
  const queryClient = useQueryClient()

  const [accountId, setAccountId] = useState<number | null>(null)
  const [date, setDate] = useState(todayISO)
  const [loaded, setLoaded] = useState<{
    accountId: number
    date: string
  } | null>(null)
  const [rows, setRows] = useState<EntryRow[]>([])
  const [baseline, setBaseline] = useState<EntryRow[]>([])
  const [newModel, setNewModel] = useState('')
  const [validationError, setValidationError] = useState<string | null>(null)
  const [addError, setAddError] = useState<string | null>(null)
  const [savedCount, setSavedCount] = useState<number | null>(null)

  const accountsQuery = useQuery({
    queryKey: ACCOUNTS_QUERY_KEY,
    queryFn: ({ signal }) => listAccounts({}, signal),
    retry: false,
    staleTime: 30_000,
  })
  const accounts = accountsQuery.data?.items ?? []

  // 默认选中第一个账号；管理员仍可在下拉里切换。
  useEffect(() => {
    if (accountId !== null) return
    const first = accountsQuery.data?.items[0]
    if (first !== undefined) setAccountId(first.id)
  }, [accountId, accountsQuery.data])

  const snapshotsQuery = useQuery({
    queryKey: [
      ...SNAPSHOTS_QUERY_KEY,
      loaded?.accountId ?? null,
      loaded?.date ?? null,
    ],
    queryFn: ({ signal }) => {
      if (loaded === null) {
        throw new Error('snapshot query ran without a loaded date')
      }
      return listSnapshots(
        { accountId: loaded.accountId, from: loaded.date, to: loaded.date },
        signal,
      )
    },
    enabled: loaded !== null,
    retry: false,
    // 加载按钮显式重取；后台窗口聚焦不得清掉未保存的编辑。
    staleTime: Infinity,
    refetchOnWindowFocus: false,
  })

  const dirtyFlags = useMemo(
    () => rows.map((row, index) => isRowDirty(row, baseline[index])),
    [rows, baseline],
  )
  const dirtyCount = dirtyFlags.filter(Boolean).length
  // 效果只在 `snapshots` 身份变化时运行；用 ref 读取最新脏计数，避免把
  // dirtyCount 加入依赖后在保存成功（脏计数归零）时用旧响应覆盖新基线。
  const dirtyCountRef = useRef(0)
  dirtyCountRef.current = dirtyCount

  const snapshots = snapshotsQuery.data?.items
  useEffect(() => {
    if (snapshots === undefined) return
    // 未保存的编辑优先于任何后台重取：否则一次迟到的响应会静默清空表格。
    if (dirtyCountRef.current > 0) return
    const next = snapshots.map(fromSnapshot)
    setRows(next)
    setBaseline(next)
    setValidationError(null)
  }, [snapshots])

  const saveMutation = useMutation({
    mutationFn: (payload: {
      accountId: number
      items: UpstreamSnapshotWrite[]
    }) =>
      upsertSnapshots(
        { account_id: payload.accountId, items: payload.items },
        createIdempotencyKey(),
      ),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: SNAPSHOTS_QUERY_KEY })
    },
  })

  /** 有未保存编辑时，丢弃前须经管理员确认；拒绝则不做任何状态变更。 */
  function confirmDiscard(): boolean {
    if (dirtyCount === 0) return true
    return globalThis.confirm(
      t('admin.upstream_reconciliation.entry.confirm_discard'),
    )
  }

  async function load(): Promise<void> {
    if (accountId === null || date === '') return
    const discarding = dirtyCount > 0
    if (!confirmDiscard()) return
    setValidationError(null)
    setSavedCount(null)
    if (loaded !== null && loaded.accountId === accountId && loaded.date === date) {
      // 确认丢弃后必须先从当前缓存恢复干净基线：重取若与上次结构相同，
      // React Query 会复用同一 data 引用，同步效果便不会再次运行。
      if (discarding) {
        const current = snapshotsQuery.data?.items
        if (current !== undefined) {
          const restored = current.map(fromSnapshot)
          setRows(restored)
          setBaseline(restored)
          setValidationError(null)
        } else {
          setRows([])
          setBaseline([])
        }
      }
      await snapshotsQuery.refetch()
      return
    }
    // 更换账号/日期后旧行不再属于当前视图；先清空，避免把上一日的编辑
    // 误存到新日期下。
    setRows([])
    setBaseline([])
    setLoaded({ accountId, date })
  }

  /** 工具栏选择已变化：丢弃当前加载结果，等待再次点击加载。 */
  function clearLoaded(): void {
    setLoaded(null)
    setRows([])
    setBaseline([])
    setValidationError(null)
    setSavedCount(null)
  }

  /**
   * 只接受计数与金额的补丁：模型名在新增时固定，避免一次「重命名」变成
   * 新行 upsert 而把旧行（连同旧计数）留在服务端。
   */
  function updateRow(
    index: number,
    patch: CountPatch & { amount_yuan?: string },
  ): void {
    setRows((prev) =>
      prev.map((row, i) => (i === index ? { ...row, ...patch } : row)),
    )
    setValidationError(null)
    setSavedCount(null)
  }

  function updateCount(index: number, field: CountField, raw: string): void {
    const patch: CountPatch = {}
    patch[field] = parseCount(raw)
    updateRow(index, patch)
  }

  function addRow(): void {
    const model = newModel.trim()
    if (model === '') return
    // 同一日期同一模型是 upsert 主键；重复添加只会让表格出现两行同键数据。
    // 保留输入文本，让管理员改掉或删掉重复的名字即可。
    if (rows.some((row) => row.upstream_model.trim() === model)) {
      setAddError(
        t('admin.upstream_reconciliation.entry.error_duplicate_model'),
      )
      return
    }
    setRows((prev) => [...prev, { ...emptyRow(), upstream_model: model }])
    setNewModel('')
    setAddError(null)
    setValidationError(null)
    setSavedCount(null)
  }

  function reset(): void {
    setRows(baseline.map((row) => ({ ...row })))
    setValidationError(null)
    setSavedCount(null)
  }

  async function save(): Promise<void> {
    if (loaded === null) return
    const built = buildItems(rows, baseline, loaded.date)
    if ('error' in built) {
      setValidationError(t(VALIDATION_KEYS[built.error]))
      setSavedCount(null)
      return
    }
    if (built.items.length === 0) return

    setValidationError(null)
    try {
      const result = await saveMutation.mutateAsync({
        accountId: loaded.accountId,
        items: built.items,
      })
      // 立即以「已保存」为基线，脏标记无需等待重取即可清除。
      setBaseline(rows)
      setSavedCount(result.updated)
    } catch {
      // 失败文案由 `saveMutation.error` 渲染（RFC 7807 detail）。
    }
  }

  return (
    <div className="flex flex-col gap-3" data-testid="entry-tab">
      {!canManage ? (
        <FormAlert tone="info">
          {t('admin.upstream_reconciliation.entry.read_only')}
        </FormAlert>
      ) : null}

      <div className="flex flex-wrap items-end gap-3">
        <label className="flex flex-col gap-1 text-meta text-ink-2">
          {t('admin.upstream_reconciliation.entry.account')}
          <select
            aria-label={t('admin.upstream_reconciliation.entry.account')}
            value={accountId === null ? '' : String(accountId)}
            onChange={(event) => {
              const next = Number(event.target.value)
              if (!confirmDiscard()) return
              setAccountId(Number.isInteger(next) && next > 0 ? next : null)
              setAddError(null)
              clearLoaded()
            }}
            disabled={accountsQuery.isPending}
            className="h-10 min-w-48 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1 disabled:cursor-not-allowed disabled:opacity-50"
          >
            <option value="">
              {t('admin.upstream_reconciliation.entry.select_account')}
            </option>
            {accounts.map((account) => (
              <option key={account.id} value={String(account.id)}>
                {account.name}
              </option>
            ))}
          </select>
        </label>

        <div className="w-44">
          <TextField
            label={t('admin.upstream_reconciliation.entry.date')}
            type="date"
            value={date}
            onChange={(event) => {
              if (!confirmDiscard()) return
              setDate(event.target.value)
              setAddError(null)
              clearLoaded()
            }}
          />
        </div>

        <Button
          variant="secondary"
          className="h-10"
          disabled={accountId === null || date === ''}
          onClick={() => void load()}
        >
          {t('admin.upstream_reconciliation.entry.load')}
        </Button>

        {canManage ? (
          <>
            <div className="w-60">
              <TextField
                label={t('admin.upstream_reconciliation.entry.add_model')}
                value={newModel}
                placeholder={t(
                  'admin.upstream_reconciliation.entry.model_placeholder',
                )}
                disabled={accountId === null || date === ''}
                onChange={(event) => {
                  setNewModel(event.target.value)
                  setAddError(null)
                }}
                onKeyDown={(event) => {
                  if (event.key !== 'Enter') return
                  event.preventDefault()
                  addRow()
                }}
              />
            </div>
            <Button
              variant="secondary"
              className="h-10"
              disabled={
                newModel.trim() === '' || accountId === null || date === ''
              }
              onClick={addRow}
            >
              {t('admin.upstream_reconciliation.entry.add')}
            </Button>
          </>
        ) : null}
      </div>

      {addError !== null ? (
        <FormAlert tone="danger">{addError}</FormAlert>
      ) : null}

      {accountsQuery.isError ? (
        <FormAlert tone="danger">
          {problemMessage(accountsQuery.error)}
        </FormAlert>
      ) : null}
      {accountsQuery.isSuccess && accounts.length === 0 ? (
        <p className="text-body text-ink-3">
          {t('admin.upstream_reconciliation.entry.no_accounts')}
        </p>
      ) : null}

      {loaded === null ? (
        <FormAlert tone="info">
          {t('admin.upstream_reconciliation.entry.load_hint')}
        </FormAlert>
      ) : null}
      {snapshotsQuery.isError ? (
        <FormAlert tone="danger">
          {problemMessage(snapshotsQuery.error)}
        </FormAlert>
      ) : null}

      {validationError !== null ? (
        <FormAlert tone="danger">{validationError}</FormAlert>
      ) : null}
      {saveMutation.isError ? (
        <FormAlert tone="danger">
          {problemMessage(saveMutation.error)}
        </FormAlert>
      ) : null}
      {savedCount !== null ? (
        <FormAlert tone="success">
          {t('admin.upstream_reconciliation.entry.saved', { count: savedCount })}
        </FormAlert>
      ) : null}

      {loaded !== null && snapshotsQuery.isPending ? (
        <p className="text-body text-ink-3">
          {t('admin.upstream_reconciliation.entry.loading')}
        </p>
      ) : null}
      {loaded !== null && snapshotsQuery.isSuccess && rows.length === 0 ? (
        <p className="text-body text-ink-3">
          {t('admin.upstream_reconciliation.entry.empty')}
        </p>
      ) : null}

      {rows.length > 0 ? (
        <div className="overflow-x-auto rounded-xl border border-border-default">
          <table className="w-full min-w-[1080px] text-left text-body">
            <thead className="text-meta text-ink-3">
              <tr>
                <th className="px-2 py-1.5 font-medium">
                  {t('admin.upstream_reconciliation.entry.col_model')}
                </th>
                {COUNT_FIELDS.map((field) => (
                  <th key={field} className="px-2 py-1.5 text-right font-medium">
                    {t(COUNT_LABEL_KEYS[field])}
                  </th>
                ))}
                <th className="px-2 py-1.5 text-right font-medium">
                  {t('admin.upstream_reconciliation.entry.col_amount')}
                </th>
                <th className="w-24 px-2 py-1.5 font-medium">
                  {t('admin.upstream_reconciliation.entry.col_state')}
                </th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row, index) => (
                <tr
                  key={`${row.upstream_model}-${String(index)}`}
                  data-testid="entry-model-row"
                  className="border-t border-border-hairline"
                >
                  <td className="px-2 py-1.5">
                    <span className={MODEL_CELL_CLASSES}>
                      {row.upstream_model}
                    </span>
                  </td>
                  {COUNT_FIELDS.map((field) => (
                    <td key={field} className="px-2 py-1.5 text-right">
                      <input
                        type="number"
                        inputMode="numeric"
                        min={0}
                        step={1}
                        aria-label={t(COUNT_LABEL_KEYS[field])}
                        value={row[field] ?? ''}
                        disabled={!canManage || saveMutation.isPending}
                        onChange={(event) => {
                          updateCount(index, field, event.target.value)
                        }}
                        className={CELL_INPUT_CLASSES}
                      />
                    </td>
                  ))}
                  <td className="px-2 py-1.5 text-right">
                    <input
                      type="text"
                      inputMode="decimal"
                      aria-label={t(
                        'admin.upstream_reconciliation.entry.col_amount',
                      )}
                      value={row.amount_yuan}
                      disabled={!canManage || saveMutation.isPending}
                      onChange={(event) => {
                        updateRow(index, { amount_yuan: event.target.value })
                      }}
                      className={CELL_INPUT_CLASSES}
                    />
                  </td>
                  <td className="px-2 py-1.5">
                    {dirtyFlags[index] === true ? (
                      <span className="text-meta text-ink-3">
                        {t('admin.upstream_reconciliation.entry.unsaved')}
                      </span>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      {canManage ? (
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-meta text-ink-3">
            {t('admin.upstream_reconciliation.entry.dirty_count', {
              count: dirtyCount,
            })}
          </p>
          <div className="flex gap-2">
            <Button
              variant="secondary"
              onClick={reset}
              disabled={dirtyCount === 0 || saveMutation.isPending}
            >
              {t('admin.upstream_reconciliation.entry.reset')}
            </Button>
            <Button
              onClick={() => void save()}
              disabled={
                dirtyCount === 0 ||
                loaded === null ||
                saveMutation.isPending
              }
            >
              {saveMutation.isPending
                ? t('admin.upstream_reconciliation.entry.saving')
                : t('admin.upstream_reconciliation.entry.save')}
            </Button>
          </div>
        </div>
      ) : null}
    </div>
  )
}
