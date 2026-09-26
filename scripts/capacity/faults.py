"""Bounded faults against the isolated owlet-capacity project only.

Run after seeding compose.capacity.soak.yml. Each dependency is stopped for
20 seconds during a 90-second, 20 RPS write stage and restored in finally.
Never substitutes another compose file, project, host, or credentials.
"""
import datetime as dt
import argparse
import json
from pathlib import Path
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[2]
OUTPUT = ROOT / "docs/capacity/2026-09-26-steady"
COMPOSE = ["docker", "compose", "-f", "compose.capacity.yml", "-f", "compose.capacity.soak.yml"]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--services", nargs="+", choices=("worker", "redis", "rabbitmq", "mysql"), default=["worker", "redis", "rabbitmq", "mysql"])
    parser.add_argument("--label", default="fault")
    args = parser.parse_args()
    OUTPUT.mkdir(parents=True, exist_ok=True)
    for service in args.services:
        prefix = f"{args.label}-{service}"
        if (OUTPUT / f"{prefix}-async-20.json").exists():
            raise SystemExit(f"Refusing to overwrite {prefix}")
        timeline = []

        def action(verb):
            timeline.append({"at": dt.datetime.now(dt.timezone.utc).isoformat(), "action": f"{verb} {service}", "phase": "start"})
            print(f"FAULT {verb} {service}", flush=True)
            result = subprocess.run([*COMPOSE, verb, service], cwd=ROOT, capture_output=True, text=True, timeout=60)
            timeline.append({"at": dt.datetime.now(dt.timezone.utc).isoformat(), "action": f"{verb} {service}", "phase": "end", "returncode": result.returncode, "output": result.stdout + result.stderr})
            result.check_returncode()

        proc = subprocess.Popen([sys.executable, "-X", "utf8", "scripts/capacity/run.py", "--scenarios", "async", "--rates", "20", "--duration", "90s", "--label", prefix, "--output", str(OUTPUT)], cwd=ROOT)
        try:
            time.sleep(20)
            if proc.poll() is not None:
                raise RuntimeError("Load stage exited before fault injection")
            action("stop")
            time.sleep(20)
        finally:
            try:
                action("start")
            finally:
                (OUTPUT / f"{prefix}-timeline.json").write_text(json.dumps(timeline, indent=2), encoding="utf-8")
        if proc.wait(timeout=150):
            raise SystemExit("Fault stage failed to produce a valid result")
        result = json.loads((OUTPUT / f"{prefix}-async-20.json").read_text())
        if result["backlog"][-1]["pendingCommands"] or result["backlog"][-1]["pendingOutbox"] or result["CommandsFailed"] or result["AcceptedAsyncCompleted"] != result["AcceptedAsync"]:
            raise SystemExit("Recovery incomplete; stopping further faults")
        # Capture RabbitMQ after each stage too: DB drain alone is insufficient.
        queues = subprocess.run([*COMPOSE, "exec", "-T", "rabbitmq", "rabbitmqctl", "list_queues", "name", "messages_ready", "messages_unacknowledged"], cwd=ROOT, capture_output=True, text=True, timeout=60)
        (OUTPUT / f"{prefix}-queues.txt").write_text(queues.stdout + queues.stderr, encoding="utf-8")
        queues.check_returncode()
        counts = [line.split() for line in queues.stdout.splitlines() if line.startswith("owlet.")]
        if len(counts) != 3 or any(len(row) != 3 or row[1:] != ["0", "0"] for row in counts):
            raise SystemExit("RabbitMQ not drained; stopping further faults")
        time.sleep(5)


if __name__ == "__main__":
    main()
