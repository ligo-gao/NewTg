import { useEffect, useId, useMemo, useState, type ReactNode } from 'react'

import { ApiError } from '@/api/errors'
import {
  fieldErrorsOf,
  type SettingsItem,
  type SettingsSectionResponse,
} from '@/features/admin/settings/api'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert, TextField } from '@/shared/ui/form'
import { StatusBadge } from '@/shared/ui/status-badge'
import { useToastStore } from '@/shared/ui/toast'

import { savePerfSettings, type PerfConfig } from '../api'
import { formatDateTime } from '../utils'

/**
 * 测速节奏四项（perf.schedule.*）的在线表单（Task 17 / adjudications §1–§3）。
 *
 * 值有两个来源：`section.items` 是设置注册表的落库值（可写），
 * `config.schedule` 是 worker 实际生效的节奏（只读）。前者为 null 时输入框
 * 显示注册表默认值，并在页脚说明当前仍由环境变量决定；保存一次后以本页为准。
 *
 * 前端**不做范围校验**：`SettingsItem` 不带 min/max，复制后端上下界只会漂移。
 * 这里只拦「空」与「非 ≥1 整数」，越界交给服务端 422，字段错误经
 * `fieldErrorsOf` 挂回对应输入框。
 */

type Schedule = NonNullable<PerfConfig['schedule']>
type ScheduleKey =
  | 'perf.schedule.enabled'
  | 'perf.schedule.interval_hours'
  | 'perf.schedule.runs'
  | 'perf.schedule.concurrency'

export const SCHEDULE_KEYS: readonly ScheduleKey[] = [
  'perf.schedule.enabled',
  'perf.schedule.interval_hours',
  'perf.schedule.runs',
  'perf.schedule.concurrency',
]

const NUMERIC_KEYS: readonly ScheduleKey[] = [
  'perf.schedule.interval_hours',
  'perf.schedule.runs',
  'perf.schedule.concurrency',
]

/** 四个键在环境变量层的家族名；页脚说明按整条节奏措辞，不逐键展开。 */
const SCHEDULE_ENV_CODE = 'PERF_SCHEDULE_*'

/** 开关错误文案的 id；开关与文案互相引用，读屏才能把错误读出来。 */
const ENABLED_ERROR_ID = 'probe-schedule-enabled-error'

type Draft = Partial<Record<ScheduleKey, string | boolean>>
type FieldErrors = Partial<Record<ScheduleKey, string>>

function isScheduleKey(key: string): key is ScheduleKey {
  return (SCHEDULE_KEYS as readonly string[]).includes(key)
}

/** 传输层错误的可读文案；与设置表单同一口径。 */
function errorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.detail ?? error.title
  return error instanceof Error ? error.message : String(error)
}

function scheduleFallback(
  schedule: Schedule,
  key: ScheduleKey,
): boolean | number {
  switch (key) {
    case 'perf.schedule.enabled':
      return schedule.enabled
    case 'perf.schedule.interval_hours':
      return schedule.interval_hours
    case 'perf.schedule.runs':
      return schedule.runs
    case 'perf.schedule.concurrency':
      return schedule.concurrency
  }
}

/**
 * 输入框的基线值。`item.value === null` 表示键从未落库，此时展示注册表
 * 默认值（adjudications §1）；该项不在 section 里则退回生效节奏。
 */
function baselineValue(
  item: SettingsItem | undefined,
  key: ScheduleKey,
  schedule: Schedule,
): boolean | number | null {
  if (item === undefined) return scheduleFallback(schedule, key)
  const raw =
    item.value !== null && item.value !== undefined ? item.value : item.default
  if (typeof raw === 'boolean') return raw
  if (typeof raw === 'number' && Number.isFinite(raw)) return raw
  if (typeof raw === 'string' && raw.trim() !== '') {
    const parsed = Number(raw)
    if (Number.isFinite(parsed)) return parsed
  }
  return null
}

function sameBoolean(
  item: SettingsItem | undefined,
  schedule: Schedule,
  value: boolean,
): boolean {
  return baselineValue(item, 'perf.schedule.enabled', schedule) === value
}

