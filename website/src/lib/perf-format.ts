const PLACEHOLDER = "—";

/** 时延列以毫秒整数展示，单位放在表头（列宽宝贵，不重复单位）。 */
export function formatLatencyMs(ms: number | null | undefined, locale: string): string {
  if (ms === null || ms === undefined || !Number.isFinite(ms) || ms < 0) {
    return PLACEHOLDER;
  }
  return new Intl.NumberFormat(locale, { maximumFractionDigits: 0 }).format(ms);
}

/** 输出速度保留一位小数，单位在表头。 */
export function formatTps(tps: number | null | undefined, locale: string): string {
  if (tps === null || tps === undefined || !Number.isFinite(tps)) {
    return PLACEHOLDER;
  }
  return new Intl.NumberFormat(locale, {
    minimumFractionDigits: 1,
    maximumFractionDigits: 1,
  }).format(tps);
}

/** 成功率按百分比展示，保留一位小数；缺失显示 —。 */
export function formatSuccessRate(rate: number | null | undefined, locale: string): string {
  if (rate === null || rate === undefined || !Number.isFinite(rate)) {
    return PLACEHOLDER;
  }
  return new Intl.NumberFormat(locale, {
    style: "percent",
    minimumFractionDigits: 1,
    maximumFractionDigits: 1,
  }).format(rate);
}

/** 探测样本数按当前语言分组，例如 1,543。 */
export function formatSampleSize(sample: number | null | undefined, locale: string): string {
  if (sample === null || sample === undefined || !Number.isFinite(sample)) {
    return PLACEHOLDER;
  }
  return new Intl.NumberFormat(locale).format(sample);
}

/**
 * 最近探测时间显示为相对时间（刚刚 / 2 小时前 / in 1 hour）。
 * `now` 可注入，测试才能钉住边界。
 */
export function formatTestedAt(
  iso: string | null | undefined,
  locale: string,
  now: number = Date.now(),
): string {
  if (!iso) {
    return PLACEHOLDER;
  }
  const timestamp = Date.parse(iso);
  if (!Number.isFinite(timestamp)) {
    return PLACEHOLDER;
  }
  const diff = timestamp - now;
  const absolute = Math.abs(diff);
  const relative = new Intl.RelativeTimeFormat(locale, { numeric: "auto" });
  if (absolute < 60_000) {
    return relative.format(0, "minute");
  }
  if (absolute < 3_600_000) {
    return relative.format(Math.trunc(diff / 60_000), "minute");
  }
  if (absolute < 86_400_000) {
    return relative.format(Math.trunc(diff / 3_600_000), "hour");
  }
  return relative.format(Math.trunc(diff / 86_400_000), "day");
}

/** ≥99% 视为健康，<95% 需要提示；中间档保持中性色。 */
export type SuccessTone = "good" | "plain" | "warn" | "missing";

export function successTone(rate: number | null | undefined): SuccessTone {
  if (rate === null || rate === undefined || !Number.isFinite(rate)) {
    return "missing";
  }
  if (rate >= 0.99) {
    return "good";
  }
  if (rate < 0.95) {
    return "warn";
  }
  return "plain";
}
