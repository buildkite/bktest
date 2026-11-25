"""Prepare the legacy sync PR or tag its verified merge commit."""

import argparse
import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile

from publish import open_stage, validate_ref
from release_input import approved_input
from release_pr import ReleasePR, git, git_auth

LEGACY = 'https://github.com/buildkite/test-engine-client.git'
PROJECTOR = 'c4eccfc3f559a449b0f7018581659cedc1eba5bd'


def project(canonical, legacy, projector, tag, source, expected):
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


def tag_merge(legacy, projector, expected, commit, tag):
    git(legacy, 'checkout', '--detach', commit)
    if projector.committed_projection(legacy, expected) != expected:
        raise ValueError('merged legacy commit does not match the approved projection')
    auth = git_auth()
    tag_ref = f'refs/tags/{tag}'
    git(legacy, 'push', LEGACY, f'{commit}:{tag_ref}', env=auth)
    if git(legacy, 'ls-remote', LEGACY, tag_ref, env=auth) != f'{commit}\t{tag_ref}':
        raise ValueError('legacy tag readback differs after the push')
    subprocess.run(['buildkite-agent', 'meta-data', 'set', 'cli-legacy-source', commit], check=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['prepare', 'tag'])
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
    with approved_input(stable=False) as (archive, _, sha, _, approved_tag, _, approved_source):
        with open_stage(Path(archive), sha, approved_tag, approved_source):
            pass
    with tempfile.TemporaryDirectory() as temp:
        legacy = Path(temp) / 'legacy'
        git(temp, 'clone', '--branch', 'main', LEGACY, str(legacy))
        tool = Path(temp) / 'project.py'
        for file in ('project.py', 'manifest'):
            (Path(temp) / file).write_text(git(legacy, 'show', f'{PROJECTOR}:util/legacy-source/{file}') + '\n')
        spec = importlib.util.spec_from_file_location('legacy_project', tool)
        projector = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(projector)
        expected = projector.expected_tree(canonical, source)
        pr = ReleasePR('test-engine-client', 'main', tag)
        if args.action == 'prepare':
            # Project on a detached HEAD so the base ref remains the checked-out main tip.
            git(legacy, 'checkout', '--detach')
            project(canonical, legacy, projector, tag, source, expected)
            pr.prepare(legacy)
        else:
            commit = pr.merged_commit(legacy)
            tag_merge(legacy, projector, expected, commit, tag)


if __name__ == '__main__':
    main()
