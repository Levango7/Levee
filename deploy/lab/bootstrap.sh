#!/bin/sh
# LEVEE 实验室引导：把二进制与安装器推到每台 VM，安装并启动（幂等）。
#
# 在你的工作站（Git Bash / WSL / 任意 POSIX sh，需 ssh/scp/openssl）运行：
#   LEVEE_BIN=./levee ./deploy/lab/bootstrap.sh root@192.168.56.10 root@192.168.56.11 ...
#
# 环境变量：
#   LEVEE_BIN      要安装的 levee 二进制（必填；make build 产物或 release 归档解出）
#   LEVEE_CONFIG   随发的配置（默认仓库根 config.example.yaml）
#
# 幂等：重复执行更新二进制、不覆盖既有 /etc/levee 配置；凭据只在首次生成并
# 打印一次。内部调用 deploy/baremetal/install.sh（建用户 / 装二进制 / 渲染
# systemd 单元 / 0600 凭据模板），生产语义与实验室一致。

set -eu

BIN=${LEVEE_BIN:?请设置 LEVEE_BIN=/path/to/levee}
[ -f "$BIN" ] || { echo "找不到二进制：$BIN" >&2; exit 1; }

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
CONFIG=${LEVEE_CONFIG:-$REPO/config.example.yaml}
INSTALLER="$SCRIPT_DIR/../baremetal/install.sh"

[ $# -gt 0 ] || { echo "用法：$0 user@host [user@host ...]" >&2; exit 2; }

for host in "$@"; do
    echo "== $host：推送 =="
    ssh "$host" 'mkdir -p /tmp/levee-install/deploy/systemd'
    scp -q "$BIN" "$host:/tmp/levee-install/levee"
    scp -q "$INSTALLER" "$host:/tmp/levee-install/install.sh"
    scp -q "$CONFIG" "$host:/tmp/levee-install/config.example.yaml"
    scp -q "$REPO/deploy/systemd/levee.service" "$host:/tmp/levee-install/deploy/systemd/levee.service"

    echo "== $host：安装（幂等）=="
    ssh "$host" "sh /tmp/levee-install/install.sh --binary /tmp/levee-install/levee --src /tmp/levee-install"

    echo "== $host：配置凭据并启动 =="
    # 凭据在远端生成：命名令牌（审计能证明"谁做的"）+ 引擎主密码。
    # 只在首次写入，令牌值仅此一次打印，请立即保存。
    ssh "$host" 'sh -s' <<'REMOTE'
set -eu
ENVF=/etc/levee/levee.env
touch "$ENVF"
chmod 0600 "$ENVF"
if ! grep -q "^LEVEE_SERVE_EXTRA=" "$ENVF"; then
    rand() { openssl rand -hex 24 2>/dev/null || head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
    TOKEN=$(rand)
    {
        echo "# 由 deploy/lab/bootstrap.sh 生成（实验室凭据，勿用于生产）"
        echo "LEVEE_SERVE_EXTRA=--auth-token admin=$TOKEN"
        echo "LEVEE_MASTER_PASSWORD=$(rand)"
    } >> "$ENVF"
    echo ">>> 命名令牌（仅此一次打印，立即保存）：admin = $TOKEN"
fi
systemctl daemon-reload 2>/dev/null || true
systemctl enable --now levee
systemctl is-active levee
REMOTE
done

echo ""
echo "全部完成。逐台验收："
echo "  curl -sf http://<host>:8080/healthz"
echo "  ssh <host> /usr/local/bin/levee doctor"