export interface ProbeScheduleFormProps {
  section: SettingsSectionResponse
  config: PerfConfig
  /** 缺少 `settings:manage` 时整块只读。 */
  canManage: boolean
  /** 保存成功后由页面重拉 config 与 section。 */
  onSaved: () => void
  /**
   * 手动触发一轮测速。走的是既有任务触发端点
   * `POST /api/system/tasks/model_perf_test:trigger`（spec §6.5 第 6 条），
   * 由页面装配并传入；调用方不传就不渲染按钮，绝不伪造一个会失败的入口。
   */
  onRunNow?: () => void
}

export function ProbeScheduleForm({
  section,
  config,
  canManage,
  onSaved,
  onRunNow,
}: ProbeScheduleFormProps) {
  const { t, locale } = useI18n()
  const push = useToastStore((state) => state.push)
  const titleId = useId()

  const [draft, setDraft] = useState<Draft>({})
  const [fieldErrors, setFieldErrors] = useState<FieldErrors>({})
  const [serverError, setServerError] = useState<string | null>(null)
  const [conflict, setConflict] = useState(false)
  const [saving, setSaving] = useState(false)
  /**
   * 乐观锁快照：页面每 60s 重拉 section，若每次保存都用最新的
   * `section.updated_at`，409 后重试必然成功，锁等于不存在。所以在「首次变脏」
   * 那一刻快照，保存成功或用户显式放弃才换。
   *
   * 用对象哨兵而不是 `string | null`：`updated_at` 本身可以是 null（全新部署的
   * perf section 没有种子行），`lockToken === null` 就无法区分「尚未快照」与
   * 「快照值就是 null」——后者会让轮询期间别人保存产生的值被误写进令牌。
   */
  const [lock, setLock] = useState<{ token: string | null } | null>(null)

  const itemByKey = useMemo(() => {
    const map = new Map<string, SettingsItem>()
    for (const item of section.items) map.set(item.key, item)
    return map
  }, [section.items])

  const schedule = config.schedule

  const changed = schedule === null ? [] : changedEntries(schedule)
  // 非法草稿也算「有未保存改动」：保存按钮必须可点，本地校验才能把错误
  // 挂到输入框上；否则用户会看到一个不可点击的按钮而没有任何解释。
  const dirty = changed.length > 0

  // 首次变脏时快照 `updated_at`：轮询随后刷新的 section 不影响本次保存的
  // 乐观锁令牌，否则 409 后重试必然成功、锁形同虚设。
  useEffect(() => {
    if (dirty && lock === null) {
      setLock({ token: section.updated_at })
    } else if (!dirty && lock !== null) {
      setLock(null)
    }
  }, [dirty, lock, section.updated_at])

  if (schedule === null) {
    return (
      <section
        data-testid="probe-schedule-form"
        aria-labelledby={titleId}
        className="rounded-xl border border-border-default bg-surface-card p-5"
      >
        <Header titleId={titleId} title={t('admin.performance.settings.probe_title')}>
          <StatusBadge variant="neutral-outline">
            {t('admin.performance.settings.readonly')}
          </StatusBadge>
        </Header>
        <div
          data-testid="probe-schedule-readonly"
          className="mt-4 rounded-lg border border-dashed border-border-default bg-surface-subtle px-4 py-3"
        >
          <p className="text-meta text-ink-2">
            {t('admin.performance.settings.readonly_note')}
          </p>
        </div>
      </section>
    )
  }

  // 闭包不会保留 `schedule === null` 的类型收窄；这里钉住非空引用。
  const effective = schedule
  const disabled = !canManage || saving

  /** 输入框当前文本；草稿优先，其次基线（null → 默认值/生效值）。 */
  function inputText(key: ScheduleKey): string {
    const entry = draft[key]
    if (entry !== undefined) {
      return typeof entry === 'string' ? entry : entry ? 'true' : 'false'
    }
    const baseline = baselineValue(itemByKey.get(key), key, effective)
    return baseline === null ? '' : String(baseline)
  }

  /** 连通首尾的勾选值（开关没有草稿时按基线）。 */
  function switchChecked(): boolean {
    const entry = draft['perf.schedule.enabled']
    if (typeof entry === 'boolean') return entry
    return (
      baselineValue(
        itemByKey.get('perf.schedule.enabled'),
        'perf.schedule.enabled',
        effective,
      ) === true
    )
  }

  function updateDraft(key: ScheduleKey, value: string | boolean): void {
    setDraft((current) => ({ ...current, [key]: value }))
    setFieldErrors((current) => {
      if (!(key in current)) return current
      const next = { ...current }
      delete next[key]
      return next
    })
    setServerError(null)
  }

  /**
   * 与最近一次 GET 的 `item.value` 比较后只保留改动过的键；`null` 表示未落库，
   * 用户一旦编辑就按改动提交（即使数值恰好等于默认值）。
   */
  function changedEntries(
    current: Schedule,
  ): Array<[ScheduleKey, unknown]> {
    const entries: Array<[ScheduleKey, unknown]> = []
    for (const key of SCHEDULE_KEYS) {
      const item = itemByKey.get(key)
      // 项缺失时输入框保持禁用，草稿里不会出现该键。
      if (item === undefined) continue
      const entry = draft[key]
      if (entry === undefined) continue

      if (key === 'perf.schedule.enabled') {
        if (typeof entry !== 'boolean') continue
        if (item.value === null || item.value === undefined) {
          entries.push([key, entry])
        } else if (!sameBoolean(item, current, entry)) {
          entries.push([key, entry])
        }
        continue
      }

      if (typeof entry !== 'string') continue
      const trimmed = entry.trim()
      const parsed = Number(trimmed)
      // 非法输入也进入提交列表，交给本地校验（或服务端）给出字段错误；
      // 静默丢弃会让「保存」看起来成功但什么都没写。
      if (trimmed === '' || !Number.isFinite(parsed)) {
        entries.push([key, entry])
        continue
      }
      if (item.value === null || item.value === undefined) {
        entries.push([key, parsed])
        continue
      }
      if (parsed !== baselineValue(item, key, current)) {
        entries.push([key, parsed])
      }
    }
    return entries
  }

  function validate(changedKeys: readonly ScheduleKey[]): FieldErrors {
    const errors: FieldErrors = {}
    for (const key of changedKeys) {
      if (!NUMERIC_KEYS.includes(key)) continue
      const entry = draft[key]
      if (typeof entry !== 'string') continue
      const trimmed = entry.trim()
      if (trimmed === '' || !/^\d+$/.test(trimmed) || Number(trimmed) < 1) {
        errors[key] = t('admin.performance.errors.invalid_value')
      }
    }
    return errors
  }

  async function save(): Promise<void> {
    if (!canManage || saving) return
    const entries = changedEntries(effective)
    if (entries.length === 0) return

    const errors = validate(entries.map(([key]) => key))
    if (Object.keys(errors).length > 0) {
      setFieldErrors(errors)
      const first = entries.find(([key]) => errors[key] !== undefined)
      if (first !== undefined) {
        document.getElementById(`probe-schedule-${first[0]}`)?.focus()
      }
      return
    }

    const values: Record<string, unknown> = {}
    for (const [key, value] of entries) values[key] = value

    setSaving(true)
    setServerError(null)
    setConflict(false)
    try {
      await savePerfSettings(
        values,
        lock === null ? section.updated_at : lock.token,
      )
      setDraft({})
      setFieldErrors({})
      setLock(null)
      push({
        tone: 'success',
        message: t('admin.performance.settings.saved'),
      })
      onSaved()
    } catch (error) {
      if (error instanceof ApiError && error.kind === 'conflict') {
        // 409：保留用户输入**与旧令牌**，不自动覆盖、不自动重拉替换——由用户
        // 自己决定刷新还是重新输入（adjudications §2）。旧令牌保留意味着再次
        // 保存仍会 409，不会静默覆盖别人的改动。
        setConflict(true)
        setServerError(t('admin.performance.settings.conflict'))
      } else {
        const fieldErrorList = fieldErrorsOf(error)
        if (fieldErrorList.length > 0) {
          const next: FieldErrors = {}
          for (const entry of fieldErrorList) {
            if (!isScheduleKey(entry.field)) continue
            next[entry.field] =
              entry.message !== ''
                ? entry.message
                : t('admin.performance.errors.invalid_value')
          }
          // 服务端报的是别的字段（或键名变体）时不要静默吞掉：整块提示仍
          // 比「点了保存什么都没发生」可诊断。
          if (Object.keys(next).length > 0) setFieldErrors(next)
          else setServerError(errorMessage(error))
        } else {
          setServerError(errorMessage(error))
        }
      }
    } finally {
      setSaving(false)
    }
  }

  /**
   * 显式放弃：清掉草稿与旧令牌，让父层重拉 —— 拿到最新的 `updated_at` 与值后，
   * 用户再编辑保存时用的就是刷新后的版本。
   */
  function discardAndReload(): void {
    setDraft({})
    setFieldErrors({})
    setServerError(null)
    setConflict(false)
    setLock(null)
    onSaved()
  }

  const nextRun = !schedule.enabled
    ? t('admin.performance.settings.next_run_disabled')
    : schedule.next_run_at === null
      ? t('admin.performance.settings.next_run_unknown')
      : t('admin.performance.settings.next_run', {
          time: formatDateTime(schedule.next_run_at, locale),
        })

  const sourceKind = sourceKindOf(schedule, itemByKey)
  const source = SOURCE_COPY[sourceKind]
  const sourceNote = t(source.note, {
    code: SCHEDULE_ENV_CODE,
    hours: schedule.interval_hours,
    times: schedule.legacy_times || '—',
    runs: schedule.runs,
    concurrency: schedule.concurrency,
  })

  // 409 的提示与「放弃」按钮由 conflict 驱动：用户在冲突后继续编辑时提示不能
  // 跟着 serverError 一起消失，否则必须再失败一次才看得到出口。
  const alertMessage = conflict
    ? t('admin.performance.settings.conflict')
    : serverError

  return (
    <section
      data-testid="probe-schedule-form"
      aria-labelledby={titleId}
      className="rounded-xl border border-border-default bg-surface-card p-5"
    >
      <Header titleId={titleId} title={t('admin.performance.settings.probe_title')}>
        {canManage ? null : (
          <StatusBadge variant="neutral-outline">
            {t('admin.performance.settings.readonly')}
          </StatusBadge>
        )}
      </Header>
      <p className="mt-1 max-w-[72ch] text-meta text-ink-3">
        {t('admin.performance.settings.probe_note')}
      </p>

      {alertMessage !== null ? (
        <div className="mt-4 space-y-2">
          <FormAlert tone="danger">{alertMessage}</FormAlert>
          {conflict ? (
            <Button variant="secondary" onClick={discardAndReload}>
              {t('admin.performance.settings.conflict_reload')}
            </Button>
          ) : null}
        </div>
      ) : null}

      <div className="mt-4 space-y-4">
        <div className="flex items-center justify-between gap-6">
          <div className="flex items-center gap-2">
            <label
              htmlFor="probe-schedule-perf.schedule.enabled"
              className="text-body font-medium text-ink-1"
            >
              {t('admin.performance.settings.enabled')}
            </label>
            {missingReadonly(itemByKey, 'perf.schedule.enabled') ? (
              <span className="text-meta text-ink-3">
                {t('admin.performance.settings.readonly')}
              </span>
            ) : null}
          </div>
          <div className="flex items-center gap-3">
            <StatusBadge variant={switchChecked() ? 'success' : 'neutral'}>
              {switchChecked()
                ? t('admin.performance.settings.enabled')
                : t('admin.performance.settings.disabled')}
            </StatusBadge>
            <input
              id="probe-schedule-perf.schedule.enabled"
              type="checkbox"
              role="switch"
              aria-checked={switchChecked()}
              aria-invalid={fieldErrors['perf.schedule.enabled'] !== undefined}
              aria-describedby={
                fieldErrors['perf.schedule.enabled'] === undefined
                  ? undefined
                  : ENABLED_ERROR_ID
              }
              disabled={
                disabled || !itemByKey.has('perf.schedule.enabled')
              }
              checked={switchChecked()}
              onChange={(event) =>
                updateDraft('perf.schedule.enabled', event.target.checked)
              }
              className="h-5 w-9 appearance-none rounded-full border border-border-default bg-surface-subtle transition-colors checked:border-brand checked:bg-brand disabled:cursor-not-allowed disabled:opacity-50"
            />
          </div>
        </div>
        {fieldErrors['perf.schedule.enabled'] !== undefined ? (
          <p id={ENABLED_ERROR_ID} className="text-meta text-danger">
            {fieldErrors['perf.schedule.enabled']}
          </p>
        ) : null}

        <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
          <TextField
            id="probe-schedule-perf.schedule.interval_hours"
            type="number"
            inputMode="numeric"
            step={1}
            label={t('admin.performance.settings.interval')}
            labelAction={
              <span className="text-meta text-ink-3">
                {t('admin.performance.settings.interval_unit')}
              </span>
            }
            value={inputText('perf.schedule.interval_hours')}
            disabled={disabled || !itemByKey.has('perf.schedule.interval_hours')}
            hint={
              itemByKey.has('perf.schedule.interval_hours')
                ? undefined
                : t('admin.performance.settings.readonly')
            }
            error={fieldErrors['perf.schedule.interval_hours']}
            onChange={(event) =>
              updateDraft('perf.schedule.interval_hours', event.target.value)
            }
          />
          <TextField
            id="probe-schedule-perf.schedule.runs"
            type="number"
            inputMode="numeric"
            step={1}
            label={t('admin.performance.settings.runs')}
            value={inputText('perf.schedule.runs')}
            disabled={disabled || !itemByKey.has('perf.schedule.runs')}
            hint={
              itemByKey.has('perf.schedule.runs')
                ? undefined
                : t('admin.performance.settings.readonly')
            }
            error={fieldErrors['perf.schedule.runs']}
            onChange={(event) =>
              updateDraft('perf.schedule.runs', event.target.value)
            }
          />
          <TextField
            id="probe-schedule-perf.schedule.concurrency"
            type="number"
            inputMode="numeric"
            step={1}
            label={t('admin.performance.settings.concurrency')}
            value={inputText('perf.schedule.concurrency')}
            disabled={
              disabled || !itemByKey.has('perf.schedule.concurrency')
            }
            hint={
              itemByKey.has('perf.schedule.concurrency')
                ? undefined
                : t('admin.performance.settings.readonly')
            }
            error={fieldErrors['perf.schedule.concurrency']}
            onChange={(event) =>
              updateDraft('perf.schedule.concurrency', event.target.value)
            }
          />
        </div>
      </div>

      <footer className="mt-5 space-y-2 border-t border-border-hairline pt-4">
        {canManage ? (
          <div className="flex flex-wrap items-center justify-end gap-2">
            {onRunNow === undefined ? null : (
              <Button variant="secondary" onClick={onRunNow} disabled={saving}>
                {t('admin.performance.settings.run_now')}
              </Button>
            )}
            <Button onClick={() => void save()} disabled={!dirty || saving}>
              {t('admin.performance.settings.save')}
            </Button>
          </div>
        ) : null}

        <p data-testid="probe-next-run" className="text-meta text-ink-3">
          {nextRun}
        </p>
        <p
          data-testid="probe-source"
          className="flex flex-wrap items-center gap-2 text-meta text-ink-3"
        >
          <StatusBadge variant={source.variant}>{t(source.badge)}</StatusBadge>
          <span>{sourceNote}</span>
        </p>
      </footer>
    </section>
  )
}

