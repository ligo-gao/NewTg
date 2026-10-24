/**
 * 案卷：结论 → 判定依据 → 建议动作，外加事实条与规则脚注。
 *
 * 三层结构是产品本身：先给一个可转述的结论，再列引擎据以判定的原子事实，
 * 最后给管理员可直接执行的动作。所有文案经 `verdict-copy.ts` 取词典。
 */
import { Button } from '@/shared/ui/button'
import { useToastStore } from '@/shared/ui/toast'
import { formatRelativeTime } from '@/shared/lib/datetime'
import { useI18n } from '@/shared/lib/i18n-context'

import type { RequestTriage } from './api'
import type { TriageActionCopy, TriageEvidence, TriageVerdict } from './verdict-copy'
import {
  ownerLabelKey,
  ownerStampClass,
  ruleCopy,
  verdictCopy,
} from './verdict-copy'

function formatDateTime(value: string, locale: string): string {
  const timestamp = Date.parse(value)
  if (Number.isNaN(timestamp)) return '—'
  return new Intl.DateTimeFormat(locale, {
    dateStyle: 'medium',
    timeStyle: 'medium',
  }).format(timestamp)
}

interface Fact {
  key: string
  label: string
  value: string
  mono?: boolean
}

/** 空串与 null 都表示「无值」；退款/解析失败的行会刻意存空 model。 */
function displayValue(value: string | null | undefined): string {
  return value === null || value === undefined || value === '' ? '—' : value
}

function factValue(evidence: TriageEvidence, t: (key: string, values?: Record<string, string | number>) => string): {
  route: string
  channel: string
  upstreamStatus: string
  attempts: string
  billing: string
} {
  const conversion = evidence.conversion
  let route = '—'
  if (evidence.channel_id === null && evidence.upstream_status === 0) {
    route = t('admin.failure_triage.values.route_not_reached')
  } else if (conversion.length > 0) {
    const first = conversion[0]
    const last = conversion[conversion.length - 1]
    route = `${first?.from ?? evidence.route_from} → ${last?.to ?? evidence.route_to}`
  } else if (evidence.route_from !== '' && evidence.route_to !== '') {
    route = t('admin.failure_triage.values.route_direct', {
      from: evidence.route_from,
      to: evidence.route_to,
    })
  }

  const channel =
    evidence.channel_id === null
      ? '—'
      : t('admin.failure_triage.values.channel', {
          type: evidence.channel_type !== '' ? evidence.channel_type : '—',
          id: evidence.channel_id,
        })

  const upstreamStatus =
    evidence.upstream_status === 0
      ? t('admin.failure_triage.values.upstream_none')
      : String(evidence.upstream_status)

  const attempts =
    evidence.attempts === 0
      ? t('admin.failure_triage.values.attempts_none')
      : t('admin.failure_triage.values.attempts', { count: evidence.attempts })

  const billing =
    evidence.billing.refunded > 0
      ? t('admin.failure_triage.values.billing_refunded')
      : evidence.billing.cost > 0
        ? t('admin.failure_triage.values.billing_charged')
        : t('admin.failure_triage.values.billing_none')

  return { route, channel, upstreamStatus, attempts, billing }
}

function CaseError({ onRetry }: { onRetry: () => void }) {
  const { t } = useI18n()
  return (
    <div role="alert" className="flex items-center gap-3 py-8 text-meta">
      <span>{t('admin.failure_triage.error.case')}</span>
      <Button variant="secondary" onClick={onRetry}>
        {t('admin.failure_triage.error.retry')}
      </Button>
    </div>
  )
}

export interface CaseFileProps {
  triage: RequestTriage
  onRetry: () => void
}

export function CaseFile({ triage, onRetry }: CaseFileProps) {
  const { t, locale } = useI18n()
  const push = useToastStore((state) => state.push)
  const evidence = triage.evidence
  const verdict = triage.verdict
  if (evidence === undefined || verdict === undefined) {
    return <CaseError onRetry={onRetry} />
  }

  return <CaseFileBody triage={triage} evidence={evidence} verdict={verdict} push={push} t={t} locale={locale} />
}

type Translate = ReturnType<typeof useI18n>['t']
type Push = ReturnType<typeof useToastStore.getState>['push']

