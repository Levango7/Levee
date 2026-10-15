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

command -v git >/dev/null 2>&1 || fail "git not found: the release-version check proves referenced versions against the tag set"
[ -d "$CHART" ] || fail "chart directory not found: $CHART"

echo "== release version consistency (chart appVersion / image.tag / git tags / CHANGELOG)"
# Runs before the helm checks on purpose: it needs no rendering, and it is the
# only check that catches a chart shipping a version that was never released —
# a defect spread across Chart.yaml, values.yaml and CHANGELOG, invisible in a
# per-file review. It fails loudly when no tags are visible rather than passing
# quietly against an empty tag set.
python3 scripts/check_release_versions.py "$CHART" CHANGELOG.md \
	|| fail "release version references are inconsistent (see the checker output above)"

command -v "$HELM" >/dev/null 2>&1 || fail "helm not found (set HELM_BIN); the chart is a shipped artifact and must be rendered in CI"

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
render_profile single-postgres --set postgres.dsn="postgres://levee:ci-dsn-marker@db.internal:5432/levee"
render_profile single-postgres-embedded --set postgres.enabled=true
render_profile single-postgres-contradiction --set postgres.enabled=true --set config.database.driver=sqlite
render_profile single-postgres-external-secret --set postgres.externalSecretName=ops-managed-pg
render_profile single-postgres-external-conflict --set postgres.externalSecretName=ops-managed-pg --set postgres.dsn="postgres://levee:ignored@db:5432/levee"

# Content assertions on top of "it rendered". render_profile can only check that a
# document came out; a chart branch that renders an object nobody wires is
# invisible to it, and that is how `postgres.enabled=true` in single mode used to
# render NOTHING at all while the profile name promised PostgreSQL — every gate
# in the templates was `mode == cluster`.
require_in() {
	local name="$1" needle="$2" why="$3"
	grep -qF -- "$needle" "$WORKDIR/$name.yaml" || fail "profile '$name': missing [$needle] — $why"
}
refute_in() {
	local name="$1" needle="$2" why="$3"
	if grep -qF -- "$needle" "$WORKDIR/$name.yaml"; then
		grep -nF -- "$needle" "$WORKDIR/$name.yaml" >&2
		fail "profile '$name': found [$needle], which must not be rendered — $why"
	fi
}
# A Secret is referenced by name from the Deployment whether or not it is
# rendered, so grepping the name alone proves nothing about the object. This
# looks for the name inside a Secret document only.
require_secret() {
	local name="$1" want="$2" why="$3"
	if ! grep -A 3 -F "kind: Secret" "$WORKDIR/$name.yaml" | grep -qF -- "name: $want"; then
		fail "profile '$name': no Secret named [$want] was rendered — $why"
	fi
}
refute_secret() {
	local name="$1" want="$2" why="$3"
	if grep -A 3 -F "kind: Secret" "$WORKDIR/$name.yaml" | grep -qF -- "name: $want"; then
		fail "profile '$name': rendered a Secret named [$want] — $why"
	fi
}
# Occurrences, not mere presence: cluster mode passes --pg-dsn from two templates
# (master Deployment and worker StatefulSet). Asserting "the string appears" let a
# mutation delete it from the master while the worker's copy kept the gate green.
require_count() {
	local name="$1" needle="$2" want="$3" why="$4"
	local got
	got=$(grep -cF -- "$needle" "$WORKDIR/$name.yaml")
	[ "$got" -ge "$want" ] || fail "profile '$name': found [$needle] $got time(s), want >= $want — $why"
}

echo "== single-node PostgreSQL wiring"
# The DSN reaches the container through the Secret and LEVEE_DATABASE_DSN; the
# ConfigMap carries the driver and nothing else. `database.dsn` is bound in
# internal/config (allKeys) precisely so a credential never has to live in a
# ConfigMap, where `kubectl get cm -o yaml` shows it to anyone who can list.
require_in single-postgres "- name: LEVEE_DATABASE_DSN" "the pod must get its DSN from the Secret"
require_secret single-postgres "levee-pg" "the DSN Secret must exist outside cluster mode too"
require_in single-postgres "driver: postgres" "config.yaml must name the backend the operator asked for"
require_in single-postgres 'levee.qoder/database-backend: "postgres"' "the workload must state the backend it actually got"
# --pg-dsn without --cluster is a startup refusal (cmd_serve.go), so rendering it
# here would ship a pod that CrashLoops with a message the chart could have said.
refute_in single-postgres "- --pg-dsn" "single mode selects PostgreSQL through the configuration, not the cluster flag"
refute_in single-postgres-embedded "- --pg-dsn" "single mode selects PostgreSQL through the configuration, not the cluster flag"
require_in single-postgres-embedded "name: levee-postgres" "postgres.enabled must render the trial instance in single mode too"

