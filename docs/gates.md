# Verification Gates

This document describes LEVEE's verification gates (`internal/verify`), the
LEVEELang declarations that materialise them (`internal/dsl` →
`internal/engine/step_gates.go`), and the runtime configuration each gate
type needs.

## Phases

A gate is bound to exactly one of four pipeline moments:

| Phase | Runs | Failure effect |
|---|---|---|
| `pre_apply` | before any change is applied | run aborts, nothing changed |
| `post_batch` | after each batch completes | next batch blocked, rollback triggered |
| `post_apply` | after apply finishes | rollback of the whole run |
| `grace_period` | after a configurable cool-down post-apply | rollback of the whole run |

## From workflow declaration to runnable gate

A compiled plan carries gate *declarations* (`plan.PlanStep.Gate`, a
`dsl.GateSpec` with `Pre` / `Batch` / `Post` check lists). Before a run
starts, `engine.materializeStepGates` compiles every declaration into a named
`verify.Gate` and registers it with the runner's `verify.GateManager`.
Names are deterministic per step, timing and index:

```
step:<name>:pre:<i>      dsl.GateSpec.Pre   → verify.PhasePreApply
step:<name>:batch:<i>    dsl.GateSpec.Batch → verify.PhasePostBatch
step:<name>:post:<i>     dsl.GateSpec.Post  → verify.PhasePostApply
```

Re-running a runner over the same plan overwrites registrations rather than
duplicating them.

### Fail-closed policy

A declaration the engine cannot execute aborts **materialisation** with an
error instead of being silently skipped: unknown check types, invalid params,
and slo/human gates whose runtime dependency is missing (below). A
declared-but-unexecutable gate must never masquerade as a passing one. The
same philosophy applies inside the gates themselves: configuration errors
detected at construction time are surfaced by `Check` as
`Passed=false` plus an error (mirroring `CommandGate.policyErr`).

### GateRuntime configuration

Some gates need process-level wiring beyond the workflow YAML. It is supplied
to the engine via `engine.WithGateRuntime(engine.GateRuntime{...})` on
`NewClosureRunner`:

| Field | Backs | Configuration pointer |
|---|---|---|
| `PrometheusURL string` | `slo` gates | `verify.prometheus_url` config key (e.g. `http://prom:9090`) |
| `Approver verify.HumanApprover` | `human` gates | approver transport implementation wired by the host process |

The zero `GateRuntime` is valid for plans that only declare `cmd` / `probe`
gates. A plan declaring `slo` without `PrometheusURL`, or `human` without an
`Approver`, fails materialisation with an explicit error naming the missing
configuration.

## Gate types

### `cmd` — command check (`command_gate.go`)

Runs a shell command through `GateInput.Channel`; judges exit code and
(optionally) stdout. No `GateRuntime` dependency; a missing channel reports
`Passed=false` ("missing channel") which fails the phase honestly.

| Param | Type | Default | Notes |
|---|---|---|---|
| `run` | string | — (required) | command line; subject to the verify-gate metacharacter blacklist |
| `expect_exit` | int | `0` | expected exit code |
| `expect_stdout` | string | unset | exact match after trailing-newline trim |
| `timeout` | duration | `30s` | per-attempt timeout |

### `probe` — http / tcp / script reachability probe (`probe_gate.go`)

Parameterised health probe. Needs **nothing** from `GateRuntime`: all knobs
live in the declaration's free-form `params:` mapping, validated strictly
(unknown keys are rejected fail-closed listing the valid keys).

Modes: `direct` (default) probes from the orchestrator's network position;
`remote` executes the check **from the target** through the live channel and
therefore requires one (missing channel ⇒ failed gate).

