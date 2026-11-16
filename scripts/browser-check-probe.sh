#!/usr/bin/env bash
# browser-check-probe.sh — browser-check 在 windows-latest 上的偶发失败的
# **复现探针**。
#
# 背景（docs/product-roadmap.md 的 P1 行、docs/repro-probes.md）：这条腿的失败
# 有**两个不同模式**，账本分开登记：
#
#   模式 A：30 秒 settle 超时后断言读到空 DOM。已有 `settleDiagnosis` 自证
#           （mounted=false / inflight>0 / quiet<=150ms 三者哪一条没满足），
#           并对那个页面截图。
#   模式 B：`puppeteer.launch` 等不到 Chrome 的 WS 端点——**Chrome 进程本身
#           没起来**，无截图、无 settle note，从 "launching Chrome" 到作业结束
#           仅 38 秒。
#
# 探针要做的事：
#
#   - **重复跑**（默认 5 轮）：偶发问题的关键信息是频率，不是第一次的截图。
#     一轮只能回答「这次红了」，多轮能回答「十次红几次」。
#   - **失败不中断**：第一轮红了就停，等于把「十次里红一次」和「十次里红九次」
#     混成同一件事——前者接近环境噪声，后者是代码问题。
#   - 每轮的完整日志与截图分开存放（`.browser-shots/run-N/`），因为「哪一轮
#     红了」本身就是证据。
#   - 模式 B 的证据是 **Chrome 自己的 stdout/stderr**：探针打开
#     `BROWSER_CHECK_DUMPIO=1`，让浏览器进程的输出进日志（Chrome 起不来时
#     通常会说点什么，比如 profile 目录被占用、缺少 DLL）。
#   - 结束时打印汇总并以非零退出码收尾——**不洗绿**。
#
# 用法：
#   RUNS=5 scripts/browser-check-probe.sh
#
# 依赖与 browser-check.mjs 相同（node、puppeteer-core、一个能起来的 mock
# gateway 与 vite dev server）。服务在脚本内部启动并在结束时清理。

set -uo pipefail

RUNS="${RUNS:-5}"
PROBE_DIR="${PROBE_DIR:-repro-artifacts/browser}"
BROWSER="${BROWSER_CHECK_BROWSER:-chrome}"
MOCK_PORT="${MOCK_PORT:-4599}"
DEV_PORT="${DEV_PORT:-5173}"
LOG_DIR="${PROBE_DIR}/logs"
SHOT_ROOT="${PROBE_DIR}/shots"

mkdir -p "$LOG_DIR" "$SHOT_ROOT"

# Chrome 的 stdout/stderr 必须进日志：模式 B 的证据就在那里。
export BROWSER_CHECK_DUMPIO=1
export BROWSER_CHECK_BROWSER="$BROWSER"

# 前端必须知道 mock 网关在哪。
#
# 这一行是 2026-11-16 实跑时补上的：漏了它，vite 会把 API 代理指向默认的
# http://localhost:8080（没有服务在那儿），于是**每个页面都是空的**——
# rows=0、趋势图没有段、403 档渲染成「加载失败」。那不是被探的偶发问题，
# 是探针自己把环境接错了，而它会以「assert_failures」的样子出现，指向错误
# 的排查方向。CI 的 browser-check 腿里就有这一行（VITE_API_TARGET），探针
# 必须自己也有——它不共享那条腿的环境。
export VITE_API_TARGET="http://127.0.0.1:${MOCK_PORT}"

cleanup() {
  if [ -n "${MOCK_PID:-}" ]; then kill "$MOCK_PID" 2>/dev/null || true; fi
  if [ -n "${DEV_PID:-}" ]; then kill "$DEV_PID" 2>/dev/null || true; fi
}
trap cleanup EXIT

node scripts/mock-gateway.mjs > "${LOG_DIR}/mock-gateway.log" 2>&1 &
MOCK_PID=$!
npm run dev -- --host 127.0.0.1 --port "$DEV_PORT" > "${LOG_DIR}/vite.log" 2>&1 &
DEV_PID=$!

