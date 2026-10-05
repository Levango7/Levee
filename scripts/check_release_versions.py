#!/usr/bin/env python3
"""check_release_versions.py — 交付物引用的版本必须与真实发布过的版本同源。

Why: CHANGELOG 可以写下 `## [vX.Y.Z] - 日期` 小节、Helm chart 可以把 appVersion 与
镜像 tag 指到那个版本，而 tag 从未打过——于是 `helm install` 拉到的是上一个版本的镜像
（或一个不存在的 tag），而 chart 与 changelog 对外声称的是另一个版本。这类不一致横跨
三个文件，逐文件 review 看不见：本仓的 v1.19.0 正是停在这个状态（CHANGELOG 小节、
docs/release-notes/v1.19.0.md 与 Chart appVersion 都写了 1.19.0，而 origin 上最新的
tag 是 v1.18.0、values.image.tag 还是 1.18.0，镜像 `:1.19.0` 从未被构建过）。

规则（全部硬失败，不留"跳过即通过"的分支）：
  1. Chart.yaml 的 appVersion 与 values.yaml 的 image.tag 必须是**同一个版本号**——
     渲染出的 Deployment 拉取的镜像，就是 chart 自称的那个版本。两者拼写有意不同
     （appVersion 按 Helm 惯例不带 v，image.tag 按规则 2 必须逐字是 tag 名），所以比的是
     数字而不是字符串。
  2. image.tag 必须**逐字**存在于远端 tag 集合里（不是给它补一个 v 再查）——
     release.yml 推的镜像 tag 就是 `${{ github.ref_name }}`，也就是 git tag 名本身。
     appVersion 同样必须已发布（v 前缀可有可无）。
     为什么强调"逐字"：本脚本第一版按 `v<image.tag>` 合成查询，于是 chart 写
     `image.tag: "1.19.0"` 也能通过（git tag 是 v1.19.0），而 ghcr 上从来只有 `v1.19.0`
     ——`helm install` 默认值拉到的是一个不存在的镜像。实测：helm template 渲染出
     `ghcr.io/levango7/levee:1.19.0`，registry 对该 manifest 返回 404，tags/list 只有
     ["v1.18.0","latest","v1.19.0"]。自 v1.18.0 起即如此。
  3. CHANGELOG 里每个 `## [vX.Y.Z]` 小节：若它比最新 tag 更晚（= 声称了一个还没切版
     的发布），要么已打 tag，要么在小节正文里显式写明"未切版"，否则失败。比最新 tag
     更早的无 tag 小节属历史追溯，不阻断交付，但会被逐条点名（不静默）。
  4. 拿不到任何 v* tag 时直接失败：静默的空 tag 列表会让前三条永远"通过"。

为什么 tag 集合取远端而不是本地：本地克隆可能残留从未推送的 tag。本仓实测本地有
v1.0.0–v1.9.0，而 origin 只有 v1.10.0+ ——用本地 tag 会让"已发布"类断言假绿（本脚本
第一版就在本地放行、在 CI 变红，正是这个差别）。CI 里 checkout 抓来的就是远端集合，
取不到远端时退回本地并打印来源，绝不静默。

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


def note(msg: str) -> None:
    print(f"note: {msg}")


def run(args: list[str], timeout: int = 30) -> tuple[int, str, str]:
    try:
        proc = subprocess.run(args, capture_output=True, text=True,
                              encoding="utf-8", errors="replace", timeout=timeout)
    except (OSError, subprocess.TimeoutExpired) as exc:
        return 1, "", str(exc)
    return proc.returncode, proc.stdout, proc.stderr


def parse_version(tag: str) -> tuple[int, ...]:
    return tuple(int(x) for x in re.findall(r"\d+", tag))


def with_v(version: str) -> str:
    """Normalise a version spelling to a git tag name."""
    return version if version.startswith("v") else f"v{version}"


def tag_set() -> tuple[set[str], str]:
    """Return (version tags, where they came from) — remote preferred."""
    code, out, _ = run(["git", "ls-remote", "--tags", "origin"])
    if code == 0 and out.strip():
        tags = {re.sub(r"\^\{\}$", "", m.group(1))
                for m in re.finditer(r"refs/tags/(v[0-9][^\s]*)$", out, re.MULTILINE)}
        return tags, "origin (git ls-remote --tags)"
    code, out, err = run(["git", "tag", "--list", "v*"])
    if code != 0:
        fail(f"reading tags failed: {err.strip()}")
        return set(), "unreadable"
    note("tag set taken from the LOCAL clone (remote unreachable) — local tags can "
         "include versions that were never pushed")
    return {t.strip() for t in out.splitlines() if t.strip()}, "local clone (fallback)"


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
            m = re.match(r'^\s+tag:\s*"?([^"\s#]+)"?', raw)
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
        if line.startswith("## ["):  # [Unreleased] or another header closes it
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
    # Compared by version number, not spelling: image.tag carries the `v` that
    # appVersion conventionally omits (see rule 2 for why neither may be guessed).
    if app_version and image_tag and parse_version(app_version) != parse_version(image_tag):
        fail(
            f"chart appVersion ({app_version}) != values.image.tag ({image_tag}): "
            f"`helm install` pulls :{image_tag} while the chart advertises {app_version}"
        )

    tags, source = tag_set()
    if not tags:
        fail(
            "no v* tags are visible (remote and local both empty) — the tag set must be "
            "fetched (actions/checkout: fetch-depth: 0 / fetch-tags: true); an empty tag "
            "list would silently pass every version check below"
        )
        return report()

    newest_tag = max(tags, key=parse_version)
    newest_v = parse_version(newest_tag)

    # Rule 2a — the image reference the rendered Deployment pulls must name a tag
    # that exists. Literal, no synthesised prefix: release.yml publishes the image
    # under the git tag name itself (github.ref_name), so the set of image tags on
    # ghcr is exactly the set of git tag names.
    if image_tag and image_tag not in tags:
        fail(
            f"values.image.tag ({image_tag}) is not a released tag name on {source}; "
            f"release.yml publishes the image under the git tag name, so `helm install` "
            f"would pull ghcr.io ...:levee:{image_tag}, an image that was never built "
            f"(newest released tag: {newest_tag})"
        )

    # Rule 2b — the version the chart advertises must be released too.
    if app_version and with_v(app_version) not in tags:
        fail(
            f"chart appVersion ({app_version}) has no tag {with_v(app_version)} on "
            f"{source}, so the version it advertises was never released"
        )

    legacy: list[str] = []
    # Rule 3 — a section newer than the last tag is a release claim.
    for version, body in changelog_sections(log_text):
        if f"v{version}" in tags:
            continue
        if parse_version(version) > newest_v:
            if PENDING_MARKER in body:
                note(f"CHANGELOG v{version} is declared {PENDING_MARKER} (no tag v{version} yet)")
                continue
            fail(
                f"CHANGELOG declares `## [v{version}]` (newer than {newest_tag}) as a "
                f"released section but tag v{version} does not exist; either cut the tag "
                f"or mark the section with '{PENDING_MARKER}'"
            )
        else:
            legacy.append(version)

    if legacy:
        note(
            "CHANGELOG has historical sections with no matching tag (older than "
            f"{newest_tag}, so they do not affect what deploys): "
            + ", ".join("v" + v for v in sorted(legacy, key=parse_version))
            + " — recorded rather than silently accepted"
        )

    return report(f"chart={app_version} tag={image_tag} newest_tag={newest_tag} "
                  f"tag_source={source} tags={len(tags)}")


def report(ok_summary: str = "") -> int:
    if failures:
        for f in failures:
            print(f"RELEASE-VERSION CHECK FAILED: {f}", file=sys.stderr)
        return 1
    print(f"RELEASE VERSION CHECK PASSED: {ok_summary}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
