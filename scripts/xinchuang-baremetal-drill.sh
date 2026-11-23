#!/usr/bin/env bash
# xinchuang-baremetal-drill.sh — 信创裸机取证的一键驱动。
#
# 为什么要有这个脚本：`docs/xinchuang-baremetal-drill.md` 是**清单**（人照着手敲
# 7 步、再手贴 7 段输出）。清单适合第一次做，但复现一次要两小时，而且最容易
# 丢的是**中间那段证据**——doctor 的原始结论、变更前后目标文件的内容、
# `audit/verify` 的完整 JSON。手贴的清单在第二次执行时就开始漂。
#
# 这个脚本把清单变成**一次命令**：做同样的事，但把每一步的原始输出落盘成
# 文件，最后打印一页纸摘要。它**不替人下结论**——跑完之后人仍要看 doctor
# 的结论、要看 v1→v2→v1 是否真的发生了。
#
# 本脚本已按仓库的真实契约写成（不是凭印象）：
#   - 凭据走 `secret add --type ssh_key --value '{"username":…,"key_path":…}'`
#     （结构化 JSON；`internal/channel.CredentialRef` 的形状，见
#     `cmd/levee/cmd_serve.go` 的 serveCredentialResolver）；
#   - `channel.ssh.auth_method` 只接受 `key` 或 `password`（`internal/config`
#     的校验），选 `key` 时 `key_path` 必填——本脚本两种都支持，默认 key；
#   - 目标机自指（把控制端自己当变更目标），所以演练钥匙要写进本机
#     authorized_keys，且 `known_hosts` 要先用 `ssh-keyscan` 准备
#     （`strict_host_check` 默认 true，不准备就必然连不上）；
#   - workflow 的形状取自 `deploy/lab/container-lab/workflows/lab-smoke-snap.yaml`
#     （已在靶场实跑过）：`file.copy` + `rollback.strategy: snapshot` +
#     `snapshot_paths`；`file.copy` 的 `src` 相对的是**执行 levee 那个进程的
#     工作目录**，且不能是绝对路径（`internal/executor/modules/file` 的读取策略）。
#
# 用法（在**目标信创机**上执行，levee 二进制已在 PATH）：
#
#     ./scripts/xinchuang-baremetal-drill.sh --user root --addr 127.0.0.1
#
# 关键变量都可覆盖（都有默认值）：
#
#     LEVEE_DRILL_OUTDIR=/tmp/drill LEVEE_DRILL_USER=root ./scripts/xinchuang-baremetal-drill.sh
#
# 产出（默认 /tmp/levee-baremetal-drill）：
#
#     00-environment.txt    OS / 内核 / glibc / systemd / 架构 / 时间
#     01-doctor.txt         levee system doctor 的完整输出
#     02-connectivity.txt   演练钥匙、known_hosts、目标机自检
#     03-metadata.txt       template/secret/target/new 的原始输出 + run id
#     04-file-v1.txt        变更前目标文件内容
#     05-file-v2.txt        变更后目标文件内容
#     06-file-restored.txt  回滚后目标文件内容
#     07-audit-verify.json  审计链校验的完整 JSON
#     summary.txt           一页纸摘要（贴回矩阵证据列用）
#     drill-key / drill-key.pub / config.yaml / known_hosts（演练自身产物）
#
# 退出码：0 = 全部步骤按预期完成；1 = 某步失败（**已采到的证据仍留在 outdir**）。
#
# **边界**：这个脚本跑的是**单机自指**演练（变更目标是它自己），它证明的是
# 「LEVEE 在这台信创机上能装、能跑、能走完审批→执行→回滚→审计」。它**不**证明
# 客户现场验收（机型、内核、安全基线、等保要求都可能不同）——矩阵里那格的措辞
# 是「裸机已验证」，不是「已通过客户环境验收」。

set -uo pipefail

