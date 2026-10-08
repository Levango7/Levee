// Health and check-verdict labels for the system page.
//
// Two vocabularies live in this file, deliberately as two lists and two tables,
// because the page renders both side by side:
//
//   - the overall daemon health, owned by `diagnosis.HealthStatus`
//     (internal/diagnosis/health_probe.go:39-49) — healthy / degraded /
//     unhealthy / unknown;
//   - the per-check verdict produced by `GET /system/doctor`, which today is
//     written as bare literals in internal/grpc/system_service.go
//     (pass / warn / fail / skip — `skip` is the database check declining to run
//     when no config is loaded, system_service.go:346-349). It has no owning
//     constant set yet.
//
// Four verdicts, not three: the guard in internal/grpc pins the list against
// whatever the endpoint actually assigns, and it caught `skip` — a value that
// would otherwise have rendered as the bare word on a Chinese page.
//
// They must stay separate. `unknown` means "the probe could not tell" on the
// health side; there is no such verdict for a single check. Collapsing them into
// one label table is what let #92's batch/assignment vocabularies drift, and the
// guards in internal/diagnosis and internal/grpc pin the two apart.
//
// Like ./batch.ts and ./assignment.ts, these are plain functions rather than
// component code: the repo has no @vue/test-utils, so what the page renders has
// to be assertable in a browser-free test runner.

export const HEALTH_STATES = ['healthy', 'degraded', 'unhealthy', 'unknown'] as const
export type HealthState = (typeof HEALTH_STATES)[number]

const HEALTH_LABEL: Record<HealthState, string> = {
  healthy: '健康',
  degraded: '降级',
  unhealthy: '不健康',
  unknown: '未知',
}

export const CHECK_VERDICTS = ['pass', 'warn', 'fail', 'skip'] as const
export type CheckVerdict = (typeof CHECK_VERDICTS)[number]

const VERDICT_LABEL: Record<CheckVerdict, string> = {
  pass: '通过',
  warn: '告警',
  fail: '未通过',
  skip: '已跳过',
}

/** Chinese label for the overall daemon health. Unknown spellings pass through. */
export function healthLabel(state: string): string {
  return HEALTH_LABEL[state as HealthState] ?? state
}

/** Chinese label for one doctor check's verdict. Unknown spellings pass through. */
export function verdictLabel(verdict: string): string {
  return VERDICT_LABEL[verdict as CheckVerdict] ?? verdict
}
