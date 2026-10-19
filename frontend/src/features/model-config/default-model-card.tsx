import { useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'

import { ApiError } from '@/api/errors'
import {
  SELF_SETTINGS_QUERY_KEY,
  updateDefaultModel,
  type SelfSettings,
} from '@/lib/self-settings'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { useToastStore } from '@/shared/ui/toast'

import { allUsableModels, type UsableGroup } from './api'

export interface DefaultModelCardProps {
  groups: readonly UsableGroup[]
  settings: SelfSettings | undefined
  loading: boolean
  error: boolean
  onRetry: () => void
  /** `/model-config?focus=default-model`：滚动并聚焦本卡。 */
  focus?: boolean
}

/**
 * 默认模型卡（§2.15）。
 *
 * 选中只改本地态，「保存」才提交；切换分组只改变浏览范围，不改变已选值——
 * 已选模型若不在当前分组就保留并标注不可用，由服务端 422 兜底。这样不会
 * 因为浏览动作静默丢掉用户的选择。
 */
export function DefaultModelCard({
  groups,
  settings,
  loading,
  error,
  onRetry,
  focus = false,
}: DefaultModelCardProps) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)

  const serverValue = settings?.default_model ?? ''
  const [selected, setSelected] = useState(serverValue)
  const [saving, setSaving] = useState(false)
  const [fieldError, setFieldError] = useState<string | undefined>(undefined)
  const sectionRef = useRef<HTMLElement>(null)

  // 服务端回读值到达或刷新后同步本地选择；用 serverValue 作为依赖即可，
  // 未保存的本地选择不会因无关重渲染被覆盖。
  useEffect(() => {
    setSelected(serverValue)
  }, [serverValue])

  useEffect(() => {
    if (!focus) return
    const section = sectionRef.current
    if (section === null) return
    section.scrollIntoView?.({ block: 'start' })
    section.focus({ preventScroll: true })
  }, [focus, loading])

  const usable = allUsableModels(groups)
  const activeGroup =
    groups.find((group) => group.models.includes(selected)) ?? groups[0]
  const [activeGroupName, setActiveGroupName] = useState<string>('')
  const currentGroup =
    groups.find((group) => group.name === activeGroupName) ?? activeGroup

  const dirty = selected !== serverValue
  const selectedUnavailable = selected !== '' && !usable.has(selected)

  async function persist(nextValue: string): Promise<boolean> {
    if (settings === undefined) return false
    setSaving(true)
    setFieldError(undefined)
    try {
      const updated = await updateDefaultModel(nextValue, settings.updated_at)
      queryClient.setQueryData(SELF_SETTINGS_QUERY_KEY, updated)
      setSelected(updated.default_model ?? '')
      push({
        tone: 'success',
        message:
          nextValue === ''
            ? t('account.model_config.default.cleared')
            : t('account.model_config.default.saved'),
      })
      return true
    } catch (err) {
      if (err instanceof ApiError && err.kind === 'validation') {
        // 422 user.default_model_invalid：字段级错误并回读服务端值，
        // 页面不能保留一个服务端从未接受的选择（即使回读后原值没变，
        // 本地单选态也必须回到服务端事实）。
        setFieldError(t('account.model_config.default.invalid'))
        await queryClient.invalidateQueries({
          queryKey: SELF_SETTINGS_QUERY_KEY,
        })
        const latest = queryClient.getQueryData<SelfSettings>(
          SELF_SETTINGS_QUERY_KEY,
        )
        setSelected(latest?.default_model ?? '')
      } else if (err instanceof ApiError && err.kind === 'rate_limited') {
        push({ tone: 'error', message: t('account.model_config.rate_limited') })
      } else {
        push({
          tone: 'error',
          message: t('account.model_config.default.save_error'),
          requestId: err instanceof ApiError ? err.request_id : undefined,
        })
      }
      return false
    } finally {
      setSaving(false)
    }
  }

  const groupNames = groups.map((group) => group.name)
  const effectiveGroupName =
    currentGroup !== undefined && groupNames.includes(currentGroup.name)
      ? currentGroup.name
      : groupNames[0] ?? ''

  return (
    <section
      ref={sectionRef}
      tabIndex={-1}
      aria-labelledby="default-model-title"
      data-testid="default-model-card"
      className="rounded-xl border border-border-hairline bg-surface-card p-5 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
    >
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 id="default-model-title" className="text-base font-semibold text-ink-1">
            {t('account.model_config.default.title')}
          </h2>
          <p className="mt-1 text-[13px] text-ink-2">
            {t('account.model_config.default.description')}
          </p>
        </div>
        <p className="text-[13px] text-ink-2">
          <span className="text-ink-3">
            {t('account.model_config.default.current_label')}:
          </span>{' '}
          <span className="font-mono text-ink-1" data-testid="default-model-current">
            {loading
              ? t('account.model_config.default.loading')
              : serverValue === ''
                ? t('account.model_config.default.not_set')
                : serverValue}
          </span>
        </p>
      </div>

      {error ? (
        <div className="mt-4 flex items-center gap-3 text-[13px] text-danger">
          <span>{t('account.model_config.load_error')}</span>
          <Button variant="secondary" onClick={onRetry}>
            {t('account.model_config.retry')}
          </Button>
        </div>
      ) : (
        <>
          <div
            role="tablist"
            aria-label={t('account.model_config.default.group_label')}
            className="mt-4 flex flex-wrap gap-2"
          >
            {groups.map((group) => {
              const active = group.name === effectiveGroupName
              return (
                <button
                  key={group.name}
                  type="button"
                  role="tab"
                  aria-selected={active}
                  onClick={() => setActiveGroupName(group.name)}
                  className={[
                    'h-8 rounded-lg border px-3 text-[13px]',
                    active
                      ? 'border-brand-line bg-brand-subtle text-brand'
                      : 'border-border-default bg-surface-page text-ink-2 hover:bg-surface-hover',
                  ].join(' ')}
                >
                  {group.name}
                  <span className="ml-1 text-ink-3">({group.models.length})</span>
                </button>
              )
            })}
          </div>

          {currentGroup === undefined || currentGroup.models.length === 0 ? (
            <p className="mt-4 text-[13px] text-ink-3">
              {t('account.model_config.default.empty_models')}
            </p>
          ) : (
            <div
              role="radiogroup"
              aria-label={t('account.model_config.default.title')}
              className="mt-3 grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3"
            >
              {currentGroup.models.map((model) => {
                const checked = selected === model
                const id = `default-model-${currentGroup.name}-${model}`
                return (
                  <label
                    key={model}
                    htmlFor={id}
                    className={[
                      'flex min-h-9 cursor-pointer items-center gap-2 rounded-lg border px-3 py-2 text-[13px] transition-colors',
                      checked
                        ? 'border-brand bg-brand-subtle text-brand'
                        : 'border-border-default bg-surface-page text-ink-1 hover:bg-surface-hover',
                    ].join(' ')}
                  >
                    <input
                      id={id}
                      type="radio"
                      name="default-model"
                      value={model}
                      checked={checked}
                      onChange={() => {
                        setSelected(model)
                        setFieldError(undefined)
                      }}
                      className="sr-only"
                    />
                    <span
                      aria-hidden="true"
                      className={[
                        'grid size-4 shrink-0 place-items-center rounded-full border',
                        checked ? 'border-brand' : 'border-border-default',
                      ].join(' ')}
                    >
                      {checked ? (
                        <span className="size-2 rounded-full bg-brand" />
                      ) : null}
                    </span>
                    <span className="truncate font-mono text-xs">{model}</span>
                  </label>
                )
              })}
            </div>
          )}

          {selectedUnavailable ? (
            <p className="mt-2 text-xs text-warning" role="status">
              {t('account.model_config.default.unavailable_selected')}
            </p>
          ) : null}
          {fieldError !== undefined ? (
            <p className="mt-2 text-xs text-danger" role="alert">
              {fieldError}
            </p>
          ) : null}

          <div className="mt-4 flex flex-wrap items-center gap-2">
            <Button
              onClick={() => {
                void persist(selected)
              }}
              disabled={saving || !dirty || settings === undefined}
              aria-busy={saving}
            >
              {saving
                ? t('account.model_config.default.saving')
                : t('account.model_config.default.save')}
            </Button>
            <Button
              variant="secondary"
              onClick={() => {
                void persist('')
              }}
              disabled={saving || (serverValue === '' && selected === '')}
            >
              {t('account.model_config.default.clear')}
            </Button>
          </div>
        </>
      )}
    </section>
  )
}