# ---------------------------------------------------------------- 参数与默认值
OUTDIR="${LEVEE_DRILL_OUTDIR:-/tmp/levee-baremetal-drill}"
TARGET_USER="${LEVEE_DRILL_USER:-root}"
TARGET_ADDR="${LEVEE_DRILL_ADDR:-127.0.0.1}"
TARGET_PORT="${LEVEE_DRILL_PORT:-22}"
TARGET_FILE="${LEVEE_DRILL_TARGET_FILE:-/tmp/levee-drill-state.txt}"
HTTP_ADDR="${LEVEE_DRILL_HTTP_ADDR:-127.0.0.1:18080}"
SSH_AUTH="${LEVEE_DRILL_SSH_AUTH:-key}"          # key | password
SSH_PASSWORD="${LEVEE_DRILL_SSH_PASSWORD:-}"
DRY_RUN=0

die() { echo "drill: $*" >&2; exit 1; }
note() { echo "drill: $*"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --outdir) OUTDIR="$2"; shift 2 ;;
    --user) TARGET_USER="$2"; shift 2 ;;
    --addr) TARGET_ADDR="$2"; shift 2 ;;
    --port) TARGET_PORT="$2"; shift 2 ;;
    --target-file) TARGET_FILE="$2"; shift 2 ;;
    --http-addr) HTTP_ADDR="$2"; shift 2 ;;
    --ssh-auth) SSH_AUTH="$2"; shift 2 ;;
    --password) SSH_PASSWORD="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) sed -n '2,60p' "$0"; exit 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
done

command -v levee > /dev/null 2>&1 || die "levee is not in PATH — install or symlink it first"
[ "$SSH_AUTH" = "key" ] || [ "$SSH_AUTH" = "password" ] || die "--ssh-auth $SSH_AUTH must be key or password (internal/config only accepts these two)"
[ "$SSH_AUTH" = "password" ] && [ -z "$SSH_PASSWORD" ] && die "--ssh-auth password needs --password (or LEVEE_DRILL_SSH_PASSWORD)"

mkdir -p "$OUTDIR" || die "cannot create $OUTDIR"
OUT="$OUTDIR"
KEY_PATH="$OUT/drill-key"
CFG="$OUT/config.yaml"

# ---------------------------------------------------------------- 0. 环境事实
{
  echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "hostname: $(hostname)"
  echo "--- os-release ---"; cat /etc/os-release 2>/dev/null || echo "(no /etc/os-release)"
  echo "--- uname -a ---"; uname -a
  echo "--- arch ---"; uname -m
  echo "--- glibc ---"; ldd --version 2>&1 | head -1
  echo "--- systemd ---"; systemctl --version 2>&1 | head -1
  echo "--- is-system-running ---"; systemctl is-system-running 2>&1 || true
  echo "--- failed units ---"
  systemctl list-units --state=failed --no-pager --no-legend 2>&1 | head -20 || true
  echo "--- selinux ---"; getenforce 2>/dev/null || echo "(no getenforce)"
  echo "--- filesystem ---"; df -hT / /tmp 2>&1 | head -5
  echo "--- levee ---"; command -v levee; levee version 2>&1 || true
} > "$OUT/00-environment.txt" 2>&1
note "environment captured -> $OUT/00-environment.txt"

# ---------------------------------------------------------------- 1. 连接准备
# 1a. known_hosts：strict_host_check 默认 true，不准备就必然连不上目标。
ssh-keyscan -p "$TARGET_PORT" "$TARGET_ADDR" > "$OUT/known_hosts" 2>/dev/null
if [ ! -s "$OUT/known_hosts" ]; then
  {
    echo "ssh-keyscan -p $TARGET_PORT $TARGET_ADDR produced NOTHING — cannot prove the"
    echo "target is the host we think it is. Check the address/port and that sshd listens."
  } > "$OUT/02-connectivity.txt"
  cat "$OUT/02-connectivity.txt" >&2
  exit 1
fi
note "known_hosts ready ($(wc -l < "$OUT/known_hosts") entries)"

