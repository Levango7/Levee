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
UNRELEASED_SUBHEAD = re.compile(r'^### (.+)$', re.M)
CODE_SPAN = re.compile(r'`[^`]*`')


def _strip_code_spans(text):
    """Drop backtick-quoted spans so a heading *quoting* a marker is not a marker."""
    return CODE_SPAN.sub('', text)


def unreleased_subsection_titles(text):
    """(1-based line, title) for each `### ` subsection heading inside [Unreleased].

    The block boundary is the one the grouping check already uses: from the
    `## [Unreleased]` heading to the first released-version heading. Returns []
    for shapes that check does not assert on (no version sections yet).
    """
    head = UNRELEASED_HEAD.search(text)
    version = VERSION_HEAD.search(text)
    if head is None or version is None or version.start() <= head.end():
        return []
    block = text[head.end():version.start()]
    base = head.end()
    return [
        (text[:base + m.start()].count('\n') + 1, m.group(1))
        for m in UNRELEASED_SUBHEAD.finditer(block)
    ]

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

# Fixtures for the [Unreleased] subsection checks. SUBGOOD is the shape the
# real file carries (two distinct `### ` sections under one [Unreleased]);
# SUBDUP re-uses the first title for the second section, which is what the
# 2026-10-31 same-anchor surgery actually produced; SUBGLUED puts two headings
# on one line, which the same round also produced.
SUBGOOD = (
    '# Changelog\n'
    '\n'
    '## [Unreleased]\n'
    '\n'
    '### 修复（甲）\n'
    '\n'
    '- a\n'
    '\n'
    '### 新增（乙）\n'
    '\n'
    '- b\n'
    '\n'
    '## [v1.0.0] - 2026-01-01\n'
)

SUBDUP = SUBGOOD.replace('### 新增（乙）', '### 修复（甲）')

SUBGLUED = (
    '# Changelog\n'
    '\n'
    '## [Unreleased]\n'
    '\n'
    '### 修复（甲）### 新增（乙）\n'
    '\n'
    '- a\n'
    '- b\n'
    '\n'
    '## [v1.0.0] - 2026-01-01\n'
)


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

    def test_no_duplicate_unreleased_subsection_titles(self):
        """Every [Unreleased] `### ` subsection title may appear once.

        Incident behind this one: on 2026-10-31 two agents appended sections to
        the same `## [Unreleased]` anchor in one window, and the surgery
        re-inserted a section that already existed. Nothing in CI saw it -- the
        only duplicate check that existed covered `## [tag]` headings, and the
        duplication survived a green release gate. The damage is invisible to
        the file's normal readers (a duplicated title still reads as a normal
        entry) and expensive for the next agent, who re-derives "what is already
        written down" from these titles. Five agents are inserting into this
        block today; the surgical surface only grows.

        Exact match by design: a near-duplicate title (different wording for the
        same change) is a judgment call for the author, not a machine verdict --
        the gate reports the repetition, a human decides it is wrong.
        """
        seen, dupes = {}, []
        for line, title in unreleased_subsection_titles(self.text):
            if title in seen:
                dupes.append('%r at lines %d and %d' % (title, seen[title], line))
            else:
                seen[title] = line
        self.assertEqual([], dupes, 'each [Unreleased] subsection title may appear once')

    def test_subsection_headings_are_not_glued(self):
        """A `### ` heading line must not carry a second heading marker.

        Same surgery round, same file: an insertion glued two heading lines
        together. A glued pair parses as ONE section whose title is `甲### 乙`
        -- unique, well-formed, and wrong: the second entry became a title word
        instead of an entry, and no heading-*count* check can see the loss.
        Backtick-quoted markers (`### foo` written as an example) are stripped
        first: quoting a heading inside a title is legitimate.
        """
        glued = []
        for line, title in unreleased_subsection_titles(self.text):
            if '### ' in _strip_code_spans(title) or '## ' in _strip_code_spans(title):
                glued.append('line %d: %r' % (line, title))
        self.assertEqual([], glued, 'each heading must stand alone on its line')


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

    def test_good_subsection_fixture_passes(self):
        titles = unreleased_subsection_titles(SUBGOOD)
        self.assertEqual([(5, '修复（甲）'), (9, '新增（乙）')], titles)
        for _, title in titles:
            self.assertNotIn('### ', _strip_code_spans(title))
            self.assertNotIn('## ', _strip_code_spans(title))

    def test_duplicate_subsection_is_detected(self):
        names = [t for _, t in unreleased_subsection_titles(SUBDUP)]
        self.assertEqual(
            2, names.count('修复（甲）'),
            'the detector must see two subsections sharing one title: %s' % names)

    def test_duplicate_subsection_detection_is_not_position_dependent(self):
        # The 2026-10-31 duplication was NOT adjacent to its twin -- it sat deep
        # in the block, which is why a "newest section only" reading missed it.
        # The filler must land *inside* the [Unreleased] block (before the first
        # version heading): appended after it, the extra section belongs to the
        # released version's block and is legitimately out of scope.
        late = SUBGOOD.replace(
            '## [v1.0.0] - 2026-01-01\n',
            ('- filler\n' * 200) + '\n### 修复（甲）\n\n## [v1.0.0] - 2026-01-01\n')
        names = [t for _, t in unreleased_subsection_titles(late)]
        self.assertEqual(2, names.count('修复（甲）'), 'two same-titled subsections, far apart: %s' % names)

    def test_glued_headings_are_detected(self):
        titles = unreleased_subsection_titles(SUBGLUED)
        self.assertEqual(1, len(titles), 'a glued pair must parse as exactly one heading line: %s' % titles)
        self.assertIn('### ', _strip_code_spans(titles[0][1]),
                      'the surviving title must visibly contain the swallowed heading')

    def test_quoted_heading_markers_in_titles_are_not_glued(self):
        # A title that *quotes* a heading as an example (`### 甲`) is legitimate
        # prose; only unstripped markers count.
        quoted = SUBGOOD.replace('### 修复（甲）', '### 修复（旧文写作 `### 甲` 的那节）')
        for _, title in unreleased_subsection_titles(quoted):
            self.assertNotIn('### ', _strip_code_spans(title))


if __name__ == '__main__':
    unittest.main()
