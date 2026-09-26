"""Print health JSON and exit nonzero for degraded/unknown status.

Run every 10 seconds from a monitoring agent and route nonzero results through
its existing notification channel. This script never sends external messages.
"""
import json
import sys
import urllib.request


def main():
    url = sys.argv[1] if len(sys.argv) == 2 else "http://127.0.0.1:8080/healthz"
    try:
        with urllib.request.urlopen(url, timeout=3) as response:
            result = json.load(response)
        failed = result.get("status") != "ok" or result.get("writes", {}).get("status") != "ok"
    except (OSError, ValueError) as error:
        result = {"status": "unavailable", "error": str(error)}
        failed = True
    print(json.dumps(result, ensure_ascii=False))
    return int(failed)


if __name__ == "__main__":
    raise SystemExit(main())
