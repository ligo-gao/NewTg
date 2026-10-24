import { useQuery } from '@tanstack/react-query'
import { useState, type ReactNode } from 'react'

import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/button'
import { FormAlert } from '@/shared/ui/form'

import {
  createIdempotencyKey,
  dryRunTaskPlugin,
  listTaskPluginVersions,
  listTaskPlugins,
  type PluginDryRunResult,
} from './api'
import { SectionCard, SkeletonRows } from './components'
import { errorMessage, issueMessages, prettyJson, statusValueOf, TASK_STATES } from './utils'

/**
 * 沙箱端点下拉（`relay-06` §3.8 的规范任务动作；契约注释同集）。
 * 值原样发送，服务端按钩子名解析。
 */
const ENDPOINTS = ['submit', 'query', 'result', 'submit_response'] as const

/** 设计假设 5s（§3.10 F-08-35）；超时中断由前端 AbortController 保证。 */
const DRY_RUN_TIMEOUT_MS = 5000

export interface SandboxTabProps {
  canManage: boolean
  online: boolean
}

/**
 * 沙箱 Tab（F-08-35）。
 *
 * 零副作用由服务端保证：不写任务表、不扣费、默认不出站。出站执行器尚未
 * 实现，`allow_outbound=true` 会 422 `dryrun_outbound_unavailable`，因此复选框
 * 以禁用态呈现并给出说明——不渲染一个「点了必然失败」的假入口。
 *
 * 结果区四块中，契约只返回钩子原始返回值（`result`）；注入上下文与宿主
 * 解析结果无字段可填，明确标注「当前接口未返回」而不是伪造内容（已登记）。
 */
