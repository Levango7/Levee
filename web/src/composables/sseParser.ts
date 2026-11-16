// sseParser — the frame parser for a text/event-stream.
//
// Why it is its own module: the console's two live feeds used to be read with
// the browser's EventSource, which CANNOT send an Authorization header. Every
// deployment that configured a bearer token therefore answered the stream 401
// (internal/grpc/rest.go's authMiddleware demands the header), and the console
// retried forever in silence — EventSource reports an auth failure as a
// network event, never a page error. The replacement reads the same wire
// format with fetch(), and the part that can actually break — splitting a byte
// stream into frames — is a pure function here, so it is unit-tested without
// a browser.
//
// The format is the WHATWG event-stream format the server writes
// (internal/grpc/sse.go): a frame ends at a blank line, its fields are
// "name: value" lines, a line starting with ":" is a comment (the server's
// keep-alive is ": ping"), and several "data:" lines are joined with "\n".
//
// A frame with no "event:" field is delivered as type "message" — the default
// the browser's EventSource used, and the type the ALERT stream's frames
// carry (sse_alerts.go writes a bare "data: ...").
//
// A frame WITH an "event:" field is delivered under THAT name, and this is
// the second defect the EventSource client had: the server publishes change
// events as "event: status_changed", while the console listened only to
// "onmessage" — which fires for the default type alone. The monitor therefore
// never refreshed on a real event, and CI stayed green because the mock
// gateway sent UNNAMED frames (web/scripts/mock-gateway.mjs), i.e. the mock
// asserted the client's assumption instead of the server's behaviour.

export interface SSEEvent {
  /** The event type: the frame's "event:" field, or "message" by default. */
  event: string
  /** The frame's data lines, joined with "\n" (the server writes one). */
  data: string
  /** The last "id:" field seen on the stream, or '' when there was none. */
  id: string
}

/**
 * SSEParser turns a stream of text chunks into frames.
 *
 * Chunks arrive at arbitrary boundaries: a frame can be split down the
 * middle of a field name, between the CR and the LF of a CRLF pair, or
 * between the two newlines that end a frame. The parser buffers whatever
 * does not yet form a complete line and only emits a frame once its
 * terminating blank line has arrived.
 */
export class SSEParser {
  private buffer = ''
  private dataLines: string[] = []
  private eventName = ''
  private lastId = ''

  /** Feed a decoded chunk; returns the frames that completed within it. */
  push(chunk: string): SSEEvent[] {
    this.buffer += chunk
    const events: SSEEvent[] = []
    for (;;) {
      const line = this.takeLine()
      if (line === null) break
      const ev = this.handleLine(line)
      if (ev) events.push(ev)
    }
    return events
  }

  /**
   * Flush the tail at the end of the stream: a frame whose blank line never
   * arrived (the server closed mid-frame) is still delivered, because the
   * data that did arrive is real.
   */
  flush(): SSEEvent[] {
    const events: SSEEvent[] = []
    const rest = this.buffer
    this.buffer = ''
    if (rest.length > 0) {
      // A trailing CR terminates the last line.
      const line = rest.endsWith('\r') ? rest.slice(0, -1) : rest
      const ev = this.handleLine(line)
      if (ev) events.push(ev)
    }
    const ev = this.dispatch()
    if (ev) events.push(ev)
    return events
  }

  /**
   * takeLine removes and returns the next complete line (without its
   * terminator), or null when the buffer holds no complete line yet.
   *
   * A CR at the very end of the buffer is NOT consumed: it may be the first
   * half of a CRLF whose LF arrives in the next chunk, and treating it as a
   * terminator would emit a phantom empty line — and an empty line ends a
   * frame.
   */
  private takeLine(): string | null {
    let end = -1
    for (let i = 0; i < this.buffer.length; i++) {
      const c = this.buffer[i]
      if (c === '\n' || c === '\r') {
        end = i
        break
      }
    }
    if (end < 0) return null
    let next = end + 1
    if (this.buffer[end] === '\r') {
      if (next >= this.buffer.length) return null // wait: maybe CRLF
      if (this.buffer[next] === '\n') next++
    }
    const line = this.buffer.slice(0, end)
    this.buffer = this.buffer.slice(next)
    return line
  }

  /** handleLine folds one line into the pending frame; a blank line ends it. */
  private handleLine(line: string): SSEEvent | null {
    if (line === '') return this.dispatch()
    // A comment line (":" alone or ": ping") is the server's keep-alive.
    if (line.startsWith(':')) return null
    const colon = line.indexOf(':')
    const field = colon < 0 ? line : line.slice(0, colon)
    let value = colon < 0 ? '' : line.slice(colon + 1)
    // Exactly one leading space after the colon is part of the separator.
    if (value.startsWith(' ')) value = value.slice(1)
    switch (field) {
      case 'event':
        this.eventName = value
        break
      case 'data':
        this.dataLines.push(value)
        break
      case 'id':
        // The spec ignores an id containing NUL.
        if (!value.includes('\0')) this.lastId = value
        break
      case 'retry':
        // The reconnection delay is the client's own policy (backoff), so
        // the server's hint is read but not obeyed.
        break
      default:
        // Unknown fields are ignored, per the spec.
        break
    }
    return null
  }

  /** dispatch emits the pending frame, if it carries any data. */
  private dispatch(): SSEEvent | null {
    if (this.dataLines.length === 0) {
      // No data: the event type is reset, and nothing is delivered.
      this.eventName = ''
      return null
    }
    const ev: SSEEvent = {
      event: this.eventName === '' ? 'message' : this.eventName,
      data: this.dataLines.join('\n'),
      id: this.lastId,
    }
    this.dataLines = []
    this.eventName = ''
    return ev
  }
}
