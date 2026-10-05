#!/bin/bash
# smoke_serve.sh — exercise the `serve` entry point the way an operator does.
#
# Why this exists: the unit/race matrix verifies packages, and the integration
# job verifies cluster mode against a real registry. Neither one assembles the
# *single-node daemon*, so `serve --engine-enabled` without `--cluster` shipped
# a nil-pointer panic that fired ~10s after startup (the leader-only dispatch
# sweep dereferencing a nil ClusterManager on its first tick). The helper now
# has a unit test; this script guards the whole class of bug that a per-package
# test round cannot see — "start the real binary, keep it alive past every
# background interval, and require it to answer".
#
# Usage:
#   scripts/smoke_serve.sh <path-to-levee-binary>
#
# Environment overrides (for CI runners or local runs with busy ports):
#   SMOKE_GRPC_PORT / SMOKE_HTTP_PORT   listener ports        (default 9091/9092)
#   SMOKE_OBSERVE_SECONDS               live time to watch    (default 25)
#
# Exit codes: 0 = pass, 1 = fail (the daemon log is dumped on failure).
set -uo pipefail

BIN="${1:?usage: smoke_serve.sh <levee-binary>}"
GRPC_PORT="${SMOKE_GRPC_PORT:-9091}"
HTTP_PORT="${SMOKE_HTTP_PORT:-9092}"

# Must clear dispatch.DefaultInterval (10s, internal/dispatch/dispatch.go) by a
# full margin: the guarded bug crashed on the FIRST tick, so a 5s probe would
# pass with the bug present.
OBSERVE_SECONDS="${SMOKE_OBSERVE_SECONDS:-25}"

# 0. The ports must be unclaimed before anything is launched.
#
# Why this exists: a container stack that maps the same loopback ports is the
# normal way this script goes red on a developer machine, and it failed in the
# most misleading shape. Measured root cause: `--http-addr ":9092"` binds the
# wildcard address, which does not collide with an existing 127.0.0.1-only
# listener, so the daemon stays up and logs "REST gateway listening addr=:9092"
# while curl to 127.0.0.1 is answered by the *more specific* socket (observed:
# prometheus replying HTTP 404 for /healthz). The report then read "healthz
# never became reachable" — a bug that is not in this repo. Naming the port
# costs nothing and ends the guessing.
assert_port_free() {
  label="$1"; port="$2"
  # The socket is opened inside a subshell so it is reaped with the subshell.
  # Do NOT close the fd here: closing a descriptor this shell never opened is a
  # failed redirection, and a failed redirection aborts a non-interactive shell
  # — silently, with status 0. Measured on bash 5.2 (msys): the script stopped
  # mid-body and exited 0 while still reporting nothing.
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    # Optional colour for the message: who is answering? curl is already a hard
    # dependency of the /healthz probe below, and a missing/unreachable answer
    # just leaves the hint out ("000" is what curl reports for a non-HTTP
    # listener such as the gRPC port).
    hint=""
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://127.0.0.1:$port/" 2>/dev/null || true)"
    case "$code" in ''|000) ;; *) hint=", it answered HTTP $code" ;; esac
    printf '::error::smoke cannot start: something already listens on :%s (%s%s) — free it or set SMOKE_%s_PORT\n' \
      "$port" "$label" "$hint" "$label" >&2
    exit 1
  fi
}
assert_port_free HTTP "$HTTP_PORT"
assert_port_free GRPC "$GRPC_PORT"

WORKDIR="$(mktemp -d)"
cleanup() {
  # Reap the daemon before deleting its data_dir. On Windows an open file is a
  # locked file, so the old kill-then-rm trap raced the process exit and left
  # every smoke workdir behind (measured: "rm: cannot remove .../levee.db:
  # Device or resource busy" printed on stderr by a run that reported success).
  if [ -n "${SERVE_PID:-}" ]; then
    kill "${SERVE_PID}" 2>/dev/null || true
    wait "${SERVE_PID}" 2>/dev/null || true
  fi
  rm -rf "$WORKDIR" || echo "smoke: warning: could not remove $WORKDIR" >&2
}
trap cleanup EXIT

# An isolated data_dir keeps the smoke run off the operator's real ~/.levee
# store, and doubles as a check that config-file loading works from a binary.
# The levee binary is a native executable: on a Windows/MSYS shell a POSIX
# /tmp path means nothing to it, so hand the config the platform spelling
# (a plain YAML scalar keeps the backslashes literal).
mkdir -p "$WORKDIR/data"
DATA_DIR="$WORKDIR/data"
if command -v cygpath >/dev/null 2>&1; then
  DATA_DIR="$(cygpath -w "$WORKDIR/data")"
fi
cat > "$WORKDIR/config.yaml" <<EOF
server:
  data_dir: $DATA_DIR
EOF

LOG="$WORKDIR/serve.log"
LEVEE_MASTER_PASSWORD=smoke-not-a-real-secret \
  "$BIN" serve --insecure --engine-enabled \
    --addr ":$GRPC_PORT" --http-addr ":$HTTP_PORT" \
    -c "$WORKDIR/config.yaml" > "$LOG" 2>&1 &
SERVE_PID=$!

fail() {
  echo "::error::smoke failed: $1"
  echo "----- serve.log -----"
  cat "$LOG"
  echo "---------------------"
  exit 1
}

# 1. The daemon must come up at all (a bind/store failure kills it here).
READY=""
for _ in $(seq 1 60); do
  if ! kill -0 "$SERVE_PID" 2>/dev/null; then
    fail "serve exited during startup"
  fi
  if curl -fsS "http://127.0.0.1:$HTTP_PORT/healthz" >/dev/null 2>&1; then
    READY=yes
    break
  fi
  sleep 1
done
[ -n "$READY" ] || fail "/healthz never became reachable on :$HTTP_PORT"
echo "smoke: /healthz answered on :$HTTP_PORT"

# 2. Then it must still be alive after every background interval has fired at
#    least once -- this is the assertion the panic actually violated.
sleep "$OBSERVE_SECONDS"
kill -0 "$SERVE_PID" 2>/dev/null || fail "serve died within ${OBSERVE_SECONDS}s (post-readiness)"

# 3. And a crash-prone goroutine must not have printed a trace while running.
if grep -q "panic" "$LOG"; then
  fail "panic found in the daemon log"
fi

curl -fsS "http://127.0.0.1:$HTTP_PORT/healthz" >/dev/null 2>&1 \
  || fail "/healthz stopped answering after ${OBSERVE_SECONDS}s"

echo "smoke: serve stayed healthy for ${OBSERVE_SECONDS}s across the dispatch interval"
exit 0
