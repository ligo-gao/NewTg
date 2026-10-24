import { useEffect, useMemo, useState } from 'react'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { Dialog } from '@/shared/ui/base/dialog'

import { fetchChannelModels } from './api'
import type { FetchChannelModelsInput } from './types'

interface FetchModelsDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  request: FetchChannelModelsInput
  /** Models already on the form; preselected so applying never silently drops them. */
  existingModels: string[]
  onApply: (models: string[]) => void
}

type LoadState = 'loading' | 'ready' | 'error'

/**
 * Pulls the upstream's model catalog for the current form values and lets the
 * administrator choose which IDs to write into `supported_models`.
 *
 * Create mode preselects everything fetched (the list is the point of the
 * action); edit mode preselects the models already on the channel so applying
 * cannot silently remove aliases that the upstream no longer advertises.
 */
export function FetchModelsDialog({
  open,
  onOpenChange,
  request,
  existingModels,
  onApply,
}: FetchModelsDialogProps) {
  const { t } = useI18n()
  const [state, setState] = useState<LoadState>('loading')
  const [errorMessage, setErrorMessage] = useState('')
  const [models, setModels] = useState<string[]>([])
  const [selected, setSelected] = useState<Set<string>>(() => new Set())
  const [query, setQuery] = useState('')
  const [nonce, setNonce] = useState(0)

  const existingKey = existingModels.join('\u0000')
  const requestKey = [
    request.channel_id ?? '',
    request.type,
    request.base_url ?? '',
    request.credentials?.api_key ?? '',
    request.credentials?.deployment ?? '',
    request.credentials?.api_version ?? '',
  ].join('\u0000')

  useEffect(() => {
    if (!open) return
    let cancelled = false

    async function load() {
      setState('loading')
      setErrorMessage('')
      setQuery('')
      try {
        const response = await fetchChannelModels(request)
        if (cancelled) return
        const merged = Array.from(new Set([...existingModels, ...response.models]))
        setModels(merged)
        setSelected(new Set(existingModels.length > 0 ? existingModels : merged))
        setState('ready')
      } catch (error) {
        if (cancelled) return
        setErrorMessage(
          error instanceof ApiError
            ? error.message
            : t('common.channels.fetchModels.error'),
        )
        setState('error')
      }
    }

    void load()
    return () => {
      cancelled = true
    }
    // The request/existing values are captured above; their stable keys are the deps.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, nonce, requestKey, existingKey, t])

  const filtered = useMemo(() => {
    const needle = query.trim().toLowerCase()
    if (needle === '') return models
    return models.filter((model) => model.toLowerCase().includes(needle))
  }, [models, query])

  function toggle(model: string) {
    setSelected((previous) => {
      const next = new Set(previous)
      if (next.has(model)) next.delete(model)
      else next.add(model)
      return next
    })
  }

  function selectVisible() {
    setSelected((previous) => {
      const next = new Set(previous)
      filtered.forEach((model) => next.add(model))
      return next
    })
  }

  function clearAll() {
    setSelected(new Set())
  }

  function apply() {
    if (selected.size === 0) return
    onApply(Array.from(selected))
    onOpenChange(false)
  }

  const footer = (
    <>
      <Button variant="secondary" onClick={() => onOpenChange(false)}>
        {t('common.channels.fetchModels.cancel')}
      </Button>
      <Button onClick={apply} disabled={state !== 'ready' || selected.size === 0}>
        {t('common.channels.fetchModels.apply', { count: selected.size })}
      </Button>
    </>
  )

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={t('common.channels.fetchModels.title')}
      description={t('common.channels.fetchModels.description')}
      footer={footer}
      panelClassName="max-w-[640px]"
    >
      {state === 'loading' ? (
        <div className="flex min-h-[200px] items-center justify-center gap-2 text-body text-ink-2">
          <span className="size-4 animate-spin rounded-full border-2 border-border-default border-t-brand" />
          {t('common.channels.fetchModels.loading')}
        </div>
      ) : state === 'error' ? (
        <div className="flex min-h-[200px] flex-col items-center justify-center gap-3 text-center">
          <p className="text-body text-danger">{errorMessage}</p>
          <Button variant="secondary" onClick={() => setNonce((value) => value + 1)}>
            {t('common.channels.fetchModels.retry')}
          </Button>
        </div>
      ) : (
        <div className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <input
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder={t('common.channels.fetchModels.searchPlaceholder')}
              className="h-9 min-w-0 flex-1 rounded-lg border border-border-default bg-surface-card px-3 text-body text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            />
            <Button variant="ghost" onClick={selectVisible}>
              {t('common.channels.fetchModels.selectAll')}
            </Button>
            <Button variant="ghost" onClick={clearAll}>
              {t('common.channels.fetchModels.clearAll')}
            </Button>
          </div>

          <p className="text-meta text-ink-3">
            {t('common.channels.fetchModels.selectedCount', { count: selected.size })}
          </p>

          <div className="max-h-[320px] overflow-y-auto rounded-lg border border-border-hairline">
            {filtered.length === 0 ? (
              <p className="px-4 py-8 text-center text-body text-ink-3">
                {t('common.channels.fetchModels.empty')}
              </p>
            ) : (
              <ul className="divide-y divide-border-hairline">
                {filtered.map((model) => (
                  <li key={model}>
                    <label className="flex cursor-pointer items-center gap-3 px-4 py-2.5 text-body text-ink-1 hover:bg-surface-hover">
                      <input
                        type="checkbox"
                        checked={selected.has(model)}
                        onChange={() => toggle(model)}
                        className="size-4 rounded border-border-default"
                      />
                      <span className="truncate font-mono text-meta">{model}</span>
                    </label>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </div>
      )}
    </Dialog>
  )
}
