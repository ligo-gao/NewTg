/**
 * 判定引擎 code → 词典 key 的唯一映射。
 *
 * 服务端只下发 `owner` / `sub` / `rule` / `signals[].code` / `signals[].vote`
 * 与少量 `params`，从不下发自然语言。所有人类可读文本都经这里取
 * `admin.failure_triage.*`，组件里不得出现字面句子。任何未收录的取值都必须
 * 落到 `unknown` 家族，绝不把原始 code 渲染给正在排障的管理员。
 */
import type { components } from '@/api/schema'
import type { TranslateFn, TranslateValues } from '@/shared/lib/i18n-context'

export type TriageOwner = components['schemas']['TriageOwner']
export type TriageRule = components['schemas']['TriageRule']
export type TriageSignal = components['schemas']['TriageSignal']
export type TriageVote = components['schemas']['TriageVote']
export type TriageVerdict = components['schemas']['TriageVerdict']
export type TriageEvidence = components['schemas']['TriageEvidence']

const PREFIX = 'admin.failure_triage'

const OWNERS: readonly TriageOwner[] = ['user', 'channel', 'gateway', 'unknown']

const RULES: readonly TriageRule[] = [
  'USR-01',
  'USR-02',
  'CH-01',
  'CH-02',
  'GW-01',
  'GW-02',
  'UNKNOWN',
]

/** 引擎可能下发的全部 `Verdict.Sub`（含 reject_stage 与空串兜底）。 */
const SUBS: readonly string[] = [
  'request_structure',
  'quota',
  'rate_limit',
  'auth',
  'model_admission',
  'channel_selection',
  'upstream_auth',
  'upstream_server',
  'protocol_conversion',
  'unsupported',
  'insufficient_evidence',
  'unknown',
]

/** `backend/internal/triage/triage.go` 的 15 个 Signal.Code。 */
const SIGNAL_CODES: readonly string[] = [
  'request_structure_invalid',
  'request_structure_valid',
  'request_structure_unverified',
  'refused_before_upstream',
  'no_upstream_attempt',
  'no_upstream_response',
  'no_upstream_server_error',
  'upstream_status',
  'upstream_auth_rejected',
  'upstream_server_error',
  'attempt_count',
  'conversion_applied',
  'conversion_absent',
  'error_class',
  'error_class_unsupported',
]

const VOTES: readonly TriageVote[] = [
  'user',
  'channel',
  'gateway',
  'exclude-user',
  'exclude-channel',
  'neutral',
]

function normalizeOwner(owner: string): TriageOwner {
  return (OWNERS as readonly string[]).includes(owner)
    ? (owner as TriageOwner)
    : 'unknown'
}

function normalizeRule(rule: string): TriageRule {
  return (RULES as readonly string[]).includes(rule)
    ? (rule as TriageRule)
    : 'UNKNOWN'
}

function normalizeSub(sub: string): string {
  return SUBS.includes(sub) ? sub : 'unknown'
}

function normalizeVote(vote: string): TriageVote {
  return (VOTES as readonly string[]).includes(vote)
    ? (vote as TriageVote)
    : 'neutral'
}

export interface RuleCopy {
  titleKey: string
  summaryKey: string
}

export function ruleCopy(rule: string): RuleCopy {
  const known = normalizeRule(rule)
  return {
    titleKey: `${PREFIX}.rule.${known}.title`,
    summaryKey: `${PREFIX}.rule.${known}.summary`,
  }
}

/** 责任方的名词标签（用户请求 / 上游渠道 / 网关自身 / 证据不足）。 */
export function ownerLabelKey(owner: string): string {
  return `${PREFIX}.owner.${normalizeOwner(owner)}`
}

/** 规则内细分原因的标签；空串与未知值都落到 `sub.unknown`。 */
export function subLabelKey(sub: string): string {
  return `${PREFIX}.sub.${normalizeSub(sub)}`
}

