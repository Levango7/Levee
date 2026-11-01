#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""LEVEE 本地容器靶场驱动脚本。

为什么需要这个脚本（PowerShell 上的两个坑）：

1. **多行参数被按行拆散**。`levee template create --content <整份 YAML>` 在
   PowerShell 里会被拆成一个参数一行，原生程序收到的是一堆碎片。
2. **内嵌双引号被剥掉**。`--value '{"username":"root","password":"rootpass"}'`
   到程序手里变成 `{username:root,password:rootpass}`，**不报错**；下游按 JSON
   解析失败后会回退成"把整段当口令"，最终表现为一次莫名其妙的认证失败。

Python 的 `subprocess.run([...])` 列表形式在 Windows 下走 `list2cmdline`，
引号与换行都原样送达，所以这个脚本是"能不能复现"的关键，不是便利包装。

用法（在 `deploy/lab/container-lab/` 目录下）：

    python labdrive.py secret reset ssh-lab
    python labdrive.py target
    python labdrive.py template workflows/lab-smoke.yaml lab-smoke
    python labdrive.py new lab-smoke
    python labdrive.py plan <run-id> 127.0.0.11,127.0.0.12
    python labdrive.py apply <run-id> --force
    python labdrive.py <任意 levee 子命令...>      # 透传

