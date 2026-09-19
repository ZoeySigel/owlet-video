#!/usr/bin/env python3
"""Forced SSH command: receive one validated release and atomically switch it."""
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.request

BASE = pathlib.Path('/srv/owlet-video')
RELEASES = BASE / 'releases'
CURRENT = BASE / 'current'
MAX_ARCHIVE = 150 * 1024 * 1024
MAX_EXTRACTED = 300 * 1024 * 1024


def restart():
    for service in ('owlet-video-api.service', 'owlet-video-worker.service'):
        subprocess.run(['/usr/bin/sudo', '-n', '/usr/bin/systemctl', 'restart', service], check=True)


def healthy():
    for _ in range(20):
        try:
            worker = subprocess.run(['/usr/bin/systemctl', 'is-active', '--quiet', 'owlet-video-worker.service'])
            with urllib.request.urlopen('http://127.0.0.1:8080/healthz', timeout=2) as response:
                if response.status == 200 and worker.returncode == 0:
                    return True
        except Exception:
            pass
        time.sleep(2)
    return False


def main():
    command = os.environ.get('SSH_ORIGINAL_COMMAND', '')
    match = re.fullmatch(r'deploy ([0-9a-f]{40})', command)
    if not match:
        raise ValueError('expected deploy <40-character commit SHA>')
    revision = match.group(1)
    RELEASES.mkdir(parents=True, exist_ok=True)
    target = RELEASES / revision
    if target.exists():
        raise ValueError('release already exists')
    with tempfile.NamedTemporaryFile(dir=BASE, prefix='.incoming-', delete=False) as stream:
        archive = pathlib.Path(stream.name)
        total = 0
        while True:
            chunk = sys.stdin.buffer.read(1024 * 1024)
            if not chunk:
                break
            total += len(chunk)
            if total > MAX_ARCHIVE:
                raise ValueError('archive too large')
            stream.write(chunk)
    staged = pathlib.Path(tempfile.mkdtemp(dir=RELEASES, prefix='.staged-'))
    staged.chmod(0o755)
    try:
        extracted = 0
        with tarfile.open(archive, 'r:gz') as bundle:
            for member in bundle:
                path = pathlib.PurePosixPath(member.name)
                if path.is_absolute() or '..' in path.parts or path.parts[0] not in ('bin', 'web'):
                    raise ValueError('invalid archive path')
                if not (member.isdir() or member.isfile()):
                    raise ValueError('archive links and devices are not allowed')
                extracted += member.size
                if extracted > MAX_EXTRACTED:
                    raise ValueError('extracted release too large')
                destination = staged.joinpath(*path.parts)
                if member.isdir():
                    destination.mkdir(parents=True, exist_ok=True)
                else:
                    destination.parent.mkdir(parents=True, exist_ok=True)
                    with bundle.extractfile(member) as source, destination.open('wb') as output:
                        shutil.copyfileobj(source, output)
                    destination.chmod(0o755 if path.as_posix() == 'bin/owlet-video' else 0o644)
        if not (staged / 'bin/owlet-video').is_file() or not (staged / 'web/index.html').is_file():
            raise ValueError('incomplete release')
        os.replace(staged, target)
        old = os.readlink(CURRENT) if CURRENT.is_symlink() else None
        link = BASE / ('.current-' + revision)
        link.symlink_to(target)
        os.replace(link, CURRENT)
        try:
            restart()
            if not healthy():
                raise RuntimeError('health check failed')
        except Exception:
            if old:
                rollback = BASE / '.rollback'
                rollback.symlink_to(old)
                os.replace(rollback, CURRENT)
                restart()
            raise
        print(f'deployed {revision}')
    finally:
        archive.unlink(missing_ok=True)
        if staged.exists():
            shutil.rmtree(staged)


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        print(f'deploy failed: {exc}', file=sys.stderr)
        sys.exit(1)
