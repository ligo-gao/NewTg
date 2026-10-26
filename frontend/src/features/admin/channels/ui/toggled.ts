/** 勾选 / 取消勾选一个值（保持其余元素的顺序）。 */
export function toggled<T>(list: readonly T[], value: T): T[] {
  return list.includes(value)
    ? list.filter((item) => item !== value)
    : [...list, value]
}