/** 判定依据的投票标签（指向用户 / 排除渠道 / 中性…）。 */
export function voteLabelKey(vote: string): string {
  return `${PREFIX}.vote.${normalizeVote(vote)}`
}

/**
 * 投票颜色：只有指向责任方的方向票使用判定色；`exclude-*` 是排除性证据，
 * 以墨色呈现；`neutral` 用更浅的墨色。绝不引入第四种判定色。
 */
export function voteClass(vote: string): string {
  switch (normalizeVote(vote)) {
    case 'user':
      return 'text-verdict-user'
    case 'channel':
      return 'text-verdict-channel'
    case 'gateway':
      return 'text-verdict-gateway'
    case 'exclude-user':
    case 'exclude-channel':
      return 'text-ink-2'
    default:
      return 'text-ink-3'
  }
}

/** 印章底色：unknown 不是第四类判决，因此用中性墨色而不是新增色相。 */
export function ownerStampClass(owner: string): string {
  switch (normalizeOwner(owner)) {
    case 'user':
      return 'bg-verdict-user'
    case 'channel':
      return 'bg-verdict-channel'
    case 'gateway':
      return 'bg-verdict-gateway'
    default:
      return 'bg-ink-2'
  }
}

/** 列表行 / 圆点的文字与底色，与印章保持同一套语义。 */
export function ownerTextClass(owner: string): string {
  switch (normalizeOwner(owner)) {
    case 'user':
      return 'text-verdict-user'
    case 'channel':
      return 'text-verdict-channel'
    case 'gateway':
      return 'text-verdict-gateway'
    default:
      return 'text-ink-2'
  }
}

export function ownerDotClass(owner: string): string {
  switch (normalizeOwner(owner)) {
    case 'user':
      return 'bg-verdict-user'
    case 'channel':
      return 'bg-verdict-channel'
    case 'gateway':
      return 'bg-verdict-gateway'
    default:
      return 'bg-ink-2'
  }
}

function stringParam(
  params: Record<string, unknown> | undefined,
  key: string,
): string | undefined {
  const value = params?.[key]
  return typeof value === 'string' && value !== '' ? value : undefined
}

function numberParam(
  params: Record<string, unknown> | undefined,
  key: string,
): number | undefined {
  const value = params?.[key]
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined
}

export interface SignalCopy {
  factKey: string
  noteKey: string
  voteKey: string
  voteClass: string
  /** 已解析的插值值；模板里的占位符一定有对应值，不会漏 `{name}`。 */
  values: TranslateValues
}

/**
 * 把一条 signal 映射成词典 key 与插值值。
 *
 * `params` 是可选的：缺省时用同一份 `evidence` 兜底，因此像
 * `request_structure_valid`（本就无 params）或 `conversion_absent`（路由
 * 标签缺失时 params 为 nil）都不会把 `{from}` 之类的占位符漏到界面上。
 */
export function signalCopy(
  signal: TriageSignal,
  evidence: TriageEvidence,
  t: TranslateFn,
): SignalCopy {
  const code = SIGNAL_CODES.includes(signal.code) ? signal.code : 'unknown'
  const values: TranslateValues = {}
  const params = signal.params

  switch (code) {
    case 'refused_before_upstream':
      values.stage = t(
        subLabelKey(stringParam(params, 'stage') ?? 'unknown'),
      )
      break
    case 'no_upstream_attempt':
    case 'no_upstream_response':
    case 'no_upstream_server_error':
    case 'attempt_count':
      values.attempts = numberParam(params, 'attempts') ?? evidence.attempts
      break
    case 'upstream_status':
    case 'upstream_auth_rejected':
    case 'upstream_server_error':
      values.upstream_status =
        numberParam(params, 'upstream_status') ?? evidence.upstream_status
      break
    case 'conversion_applied':
    case 'conversion_absent':
      values.from =
        stringParam(params, 'from') ??
        (evidence.route_from !== ''
          ? evidence.route_from
          : t(`${PREFIX}.values.protocol_unknown`))
      values.to =
        stringParam(params, 'to') ??
        (evidence.route_to !== ''
          ? evidence.route_to
          : t(`${PREFIX}.values.protocol_unknown`))
      break
    case 'error_class':
    case 'error_class_unsupported':
      values.class =
        stringParam(params, 'class') ??
        (evidence.error_class !== '' ? evidence.error_class : '—')
      break
    default:
      break
  }

  return {
    factKey: `${PREFIX}.signal.${code}.fact`,
    noteKey: `${PREFIX}.signal.${code}.note`,
    voteKey: voteLabelKey(signal.vote),
    voteClass: voteClass(signal.vote),
    values,
  }
}

