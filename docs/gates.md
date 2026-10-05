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

A declaration this process cannot execute is refused **at plan time**, before
any target is touched: `internal/wiring` asks the engine's
`PlanGateBlockers(p, gateRuntime)` inside `GeneratePlan` — the one funnel every
plan goes through (gRPC `PlanChange`, `levee plan --local`, and the re-plan
inside apply) — and returns `engine.ErrGateNotExecutable`, which the gRPC layer
maps to `FailedPrecondition`. Materialisation applies the **same** judgement
again, so the two halves cannot disagree.

Why plan time and not just materialisation: `RunPhase` materialises gates when
its phase begins, so a post-apply or post-batch declaration that turns out to be
unexecutable kills the run **after** earlier batches already modified targets —
the operator learns about a configuration gap mid-change, with rollback to sort
out. The refusal is not a ban on the gate type: wire the capability and the same
workflow plans.

Also refused here: unknown check types, invalid params, and an `slo` check bound
to a phase the engine never evaluates. A declared-but-unexecutable gate must
never masquerade as a passing one. The same philosophy applies inside the gates
themselves: configuration errors detected at construction time are surfaced by
`Check` as `Passed=false` plus an error (mirroring `CommandGate.policyErr`).

### GateRuntime configuration

Some gates need process-level wiring beyond the workflow YAML. The deployment
builds exactly one value for it — `Engine.gateRuntime()` in `internal/wiring`,
which both the plan-time refusal and `engine.WithGateRuntime(...)` read:

| Field | Backs | Configuration pointer |
|---|---|---|
| `PrometheusURL string` | `slo` gates | `verify.prometheus_url` config key (e.g. `http://prom:9090`) |
| `Approver verify.HumanApprover` | `human` gates | approver transport implementation wired by the host process |

The zero `GateRuntime` is valid for plans that only declare `cmd` / `probe`
gates. A plan declaring `slo` without `PrometheusURL`, or `human` without an
`Approver`, is refused at plan time with an explicit error naming the missing
configuration.

> **No approver transport ships today.** `verify.HumanApprover` has exactly one
> production-shaped consumer path (`engine.GateRuntime.Approver`) and **zero**
> implementations in the repo outside tests — chat-ops round-trip, ticket-system
> callback and terminal prompt are all unwritten. So in a stock `levee serve`,
> `WithGateApprover` is never installed and any workflow declaring a `human`
> check is refused at plan time. Wiring one is a product decision (which
> transport, how the answer arrives, what happens on transport failure), tracked
> in `docs/product-roadmap.md`; installing it there is all a deployment needs —
> there is no second switch to remember.

> **`position` decides when a check runs, and all three declaration sites are
> executed.** A declaration's position keyword (`pre_apply` / `post_batch` /
> `post_apply`, spec §2.2) selects the `GateSpec` slot, and the slot is what binds
> a `verify.GatePhase`: `walkPlanGates` registers the workflow-level `gates:`
> entries, `batches.gate:`, and per-step `verify:` blocks alike. It did not used to
> work that way: `convertGate` filed every declaration in `GateSpec.Post`
> (`post_apply`), nothing populated `GateSpec.Batch` — the only slot bound to
> `PhasePostBatch` — and the walk covered step gates only, so a batch-level or
> workflow-level declaration was parsed, hashed into `plan_hash` and then run
> nowhere. Two consequences are now visible to authors: an `slo` check (which must
> run in `post_batch`) is declarable, and `examples/gate-templates/redis.yaml`
> really does run "after every batch" as its comment always claimed.
>
> That is a behaviour change, not a cleanup: a batch-level gate that has never run
> can now fail a batch that previously passed. See `CHANGELOG.md` and the
> `post_batch` routing row in `docs/product-roadmap.md`.

## Gate types

### `cmd` — command check (`command_gate.go`)

Runs a shell command through `GateInput.Channel`; judges exit code and
(optionally) stdout. No `GateRuntime` dependency; a missing channel reports
`Passed=false` ("missing channel") which fails the phase honestly.

> **现状（2026-10-05 实测）**：run 路径**从不给 `GateInput.Channel` 赋值**——
> `internal/engine/closure.go:393`（pre_apply）与 `:502`（post_batch）只填 RunID /
> BatchID / TargetIDs，全仓唯一的非测试 `Channel:` 赋值在 `internal/wiring/exec.go:114`，
> 而那是给步骤执行用的。所以今天任何被声明出来的 `cmd` 门禁都会以 "missing channel"
> 失败关闭：**它不会伪造通过（安全），但它也从未执行你声明的那条命令（不可用）**。
> `probe` 的 `remote` 模式同理。证据：`tests/integration/gate_block_rollback_e2e_test.go`
> 第一条用例；是否给门禁供通道是需要产品定夺的条目，登记在 `docs/product-roadmap.md`。

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
error. Declare it where the position reaches that phase — `batches.gate:`, a
step `verify:` block with `position: post_batch`, or a `gates:` entry with the
same position (see `examples/gate-templates/redis.yaml` for the shape).

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
