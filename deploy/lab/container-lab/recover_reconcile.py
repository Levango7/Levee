#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""断网恢复四步对账工具（断网策略 Q4）。

为什么需要它：断网策略 Q4 拍板的是**人驱动**的四步对账——自动重驱在状态
不明时自动产生远端副作用，正面违反 R1（docs/design-cluster-failover.md §2）。
但「人驱动」不等于「看着像好了」：设计文档 §3 第 3 条要求四步每一步都有
机器判据。本工具把那四步的判据写成可执行断言，并把证据落盘。

四步（与 docs/design-disconnect-policy.md §2.4 一一对应）：

    ① 愈合确认   —— 控制面数据库读得到（psql SELECT 1 + /system/status 可读）
    ② 孤儿锁     —— locks 表里目标机的未过期锁为 0（TTL 300s 是上界不是估计）
    ③ 人驱动重驱 —— POST /changes/<id>/retry，replan=false + 显式 targetHosts
                     **必须**显式 --apply；不给就只打印将要发的请求并退出
    ④ 对账断言   —— run 终态 completed；每步恰一行 success；目标机标记文件
                     mtime 推后（物理 at-least-once 如实呈现）；trace 没有新增
                     evidence_discarded（围栏没有再丢证据）

与 #96 驱动同构：判断与取数复用 container-lab/clusterdrive.py（同一套 psql /
docker / REST 入口），不另造一套连接方式。

用法（在 deploy/lab/container-lab/ 下，拓扑需已由 clusterdrive.py up 起好）：

    python recover_reconcile.py --run-id <run-id> --target levee-lab-1
    python recover_reconcile.py --run-id <run-id> --target levee-lab-1 --apply

