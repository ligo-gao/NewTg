import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { Dialog } from '@/shared/ui/base/dialog'
import { useToastStore } from '@/shared/ui/toast'

import {
  listUserTokens,
  modelLimitsSummary,
  updateTokenModelLimits,
  type Token,
  type UsableGroup,
} from './api'
import { ModelMultiSelect } from './model-multi-select'

const TOKENS_QUERY_KEY = ['model-config', 'tokens'] as const

interface BatchFailure {
  name: string
  reason: string
}

interface BatchOutcome {
  succeeded: number
  failed: BatchFailure[]
}

export interface KeyLimitsCardProps {
  groups: readonly UsableGroup[]
  /** 默认模型卡加载失败不影响本卡；但两者共用可用分组数据。 */
  groupsError: boolean
}

function reasonFor(error: unknown, t: (key: string) => string): string {
  if (error instanceof ApiError) {
    if (error.kind === 'not_found') {
      return t('account.model_config.keys.failure_not_found')
    }
    if (error.kind === 'validation') {
      return t('account.model_config.keys.failure_invalid')
    }
    if (error.kind === 'rate_limited') {
      return t('account.model_config.keys.failure_rate_limited')
    }
    return error.detail ?? error.title
  }
  return t('account.model_config.keys.failure_generic')
}

/**
 * 令牌模型限制卡（§2.15）。
 *
 * 服务端列表是唯一真源：每次写入用响应替换缓存中的行，批量部分失败时再
 * 失效 `['model-config','tokens']` 重新拉取，避免本地拼出一个与服务器不一致
 * 的表格。404 语义是「令牌已不存在且不泄露他人存在性」，因此只刷新、不
 * 在被删除的行上继续编辑。
 */
