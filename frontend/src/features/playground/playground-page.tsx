import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'

import { apiClient } from '@/api/client'
import { requireData } from '@/api/result'
import { meQueryOptions } from '@/lib/session'
import { publicStatusQueryOptions } from '@/lib/public-status'
import { selfSettingsQueryOptions } from '@/lib/self-settings'
import { formatCredits } from '@/shared/lib/format'
import { useI18n } from '@/shared/lib/i18n-context'
import { Button } from '@/shared/ui/base/button'
import { useToastStore } from '@/shared/ui/toast'

import {
  PlaygroundError,
  fetchTurnCredits,
  isAbortError,
  streamChatCompletion,
  type PlaygroundChatMessage,
} from './api'
import { MessageContent } from './message-content'
import {
  clearPlaygroundMessages,
  loadPlaygroundMessages,
  playgroundSessionKey,
  savePlaygroundMessages,
  type PlaygroundMessage,
} from './session-storage'
import type { UsableGroup } from '@/features/model-config/api'

/**
 * 与充值页共用的钱包缓存键（F-03-01）。键形状必须一致，完成一轮对话后
 * 失效才会让钱包页/本页余额同时刷新。
 */
const WALLET_SUMMARY_KEY = ['wallet', 'summary'] as const

type PlaygroundFailure =
  | { kind: 'quota' }
  | { kind: 'forbidden' }
  | { kind: 'auth' }
  | { kind: 'session' }
  | { kind: 'csrf' }
  | { kind: 'server'; requestId?: string }
  | { kind: 'other'; requestId?: string; message?: string }

function newId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `msg-${Date.now()}-${Math.random().toString(36).slice(2)}`
}

/**
 * Playground 在线调试（modules-02 §2.16，F-02-19）。
 *
 * 计费边界是硬约束，页面只做呈现、不重算：身份 = 会话（无 Authorization、
 * `usage_logs.token_id` 为 NULL），跳过令牌额度与令牌白名单，但钱包/订阅
 * 照常扣费、违规费不豁免。请求成功后按 `X-Request-ID` 回读 `usage_logs`
 * 拿本轮 credits，绝不用本地 tokenizer 估算。
 */
