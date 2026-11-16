// sseParser.spec.ts — the frame parser is the part of the live-feed client
// that can actually break, so it is tested as a pure function.
//
// What is pinned here is the behaviour the server actually produces
// (internal/grpc/sse.go, internal/grpc/sse_alerts.go):
//
//   - a change event is a NAMED frame ("event: status_changed"); a listener
//     that only handles the default "message" type hears nothing, which is
//     exactly what the console did while the mock gateway sent unnamed
//     frames;
//   - the alert stream writes a BARE "data:" frame, which is the default
//     type;
//   - the server's keep-alive is a COMMENT line (": ping"), not a frame;
//   - frames arrive split at arbitrary byte boundaries.
import { describe, expect, it } from 'vitest'

import { SSEParser } from './sseParser'

describe('SSEParser', () => {
  it('delivers a bare data frame as the default "message" type', () => {
    const p = new SSEParser()
    const events = p.push('data: {"id":"a-1"}\n\n')
    expect(events).toHaveLength(1)
    expect(events[0]?.event).toBe('message')
    expect(events[0]?.data).toBe('{"id":"a-1"}')
  })

  it('delivers a named frame under its name', () => {
    const p = new SSEParser()
    const events = p.push('event: status_changed\ndata: {"changeId":"run-1"}\n\n')
    expect(events).toHaveLength(1)
    expect(events[0]?.event).toBe('status_changed')
    expect(events[0]?.data).toBe('{"changeId":"run-1"}')
  })

  it('ignores the keep-alive comment', () => {
    const p = new SSEParser()
    expect(p.push(': ping\n\n')).toHaveLength(0)
    expect(p.push(': ping\n')).toHaveLength(0)
  })

  it('joins several data lines with a newline', () => {
    const p = new SSEParser()
    const events = p.push('data: first\ndata: second\n\n')
    expect(events).toHaveLength(1)
    expect(events[0]?.data).toBe('first\nsecond')
  })

  it('reassembles a frame split across chunks', () => {
    const p = new SSEParser()
    expect(p.push('event: status_ch')).toHaveLength(0)
    expect(p.push('anged\ndata: {"x":')).toHaveLength(0)
    expect(p.push('1}\n')).toHaveLength(0)
    const events = p.push('\n')
    expect(events).toHaveLength(1)
    expect(events[0]?.event).toBe('status_changed')
    expect(events[0]?.data).toBe('{"x":1}')
  })

  it('splits two frames delivered in one chunk', () => {
    const p = new SSEParser()
    const events = p.push('data: one\n\ndata: two\n\n')
    expect(events.map((e) => e.data)).toEqual(['one', 'two'])
  })

  it('handles CRLF line endings without inventing an empty frame', () => {
    const p = new SSEParser()
    const events = p.push('event: status_changed\r\ndata: {"x":1}\r\n\r\n')
    expect(events).toHaveLength(1)
    expect(events[0]?.event).toBe('status_changed')
    expect(events[0]?.data).toBe('{"x":1}')
  })

  it('does not split a CRLF pair that arrives in two chunks', () => {
    const p = new SSEParser()
    expect(p.push('data: one\r')).toHaveLength(0)
    const events = p.push('\n\r\n')
    expect(events).toHaveLength(1)
    expect(events[0]?.data).toBe('one')
  })

  it('keeps the last id for later frames', () => {
    const p = new SSEParser()
    p.push('id: 42\ndata: one\n\n')
    const events = p.push('data: two\n\n')
    expect(events[0]?.id).toBe('42')
  })

  it('strips exactly one space after the colon', () => {
    const p = new SSEParser()
    const events = p.push('data:  two spaces\n\n')
    expect(events[0]?.data).toBe(' two spaces')
  })

  it('delivers the tail of a stream that ended mid-frame', () => {
    const p = new SSEParser()
    expect(p.push('data: tail')).toHaveLength(0)
    const events = p.flush()
    expect(events).toHaveLength(1)
    expect(events[0]?.data).toBe('tail')
  })

  it('does not deliver an event for a frame without data', () => {
    const p = new SSEParser()
    expect(p.push('event: status_changed\n\n')).toHaveLength(0)
    // The event name is reset by an empty frame, so the next data frame is
    // the default type again (the spec's rule).
    const events = p.push('data: after\n\n')
    expect(events[0]?.event).toBe('message')
  })
})
