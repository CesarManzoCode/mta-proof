"""rfc5322_format: every captured message parses without defects (4, D07).

Reads every messages/*.eml under out/runs, the bytes the receiver accepted
in all the runs of this verify (simulated and real), and parses each with
email.message_from_bytes(..., policy=email.policy.strict) of Python
[R-5322]. strict raises on the first defect; every part of the parsed tree
must also come out with an empty defects list.
"""

import email
import email.policy
import os

from verifier import report

NAME = "rfc5322_format"


def defects_of(data):
    """None when data parses with policy.strict and no part has defects,
    else the reason."""
    try:
        msg = email.message_from_bytes(data, policy=email.policy.strict)
        for part in msg.walk():
            if part.defects:
                return f"{type(part.defects[0]).__name__}: {part.defects}"
    except Exception as e:  # noqa: BLE001 - strict reports defects as exceptions
        return f"{type(e).__name__}: {e}"
    return None


# out/runs/mutation holds the inner suites of mutation_detection, each with
# its own rfc5322_format; their captures are not this run's.
SKIP = ("mutation",)


def captured(runs_dir):
    """Paths of the captured .eml files under runs_dir, sorted, outside SKIP."""
    out = []
    for dirpath, dirs, files in os.walk(runs_dir):
        if os.path.normpath(dirpath) == os.path.normpath(runs_dir):
            dirs[:] = [d for d in dirs if d not in SKIP]
        if os.path.basename(dirpath) != "messages":
            continue
        out += [os.path.join(dirpath, f) for f in files if f.endswith(".eml")]
    return sorted(out)


def check(paths, rel=lambda p: p):
    """Returns (ok, details, failures)."""
    failures = []
    runs = set()
    for path in paths:
        runs.add(rel(os.path.dirname(os.path.dirname(path))))
        with open(path, "rb") as f:
            reason = defects_of(f.read())
        if reason:
            failures.append(f"{rel(path)}: {reason}")
    if not paths:
        failures.append("no captured .eml files")
    details = {"files": len(paths), "with_defects": len(failures) if paths else 0, "runs": sorted(runs)}
    return not failures, details, failures


def run(ctx):
    runs_dir = ctx.run_dir("")
    ok, details, failures = check(captured(runs_dir), ctx.rel)
    return report.result(NAME, ok, details, trace=ctx.rel(runs_dir), failures=failures)
