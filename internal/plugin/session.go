// session.go is the long-lived sibling of pluginProc: one resident plugin
// sub-process serving many requests over the same pipes. It exists for the
// channel consumption point, where "connect once, exec many" is the whole
// point of the interface — a per-call process would drop the session state a
// NETCONF peer or a bastion jump host just spent a dial establishing.
//
// Three properties are load-bearing and tested:
//
//   - Requests are serialised. One pipe carries one request at a time; the
//     channel contract explicitly allows a Channel to be Exec'd from several
//     goroutines, so the session takes a lock rather than letting interleaved
//     writes corrupt the stream.
//   - A failed or abandoned request kills the session. Once a request has no
//     answer (timeout, cancellation, protocol breach) the next read would
//     return the stale line, so the stream is not "recoverable": the session
//     goes dead, the process is killed, and the caller re-dials.
//   - Teardown never waits on an in-flight request. close/kill mark the
//     session dead under their own lock and then reap the process, which
//     unblocks whatever request was mid-flight. A Close that waited for a
//     request that may never answer would turn a clean finish into a hang.
package plugin

import (
	"context"
	"fmt"
	"sync"
)

// pluginSession is one resident plugin process with serialised request/
// response access. The zero value is not usable; construct with
// startPluginSession.
type pluginSession struct {
	name string
	proc *pluginProc

	// cancel releases the session context that owns the process. It is called
	// by kill and close — never by startPluginSession, whose success path
	// deliberately hands the live session to the caller.
	cancel context.CancelFunc

	// mu serialises requests: the JSON-line protocol has no request ids, so
	// one in-flight request at a time is the only honest reading. It is
	// acquired by request only — teardown paths must not block on it.
	mu sync.Mutex

	// stateMu guards dead, which is written by request failures AND by
	// teardown. Two locks because the two facts have different waiting rules:
	// requests wait for their turn, teardown never waits for a request.
	stateMu sync.Mutex
	dead    bool
}

// startPluginSession spawns the plugin and performs the init exchange. The
// ctx passed here bounds the handshake only; the session's lifetime is bounded
// by close (or by kill on failure).
func startPluginSession(ctx context.Context, name, binaryPath, configYAML string) (*pluginSession, error) {
	bin, err := resolvePluginBinary(binaryPath)
	if err != nil {
		return nil, wrapError(name, err)
	}

	// The process lifetime is its own context: cancelling it kills the child
	// (the pluginProc killer goroutine watches it), which is what teardown
	// and every failure path rely on.
	sessionCtx, cancel := context.WithCancel(context.Background())

	proc, err := startPluginProc(sessionCtx, bin)
	if err != nil {
		cancel()
		return nil, wrapError(name, err)
	}
	s := &pluginSession{name: name, proc: proc, cancel: cancel}

	cfg := map[string]any{}
	if configYAML != "" {
		parsed, err := parseConfigYAML(configYAML)
		if err != nil {
			s.kill()
			return nil, wrapError(name, fmt.Errorf("stored config: %w", err))
		}
		cfg = parsed
	}

	initCtx, initCancel := context.WithTimeout(ctx, probeDeadline)
	defer initCancel()
	resp, err := s.Request(initCtx, HostRequest{Action: "init", Config: cfg})
	if err != nil {
		s.kill()
		return nil, wrapError(name, err)
	}
	if !resp.OK {
		s.kill()
		return nil, wrapError(name, fmt.Errorf("init refused: %s", resp.Error))
	}
	return s, nil
}

// Request sends one request and returns its response, serialised against other
// callers. When ctx ends before the answer arrives the session is killed (the
// stream cannot be resynchronised) and ctx.Err() is returned verbatim.
func (s *pluginSession) Request(ctx context.Context, req HostRequest) (PluginResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.isDead() {
		return PluginResponse{}, fmt.Errorf("plugin %q: session is closed", s.name)
	}

	type outcome struct {
		resp PluginResponse
		err  error
	}
	// Buffered so the goroutine can always finish even when the caller has
	// already returned on ctx — no leak, no blocked send.
	done := make(chan outcome, 1)
	go func() {
		resp, err := s.proc.request(req)
		done <- outcome{resp: resp, err: err}
	}()

	select {
	case o := <-done:
		if o.err != nil {
			// A protocol breach or an exited process leaves the stream in an
			// unknown position: dead, not merely failed.
			s.markDead()
			s.proc.killProcess()
			return PluginResponse{}, fmt.Errorf("plugin %q: %w", s.name, o.err)
		}
		return o.resp, nil
	case <-ctx.Done():
		s.markDead()
		s.proc.killProcess()
		return PluginResponse{}, ctx.Err()
	}
}

func (s *pluginSession) isDead() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.dead
}

func (s *pluginSession) markDead() {
	s.stateMu.Lock()
	s.dead = true
	s.stateMu.Unlock()
}

// Alive reports whether the session can still serve requests. It is the
// channel adapter's IsConnected.
func (s *pluginSession) Alive() bool { return !s.isDead() }

// kill tears the session down without a protocol goodbye: mark dead, kill the
// process, reap it, release the session context. Used for every failure path.
func (s *pluginSession) kill() {
	s.markDead()
	s.proc.killProcess()
	s.proc.reap()
	s.cancel()
}

// teardown ends a session for good: the target-level goodbye (best effort),
// then the process reap. Close used to do this inline; with the cross-run pool
// (pool.go) the owner of the lifetime is the pool, so the goodbye lives here
// and Close only returns its lease.
func (s *pluginSession) teardown() {
	if s.Alive() {
		ctx, cancel := context.WithTimeout(context.Background(), sessionTeardownDeadline)
		_, _ = s.Request(ctx, HostRequest{Action: "disconnect"})
		cancel()
	}
	s.close()
}

// close performs the orderly goodbye when the session is still healthy: one
// best-effort "close" under the teardown deadline, then reap. It never reports
// an error — the caller is done with the session either way, and a teardown
// failure that failed the run would be a lie about the run's outcome.
//
// The "close" line is only sent when no request is in flight (TryLock): a
// teardown that waited for a mid-flight request could hang on a plugin that
// never answers. Skipping the goodbye for such a session is the correct trade
// — the process is reaped regardless.
func (s *pluginSession) close() {
	wasDead := s.isDead()
	s.markDead()

	if !wasDead && s.mu.TryLock() {
		ctx, cancel := context.WithTimeout(context.Background(), sessionTeardownDeadline)
		_, _ = s.requestBestEffort(ctx)
		cancel()
		s.mu.Unlock()
	}
	s.proc.reap()
	s.cancel()
}

// requestBestEffort sends one request without taking the request lock; the
// caller has already arranged exclusive access (close holds it via TryLock).
func (s *pluginSession) requestBestEffort(ctx context.Context) (PluginResponse, error) {
	type outcome struct {
		resp PluginResponse
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		resp, err := s.proc.request(HostRequest{Action: "close"})
		done <- outcome{resp: resp, err: err}
	}()
	select {
	case o := <-done:
		return o.resp, o.err
	case <-ctx.Done():
		return PluginResponse{}, ctx.Err()
	}
}
