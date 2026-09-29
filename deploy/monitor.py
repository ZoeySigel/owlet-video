#!/usr/bin/env python3
"""Read-only production checks. Alerts go to journald; no external recipients."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import time
import urllib.request

STATE = Path('/var/lib/owlet-video-monitor/state.json')

def queues_healthy(rows, core=False):
    queues = {parts[0]: int(parts[1])+int(parts[2]) for line in rows.splitlines() if len(parts := line.split()) == 3 and parts[0].startswith('owlet.')}
    required = ['owlet.events', 'owlet.retry', 'owlet.dead']
    if core:
        required += ['owlet.core.action_changed', 'owlet.core.published', 'owlet.core.embedding']
    return all(name in queues for name in required) and queues['owlet.dead'] == 0 and sum(queues.values()) < 200


def run(args):
    return subprocess.run(args, check=True, capture_output=True, text=True, timeout=20).stdout


def collect():
    checks = {}
    def check(name, fn):
        try:
            checks[name] = bool(fn())
        except Exception:
            checks[name] = False
    def health(url='http://127.0.0.1:8080/healthz'):
        with urllib.request.urlopen(url, timeout=3) as response:
            value = json.load(response)
        return value.get('status') == 'ok' and value.get('writes', {}).get('status') == 'ok'
    check('application', health)
    check('public_https', lambda: health('https://owl-et.me/healthz'))
    for service in ('owlet-video-api', 'owlet-video-worker', 'caddy'):
        check(service, lambda s=service: run(['systemctl', 'is-active', s]).strip() == 'active')
    def queues():
        rows = run(['rabbitmqctl', '-q', 'list_queues', '-p', os.environ.get('RABBITMQ_VHOST', 'owlet_video'), 'name', 'messages_ready', 'messages_unacknowledged'])
        return queues_healthy(rows, os.environ.get('BACKEND_ENGINE') == 'gcfeed')
    check('message_queues', queues)
    def backup():
        files = list(Path('/var/backups/owlet-video').glob('20*/mysql.sql.gz'))
        return bool(files) and time.time() - max(p.stat().st_mtime for p in files if p.stat().st_size > 0) < 36*3600
    check('backup_freshness', backup)
    check('disk_space', lambda: shutil.disk_usage('/').free >= 8*1024**3)
    def memory():
        fields = {line.split(':')[0]: int(line.split()[1]) for line in Path('/proc/meminfo').read_text().splitlines()}
        return fields['MemAvailable'] >= 160*1024 and fields['SwapTotal']-fields['SwapFree'] < 256*1024
    check('memory_headroom', memory)
    return checks


def advance(previous, checks):
    state, events = {}, []
    for name, ok in checks.items():
        old = previous.get(name, {})
        failures = 0 if ok else old.get('failures', 0)+1
        successes = old.get('successes', 0)+1 if ok else 0
        active = old.get('active', False)
        if failures >= 3 and not active:
            active = True
            events.append({'event': 'alert', 'check': name})
        elif successes >= 3 and active:
            active = False
            events.append({'event': 'recovered', 'check': name})
        state[name] = {'failures': failures, 'successes': successes, 'active': active}
    return state, events


def main():
    try:
        previous = json.loads(STATE.read_text())
    except (OSError, ValueError):
        previous = {}
    checks = collect()
    state, events = advance(previous, checks)
    STATE.parent.mkdir(parents=True, exist_ok=True)
    temp = STATE.with_suffix('.tmp')
    temp.write_text(json.dumps(state))
    os.replace(temp, STATE)
    for event in events:
        print(json.dumps(event), flush=True)
    print(json.dumps({'checks': checks, 'activeAlerts': [name for name, value in state.items() if value['active']]}), flush=True)
    return int(any(value['active'] for value in state.values()))


if __name__ == '__main__':
    raise SystemExit(main())
