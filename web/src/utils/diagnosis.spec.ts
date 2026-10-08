import { describe, expect, it } from 'vitest'

import { CHECK_VERDICTS, HEALTH_STATES, healthLabel, verdictLabel } from './diagnosis'

// Two vocabularies, deliberately kept apart: overall daemon health comes from
// diagnosis.HealthStatus, a single doctor check's verdict is pass/warn/fail.
// The Go side of these pins is internal/diagnosis/health_status_vocabulary_test.go.

describe('HEALTH_STATES', () => {
  it('is exactly the four states diagnosis.HealthStatus declares', () => {
    expect([...HEALTH_STATES].sort()).toEqual(['degraded', 'healthy', 'unknown', 'unhealthy'].sort())
  })

  it('names every declared state in Chinese', () => {
    for (const state of HEALTH_STATES) {
      const label = healthLabel(state)
      expect(label).not.toBe('')
      expect(label).not.toBe(state)
    }
  })
})

describe('healthLabel', () => {
  it('maps the health values', () => {
    expect(healthLabel('healthy')).toBe('健康')
    expect(healthLabel('degraded')).toBe('降级')
    expect(healthLabel('unhealthy')).toBe('不健康')
    expect(healthLabel('unknown')).toBe('未知')
  })

  it('does not translate a health word into a check verdict', () => {
    // `healthy` and `pass` both mean "fine" in prose but belong to different
    // columns on the same page; collapsing them is how #92's vocabularies got
    // confused.
    expect(healthLabel('healthy')).not.toBe(verdictLabel('pass'))
  })

  it('shows an unrecognised wire value verbatim instead of guessing', () => {
    expect(healthLabel('unwell')).toBe('unwell')
  })
})

describe('verdictLabel', () => {
  it('is exactly the four verdicts the doctor endpoint emits', () => {
    // Four, not three: `skip` is assigned by the database check when no config
    // is loaded (system_service.go:346-349). The Go guard discovered it.
    expect([...CHECK_VERDICTS].sort()).toEqual(['fail', 'pass', 'skip', 'warn'].sort())
  })

  it('maps them without merging 告警 into 未通过 or 已跳过 into 通过', () => {
    expect(verdictLabel('pass')).toBe('通过')
    expect(verdictLabel('warn')).toBe('告警')
    expect(verdictLabel('fail')).toBe('未通过')
    expect(verdictLabel('skip')).toBe('已跳过')
    // A warn row is "look at this", a fail row is "this broke", a skip row is
    // "nothing was checked" — reading any two as the same words would hide the
    // difference the checks were built for.
    expect(verdictLabel('warn')).not.toBe(verdictLabel('fail'))
    expect(verdictLabel('skip')).not.toBe(verdictLabel('pass'))
  })

  it('shows an unrecognised verdict verbatim', () => {
    expect(verdictLabel('skipped')).toBe('skipped')
  })
})
