#!/bin/bash
# release_gate.sh — LEVEE 发布门禁检查
#
# 用法: ./scripts/release_gate.sh [--skip-e2e]
# 输出: 门禁报告到 stdout，通过/失败到退出码
#
# 门禁检查项:
#   G-01  编译通过        go build ./...
#   G-02  单元测试通过    go test ./... -count=1
#   G-03  go vet 无问题   go vet ./...
#   G-04  gofmt 格式正确  gofmt -l .
#   G-05  E2E 测试通过    go test -tags e2e ./tests/e2e/... -count=1
#   G-06  测试门禁 10 分钟  go test ./... -count=1 跑成(rc=0) 且 <= 600s
#   G-07  二进制可构建    go build -o /tmp/levee ./cmd/levee/
#
# 退出码:
#   0 — ALL PASS（所有门禁项通过）
#   1 — FAILED（存在失败项）

set -euo pipefail

# --- 参数解析 -----------------------------------------------------------------

SKIP_E2E=false
if [[ "${1:-}" == "--skip-e2e" ]]; then
    SKIP_E2E=true
fi

# --- 计数器与结果收集 ----------------------------------------------------------

PASS=0
FAIL=0
SKIP=0
RESULTS=()

# --- 检查函数 -----------------------------------------------------------------

check() {
    local name="$1"
    local cmd="$2"
    local expect="$3"  # "zero" or "empty"

    echo "检查 $name ..."
    local start
    start=$(date +%s)
    local output
    local exit_code=0
    output=$(eval "$cmd" 2>&1) || exit_code=$?
    local end
    end=$(date +%s)
    local duration=$((end - start))

    local passed=false
    case "$expect" in
        zero)   [[ $exit_code -eq 0 ]] && passed=true ;;
        empty)  [[ -z "$output" ]] && passed=true ;;
    esac

    if $passed; then
        PASS=$((PASS + 1))
        RESULTS+=("PASS  $name (${duration}s)")
    else
        FAIL=$((FAIL + 1))
        RESULTS+=("FAIL  $name (${duration}s)")
        echo "  输出: $(echo "$output" | head -5)"
    fi
}

# --- G-01: 编译通过 -----------------------------------------------------------

check "G-01 编译通过" "go build ./..." "zero"

# --- G-02: 单元测试通过 -------------------------------------------------------

check "G-02 单元测试通过" "go test ./... -count=1" "zero"

# --- G-03: go vet 无问题 ------------------------------------------------------

check "G-03 go vet 无问题" "go vet ./..." "zero"

# --- G-04: gofmt 格式正确 -----------------------------------------------------

check "G-04 gofmt 格式正确" "gofmt -l ." "empty"

# --- G-05: E2E 测试通过 -------------------------------------------------------

if ! $SKIP_E2E; then
    check "G-05 E2E 测试通过" "go test -tags e2e ./tests/e2e/... -count=1" "zero"
else
    SKIP=$((SKIP + 1))
    RESULTS+=("SKIP  G-05 E2E 测试通过 (--skip-e2e)")
fi

# --- G-06: 测试门禁 10 分钟 ---------------------------------------------------
#
# 判据是两条，不是一条：测试必须**跑成**（rc=0）才算数，然后才谈时长预算。
# 这里原先写成 `go test … || true` 再只比计时，于是"根本没跑起来"也能报 PASS。
# 2026-10-31 本机实测过一次：GOSUMDB=off 挡住 go.mod 声明的 1.26.10 工具链切换，
# G-01/02/03/07 全红，同一张报告里 G-06 是 PASS（1s）。一条会给坏状态背书的门
# 比没有门更糟——它把"没跑到"翻译成了"跑到且不超时"。

START_G6=$(date +%s)
RC_G6=0
go test ./... -count=1 > /dev/null 2>&1 || RC_G6=$?
END_G6=$(date +%s)
DURATION_G6=$((END_G6 - START_G6))
if [[ $RC_G6 -ne 0 ]]; then
    FAIL=$((FAIL + 1))
    RESULTS+=("FAIL  G-06 测试门禁 10 分钟 (go test rc=${RC_G6}，测试没跑成或跑红——先修这个再谈时长)")
elif [[ $DURATION_G6 -gt 600 ]]; then
    FAIL=$((FAIL + 1))
    RESULTS+=("FAIL  G-06 测试门禁 10 分钟 (${DURATION_G6}s > 600s)")
else
    PASS=$((PASS + 1))
    RESULTS+=("PASS  G-06 测试门禁 10 分钟 (${DURATION_G6}s)")
fi

# --- G-07: 二进制可构建 -------------------------------------------------------

check "G-07 二进制可构建" "go build -o /tmp/levee ./cmd/levee/" "zero"

# --- 输出报告 -----------------------------------------------------------------

echo ""
echo "================================"
echo "  LEVEE Release Gate Report"
echo "================================"
for r in "${RESULTS[@]}"; do
    echo "  $r"
done
echo "================================"
echo "  PASS: $PASS  FAIL: $FAIL  SKIP: $SKIP"
echo "================================"

if [[ $FAIL -eq 0 ]]; then
    echo "  Result: ALL PASS"
    exit 0
else
    echo "  Result: FAILED"
    exit 1
fi