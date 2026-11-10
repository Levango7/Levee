#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""容器靶场灾备验收驱动：S1 长命令失联 / S2 专线中断 / S3 DB 切换。

为什么单独一个驱动（不复用 labdrive.py）：labdrive.py 驱动的是单机
SQLite 形态（levee apply 进程内同步执行）；灾备三场景要的是集群形态——
PG 存储 + master/worker 两个 serve 进程 + docker 级的杀进程/断网/重启 DB
操作 + REST 恢复入口。动作序列与断言完全不同，硬塞 labdrive 会把两条
都已验证的路径搅在一起。

拓扑（全在 docker 网 levee-dr-net 上）：
  levee-dr-pg     postgres:16-alpine（命名卷 levee-dr-pgdata）
  levee-dr-master levee serve --cluster --node-role master --node-id node-b
  levee-dr-worker levee serve --cluster --node-role worker --node-id node-a
  levee-lab-1..3  既有靶机容器（docker network connect 进本网）

master 的 REST 网关发布到宿主机 127.0.0.1:18088；宿主机 CLI 经
127.0.0.1:15432 直连 PG 做元数据操作（new/plan/approve/secret）。
每个场景跑前 ensure_infra 会清卷重建 PG（元数据全新，证据已在宿主机
落盘，不跨场景复用）。

用法（在 deploy/lab/container-lab/ 下）：
    python clusterdrive.py up          # 起全拓扑（幂等）
    python clusterdrive.py s1          # 场景 1：kill -9 执行节点
    python clusterdrive.py s2          # 场景 2：执行节点网络分区（进程活）
    python clusterdrive.py s3          # 场景 3：PG 进程级重启
    python clusterdrive.py status      # 集群现状速览
    python clusterdrive.py down        # 全拆（保留靶机；卷数据保留）
    python clusterdrive.py <任意 levee 子命令>   # 宿主机 CLI 透传（PG 库）

证据一律落 run/cluster/evidence/<场景>/（.gitignore 已覆盖 run/）。
"""

import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
CLUSTER = os.path.join(HERE, "run", "cluster")
EVIDENCE = os.path.join(CLUSTER, "evidence")
CONFIG_HOST = os.path.join(CLUSTER, "config.host.yaml")
CONFIG_SERVE = os.path.join(CLUSTER, "config.serve.yaml")
INVENTORY = os.path.join(CLUSTER, "inventory.yaml")
WORKFLOW = os.path.join(HERE, "workflows", "lab-cluster-takeover.yaml")
CRED_FILE = os.path.join(HERE, "run", "credential-value.json")

NET = "levee-dr-net"
PG = "levee-dr-pg"
MASTER = "levee-dr-master"
WORKER = "levee-dr-worker"
NODE_IMG = "levee-lab-node:local"
PG_DSN_CTR = "postgres://levee:levee-pass@levee-dr-pg:5432/levee?sslmode=disable"
PG_DSN_HOST = "postgres://levee:levee-pass@127.0.0.1:15432/levee?sslmode=disable"
MASTER_HTTP = "http://127.0.0.1:18088"
TARGETS = ["levee-lab-1", "levee-lab-2", "levee-lab-3"]
MARKER = "/tmp/levee-takeover-marker.txt"
MASTER_PASSWORD = "lab-master-pass"
WORKER_ID = "node-a"
MASTER_ID = "node-b"
WF_NAME = "lab-cluster-takeover"

SERVE_CONFIG = """# clusterdrive.py 生成（容器侧 serve 共用这一份；--pg-dsn 与 database.dsn
# 必须同名同值，serve 启动会校验）。channel.ssh.* 只有 serve 路径读
# （container-lab README §4.1）：strict_host_check=false 是本靶场的刻意
# 选择，生产必须 true。
server:
  data_dir: /tmp/levee
  log_level: info
  log_format: text

database:
  driver: postgres
  dsn: {dsn}

channel:
  ssh:
    port: 22
    auth_method: password
    key_path: ""
    known_hosts: /tmp/levee/known_hosts
    strict_host_check: false
    connect_timeout: 10s
    pool_size: 4
    become_method: ""
    become_user: ""

credential:
  storage: local
  encryption: aes256-gcm
  key_derivation: argon2id