ready=0
for _ in $(seq 1 90); do
  if curl -sf "http://127.0.0.1:${MOCK_PORT}/__mode?mode=ok" > /dev/null 2>&1 \
     && curl -sf "http://127.0.0.1:${DEV_PORT}/src/main.ts" > /dev/null 2>&1; then
    ready=1
    break
  fi
  if ! kill -0 "$MOCK_PID" 2>/dev/null; then
    echo "browser-check probe: mock gateway died" | tee -a "${LOG_DIR}/probe.log"
    exit 1
  fi
  if ! kill -0 "$DEV_PID" 2>/dev/null; then
    echo "browser-check probe: dev server died" | tee -a "${LOG_DIR}/probe.log"
    exit 1
  fi
  sleep 1
done
if [ "$ready" -ne 1 ]; then
  echo "browser-check probe: services did not come up in 90s" | tee -a "${LOG_DIR}/probe.log"
  exit 1
fi

echo "browser-check probe: browser=${BROWSER} runs=${RUNS} dir=${PROBE_DIR}"

failed_runs=()
launch_failures=0
settle_failures=0
assert_failures=0
env_failures=0

for i in $(seq 1 "$RUNS"); do
  shot_dir="${SHOT_ROOT}/run-${i}"
  mkdir -p "$shot_dir"
  log="${LOG_DIR}/run-${i}.log"
  echo "=== round $i/${RUNS} ==="
  BROWSER_CHECK_REQUIRE=1 \
    BASE_URL="http://127.0.0.1:${DEV_PORT}" \
    MOCK_URL="http://127.0.0.1:${MOCK_PORT}" \
    SHOT_DIR="$shot_dir" \
    node scripts/browser-check.mjs --shots > "$log" 2>&1
  rc=$?
  echo "round ${i} rc=${rc} (log: ${log})"
  if [ "$rc" -ne 0 ]; then
    failed_runs+=("run-${i}")
    # 两个模式的判据各自有明确措辞：模式 B 是「Chrome 没起来」，模式 A 是
    # 「页面没 settle」。混在一起说「断言失败」会把排查方向带偏。
    if grep -qs "did not start within\|waiting for the WS endpoint" "$log"; then
      launch_failures=$((launch_failures + 1))
    elif grep -qs "settle" "$log"; then
      settle_failures=$((settle_failures + 1))
    elif grep -qs "puppeteer-core not found\|no chrome found\|no firefox found" "$log"; then
      # 探针自己的运行环境缺东西（没装 puppeteer-core / 机器上没有浏览器）。
      # 那不是控制台的回归，也不是这个偶发问题的复发——把它算成「断言失败」
      # 会把排查方向带偏，所以单列一类。
      env_failures=$((env_failures + 1))
    else
      assert_failures=$((assert_failures + 1))
    fi
  fi
done

summary="${PROBE_DIR}/summary.txt"
{
  echo "runs=${RUNS}"
  echo "browser=${BROWSER}"
  echo "failed_runs=${failed_runs[*]:-none}"
  echo "launch_failures=${launch_failures}"
  echo "settle_failures=${settle_failures}"
  echo "assert_failures=${assert_failures}"
  echo "env_failures=${env_failures}"
  # 一轮都没红不等于「今天稳」：也可能是这一轮根本没跑到断言（例如页面全空）。
  # 提示读日志的人先确认环境接对了，再谈复发。
  echo "note=if every round failed with rows=0 / 加载失败, check VITE_API_TARGET and the mock gateway first"
} > "$summary"

echo "--- browser-check probe summary ---"
cat "$summary"

if [ "${#failed_runs[@]}" -gt 0 ]; then
  echo "browser-check probe: REPRODUCED (see ${PROBE_DIR})"
  exit 1
fi
echo "browser-check probe: no failure in ${RUNS} rounds"
exit 0
