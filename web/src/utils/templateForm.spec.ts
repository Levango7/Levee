// Unit tests for the template-instantiation wizard's logic.
//
// The two mistakes this pins are both taken from the flow it replaces: a
// submission with a required parameter blank (the server refuses it, after a
// round trip) and an environment typed by hand (the authorization layer judges
// a change in the environment it declares, so a typo silently changes who may
// act on it — and the value is permanent).
import { describe, expect, it } from 'vitest'

import type { Template } from '@/types/levee'
import {
  blankForm,
  environmentOptions,
  instantiateIssues,
  instantiateWarnings,
  paramsRows,
  type InstantiateForm,
} from './templateForm'

const template = (requiredParams: string[]): Template => ({
  name: 'patch-rolling',
  description: 'OS patch rolling upgrade',
  workflowContent: 'name: patch-rolling\n',
  requiredParams,
  createdAt: 0,
  updatedAt: 0,
})

const form = (patch: Partial<InstantiateForm> = {}): InstantiateForm => ({
  ...blankForm(template(['pkg_name', 'pkg_version'])),
  ...patch,
})

describe('instantiateIssues', () => {
  it('blocks a blank label', () => {
    const issues = instantiateIssues(form({ label: '   ' }), ['pkg_name'])
    expect(issues.map((i) => i.field)).toContain('label')
  })

  it('blocks every required parameter that is empty or whitespace', () => {
    const issues = instantiateIssues(
      form({ params: { pkg_name: 'nginx', pkg_version: '  ' } }),
      ['pkg_name', 'pkg_version'],
    )
    expect(issues).toHaveLength(1)
    expect(issues[0]?.field).toBe('param:pkg_version')
    expect(issues[0]?.message).toContain('pkg_version')
  })

  it('passes a complete form', () => {
    expect(
      instantiateIssues(form({ params: { pkg_name: 'nginx', pkg_version: '1.2.3' } }), [
        'pkg_name',
        'pkg_version',
      ]),
    ).toHaveLength(0)
  })

  it('does not require parameters the template does not declare', () => {
    // Extra keys in params are the operator's own; only declared ones gate.
    expect(instantiateIssues(form({ params: { pkg_name: 'a' } }), ['pkg_name'])).toHaveLength(0)
  })
})

describe('instantiateWarnings', () => {
  it('warns that an empty environment becomes the permanent judgement basis', () => {
    const warnings = instantiateWarnings(form({ environment: '' }))
    expect(warnings.map((w) => w.field)).toContain('environment')
    expect(warnings[0]?.message).toContain('默认环境')
  })

  it('does not warn when the environment is given', () => {
    expect(instantiateWarnings(form({ environment: 'prod' })).map((w) => w.field)).not.toContain(
      'environment',
    )
  })

  it('says the team is echoed but not persisted, instead of implying otherwise', () => {
    const team = instantiateWarnings(form({ team: 'sre' })).find((w) => w.field === 'team')
    expect(team, 'a typed team must be reported as echoed-only').toBeDefined()
    expect(team?.message).toContain('不落库')
    // and the same form (empty environment) still warns about the environment:
    // the two findings are independent.
    expect(instantiateWarnings(form({ team: 'sre' })).map((w) => w.field)).toContain('environment')
  })

  it('never blocks: warnings are a separate list from issues', () => {
    const f = form({ environment: '', team: 'sre', params: { pkg_name: 'a', pkg_version: 'b' } })
    expect(instantiateIssues(f, ['pkg_name', 'pkg_version'])).toHaveLength(0)
    expect(instantiateWarnings(f).length).toBeGreaterThan(0)
  })
})

describe('paramsRows', () => {
  it('lists declared parameters in a stable order with their values', () => {
    const rows = paramsRows(form({ params: { pkg_version: '1.2.3', pkg_name: 'nginx' } }), [
      'pkg_name',
      'pkg_version',
    ])
    expect(rows.map((r) => r.name)).toEqual(['pkg_name', 'pkg_version'])
    expect(rows[0]?.value).toBe('nginx')
    expect(rows.every((r) => !r.missing)).toBe(true)
  })

  it('flags a missing value so the review step shows it too', () => {
    const rows = paramsRows(form({ params: { pkg_name: 'nginx' } }), ['pkg_name', 'pkg_version'])
    expect(rows.find((r) => r.name === 'pkg_version')?.missing).toBe(true)
  })
})

describe('environmentOptions', () => {
  it('dedupes, trims and sorts what the inventory and the forms already use', () => {
    expect(environmentOptions(['prod', ' dev '], ['dev', 'staging', ''])).toEqual([
      'dev',
      'prod',
      'staging',
    ])
  })
})

describe('blankForm', () => {
  it('pre-fills the label and one empty field per required parameter', () => {
    const f = blankForm(template(['a', 'b']))
    expect(f.label).toContain('patch-rolling-')
    expect(f.params).toEqual({ a: '', b: '' })
    expect(f.priority).toBe('normal')
    expect(f.dryRun).toBe(false)
  })
})
