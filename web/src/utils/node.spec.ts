import { describe, expect, it } from 'vitest'

import {
  NODE_ROLES,
  NODE_STATUSES,
  nodeRoleLabel,
  nodeStatusLabel,
  nodeStatusTone,
} from './node'

// The vocabularies under test are internal/cluster's typed groups
// (internal/cluster/node.go) — NodeStatus and NodeRole — not the batch or assignment
// ones the same page also renders. internal/cluster/node_vocabulary_test.go pins both
// lists against those constants and additionally checks that every tone used here has
// a rule in web/src/styles/base.css; this file covers the label/tone behaviour.

describe('NODE_ROLES', () => {
  it('is exactly the two roles internal/cluster declares', () => {
    expect([...NODE_ROLES].sort()).toEqual(['master', 'worker'])
  })

  it('names every declared role in Chinese', () => {
    for (const role of NODE_ROLES) {
      const label = nodeRoleLabel(role)
      expect(label).not.toBe('')
      expect(label).not.toBe(role)
    }
  })

  it('shows an unrecognised wire value verbatim instead of guessing', () => {
    expect(nodeRoleLabel('standby')).toBe('standby')
  })
})

describe('NODE_STATUSES', () => {
  it('is exactly the three states internal/cluster declares', () => {
    expect([...NODE_STATUSES].sort()).toEqual(['active', 'leaving', 'offline'])
  })

  it('names every declared state in Chinese', () => {
    for (const status of NODE_STATUSES) {
      const label = nodeStatusLabel(status)
      expect(label).not.toBe('')
      expect(label).not.toBe(status)
    }
  })

  it('keeps the three states apart in the tooltip', () => {
    const labels = NODE_STATUSES.map(nodeStatusLabel)
    expect(new Set(labels).size).toBe(NODE_STATUSES.length)
  })

  it('does not call a graceful leave a failure', () => {
    // The expression this replaces was `status === 'active' ? 'ok' : 'bad'`, which
    // painted `leaving` — a node that asked to go away cleanly — with the outage
    // colour. A map that collapses warn back onto bad fails here.
    expect(nodeStatusTone('leaving')).toBe('warn')
    expect(nodeStatusTone('leaving')).not.toBe(nodeStatusTone('offline'))
  })

  it('colors the healthy and offline states apart', () => {
    expect(nodeStatusTone('active')).toBe('ok')
    expect(nodeStatusTone('offline')).toBe('bad')
  })

  it('treats an unknown state as not-healthy rather than guessing', () => {
    // Falling back to `ok` would let a new backend state read as healthy until
    // someone noticed; the point of the guard test is that nobody has to notice.
    expect(nodeStatusTone('draining')).toBe('bad')
  })

  it('shows an unrecognised wire value verbatim instead of guessing', () => {
    expect(nodeStatusLabel('draining')).toBe('draining')
  })
})
