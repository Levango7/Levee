#!/usr/bin/env python3
"""test_xinchuang_baremetal_drill.py — 裸机取证脚本的结构守卫。

Why: `scripts/xinchuang-baremetal-drill.sh` 的价值在于「一次命令拿到全部证据」，
而它有三个**只在真跑时才暴露**的失败点——三者都让脚本看起来成功、实际什么都没做：

1. **漏了 plan 步骤**。REST 的治理链是 plan → approve → apply → rollback；少了它，
   approve 回 `has no persisted plan to approve`、apply 回 `is in draft state`、
   rollback 回 `cannot ... current status draft`。三个错误各自指向别的方向，
   合起来才是同一个缺失。
2. **就绪探针没带 token**。serve 开了 `--auth-token` 后 `/system/version` 要鉴权；
   不带 token 的探针把「已经起来」读成「没起来」，于是整个演练在门口就退出了。
3. **凭据库没有主口令**。`secret add` 直接报
   `LEVEE_MASTER_PASSWORD environment variable is not set`——演练在第一步就断。

这三个都是 2026-11-16 实跑时踩到的。写在这里而不是靠README记住，是因为：
改这个脚本的人不会重跑一遍（要一台真机），而任何一条悄悄退化都会让下一次
取证变成「跑完了，但证据是空的」。

本文件读**入库文本**并断言那些承重性质。跑法：
    python -m unittest discover -s scripts -p 'test_*.py'
"""

from __future__ import annotations

import pathlib
import re
import unittest

REPO = pathlib.Path(__file__).resolve().parent.parent
SCRIPT = REPO / "scripts" / "xinchuang-baremetal-drill.sh"
DOC = REPO / "docs" / "xinchuang-baremetal-drill.md"


class BaremetalDrillScriptTest(unittest.TestCase):
    def setUp(self) -> None:
        self.text = SCRIPT.read_text(encoding="utf-8")

    def test_the_script_exists(self) -> None:
        self.assertTrue(SCRIPT.is_file(), "xinchuang-baremetal-drill.sh is missing")

    def test_the_governance_chain_is_in_order(self) -> None:
        # plan must come before approve, approve before apply, apply before
        # rollback. Order is the whole point: each step gates the next.
        order = ["/plan", "/approve", "/apply", "/rollback"]
        positions = []
        for step in order:
            hit = self.text.find(f'"$BASE/changes/$RUN_ID{step}"')
            self.assertNotEqual(hit, -1, f"the drill never calls {step}")
            positions.append(hit)
        self.assertEqual(positions, sorted(positions),
                         "the REST chain must be plan → approve → apply → rollback, in that order")

    def test_plan_is_called_with_a_json_body(self) -> None:
        # An empty body makes handleChangePlan answer 400 "invalid body: EOF",
        # and a body that names the target keeps the plan honest about WHERE.
        call = re.search(r'curl -sX POST "\$BASE/changes/\$RUN_ID/plan".{0,200}?\}', self.text, re.S)
        self.assertIsNotNone(call, "cannot find the plan call")
        body = call.group(0)
        self.assertIn("targetHosts", body, "plan must name the target hosts in its body")
        self.assertIn("${TARGET_ADDR}", body, "plan must name THIS drill's target")

    def test_the_readiness_probe_carries_the_token(self) -> None:
        # Without the bearer token the probe turns "already up" into "never
        # came up", and the drill exits at the door with a misleading message.
        probe = re.search(r'curl -sf "http://\$\{HTTP_ADDR\}/api/v1/system/version"[^\n]*',
                          self.text)
        self.assertIsNotNone(probe, "the readiness probe is gone")
        self.assertIn("Authorization: Bearer", probe.group(0),
                      "the readiness probe must authenticate — serve was started with --auth-token")

    def test_the_master_password_is_resolved_and_reused(self) -> None:
        for needle in ("LEVEE_MASTER_PASSWORD", "export LEVEE_MASTER_PASSWORD"):
            self.assertIn(needle, self.text,
                          f"the credential store needs {needle}; without it `secret add` fails outright")
        self.assertIn("${LEVEE_MASTER_PASSWORD:-}", self.text,
                      "an environment-provided master password must be honoured, not overwritten")

    def test_only_the_two_accepted_auth_methods_reach_the_config(self) -> None:
        # internal/config accepts exactly `key` or `password`; anything else is
        # a config validation error deep inside serve, far from the cause.
        self.assertIn('must be key or password', self.text)
        self.assertIn(r'[ "$SSH_AUTH" = "key" ] || [ "$SSH_AUTH" = "password" ]', self.text)

    def test_the_key_path_is_the_documented_one(self) -> None:
        # internal/config requires channel.ssh.key_path when auth_method=key.
        self.assertIn('KEY_PATH="$OUT/drill-key"', self.text,
                      "the drill generates a key path under its outdir")
        self.assertRegex(self.text, r"key_path: \$\{KEY_PATH\}",
                         "the generated config must set channel.ssh.key_path")

    def test_the_credential_is_the_structured_ssh_ref(self) -> None:
        # channel.CredentialRef's JSON shape, as consumed by serveCredentialResolver.
        # The script writes the JSON with escaped quotes (bash string
        # interpolation), so the literal text carries backslashes.
        self.assertIn('\\"username\\":\\"${TARGET_USER}\\"', self.text,
                      "the credential must carry the structured username field")
        self.assertIn('--type ssh_key', self.text)
        self.assertIn('credential_ref: ssh-drill', self.text,
                      "the inventory must reference the credential by name")

    def test_known_hosts_is_prepared_because_strict_check_defaults_on(self) -> None:
        self.assertIn("ssh-keyscan", self.text)
        self.assertIn("known_hosts", self.text)

    def test_the_file_copy_source_stays_relative_and_inside_the_workdir(self) -> None:
        # internal/executor/modules/file rejects absolute paths and paths that
        # escape the process working directory; serve is started from $OUT/work
        # for exactly that reason.
        self.assertIn('src: "src/marker.txt"', self.text)
        self.assertIn('cd "$OUT/work"', self.text,
                      "serve must run from the directory that holds the copy source")

    def test_audit_verify_is_the_last_word(self) -> None:
        self.assertIn("/audit/verify?changeId=$RUN_ID", self.text)

    def test_a_failure_keeps_the_artifacts(self) -> None:
        # Every step writes its raw output BEFORE it can fail, so a red run is
        # a diagnosable run.
        for step in ("01-doctor.txt", "03-metadata.txt", "07-audit-verify.json"):
            self.assertIn(step, self.text)
        self.assertIn("the drill stops here", self.text)

    def test_the_document_points_at_the_script(self) -> None:
        self.assertTrue(DOC.is_file(), "xinchuang-baremetal-drill.md is missing")
        body = DOC.read_text(encoding="utf-8")
        self.assertIn("xinchuang-baremetal-drill.sh", body,
                      "the checklist doc must name the script that now drives it")


if __name__ == "__main__":
    unittest.main()