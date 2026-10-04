#!/bin/bash
# validate_delivery.sh — lint and render the shipped deployment artifacts.
#
# Why this exists: the Helm chart and the systemd unit are release artifacts,
# and nothing in CI ever ran helm against them. The chart passed
# `--cluster-dispatch-worker-capacity` while the binary registers
# `--cluster-dispatch-capacity`, so every `helm install` produced a Deployment
# whose pods CrashLoopBackOff on "unknown flag" — a defect that survives review
# because the wrong token is only visible once someone actually renders it.
#
# Two gates cover this surface, from different sides, and both are needed:
#   * cmd/levee/cli_reference_flags_test.go compares every flag token in the
#     chart templates and the systemd unit against the live cobra tree, so a
#     renamed or removed flag is caught without needing a cluster.
#   * this script renders the chart, which no text scan can do: template
#     syntax errors, `.Values` paths that no longer exist, `helm lint --strict`
#     failures, and an unset value reaching the args list as the literal
#     string "<no value>" (helm's own signature for a key it could not find).
#
# Usage:
#   scripts/validate_delivery.sh [path-to-chart]        (default deploy/helm/levee)
#
# Environment overrides:
#   HELM_BIN   helm executable to use                   (default: helm on PATH)
#
# Exit codes: 0 = pass, 1 = fail.

set -uo pipefail

HELM="${HELM_BIN:-helm}"
CHART="${1:-deploy/helm/levee}"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

fail() {
	echo "DELIVERY-VALIDATION FAILED: $*" >&2
	exit 1
}

command -v "$HELM" >/dev/null 2>&1 || fail "helm not found (set HELM_BIN); the chart is a shipped artifact and must be rendered in CI"
[ -d "$CHART" ] || fail "chart directory not found: $CHART"

echo "== helm version"
"$HELM" version --short || fail "helm is not runnable"

echo "== helm lint --strict"
# --strict turns chart warnings into errors: a deprecated apiVersion or an
# unquoted numeric value renders fine today and breaks on the next cluster
# upgrade, which is the kind of thing that should not wait for a customer.
"$HELM" lint --strict "$CHART" || fail "helm lint --strict rejected $CHART"

# Each profile below is a render the operator can actually ask for. A template
# branch that is never rendered is never checked, so the list is deliberately
# keyed on the toggles that gate arg blocks and extra objects.
render_profile() {
	local name="$1"
	shift
	local out="$WORKDIR/$name.yaml"
	echo "== render: $name"
	# --set mode=cluster is the profile that exercises the dispatch args, i.e.
	# the exact block that shipped the wrong flag name.
	if ! "$HELM" template levee "$CHART" "$@" >"$out" 2>"$WORKDIR/$name.err"; then
		cat "$WORKDIR/$name.err" >&2
		fail "helm template failed for profile: $name"
	fi
	[ -s "$out" ] || fail "profile '$name' rendered an empty document"

	# A key helm could not resolve prints literally, and the resulting manifest
	# is valid YAML — so this only shows up as a container arg of "<no value>".
	if grep -q '<no value>' "$out"; then
		grep -n '<no value>' "$out" >&2
		fail "profile '$name' rendered an unset value as '<no value>'"
	fi

	# Render must produce the workload the chart promises, with the serve
	# command it promises.
	grep -q '^kind: Deployment' "$out" || fail "profile '$name' rendered no Deployment"
	grep -q 'args:' "$out" || fail "profile '$name' rendered a container without an args list"

	local flags
	flags="$(grep -oE '^\s*- +--[a-z0-9][a-z0-9-]*' "$out" | grep -oE '\-\-[a-z0-9-]+' | sort -u | wc -l | tr -d ' ')"
	[ "$flags" -gt 0 ] || fail "profile '$name' passed no flags to levee serve"
	echo "   rendered $flags distinct serve flags"
}

render_profile defaults
render_profile cluster --set mode=cluster
render_profile cluster-no-snapshot --set mode=cluster --set engine.snapshotDir=""
render_profile ingress --set ingress.enabled=true --set ingress.host=levee.example.com
render_profile postgres --set postgres.enabled=true
render_profile cluster-3-workers --set mode=cluster --set cluster.workers=3

echo "== helm package"
# Packaging is how the chart reaches users (deploy/README.md tells them to
# install an .tgz), and it is the only step that validates Chart.yaml's
# version fields against the registry filename.
(
	cd "$WORKDIR" && "$HELM" package "$(cd - >/dev/null; pwd)/$CHART" \
		--destination "$WORKDIR/pkg" >/dev/null
) || fail "helm package rejected $CHART"
ls "$WORKDIR"/pkg/*.tgz >/dev/null 2>&1 || fail "helm package produced no .tgz"

echo "DELIVERY VALIDATION PASSED"