export type TriageActionTarget =
  | { kind: 'copy'; source: 'summary' | 'reply' | 'request' | 'rule' }
  | { kind: 'link'; href: string }

export interface TriageActionCopy {
  titleKey: string
  noteKey: string
  buttonKey: string
  target: TriageActionTarget
}

function action(
  owner: TriageOwner,
  index: number,
  target: TriageActionTarget,
): TriageActionCopy {
  return {
    titleKey: `${PREFIX}.action.${owner}.${index}.title`,
    noteKey: `${PREFIX}.action.${owner}.${index}.note`,
    buttonKey: `${PREFIX}.action.${owner}.${index}.button`,
    target,
  }
}

/**
 * 「Open usage logs」链接带上 requestId：目标页已接受 request_id 过滤
 * （`features/logs/search.ts`），管理员不必再从案件头部复制粘贴 id。
 * requestId 经 URL 编码；缺失时退回未过滤列表。
 */
function usageLogsHref(requestId: string): string {
  if (requestId === '') return '/dashboard/usage-logs'
  return `/dashboard/usage-logs?request_id=${encodeURIComponent(requestId)}`
}

/**
 * 建议动作。每个按钮都必须在客户端真实可用：复制类动作写入剪贴板，链接类
 * 动作指向已有的控制台页面。没有实现的入口（例如「脱敏请求结构」视图）
 * 不放进动作列表，避免排障时点到死按钮。
 */
export function actionCopies(owner: string, requestId = ''): TriageActionCopy[] {
  const known = normalizeOwner(owner)
  switch (known) {
    case 'user':
      return [
        action(known, 1, { kind: 'copy', source: 'summary' }),
        action(known, 2, {
          kind: 'link',
          href: usageLogsHref(requestId),
        }),
        action(known, 3, { kind: 'copy', source: 'reply' }),
      ]
    case 'channel':
      return [
        action(known, 1, { kind: 'copy', source: 'summary' }),
        action(known, 2, { kind: 'link', href: '/admin/channels' }),
        action(known, 3, { kind: 'copy', source: 'request' }),
      ]
    case 'gateway':
      return [
        action(known, 1, { kind: 'copy', source: 'summary' }),
        action(known, 2, {
          kind: 'link',
          href: usageLogsHref(requestId),
        }),
        action(known, 3, { kind: 'copy', source: 'rule' }),
      ]
    default:
      return [
        action('unknown', 1, { kind: 'copy', source: 'summary' }),
        action('unknown', 2, {
          kind: 'link',
          href: usageLogsHref(requestId),
        }),
        action('unknown', 3, { kind: 'copy', source: 'request' }),
      ]
  }
}

export interface VerdictCopy {
  titleKey: string
  summaryKey: string
  subKey: string
  signals: SignalCopy[]
  actions: TriageActionCopy[]
}

/** 一次判定在界面上需要的全部词典 key。 */
export function verdictCopy(
  verdict: TriageVerdict,
  evidence: TriageEvidence,
  t: TranslateFn,
  requestId = '',
): VerdictCopy {
  const rule = ruleCopy(verdict.rule)
  return {
    titleKey: rule.titleKey,
    summaryKey: rule.summaryKey,
    subKey: subLabelKey(verdict.sub),
    signals: verdict.signals.map((signal) => signalCopy(signal, evidence, t)),
    actions: actionCopies(verdict.owner, requestId),
  }
}
