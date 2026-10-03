#!/bin/sh
# levee 裸金属 / VM 安装脚本（幂等；重复执行安全）
#
# 用法（在解压好的 Release 包目录或源码目录下执行）：
#   sudo ./deploy/baremetal/install.sh --binary ./levee [--start]
#
# 选项：
#   --binary PATH     要安装的 levee 二进制（必填）
#   --prefix DIR      安装前缀（默认 /usr/local；二进制落在 DIR/bin/levee）
#   --data-dir DIR    数据目录（默认 /var/lib/levee）
#   --user NAME       运行用户（默认 levee，不存在则创建系统用户）
#   --no-systemd      跳过 systemd 单元安装（容器/非 systemd 主机）
#   --start           安装后 systemctl enable --now（默认只安装不启动）
#   --src DIR         仓库根目录（用于拷贝 config.example.yaml；默认脚本上两级）
#
# 环境变量 DESTDIR 会把所有绝对路径整体前移（打包/测试用，例如 DESTDIR=/tmp/root）。
#
# 安装后：
#   1) 编辑 /etc/levee/levee.env 配认证（serve 无认证默认拒绝启动）
#   2) systemctl start levee && levee doctor
#   3) 演练一条变更后再接入生产目标机（见 docs/deployment.md 交付检查单）

set -eu

BINARY=""
PREFIX="/usr/local"
DATA_DIR="/var/lib/levee"
RUN_USER="levee"
UNIT_DIR_NAME="systemd/system"
WITH_SYSTEMD=1
START_NOW=0
SRC_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)"

while [ $# -gt 0 ]; do
    case "$1" in
        --binary) BINARY="$2"; shift 2 ;;
        --prefix) PREFIX="$2"; shift 2 ;;
        --data-dir) DATA_DIR="$2"; shift 2 ;;
        --user) RUN_USER="$2"; shift 2 ;;
        --no-systemd) WITH_SYSTEMD=0; shift ;;
        --start) START_NOW=1; shift ;;
        --src) SRC_DIR="$2"; shift 2 ;;
        -h|--help) sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) echo "未知参数：$1（--help 查看用法）" >&2; exit 2 ;;
    esac
done

if [ -z "$BINARY" ] || [ ! -f "$BINARY" ]; then
    echo "错误：--binary 必须指向存在的 levee 二进制（当前：'$BINARY'）" >&2
    exit 2
fi
if [ "$(id -u)" != "0" ]; then
    echo "错误：需要 root（创建用户、写 /etc 与 systemd 单元）" >&2
    exit 2
fi

DESTDIR="${DESTDIR:-}"
ETC_DIR="${DESTDIR}/etc/levee"
BIN_DIR="${DESTDIR}${PREFIX}/bin"
UNIT_DIR="${DESTDIR}/etc/${UNIT_DIR_NAME}"
DATA_DIR_FULL="${DESTDIR}${DATA_DIR}"

say() { printf '%s\n' "$*"; }

# 1) 运行用户（幂等）
if ! id -u "$RUN_USER" >/dev/null 2>&1; then
    if command -v useradd >/dev/null 2>&1; then
        useradd --system --home-dir "$DATA_DIR_FULL" --shell /usr/sbin/nologin "$RUN_USER" 2>/dev/null \
            || useradd --system "$RUN_USER"
        say "已创建系统用户 $RUN_USER"
    else
        say "警告：无 useradd，跳过用户创建（单元里的 User=$RUN_USER 需要手动满足）"
    fi
else
    say "用户 $RUN_USER 已存在（跳过）"
fi

# 2) 目录
install -d -m 0755 "$BIN_DIR"
install -d -m 0750 -o root -g "$RUN_USER" "$ETC_DIR" 2>/dev/null || install -d -m 0750 "$ETC_DIR"
install -d -m 0750 -o "$RUN_USER" -g "$RUN_USER" "$DATA_DIR_FULL" 2>/dev/null || install -d -m 0750 "$DATA_DIR_FULL"

