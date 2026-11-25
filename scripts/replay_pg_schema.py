#!/usr/bin/env python3
"""Run the opt-in PostgreSQL schema recovery proof on a real SSH target.

The launcher creates a disposable SSH+PostgreSQL container with no published
database port, generates an ephemeral SSH keypair, and removes both the
container and the key material afterwards. No shell command strings are used.
"""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time

CONTAINER = "levee-pg-replay"
IMAGE = "levee-pg-replay:local"
ROOT = Path(__file__).resolve().parents[1]


def run(args: list[str], **kwargs: object) -> subprocess.CompletedProcess[str]:
    """Execute one argument vector without a shell."""
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
        "#!/bin/sh\nset -eu\n/usr/local/bin/docker-entrypoint.sh postgres &\nexec /usr/sbin/sshd -D -e\n",
        encoding="utf-8",
        newline="\n",
    )
    directory.joinpath("replay-start").chmod(0o755)
    directory.joinpath("Dockerfile").write_text(
        """FROM postgres:16-alpine
RUN apk add --no-cache openssh \
    && mkdir -p /run/sshd /var/lib/postgresql/.ssh \
    && ssh-keygen -A \
    && sed -i 's/^#\\?PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config \
    && sed -i 's/^#\\?PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config \
    && passwd -u postgres
COPY authorized_keys /var/lib/postgresql/.ssh/authorized_keys
COPY authorized_keys /root/.ssh/authorized_keys
COPY replay-start /usr/local/bin/replay-start
RUN chown -R root:root /root/.ssh \
    && chmod 700 /root/.ssh \
    && chmod 600 /root/.ssh/authorized_keys \
    && chown -R postgres:postgres /var/lib/postgresql/.ssh \
    && chmod 750 /var/lib/postgresql \
    && chmod 700 /var/lib/postgresql/.ssh \
    && chmod 600 /var/lib/postgresql/.ssh/authorized_keys \
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


def wait_for_postgres() -> None:
    for _ in range(60):
        probe = subprocess.run(
            ["docker", "exec", "--user", "postgres", CONTAINER, "pg_isready", "-h", "localhost"],
            shell=False,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        if probe.returncode == 0:
            return
        time.sleep(0.5)
    raise RuntimeError("PostgreSQL did not become ready")


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
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--suite", choices=("pg-schema", "pkg-alpine", "capabilities-alpine"), default="pg-schema")
    args = parser.parse_args()
    require_tools()
    existing = subprocess.run(["docker", "inspect", CONTAINER], shell=False, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if existing.returncode == 0:
        raise RuntimeError(f"{CONTAINER} already exists; refusing to replace it")

    created = False
    proof_succeeded = False
    with tempfile.TemporaryDirectory(prefix="levee-pg-replay-") as raw_dir:
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
                "--tmpfs", "/var/lib/postgresql/data",
                "--env", "POSTGRES_HOST_AUTH_METHOD=trust",
                IMAGE,
            ])
            created = True
            wait_for_postgres()
            port = published_ssh_port()
            write_known_hosts(known_hosts, port)
            env = os.environ.copy()
            env.update({
                "GOSUMDB": "sum.golang.org",
                "LEVEE_PG_REPLAY": "1",
                "LEVEE_PG_REPLAY_PORT": str(port),
                "LEVEE_PG_REPLAY_KEY": str(key),
                "LEVEE_PG_REPLAY_KNOWN_HOSTS": str(known_hosts),
            })
            if args.suite == "pkg-alpine":
                command = [
                    "go", "test", "-tags=pkgreplay", "./internal/executor/modules/pkg",
                    "-run", "^TestAlpinePackageManagerReplay$", "-count=1", "-v",
                ]
            elif args.suite == "capabilities-alpine":
                command = [
                    "go", "test", "-tags=capabilityreplay", "./internal/targetcap",
                    "-run", "^TestAlpineCapabilityProbe$", "-count=1", "-v",
                ]
            else:
                command = [
                    "go", "test", "-tags=pgreplay", "./internal/executor/modules/pg",
                    "-run", "^TestRealPostgresSchemaReplay$", "-count=1", "-v",
                ]
            proof = run(command, cwd=ROOT, env=env)
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
    print(f"REAL_PG_REPLAY launcher: PASS suite={args.suite} (container and ephemeral keys removed)")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (RuntimeError, subprocess.CalledProcessError) as exc:
        if isinstance(exc, subprocess.CalledProcessError) and exc.stdout:
            print(exc.stdout, file=sys.stderr)
        print(f"REAL_PG_REPLAY launcher: FAIL: {exc}", file=sys.stderr)
        raise SystemExit(1) from exc