export function PlaygroundPage() {
  const { t, locale } = useI18n()
  const queryClient = useQueryClient()
  const push = useToastStore((state) => state.push)

  const statusQuery = useQuery(publicStatusQueryOptions())
  const settingsQuery = useQuery(selfSettingsQueryOptions())
  const meQuery = useQuery(meQueryOptions())
  const walletQuery = useQuery({
    queryKey: WALLET_SUMMARY_KEY,
    queryFn: async () =>
      requireData(await apiClient.GET('/api/wallet/summary')),
  })

  const groups: UsableGroup[] = statusQuery.data?.usable_groups ?? []
  /**
   * `/pg` 的准入按 `users.group`（当前分组）判定，不是全部可用分组。
   * 下拉只展示当前分组的模型，避免多分组部署下选到别的分组、首条请求
   * 就 403。当前分组取自钱包摘要（schema 的 `group`），因为它是本页已经
   * 需要读取的结算身份来源。
   */
  const currentGroupName = walletQuery.data?.group ?? ''
  const currentGroup = groups.find((group) => group.name === currentGroupName)
  const availableModels = currentGroup?.models ?? []
  const availableSet = new Set(availableModels)
  const groupReady = walletQuery.isSuccess && currentGroup !== undefined
  const groupUnknown = !walletQuery.isPending && !groupReady
  const defaultModel = settingsQuery.data?.default_model ?? ''
  const username = meQuery.data?.username ?? ''

  const [model, setModel] = useState('')
  const [messages, setMessages] = useState<PlaygroundMessage[]>([])
  const [draft, setDraft] = useState('')
  const [streaming, setStreaming] = useState(false)
  const [failure, setFailure] = useState<PlaygroundFailure | null>(null)
  const [announcement, setAnnouncement] = useState('')
  const [announceReplies, setAnnounceReplies] = useState(true)
  const controllerRef = useRef<AbortController | null>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  /** 用户是否手动选过模型：手动选择后，服务端默认模型到达也不能覆盖它。 */
  const pickedModelRef = useRef(false)

  // 初始模型：default_model（仍可用且属于当前分组）→ 当前分组首个可用
  // 模型（§2.16 主流程 1 + /pg 当前分组准入）。必须等钱包摘要与个人设置
  // 都落定再决定，否则会出现一次可见的闪跳。
  const availableKey = availableModels.join('\u0000')
  const defaultsReady = !settingsQuery.isPending && walletQuery.isSuccess
  useEffect(() => {
    if (!defaultsReady) return
    if (pickedModelRef.current && model !== '' && availableSet.has(model)) {
      return
    }
    const fallback =
      defaultModel !== '' && availableSet.has(defaultModel)
        ? defaultModel
        : availableModels[0] ?? ''
    if (fallback !== model) setModel(fallback)
    // eslint-disable-next-line react-hooks/exhaustive-deps -- availableSet 由 availableKey 派生
  }, [availableKey, defaultModel, defaultsReady, model])

  const storageKey =
    username !== '' && model !== ''
      ? playgroundSessionKey(username, model)
      : null

  // 每个「用户 + 模型」组合各自一条会话；切换模型时载入对应历史。
  useEffect(() => {
    if (storageKey === null) {
      setMessages([])
      return
    }
    setMessages(loadPlaygroundMessages(storageKey))
  }, [storageKey])

  function commit(next: PlaygroundMessage[]): void {
    setMessages(next)
    if (storageKey !== null) savePlaygroundMessages(storageKey, next)
  }

  function failFromError(error: PlaygroundError): void {
    if (error.status === 402) {
      setFailure({ kind: 'quota' })
      return
    }
    if (error.status === 403) {
      // 403 的语义由 problem type/code 决定，不能一律当「模型不可用」。
      if (error.code === 'model_not_available') {
        setFailure({ kind: 'forbidden' })
      } else if (error.code === 'session_required') {
        setFailure({ kind: 'session' })
      } else if (error.code === 'csrf_failed') {
        setFailure({ kind: 'csrf' })
      } else {
        setFailure({ kind: 'forbidden' })
      }
      return
    }
    if (error.status === 401) {
      setFailure({ kind: 'auth' })
      return
    }
    if (error.status >= 500) {
      setFailure({ kind: 'server', requestId: error.requestId })
      return
    }
    setFailure({
      kind: 'other',
      requestId: error.requestId,
      message: error.message,
    })
  }

  async function handleSend(): Promise<void> {
    const text = draft.trim()
    if (text === '' || streaming || model === '' || storageKey === null) return

    const prior = messages
    const userMessage: PlaygroundMessage = {
      id: newId(),
      role: 'user',
      content: text,
    }
    const assistantId = newId()
    setFailure(null)
    setAnnouncement('')
    setDraft('')
    setMessages([...prior, userMessage, {
      id: assistantId,
      role: 'assistant',
      content: '',
      model,
    }])
    setStreaming(true)

    const controller = new AbortController()
    controllerRef.current = controller
    let accumulated = ''

    const requestMessages: PlaygroundChatMessage[] = [
      ...prior.map((message) => ({
        role: message.role,
        content: message.content,
      })),
      { role: 'user', content: text },
    ]

    try {
      const result = await streamChatCompletion({
        model,
        messages: requestMessages,
        signal: controller.signal,
        onToken: (delta) => {
          accumulated += delta
          setMessages((previous) =>
            previous.map((message) =>
              message.id === assistantId
                ? { ...message, content: message.content + delta }
                : message,
            ),
          )
        },
      })

      const credits =
        result.usage !== undefined && result.requestId !== undefined
          ? await fetchTurnCredits(result.requestId)
          : null
      const assistant: PlaygroundMessage = {
        id: assistantId,
        role: 'assistant',
        content: result.content,
        model,
        stopped: false,
      }
      // EOF 未见 `[DONE]` = 流被截断。保留已生成内容，但必须标记「已中断」，
      // 不能伪装成一次成功回复（MEDIUM-1）。
      if (!result.done) assistant.interrupted = true
      if (result.usage !== undefined) {
        assistant.usage = {
          promptTokens: result.usage.prompt_tokens,
          completionTokens: result.usage.completion_tokens,
          credits,
        }
      }
      const hasAssistant = result.content !== '' || result.usage !== undefined
      commit(
        hasAssistant
          ? [...prior, userMessage, assistant]
          : [...prior, userMessage],
      )
      if (result.done) {
        setAnnouncement(t('account.playground.reply_ready'))
      } else {
        setAnnouncement(t('account.playground.interrupted'))
        push({ tone: 'info', message: t('account.playground.interrupted') })
      }
      // 余额在每次完成后刷新（不本地递增，避免与服务端结算分叉）。
      void queryClient.invalidateQueries({ queryKey: WALLET_SUMMARY_KEY })
    } catch (error) {
      if (isAbortError(error)) {
        // 停止：保留已生成内容并标记；服务端按实际 usage 结算，本地不估算。
        const assistant: PlaygroundMessage = {
          id: assistantId,
          role: 'assistant',
          content: accumulated,
          model,
          stopped: true,
        }
        const next =
          accumulated === ''
            ? [...prior, userMessage]
            : [...prior, userMessage, assistant]
        commit(next)
        setAnnouncement(t('account.playground.stopped'))
      } else if (error instanceof PlaygroundError) {
        if (error.status === 429) {
          // 限流不代表消息已受理：回滚本轮两条消息并保留输入。
          commit(prior)
          setDraft(text)
          push({ tone: 'error', message: t('account.playground.rate_limited') })
        } else {
          // 402/403/5xx：用户消息保留在会话里，错误卡解释原因。
          commit([...prior, userMessage])
          failFromError(error)
          setAnnouncement(t('account.playground.error_title'))
        }
      } else if (accumulated !== '') {
        // 传输层中断（断网/连接被切断）且已生成部分内容：保留并标记中断，
        // 不显示错误卡，也不要求用户重新发送已经拿到的内容。
        const assistant: PlaygroundMessage = {
          id: assistantId,
          role: 'assistant',
          content: accumulated,
          model,
          interrupted: true,
        }
        commit([...prior, userMessage, assistant])
        setAnnouncement(t('account.playground.interrupted'))
        push({
          tone: 'error',
          message: t('account.playground.interrupted_network'),
        })
      } else {
        // 尚未收到任何 token 的传输失败：回滚并保留草稿。
        commit(prior)
        setDraft(text)
        push({ tone: 'error', message: t('account.playground.offline') })
      }
    } finally {
      setStreaming(false)
      controllerRef.current = null
    }
  }

  function handleStop(): void {
    controllerRef.current?.abort()
  }

  function handleClear(): void {
    if (streaming || storageKey === null) return
    clearPlaygroundMessages(storageKey)
    setMessages([])
    setFailure(null)
    setAnnouncement(t('account.playground.session_cleared'))
  }

  const balance = walletQuery.data?.balance ?? null
  const noModels = availableModels.length === 0
  const sendDisabled =
    streaming || draft.trim() === '' || !groupReady || model === ''
  const modelsLoading = statusQuery.isPending || walletQuery.isPending

  return (
    <div className="mx-auto flex max-w-[1520px] flex-col gap-4">
      <header className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-page-title font-semibold text-ink-1">
          {t('account.playground.title')}
        </h1>
        <div className="flex flex-wrap items-center gap-2">
          {modelsLoading ? (
            <div
              aria-busy="true"
              data-testid="playground-model-skeleton"
              className="h-9 w-[220px] animate-pulse rounded-lg bg-surface-subtle motion-reduce:animate-none"
            />
          ) : groupUnknown || currentGroup === undefined ? (
            <p
              role="alert"
              data-testid="playground-group-unknown"
              className="max-w-[260px] text-meta text-danger"
            >
              {t('account.playground.group_unknown')}
            </p>
          ) : (
            <>
              <label className="sr-only" htmlFor="playground-model">
                {t('account.playground.select_model')}
              </label>
              <select
                id="playground-model"
                value={model}
                disabled={streaming || noModels}
                onChange={(event) => {
                  pickedModelRef.current = true
                  setModel(event.target.value)
                }}
                className="h-9 min-w-[220px] rounded-lg border border-border-default bg-surface-page px-2 text-body text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:opacity-60"
              >
                {noModels ? (
                  <option value="">{t('account.playground.no_models')}</option>
                ) : null}
                <optgroup label={currentGroup.name}>
                  {currentGroup.models.map((name) => (
                    <option key={name} value={name}>
                      {name}
                    </option>
                  ))}
                </optgroup>
              </select>
            </>
          )}

          <a
            href="/wallet"
            className="inline-flex h-9 items-center gap-1 rounded-lg border border-border-default bg-surface-card px-3 text-meta text-ink-2 hover:bg-surface-hover hover:text-ink-1"
            aria-label={t('account.playground.balance_link')}
          >
            <span className="text-ink-3">{t('account.playground.balance')}:</span>
            <span className="font-mono text-ink-1" data-testid="playground-balance">
              {balance === null ? '—' : formatCredits(balance, locale)}
            </span>
          </a>

          {messages.length > 0 ? (
            <Button
              variant="secondary"
              onClick={handleClear}
              disabled={streaming}
            >
              {t('account.playground.clear')}
            </Button>
          ) : null}
          <button
            type="button"
            aria-pressed={announceReplies}
            onClick={() => setAnnounceReplies((value) => !value)}
            className="rounded-lg border border-border-default bg-surface-card px-2 py-1 text-badge text-ink-2 hover:bg-surface-hover hover:text-ink-1 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
          >
            {t('account.playground.announce_toggle')}
          </button>
          <a
            href="/model-config?focus=default-model"
            className="text-meta text-brand underline-offset-2 hover:underline"
          >
            {t('account.playground.default_model_link')}
          </a>
        </div>
      </header>

      <div
        role="note"
        className="rounded-lg border border-info-line bg-info-bg px-3 py-2 text-meta text-ink-2"
      >
        <p>{t('account.playground.billing_notice')}</p>
        <p>{t('account.playground.billing_quota')}</p>
        <p>{t('account.playground.billing_violation')}</p>
      </div>

      <p role="status" aria-live="polite" className="sr-only">
        {announcement}
      </p>

      <section
        aria-label={t('account.playground.messages_label')}
        role="log"
        aria-live={streaming || !announceReplies ? 'off' : 'polite'}
        aria-relevant="additions"
        className="min-h-[320px] space-y-3 rounded-xl border border-border-hairline bg-surface-card p-4"
      >
        {messages.length === 0 ? (
          <EmptyState
            onPick={(prompt) => {
              setDraft(prompt)
              textareaRef.current?.focus()
            }}
          />
        ) : (
          messages.map((message) => (
            <article
              key={message.id}
              data-testid={`message-${message.role}`}
              className={[
                'rounded-lg px-3 py-2',
                message.role === 'user'
                  ? 'ml-auto max-w-[85%] bg-surface-subtle text-ink-1'
                  : 'mr-auto max-w-full bg-surface-card text-ink-1',
              ].join(' ')}
            >
              <p className="mb-1 text-badge font-medium text-ink-3">
                {message.role === 'user'
                  ? t('account.playground.user_label')
                  : t('account.playground.assistant_label')}
                {message.role === 'assistant' && message.model !== undefined
                  ? ` · ${message.model}`
                  : ''}
              </p>
              {message.role === 'assistant' ? (
                <MessageContent text={message.content} />
              ) : (
                <p className="whitespace-pre-wrap break-words text-body text-ink-1">
                  {message.content}
                </p>
              )}
              {message.stopped === true ? (
                <p className="mt-1 text-meta text-warning">
                  {t('account.playground.stopped')}
                </p>
              ) : null}
              {message.interrupted === true ? (
                <p className="mt-1 text-meta text-warning">
                  {t('account.playground.interrupted')}
                </p>
              ) : null}
              {message.usage !== undefined ? (
                <p className="mt-1 font-mono text-badge text-ink-3">
                  {t('account.playground.usage_prompt', {
                    count: message.usage.promptTokens,
                  })}
                  {' · '}
                  {t('account.playground.usage_completion', {
                    count: message.usage.completionTokens,
                  })}
                  {' · '}
                  {message.usage.credits === null
                    ? t('account.playground.usage_pending')
                    : t('account.playground.turn_credits', {
                        count: message.usage.credits,
                      })}
                  {' · '}
                  {t('account.playground.usage_token_null')}
                </p>
              ) : null}
            </article>
          ))
        )}
        {streaming ? (
          <p className="text-meta text-ink-3" data-testid="playground-streaming">
            {t('account.playground.streaming')}
            <span
              aria-hidden="true"
              className="ml-1 inline-block animate-pulse motion-reduce:animate-none"
            >
              ▍
            </span>
          </p>
        ) : null}
      </section>

      {failure !== null ? (
        <FailureCard failure={failure} />
      ) : null}

      <section className="rounded-xl border border-border-hairline bg-surface-card p-3">
        <label
          htmlFor="playground-input"
          className="text-meta font-medium text-ink-1"
        >
          {t('account.playground.input_label')}
        </label>
        <textarea
          id="playground-input"
          ref={textareaRef}
          value={draft}
          rows={3}
          onChange={(event) => setDraft(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter' && !event.shiftKey) {
              event.preventDefault()
              void handleSend()
            }
          }}
          placeholder={
            groupUnknown
              ? t('account.playground.group_unknown')
              : noModels
                ? t('account.playground.no_models')
                : t('account.playground.input_placeholder')
          }
          aria-describedby="playground-input-hint"
          className="mt-1 min-h-[72px] w-full resize-y rounded-lg border border-border-default bg-surface-page px-3 py-2 text-body text-ink-1 placeholder:text-ink-3 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        />
        <div className="mt-2 flex items-center justify-between gap-2">
          <p id="playground-input-hint" className="text-meta text-ink-3">
            {t('account.playground.input_hint')}
          </p>
          <div className="flex gap-2">
            {streaming ? (
              <Button variant="secondary" onClick={handleStop}>
                {t('account.playground.stop')}
              </Button>
            ) : null}
            <Button onClick={() => void handleSend()} disabled={sendDisabled}>
              {t('account.playground.send')}
            </Button>
          </div>
        </div>
      </section>
    </div>
  )
}

