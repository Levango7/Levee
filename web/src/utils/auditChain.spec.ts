import { describe, expect, it } from 'vitest'

import { CHAIN_MESSAGE_PREFIXES, chainModeLabel, chainVerdict } from './auditChain'

// The verdict mapping is a contract with the server (docs/levee-api.md
// §13.2c): the server guarantees four message prefixes and the console reads
// them. These tests pin both directions — each prefix maps to the right
// verdict, and a verdict is never reached by accident.
describe('chainVerdict', () => {
  it('reads a verified chain as intact', () => {
    const v = chainVerdict(`${CHAIN_MESSAGE_PREFIXES.valid} 3 records verified`, true)
    expect(v.type).toBe('success')
    expect(v.label).toBe('哈希链完整')
  })

  it('reads an unsealed tail as NOT a broken chain', () => {
    // The chain fails closed (valid=false) because an empty hash must never
    // read as "fine" — but an unsealed tail is a run that has not settled,
    // not tampering. The two have to stay apart.
    const v = chainVerdict(
      `${CHAIN_MESSAGE_PREFIXES.notSealed} 2 of 5 records carry no hash (the run has not settled)`,
      false,
    )
    expect(v.type).toBe('warning')
    expect(v.label).toBe('哈希链未封口')
    expect(v.label).not.toBe('哈希链未通过')
  })

  it('reads a broken chain as broken', () => {
    const v = chainVerdict(
      `${CHAIN_MESSAGE_PREFIXES.broken} 1 of 5 records failed verification (first: hash_mismatch at trc-2)`,
      false,
    )
    expect(v.type).toBe('danger')
    expect(v.label).toBe('哈希链未通过')
  })

  it('reads an empty chain as nothing to check', () => {
    const v = chainVerdict(CHAIN_MESSAGE_PREFIXES.noTraces, true)
    expect(v.type).toBe('info')
    expect(v.label).toBe('暂无可校验的链')
  })

  it('falls back to the boolean when the message carries no known prefix', () => {
    // An unrecognised message is not a reason to guess "fine": the
    // conservative reading is the one that cannot mislead.
    expect(chainVerdict('', true).type).toBe('success')
    expect(chainVerdict('', false).type).toBe('danger')
    expect(chainVerdict('something else entirely', false).type).toBe('danger')
    expect(chainVerdict('something else entirely', true).type).toBe('success')
  })

  it('does not match a prefix that is merely contained in the message', () => {
    // The prefixes are prefixes, not substrings: a message that mentions
    // "hash chain broken:" in the middle is not a broken-chain message.
    const v = chainVerdict(`the server said "hash chain broken:" earlier`, true)
    expect(v.type).toBe('success')
    expect(v.label).toBe('哈希链完整')
  })
})

describe('chainModeLabel', () => {
  it('names the keyed digest as the stronger guarantee', () => {
    const v = chainModeLabel({ mode: 'keyed', algorithm: 'HMAC-SHA256', keyed: true })
    expect(v.type).toBe('success')
    expect(v.label).toContain('HMAC-SHA256')
    expect(v.label).toContain('口令摘要')
  })

  it('names the unkeyed digest as the weaker one', () => {
    // On an unkeyed deployment anyone who can write to the store can recompute
    // the whole chain, so the label must not read like the keyed promise.
    const v = chainModeLabel({ mode: 'unkeyed', algorithm: 'SHA-256', keyed: false })
    expect(v.type).toBe('warning')
    expect(v.label).toContain('SHA-256')
    expect(v.label).toContain('无口令摘要')
  })
})
