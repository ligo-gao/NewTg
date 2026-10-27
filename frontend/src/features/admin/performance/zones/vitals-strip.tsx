import { useI18n } from '@/shared/lib/i18n-context'
import type { TranslateFn } from '@/shared/lib/i18n-context'
import { StatusBadge } from '@/shared/ui/status-badge'

import type { PerfModels, PerfOverview } from '../api'
import { Sparkline, type SparklineTone } from '../charts/sparkline'
import { formatPercent, vitalTone } from '../utils'

/**
 * 五格体检条：四格来自 Prometheus（网关 / 数据库 / Redis / 主机），第五格由
 * 模型汇总派生并恒定在末尾（Task 15 brief + adjudications §1–3）。
 *
 * 这一层只做「契约形状 → 文案」的映射，所有阈值与色调都来自既有
 * `vitalTone`，数字格式化复用 `utils.ts`——不在组件里另造一套口径。
 */

type Vital = PerfOverview['vitals'][number]
type VitalStatus = Vital['status']
type ModelSummary = PerfModels['summary']

/** 本页只认这四个 key；其它 key 按 API 顺序跳过，不臆造格子。 */
const PROMETHEUS_VITALS = [
  'gateway.rps',
  'db.pool',
  'redis.hit_rate',
  'host.disk',
] as const

type PrometheusVitalKey = (typeof PROMETHEUS_VITALS)[number]

const NAME_KEYS: Record<PrometheusVitalKey, string> = {
  'gateway.rps': 'admin.performance.vitals.gateway',
  'db.pool': 'admin.performance.vitals.postgres',
  'redis.hit_rate': 'admin.performance.vitals.redis',
  'host.disk': 'admin.performance.vitals.host',
}

function isPrometheusVitalKey(key: string): key is PrometheusVitalKey {
  return (PROMETHEUS_VITALS as readonly string[]).includes(key)
}

/** details 是 `{key,value,unit}` 数组；同一 key 只取第一条有限值。 */
function detailMap(vital: Vital): Map<string, number> {
  const map = new Map<string, number>()
  for (const detail of vital.details) {
    if (!Number.isFinite(detail.value) || map.has(detail.key)) continue
    map.set(detail.key, detail.value)
  }
  return map
}

function finiteDetail(
  details: Map<string, number>,
  key: string,
): number | undefined {
  const value = details.get(key)
  return value === undefined ? undefined : value
}

/** 主读数：永远来自 `vital.value`，null 才是 `—`（sourceDown 之外也可能）。 */
function figureFor(key: PrometheusVitalKey, value: number | null): string {
  if (value === null || !Number.isFinite(value)) return '—'
  switch (key) {
    case 'gateway.rps':
      return value.toFixed(1)
    case 'db.pool':
      return String(Math.round(value))
    case 'redis.hit_rate':
    case 'host.disk':
      return (value * 100).toFixed(1)
  }
}

/** 单位：`unit_conn` 的 `{max}` 缺数据时退化成 `—`，不整格消失。 */
function unitFor(
  key: PrometheusVitalKey,
  details: Map<string, number>,
  t: TranslateFn,
): string {
  switch (key) {
    case 'gateway.rps':
      return t('admin.performance.vitals.unit_rps')
    case 'db.pool': {
      const max = finiteDetail(details, 'max_backends')
      return t('admin.performance.vitals.unit_conn', {
        max: max === undefined ? '—' : String(Math.round(max)),
      })
    }
    case 'redis.hit_rate':
      return t('admin.performance.vitals.unit_hit')
    case 'host.disk':
      return t('admin.performance.vitals.unit_disk')
  }
}

/**
 * 副标题只在它需要的 details **全部**存在时渲染；缺任意一条整行不渲染，
 * 不显示半截 `—`（adjudications §3）。
 */
function subtitleFor(
  key: PrometheusVitalKey,
  details: Map<string, number>,
  t: TranslateFn,
): string | null {
  switch (key) {
    case 'gateway.rps': {
      const p95 = finiteDetail(details, 'p95')
      const errors = finiteDetail(details, 'errors')
      if (p95 === undefined || errors === undefined) return null
      return t('admin.performance.vitals.sub_gateway', {
        p95: `${p95.toFixed(2)}s`,
        errors: formatPercent(errors),
      })
    }
    case 'db.pool': {
      const tps = finiteDetail(details, 'tps')
      const backends = finiteDetail(details, 'backends')
      const max = finiteDetail(details, 'max_backends')
      if (tps === undefined || backends === undefined || max === undefined) {
        return null
      }
      return t('admin.performance.vitals.sub_postgres', {
        tps: String(Math.round(tps)),
        backends: String(Math.round(backends)),
        max_backends: String(Math.round(max)),
      })
    }
    case 'redis.hit_rate': {
      const ops = finiteDetail(details, 'ops')
      const clients = finiteDetail(details, 'clients')
      if (ops === undefined || clients === undefined) return null
      return t('admin.performance.vitals.sub_redis', {
        ops: String(Math.round(ops)),
        clients: String(Math.round(clients)),
      })
    }
    case 'host.disk': {
      const cpu = finiteDetail(details, 'cpu')
      const mem = finiteDetail(details, 'mem')
      const disk = finiteDetail(details, 'disk')
      if (cpu === undefined || mem === undefined || disk === undefined) {
        return null
      }
      return t('admin.performance.vitals.sub_host', {
        cpu: formatPercent(cpu),
        mem: formatPercent(mem),
        disk: formatPercent(disk),
      })
    }
  }
}

const KNOWN_STATUSES: readonly VitalStatus[] = ['ok', 'warn', 'crit', 'unknown']

function normalizedStatus(status: string): VitalStatus {
  return (KNOWN_STATUSES as readonly string[]).includes(status)
    ? (status as VitalStatus)
    : 'unknown'
}

