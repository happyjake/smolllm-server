#!/usr/bin/env python3
"""Build a provenance-stamped artifact from two clean, committed repositories.

Writes only the explicit artifact and its .build.json receipt. Never installs or
restarts anything. Each build uses git archives so untracked/ignored files cannot
silently enter a release. The local replace dependency is archived beside it.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile


def git(repo, *args):
    return subprocess.check_output(['git', '--no-replace-objects', '-C', str(repo)] + list(args), text=True, stderr=subprocess.DEVNULL).strip()


def revision(repo):
    try:
        root = Path(git(repo, 'rev-parse', '--show-toplevel')).resolve()
    except subprocess.CalledProcessError:
        raise ValueError('Expected a working repository root') from None
    if root != repo.resolve():
        raise ValueError('Expected a repository root')
    if git(repo, 'status', '--porcelain', '--untracked-files=all'):
        raise ValueError('Release inputs must be clean and committed')
    return git(repo, 'rev-parse', 'HEAD')


def archive(repo, rev, target, temp):
    tar = temp / (target.name + '.tar')
    subprocess.run(['git', '--no-replace-objects', '-C', str(repo), 'archive', '--format=tar', '-o', str(tar), rev], check=True)
    target.mkdir()
    with tarfile.open(tar) as f:
        # git archives contain trusted committed paths, but reject links escaping
        # the checkout before extracting for build tools to follow them.
        for m in f.getmembers():
            name = Path(m.name)
            if name.is_absolute() or '..' in name.parts:
                raise ValueError('Unsafe archive path')
            if m.issym() or m.islnk():
                dest = (target / name.parent / m.linkname).resolve()
                if target.resolve() not in (dest, *dest.parents):
                    raise ValueError('Archive link escapes repository')
        f.extractall(target, filter='data')


def validate_modules(temp, env):
    server = temp / 'smolllm-server'
    if (server / 'vendor').exists():
        raise ValueError('Vendored modules are not supported by this archived build')
    raw = subprocess.check_output(['go', 'list', '-m', '-json', 'all'], cwd=server, env=env, text=True)
    decoder, modules = json.JSONDecoder(), []
    while raw.strip():
        module, end = decoder.raw_decode(raw.lstrip())
        modules.append(module)
        raw = raw.lstrip()[end:]
    libraries = [m for m in modules if m.get('Path') == 'github.com/rocry/smolllm-go']
    if (len(libraries) != 1 or
            Path(libraries[0].get('Replace', {}).get('Dir', '/missing')).resolve() != (temp / 'smolllm-go').resolve()):
        raise ValueError('The compiled library must be the archived sibling repository')
    for module in modules:
        replacement = module.get('Replace', {}).get('Dir')
        if replacement:
            directory = Path(replacement).resolve()
            if temp.resolve() not in (directory, *directory.parents):
                raise ValueError('Module replacement escapes archived sources')


def build(server, library, output, goos, goarch):
    server, library, output = server.resolve(), library.resolve(), output.resolve()
    if output == (Path.home() / '.local/bin/smolllm-server').resolve():
        raise ValueError('Use a staging artifact path, not the live service binary')
    for repo in (server, library):
        if repo in (output, *output.parents):
            raise ValueError('Output must be outside source repositories')
    if output.exists() or Path(str(output) + '.build.json').exists():
        raise ValueError('Artifact/receipt already exists; choose a new staging path')
    pair = {'server_revision': revision(server), 'library_revision': revision(library)}
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='smolllm-build-') as td:
        temp = Path(td)
        archive(server, pair['server_revision'], temp / 'smolllm-server', temp)
        archive(library, pair['library_revision'], temp / 'smolllm-go', temp)
        prefix = 'github.com/rocry/smolllm-server/internal/buildinfo.'
        flags = ' '.join('-X ' + prefix + k + '=' + v for k, v in (
            ('serverRevision', pair['server_revision']), ('libraryRevision', pair['library_revision']), ('sourceClean', 'true')))
        env = dict(os.environ, CGO_ENABLED='0', GOOS=goos, GOARCH=goarch,
                   GOWORK='off', GOENV='off', GOTOOLCHAIN='local', GOFLAGS='-mod=readonly')
        # Disable persisted config, caller overlays and automatic toolchain swaps.
        # Verify that the archived library, not a vendor/cache/external checkout,
        # is the actual module selected by the committed server go.mod.
        validate_modules(temp, env)
        artifact = temp / 'artifact'
        subprocess.run(['go', 'build', '-trimpath', '-ldflags', flags, '-o', str(artifact), './cmd/server'],
                       cwd=temp / 'smolllm-server', env=env, check=True)
        payload = artifact.read_bytes()
        receipt = dict(pair, schema_version=1, source_clean=True, goos=goos, goarch=goarch,
                       go_version=subprocess.check_output(['go', 'version'], cwd=temp / 'smolllm-server', env=env, text=True).strip(),
                       sha256=hashlib.sha256(payload).hexdigest())
        # Exclusive creation prevents accidentally replacing a deployed artifact.
        fd = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o755)
        with os.fdopen(fd, 'wb') as f:
            f.write(payload)
        receipt_path = Path(str(output) + '.build.json')
        fd = os.open(receipt_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, 'w') as f:
            json.dump(receipt, f, indent=2)
            f.write('\n')
    return receipt


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--server', type=Path, default=Path(__file__).resolve().parent.parent)
    p.add_argument('--library', type=Path)
    p.add_argument('--output', type=Path, required=True)
    hostenv = dict(os.environ, GOENV='off', GOTOOLCHAIN='local', GOFLAGS='-mod=readonly', GOWORK='off')
    p.add_argument('--goos', default=subprocess.check_output(['go', 'env', 'GOHOSTOS'], env=hostenv, text=True).strip())
    p.add_argument('--goarch', default=subprocess.check_output(['go', 'env', 'GOHOSTARCH'], env=hostenv, text=True).strip())
    a = p.parse_args()
    import sys
    if sys.version_info < (3, 12):
        p.exit(1, 'Build refused: Python 3.12+ is required\n')
    try:
        receipt = build(a.server, a.library or a.server.parent / 'smolllm-go', a.output, a.goos, a.goarch)
    except (ValueError, subprocess.CalledProcessError) as e:
        p.exit(1, 'Build refused: ' + str(e) + '\n')
    print(json.dumps(receipt, indent=2))


if __name__ == '__main__':
    main()