echo "== an externally managed Secret is referenced, never recreated"
# The third PostgreSQL source. Chart-side `name: ops-managed-pg` in an env ref is
# the whole contract here: the chart must not also render a Secret with that name
# (it would overwrite whatever the platform owns), and the pod must still get the
# DSN through the same env path.
require_in single-postgres-external-secret "name: ops-managed-pg" "the external Secret must be the referenced source"
refute_secret single-postgres-external-secret "ops-managed-pg" "the chart must not render a Secret it does not own"
refute_secret single-postgres-external-secret "levee-pg" "no chart-created DSN Secret when an external one is named"
require_in single-postgres-external-secret "- name: LEVEE_DATABASE_DSN" "external or not, the DSN arrives the same way"
require_in single-postgres-external-secret "driver: postgres" "the backend is named whether the DSN is chart-made or ops-made"

echo "== when both sources are named, the external Secret wins outright"
# An operator who sets externalSecretName and leaves a postgres.dsn behind has
# given two answers to one question. The chart resolves it by deferring to the
# platform-owned Secret entirely: it must not render a Secret with that name
# (overwriting an object the cluster team owns is worse than a missing one), and
# it must not fall back to the values-side DSN.
require_in single-postgres-external-conflict "name: ops-managed-pg" "the external Secret keeps precedence over postgres.dsn"
refute_secret single-postgres-external-conflict "ops-managed-pg" "the chart must not render a Secret bearing the external Secret's name"
refute_in single-postgres-external-conflict "postgres://levee:ignored@db" "the values-side DSN must not be written anywhere once an external Secret is named"

echo "== the chart must not rewrite an explicit driver"
# postgres.enabled together with `driver: sqlite` is a contradiction the operator
# wrote. Silently coercing it to postgres would make the rendered artifact
# disagree with the values file; the binary's config validation refuses it
# loudly instead, so the chart leaves the value alone.
require_in single-postgres-contradiction "driver: sqlite" "an explicit driver is never rewritten by the chart"
require_in single-postgres-contradiction 'levee.qoder/database-backend: "sqlite"' \
	"the annotation must report what the operator actually asked for"

echo "== no PostgreSQL plumbing leaks into the plain render"
refute_in defaults "- name: LEVEE_DATABASE_DSN" "nothing may inject a DSN the operator did not ask for"
refute_secret defaults "levee-pg" "no DSN Secret without a PostgreSQL source"
refute_in defaults "name: levee-postgres" "no embedded instance unless postgres.enabled"
require_in defaults 'levee.qoder/database-backend: "sqlite"' "the default render is the SQLite shape it always was"

echo "== the DSN never lands in the ConfigMap"
# The one assertion that makes the Secret path meaningful: a plaintext credential
# in a ConfigMap is readable by anyone who can list them, and the marker below is
# the only place the DSN appears in the render.
config_block="$(awk '/kind: ConfigMap/,/^---/' "$WORKDIR/single-postgres.yaml")"
if printf '%s' "$config_block" | grep -qF "ci-dsn-marker"; then
	fail "single-postgres: the DSN was rendered into the ConfigMap — credentials belong in the Secret only"
fi
dsn_hits=$(grep -cF "ci-dsn-marker" "$WORKDIR/single-postgres.yaml")
[ "$dsn_hits" -eq 1 ] || fail "single-postgres: expected the DSN exactly once (in the Secret), found $dsn_hits"

echo "== cluster mode keeps its own door"
# --pg-dsn + LEVEE_PG_DSN is the cluster path (#47 refuses that flag without
# --cluster). Rendering LEVEE_DATABASE_DSN there too would let the bound env
# silently override a database.dsn the operator wrote in values.config.
require_count cluster "- --pg-dsn" 2 "both the master Deployment and the worker template must still pass the flag"
require_count cluster "- name: LEVEE_PG_DSN" 2 "both cluster workloads need the Secret-derived env"
refute_in cluster "- name: LEVEE_DATABASE_DSN" "cluster mode must not gain a second, overriding DSN source"

render_profile cluster-caps --set mode=cluster --set 'cluster.nodeCapabilities={shell,pkg}'

echo "== capability declaration reaches the container args"
# The toggle gates an arg block, and a gated block that is never rendered is
# never checked: an operator asking for it and getting a pod that never declares
# anything would look like a working feature until someone filtered on it.
# Checked PER DOCUMENT on purpose — cluster mode renders a master Deployment and
# a separate worker Deployment, and workers are the pods that receive dispatched
# runs. A whole-file grep would be satisfied by wiring only the master.
caps_doc() {
	awk -v src="levee/templates/$1" '
		$0 == "# Source: " src { f = 1; next }
		/^# Source: / { f = 0 }
		f' "$WORKDIR/cluster-caps.yaml"
}
for doc in deployment.yaml cluster-worker.yaml; do
	caps_doc "$doc" | grep -q -- '--node-capabilities' ||
		fail "cluster-caps: $doc rendered no --node-capabilities, so cluster.nodeCapabilities has no effect there"
	caps_doc "$doc" | grep -q '"shell,pkg"' ||
		fail "cluster-caps: $doc must pass the list as one joined value, not one flag per name"
done
# And the default deployment is untouched: a chart that never mentions the new
# value must start exactly as it did before it existed.
if grep -q -- '--node-capabilities' "$WORKDIR/cluster.yaml"; then
	grep -n -- '--node-capabilities' "$WORKDIR/cluster.yaml" >&2
	fail "--node-capabilities appeared in the default cluster render; the block must be gated"
fi

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
