#!/usr/bin/env bash
# calendar-repro-probe.sh — calendar Windows 偶发崩溃的**复现探针**。
#
# 背景（docs/product-roadmap.md 的 P1 行、docs/repro-probes.md）：
# `internal/calendar` 的测试在 CI 的 windows-latest 上偶发以
# `exit status 0xc0000005`（访问违例）崩掉——runtime 一个字都没打印，既没有
# panic，也没有 goroutine dump，更没有失败用例明细。复发时该取什么，账本里
# 早就写清楚了：
#
#   ① `go test -count=5 -v ./internal/calendar/` 的**重复**结果；
#   ② 完整 stderr；
#   ③ 崩溃**是否总与 `-cover` 插值**有关。
#
# 这个脚本就是把那三条变成自动采集的东西：
#
#   - 每一轮跑两次：不带 `-cover` 与带 `-cover`（回答 ③：只有带的那次崩，
#     就说明与覆盖率插值有关；两次都崩，说明与它无关）；
#   - 每一轮的完整 stdout/stderr 落盘（回答 ②）；
#   - **不提前中断**：一轮崩了就继续跑下一轮，因为「十次里崩一次」和「十次
#     里崩九次」是两种完全不同的故障，前者接近环境噪声，后者是代码问题；
#   - 结束时打印一份汇总（崩了几轮、哪一轮、哪种形态），并以非零退出码收尾
#     ——探针红了就是红了，它**不洗绿**（这正是账本里「不给 CI 加自动重试」
#     那条纪律：重试会把一次真崩溃洗成一次绿）。
#
# 产物目录由调用方通过 PROBE_DIR 指定（默认 ./repro-artifacts/calendar）。
#
# 用法：
#   PROBE_DIR=out/calendar RUNS=5 scripts/calendar-repro-probe.sh
#
# 只在 windows-latest 上有意义（Linux/macOS 上从未复现），但脚本本身跨平台，
# 在别的平台上跑一遍同样有价值：它给出的是「这条腿今天稳不稳」的基线。

set -uo pipefail

RUNS="${RUNS:-5}"
PROBE_DIR="${PROBE_DIR:-repro-artifacts/calendar}"
PKG="${PKG:-./internal/calendar/}"

mkdir -p "$PROBE_DIR"

# 崩溃时 Go 要打印 goroutine dump，否则「runtime 一个字都没打」这件事还会
# 继续发生——而那正是当初最难查的原因。crash dump 只有在有 dump 时才存在。
export GOTRACEBACK=all

echo "calendar repro probe: runs=${RUNS} pkg=${PKG} dir=${PROBE_DIR}"
go version || true
go env GOOS GOARCH GOVERSION || true
{
  echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "runner: ${RUNNER_NAME:-unknown} ${RUNNER_OS:-unknown}"
  echo "go: $(go version 2>&1)"
} > "${PROBE_DIR}/environment.txt"

plain_failures=0
cover_failures=0
failed_runs=()

for i in $(seq 1 "$RUNS"); do
  echo "=== round $i/${RUNS}: plain (no -cover) ==="
  go test -count=5 -v "$PKG" > "${PROBE_DIR}/plain-round-${i}.log" 2>&1
  plain_rc=$?
  echo "plain round ${i} rc=${plain_rc}"
  if [ "$plain_rc" -ne 0 ]; then
    plain_failures=$((plain_failures + 1))
    failed_runs+=("plain-${i}")
  fi

  echo "=== round $i/${RUNS}: with -cover ==="
  go test -cover -count=5 -v "$PKG" > "${PROBE_DIR}/cover-round-${i}.log" 2>&1
  cover_rc=$?
  echo "cover round ${i} rc=${cover_rc}"
  if [ "$cover_rc" -ne 0 ]; then
    cover_failures=$((cover_failures + 1))
    failed_runs+=("cover-${i}")
  fi
done

# 崩溃形态：访问违例（Windows 报 0xc0000005）在这批日志里的样子是
# 「没有 panic: 没有 goroutine」——Go 运行时被信号打断时未必来得及打印。
# 两种形态必须分开记账：带 panic 的说明是 Go 层面的失败（好查），没有的
# 说明进程被外部打断（难查，正是当前这一类）。
crash_shape="none"
if grep -lqs "panic:" "${PROBE_DIR}"/*.log 2>/dev/null; then
  crash_shape="go-panic"
elif grep -lqs "signal arrived during\|exit status 0xc0000005\|unexpected fault address" "${PROBE_DIR}"/*.log 2>/dev/null; then
  crash_shape="access-violation-without-panic"
fi

summary="${PROBE_DIR}/summary.txt"
{
  echo "runs=${RUNS}"
  echo "plain_failures=${plain_failures}"
  echo "cover_failures=${cover_failures}"
  echo "crash_shape=${crash_shape}"
  echo "failed_runs=${failed_runs[*]:-none}"
  echo "coverage_correlation=$( [ "$plain_failures" -eq 0 ] && [ "$cover_failures" -gt 0 ] && echo "yes" || echo "no" )"
} > "$summary"

echo "--- calendar repro summary ---"
cat "$summary"

if [ "$plain_failures" -gt 0 ] || [ "$cover_failures" -gt 0 ]; then
  echo "calendar repro probe: REPRODUCED (see ${PROBE_DIR})"
  exit 1
fi
echo "calendar repro probe: no failure in ${RUNS} rounds"
exit 0
