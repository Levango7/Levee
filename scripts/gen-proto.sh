#!/bin/bash
# gen-proto.sh — regenerate LEVEE's Go protobuf bindings from proto/.
#
# Pinned toolchain (output must stay byte-identical to the checked-in files;
# the CI "proto regenerate check" job enforces this with git diff):
#
#   protoc              v27.0   (libprotoc 27.0)
#   protoc-gen-go       v1.36.12
#   protoc-gen-go-grpc  v1.6.2
#
# Install the plugins with:
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
#   go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
#
# Outputs:
#   proto/levee.proto       -> internal/grpc/pb/levee.pb.go
#                              internal/grpc/pb/levee_grpc.pb.go
#      Canonical generated code, always compiled.
#
#   proto/levee_extra.proto -> internal/grpc/pb/levee_extra.regen.pb.go
#                              internal/grpc/pb/levee_extra_grpc.regen.pb.go
#      Generated stand-ins for the hand-written extra bindings, compiled
#      only with `-tags proto_regenerate`. The hand-written files
#      (levee_extra*.pb.go, inventory_extra*.pb.go) carry the inverse
#      `//go:build !proto_regenerate` tag and remain the default build.
#
# Usage:
#   ./scripts/gen-proto.sh          # regenerate in place
#   PROTOC=/path/to/protoc ./scripts/gen-proto.sh
#
# Exit codes:
#   0 — regeneration succeeded
#   1 — protoc or a plugin is missing, or generation failed

set -euo pipefail

cd "$(dirname "$0")/.."

PROTOC="${PROTOC:-protoc}"

if ! command -v "$PROTOC" >/dev/null 2>&1; then
    echo "error: protoc not found (set PROTOC=/path/to/protoc)" >&2
    exit 1
fi
for plugin in protoc-gen-go protoc-gen-go-grpc; do
    if ! command -v "$plugin" >/dev/null 2>&1; then
        echo "error: $plugin not found in PATH" >&2
        exit 1
    fi
done

"$PROTOC" --version

# Relative temp dir (not mktemp) so the path is also valid for native
# Windows protoc.exe when the script runs under git bash.
OUT=".protoc-out.$$.tmp"
rm -rf "$OUT"
mkdir -p "$OUT"
trap 'rm -rf "$OUT"' EXIT

"$PROTOC" \
    --proto_path=proto \
    --go_out=paths=source_relative:"$OUT" \
    --go-grpc_out=paths=source_relative:"$OUT" \
    proto/levee.proto proto/levee_extra.proto

# Main bindings: canonical generated output, always compiled.
cp "$OUT/levee.pb.go"      internal/grpc/pb/levee.pb.go
cp "$OUT/levee_grpc.pb.go" internal/grpc/pb/levee_grpc.pb.go

# Extra bindings: regenerated track, gated behind the proto_regenerate tag.
{
    echo '//go:build proto_regenerate'
    echo
    cat "$OUT/levee_extra.pb.go"
} > internal/grpc/pb/levee_extra.regen.pb.go
{
    echo '//go:build proto_regenerate'
    echo
    cat "$OUT/levee_extra_grpc.pb.go"
} > internal/grpc/pb/levee_extra_grpc.regen.pb.go

echo "regenerated internal/grpc/pb/:"
ls -1 internal/grpc/pb/