export function KeyLimitsCard({ groups, groupsError }: KeyLimitsCardProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)

  const tokensQuery = useQuery({
    queryKey: TOKENS_QUERY_KEY,
    queryFn: listUserTokens,
    retry: false,
  })

  const [keyword, setKeyword] = useState('')
  const [selected, setSelected] = useState<Set<number>>(() => new Set())
  const [editingId, setEditingId] = useState<number | null>(null)
  const [editingValue, setEditingValue] = useState<string[]>([])
  const [rowBusy, setRowBusy] = useState(false)
  const [rowNotice, setRowNotice] = useState<Record<number, string>>({})

  const [batchOpen, setBatchOpen] = useState(false)
  const [batchValue, setBatchValue] = useState<string[]>([])
  const [batchBusy, setBatchBusy] = useState(false)
  const [batchOutcome, setBatchOutcome] = useState<BatchOutcome | null>(null)

  const tokens = tokensQuery.data ?? []
  const normalized = keyword.trim().toLowerCase()
  const visibleTokens =
    normalized === ''
      ? tokens
      : tokens.filter((token) => token.name.toLowerCase().includes(normalized))

  function replaceToken(updated: Token): void {
    queryClient.setQueryData<Token[]>(TOKENS_QUERY_KEY, (previous) =>
      previous?.map((token) => (token.id === updated.id ? updated : token)),
    )
  }

  function toggleSelected(id: number, checked: boolean): void {
    setSelected((previous) => {
      const next = new Set(previous)
      if (checked) next.add(id)
      else next.delete(id)
      return next
    })
  }

  function openEditor(token: Token): void {
    setEditingId(token.id)
    setEditingValue([...(token.model_limits ?? [])])
    setRowNotice((previous) => {
      const next = { ...previous }
      delete next[token.id]
      return next
    })
  }

  async function saveRow(token: Token): Promise<void> {
    setRowBusy(true)
    try {
      const updated = await updateTokenModelLimits(token, editingValue)
      replaceToken(updated)
      setEditingId(null)
      push({ tone: 'success', message: t('account.model_config.keys.row_saved') })
    } catch (error) {
      if (error instanceof ApiError && error.kind === 'not_found') {
        const message = t('account.model_config.keys.row_not_found')
        setRowNotice((previous) => ({
          ...previous,
          [token.id]: message,
        }))
        // Toast 独立于行内提示：列表刷新后行可能消失，提示不能跟着瞬间丢。
        push({ tone: 'error', message })
        setEditingId(null)
        await queryClient.invalidateQueries({ queryKey: TOKENS_QUERY_KEY })
      } else if (error instanceof ApiError && error.kind === 'rate_limited') {
        push({ tone: 'error', message: t('account.model_config.rate_limited') })
      } else {
        push({
          tone: 'error',
          message: t('account.model_config.keys.failure_generic'),
          requestId: error instanceof ApiError ? error.request_id : undefined,
        })
      }
    } finally {
      setRowBusy(false)
    }
  }

  async function runBatch(): Promise<void> {
    const targets = tokens.filter((token) => selected.has(token.id))
    if (targets.length === 0) return
    setBatchBusy(true)
    const failed: BatchFailure[] = []
    let succeeded = 0
    let rateLimited = false
    for (let index = 0; index < targets.length; index += 1) {
      const token = targets[index]
      if (token === undefined) continue
      if (rateLimited) {
        failed.push({
          name: token.name,
          reason: t('account.model_config.keys.failure_rate_limited'),
        })
        continue
      }
      try {
        const updated = await updateTokenModelLimits(token, batchValue)
        replaceToken(updated)
        succeeded += 1
      } catch (error) {
        failed.push({ name: token.name, reason: reasonFor(error, t) })
        if (error instanceof ApiError && error.kind === 'rate_limited') {
          rateLimited = true
        }
      }
    }
    setBatchBusy(false)
    setBatchOutcome({ succeeded, failed })
    if (rateLimited) {
      push({ tone: 'error', message: t('account.model_config.rate_limited') })
    }
    if (failed.length === 0) {
      push({
        tone: 'success',
        message: t('account.model_config.keys.batch_success', {
          count: succeeded,
        }),
      })
      setBatchOpen(false)
      setSelected(new Set())
      setBatchValue([])
    }
    // 部分失败时逐条刷新：失败的 404 行会从服务端列表消失，成功行的
    // updated_at 也已推进，下一次 PATCH 必须用新的乐观锁值。
    await queryClient.invalidateQueries({ queryKey: TOKENS_QUERY_KEY })
  }

  const batchCandidateGroups = groups

  return (
    <section
      aria-labelledby="key-limits-title"
      data-testid="key-limits-card"
      className="rounded-xl border border-border-hairline bg-surface-card p-5"
    >
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 id="key-limits-title" className="text-base font-semibold text-ink-1">
            {t('account.model_config.keys.title')}
          </h2>
          <p className="mt-1 text-[13px] text-ink-2">
            {t('account.model_config.keys.description')}
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <label className="sr-only" htmlFor="model-config-key-search">
            {t('account.model_config.keys.search_label')}
          </label>
          <input
            id="model-config-key-search"
            type="search"
            value={keyword}
            onChange={(event) => setKeyword(event.target.value)}
            placeholder={t('account.model_config.keys.search_placeholder')}
            className="h-9 rounded-lg border border-border-default bg-surface-page px-3 text-sm text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          />
          <Button
            variant="secondary"
            disabled={selected.size === 0 || groupsError}
            onClick={() => {
              setBatchOutcome(null)
              setBatchOpen(true)
            }}
          >
            {t('account.model_config.keys.batch_action')}
            {selected.size > 0
              ? ` (${selected.size})`
              : ''}
          </Button>
        </div>
      </div>

      {tokensQuery.isPending ? (
        <div className="mt-4 space-y-2" aria-busy="true">
          {[0, 1, 2, 3, 4].map((row) => (
            <div
              key={row}
              className="h-10 animate-pulse rounded-lg bg-surface-subtle motion-reduce:animate-none"
            />
          ))}
        </div>
      ) : tokensQuery.isError ? (
        <div className="mt-4 flex items-center gap-3 text-[13px] text-danger">
          <span>{t('account.model_config.keys.load_error')}</span>
          <Button variant="secondary" onClick={() => void tokensQuery.refetch()}>
            {t('account.model_config.retry')}
          </Button>
        </div>
      ) : tokens.length === 0 ? (
        <div className="mt-4 flex items-center gap-3 text-[13px] text-ink-2">
          <span>{t('account.model_config.keys.empty')}</span>
          <a
            href="/tokens"
            className="text-brand underline-offset-2 hover:underline"
          >
            {t('account.model_config.keys.empty_action')}
          </a>
        </div>
      ) : (
        <div className="mt-4 overflow-x-auto">
          <table className="w-full min-w-[720px] border-collapse text-[13px]">
            <thead>
              <tr className="border-b border-border-hairline text-left text-xs text-ink-3">
                <th scope="col" className="w-8 px-2 py-2">
                  <span className="sr-only">
                    {t('account.model_config.keys.selected_count', {
                      count: selected.size,
                    })}
                  </span>
                </th>
                <th scope="col" className="px-2 py-2">
                  {t('account.model_config.keys.name')}
                </th>
                <th scope="col" className="px-2 py-2">
                  {t('account.model_config.keys.group')}
                </th>
                <th scope="col" className="px-2 py-2">
                  {t('account.model_config.keys.key')}
                </th>
                <th scope="col" className="px-2 py-2">
                  {t('account.model_config.keys.limits')}
                </th>
                <th scope="col" className="px-2 py-2 text-right">
                  {t('account.model_config.keys.actions')}
                </th>
              </tr>
            </thead>
            <tbody>
              {visibleTokens.map((token) => {
                const summary = modelLimitsSummary(token.model_limits)
                const editing = editingId === token.id
                const notice = rowNotice[token.id]
                return (
                  <TokenRows
                    key={token.id}
                    token={token}
                    groups={groups}
                    summary={summary}
                    editing={editing}
                    editingValue={editingValue}
                    rowBusy={rowBusy}
                    notice={notice}
                    checked={selected.has(token.id)}
                    onToggle={(checked) => toggleSelected(token.id, checked)}
                    onEdit={() => openEditor(token)}
                    onCancel={() => setEditingId(null)}
                    onValueChange={setEditingValue}
                    onSave={() => void saveRow(token)}
                  />
                )
              })}
            </tbody>
          </table>
        </div>
      )}

      <Dialog
        open={batchOpen}
        onOpenChange={(open) => {
          if (batchBusy) return
          setBatchOpen(open)
          if (!open) setBatchOutcome(null)
        }}
        title={t('account.model_config.keys.batch_title')}
        description={t('account.model_config.keys.batch_description', {
          count: selected.size,
        })}
        footer={
          <>
            <Button
              variant="secondary"
              onClick={() => setBatchOpen(false)}
              disabled={batchBusy}
            >
              {t('account.model_config.keys.cancel')}
            </Button>
            <Button
              onClick={() => void runBatch()}
              disabled={batchBusy || selected.size === 0}
              aria-busy={batchBusy}
            >
              {batchBusy
                ? t('account.model_config.keys.saving')
                : t('account.model_config.keys.batch_confirm')}
            </Button>
          </>
        }
      >
        <div className="space-y-4">
          <ModelMultiSelect
            groups={batchCandidateGroups}
            value={batchValue}
            onChange={setBatchValue}
            label={t('account.model_config.keys.batch_models_label')}
            emptyLabel={t('account.model_config.keys.models_empty')}
            idPrefix="batch-model-limits"
          />
          {batchOutcome !== null && batchOutcome.failed.length > 0 ? (
            <div className="rounded-lg border border-danger-line bg-danger-bg p-3">
              <p className="text-[13px] font-medium text-danger">
                {t('account.model_config.keys.batch_partial', {
                  count: batchOutcome.failed.length,
                })}
              </p>
              <table className="mt-2 w-full text-left text-xs">
                <thead>
                  <tr className="text-ink-3">
                    <th scope="col" className="py-1">
                      {t('account.model_config.keys.failure_token')}
                    </th>
                    <th scope="col" className="py-1">
                      {t('account.model_config.keys.failure_reason')}
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {batchOutcome.failed.map((failure) => (
                    <tr key={failure.name}>
                      <td className="py-1 font-mono">{failure.name}</td>
                      <td className="py-1">{failure.reason}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : null}
        </div>
      </Dialog>
    </section>
  )
}

interface TokenRowsProps {
  token: Token
  groups: readonly UsableGroup[]
  summary: { unlimited: boolean; count: number }
  editing: boolean
  editingValue: string[]
  rowBusy: boolean
  notice: string | undefined
  checked: boolean
  onToggle: (checked: boolean) => void
  onEdit: () => void
  onCancel: () => void
  onValueChange: (next: string[]) => void
  onSave: () => void
}

/** 行 + 可展开编辑器；拆成组件让每行的 aria-label 只引用自己的令牌名。 */
function TokenRows({
  token,
  groups,
  summary,
  editing,
  editingValue,
  rowBusy,
  notice,
  checked,
  onToggle,
  onEdit,
  onCancel,
  onValueChange,
  onSave,
}: TokenRowsProps) {
  const { t } = useI18n()
  // 编辑器只展示该令牌真正可选的分组：普通令牌是绑定分组，auto 令牌是
  // auto_groups 命中的分组。多选器因此不可能展示用户不可用分组的模型。
  const editorGroups =
    token.group === 'auto'
      ? groups.filter((group) => (token.auto_groups ?? []).includes(group.name))
      : groups.filter((group) => group.name === token.group)

  return (
    <>
      <tr
        data-testid={`key-row-${token.id}`}
        className="border-b border-border-hairline align-top"
      >
        <td className="px-2 py-2">
          <input
            type="checkbox"
            checked={checked}
            onChange={(event) => onToggle(event.target.checked)}
            aria-label={t('account.model_config.keys.select_row', {
              name: token.name,
            })}
            className="size-4 accent-brand"
          />
        </td>
        <td className="px-2 py-2 font-medium text-ink-1">{token.name}</td>
        <td className="px-2 py-2 text-ink-2">
          <span className="rounded border border-border-hairline bg-surface-subtle px-1.5 py-0.5 text-xs">
            {token.group}
          </span>
        </td>
        <td className="px-2 py-2 font-mono text-xs text-ink-2" data-testid="masked-key">
          {token.key_masked}
        </td>
        <td className="px-2 py-2 text-xs text-ink-2">
          {summary.unlimited ? (
            <span className="text-ink-3">
              {t('account.model_config.keys.unlimited')}
            </span>
          ) : (
            <span title={(token.model_limits ?? []).join(', ')}>
              {t('account.model_config.keys.model_count', {
                count: summary.count,
              })}
            </span>
          )}
          {notice !== undefined ? (
            <p className="mt-1 text-xs text-warning" role="status">
              {notice}
            </p>
          ) : null}
        </td>
        <td className="px-2 py-2 text-right">
          <Button variant="ghost" onClick={onEdit} disabled={editing}>
            {t('account.model_config.keys.edit')}
          </Button>
        </td>
      </tr>
      {editing ? (
        <tr className="border-b border-border-hairline bg-surface-page">
          <td colSpan={6} className="px-4 py-3">
            <div className="max-w-2xl space-y-3">
              <p className="text-[13px] font-medium text-ink-1">
                {t('account.model_config.keys.editor_label', {
                  name: token.name,
                })}
              </p>
              <ModelMultiSelect
                groups={editorGroups}
                value={editingValue}
                onChange={onValueChange}
                label={t('account.model_config.keys.editor_label', {
                  name: token.name,
                })}
                emptyLabel={t('account.model_config.keys.models_empty')}
                idPrefix={`row-model-limits-${token.id}`}
              />
              <div className="flex gap-2">
                <Button onClick={onSave} disabled={rowBusy} aria-busy={rowBusy}>
                  {rowBusy
                    ? t('account.model_config.keys.saving')
                    : t('account.model_config.keys.save')}
                </Button>
                <Button variant="secondary" onClick={onCancel} disabled={rowBusy}>
                  {t('account.model_config.keys.cancel')}
                </Button>
              </div>
            </div>
          </td>
        </tr>
      ) : null}
    </>
  )
}
