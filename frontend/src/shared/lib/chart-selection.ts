/**
 * 折线图共用的纯选择逻辑。
 *
 * `shared/ui/analytics-chart.tsx` 与性能页手写的折线图在几何、颜色与表格
 * 契约上完全不同（所以不复用渲染层），但「指针落在哪个点」「方向键移到
 * 哪个点」这两段索引计算必须一致：各写一份迟早漂移。这里只抽这两段纯
 * 函数，不共享任何 className 或 JSX。
 */

/**
 * 返回与 `px` 水平距离最近的候选位置下标；没有候选时返回 0。
 *
 * `xs` 与 `px` 必须在同一坐标空间（viewBox 坐标或 CSS 像素均可），
 * `width` 是该坐标空间的宽度，只用于拒绝无效测量（0 / NaN）。
 * 距离相等时保留靠前的候选，与既有 AnalyticsChart 的行为一致。
 */
export function selectNearest(
  xs: readonly number[],
  px: number,
  width: number,
): number {
  if (xs.length === 0) return 0
  if (!Number.isFinite(width) || width <= 0 || !Number.isFinite(px)) return 0

  let nearest = 0
  let best = Number.POSITIVE_INFINITY
  xs.forEach((x, index) => {
    const distance = Math.abs(x - px)
    if (distance < best) {
      best = distance
      nearest = index
    }
  })
  return nearest
}

/**
 * ArrowLeft / ArrowRight / Home / End 的索引迁移。
 *
 * 返回 `null` 表示「这个键不处理」——Escape 的清空语义由调用方负责，避免
 * 把「未处理」和「清空选中」压进同一个返回值。首次按方向键时从端点进入
 * （右→第一个，左→最后一个），与 AnalyticsChart 既有的无障碍口径一致。
 */
export function nextIndex(
  key: string,
  current: number | null,
  length: number,
): number | null {
  if (length <= 0) return null
  const last = length - 1
  switch (key) {
    case 'ArrowLeft':
      return current === null ? last : Math.max(0, current - 1)
    case 'ArrowRight':
      return current === null ? 0 : Math.min(last, current + 1)
    case 'Home':
      return 0
    case 'End':
      return last
    default:
      return null
  }
}