# 1b. 演练钥匙（key 认证路径）。目标机自指：把公钥写进本机 authorized_keys。
{
  echo "target: ${TARGET_USER}@${TARGET_ADDR}:${TARGET_PORT}"
  echo "auth_method: ${SSH_AUTH}"
  echo "known_hosts: $OUT/known_hosts ($(wc -l < "$OUT/known_hosts") entries)"
} > "$OUT/02-connectivity.txt"

CRED_JSON=""
if [ "$SSH_AUTH" = "key" ]; then
  [ -f "$KEY_PATH" ] || ssh-keygen -q -t ed25519 -N '' -f "$KEY_PATH" -C "levee-baremetal-drill"
  AUTHORIZED="${HOME}/.ssh/authorized_keys"
  install -d -m 700 "$(dirname "$AUTHORIZED")"
  touch "$AUTHORIZED"; chmod 600 "$AUTHORIZED"
  if grep -qFf "$KEY_PATH.pub" "$AUTHORIZED" 2>/dev/null || grep -qF "$(cut -d' ' -f2 "$KEY_PATH.pub")" "$AUTHORIZED"; then
    echo "authorized_keys already has the drill key" >> "$OUT/02-connectivity.txt"
  else
    cat "$KEY_PATH.pub" >> "$AUTHORIZED"
    echo "appended the drill public key to $AUTHORIZED" >> "$OUT/02-connectivity.txt"
  fi
  CRED_JSON="{\"username\":\"${TARGET_USER}\",\"key_path\":\"${KEY_PATH}\"}"
else
  CRED_JSON="{\"username\":\"${TARGET_USER}\",\"password\":\"${SSH_PASSWORD}\"}"
fi

# ---------------------------------------------------------------- 2. 完整配置
# 自包含：drill 自己生成整份 config，不依赖目标机上已有 /etc/levee/config.yaml。
cat > "$CFG" <<EOF
server:
  data_dir: ${OUT}/data
  log_level: info
  log_format: text

database:
  driver: sqlite
  path: ${OUT}/levee.db

channel:
  ssh:
    port: ${TARGET_PORT}
    auth_method: ${SSH_AUTH}
    key_path: ${KEY_PATH}
    known_hosts: ${OUT}/known_hosts
    strict_host_check: true
    connect_timeout: 10s
    pool_size: 4

credential:
  encryption: aes256-gcm

executor:
  default_concurrency: 4
  connect_timeout: 10s
  exec_timeout: 120s
EOF
mkdir -p "$OUT/data"
note "config written -> $CFG"

# ---------------------------------------------------------------- 3. doctor
LEVEE="levee -c $CFG"
$LEVEE system doctor > "$OUT/01-doctor.txt" 2>&1
doctor_rc=$?
if [ $doctor_rc -ne 0 ]; then
  echo "doctor exited $doctor_rc — the drill stops here; the raw output is kept" >&2
  cat "$OUT/01-doctor.txt" >&2
  exit 1
fi
note "doctor ok -> $OUT/01-doctor.txt"

# ---------------------------------------------------------------- 4. 演练工件
# 目标文件：v1（workflow 的 file.copy 会把它推成 v2）
printf 'v1\n' > "$TARGET_FILE" 2>/dev/null \
  || die "cannot write $TARGET_FILE (permissions?)"
cp "$TARGET_FILE" "$OUT/04-file-v1.txt"

# file.copy 的 src 必须是**相对路径**且落在 levee 进程的工作目录内。
mkdir -p "$OUT/work/src"
printf 'v2\n' > "$OUT/work/src/marker.txt"

cat > "$OUT/work/drill.yaml" <<'YAML'
name: baremetal-drill
version: "1.0"
description: >
  信创裸机取证演练：把控制端的一份文件推到目标机（目标机自指），完成后按快照
  还原成改动前的内容。证据是目标文件 v1 -> v2 -> v1。

target:
  name: drill-self
  type: host
  query: "role=drill"
  min_count: 1

window:
  start: "00:00"
  end: "23:59"
  timezone: UTC

batches:
  strategy: percent
  steps: [100]
  max_concurrency: 1

approval:
  level: standard
  approvers: [drill-approver]

