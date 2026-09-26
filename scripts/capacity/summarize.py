"""Validate raw counter invariants and render a measured-results table."""
import argparse
import json
import statistics
from pathlib import Path


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("directory")
    args = parser.parse_args()
    directory = Path(args.directory)
    rows = []
    for path in directory.glob("*.json"):
        result = json.loads(path.read_text(encoding="utf-8-sig"))
        if not isinstance(result, dict) or "Planned" not in result:
            continue
        assert result["Planned"] == result["Sent"] + result["Dropped"], path
        assert result["Sent"] == result["Success"] + result["Errors"], path
        assert sum(result["status"].values()) == result["Sent"], path
        assert result["AcceptedAsyncCompleted"] <= result["AcceptedAsync"], path
        rows.append((path, result))
    rows.sort(key=lambda pair: pair[1]["Started"])
    lines = ["# 原始测量汇总", "", "本表只展示各阶段实测，不自动将短时峰值认定为持续容量。", "",
             "异步成功 RPS 表示 HTTP 受理速率。完成延迟仅统计已完成命令；仍有积压时，它不能代表全部请求。", "",
             "| 阶段 | 目标 RPS | 秒 | 实际成功 RPS | 错误/丢弃 | P95/P99 ms | Outbox 起始→峰值→结束 | 命令积压峰值→结束 | 异步完成/受理 | 已完成命令 P95 ms |",
             "| --- | ---: | ---: | ---: | --- | --- | --- | --- | --- | ---: |"]
    for path, r in rows:
        b = r["backlog"]
        backlog = f'{b[0]["pendingOutbox"]}→{max(s["pendingOutbox"] for s in b)}→{b[-1]["pendingOutbox"]}'
        commands = f'{max(s["pendingCommands"] for s in b)}→{b[-1]["pendingCommands"]}'
        completed = f'{r["AcceptedAsyncCompleted"]}/{r["AcceptedAsync"]}'
        lines.append(f'| [{path.stem}]({path.name}) | {r["targetRps"]} | {r["windowSeconds"]:g} | {r["successRps"]:.1f} | {r["Errors"]}/{r["Dropped"]} | {r["LatencyMS"]["P95"]:.1f}/{r["LatencyMS"]["P99"]:.1f} | {backlog} | {commands} | {completed} | {r["CommandCompletionMS"]["P95"]:.1f} |')
    (directory / "measurements.md").write_text("\n".join(lines) + "\n", encoding="utf-8")
    resource_rows = []
    for path in sorted(directory.glob("*-resources.jsonl")):
        groups = {}
        for line in path.read_text(encoding="utf-8").splitlines():
            record = json.loads(line)
            if "stats" in record:
                s = record["stats"]
                groups.setdefault(s["Name"], []).append(s)
        for name, samples in groups.items():
            cpu = [float(s["CPUPerc"].rstrip("%")) for s in samples]
            memory = [float(s["MemPerc"].rstrip("%")) for s in samples]
            resource_rows.append({"stage": path.stem.removesuffix("-resources"), "container": name, "samples": len(samples), "cpuPercentMean": statistics.mean(cpu), "cpuPercentMax": max(cpu), "memoryPercentMax": max(memory)})
    (directory / "resource-summary.json").write_text(json.dumps(resource_rows, indent=2) + "\n", encoding="utf-8")
    print("\n".join(lines))


if __name__ == "__main__":
    main()
