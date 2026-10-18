import { useEffect, useRef, useState } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { Dialog } from '@/shared/ui/base/dialog'

import { testChannel } from './api'
import { testErrorKey } from './test-error'
import type { Channel } from './types'

type TestStatus = 'pending' | 'running' | 'success' | 'failed'

interface TestEntry {
  status: TestStatus
  latency?: number
  errorClass?: string
}

const CONCURRENCY = 3

export interface ChannelTestDialogProps {
  open: boolean
  channel: Channel | null
  onOpenChange: (open: boolean) => void
  /** Called after a run finishes so the list can refresh last_test_* columns. */
  onCompleted: () => void
}

/**
 * Batch connectivity test (modules-06 §4, F-06-06/F-06-07).
 *
 * The backend exposes a per-model endpoint; the dialog orchestrates three
 * concurrent workers and keeps completed results when the user stops early.
 */
export function ChannelTestDialog({
  open,
  channel,
  onOpenChange,
  onCompleted,
}: ChannelTestDialogProps) {
  const { t } = useI18n()
  const [selectedModels, setSelectedModels] = useState<string[]>([])
  const [results, setResults] = useState<Record<string, TestEntry>>({})
  const [completed, setCompleted] = useState(0)
  const [running, setRunning] = useState(false)
  const [stream, setStream] = useState(false)
  const stopRef = useRef(false)

  const channelId = channel?.id

  // Reset only when the dialog opens or the target changes. Depending on the
  // models array identity would wipe live results whenever a list refetch
  // replaced the row object after `onCompleted`.
  useEffect(() => {
    if (!open) return
    setSelectedModels(channel === null ? [] : [...channel.supported_models])
    setResults({})
    setCompleted(0)
    setRunning(false)
    setStream(false)
    stopRef.current = false
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, channelId])

  if (channel === null) return null

  // Captured after the guard so the async workers keep the narrowed type.
  const activeChannel = channel

  const total = selectedModels.length
  const succeeded = Object.values(results).filter((entry) => entry.status === 'success').length
  const failed = Object.values(results).filter((entry) => entry.status === 'failed').length

  function toggleModel(model: string): void {
    if (running) return
    setSelectedModels((current) =>
      current.includes(model)
        ? current.filter((entry) => entry !== model)
        : [...current, model],
    )
  }

  async function start(): Promise<void> {
    if (running || selectedModels.length === 0) return
    stopRef.current = false
    setRunning(true)
    setCompleted(0)
    const queue = [...selectedModels]
    const initial: Record<string, TestEntry> = {}
    for (const model of queue) initial[model] = { status: 'pending' }
    setResults(initial)

    let finished = 0
    const worker = async (): Promise<void> => {
      for (;;) {
        if (stopRef.current) return
        const model = queue.shift()
        if (model === undefined) return
        setResults((current) => ({ ...current, [model]: { status: 'running' } }))
        try {
          const result = await testChannel(activeChannel.id, { model, stream })
          setResults((current) => ({
            ...current,
            [model]: result.success
              ? { status: 'success', latency: result.latency_ms }
              : {
                  status: 'failed',
                  errorClass: result.error_class ?? 'unknown',
                },
          }))
        } catch {
          setResults((current) => ({
            ...current,
            [model]: { status: 'failed', errorClass: 'network' },
          }))
        } finally {
          finished += 1
          setCompleted(finished)
        }
      }
    }

    await Promise.all(
      Array.from({ length: CONCURRENCY }, () => worker()),
    )
    setRunning(false)
    onCompleted()
  }

  function reasonFor(errorClass: string | undefined): string {
    return t(testErrorKey(errorClass))
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!running) onOpenChange(next)
      }}
      title={t('common.channels.test.title', { name: channel.name })}
      footer={
        <>
          <Button
            variant="secondary"
            disabled={running}
            onClick={() => {
              onOpenChange(false)
            }}
          >
            {t('common.channels.test.close')}
          </Button>
          {running ? (
            <Button
              variant="secondary"
              onClick={() => {
                // Stops scheduling new models; in-flight requests finish and
                // keep their results (F-06-07 acceptance).
                stopRef.current = true
              }}
            >
              {t('common.channels.test.stop')}
            </Button>
          ) : (
            <Button disabled={selectedModels.length === 0} onClick={() => void start()}>
              {t('common.channels.test.start')}
            </Button>
          )}
        </>
      }
    >
      {channel.supported_models.length === 0 ? (
        <p className="text-sm text-ink-2">{t('common.channels.test.noModels')}</p>
      ) : (
        <div className="flex flex-col gap-4">
          <ul className="max-h-40 overflow-y-auto rounded-lg border border-border-hairline">
            {channel.supported_models.map((model) => (
              <li
                key={model}
                className="flex items-center gap-2 border-b border-border-hairline px-3 py-2 last:border-b-0"
              >
                <input
                  type="checkbox"
                  aria-label={t('common.channels.test.selectModel', { model })}
                  checked={selectedModels.includes(model)}
                  disabled={running}
                  onChange={() => {
                    toggleModel(model)
                  }}
                />
                <span className="flex-1 font-mono text-xs text-ink-1">{model}</span>
                <TestRowStatus entry={results[model]} reasonFor={reasonFor} />
              </li>
            ))}
          </ul>

          <label className="flex items-center gap-2 text-sm text-ink-1">
            <input
              type="checkbox"
              checked={stream}
              disabled={running}
              onChange={(event) => {
                setStream(event.target.checked)
              }}
            />
            {t('common.channels.test.stream')}
          </label>

          <div>
            <div
              role="progressbar"
              aria-valuemin={0}
              aria-valuemax={total}
              aria-valuenow={completed}
              aria-label={t('common.channels.test.progress', {
                completed,
                total,
              })}
              className="h-2 overflow-hidden rounded-full bg-surface-subtle"
            >
              <div
                className="h-full rounded-full bg-info transition-[width]"
                style={{ width: total === 0 ? '0%' : `${(completed / total) * 100}%` }}
              />
            </div>
            <p className="mt-1 text-xs text-ink-2">
              {t('common.channels.test.progress', { completed, total })}
            </p>
          </div>

          <p className="text-sm text-ink-1" data-testid="test-summary">
            {t('common.channels.test.summary', { succeeded, failed })}
          </p>
        </div>
      )}
    </Dialog>
  )
}

function TestRowStatus({
  entry,
  reasonFor,
}: {
  entry: TestEntry | undefined
  reasonFor: (errorClass: string | undefined) => string
}) {
  const { t } = useI18n()
  if (entry === undefined) {
    return <span className="text-xs text-ink-3">{t('common.channels.test.pending')}</span>
  }
  if (entry.status === 'running') {
    return <span className="text-xs text-info">{t('common.channels.test.running')}</span>
  }
  if (entry.status === 'pending') {
    return <span className="text-xs text-ink-3">{t('common.channels.test.pending')}</span>
  }
  if (entry.status === 'success') {
    return (
      <span className="text-xs text-success">
        ✓ {entry.latency === undefined ? '' : `${entry.latency}ms`}
      </span>
    )
  }
  return (
    <span className="text-xs text-danger">✗ {reasonFor(entry.errorClass)}</span>
  )
}