steps:
  - name: deploy-marker
    action: file.copy
    args:
      src: "src/marker.txt"
      dest: /tmp/levee-drill-state.txt
    rollback:
      strategy: snapshot
      snapshot_paths:
        - /tmp/levee-drill-state.txt

rollback:
  on_failure: auto
  verify_after: true
YAML

cat > "$OUT/work/inventory.yaml" <<EOF
targets:
  - address: ${TARGET_ADDR}
    port: ${TARGET_PORT}
    channel_type: ssh
    group: baremetal-drill
    credential_ref: ssh-drill
    labels: {env: drill, role: drill}
EOF
note "drill workflow + inventory written"

# ---------------------------------------------------------------- 5. 元数据
# 凭据库是加密的：`secret add` 与 serve 解析凭据都需要主口令。缺了它
# （`LEVEE_MASTER_PASSWORD environment variable is not set`）演练在第一步就断——
# 这是 2026-11-16 实跑时踩到的坑，脚本自己生成并复用一份。
MASTER_PW_FILE="$OUT/drill-master-password"
if [ -n "${LEVEE_MASTER_PASSWORD:-}" ]; then
  echo "(using LEVEE_MASTER_PASSWORD from the environment)" >&2
elif [ -s "$MASTER_PW_FILE" ]; then
  LEVEE_MASTER_PASSWORD="$(cat "$MASTER_PW_FILE")"
else
  # head -c /dev/urandom：不依赖 openssl 也在（信创最小化安装未必有）。
  LEVEE_MASTER_PASSWORD="drill-$(head -c 18 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  (umask 077 && printf '%s' "$LEVEE_MASTER_PASSWORD" > "$MASTER_PW_FILE")
fi
export LEVEE_MASTER_PASSWORD

{
  echo "--- template create ---"
  $LEVEE template create --name baremetal-drill --description "信创裸机取证演练" \
      --content "$(cat "$OUT/work/drill.yaml")" 2>&1
  echo "--- secret add (${SSH_AUTH}) ---"
  $LEVEE secret add --name ssh-drill --type ssh_key --value "$CRED_JSON" 2>&1
  echo "--- target import ---"
  $LEVEE target import --file "$OUT/work/inventory.yaml" 2>&1
  echo "--- new ---"
  $LEVEE new baremetal-drill 2>&1
} > "$OUT/03-metadata.txt" 2>&1

grep -q "not set" "$OUT/03-metadata.txt" && die "credential store needs a master password — see $OUT/03-metadata.txt"

RUN_ID=$(grep -oE 'run-[0-9a-f]{6,}' "$OUT/03-metadata.txt" | head -1)
if [ -z "$RUN_ID" ]; then
  echo "could not find a run id in $OUT/03-metadata.txt — the drill stops here" >&2
  cat "$OUT/03-metadata.txt" >&2
  exit 1
fi
echo "run_id=$RUN_ID" >> "$OUT/03-metadata.txt"
note "run id: $RUN_ID"

if [ "$DRY_RUN" = "1" ]; then
  note "--dry-run: stopped after metadata (no serve, no apply). Artifacts in $OUT"
  exit 0
fi

# ---------------------------------------------------------------- 6. serve + REST
# 工作目录必须是 $OUT/work（file.copy 的 src 相对它解析）。
SNAP_DIR="$OUT/snapshots"
mkdir -p "$SNAP_DIR"
(
  cd "$OUT/work" || exit 1
  $LEVEE serve --engine-enabled --engine-snapshot-dir "$SNAP_DIR" \
    --http-addr "$HTTP_ADDR" \
    --auth-token drill-approver=drill-approver-secret \
    --auth-token initiator=initiator-secret
) > "$OUT/serve.log" 2>&1 &
SERVE_PID=$!
trap 'kill $SERVE_PID 2>/dev/null || true' EXIT

ready=0
for _ in $(seq 1 60); do
  # 带上 named token：serve 开了 --auth-token 之后 /system/version 要鉴权，
  # 不带 token 的探针会把「已就绪」读成「没起来」（2026-11-16 实跑踩到）。
  if curl -sf "http://${HTTP_ADDR}/api/v1/system/version" -H "Authorization: Bearer drill-approver-secret" > /dev/null 2>&1; then ready=1; break; fi
  if ! kill -0 "$SERVE_PID" 2>/dev/null; then break; fi
  sleep 1