二进制：优先用环境变量 `LEVEE_BIN`，其次同目录下的 `levee.exe`，
最后用 PATH 上的 `levee`。
"""

import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
RUNDIR = os.path.join(HERE, "run")
CFG = os.path.join(RUNDIR, "config.yaml")
CFG_TEMPLATE = os.path.join(RUNDIR, "config.template.yaml")
INVENTORY = os.path.join(RUNDIR, "inventory.yaml")
CRED_FILE = os.path.join(RUNDIR, "credential-value.json")


def ensure_dirs():
    """Create the local directories the lab needs.

    The sqlite store opens `run/data/levee.db` and applies
    `PRAGMA journal_mode=WAL` — it does **not** create the directory, and the
    failure surfaces as `unable to open database file (14)`, which reads like a
    permissions problem rather than a missing folder.
    """
    for sub in ("data", "snapshots"):
        path = os.path.join(RUNDIR, sub)
        if not os.path.isdir(path):
            os.makedirs(path)
            print("created %s" % path)


def ensure_config():
    """Generate run/config.yaml from the template on first run.

    The template carries a {{LAB_DIR}} placeholder: the config needs absolute
    paths (data_dir, the sqlite file) and hard-coding somebody's paths into a
    committed file is how recipes rot.
    """
    if not os.path.exists(CFG):
        if not os.path.exists(CFG_TEMPLATE):
            raise SystemExit("missing %s" % CFG_TEMPLATE)
        with open(CFG_TEMPLATE, "r", encoding="utf-8") as handle:
            body = handle.read()
        with open(CFG, "w", encoding="utf-8") as handle:
            handle.write(body.replace("{{LAB_DIR}}", HERE.replace("\\", "/")))
        print("generated %s" % CFG)
        return
    with open(CFG, "r", encoding="utf-8") as handle:
        if "{{LAB_DIR}}" in handle.read():
            raise SystemExit(
                "%s still carries the {{LAB_DIR}} placeholder — delete it and re-run "
                "(it is regenerated from the template)" % CFG)


def levee_bin():
    """Locate the levee binary: env override, then the local build, then PATH."""
    env = os.environ.get("LEVEE_BIN")
    if env:
        return env
    for name in ("levee.exe", "levee"):
        candidate = os.path.join(HERE, name)
        if os.path.exists(candidate):
            return candidate
    return "levee"


def env():
    """Environment for every levee call.

    LEVEE_MASTER_PASSWORD is the credential-store master key. The local CLI
    path needs it to dial credentialed channels (cmd/levee/engine_cli.go).

    LEVEE_FILE_MODULE_EXTRA_DIRS whitelists the directory the file module may
    read from: the file module refuses absolute src paths and ../ escapes, and
    it resolves a relative src against the **working directory of the levee
    process** — which is why this script runs levee with cwd=HERE (the lab
    root), and why the workflows' `src` is `workflows/lab-site.conf`.
    """
    e = dict(os.environ)
    e["LEVEE_MASTER_PASSWORD"] = e.get("LEVEE_MASTER_PASSWORD", "lab-master-pass")
    e["LEVEE_FILE_MODULE_EXTRA_DIRS"] = HERE
    return e


def run(args, capture=True, stdin=None):
    ensure_dirs()
    ensure_config()
    cmd = [levee_bin(), "-c", CFG] + args
    print("$ " + " ".join(cmd))
    proc = subprocess.run(
        cmd,
        capture_output=capture,
        env=env(),
        cwd=HERE,
        input=stdin,
    )
    out = proc.stdout.decode("utf-8", "replace") if proc.stdout else ""
    err = proc.stderr.decode("utf-8", "replace") if proc.stderr else ""
    if out:
        print(out.rstrip())
    if err:
        print("[stderr] " + err.rstrip())
    print("rc=%d" % proc.returncode)
    return proc.returncode, out, err


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return 2

    mode = sys.argv[1]

    if mode == "target":
        # target import --file run/inventory.yaml —— 幂等（按 address 匹配）
        return run(["target", "import", "--file", INVENTORY])[0]

    if mode == "template":
        # template create --name <名> --content <整份 YAML>
        yaml_path, name = sys.argv[2], sys.argv[3]
        with open(yaml_path, "r", encoding="utf-8") as handle:
            content = handle.read()
        return run(["template", "create", "--name", name, "--content", content])[0]

    if mode == "new":
        # new <模板名> —— 建 run；参数在打包期替换
        name = sys.argv[2]
        rc, out, _ = run(["new", name])
        if rc == 0:
            import re
            match = re.search(r"run_id:\s*(\S+)", out)
            if match:
                print("RUN_ID=" + match.group(1))
        return rc

    if mode == "plan":
        # plan <run-id> --targets <主机名,主机名>  —— 目标只能是清单里的主机名
        run_id, targets = sys.argv[2], sys.argv[3]
        args = ["plan", run_id, "--targets", targets]
        if "--dry-run" in sys.argv:
            args.append("--dry-run")
        return run(args)[0]

    if mode == "secret":
        # The value is read from a file, never passed on the command line —
        # see the module docstring for why.
        action, name = sys.argv[2], sys.argv[3]
        with open(CRED_FILE, "r", encoding="utf-8") as handle:
            value = handle.read().strip()
        print("credential value read from %s: %d bytes" % (CRED_FILE, len(value)))
        if action == "reset":
            run(["secret", "revoke", "--name", name])
            return run(["secret", "add", "--name", name, "--type", "ssh_password",
                        "--value", value])[0]
        if action == "rotate":
            cmd = [levee_bin(), "-c", CFG, "secret", "rotate", "--name", name]
            print("$ " + " ".join(cmd))
            proc = subprocess.run(cmd, input=(value + "\n").encode(),
                                  capture_output=True, env=env())
            print(proc.stdout.decode("utf-8", "replace").rstrip())
            print(proc.stderr.decode("utf-8", "replace").rstrip())
            print("rc=%d" % proc.returncode)
            return proc.returncode
        return run(["secret", "add", "--name", name, "--type", "ssh_password",
                    "--value", value])[0]

    if mode == "apply":
        # apply <run-id> --engine-enabled [--force]
        # --force = skip the approval check (the engine path treats it as
        # auto-approve); the lab has no real approvers.
        run_id = sys.argv[2]
        args = ["apply", run_id, "--engine-enabled"]
        if "--force" in sys.argv:
            args.append("--force")
        if "--max-concurrency" in sys.argv:
            args += ["--max-concurrency", sys.argv[sys.argv.index("--max-concurrency") + 1]]
        return run(args)[0]

    # Pass anything else straight through to the levee CLI.
    return run(sys.argv[1:])[0]


if __name__ == "__main__":
    sys.exit(main())
