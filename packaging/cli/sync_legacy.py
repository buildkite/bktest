"""Project the canonical CLI into the legacy Go module and create its paired tag."""

import argparse
import base64
import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile

from publish import open_stage, validate_ref
from release_input import approved_input

LEGACY = 'https://github.com/buildkite/test-engine-client.git'
PROJECTOR = 'c4eccfc3f559a449b0f7018581659cedc1eba5bd'


def git(repo, *args, env=None):
    return subprocess.check_output(['git', '-C', repo, *args], env=env, text=True).strip()


def sync(canonical, legacy, projector, tag, source, auth, apply):
    tag_ref = f'refs/tags/{tag}'
    expected = projector.expected_tree(canonical, source)
    existing = git(legacy, 'tag', '--list', tag)
    if existing:
        git(legacy, 'checkout', '--detach', f'{tag_ref}^{{commit}}')
        if projector.committed_projection(legacy, expected) != expected:
            raise ValueError('existing legacy tag does not match the approved projection')
        git(legacy, 'merge-base', '--is-ancestor', 'HEAD', 'refs/heads/main')
        return git(legacy, 'rev-parse', 'HEAD')

    previous = (legacy / '.bktest-source').read_text().splitlines()
    prior = previous[-1].removeprefix('commit ')
    validate_ref(tag, source, prior)
    git(canonical, 'merge-base', '--is-ancestor', prior, source)
    old = projector.expected_tree(canonical, prior)
    if projector.committed_projection(legacy, old) != old:
        raise ValueError('legacy main differs from its recorded projection')
    for name in sorted(old.keys() - expected.keys()):
        (legacy / name).unlink()
    for name, (mode, data) in expected.items():
        file = legacy / name
        if name not in old and (file.exists() or file.is_symlink()):
            raise ValueError(f'projection would overwrite legacy-owned file: {name}')
        if any(parent.is_symlink() for parent in file.parents if parent != legacy.parent):
            raise ValueError(f'projection traverses a symlink: {name}')
        file.parent.mkdir(parents=True, exist_ok=True)
        file.write_bytes(data)
        file.chmod(mode)
    git(legacy, 'add', '--all')
    if git(legacy, 'diff', '--cached', '--name-only'):
        git(legacy, '-c', 'user.name=Buildkite CLI Release', '-c',
            'user.email=release@buildkite.com', 'commit', '-m', f'Sync CLI source for {tag}')
    if projector.committed_projection(legacy, expected) != expected:
        raise ValueError('generated commit does not match the approved projection')
    if apply:
        # No force, and no partial update if the tag suddenly exists or main moved.
        git(legacy, 'push', '--atomic', LEGACY, 'HEAD:refs/heads/main', f'HEAD:{tag_ref}', env=auth)
        remote = git(legacy, 'ls-remote', LEGACY, tag_ref, env=auth)
        if remote != f'{git(legacy, "rev-parse", "HEAD")}\t{tag_ref}':
            raise ValueError('legacy tag readback differs after the push')
    return git(legacy, 'rev-parse', 'HEAD')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    canonical = Path.cwd()
    tag = os.environ.get('BUILDKITE_TAG', '')
    source = git(canonical, 'rev-parse', 'HEAD')
    validate_ref(tag, source)
    if git(canonical, 'rev-parse', f'refs/tags/{tag}^{{commit}}') != source:
        raise ValueError('canonical tag differs from the checkout')
    ref = f'refs/tags/{tag}'
    remote = git(canonical, 'ls-remote', 'https://github.com/buildkite/bktest.git', ref, f'{ref}^{{}}')
    targets = {name: sha for sha, name in (line.split('\t') for line in remote.splitlines())}
    if targets.get(f'{ref}^{{}}', targets.get(ref)) != source:
        raise ValueError('remote canonical tag differs from the checkout')
    fetch = ['fetch', '--no-tags']
    if git(canonical, 'rev-parse', '--is-shallow-repository') == 'true':
        fetch.append('--unshallow')
    git(canonical, *fetch, 'https://github.com/buildkite/bktest.git', 'refs/heads/main')
    git(canonical, 'merge-base', '--is-ancestor', source, 'FETCH_HEAD')
    auth = None
    if args.apply:
        with approved_input(stable=False) as (archive, _, sha, _, approved_tag, _, approved_source):
            with open_stage(Path(archive), sha, approved_tag, approved_source):
                pass
        token = os.environ['CLI_LEGACY_TOKEN']
        if not token:
            raise ValueError('missing legacy sync credential')
        auth = dict(os.environ, GIT_CONFIG_COUNT='1', GIT_CONFIG_KEY_0='http.https://github.com/.extraheader',
                    GIT_CONFIG_VALUE_0='Authorization: Basic ' + base64.b64encode(f'x-access-token:{token}'.encode()).decode())
    with tempfile.TemporaryDirectory() as temp:
        legacy = Path(temp) / 'legacy'
        git(temp, 'clone', '--branch', 'main', LEGACY, str(legacy))
        tool = Path(temp) / 'project.py'
        for file in ('project.py', 'manifest'):
            (Path(temp) / file).write_text(git(legacy, 'show', f'{PROJECTOR}:util/legacy-source/{file}') + '\n')
        spec = importlib.util.spec_from_file_location('legacy_project', tool)
        projector = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(projector)
        projected = sync(canonical, legacy, projector, tag, source, auth, args.apply)
        if args.apply:
            subprocess.run(['buildkite-agent', 'meta-data', 'set', 'cli-legacy-source', projected], check=True)
        print(projected)


if __name__ == '__main__':
    main()