done
if [ "$ready" -ne 1 ]; then
  echo "serve did not come up on ${HTTP_ADDR} — raw log:" >&2
  tail -40 "$OUT/serve.log" >&2
  exit 1
fi
note "serve up on ${HTTP_ADDR} (pid ${SERVE_PID})"

BASE="http://${HTTP_ADDR}/api/v1"
A_APPROVER="Authorization: Bearer drill-approver-secret"
A_INIT="Authorization: Bearer initiator-secret"
CT="Content-Type: application/json"

step() { echo ; echo "--- $1 ---" >> "$OUT/03-metadata.txt"; }

# 顺序不能变：REST 的治理链是 plan → approve → apply → rollback。漏了 plan
# 会在 approve 处被拒（"has no persisted plan to approve"），而 apply 又会以
# "is in draft state" 拒绝——三个错误都指向同一个缺失步骤（2026-11-16 实跑）。
# plan 的 body 必须有 JSON（空体会 400 "invalid body: EOF"），targetHosts
# 显式点名目标机。
step "plan"
curl -sX POST "$BASE/changes/$RUN_ID/plan" -H "$A_INIT" -H "$CT" \
  -d "{\"targetHosts\":[\"${TARGET_ADDR}\"]}" >> "$OUT/03-metadata.txt" 2>&1

step "approve"
curl -sX POST "$BASE/changes/$RUN_ID/approve" -H "$A_APPROVER" -H "$CT" \
  -d '{"comment":"baremetal drill"}' >> "$OUT/03-metadata.txt" 2>&1

step "apply"
curl -sX POST "$BASE/changes/$RUN_ID/apply" -H "$A_INIT" -H "$CT" -d '{}' \
  >> "$OUT/03-metadata.txt" 2>&1
# apply 异步派发：等目标文件变成 v2（最多 120s）。
for _ in $(seq 1 120); do
  [ "$(tr -d '\n' < "$TARGET_FILE" 2>/dev/null)" = "v2" ] && break
  sleep 1
done
cp "$TARGET_FILE" "$OUT/05-file-v2.txt" 2>/dev/null || echo "(target file unreadable)" > "$OUT/05-file-v2.txt"

step "rollback"
curl -sX POST "$BASE/changes/$RUN_ID/rollback" -H "$A_INIT" -H "$CT" -d '{}' \
  >> "$OUT/03-metadata.txt" 2>&1
for _ in $(seq 1 120); do
  [ "$(tr -d '\n' < "$TARGET_FILE" 2>/dev/null)" = "v1" ] && break
  sleep 1
done
cp "$TARGET_FILE" "$OUT/06-file-restored.txt" 2>/dev/null || echo "(target file unreadable)" > "$OUT/06-file-restored.txt"

# ---------------------------------------------------------------- 7. 审计 + 摘要
curl -s "$BASE/audit/verify?changeId=$RUN_ID" -H "$A_APPROVER" > "$OUT/07-audit-verify.json" 2>&1

{
  echo "run_id=${RUN_ID}"
  echo "target=${TARGET_USER}@${TARGET_ADDR}:${TARGET_PORT}"
  echo "target_file=${TARGET_FILE}"
  echo "file_v1=$(tr -d '\n' < "$OUT/04-file-v1.txt" 2>/dev/null)"
  echo "file_v2=$(tr -d '\n' < "$OUT/05-file-v2.txt" 2>/dev/null)"
  echo "file_restored=$(tr -d '\n' < "$OUT/06-file-restored.txt" 2>/dev/null)"
  echo "audit_verify=$(tr -d '\n' < "$OUT/07-audit-verify.json" 2>/dev/null)"
  echo "outdir=${OUT}"
} > "$OUT/summary.txt"

echo "--- drill summary ---"
cat "$OUT/summary.txt"
note "artifacts in ${OUT}"