不带 --apply 时只跑到第 ② 步并打印第 ③ 步将要发出的请求（人驱动边界）。
"""

import argparse
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import clusterdrive as cd  # noqa: E402  (同目录驱动复用：同一套 psql/docker/REST)


# --------------------------------------------------------------------------
# 纯判断函数（无 I/O，可被单测承重）
# --------------------------------------------------------------------------

def parse_locks(psql_text):
    """把 `scope|owner|acquired_at|expires_at` 文本行解析成 dict 列表。

    clusterdrive.save_locks 的列序就是这四列；解析放在这里而不是散在流程里，
    是为了让「哪些锁算孤儿」这条规则有一个能被单测钉住的落点。
    """
    rows = []
    for line in (psql_text or "").splitlines():
        line = line.strip()
        if not line or line == "(none)":
            continue
        parts = line.split("|")
        if len(parts) < 4:
            continue
        rows.append({
            "scope": parts[0].strip(),
            "owner": parts[1].strip(),
            "acquired_at": parts[2].strip(),
            "expires_at": parts[3].strip(),
        })
    return rows


def judge_locks(rows, target):
    """目标机上「未过期」的锁是否还有。

    返回 (ok, detail)。判据与 clusterdrive.wait_lock_free 同款：PG 读不到
    不算「无锁」（那是未知），只有 count == 0 才算干净。
    """
    held = [r for r in rows if r.get("scope") == "host:%s" % target]
    if not held:
        return True, "no lock row for host:%s" % target
    # expires_at 是 PG 的 timestamptz 文本；与 PG 现在的比较交给调用方
    # （用 SQL 判过期），这里只报告「还有锁行」这一事实。
    return False, "%d lock row(s) still present for host:%s: %s" % (
        len(held), target, ", ".join("%s@%s" % (r["owner"], r["expires_at"]) for r in held))


def parse_step_rows(psql_text):
    """`step_name|host|status|started_at|completed_at` 文本行 → dict 列表。"""
    rows = []
    for line in (psql_text or "").splitlines():
        line = line.strip()
        if not line or line == "(none)":
            continue
        parts = line.split("|")
        if len(parts) < 3:
            continue
        rows.append({
            "step_name": parts[0].strip(),
            "host": parts[1].strip(),
            "status": parts[2].strip(),
            "started_at": parts[3].strip() if len(parts) > 3 else "",
            "completed_at": parts[4].strip() if len(parts) > 4 else "",
        })
    return rows


def judge_step_rows(rows):
    """每一步是否恰有一行 success。

    账本是 exactly-once：同一步出现两行 success 说明重驱写重了；出现 0 行
    success 说明这一步没有可出示的证据；出现非 success 行说明这一轮还没
    收敛。返回 (ok, detail, step_name)。
    """
    by_step = {}
    for row in rows:
        by_step.setdefault(row["step_name"], []).append(row)
    if not by_step:
        return False, "no step evidence rows at all", ""
    offenders = []
    for step, items in sorted(by_step.items()):
        successes = [i for i in items if i["status"] == "success"]
        if len(successes) != 1:
            offenders.append("%s: %d success row(s)" % (step, len(successes)))
    if offenders:
        return False, "; ".join(offenders), ""
    return True, "%d step(s), exactly one success row each" % len(by_step), ""


def judge_recovery(status, step_rows, marker_before, marker_after,
                   discarded_before, discarded_after, expected_steps=None):
    """第 ④ 步的四条机器判据。

    返回 [(ok, name, detail), ...]。四条判据各自独立可读：一条不成立就是
    「恢复没完成」，而不是笼统地说「看起来好了」。
    """
    checks = []

    checks.append((
        status == "completed",
        "run 终态为 completed",
        "status=%s" % status,
    ))

    ok, detail, _ = judge_step_rows(step_rows)
    if expected_steps:
        seen = {r["step_name"] for r in step_rows}
        missing = [s for s in expected_steps if s not in seen]
        if missing:
            ok = False
            detail += "; missing step(s): %s" % ",".join(missing)
    checks.append((ok, "每步恰一行 success 证据", detail))

    marker_ok = (marker_before is not None and marker_after is not None
                 and str(marker_after) > str(marker_before))
    checks.append((
        marker_ok,
        "目标机标记文件 mtime 推后（物理 at-least-once 如实呈现）",
        "before=%s after=%s" % (marker_before, marker_after),
    ))

    checks.append((
        discarded_after <= discarded_before,
        "trace 没有新增 evidence_discarded（围栏未再丢证据）",
        "before=%s after=%s" % (discarded_before, discarded_after),
    ))
    return checks


# --------------------------------------------------------------------------
# 现场流程
# --------------------------------------------------------------------------

def check_healed():
    """① 愈合确认：控制面读得到。"""
    row = cd.psql("SELECT 1", tolerate=True)
    if row is None:
        raise cd.LabError("healing check failed: control plane database is still unreachable")
    code, body = cd.rest("GET", "/system/status")
    if code != 200:
        raise cd.LabError("healing check failed: GET /system/status returned HTTP %d: %s" % (code, body))
    return body


def orphan_locks(target):
    """② 孤儿锁查询：目标机上未过期的锁行。"""
    row = cd.psql(
        "SELECT count(*) FROM locks WHERE scope='host:%s' AND expires_at > now()" % target,
        tolerate=True)
    if row is None:
        raise cd.LabError("cannot read the locks table — unknown is not 'no locks'")
    return int(row) if row.isdigit() else -1


def main(argv=None):
    parser = argparse.ArgumentParser(description="断网恢复四步对账（Q4）")
    parser.add_argument("--run-id", required=True, help="待恢复的 change/run id")
    parser.add_argument("--target", default=cd.TARGETS[0], help="目标机容器名")
    parser.add_argument("--evidence-dir", default=None, help="证据落盘目录")
    parser.add_argument("--expected-step", action="append", default=[],
                        help="期望存在的步骤名（可重复）；缺省只校验「有 success 行」")
    parser.add_argument("--apply", action="store_true",
                        help="显式执行第 ③ 步的重驱（人驱动边界：不带则只打印请求）")
    args = parser.parse_args(argv)

    evdir = args.evidence_dir or os.path.join(cd.EVIDENCE, "recovery-%s" % args.run_id)
    os.makedirs(evdir, exist_ok=True)

    print("step 1/4 愈合确认")
    status_body = check_healed()
    cd.save(evdir, "status-after-heal.json", status_body)
    print("  control plane reachable")

    print("step 2/4 孤儿锁查询")
    held = orphan_locks(args.target)
    cd.save(evdir, "locks-before-retry.txt",
            cd.psql("SELECT scope, owner, acquired_at, expires_at FROM locks "
                    "ORDER BY scope", tolerate=True) or "(none)")
    if held != 0:
        print("  %d unexpired lock row(s) on host:%s — waiting for TTL (upper bound 300s)"
              % (held, args.target))
        cd.wait_lock_free(evdir, "after-retry", timeout=330)
    print("  no unexpired lock on host:%s" % args.target)

    print("step 3/4 人驱动重驱")
    # 物理 at-least-once 的「前值」必须在重驱**之前**记：标记文件的 mtime
    # 推后是「这一轮真的又跑了一次」的物证，事后取值无法证明推后。
    marker_before = cd.marker_mtime()
    cd.save(evdir, "marker-before-retry.txt", str(marker_before or ""))
    body = {"replan": False, "targetHosts": [args.target]}
    request = "POST %s/changes/%s/retry %s" % (cd.MASTER_HTTP, args.run_id,
                                               json.dumps(body))
    if not args.apply:
        print("  --apply not given; the following request was NOT sent:")
        print("  " + request)
        print("  (Q4 is human-driven: automatic re-drive is refused by design)")
        return 2
    code, resp = cd.rest("POST", "/changes/%s/retry" % args.run_id, body, timeout=300)
    cd.save(evdir, "retry-rest.txt", "HTTP %d\n%s" % (code, resp))
    if code not in (200, 201):
        raise cd.LabError("REST retry returned HTTP %d: %s" % (code, resp))

    print("step 4/4 对账断言")
    status = cd.run_status(args.run_id)
    rows = parse_step_rows(cd.step_rows(args.run_id))
    marker = cd.marker_mtime()
    discarded = cd.trace_event_count(args.run_id, "evidence_discarded")
    cd.save(evdir, "steps-after-retry.txt", cd.step_rows(args.run_id))
    cd.save(evdir, "trace-after-retry.txt",
            cd.psql("SELECT event, actor, detail, timestamp FROM trace WHERE run_id='%s' "
                    "ORDER BY timestamp, id" % args.run_id, tolerate=True) or "(none)")

    checks = judge_recovery(status, rows, marker_before, marker, 0, discarded,
                            expected_steps=args.expected_step)
    ok = True
    for good, name, detail in checks:
        print("  [%s] %s — %s" % ("PASS" if good else "FAIL", name, detail))
        ok = ok and good
    print("VERDICT: %s" % ("PASS" if ok else "FAIL"))
    return 0 if ok else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except cd.LabError as exc:
        print("ERROR: %s" % exc, file=sys.stderr)
        sys.exit(1)
