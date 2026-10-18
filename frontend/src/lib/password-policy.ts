/**
 * Password policy frozen by M1 Task 2 (modules-01 F-01-01/§3.14):
 * length only, 8..128 **code points**, no composition rules.
 *
 * `.length` counts UTF-16 units, so an 8-emoji password would measure 16 and
 * an 8-code-point CJK password would pass only by accident. `Array.from`
 * iterates code points, matching the backend's `utf8.RuneCountInString`.
 */

export const PASSWORD_MIN_LENGTH = 8
export const PASSWORD_MAX_LENGTH = 128

export type PasswordPolicyProblem = 'too_short' | 'too_long'

export function passwordLength(value: string): number {
  return Array.from(value).length
}

export function passwordPolicyProblem(
  value: string,
): PasswordPolicyProblem | undefined {
  const length = passwordLength(value)
  if (length < PASSWORD_MIN_LENGTH) return 'too_short'
  if (length > PASSWORD_MAX_LENGTH) return 'too_long'
  return undefined
}