| Param | Type | Default | Applies to | Notes |
|---|---|---|---|---|
| `kind` | string | — (required) | all | `http` \| `tcp` \| `script` |
| `mode` | string | `direct` | all | `direct` \| `remote` |
| `url` | string | — (required for http) | http | supports `{target}` placeholder, expanded over **every** target (empty target list ⇒ one unexpanded request); all targets must pass or the failing target is named |
| `host_port` | string | — | tcp | `host:port` to test; `{target}` expansion supported; mutually exclusive with `port_from_target` |
| `port_from_target` | bool | `false` | tcp | parse `host:port` out of each `TargetIDs` entry instead |
| `expect_status` | string/int | `200-299` | http | range `"200-299"` or single code `"200"` / `200` |
| `body_contains` | string | unset | http direct | substring match on response body |
| `body_regex` | string | unset | http direct | regex match on response body |
| `script` | string | — (required for script) | script | multiline script, uploaded to `/tmp/.levee-probe-<8hex>` then executed; file removed either way |
| `interpreter` | string | `sh` | script | interpreter invoked as `<interpreter> <path>` |
| `timeout_seconds` | int | `10` | all | per-attempt timeout |
| `expect_exit` | int | `0` | script | expected script exit code |

Remote behaviour is **POSIX best-effort**: remote http shells out to
`curl -fsS -o /dev/null -w '%{http_code}' <url>` (pass = exit 0 and printed
status starts with `2`); remote tcp uses
`timeout 5 bash -c 'exec 3<>/dev/tcp/<host>/<port>'` (pass = exit 0).
Both require the corresponding tools on the target.

### `slo` — Prometheus threshold query (`slo_gate.go`)

Instant PromQL query compared against a numeric threshold. Bound to the
**post_batch** phase; declaring it in any other timing is a materialisation
error.

Requires `GateRuntime.PrometheusURL` (config key `verify.prometheus_url`);
without it materialisation fails with
`slo gate "<name>" requires verify.prometheus_url configuration`.

| Param | Type | Default | Notes |
|---|---|---|---|
| `query` | string | check's query field | PromQL instant expression; params override the classic field |
| `threshold` | float | — (required) | comparison operand |
| `comparison` | string | `lte` | `lt` \| `gt` \| `lte` \| `gte` (aliases `le`/`ge`/`eq` accepted); anything else is a hard error — never silently coerced |
| `timeout_seconds` | int | `5` | per-query HTTP timeout |

Behaviour: queries `{prometheus_url}/api/v1/query?query=<urlencoded>`;
a result with **zero series** fails closed; the first series' value is
compared. Query errors and threshold breaches retry per the SLOGate defaults
before reporting failure.

### `human` — blocking approval checkpoint (`human_gate.go`)

Calls `HumanApprover.RequestAndWait(ctx, runID, subject, reason)` at its
phase and passes only on explicit approval. Requires
`GateRuntime.Approver`; without one, materialisation fails naming the gate.

| Param | Type | Default | Notes |
|---|---|---|---|
| `reason` | string | `""` | presented to the approver |
| `timeout_seconds` | int | `1800` | wait budget; expiry/cancel ⇒ failed gate, not auto-pass |

Rejected / timed-out / cancelled decisions report `Passed=false` with a
clear message; approver transport failures additionally surface as errors.
The derived context honours parent cancellation, so aborting the run also
aborts the pending approval request.

**Limitations:** the MVP `HumanApprover` abstraction is single-approver and
blocking — no quorum / `min_approvers` semantics (workflow-level approvals
live in `internal/approval`) — and a slow approver delays its whole
`RunPhase` return, so give phases carrying human gates a sensible overall
deadline.

## Script trust level

Probe scripts (and every other gate-authored command fragment such as probe
`url` / `host_port` / `interpreter`) are **trusted-author content**: they come
from compiled plans authored by operators, mirroring the executor's
shell-module trust level. They are deliberately **not** subject to the
verify-gate metacharacter blacklist (`validateGateCommand`) used for `cmd`
checks — a multiline script cannot express itself under those restrictions,
and plan authors already hold full control over executed step commands. Treat
plan sources with the usual supply-chain care.

## Templates

Runnable examples live under [`examples/gate-templates/`](../examples/gate-templates):

- `nginx.yaml` — cmd gate (`systemctl is-active nginx`) + direct http probe
  with `{target}` expansion;
- `mysql.yaml` — cmd gate (`mysqladmin ping` variant) + remote tcp probe of
  `3306`;
- `redis.yaml` — batch-timing remote tcp probe of port `6379`
  (`batches.gate.probe`).

They are kept parser-valid by `TestParseGateTemplates`
(`internal/dsl/parser_test.go`).
