/**
 * 剪贴板降级（F-09-18 / F-09-27）。
 *
 * `navigator.clipboard` 在非安全上下文、权限被拒或 jsdom 里都可能是
 * undefined。静默失败会让「已复制」Toast 变成谎言，因此返回布尔值，
 * 由调用方决定显示成功还是「请手动复制」。
 */
export async function copyText(text: string): Promise<boolean> {
  try {
    if (typeof navigator === 'undefined' || navigator.clipboard === undefined) {
      return false
    }
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    return false
  }
}
