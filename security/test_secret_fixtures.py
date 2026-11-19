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

    def test_original_corpus_only_allowed_in_history(self):
        old = self.blob.replace(b'FAKE-EXAMPLE-VALUE', b'hunter2', 1)
        old = old.replace(b'FAKE-EXAMPLE-VALUE', b'hunter2', 1)
        old = old.replace(b'FAKE-EXAMPLE-VALUE', b'sk-example0123456789')
        old = old.replace(b'AKIA' + b'FAKEEXAMPLE000000', b'AKIA' + b'ABCDEFGHIJKLMNOP')
        old = old.replace(b'ghp_FAKEEXAMPLE000000000000000', b'ghp_abcdefghijklmnopqrstuvwx')
        line = FIXTURE + ':4:' + old.decode().splitlines()[3] + '\n'
        historical = '1' * 40 + ':' + line
        self.assertEqual(filter_matches([historical], lambda _: old), ([], 1))
        self.assertEqual(filter_matches([line], lambda _: old), ([line], 0))
        self.assertEqual(filter_matches([historical], lambda _: old + b'\n'), ([historical], 0))

    def test_empty_scan(self):
        self.assertEqual(filter_matches([], lambda _: self.blob), ([], 0))


if __name__ == '__main__':
    unittest.main()
