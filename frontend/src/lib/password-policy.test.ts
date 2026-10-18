import { describe, expect, it } from 'vitest'

import {
  PASSWORD_MAX_LENGTH,
  PASSWORD_MIN_LENGTH,
  passwordLength,
  passwordPolicyProblem,
} from './password-policy'

describe('password policy (F-01-01, Task 2 freeze)', () => {
  it('exposes the frozen 8..128 bounds', () => {
    expect(PASSWORD_MIN_LENGTH).toBe(8)
    expect(PASSWORD_MAX_LENGTH).toBe(128)
  })

  it('counts code points, not UTF-16 units', () => {
    const eightEmoji = '😀'.repeat(8)
    // `.length` would be 16 for this value; the policy must accept it.
    expect(eightEmoji.length).toBe(16)
    expect(passwordLength(eightEmoji)).toBe(8)
    expect(passwordPolicyProblem(eightEmoji)).toBeUndefined()
  })

  it('rejects values shorter than 8 code points and longer than 128', () => {
    expect(passwordPolicyProblem('😀'.repeat(7))).toBe('too_short')
    expect(passwordPolicyProblem('a'.repeat(129))).toBe('too_long')
  })

  it('applies no composition rules', () => {
    expect(passwordPolicyProblem('aaaaaaaa')).toBeUndefined()
    expect(passwordPolicyProblem('        ')).toBeUndefined()
  })
})
