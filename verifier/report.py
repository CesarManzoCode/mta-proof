"""Writer of out/report.json, contract 3.7."""

import datetime
import json
import os
import subprocess

FORMAT = "mta-proof-report/1"
VERDICTS = ("pass", "fail", "not_evaluated")
# At most this many failure descriptions per property go to the report.
MAX_FAILURES = 50


def result(name, ok, details, seed=None, trace=None, failures=()):
    """A property entry; failures are kept in details so a reader sees why."""
    failures = list(failures)
    details = dict(details)
    if failures:
        details["failures"] = failures[:MAX_FAILURES]
        details["failures_total"] = len(failures)
    return {
        "name": name,
        "verdict": "pass" if ok and not failures else "fail",
        "seed": seed,
        "trace": trace,
        "details": details,
    }


def git_head(root):
    try:
        res = subprocess.run(["git", "rev-parse", "HEAD"], cwd=root,
                             capture_output=True, text=True)
    except OSError:
        return None
    return res.stdout.strip() if res.returncode == 0 else None


def build(root, started_at, duration_s, max_memory_bytes, memory_limit_bytes, properties):
    for p in properties:
        if p["verdict"] not in VERDICTS:
            raise ValueError(f"{p['name']}: verdict {p['verdict']!r}")
    verdict = "pass" if properties and all(p["verdict"] == "pass" for p in properties) else "fail"
    return {
        "format": FORMAT,
        "git_head": git_head(root),
        "started_at": started_at.astimezone(datetime.timezone.utc).isoformat(timespec="seconds"),
        "duration_s": round(duration_s, 3),
        "max_memory_bytes": max_memory_bytes,
        "memory_limit_bytes": memory_limit_bytes,
        "verdict": verdict,
        "properties": properties,
    }


def write(path, report):
    """Write atomically, so a reader never sees half a report."""
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(report, f, indent=2, sort_keys=False)
        f.write("\n")
    os.replace(tmp, path)