export function SandboxTab({ canManage, online }: SandboxTabProps) {
  const { t } = useI18n()

  const [pluginKey, setPluginKey] = useState('')
  const [version, setVersion] = useState('')
  const [endpoint, setEndpoint] = useState<string>('submit')
  const [payloadText, setPayloadText] = useState('{}')
  const [running, setRunning] = useState(false)
  const [payloadError, setPayloadError] = useState<string | null>(null)
  const [runError, setRunError] = useState<{ message: string; issues: string[] } | null>(null)
  const [result, setResult] = useState<PluginDryRunResult | null>(null)

  const pluginsQuery = useQuery({
    queryKey: ['plugin-list', 'sandbox-all'],
    queryFn: ({ signal }) => listTaskPlugins({ page: 1, pageSize: 100 }, signal),
  })
  const versionsQuery = useQuery({
    queryKey: ['plugin-versions', pluginKey],
    queryFn: ({ signal }) => listTaskPluginVersions(pluginKey, signal),
    enabled: pluginKey !== '',
  })

  const versions = versionsQuery.data ?? []

  async function run(): Promise<void> {
    let payload: unknown
    try {
      payload = payloadText.trim() === '' ? {} : JSON.parse(payloadText)
    } catch {
      setPayloadError(t('admin.task_plugins.sandbox.invalid_json'))
      return
    }
    setPayloadError(null)
    setRunError(null)
    setResult(null)
    setRunning(true)
    const controller = new AbortController()
    const timer = globalThis.setTimeout(() => controller.abort(), DRY_RUN_TIMEOUT_MS)
    try {
      const data = await dryRunTaskPlugin(
        {
          key: pluginKey,
          version: version === '' ? undefined : version,
          endpoint,
          payload,
          // 出站执行器未实现：界面从不会发送 true（服务端也会 422 兜底）。
          allowOutbound: false,
        },
        createIdempotencyKey(),
        controller.signal,
      )
      setResult(data)
    } catch (error) {
      if (controller.signal.aborted) {
        setRunError({ message: t('admin.task_plugins.sandbox.timeout'), issues: [] })
      } else {
        setRunError({ message: errorMessage(error), issues: issueMessages(error) })
      }
    } finally {
      globalThis.clearTimeout(timer)
      setRunning(false)
    }
  }

  const parsedStatus = result === null ? undefined : statusValueOf(result.result)
  const invalidStatus =
    parsedStatus !== undefined && !TASK_STATES.includes(parsedStatus as (typeof TASK_STATES)[number])
  const canRun = canManage && online && pluginKey !== '' && !running

  return (
    <div className="grid gap-3 xl:grid-cols-2">
      <SectionCard title={t('admin.task_plugins.sandbox.config_title')}>
        {pluginsQuery.isPending ? <SkeletonRows rows={3} /> : null}
        <div className="space-y-3">
          <label className="flex flex-col gap-1 text-meta text-ink-3">
            {t('admin.task_plugins.sandbox.plugin')}
            <select
              value={pluginKey}
              onChange={(event) => {
                setPluginKey(event.target.value)
                setVersion('')
                setResult(null)
              }}
              className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1"
            >
              <option value="">{t('admin.task_plugins.sandbox.select_plugin')}</option>
              {(pluginsQuery.data?.items ?? []).map((plugin) => (
                <option key={plugin.key} value={plugin.key}>
                  {plugin.name} ({plugin.key})
                </option>
              ))}
            </select>
          </label>

          <label className="flex flex-col gap-1 text-meta text-ink-3">
            {t('admin.task_plugins.sandbox.version')}
            <select
              value={version}
              onChange={(event) => setVersion(event.target.value)}
              disabled={pluginKey === ''}
              className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1 disabled:opacity-50"
            >
              <option value="">
                {t('admin.task_plugins.sandbox.version_effective')}
              </option>
              {versions.map((entry) => (
                <option key={entry.id} value={entry.version}>
                  {entry.version}
                  {entry.active ? ` · ${t('admin.task_plugins.detail.current')}` : ''}
                </option>
              ))}
            </select>
          </label>

          <label className="flex flex-col gap-1 text-meta text-ink-3">
            {t('admin.task_plugins.sandbox.endpoint')}
            <select
              value={endpoint}
              onChange={(event) => setEndpoint(event.target.value)}
              className="h-9 rounded-lg border border-border-default bg-surface-card px-2 text-body text-ink-1"
            >
              {ENDPOINTS.map((entry) => (
                <option key={entry} value={entry}>
                  {t(`admin.task_plugins.sandbox.endpoint_${entry}`)}
                </option>
              ))}
            </select>
          </label>

          <label className="flex flex-col gap-1 text-meta text-ink-3">
            {t('admin.task_plugins.sandbox.payload')}
            <textarea
              value={payloadText}
              onChange={(event) => setPayloadText(event.target.value)}
              rows={8}
              spellCheck={false}
              className="w-full rounded-lg border border-border-default bg-surface-card px-3 py-2 font-mono text-meta text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
            />
          </label>

          {payloadError !== null ? <FormAlert>{payloadError}</FormAlert> : null}

          <div>
            <label className="flex items-center gap-2 text-body text-ink-2">
              <input type="checkbox" checked={false} disabled />
              {t('admin.task_plugins.sandbox.allow_outbound')}
            </label>
            <p className="mt-1 text-meta text-ink-3">
              {t('admin.task_plugins.sandbox.outbound_unavailable')}
            </p>
          </div>

          <div>
            <Button
              disabled={!canRun}
              title={!online ? t('admin.task_plugins.common.offline') : undefined}
              onClick={() => void run()}
            >
              {running
                ? t('admin.task_plugins.sandbox.running')
                : t('admin.task_plugins.sandbox.run')}
            </Button>
            <p className="mt-2 text-meta text-ink-3">
              {t('admin.task_plugins.sandbox.zero_side_effects')}
            </p>
            {!canManage ? (
              <p className="mt-1 text-meta text-ink-3">
                {t('admin.task_plugins.common.read_only')}
              </p>
            ) : null}
            {!online ? (
              <p className="mt-1 text-meta text-ink-3">
                {t('admin.task_plugins.common.offline')}
              </p>
            ) : null}
          </div>
        </div>
      </SectionCard>

      <SectionCard title={t('admin.task_plugins.sandbox.result_title')}>
        {pluginKey === '' ? (
          <p className="rounded-lg border border-dashed border-border-default px-4 py-8 text-center text-body text-ink-3">
            {t('admin.task_plugins.sandbox.empty')}
          </p>
        ) : (
          <div className="space-y-3">
            <ResultPane title={t('admin.task_plugins.sandbox.pane_context')}>
              <p className="text-meta text-ink-3">
                {t('admin.task_plugins.sandbox.pane_context_missing')}
              </p>
            </ResultPane>

            <ResultPane title={t('admin.task_plugins.sandbox.pane_raw')}>
              <pre className="max-h-56 overflow-auto rounded bg-surface-subtle p-2 font-mono text-meta text-ink-1">
                {result === null ? '—' : prettyJson(result.result)}
              </pre>
            </ResultPane>

            <ResultPane title={t('admin.task_plugins.sandbox.pane_parsed')}>
              {result === null ? (
                <p className="text-meta text-ink-3">
                  {t('admin.task_plugins.sandbox.not_run')}
                </p>
              ) : (
                <div className="text-meta">
                  {parsedStatus === undefined ? (
                    <p className="text-ink-3">
                      {t('admin.task_plugins.sandbox.pane_parsed_missing')}
                    </p>
                  ) : (
                    <p
                      data-testid="sandbox-parsed-status"
                      className={invalidStatus ? 'font-medium text-danger' : 'text-ink-1'}
                    >
                      {invalidStatus
                        ? t('admin.task_plugins.sandbox.invalid_status', {
                            value: parsedStatus,
                          })
                        : `${t('admin.task_plugins.sandbox.status_label')}: ${parsedStatus}`}
                    </p>
                  )}
                  <p className="mt-1 text-ink-3">
                    {t('admin.task_plugins.sandbox.duration', {
                      ms: result.duration_ms,
                    })}
                  </p>
                </div>
              )}
            </ResultPane>

            {runError !== null ? (
              <ResultPane title={t('admin.task_plugins.sandbox.pane_error')}>
                <div data-testid="sandbox-error" role="alert">
                  <p className="text-body text-danger">{runError.message}</p>
                  {runError.issues.map((line) => (
                    <p key={line} className="mt-1 font-mono text-meta text-danger">
                      {line}
                    </p>
                  ))}
                </div>
              </ResultPane>
            ) : null}
          </div>
        )}
      </SectionCard>
    </div>
  )
}

function ResultPane({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="rounded-lg border border-border-default p-3">
      <h3 className="mb-1 text-meta text-ink-3">{title}</h3>
      {children}
    </section>
  )
}
