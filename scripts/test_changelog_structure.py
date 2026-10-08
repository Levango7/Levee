"""test_changelog_structure.py — CHANGELOG 的骨架必须可被机器读。

为什么单独一条守卫：`## [Unreleased]` 出现两次时，所有既有门禁都是绿的。
`check_release_versions.py` 规则③按小节标题判断"比最新 tag 更晚的内容是否标注未切版"，
两个同名标题在它看来不是错误；Keep-a-Changelog 的读者只会看到第二个标题下面那节，
第一个标题下已有的几节在目录里凭空消失。

成因是一次锚点选择：往 `## [v1.20.0]` 之前插一整节时连带写了一个 `## [Unreleased]`，
三方合并两侧改的不是同一行 ⇒ 不冲突，于是静默落到 master。修法（本批同时做掉）是
只删重复标题那 3 行，并把这条结构检查钉进 release-gate 的逐套件点名表。
"""

import os
import re
import unittest

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CHANGELOG = os.path.join(REPO, 'CHANGELOG.md')

UNRELEASED_HEAD = re.compile(r'^## \[Unreleased\]$', re.M)
VERSION_HEAD = re.compile(r'^## \[(v[0-9][^\]]*)\]', re.M)
ANY_H2 = re.compile(r'^## ')

GOOD = (
    '# Changelog\n'
    '\n'
    '## [Unreleased]\n'
    '\n'
    '### 修复（甲）\n'
    '\n'
    '- a\n'
    '\n'
    '## [v1.0.0] - 2026-01-01\n'
    '\n'
    '### 新增（乙）\n'
    '\n'
    '- b\n'
)

DAMAGED = GOOD.replace('## [v1.0.0] - 2026-01-01\n', '## [Unreleased]\n\n### 修复（丙）\n\n- c\n\n## [v1.0.0] - 2026-01-01\n')


def unreleased_lines(text):
    """1-based line numbers of bare `## [Unreleased]` headings."""
    return [i + 1 for i, line in enumerate(text.split('\n')) if UNRELEASED_HEAD.match(line)]


class ChangelogStructureTest(unittest.TestCase):
    """Assertions about the repository's own CHANGELOG."""

    def setUp(self):
        with open(CHANGELOG, encoding='utf-8') as handle:
            self.text = handle.read()

    def test_exactly_one_unreleased_heading(self):
        lines = unreleased_lines(self.text)
        self.assertEqual(
            [5], lines,
            'CHANGELOG.md must carry exactly one `## [Unreleased]` heading (on line 5), found %s. '
            'A second one does not fail any other gate -- it just silently hides the sections under '
            'the first from anyone reading the file as a table of contents.' % (lines,))

    def test_unreleased_is_the_first_section(self):
        headings = [i for i, l in enumerate(self.text.split('\n')) if ANY_H2.match(l)]
        self.assertTrue(headings, 'CHANGELOG.md has no `## ` sections at all')
        first = headings[0]
        self.assertEqual(
            '## [Unreleased]', self.text.split('\n')[first],
            'the first `## ` heading must be [Unreleased]; found %r' % self.text.split('\n')[first])

    def test_unreleased_block_is_grouped_into_subsections(self):
        """Content under [Unreleased] must sit in `### ` subsections, not loose bullets.

        Only asserted when the block is non-empty: an empty [Unreleased] is legal
        right after a release, and asserting on an empty block would pass vacuously.
        """
        versions = list(VERSION_HEAD.finditer(self.text))
        self.assertTrue(versions, 'no released version sections -- unexpected file shape')
        block = self.text[UNRELEASED_HEAD.search(self.text).end():versions[0].start()]
        if block.strip():
            self.assertIn(
                '\n### ', block,
                'everything between [Unreleased] and the first version heading must be grouped '
                'under `### ` subsections; found loose content')

    def test_no_duplicate_version_headings(self):
        seen, dupes = {}, []
        for m in VERSION_HEAD.finditer(self.text):
            tag = m.group(1)
            line = self.text[:m.start()].count('\n') + 1
            if tag in seen:
                dupes.append('%s at lines %d and %d' % (tag, seen[tag], line))
            else:
                seen[tag] = line
        self.assertEqual([], dupes, 'each version may appear once as a `## [tag]` heading')


class DetectorIsNotVacuousTest(unittest.TestCase):
    """The checks above must be able to *see* the damage they exist for.

    A guard that only ever inspects a currently-clean file cannot tell you whether
    it would have caught anything -- so the same expressions run over synthetic
    good/bad fixtures here.
    """

    def test_good_fixture_passes(self):
        self.assertEqual([3], unreleased_lines(GOOD))
        self.assertEqual(1, len(unreleased_lines(GOOD)))

    def test_duplicate_is_detected(self):
        found = unreleased_lines(DAMAGED)
        self.assertEqual(2, len(found), 'the detector must see two headings when two exist: %s' % found)
        self.assertNotEqual([5], found)

    def test_duplicate_detection_is_not_position_dependent(self):
        # The damage on master had the extra heading far down the file (line 78),
        # not adjacent to the first one -- a check that only looks at the head of
        # the file would have missed it.
        late = GOOD + '\n' + 'x\n' * 200 + '\n## [Unreleased]\n'
        self.assertEqual(2, len(unreleased_lines(late)))


if __name__ == '__main__':
    unittest.main()
