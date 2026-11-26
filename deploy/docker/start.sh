#!/bin/sh
# Run the API and the Web UI in one image without hiding either lifecycle.
# dumb-init forwards signals to this shell; the shell forwards them to both.
set -eu

web_addr="${LEVEE_WEB_ADDR:-:8081}"
web_api="${LEVEE_WEB_API:-http://127.0.0.1:8080}"
data_dir="${LEVEE_SERVER_DATA_DIR:-/var/lib/levee/data}"
template_dir="$(dirname "$data_dir")/templates"
mkdir -p "$data_dir" "$template_dir"
serve_pid=""
web_pid=""

shutdown() {
    if [ -n "$serve_pid" ]; then kill -TERM "$serve_pid" 2>/dev/null || true; fi
    if [ -n "$web_pid" ]; then kill -TERM "$web_pid" 2>/dev/null || true; fi
}
trap shutdown TERM INT

levee serve "$@" &
serve_pid=$!
levee web --addr "$web_addr" --api "$web_api" &
web_pid=$!

# If either process exits, stop the other and preserve the failed exit code.
while true; do
    if ! kill -0 "$serve_pid" 2>/dev/null; then
        set +e
        wait "$serve_pid"
        status=$?
        set -e
        kill -TERM "$web_pid" 2>/dev/null || true
        wait "$web_pid" 2>/dev/null || true
        exit "$status"
    fi
    if ! kill -0 "$web_pid" 2>/dev/null; then
        set +e
        wait "$web_pid"
        status=$?
        set -e
        kill -TERM "$serve_pid" 2>/dev/null || true
        wait "$serve_pid" 2>/dev/null || true
        exit "$status"
    fi
    sleep 1
done
