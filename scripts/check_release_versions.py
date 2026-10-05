#!/usr/bin/env python3
"""check_release_versions.py — 交付物引用的版本必须与真实发布过的版本同源。

Why: CHANGELOG 可以写下 `## [vX.Y.Z] - 日期` 小节、Helm chart 可以把 appVersion 与
镜像 tag 指到那个版本，而 tag 从未打过——于是 `helm install` 拉到的是上一个版本的镜像
（或一个不存在的 tag），而 chart 与 changelog 对外声称的是另一个版本。这类不一致横跨
三个文件，逐文件 review 看不见：本仓的 v1.19.0 正是停在这个状态（CHANGELOG 小节、
docs/release-notes/v1.19.0.md 与 Chart appVersion 都写了 1.19.0，而远端最新 tag 是
v1.18.0、values.yaml 的 image.tag 还是 1.18.0，镜像 v1.19.0 从未被构建过）。

规则（全部硬失败，不留"跳过即通过"的分支）：
  1. Chart.yaml 的 appVersion 必须等于 values.yaml 的 image.tag——渲染出的
     Deployment 拉取的镜像，就是 chart 自称的那个版本。
  2. 该版本必须存在对应 git tag（v<version>）——镜像由 release.yml 在
     `push: tags: v*` 时构建，没有 tag 就没有制品。
  3. CHANGELOG 里每个 `## [vX.Y.Z]` 小节要么已有对应 tag，要么在小节正文里显式写明
     "未切版"。写下日期却不切版，等于对部署方宣称一个不存在的制品。
  4. 一个 v* tag 都看不到时直接失败：CI 的 checkout 默认不取 tag，静默的空 tag 列表
     会让上面三条永远"通过"，那比没有门禁更糟。

用法：scripts/check_release_versions.py [chart_dir] [changelog_path]
退出码：0 = 通过，1 = 失败。
"""

from __future__ import annotations

import re
import subprocess
import sys
from pathlib import Path

# 与 Chart.yaml / CHANGELOG.md 里"尚未切版"的中文标注约定同词，改词要同时改三处。
PENDING_MARKER = "未切版"

failures: list[str] = []


def fail(msg: str) -> None:
    failures.append(msg)


def run_git(*args: str) -> str:
    proc = subprocess.run(
        ["git", *args],
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
    )
    if proc.returncode != 0:
        fail(f"git {' '.join(args)} failed: {proc.stderr.strip()}")
        return ""
    return proc.stdout


def first_match(pattern: str, text: str, label: str, source: Path) -> str:
    m = re.search(pattern, text, re.MULTILINE)
    if not m:
        fail(f"{label} not found in {source}")
        return ""
    return m.group(1).strip()


def image_tag_of(values_text: str) -> str:
    """Return values.yaml's image.tag without a yaml dependency.

    Scoping to the `image:` block matters: `postgres.image.tag` (or any future
    sidecar tag) would otherwise be picked up by a loose `tag:` regex.
    """
    in_image = False
    for raw in values_text.splitlines():
        if re.match(r"^image:\s*$", raw):
            in_image = True
            continue
        if in_image:
            if raw and not raw.startswith((" ", "\t", "#")):
                in_image = False  # left the block
                continue
            m = re.match(r"^\s+tag:\s*\"?([^\"\s#]+)\"?", raw)
            if m:
                return m.group(1)
    return ""


def changelog_sections(text: str) -> list[tuple[str, str]]:
    """Return (version, section_body) for every `## [vX.Y.Z]` header."""
    out: list[tuple[str, str]] = []
    current: str | None = None
    buf: list[str] = []
    for line in text.splitlines():
        m = re.match(r"^## \[v([0-9][^\]]*)\]", line)
        if m:
            if current is not None:
                out.append((current, "\n".join(buf)))
            current, buf = m.group(1), [line]
            continue
        if current is None:
            continue
        if line.startswith("## ["):  # [Unreleased] or any other header closes it
            out.append((current, "\n".join(buf)))
            current, buf = None, []
            continue
        buf.append(line)
    if current is not None:
        out.append((current, "\n".join(buf)))
    return out


def main() -> int:
    chart_dir = Path(sys.argv[1] if len(sys.argv) > 1 else "deploy/helm/levee")
    changelog = Path(sys.argv[2] if len(sys.argv) > 2 else "CHANGELOG.md")

    for p in (chart_dir / "Chart.yaml", chart_dir / "values.yaml", changelog):
        if not p.is_file():
            print(f"RELEASE-VERSION CHECK FAILED: missing {p}", file=sys.stderr)
            return 1

    chart_text = (chart_dir / "Chart.yaml").read_text(encoding="utf-8")
    values_text = (chart_dir / "values.yaml").read_text(encoding="utf-8")
    log_text = changelog.read_text(encoding="utf-8")

    app_version = first_match(r'^appVersion:\s*"?([^"\n]+?)"?\s*$', chart_text,
                              "appVersion", chart_dir / "Chart.yaml")
    image_tag = image_tag_of(values_text)
    if not image_tag:
        fail("image.tag not found in values.yaml")

    # Rule 1 — the chart must not claim one version while deploying another.
    if app_version and image_tag and app_version != image_tag:
        fail(
            f"chart appVersion ({app_version}) != values.image.tag ({image_tag}): "
            f"`helm install` pulls :{image_tag} while the chart advertises {app_version}"
        )

    # Rule 4 — prove the tag list is real before trusting it (see docstring).
    tag_out = run_git("tag", "--list", "v*")
    tags = {t.strip() for t in tag_out.splitlines() if t.strip()}
    if not tags:
        fail(
            "no v* tags are visible in this checkout — the tag set must be fetched "
            "(actions/checkout: fetch-tags: true); an empty tag list would silently "
            "pass every version check below"
        )

    referenced = [v for v in (image_tag, app_version) if v]
    # Rule 2 — the artifact the deployment actually pulls must have been released.
    for version in referenced:
        if tags and f"v{version}" not in tags:
            fail(
                f"values.image.tag / appVersion reference {version} but tag v{version} "
                f"does not exist, so ghcr.io ...:levee:{version} was never built "
                f"(release.yml runs on `push: tags: v*`)"
            )

    # Rule 3 — a dated CHANGELOG section is a release claim.
    for version, body in changelog_sections(log_text):
        if f"v{version}" in tags:
            continue
        if PENDING_MARKER in body:
            print(f"note: CHANGELOG v{version} is declared {PENDING_MARKER} (not tagged yet)")
            continue
        fail(
            f"CHANGELOG declares `## [v{version}]` as a released section but tag v{version} "
            f"does not exist; either cut the tag or mark the section with '{PENDING_MARKER}'"
        )

    if failures:
        for f in failures:
            print(f"RELEASE-VERSION CHECK FAILED: {f}", file=sys.stderr)
        return 1

    newest = sorted(tags, key=lambda t: [int(x) for x in re.findall(r"\d+", t)])
    print(f"RELEASE VERSION CHECK PASSED: chart={app_version} tag={image_tag} "
          f"released={len(tags)} tags (newest {newest[-1]})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
