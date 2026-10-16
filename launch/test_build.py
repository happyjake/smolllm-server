import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('artifact_build', Path(__file__).with_name('build.py'))
build = importlib.util.module_from_spec(spec)
spec.loader.exec_module(build)


def repo(path):
    path.mkdir()
    subprocess.run(['git', 'init', '-q', str(path)], check=True)
    (path / 'tracked.txt').write_text('tracked')
    (path / '.gitignore').write_text('ignored.go\n')
    subprocess.run(['git', '-C', str(path), 'add', '.'], check=True)
    subprocess.run(['git', '-C', str(path), '-c', 'user.name=Test', '-c', 'user.email=test@example.invalid',
                    'commit', '-qm', 'fixture'], check=True)
    return path


class BuildTests(unittest.TestCase):
    def test_persisted_goenv_overlay_requires_explicit_disable(self):
        import os
        with tempfile.TemporaryDirectory() as td:
            goenv = Path(td) / 'goenv'
            goenv.write_text('GOFLAGS=-overlay=/nonexistent/untracked-overlay.json\n')
            env = dict(os.environ, GOENV=str(goenv), GOFLAGS='', GOTOOLCHAIN='local', GOWORK='off')
            self.assertIn('-overlay=', subprocess.check_output(['go', 'env', 'GOFLAGS'], env=env, text=True))
            env['GOENV'] = 'off'
            self.assertEqual(subprocess.check_output(['go', 'env', 'GOFLAGS'], env=env, text=True).strip(), '')

    def test_dirty_or_nonroot_inputs_rejected(self):
        with tempfile.TemporaryDirectory() as td:
            r = repo(Path(td) / 'repo')
            self.assertEqual(len(build.revision(r)), 40)
            (r / 'untracked.txt').write_text('dirty')
            with self.assertRaises(ValueError):
                build.revision(r)
            with self.assertRaises(ValueError):
                build.revision(r / '.git')

    def test_archives_exclude_ignored_inputs(self):
        with tempfile.TemporaryDirectory() as td:
            temp = Path(td)
            r = repo(temp / 'repo')
            (r / 'ignored.go').write_text('must not compile')
            target = temp / 'source'
            build.archive(r, build.revision(r), target, temp)
            self.assertTrue((target / 'tracked.txt').exists())
            self.assertFalse((target / 'ignored.go').exists())

    def test_git_replace_cannot_change_archived_revision_contents(self):
        with tempfile.TemporaryDirectory() as td:
            temp = Path(td)
            r = repo(temp / 'repo')
            original = build.revision(r)
            (r / 'tracked.txt').write_text('replacement')
            subprocess.run(['git', '-C', str(r), 'add', '.'], check=True)
            subprocess.run(['git', '-C', str(r), '-c', 'user.name=Test', '-c', 'user.email=test@example.invalid', 'commit', '-qm', 'other'], check=True)
            replacement = build.revision(r)
            subprocess.run(['git', '-C', str(r), 'replace', original, replacement], check=True)
            target = temp / 'archive'
            build.archive(r, original, target, temp)
            self.assertEqual((target / 'tracked.txt').read_text(), 'tracked')

    def test_module_selection_rejects_vendor_missing_or_external_library(self):
        with tempfile.TemporaryDirectory() as td:
            temp = Path(td)
            (temp / 'smolllm-server').mkdir()
            (temp / 'smolllm-go').mkdir()
            module = {'Path': 'github.com/rocry/smolllm-go', 'Replace': {'Dir': str(temp / 'smolllm-go')}}
            with patch.object(build.subprocess, 'check_output', return_value=json.dumps(module)):
                build.validate_modules(temp, {})
            for raw in ('{}', json.dumps(dict(module, Replace={'Dir': '/external/library'})),
                        json.dumps(module) + json.dumps({'Path': 'other', 'Replace': {'Dir': '/external/other'}})):
                with patch.object(build.subprocess, 'check_output', return_value=raw), self.assertRaises(ValueError):
                    build.validate_modules(temp, {})
            (temp / 'smolllm-server/vendor').mkdir()
            with self.assertRaises(ValueError):
                build.validate_modules(temp, {})

    def test_artifact_receipt_has_both_revisions_and_overlays_disabled(self):
        with tempfile.TemporaryDirectory() as td:
            temp = Path(td)
            server, library = repo(temp / 'server'), repo(temp / 'library')
            subprocess_run = subprocess.run
            def run(args, **kwargs):
                if args[0] == 'go':
                    self.assertEqual(kwargs['env']['GOWORK'], 'off')
                    self.assertEqual(kwargs['env']['GOFLAGS'], '-mod=readonly')
                    self.assertEqual(kwargs['env']['GOENV'], 'off')
                    self.assertEqual(kwargs['env']['GOTOOLCHAIN'], 'local')
                    self.assertIn('serverRevision=' + build.revision(server), args[4])
                    self.assertIn('libraryRevision=' + build.revision(library), args[4])
                    Path(args[6]).write_bytes(b'test-native-binary')
                else:
                    return subprocess_run(args, **kwargs)
            original = subprocess.check_output
            def output(args, **kwargs):
                if args == ['go', 'version']:
                    return 'go version go1.test darwin/arm64\n'
                if args == ['go', 'list', '-m', '-json', 'all']:
                    return json.dumps({'Path': 'github.com/rocry/smolllm-go', 'Replace': {'Dir': str(kwargs['cwd'].parent / 'smolllm-go')}})
                return original(args, **kwargs)
            artifact = temp / 'staging' / 'router'
            with patch.object(build.subprocess, 'run', side_effect=run), patch.object(build.subprocess, 'check_output', side_effect=output):
                receipt = build.build(server, library, artifact, 'linux', 'amd64')
            self.assertTrue(receipt['source_clean'])
            self.assertEqual(receipt['server_revision'], build.revision(server))
            self.assertEqual(receipt['library_revision'], build.revision(library))
            self.assertEqual(json.loads(Path(str(artifact) + '.build.json').read_text()), receipt)
            self.assertEqual(Path(str(artifact) + '.build.json').stat().st_mode & 0o777, 0o600)
            with self.assertRaises(ValueError):
                build.build(server, library, artifact, 'linux', 'amd64')
            with self.assertRaises(ValueError):
                build.build(server, library, server / 'artifact', 'linux', 'amd64')
            with self.assertRaises(ValueError):
                build.build(server, library, Path.home() / '.local/bin/smolllm-server', 'linux', 'amd64')


if __name__ == '__main__':
    unittest.main()
