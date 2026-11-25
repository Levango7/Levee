#!/usr/bin/env python3
"""Run the MySQL schema recovery proof on a real SSH target.

Creates a disposable mysql:8.0 + sshd container with no published database
port, uses an ephemeral root keypair and strict known_hosts, and removes the
container and keys afterwards. Empty MySQL root password is confined to this
short-lived, isolated lab container; it is not a deployment example.
"""

from __future__ import annotations

import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time

CONTAINER = "levee-mysql-replay"
IMAGE = "levee-mysql-replay:local"
ROOT = Path(__file__).resolve().parents[1]


def run(args: list[str], **kwargs: object) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        args,
        shell=False,
        check=True,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        **kwargs,
    )


def require_tools() -> None:
    missing = [name for name in ("docker", "go", "ssh-keygen", "ssh-keyscan") if shutil.which(name) is None]
    if missing:
        raise RuntimeError("missing required tools: " + ", ".join(missing))


def write_lab_files(directory: Path, public_key: str) -> None:
    directory.joinpath("authorized_keys").write_text(public_key, encoding="utf-8", newline="\n")
    directory.joinpath("replay-start").write_text(
        "#!/bin/sh\nset -eu\n/usr/local/bin/docker-entrypoint.sh mysqld &\nexec /usr/sbin/sshd -D -e\n",
        encoding="utf-8",
        newline="\n",
    )
    directory.joinpath("replay-start").chmod(0o755)
    directory.joinpath("Dockerfile").write_text(
        """FROM mysql:8.0
RUN microdnf install -y openssh-server \
    && microdnf clean all \
    && mkdir -p /run/sshd /root/.ssh \
    && ssh-keygen -A \
    && sed -i 's/^#\\?PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config \
    && sed -i 's/^#\\?PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config \
    && usermod -U root
COPY authorized_keys /root/.ssh/authorized_keys
COPY replay-start /usr/local/bin/replay-start
RUN chown -R root:root /root/.ssh \
    && chmod 700 /root/.ssh \
    && chmod 600 /root/.ssh/authorized_keys \
    && chmod 755 /usr/local/bin/replay-start
EXPOSE 22
ENTRYPOINT ["/usr/local/bin/replay-start"]
""",
        encoding="utf-8",
        newline="\n",
    )


def published_ssh_port() -> int:
    output = run(["docker", "port", CONTAINER, "22/tcp"]).stdout.strip()
    for line in output.splitlines():
        host, port = line.rsplit(":", 1)
        if host == "127.0.0.1":
            return int(port)
    raise RuntimeError("container has no loopback SSH publication: " + output)


def wait_for_mysql() -> None:
    for _ in range(90):
        probe = subprocess.run(
            ["docker", "exec", CONTAINER, "mysqladmin", "ping", "-h", "127.0.0.1", "-u", "root", "--silent"],
            shell=False,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        if probe.returncode == 0:
            return
        time.sleep(1)
    raise RuntimeError("MySQL did not become ready")


def write_known_hosts(destination: Path, port: int) -> None:
    for _ in range(60):
        scan = subprocess.run(
            ["ssh-keyscan", "-T", "5", "-p", str(port), "127.0.0.1"],
            shell=False,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        if scan.returncode == 0 and "ssh-" in scan.stdout:
            destination.write_text(scan.stdout, encoding="utf-8", newline="\n")
            return
        time.sleep(0.5)
    raise RuntimeError("sshd did not become reachable for strict known_hosts capture")


def main() -> int:
    require_tools()
    existing = subprocess.run(["docker", "inspect", CONTAINER], shell=False, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if existing.returncode == 0:
        raise RuntimeError(f"{CONTAINER} already exists; refusing to replace it")

    created = False
    proof_succeeded = False
    with tempfile.TemporaryDirectory(prefix="levee-mysql-replay-") as raw_dir:
        lab = Path(raw_dir)
        key = lab / "id_ed25519"
        known_hosts = lab / "known_hosts"
        run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(key)])
        write_lab_files(lab, key.with_suffix(".pub").read_text(encoding="utf-8"))
        try:
            run(["docker", "build", "--tag", IMAGE, "."], cwd=lab)
            run([
                "docker", "run", "-d", "--name", CONTAINER,
                "--publish", "127.0.0.1::22",
                "--tmpfs", "/var/lib/mysql",
                "--env", "MYSQL_ALLOW_EMPTY_PASSWORD=1",
                IMAGE,
            ])
            created = True
            wait_for_mysql()
            port = published_ssh_port()
            write_known_hosts(known_hosts, port)
            env = os.environ.copy()
            env.update({
                "GOSUMDB": "sum.golang.org",
                "LEVEE_MYSQL_REPLAY": "1",
                "LEVEE_MYSQL_REPLAY_PORT": str(port),
                "LEVEE_MYSQL_REPLAY_KEY": str(key),
                "LEVEE_MYSQL_REPLAY_KNOWN_HOSTS": str(known_hosts),
            })
            proof = run([
                "go", "test", "-tags=mysqlreplay", "./internal/executor/modules/mysql",
                "-run", "^TestRealMySQLSchemaReplay$", "-count=1", "-v",
            ], cwd=ROOT, env=env)
            sys.stdout.write(proof.stdout)
            proof_succeeded = True
        finally:
            if created and not proof_succeeded:
                logs = subprocess.run(
                    ["docker", "logs", CONTAINER],
                    shell=False,
                    text=True,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.STDOUT,
                )
                sys.stderr.write(logs.stdout)
            if created:
                subprocess.run(
                    ["docker", "rm", "-f", CONTAINER],
                    shell=False,
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                )
    print("MYSQL_SCHEMA_REPLAY launcher: PASS (container and ephemeral keys removed)")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (RuntimeError, subprocess.CalledProcessError) as exc:
        if isinstance(exc, subprocess.CalledProcessError) and exc.stdout:
            print(exc.stdout, file=sys.stderr)
        print(f"MYSQL_SCHEMA_REPLAY launcher: FAIL: {exc}", file=sys.stderr)
        raise SystemExit(1) from exc