# 3) 二进制
install -m 0755 "$BINARY" "$BIN_DIR/levee"
say "已安装二进制：$BIN_DIR/levee"

# 4) 配置模板（不覆盖既有文件）
if [ ! -f "$ETC_DIR/config.yaml" ]; then
    if [ -f "$SRC_DIR/config.example.yaml" ]; then
        install -m 0640 "$SRC_DIR/config.example.yaml" "$ETC_DIR/config.yaml"
        say "已写入配置模板：$ETC_DIR/config.yaml（按现场修改）"
    else
        say "提示：未找到 config.example.yaml，serve 将以内置默认值 + 环境变量启动"
    fi
else
    say "配置已存在：$ETC_DIR/config.yaml（不覆盖）"
fi

# 5) 凭据/参数文件（不覆盖；serve 无认证拒绝启动，这里只放模板与警告）
if [ ! -f "$ETC_DIR/levee.env" ]; then
    cat > "$ETC_DIR/levee.env" <<'EOF'
# LEVEE serve 的环境文件。必须配置认证，否则 serve 拒绝启动。
# 推荐命名令牌（审计里能证明"谁做的"；重复追加多行）：
#   LEVEE_SERVE_EXTRA=--auth-token alice=<secret> --auth-token bob=<secret>
# 单令牌（仅试用）：
#   LEVEE_TOKEN=<随机长串>
# 执行引擎凭据面（启用 --engine-enabled 时必填，32+ 字符）：
#   LEVEE_MASTER_PASSWORD=<随机长串>
# 集群模式（节点自身参数也在这里给）：
#   LEVEE_SERVE_EXTRA=--cluster --pg-dsn postgres://levee:<pw>@<pg-host>:5432/levee?sslmode=require --node-id <唯一节点名> --node-addr <本机可达地址> --node-role master
EOF
    chmod 0600 "$ETC_DIR/levee.env"
    chown root:"$RUN_USER" "$ETC_DIR/levee.env" 2>/dev/null || true
    say "已写入凭据模板：$ETC_DIR/levee.env（0600 root:$RUN_USER）——必须先配置认证"
else
    say "凭据文件已存在：$ETC_DIR/levee.env（不覆盖）"
fi

# 6) systemd 单元
if [ "$WITH_SYSTEMD" = 1 ]; then
    UNIT_SRC="$SRC_DIR/deploy/systemd/levee.service"
    if [ ! -f "$UNIT_SRC" ]; then
        say "警告：找不到 $UNIT_SRC，跳过 systemd 单元"
    else
        install -d -m 0755 "$UNIT_DIR"
        sed -e "s#/usr/local/bin/levee#${PREFIX}/bin/levee#g" \
            -e "s#/var/lib/levee#${DATA_DIR}#g" \
            "$UNIT_SRC" > "$UNIT_DIR/levee.service"
        chmod 0644 "$UNIT_DIR/levee.service"
        say "已写入 systemd 单元：$UNIT_DIR/levee.service"
        if command -v systemctl >/dev/null 2>&1 && [ -z "$DESTDIR" ]; then
            systemctl daemon-reload
            if [ "$START_NOW" = 1 ]; then
                systemctl enable --now levee
                say "levee.service 已启动（systemctl status levee 查看）"
            else
                say "下一步：配置 $ETC_DIR/levee.env 后 systemctl enable --now levee"
            fi
        else
            say "提示：当前环境无 systemctl（或使用了 DESTDIR），未执行 daemon-reload"
        fi
    fi
fi

say ""
say "安装完成。建议顺序："
say "  1) 编辑 $ETC_DIR/levee.env：配置命名令牌（或 LEVEE_TOKEN）与 LEVEE_MASTER_PASSWORD"
say "  2) $PREFIX/bin/levee doctor   # 环境自检（配置/存储/通道/网络）"
say "  3) systemctl enable --now levee"
say "  4) 按 docs/deployment.md 的交付检查单走一遍完整演练后再接入生产目标机"
