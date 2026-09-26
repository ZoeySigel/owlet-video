"""Run bounded, reproducible local capacity stages and capture Docker metrics.

Start compose.capacity.yml and seed with the Go tool first. Never targets a
remote host; resource collection is restricted to the owlet-capacity project.
"""
import argparse
import datetime as dt
import json
import os
from pathlib import Path
import subprocess
import threading
import time

ROOT = Path(__file__).resolve().parents[2]
CONTAINERS = [f"owlet-capacity-{name}-1" for name in ("api", "worker", "mysql", "redis", "rabbitmq")]


def resources(stop, path):
    with path.open("w", encoding="utf-8") as stream:
        while not stop.is_set():
            now = dt.datetime.now(dt.timezone.utc).isoformat()
            try:
                proc = subprocess.run(["docker", "stats", "--no-stream", "--format", "{{json .}}", *CONTAINERS], capture_output=True, text=True, timeout=15)
                for line in proc.stdout.splitlines():
                    stream.write(json.dumps({"at": now, "stats": json.loads(line)}) + "\n")
                if proc.returncode:
                    stream.write(json.dumps({"at": now, "error": proc.stderr.strip()}) + "\n")
            except (subprocess.TimeoutExpired, ValueError) as error:
                stream.write(json.dumps({"at": now, "error": str(error)}) + "\n")
            stream.flush()
            stop.wait(5)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--scenarios", default="detail-hot,latest,detail-random,likes,hot,write,async,mixed")
    parser.add_argument("--rates", default="20,100,300")
    parser.add_argument("--duration", default="30s")
    parser.add_argument("--label", default="screen")
    parser.add_argument("--output", default="docs/capacity/2026-09-23")
    args = parser.parse_args()
    output = ROOT / args.output
    output.mkdir(parents=True, exist_ok=True)
    binary = ROOT / ".tools/capacity" / ("loadgen.exe" if os.name == "nt" else "loadgen")
    for scenario in args.scenarios.split(","):
        for rate in map(int, args.rates.split(",")):
            name = f"{args.label}-{scenario}-{rate}"
            result_path = output / f"{name}.json"
            if result_path.exists():
                raise SystemExit(f"Refusing to overwrite {result_path}")
            print(f"START {name} duration={args.duration}", flush=True)
            stop = threading.Event()
            monitor = threading.Thread(target=resources, args=(stop, output / f"{name}-resources.jsonl"))
            monitor.start()
            try:
                with (output / f"{name}.log").open("w", encoding="utf-8") as log:
                    proc = subprocess.Popen([str(binary), "-scenario", scenario, "-rate", str(rate), "-duration", args.duration, "-out", str(result_path)], cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
                    for line in proc.stdout:
                        log.write(line)
                        log.flush()
                        print(line.rstrip(), flush=True)
                    if proc.wait():
                        raise SystemExit(proc.returncode)
            finally:
                stop.set()
                monitor.join()
            result = json.loads(result_path.read_text(encoding="utf-8"))
            failed_ratio = (result["Errors"] + result["Dropped"]) / result["Planned"]
            if failed_ratio > .05:
                print(f"STOP increasing {scenario}: failure/drop ratio={failed_ratio:.2%}", flush=True)
                break
            if result["backlog"][-1]["pendingCommands"] > 5000:
                raise SystemExit("Stopping suite: command backlog exceeded 5000")
            time.sleep(3)


if __name__ == "__main__":
    main()
