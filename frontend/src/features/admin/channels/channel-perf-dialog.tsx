import { useEffect, useRef, useState } from 'react'

import { ApiError } from '@/api/errors'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { Dialog } from '@/shared/ui/base/dialog'

import { runChannelPerformanceTest } from './api'
import { testErrorKey } from './test-error'
import type { Channel, ChannelPerformanceTest } from './types'

type PerfStatus = 'pending' | 'running' | 'success' | 'failed'

interface PerfEntry {
  status: PerfStatus
  /** Persisted aggregate for completed requests (absent for transport errors). */
  result?: ChannelPerformanceTest
  /** Mapped failure class — rendered through `testErrorKey`, never raw error text. */
  errorClass?: string
}

/**
 * Classifies a failed performance request exactly like the connectivity test:
 * transport failures are `network`, HTTP responses are `server`. Raw upstream
 * payloads never reach the dialog (GC-6).
 */
function failedEntry(error: unknown): PerfEntry {
  if (error instanceof ApiError) {
    if (error.kind === 'offline') return { status: 'failed', errorClass: 'network' }
    return { status: 'failed', errorClass: 'server' }
  }
  return { status: 'failed', errorClass: 'unknown' }
}

const CONCURRENCY = 2
const RUN_OPTIONS = [1, 3, 5] as const
const DEFAULT_RUNS = 3

function formatMs(value: number | null | undefined): string {
  return value === null || value === undefined ? '—' : String(Math.round(value))
}

function formatTps(value: number | null | undefined): string {
  return value === null || value === undefined ? '—' : value.toFixed(1)
}

function formatPercent(value: number | null | undefined): string {
  return value === null || value === undefined
    ? '—'
    : `${(value * 100).toFixed(1)}%`
}

function formatTokens(value: number | null | undefined): string {
  return value === null || value === undefined ? '—' : String(Math.round(value))
}

const CELL =
  'border-b border-border-hairline px-2 py-2 text-right tabular-nums text-ink-1'
const HEAD =
  'border-b border-border-hairline px-2 py-2 text-right text-[11px] font-semibold text-ink-2'

export interface ChannelPerformanceDialogProps {
  open: boolean
  channel: Channel | null
  onOpenChange: (open: boolean) => void
  /** Called after a run finishes so the list can re-read the dossier row. */
  onCompleted: () => void
}

/**
 * Multi-model performance test (TTFT / latency percentiles / TPS).
 *
 * Each model is one POST that runs 1..5 probes upstream; the dialog runs two
 * models in parallel and keeps completed results when the user stops early.
 * Unlike the connectivity test, a request returns the persisted aggregate, so
 * there is no separate history read on this screen.
 */
