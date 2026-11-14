"""Content-boundary tests use explicit fake resources, never a purported release package."""
import hashlib
import importlib.util
import json
import pathlib
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('pack', pathlib.Path(__file__).with_name('package-offline.py'))
pack = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pack)


class PackageBoundary(unittest.TestCase):
    def test_allowlist_relocation_and_corruption(self):
        with tempfile.TemporaryDirectory() as temp:
            root = pathlib.Path(temp)
            source, target = root / 'prepared', root / 'package'
            source.mkdir()
            manifest = {'version': 1, 'sourceCommit': 'aa21210e1f8214c9eb73301132eb5c3db351aa24', 'profile': 'release'}
            for key, relative in pack.FILES.items():
                asset = source / relative
                asset.parent.mkdir(exist_ok=True)
                asset.write_bytes(b'explicit unit fixture')
                manifest[key] = {'Path': str(asset), 'SHA256': hashlib.sha256(asset.read_bytes()).hexdigest()}
            (source / 'assets.json').write_text(json.dumps(manifest))
            (source / 'users.json').write_text('must not ship')
            (source / 'private-data').mkdir()
            target.mkdir()
            pack.copy_assets(source, target)
            moved = root / 'moved'
            target.rename(moved)
            copied = json.loads((moved / 'assets.json').read_text())
            self.assertEqual({p.relative_to(moved).as_posix() for p in moved.rglob('*') if p.is_file()}, set(pack.FILES.values()) | {'assets.json'})
            for key, relative in pack.FILES.items():
                self.assertEqual(copied[key]['Path'], relative)
                self.assertEqual(hashlib.sha256((moved / relative).read_bytes()).hexdigest(), copied[key]['SHA256'])
            (source / 'bin/ora-node').write_text('corrupted')
            with self.assertRaisesRegex(ValueError, 'digest mismatch'):
                pack.copy_assets(source, target)


if __name__ == '__main__':
    unittest.main()
