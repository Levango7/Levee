import { describe, expect, it } from 'vitest'

import {
  NODE_ROLES as NODE_ROLES_FROM_OWNER,
  NODE_STATUSES as NODE_STATUSES_FROM_OWNER,
} from '@/types/vocabulary'

import {
  NODE_ROLES,
  NODE_STATUSES,
  nodeRoleLabel,
  nodeStatusLabel,
  nodeStatusTone,
} from './node'

// The vocabularies under test are internal/cluster's typed groups
// (internal/cluster/node.go) — NodeStatus and NodeRole — not the batch or assignment
// ones the same page also renders. Neither list is written down in this repo's
// TypeScript any more: ./node.ts re-exports what internal/docgen generates from
// cluster.NodeStatusNames() / NodeRoleNames().
//
// So this file does not restate members (that would be a third copy, and the copy is
// the thing we removed). internal/cluster/node_vocabulary_test.go owns the claims it
// used to make — the artefact equals the owner's data, node.ts is wired rather than
// copying, and every tone used here has a real rule in web/src/styles/base.css — and
// the cases below cover the label/tone behaviour plus the identity of the re-export.

describe('NODE_ROLES', () => {
  it('arrives from the generated vocabulary instead of a copy in ./node.ts', () => {
    expect(NODE_ROLES).toBe(NODE_ROLES_FROM_OWNER)
    expect(NODE_ROLES.length).toBeGreaterThan(0)
    expect(new Set(NODE_ROLES).size).toBe(NODE_ROLES.length)
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
  it('arrives from the generated vocabulary instead of a copy in ./node.ts', () => {
    expect(NODE_STATUSES).toBe(NODE_STATUSES_FROM_OWNER)
    expect(NODE_STATUSES.length).toBeGreaterThan(0)
    expect(new Set(NODE_STATUSES).size).toBe(NODE_STATUSES.length)
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
