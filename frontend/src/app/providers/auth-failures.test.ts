import { describe, expect, it } from 'vitest'

import { parseProblem } from '@/api/errors'

import {
  MFA_ENROLLMENT_REQUIRED_REDIRECT,
  isForcedPasswordChangePath,
  isMfaEnrollmentLandingPath,
  isMfaEnrollmentRequired,
} from './auth-failures'

describe('isForcedPasswordChangePath', () => {
  it('matches the forced password-change route', () => {
    expect(isForcedPasswordChangePath('/security', '?force=password')).toBe(true)
  })

  it('does not match the ordinary security page', () => {
    expect(isForcedPasswordChangePath('/security', '')).toBe(false)
    expect(isForcedPasswordChangePath('/security', '?force=other')).toBe(false)
  })

  it('does not match other paths carrying the same query', () => {
    expect(isForcedPasswordChangePath('/dashboard', '?force=password')).toBe(
      false,
    )
    expect(isForcedPasswordChangePath('/401', '?force=password')).toBe(false)
  })
})

describe('MFA enrollment required landing (R-04)', () => {
  it('lands on the sign-in page with an explicit reason marker', () => {
    expect(MFA_ENROLLMENT_REQUIRED_REDIRECT).toBe(
      '/sign-in?reason=mfa_enrollment_required',
    )
  })

  it('recognises the landing pages that must never redirect onto themselves', () => {
    expect(isMfaEnrollmentLandingPath('/sign-in')).toBe(true)
    expect(isMfaEnrollmentLandingPath('/two-factor-enroll')).toBe(true)
    expect(isMfaEnrollmentLandingPath('/dashboard')).toBe(false)
    expect(isMfaEnrollmentLandingPath('/sign-in-other')).toBe(false)
  })

  it('matches the backend problem type in URI, bare and namespaced forms', () => {
    const problem = (type: string) =>
      parseProblem({ status: 403, body: { type, title: 'Forbidden' } })

    expect(
      isMfaEnrollmentRequired(
        problem('https://newtg.dev/problems/mfa_enrollment_required'),
      ),
    ).toBe(true)
    expect(isMfaEnrollmentRequired(problem('mfa_enrollment_required'))).toBe(true)
    expect(isMfaEnrollmentRequired(problem('gateway:mfa_enrollment_required'))).toBe(
      true,
    )
    expect(isMfaEnrollmentRequired(problem('https://newtg.dev/problems/forbidden'))).toBe(
      false,
    )
    // The status is part of the contract: a 400 carrying the same token is a
    // different failure and must render locally.
    expect(
      isMfaEnrollmentRequired(
        parseProblem({
          status: 400,
          body: {
            type: 'https://newtg.dev/problems/mfa_enrollment_required',
            title: 'Bad Request',
          },
        }),
      ),
    ).toBe(false)
  })
})
