import type { KeyboardEvent as ReactKeyboardEvent } from 'react'

/**
 * Shared WAI-ARIA roving-tabindex keyboard handlers (UI-5 走查).
 *
 * 设计约束（为什么放在 shared/lib 而不是每个组件各写一份）：
 * - radiogroup / tablist 的方向键行为必须完全一致，否则同一产品里两个
 *   类似控件的键盘手感会不同；集中在一个实现里也便于用一份测试锁住。
 * - 组内只有一个 tab stop：`tabIndex` 由调用方按“选中项为 0、其余为 -1”
 *   设置，本模块只负责把焦点和选中一起移动。Arrow 键在首尾循环，Home/End
 *   跳到首/末项。
 * - `preventDefault()` 是为了阻止浏览器把方向键当成滚动/光标移动；
 *   纯修饰键组合（Alt/Ctrl/Meta）不拦截，保留系统快捷键语义。
 * - 移动后调用 `.click()` 而不是只 `focus()`：两处调用方（仪表盘范围、
 *   日志范围、钱包单位）都把“选中”建模成按钮的 onClick，因此 click 才能
 *   真正提交选择；这也是测试中 aria-checked / aria-selected 变化的原因。
 */

const RADIO_SELECTOR = '[role="radio"]'
const TAB_SELECTOR = '[role="tab"]'

/** 组内可聚焦的项，按文档顺序；禁用项不参与移动。 */
function enabledItems(container: HTMLElement, selector: string): HTMLElement[] {
  return Array.from(container.querySelectorAll<HTMLElement>(selector)).filter(
    (item) =>
      item.getAttribute('aria-disabled') !== 'true' && !item.hasAttribute('disabled'),
  )
}

function handleRovingKeyDown(
  event: ReactKeyboardEvent<HTMLElement>,
  selector: string,
): void {
  // 已被其他处理器消费，或带系统修饰键时交还给浏览器。
  if (event.defaultPrevented || event.altKey || event.ctrlKey || event.metaKey) {
    return
  }

  const target =
    event.target instanceof HTMLElement
      ? event.target.closest<HTMLElement>(selector)
      : null
  if (target === null) return

  const items = enabledItems(event.currentTarget, selector)
  // 用 indexOf 而非 document.activeElement：keydown 的 target 就是当前项，
  // 这样即使焦点尚未落到按钮上（测试直接对元素派发事件）也能正确计算。
  const index = items.indexOf(target)
  if (index === -1) return

  let nextIndex: number
  switch (event.key) {
    case 'ArrowRight':
    case 'ArrowDown':
      nextIndex = (index + 1) % items.length
      break
    case 'ArrowLeft':
    case 'ArrowUp':
      nextIndex = (index - 1 + items.length) % items.length
      break
    case 'Home':
      nextIndex = 0
      break
    case 'End':
      nextIndex = items.length - 1
      break
    default:
      return
  }

  const next = items[nextIndex]
  if (next === undefined) return

  event.preventDefault()
  // Home/End 落在已经选中的边界项时只聚焦、不重复 click：重复 click 会
  // 触发一次无意义的“重新选择”（把页码重置到第 1 页、重发请求），而
  // 键盘用户的预期只是把焦点送到组首/组尾。
  if (next === target) {
    next.focus()
    return
  }
  next.focus()
  next.click()
}

/** 单选组：方向键移动焦点并同时选中（WAI-ARIA radio group 模式）。 */
export function handleRadioGroupKeyDown(
  event: ReactKeyboardEvent<HTMLElement>,
): void {
  handleRovingKeyDown(event, RADIO_SELECTOR)
}

/** 标签栏：方向键 + Home/End；Arrow 键同样提交选择（自动激活模式）。 */
export function handleTabListKeyDown(
  event: ReactKeyboardEvent<HTMLElement>,
): void {
  handleRovingKeyDown(event, TAB_SELECTOR)
}