executor:
  default_concurrency: 10
  connect_timeout: 10s
  # 300s：hold 步骤 sleep 120 必须小于执行期超时，留出 kill/分区操作窗口。
  exec_timeout: 300s

verify:
  prometheus_url: ""

notify:
  webhook:
    enabled: false
  jira:
    enabled: false
  chatops:
    enabled: false
"""

def host_config():
    # 宿主机 CLI 用：DSN 指到发布的 15432，data_dir 落本机可执行路径。
    # 不能对模板做 replace——{dsn} 占位符必须先经 format，否则宿主配置里
    # 会留下字面量 `{dsn}`（YAML 把它解析成 map，CLI 启动即报错）。
    return SERVE_CONFIG.format(dsn=PG_DSN_HOST).replace(
        "data_dir: /tmp/levee", "data_dir: " + CLUSTER + "/data")

INVENTORY_YAML = """# 集群态清单（clusterdrive.py 用）：地址是容器名（docker 网 DNS），
# 端口 22（容器直连，不走宿主机发布端口）。标签与 run/inventory.yaml
# 同形，lab-cluster-takeover.yaml 的 query env=lab AND role=web 命中
# web 两台。
groups:
  - name: lab-web
  - name: lab-app

targets:
  - address: levee-lab-1
    port: 22
    channel_type: ssh
    group: lab-web
    credential_ref: ssh-lab
    labels: {env: lab, role: web, tier: canary}

  - address: levee-lab-2
    port: 22
    channel_type: ssh
    group: lab-web
    credential_ref: ssh-lab
    labels: {env: lab, role: web, tier: regular}

  - address: levee-lab-3
    port: 22
    channel_type: ssh
    group: lab-app
    credential_ref: ssh-lab
    labels: {env: lab, role: app, tier: regular}