function sparkTone(status: VitalStatus): SparklineTone {
  if (status === 'crit') return 'danger'
  if (status === 'warn') return 'warning'
  return 'default'
}

/** series 是 `[unix 秒, 值]` 对；只取值，空点保留 null 让折线断线。 */
function sparkValues(vital: Vital): readonly (number | null)[] {
  return vital.series.map((point) =>
    typeof point[1] === 'number' ? point[1] : null,
  )
}

/** 本地 `HH:mm`：固定两位，避免不同语言的 12/24 小时制改变读数。 */
function probeClock(at: string | null): string | null {
  if (at === null) return null
  const date = new Date(at)
  if (Number.isNaN(date.getTime())) return null
  return `${String(date.getHours()).padStart(2, '0')}:${String(
    date.getMinutes(),
  ).padStart(2, '0')}`
}

function modelStatus(summary: ModelSummary | undefined): VitalStatus {
  if (summary === undefined) return 'unknown'
  if (summary.incident > 0) return 'crit'
  if (summary.degraded > 0) return 'warn'
  if (summary.models > 0) return 'ok'
  return 'unknown'
}

interface VitalCellProps {
  vitalKey: string
  name: string
  figure: string
  unit: string
  status: VitalStatus
  subtitle: string | null
  spark?: { values: readonly (number | null)[]; tone: SparklineTone }
}

function VitalCell({
  vitalKey,
  name,
  figure,
  unit,
  status,
  subtitle,
  spark,
}: VitalCellProps) {
  const { t } = useI18n()

  return (
    <article
      data-testid="vital"
      data-vital={vitalKey}
      data-state={status}
      className="min-w-0 bg-surface-card px-4 py-3.5 sm:last:col-span-2 xl:last:col-span-1"
    >
      <div className="flex items-center justify-between gap-2">
        <h3 className="text-badge font-semibold tracking-[0.01em] text-ink-2">
          {name}
        </h3>
        <StatusBadge variant={vitalTone(status)}>
          {t(`admin.performance.vitals.status_${status}`)}
        </StatusBadge>
      </div>
      <p className="mt-1.5 flex items-baseline gap-1.5 font-mono tabular-nums">
        <b
          data-testid="vital-figure"
          className={[
            'text-stat',
            status === 'unknown' ? 'text-ink-3' : 'text-ink-1',
          ].join(' ')}
        >
          {figure}
        </b>
        {unit === '' ? null : (
          <span className="text-badge text-ink-3">{unit}</span>
        )}
      </p>
      {subtitle === null ? null : (
        <p className="mt-0.5 truncate text-badge text-ink-2">{subtitle}</p>
      )}
      {spark === undefined ? null : (
        <Sparkline
          values={spark.values}
          tone={spark.tone}
          testId="vital-spark"
          className="mt-2 w-full"
        />
      )}
    </article>
  )
}

export interface ModelProbe {
  name: string
  at: string | null
}

export interface VitalsStripProps {
  vitals: PerfOverview['vitals']
  modelSummary: ModelSummary | undefined
  sourceDown: boolean
  /**
   * 第五格副标题的模型与最近探针时间。页面层从模型表里挑一行传入；缺席时
   * 不渲染副标题（不拿汇总数字编一个模型名）。
   */
  modelProbe?: ModelProbe
}

export function VitalsStrip({
  vitals,
  modelSummary,
  sourceDown,
  modelProbe,
}: VitalsStripProps) {
  const { t } = useI18n()

  const cells = vitals
    .filter((vital) => isPrometheusVitalKey(vital.key))
    .map((vital) => {
      const key = vital.key as PrometheusVitalKey
      const details = detailMap(vital)
      // sourceDown 只影响四个 Prometheus 派生格；第五格由数据库里的模型汇总
      // 渲染，监控断线时它仍然有值（adjudications §2）。
      const status = sourceDown ? 'unknown' : normalizedStatus(vital.status)
      const figure = sourceDown ? '—' : figureFor(key, vital.value)
      const subtitle = sourceDown ? null : subtitleFor(key, details, t)

      return {
        key,
        name: t(NAME_KEYS[key]),
        figure,
        unit: unitFor(key, details, t),
        status,
        subtitle,
        spark: sourceDown
          ? undefined
          : { values: sparkValues(vital), tone: sparkTone(status) },
      }
    })

  const summary = modelSummary
  const fifthStatus = modelStatus(summary)
  const probeName = modelProbe?.name
  const probeTime = modelProbe === undefined ? null : probeClock(modelProbe.at)
  let modelSubtitle: string | null = null
  if (probeName !== undefined && probeName !== '') {
    modelSubtitle =
      probeTime === null
        ? probeName
        : t('admin.performance.vitals.sub_model', {
            model: probeName,
            time: probeTime,
          })
  }

  return (
    <section
      aria-label={t('admin.performance.vitals.title')}
      className="grid grid-cols-1 gap-px overflow-hidden rounded-xl border border-border-hairline bg-border-hairline sm:grid-cols-2 xl:grid-cols-5"
    >
      {cells.map((cell) => (
        <VitalCell
          key={cell.key}
          vitalKey={cell.key}
          name={cell.name}
          figure={cell.figure}
          unit={cell.unit}
          status={cell.status}
          subtitle={cell.subtitle}
          spark={cell.spark}
        />
      ))}
      <VitalCell
        vitalKey="models"
        name={t('admin.performance.vitals.model')}
        figure={summary === undefined ? '—' : String(summary.incident)}
        unit={
          summary === undefined
            ? ''
            : t('admin.performance.vitals.unit_incident', {
                total: summary.degraded,
              })
        }
        status={fifthStatus}
        subtitle={modelSubtitle}
      />
    </section>
  )
}