export function ChannelPerformanceDialog({
  open,
  channel,
  onOpenChange,
  onCompleted,
}: ChannelPerformanceDialogProps) {
  const { t } = useI18n()
  const [selectedModels, setSelectedModels] = useState<string[]>([])
  const [runs, setRuns] = useState<number>(DEFAULT_RUNS)
  const [results, setResults] = useState<Record<string, PerfEntry>>({})
  const [running, setRunning] = useState(false)
  const stopRef = useRef(false)

  const channelId = channel?.id

  // Reset only when the dialog opens or the target changes; a list refetch
  // after `onCompleted` replaces the row object but must not wipe live rows.
  useEffect(() => {
    if (!open) return
    setSelectedModels(channel === null ? [] : [...channel.supported_models])
    setRuns(DEFAULT_RUNS)
    setResults({})
    setRunning(false)
    stopRef.current = false
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, channelId])

  if (channel === null) return null

  // Captured after the guard so the async workers keep the narrowed type.
  const activeChannel = channel

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
    const queue = [...selectedModels]
    const initial: Record<string, PerfEntry> = {}
    for (const model of queue) initial[model] = { status: 'pending' }
    setResults(initial)

    const worker = async (): Promise<void> => {
      for (;;) {
        if (stopRef.current) return
        const model = queue.shift()
        if (model === undefined) return
        setResults((current) => ({ ...current, [model]: { status: 'running' } }))
        try {
          const result = await runChannelPerformanceTest(
            activeChannel.id,
            model,
            runs,
          )
          setResults((current) => ({
            ...current,
            [model]:
              result.success_count > 0
                ? { status: 'success', result }
                : { status: 'failed', result },
          }))
        } catch (error) {
          setResults((current) => ({
            ...current,
            [model]: failedEntry(error),
          }))
        }
      }
    }

    await Promise.all(Array.from({ length: CONCURRENCY }, () => worker()))
    setRunning(false)
    onCompleted()
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!running) onOpenChange(next)
      }}
      title={t('common.channels.perf.title', { name: channel.name })}
      description={t('common.channels.perf.subtitle')}
      panelClassName="max-w-none! xl:max-w-[880px]!"
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
                // Stop scheduling; in-flight requests finish and keep results.
                stopRef.current = true
              }}
            >
              {t('common.channels.perf.stop')}
            </Button>
          ) : (
            <Button disabled={selectedModels.length === 0} onClick={() => void start()}>
              {t('common.channels.perf.start')}
            </Button>
          )}
        </>
      }
    >
      {channel.supported_models.length === 0 ? (
        <p className="text-sm text-ink-2">{t('common.channels.perf.empty')}</p>
      ) : (
        <div className="flex flex-col gap-4">
          <div className="flex items-center gap-2">
            <label htmlFor="channel-perf-runs" className="text-sm text-ink-1">
              {t('common.channels.perf.runs')}
            </label>
            <select
              id="channel-perf-runs"
              value={String(runs)}
              disabled={running}
              onChange={(event) => {
                setRuns(Number(event.target.value))
              }}
              className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-sm text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-50"
            >
              {RUN_OPTIONS.map((count) => (
                <option key={count} value={count}>
                  {t('common.channels.perf.runsOption', { count })}
                </option>
              ))}
            </select>
            {selectedModels.length === 0 ? (
              <p className="text-xs text-ink-2">
                {t('common.channels.perf.noSelection')}
              </p>
            ) : null}
          </div>

          <div className="overflow-x-auto rounded-lg border border-border-hairline">
            <table
              data-testid="perf-results"
              className="w-full border-collapse text-xs"
            >
              <thead>
                <tr>
                  <th scope="col" className={`${HEAD} text-left`}>
                    {t('common.channels.table.models')}
                  </th>
                  <th scope="col" className={`${HEAD} text-left`}>
                    {t('common.channels.table.status')}
                  </th>
                  <th scope="col" className={HEAD}>
                    {t('common.channels.perf.ttft_p50')}
                  </th>
                  <th scope="col" className={HEAD}>
                    {t('common.channels.perf.ttft_p95')}
                  </th>
                  <th scope="col" className={HEAD}>
                    {t('common.channels.perf.latency_p50')}
                  </th>
                  <th scope="col" className={HEAD}>
                    {t('common.channels.perf.latency_p95')}
                  </th>
                  <th scope="col" className={HEAD}>
                    {t('common.channels.perf.tps')}
                  </th>
                  <th scope="col" className={HEAD}>
                    {t('common.channels.perf.success')}
                  </th>
                  <th scope="col" className={HEAD}>
                    {t('common.channels.perf.tokens')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {channel.supported_models.map((model) => {
                  const entry = results[model]
                  const result = entry?.result
                  return (
                    <tr key={model} data-testid={`perf-result-${model}`}>
                      <td className={`${CELL} text-left`}>
                        <label className="flex items-center gap-2">
                          <input
                            type="checkbox"
                            checked={selectedModels.includes(model)}
                            disabled={running}
                            onChange={() => {
                              toggleModel(model)
                            }}
                          />
                          <span className="font-mono text-ink-1">{model}</span>
                        </label>
                      </td>
                      <td className={`${CELL} text-left`}>
                        <PerfStatusCell entry={entry} />
                      </td>
                      <td className={CELL}>{formatMs(result?.ttft_ms_p50)}</td>
                      <td className={CELL}>{formatMs(result?.ttft_ms_p95)}</td>
                      <td className={CELL}>{formatMs(result?.latency_ms_p50)}</td>
                      <td className={CELL}>{formatMs(result?.latency_ms_p95)}</td>
                      <td className={CELL}>{formatTps(result?.tps_avg)}</td>
                      <td className={CELL}>{formatPercent(result?.success_rate)}</td>
                      <td className={CELL}>{formatTokens(result?.output_tokens_avg)}</td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </Dialog>
  )
}

function PerfStatusCell({ entry }: { entry: PerfEntry | undefined }) {
  const { t } = useI18n()
  if (entry === undefined || entry.status === 'pending') {
    return <span className="text-ink-3">{t('common.channels.test.pending')}</span>
  }
  if (entry.status === 'running') {
    return <span className="text-info">{t('common.channels.perf.running')}</span>
  }
  if (entry.status === 'success') {
    return <span className="text-success">✓</span>
  }
  return (
    <span className="text-danger">
      ✗{' '}
      {entry.errorClass === undefined
        ? t('common.channels.perf.error')
        : t(testErrorKey(entry.errorClass))}
    </span>
  )
}
