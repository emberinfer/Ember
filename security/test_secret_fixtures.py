"""Exact synthetic allowlisting must never hide changed or unrelated material."""
import unittest
from pathlib import Path
from filter_secret_fixtures import FIXTURE, filter_matches


class FixtureFilterTests(unittest.TestCase):
    def setUp(self):
        self.blob = Path(FIXTURE).read_bytes()
        self.hit = FIXTURE + ':4:' + self.blob.decode().splitlines()[3] + '\n'

    def test_reviewed_worktree_and_history_only(self):
        for prefix in ('', '1'*40 + ':'):
            self.assertEqual(filter_matches([prefix+self.hit], lambda _: self.blob), ([], 1))

    def test_any_changed_fixture_fails_closed(self):
        self.assertEqual(filter_matches([self.hit], lambda _: self.blob+b'\n'), ([self.hit], 0))

    def test_another_path_or_line_never_allowed(self):
        for line in ('other/'+self.hit, self.hit.replace(':4:', ':5:'), self.hit+'extra'):
            self.assertEqual(filter_matches([line], lambda _: self.blob), ([line], 0))

    def test_empty_scan(self):
        self.assertEqual(filter_matches([], lambda _: self.blob), ([], 0))


if __name__ == '__main__':
    unittest.main()
