import hashlib
import tempfile
import unittest
from unittest.mock import patch
from ember_pb2 import Artifact, SourceLocator
from fetcher import fetch_sources

class SourceTests(unittest.TestCase):
    def test_verified_mirror_fallback(self):
        a=Artifact(name='model.safetensors',sha256=hashlib.sha256(b'ok').digest(),size=2)
        sources=[SourceLocator(repository='https://bad.test/repo',revision='1'*40),SourceLocator(repository='https://mirror.test/repo',revision='2'*40)]
        import io
        def download(url,timeout):
            return io.BytesIO(b'bad' if 'bad.test' in url else b'ok')
        with tempfile.TemporaryDirectory() as root, patch('fetcher.urlopen',side_effect=download):
            self.assertEqual(fetch_sources(a,root,sources).read_bytes(),b'ok')
    def test_floating_and_corrupt_fail_closed(self):
        a=Artifact(name='x',sha256=b'x'*32,size=1)
        with tempfile.TemporaryDirectory() as root:
            with self.assertRaisesRegex(ValueError,'unpinned'):
                fetch_sources(a,root,[SourceLocator(repository='https://example.org/repo',revision='main')])

    def test_modelscope_pinned_url(self):
        import io
        a=Artifact(name='model.safetensors',sha256=hashlib.sha256(b'ok').digest(),size=2)
        source=SourceLocator(repository='https://modelscope.cn/models/example/model',revision='3'*40)
        with tempfile.TemporaryDirectory() as root, patch('fetcher.urlopen',return_value=io.BytesIO(b'ok')) as download:
            self.assertEqual(fetch_sources(a,root,[source]).read_bytes(),b'ok')
            url=download.call_args.args[0]
            self.assertIn('/api/v1/models/example/model/repo?',url)
            self.assertIn('Revision='+'3'*40,url)