function EmptyState({ onPick }: { onPick: (prompt: string) => void }) {
  const { t } = useI18n()
  const examples = [
    t('account.playground.example_1'),
    t('account.playground.example_2'),
    t('account.playground.example_3'),
  ]
  return (
    <div className="mx-auto max-w-xl py-8 text-center" data-testid="playground-welcome">
      <h2 className="text-section font-semibold text-ink-1">
        {t('account.playground.welcome_title')}
      </h2>
      <p className="mt-1 text-meta text-ink-2">
        {t('account.playground.welcome_description')}
      </p>
      <p className="mt-4 text-meta text-ink-3">
        {t('account.playground.example_label')}
      </p>
      <div className="mt-2 flex flex-col gap-2">
        {examples.map((example) => (
          <button
            key={example}
            type="button"
            onClick={() => onPick(example)}
            className="rounded-lg border border-border-default bg-surface-page px-3 py-2 text-left text-meta text-ink-2 hover:bg-surface-hover hover:text-ink-1"
          >
            {example}
          </button>
        ))}
      </div>
    </div>
  )
}

function FailureCard({ failure }: { failure: PlaygroundFailure }) {
  const { t } = useI18n()

  if (failure.kind === 'quota') {
    return (
      <div role="alert" className="rounded-lg border border-warning-line bg-warning-bg p-3 text-meta">
        <p className="font-medium text-ink-1">
          {t('account.playground.insufficient')}
        </p>
        <a
          href="/wallet"
          className="mt-2 inline-block text-brand underline-offset-2 hover:underline"
        >
          {t('account.playground.topup_action')}
        </a>
      </div>
    )
  }
  if (failure.kind === 'forbidden') {
    return (
      <div role="alert" className="rounded-lg border border-warning-line bg-warning-bg p-3 text-meta">
        <p className="font-medium text-ink-1">
          {t('account.playground.unavailable')}
        </p>
        <p className="mt-1 text-meta text-ink-2">
          {t('account.playground.no_bill')}
        </p>
      </div>
    )
  }
  if (failure.kind === 'auth') {
    return (
      <div role="alert" className="rounded-lg border border-danger-line bg-danger-bg p-3 text-meta">
        <p className="font-medium text-ink-1">
          {t('common.error401.title')}
        </p>
        <a
          href="/sign-in"
          className="mt-2 inline-block text-brand underline-offset-2 hover:underline"
        >
          {t('common.error401.sign_in')}
        </a>
      </div>
    )
  }
  if (failure.kind === 'session') {
    return (
      <div role="alert" className="rounded-lg border border-danger-line bg-danger-bg p-3 text-meta">
        <p className="font-medium text-ink-1">
          {t('account.playground.session_required')}
        </p>
      </div>
    )
  }
  if (failure.kind === 'csrf') {
    return (
      <div role="alert" className="rounded-lg border border-danger-line bg-danger-bg p-3 text-meta">
        <p className="font-medium text-ink-1">
          {t('account.playground.csrf_failed')}
        </p>
      </div>
    )
  }
  return (
    <div role="alert" className="rounded-lg border border-danger-line bg-danger-bg p-3 text-meta">
      <p className="font-medium text-ink-1">
        {t('account.playground.error_title')}
      </p>
      {failure.kind === 'other' && failure.message !== undefined ? (
        <p className="mt-1 text-meta text-ink-2">{failure.message}</p>
      ) : null}
      {failure.requestId !== undefined ? (
        <p className="mt-1 font-mono text-badge text-ink-3">
          {t('account.playground.request_id', { id: failure.requestId })}
        </p>
      ) : null}
      <a
        href="/wallet"
        className="mt-2 inline-block text-meta text-brand underline-offset-2 hover:underline"
      >
        {t('account.playground.balance_link')}
      </a>
    </div>
  )
}