const SOURCE_COPY = {
  env: {
    badge: 'admin.performance.settings.source_env_badge',
    note: 'admin.performance.settings.source_env_note',
    variant: 'neutral',
  },
  settings: {
    badge: 'admin.performance.settings.source_settings_badge',
    note: 'admin.performance.settings.source_settings_note',
    variant: 'brand',
  },
  mixed: {
    badge: 'admin.performance.settings.source_mixed_badge',
    note: 'admin.performance.settings.source_mixed_note',
    variant: 'warning',
  },
} as const

/**
 * `item.value === null`（含输入框显示默认值时）走环境变量说明；否则按
 * `config.schedule.source` 三态映射。项缺失（部署未装调度器）不算 env——
 * 那是「不可写」，不是「由环境变量生效」。
 */
function sourceKindOf(
  schedule: Schedule,
  itemByKey: ReadonlyMap<string, SettingsItem>,
): keyof typeof SOURCE_COPY {
  const anyUnsaved = SCHEDULE_KEYS.some((key) => {
    const item = itemByKey.get(key)
    return item !== undefined && item.value === null
  })
  if (schedule.source === 'env' || anyUnsaved) return 'env'
  if (schedule.source === 'mixed') return 'mixed'
  return 'settings'
}

function missingReadonly(
  itemByKey: ReadonlyMap<string, SettingsItem>,
  key: ScheduleKey,
): boolean {
  return !itemByKey.has(key)
}

function Header({
  titleId,
  title,
  children,
}: {
  titleId: string
  title: string
  children?: ReactNode
}) {
  return (
    <header className="flex items-start justify-between gap-4">
      <h3
        id={titleId}
        className="text-card-title font-semibold leading-[22px] text-ink-1"
      >
        {title}
      </h3>
      {children}
    </header>
  )
}