function CaseFileBody({
  triage,
  evidence,
  verdict,
  push,
  t,
  locale,
}: {
  triage: RequestTriage
  evidence: TriageEvidence
  verdict: TriageVerdict
  push: Push
  t: Translate
  locale: string
}) {
  const copy = verdictCopy(verdict, evidence, t, triage.request_id)
  const rule = ruleCopy(verdict.rule)
  const requestId = triage.request_id
  const log = triage.log
  const createdAt = evidence.created_at
  const createdAtMs = Date.parse(createdAt)
  const createdAtTitle = Number.isNaN(createdAtMs)
    ? undefined
    : new Date(createdAtMs).toISOString()

  const values = factValue(evidence, t)
  const facts: Fact[] = [
    {
      key: 'model',
      label: t('admin.failure_triage.facts.model'),
      value: displayValue(log?.model),
    },
    {
      key: 'upstream_model',
      label: t('admin.failure_triage.facts.upstream_model'),
      value: displayValue(log?.upstream),
    },
    { key: 'channel', label: t('admin.failure_triage.facts.channel'), value: values.channel },
    {
      key: 'endpoint',
      label: t('admin.failure_triage.facts.endpoint'),
      value: evidence.endpoint !== '' ? evidence.endpoint : '—',
      mono: true,
    },
    { key: 'route', label: t('admin.failure_triage.facts.route'), value: values.route },
    {
      key: 'upstream_status',
      label: t('admin.failure_triage.facts.upstream_status'),
      value: values.upstreamStatus,
      mono: true,
    },
    {
      key: 'attempts',
      label: t('admin.failure_triage.facts.attempts'),
      value: values.attempts,
    },
    { key: 'billing', label: t('admin.failure_triage.facts.billing'), value: values.billing },
  ]

  const copyPayloads = {
    summary: t('admin.failure_triage.copy.summary', {
      title: t(rule.titleKey),
      summary: t(rule.summaryKey),
      requestId,
      rule: verdict.rule,
    }),
    reply: t('admin.failure_triage.copy.reply', {
      requestId,
      summary: t(rule.summaryKey),
    }),
    rule: t('admin.failure_triage.copy.rule_text', {
      rule: verdict.rule,
      summary: t(rule.summaryKey),
    }),
    request: requestId,
  }

  function copyToClipboard(value: string): void {
    const clipboard = navigator.clipboard
    if (clipboard === undefined) {
      // 自建 HTTP 部署没有 Clipboard API；静默失败会让按钮看起来坏了。
      push({ tone: 'error', message: t('admin.failure_triage.case.copy_failed') })
      return
    }
    void clipboard.writeText(value).then(
      () => push({ tone: 'success', message: t('admin.failure_triage.case.copied') }),
      () =>
        push({
          tone: 'error',
          message: t('admin.failure_triage.case.copy_failed'),
        }),
    )
  }

  function renderAction(target: TriageActionCopy['target'], action: TriageActionCopy) {
    if (target.kind === 'link') {
      return (
        <a
          href={target.href}
          className="ml-auto inline-flex h-9 flex-none items-center justify-center rounded-lg border border-border-default bg-surface-card px-3.5 text-body font-medium text-ink-1 hover:bg-surface-hover focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {t(action.buttonKey)}
        </a>
      )
    }
    return (
      <Button
        variant="secondary"
        className="ml-auto flex-none"
        onClick={() => copyToClipboard(copyPayloads[target.source])}
      >
        {t(action.buttonKey)}
      </Button>
    )
  }

  return (
    <article aria-live="polite" className="min-w-0 px-6 pt-5 max-[760px]:px-4" data-testid="case-file">
      <header className="flex flex-wrap items-baseline justify-between gap-4 border-b-0 pb-3 text-badge text-ink-2">
        <span className="inline-flex items-baseline gap-1.5 tabular-nums">
          <span title={createdAtTitle}>{formatDateTime(createdAt, locale)}</span>
          <span className="text-ink-3">· {formatRelativeTime(createdAt, locale)}</span>
        </span>
        <span className="inline-flex min-w-0 items-center gap-2">
          <code className="truncate font-mono text-ink-2">{requestId}</code>
          <button
            type="button"
            className="h-6 rounded-[5px] border border-border-hairline bg-surface-card px-2 text-badge text-ink-2 hover:border-border-default hover:text-ink-1"
            onClick={() => {
              copyToClipboard(requestId)
            }}
          >
            {t('admin.failure_triage.case.copy_request_id')}
          </button>
        </span>
      </header>

      <div className="grid grid-cols-[minmax(0,1fr)_168px] items-start gap-9 border-t border-border-hairline pt-[18px] pb-[22px] max-[760px]:grid-cols-1 max-[760px]:gap-[18px]">
        <div className="min-w-0">
          <h2 className="font-serif text-figure-md font-semibold tracking-[-0.01em] text-balance text-ink-1 max-[760px]:text-stat">
            {t(copy.titleKey)}
          </h2>
          <p className="mt-2.5 max-w-[58ch] text-body text-ink-2">
            {t(copy.summaryKey)}
          </p>
        </div>
        <div
          data-testid="verdict-stamp"
          data-owner={verdict.owner}
          className={[
            'flex w-[168px] flex-col justify-self-end self-start rounded px-[15px] py-[13px] max-[760px]:justify-self-start',
            ownerStampClass(verdict.owner),
          ].join(' ')}
        >
          <div className="flex flex-col gap-0.5 text-on-brand">
            <span className="text-badge tracking-[0.12em] opacity-85">
              {t('admin.failure_triage.stamp.owner_label')}
            </span>
            <strong className="text-section font-bold">
              {t(ownerLabelKey(verdict.owner))}
            </strong>
            <span className="text-badge opacity-85">
              {t('admin.failure_triage.stamp.sub_label', { sub: t(copy.subKey) })}
            </span>
          </div>
        </div>
      </div>

      <dl className="grid grid-cols-4 gap-px border-y border-border-hairline bg-border-hairline max-[1100px]:grid-cols-2 max-[760px]:grid-cols-1">
        {facts.map((fact) => (
          <div key={fact.key} className="min-w-0 bg-surface-card px-3.5 py-2.5" data-testid={`fact-${fact.key}`}>
            <dt className="text-badge text-ink-3">{fact.label}</dt>
            <dd
              title={fact.value}
              className={[
                'mt-0.5 truncate text-ink-1',
                fact.mono ? 'font-mono text-badge' : 'text-meta',
              ].join(' ')}
            >
              {fact.value}
            </dd>
          </div>
        ))}
      </dl>

      <div className="grid grid-cols-[minmax(0,1fr)_300px] max-[1240px]:grid-cols-1">
        <section className="min-w-0 border-r border-border-hairline py-[18px] pr-[30px] max-[1240px]:border-r-0 max-[1240px]:border-b max-[1240px]:pr-0">
          <h3 className="mb-1.5 text-badge font-semibold text-ink-2">
            {t('admin.failure_triage.sections.evidence')}
          </h3>
          <ul>
            {copy.signals.map((signal, index) => (
              <li
                key={`${signal.factKey}-${index}`}
                data-testid="evidence-row"
                className="grid grid-cols-[104px_minmax(0,1fr)] border-b border-border-hairline py-3 last:border-b-0 last:pb-1 max-[760px]:grid-cols-1 max-[760px]:gap-1.5"
              >
                <span
                  className={[
                    'pr-[18px] text-right text-badge font-semibold whitespace-nowrap max-[760px]:pr-0 max-[760px]:text-left',
                    signal.voteClass,
                  ].join(' ')}
                >
                  {t(signal.voteKey)}
                </span>
                <div className="min-w-0 border-l border-border-hairline pl-[18px] max-[760px]:border-l-0 max-[760px]:pl-0">
                  <div className="text-meta font-medium text-ink-1">
                    {t(signal.factKey, signal.values)}
                  </div>
                  <div className="mt-0.5 text-badge text-ink-2">
                    {t(signal.noteKey, signal.values)}
                  </div>
                </div>
              </li>
            ))}
          </ul>
        </section>

        <section className="min-w-0 py-[18px] pl-[30px] max-[1240px]:pl-0">
          <h3 className="mb-1.5 text-badge font-semibold text-ink-2">
            {t('admin.failure_triage.sections.actions')}
          </h3>
          <ul className="flex flex-col">
            {copy.actions.map((action, index) => (
              <li
                key={`${action.titleKey}-${index}`}
                className="flex items-start gap-3 border-b border-border-hairline py-3 last:border-b-0"
              >
                <div className="min-w-0">
                  <div className="text-meta font-semibold text-ink-1">
                    {t(action.titleKey)}
                  </div>
                  <div className="mt-0.5 text-badge text-ink-2">{t(action.noteKey)}</div>
                </div>
                {renderAction(action.target, action)}
              </li>
            ))}
          </ul>
        </section>
      </div>

      <footer className="flex items-baseline gap-2.5 border-t border-border-hairline py-3 text-badge text-ink-3">
        <code className="whitespace-nowrap font-mono text-ink-2">{verdict.rule}</code>
        <span className="min-w-0">{t(rule.summaryKey)}</span>
      </footer>
    </article>
  )
}