"""


class LabError(Exception):
    pass


def levee_bin():
    env = os.environ.get("LEVEE_BIN")
    if env:
        return env
    for name in ("levee.exe", "levee"):
        candidate = os.path.join(HERE, name)
        if os.path.exists(candidate):
            return candidate
    return "levee"


def sh(args, env=None, cwd=HERE):
    return subprocess.run(args, capture_output=True, env=env, cwd=cwd)


def docker(args):
    return sh(["docker"] + args)


def levee_cli(args):
    e = dict(os.environ)
    e["LEVEE_MASTER_PASSWORD"] = MASTER_PASSWORD
    cmd = [levee_bin(), "-c", CONFIG_HOST] + args
    proc = sh(cmd, env=e)
    out = proc.stdout.decode("utf-8", "replace") if proc.stdout else ""
    err = proc.stderr.decode("utf-8", "replace") if proc.stderr else ""
    if proc.returncode != 0:
        raise LabError("levee %s failed rc=%d\nstdout:%s\nstderr:%s"
                       % (" ".join(args), proc.returncode, out, err))
    return out


def psql(sql, tolerate=False):
    proc = docker(["exec", PG, "psql", "-U", "levee", "-d", "levee",
                   "-t", "-A", "-c", sql])
    out = proc.stdout.decode("utf-8", "replace")
    if proc.returncode != 0:
        if tolerate:
            return None
        raise LabError("psql failed: %s %s" % (sql, out + proc.stderr.decode()))
    return out.strip()


def rest(method, path, body=None, timeout=15):
    req = urllib.request.Request(MASTER_HTTP + path, method=method)
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        req.add_header("content-type", "application/json")
    try:
        with urllib.request.urlopen(req, data, timeout=timeout) as resp:
            return resp.status, resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode("utf-8", "replace")
    except Exception as exc:  # connection refused / reset etc.
        return 0, str(exc)


def wait_until(fn, timeout, what, interval=3.0):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            last = fn()
        except Exception as exc:  # 启动窗口期的瞬时报错不算失败
            last = exc
        if last and not isinstance(last, Exception):
            return last
        time.sleep(interval)
    raise LabError("timeout waiting for %s (last=%r)" % (what, last))


def save(evdir, name, text):
    os.makedirs(evdir, exist_ok=True)
    path = os.path.join(evdir, name)
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(text if isinstance(text, str) else text.decode("utf-8", "replace"))
    print("  evidence: %s" % path)
    return text


def run_status(run_id):
    return psql("SELECT status FROM runs WHERE id='%s'" % run_id, tolerate=True)


def step_rows(run_id):
    return psql("SELECT step_name, host, status, started_at, completed_at FROM steps "
                "WHERE run_id='%s' ORDER BY started_at, step_name" % run_id, tolerate=True) or ""


def assignment(run_id):
    return psql("SELECT owner_node, epoch, state FROM run_assignment WHERE run_id='%s'"
                % run_id, tolerate=True)


def docker_logs(name, since="5m"):
    proc = docker(["logs", "--since", since, name])
    return (proc.stdout + proc.stderr).decode("utf-8", "replace")


def write_configs():
    os.makedirs(CLUSTER, exist_ok=True)  # config.serve.yaml 的落盘目录
    with open(CONFIG_SERVE, "w", encoding="utf-8") as handle:
        handle.write(SERVE_CONFIG.format(dsn=PG_DSN_CTR))
    with open(CONFIG_HOST, "w", encoding="utf-8") as handle:
        handle.write(host_config())
    with open(INVENTORY, "w", encoding="utf-8") as handle:
        handle.write(INVENTORY_YAML)


def ensure_infra():
    write_configs()
    if not os.path.exists(os.path.join(HERE, "levee-linux")):
        raise LabError("missing %s/levee-linux — build it first:\n"
                       "  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build "
                       "-o deploy/lab/container-lab/levee-linux ./cmd/levee" % HERE)
    # 无条件重建镜像：COPY 进镜像的 levee-linux 一更新，旧镜像就是陈旧
    # 二进制（围栏 nil-db 那类修只落在新二进制上，容器不重建=修没生效）。
    proc = docker(["build", "-f", "Dockerfile.node", "-t", NODE_IMG, "."])
    if proc.returncode != 0:
        raise LabError("docker build node image failed: %s" % proc.stderr.decode())
    docker(["network", "create", NET])  # 已存在无所谓
    # 每个场景一套全新控制面库。必须先拆容器再删卷：docker volume rm 对被
    # 运行中容器挂载的卷直接报错，而这里的返回码一度被忽略——旧数据静默
    # 存活，上一场景的 run/assignment 行混进新场景的"全新库"，所有结论
    # 都建立在脏数据上。
    for name in (MASTER, WORKER, PG):
        docker(["rm", "-f", name])
    vol = docker(["volume", "rm", "-f", "levee-dr-pgdata"])
    if vol.returncode != 0 and b"no such volume" not in vol.stderr.lower():
        raise LabError("volume rm levee-dr-pgdata failed: %s" % vol.stderr.decode())
    docker(["run", "-d", "--name", PG, "--network", NET,
            "-e", "POSTGRES_USER=levee", "-e", "POSTGRES_PASSWORD=levee-pass",
            "-e", "POSTGRES_DB=levee",
            "-v", "levee-dr-pgdata:/var/lib/postgresql/data",
            "-p", "15432:5432", "postgres:16-alpine"])
    wait_until(lambda: docker(["exec", PG, "pg_isready", "-U", "levee"]).returncode == 0,
               60, "pg ready")
    # master / worker serve。node-id 的字母序决定同负载时派发给谁：
    # worker 叫 node-a、master 叫 node-b，dispatch 同负载按 ID 升序放置，
    # 单个 run 必然落在 worker 上（放置确定性，运行时仍断言 owner）。
    for name, node_id, role, publish in (
            (MASTER, MASTER_ID, "master", ["-p", "127.0.0.1:18088:8088"]),
            (WORKER, WORKER_ID, "worker", [])):
        docker(["rm", "-f", name])
        docker(["run", "-d", "--name", name, "--network", NET] + publish + [
            "-v", CONFIG_SERVE + ":/etc/levee/config.yaml:ro",
            "-e", "LEVEE_MASTER_PASSWORD=" + MASTER_PASSWORD,
            NODE_IMG, "serve", "-c", "/etc/levee/config.yaml",
            "--cluster", "--pg-dsn", PG_DSN_CTR,
            "--node-id", node_id, "--node-addr", name + ":9090",
            "--node-role", role,
            "--engine-enabled", "--insecure",
            "--addr", "0.0.0.0:9090", "--http-addr", "0.0.0.0:8088"])
    wait_until(lambda: rest("GET", "/healthz")[0] == 200, 60, "master healthz")

    def both_nodes():
        out = levee_cli(["cluster", "status", "--pg-dsn", PG_DSN_HOST])
        return out if (WORKER_ID in out and MASTER_ID in out) else None
    wait_until(both_nodes, 60, "both cluster nodes registered", interval=5)
    # 靶机：起的起、接进网
    for t in TARGETS:
        if docker(["inspect", "-f", "{{.State.Running}}", t]).stdout.strip() != b"true":
            docker(["start", t])
        c = docker(["network", "connect", NET, t])
        if c.returncode != 0 and b"already" not in c.stderr.lower():
            raise LabError("network connect %s: %s" % (t, c.stderr.decode()))
    wait_until(lambda: all(
        docker(["exec", t, "nc", "-z", "localhost", "22"]).returncode == 0 for t in TARGETS),
        60, "target sshd listening")
    # 元数据：目标进库 + 凭据值（值从文件读，不走命令行）
    levee_cli(["target", "import", "--file", INVENTORY])
    sh([levee_bin(), "-c", CONFIG_HOST, "secret", "revoke", "--name", "ssh-lab"],
       env=dict(os.environ, LEVEE_MASTER_PASSWORD=MASTER_PASSWORD))  # 首跑无此凭据，容忍
    with open(CRED_FILE, "r", encoding="utf-8") as handle:
        value = handle.read().strip()
    levee_cli(["secret", "add", "--name", "ssh-lab", "--type", "ssh_password",
               "--value", value])


def new_run(evdir):
    out = levee_cli(["template", "create", "--name", WF_NAME,
                     "--content", open(WORKFLOW, "r", encoding="utf-8").read()])
    save(evdir, "template-create.txt", out)
    out = levee_cli(["new", WF_NAME])
    m = re.search(r"run_id:\s*(\S+)", out)
    if not m:
        raise LabError("new did not print run_id: %s" % out)
    run_id = m.group(1)
    save(evdir, "new.txt", out)
    out = levee_cli(["plan", run_id, "--targets", TARGETS[0]])
    save(evdir, "plan.txt", out)
    # standard 档＝1 名审批人即可（internal/approval/levels.go 的档位表）；
    # alice 一票即满足，run 进入 approved。
    out = levee_cli(["--as", "alice", "approve", run_id, "--comment", "lab drill"])
    save(evdir, "approve-alice.txt", out)
    # bob 的第二票必须被拒：standard 档没有"等两人"的 pending 可言。这条
    # 拒绝被落盘成证据——它是档位语义的机器断言，若第二票能投进去，
    # 审批档位就名存实亡了。
    proc = sh([levee_bin(), "-c", CONFIG_HOST, "--as", "bob", "approve", run_id,
               "--comment", "redundant second vote"],
              env=dict(os.environ, LEVEE_MASTER_PASSWORD=MASTER_PASSWORD))
    err = proc.stderr.decode("utf-8", "replace")
    save(evdir, "approve-bob-rejected.txt", "rc=%d\n%s" % (proc.returncode, err))
    if proc.returncode == 0 or "no pending approval" not in err:
        raise LabError("bob's redundant vote was not rejected as expected: rc=%d %s"
                       % (proc.returncode, err))
    return run_id


def wait_dispatched(run_id):
    # 认领窗口可能只有秒级（派发秒级完成、执行秒级失败），只断言"派发
    # 发生过"（assignment 行存在且 owner 是 worker），不断言轮询瞬间
    # 仍是 pending/executing。
    def assigned():
        a = assignment(run_id)
        return a if (a and a.split("|")[0] == WORKER_ID) else None
    wait_until(assigned, 90, "dispatch assigns run to %s" % WORKER_ID)


def wait_marker_appeared():
    # 账本要等整个 closure 结束才落步级行（persist.go 的单 now 回算），
    # 但物理副作用在执行节点上立刻发生——标记文件就是"这一轮 attempt
    # 真的跑过 write-marker"的物理证据。场景开始已 rm -f，出现即本轮所写。
    def there():
        if docker(["exec", TARGETS[0], "test", "-f", MARKER]).returncode == 0:
            return marker_mtime()
        return None
    return wait_until(there, 60, "marker file appear on target (attempt 1 executed write-marker)")


def step_total(run_id, step_name=None):
    where = "run_id='%s'" % run_id
    if step_name:
        where += " AND step_name='%s'" % step_name
    row = psql("SELECT count(*) FROM steps WHERE " + where, tolerate=True)
    return int(row) if row and row.isdigit() else -1


def trace_event_count(run_id, event):
    row = psql("SELECT count(*) FROM trace WHERE run_id='%s' AND event='%s'"
               % (run_id, event), tolerate=True)
    return int(row) if row and row.isdigit() else -1


def save_traces(evdir, run_id, tag):
    save(evdir, "trace-%s.txt" % tag,
         psql("SELECT event, actor, detail, timestamp FROM trace WHERE run_id='%s' "
              "ORDER BY timestamp, id" % run_id, tolerate=True) or "(none)")


def save_locks(evdir, tag):
    save(evdir, "locks-%s.txt" % tag,
         psql("SELECT scope, owner, acquired_at, expires_at FROM locks "
              "ORDER BY scope", tolerate=True) or "(none)")


def wait_lock_free(evdir, tag, timeout=330):
    # 目标机锁的 owner 是引擎的临时执行 id（ClosureRunner.Run 铸造），与
    # runs 表的持久 id 不同——硬杀后这行锁不会随接管消失，只能等 TTL
    # （= exec_timeout 300s）自然过期。这是防物理双跑的安全网（锁 TTL
    # 覆盖单步执行超时，锁过期⇒旧执行已无在途步骤），驱动必须等，不能
    # 绕过。S2/S3 里旧 worker 的 closure 自己 defer 释放锁，愈合后很快就
    # 空；只有 S1（kill -9）要等满 TTL。
    def free():
        row = psql("SELECT count(*) FROM locks WHERE scope='host:%s' "
                   "AND expires_at > now()" % TARGETS[0], tolerate=True)
        if row is None or not row.isdigit():
            return None  # PG 不可读不算"无锁"
        return True if row == "0" else None
    wait_until(free, timeout, "target lock on %s released or expired" % TARGETS[0])
    save_locks(evdir, tag)


def wait_status(run_id, status, timeout):
    wait_until(lambda: run_status(run_id) == status, timeout,
               "run status -> %s" % status)


def retry_rest(run_id, evdir):
    code, body = rest("POST", "/changes/%s/retry" % run_id,
                      {"replan": False, "targetHosts": [TARGETS[0]]}, timeout=300)
    save(evdir, "retry-rest.txt", "HTTP %d\n%s" % (code, body))
    if code not in (200, 201):
        raise LabError("REST retry returned HTTP %d: %s" % (code, body))


class Check:
    def __init__(self):
        self.rows = []

    def __call__(self, ok, name, detail=""):
        self.rows.append((bool(ok), name, detail))
        print("  [%s] %s%s" % ("PASS" if ok else "FAIL", name,
                               (" — " + detail) if detail else ""))
        return bool(ok)

    def verdict(self):
        n = len(self.rows)
        k = sum(1 for ok, _, _ in self.rows if ok)
        print("VERDICT: %s (%d/%d)" % ("PASS" if k == n else "FAIL", k, n))
        return k == n


def snapshot(evdir, run_id, tag):
    save(evdir, "steps-%s.txt" % tag, step_rows(run_id))
    save(evdir, "assignment-%s.txt" % tag, assignment(run_id) or "(none)")
    save(evdir, "run-%s.json" % tag, levee_cli(["show", run_id, "--json"]))
    save(evdir, "cluster-status-%s.txt" % tag,
         levee_cli(["cluster", "status", "--pg-dsn", PG_DSN_HOST]))


def marker_mtime():
    proc = docker(["exec", TARGETS[0], "stat", "-c", "%Y", MARKER])
    return proc.stdout.decode().strip() if proc.returncode == 0 else None


def scenario_s1():
    ev = os.path.join(EVIDENCE, "s1")
    ensure_infra()
    docker(["exec", TARGETS[0], "rm", "-f", MARKER])
    run_id = new_run(ev)
    wait_dispatched(run_id)
    mtime_t1 = wait_marker_appeared()
    time.sleep(5)  # write-marker 之后立刻进 hold（sleep 120）；kill 落在 hold 窗口内
    snapshot(ev, run_id, "before")
    # —— kill -9 执行节点（容器级硬杀，等价于进程被 OOM/断电）——
    docker(["kill", "-s", "KILL", WORKER])
    wait_status(run_id, "interrupted", 120)
    snapshot(ev, run_id, "after-kill")
    code, body = rest("GET", "/system/takeover-events")
    save(ev, "takeover-events.txt", "HTTP %d\n%s" % (code, body))
    save_traces(ev, run_id, "after-kill")
    ck = Check()
    ck(run_status(run_id) == "interrupted",
       "run 收敛到 interrupted（不是卡 running）", run_status(run_id))
    ck(code == 200 and run_id in body, "接管事件可读且属于本 run", "HTTP %d" % code)
    # 诚实口径①：硬杀在途轮次的账本是零行——步级行只在 closure 结束时
    # 一次性落盘（persist.go 单 now 回算），死者没机会写，也不该补写。
    ck(step_total(run_id) == 0,
       "被 kill 一轮账本零行（步级行本就在 closure 结束时落盘）",
       "steps=%d" % step_total(run_id))
    ck(marker_mtime() == mtime_t1,
       "死者没有晚期写（标记文件 mtime 未变）",
       "%s -> %s" % (mtime_t1, marker_mtime()))
    # 硬杀后目标机锁成了孤儿（owner 是引擎临时执行 id，接管不碰它）：
    # 取证 + 等到 TTL 自然过期，再谈恢复。
    orphan = psql("SELECT owner FROM locks WHERE scope='host:%s'" % TARGETS[0],
                  tolerate=True)
    ck(orphan not in (None, "") and orphan != run_id,
       "硬杀后目标机锁仍挂在引擎临时执行 id 上（接管不碰它，等 TTL 过期）",
       "owner=%s" % orphan)
    save_locks(ev, "after-kill")
    wait_lock_free(ev, "after-ttl")
    # —— 人驱动恢复（REST retry，replan=false；已完成批次跳过的前提是
    # 账本里有 success 行——这里一行都没有，所以物理重跑 ——
    retry_rest(run_id, ev)
    wait_status(run_id, "completed", 240)
    snapshot(ev, run_id, "after-retry")
    save_traces(ev, run_id, "after-retry")
    mtime_t2 = marker_mtime()
    ck(run_status(run_id) == "completed", "retry 后 run completed", run_status(run_id))
    # 账本恰好一次：重跑轮次里每步各恰一行 success，没有鬼影行。
    ck(step_total(run_id, "write-marker") == 1,
       "write-marker 恰好一行 success（账本 exactly-once）",
       "rows=%d" % step_total(run_id, "write-marker"))
    ck(step_total(run_id, "hold") == 1,
       "hold 恰好一行 success", "rows=%d" % step_total(run_id, "hold"))
    # 物理 at-least-once：标记文件被重写过一次（mtime 推后）。这是设计事实，
    # 不是缺陷——目标机副作用可能重跑，账本不会。
    ck(mtime_t2 is not None and mtime_t1 is not None and int(mtime_t2) > int(mtime_t1),
       "标记文件被重跑轮次重写（物理 at-least-once，mtime 推后）",
       "%s -> %s" % (mtime_t1, mtime_t2))
    ck(docker(["exec", TARGETS[0], "test", "-f", MARKER]).returncode == 0,
       "目标机标记文件仍在")
    save(ev, "worker-logs.txt", docker_logs(WORKER, "30m"))
    save(ev, "master-logs.txt", docker_logs(MASTER, "30m"))
    return ck.verdict()


def scenario_s2():
    ev = os.path.join(EVIDENCE, "s2")
    ensure_infra()
    docker(["exec", TARGETS[0], "rm", "-f", MARKER])
    run_id = new_run(ev)
    wait_dispatched(run_id)
    mtime_t1 = wait_marker_appeared()
    time.sleep(5)
    snapshot(ev, run_id, "before")
    # —— 专线中断：worker 进程活着，但与 PG/目标的网络都断（真分区）——
    docker(["network", "disconnect", NET, WORKER])
    alive = docker(["exec", WORKER, "ps", "aux"]).stdout.decode("utf-8", "replace")
    save(ev, "worker-ps-during-partition.txt", alive)
    save(ev, "worker-logs-partition.txt", docker_logs(WORKER, "2m"))
    wait_status(run_id, "interrupted", 120)
    snapshot(ev, run_id, "after-partition")
    code, body = rest("GET", "/system/takeover-events")
    save(ev, "takeover-events.txt", "HTTP %d\n%s" % (code, body))
    # —— 愈合：旧 worker 的 closure 尾声会对已换主的 run 做晚期写，围栏必须拦 ——
    docker(["network", "connect", NET, WORKER])
    fenced = wait_until(lambda: trace_event_count(run_id, "evidence_discarded"),
                        120, "旧 worker 晚期落盘被围栏拦下（trace evidence_discarded）")
    save_traces(ev, run_id, "after-heal")
    save(ev, "worker-logs-after-heal.txt", docker_logs(WORKER, "3m"))
    ck = Check()
    ck("levee" in alive and "serve" in alive,
       "分区期间 worker 进程活着（ps 为证，区别于 S1 的 kill -9）")
    ck(run_status(run_id) == "interrupted",
       "run 收敛到 interrupted（不卡 running）", run_status(run_id))
    ck(code == 200 and run_id in body, "接管事件可读且属于本 run", "HTTP %d" % code)
    # 愈合后旧 worker 醒了也想写账本：围栏拦下，零行入帐。
    ck(step_total(run_id) == 0,
       "愈合后鬼影写被拦：steps 仍为零行", "steps=%d" % step_total(run_id))
    ck(fenced > 0,
       "trace 里有 evidence_discarded（围栏拒绝对已换主 run 的落盘）",
       "count=%d" % fenced)
    # 旧 worker 的 closure 收尾会自己 defer 释放目标机锁；这里确认锁已空
    # 再 retry，断言"恢复不会与任何残余执行物理撞车"。
    wait_lock_free(ev, "after-heal")
    retry_rest(run_id, ev)
    wait_status(run_id, "completed", 240)
    snapshot(ev, run_id, "after-retry")
    save_traces(ev, run_id, "after-retry")
    mtime_t2 = marker_mtime()
    ck(run_status(run_id) == "completed", "愈合 + retry 后 run completed",
       run_status(run_id))
    ck(step_total(run_id, "write-marker") == 1 and step_total(run_id, "hold") == 1,
       "恢复恰好一次：每步各一行 success",
       "write-marker=%d hold=%d" % (step_total(run_id, "write-marker"),
                                     step_total(run_id, "hold")))
    ck(mtime_t2 is not None and mtime_t1 is not None and int(mtime_t2) > int(mtime_t1),
       "标记文件被重跑轮次重写（物理 at-least-once）",
       "%s -> %s" % (mtime_t1, mtime_t2))
    ck(docker(["exec", TARGETS[0], "test", "-f", MARKER]).returncode == 0,
       "目标机标记文件仍在")
    save(ev, "master-logs.txt", docker_logs(MASTER, "30m"))
    return ck.verdict()


def scenario_s3():
    ev = os.path.join(EVIDENCE, "s3")
    ensure_infra()
    docker(["exec", TARGETS[0], "rm", "-f", MARKER])
    run_id = new_run(ev)
    wait_dispatched(run_id)
    mtime_t1 = wait_marker_appeared()
    time.sleep(5)
    snapshot(ev, run_id, "before")
    # —— DB 切换（本机诚实口径：PG 进程级停启，数据在命名卷上）——
    docker(["stop", PG])
    time.sleep(15)  # 短 outage（< 租约 TTL 30s）：控制面停滞但不该误判
    docker(["start", PG])
    wait_until(lambda: docker(["exec", PG, "pg_isready", "-U", "levee"]).returncode == 0,
               60, "pg back")
    mid = run_status(run_id)
    save(ev, "status-after-short-outage.txt",
         "run status after short PG outage (< TTL): %r\n" % mid)
    docker(["stop", PG])  # 这次停过 TTL：租约必然过期
    time.sleep(45)
    docker(["start", PG])
    wait_until(lambda: docker(["exec", PG, "pg_isready", "-U", "levee"]).returncode == 0,
               60, "pg back again")
    wait_status(run_id, "interrupted", 150)
    snapshot(ev, run_id, "after-db")
    code, body = rest("GET", "/system/takeover-events")
    save(ev, "takeover-events.txt", "HTTP %d\n%s" % (code, body))
    # 活 worker 的 closure 在 PG 恢复后才结束：它醒来想写账本时租约已换主，
    # 围栏必须拦下（trace evidence_discarded）。
    fenced = wait_until(lambda: trace_event_count(run_id, "evidence_discarded"),
                        120, "活 worker 晚期落盘被围栏拦下（trace evidence_discarded）")
    save_traces(ev, run_id, "after-outage")
    ck = Check()
    ck(mid == "running",
       "短 outage（<TTL）期间 run 保持 running（控制面停滞但无误判）", mid)
    ck(run_status(run_id) == "interrupted",
       "长 outage（>TTL）后 run 收敛到 interrupted", run_status(run_id))
    ck(code == 200 and run_id in body, "接管事件可读且属于本 run", "HTTP %d" % code)
    # 在途轮次 closure 没跑完就被换主：账本零行，与 S1/S2 同口径。
    ck(step_total(run_id) == 0,
       "在途轮次账本零行（closure 未跑完就被换主）", "steps=%d" % step_total(run_id))
    ck(fenced > 0,
       "trace 里有 evidence_discarded（活 worker 醒来时租约已换主）",
       "count=%d" % fenced)
    # 同 S2：活 worker 的 closure 收尾自己释放锁；确认锁空再 retry。
    wait_lock_free(ev, "after-outage")
    retry_rest(run_id, ev)
    wait_status(run_id, "completed", 240)
    snapshot(ev, run_id, "after-retry")
    save_traces(ev, run_id, "after-retry")
    mtime_t2 = marker_mtime()
    ck(run_status(run_id) == "completed", "retry 后 run completed", run_status(run_id))
    ck(step_total(run_id, "write-marker") == 1 and step_total(run_id, "hold") == 1,
       "恢复恰好一次：每步各一行 success",
       "write-marker=%d hold=%d" % (step_total(run_id, "write-marker"),
                                     step_total(run_id, "hold")))
    ck(mtime_t2 is not None and mtime_t1 is not None and int(mtime_t2) > int(mtime_t1),
       "标记文件被重跑轮次重写（物理 at-least-once）",
       "%s -> %s" % (mtime_t1, mtime_t2))
    save(ev, "master-logs.txt", docker_logs(MASTER, "30m"))
    save(ev, "worker-logs.txt", docker_logs(WORKER, "30m"))
    return ck.verdict()


def cmd_status():
    print(levee_cli(["cluster", "status", "--pg-dsn", PG_DSN_HOST]))
    print(psql("SELECT id, status, updated_at FROM runs ORDER BY updated_at DESC LIMIT 10"))
    print(psql("SELECT run_id, owner_node, epoch, state, result FROM run_assignment"))


def cmd_down():
    for name in (MASTER, WORKER, PG):
        docker(["rm", "-f", name])
    for t in TARGETS:
        docker(["network", "disconnect", NET, t])
    docker(["network", "rm", NET])
    print("targets 与卷数据保留；要清卷再跑：docker volume rm levee-dr-pgdata")


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    mode = sys.argv[1]
    if mode == "up":
        ensure_infra()
        print("infra ready: pg + master(%s) + worker(%s) + 3 targets" % (MASTER_ID, WORKER_ID))
        return 0
    if mode == "down":
        cmd_down()
        return 0
    if mode == "status":
        cmd_status()
        return 0
    if mode == "s1":
        return 0 if scenario_s1() else 1
    if mode == "s2":
        return 0 if scenario_s2() else 1
    if mode == "s3":
        return 0 if scenario_s3() else 1
    # 透传：宿主机 CLI 直连 PG 库
    e = dict(os.environ)
    e["LEVEE_MASTER_PASSWORD"] = MASTER_PASSWORD
    proc = sh([levee_bin(), "-c", CONFIG_HOST] + sys.argv[1:], env=e)
    if proc.stdout:
        sys.stdout.write(proc.stdout.decode("utf-8", "replace"))
    if proc.stderr:
        sys.stderr.write(proc.stderr.decode("utf-8", "replace"))
    return proc.returncode


if __name__ == "__main__":
    try:
        sys.exit(main())
    except LabError as exc:
        print("LAB-ERROR: %s" % exc, file=sys.stderr)
        sys.exit(1)
