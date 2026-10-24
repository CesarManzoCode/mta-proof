"""mutation_detection: the suite catches every seeded fault (4, 4.3).

Runs the suite again as a subprocess, once with the original binaries of
bin/ and once with each mutant's of bin/mut/<name>/ (make build), with every
implemented property except mutation_detection and external_validation.
Each run writes into out/runs/mutation/<name>/, with its report at
report.json and the suite's output at verify.log. Passes only if the
original suite exits 0 and every mutant's exits non-zero with at least the
property of 4.3 failing. The original binaries must hold none of the mutant
names (each mutant logs its name, so its binary holds it): a build tag that
leaked into bin/ would test a mutant as the original.
"""

import os
import subprocess
import sys
import time

from verifier import formats, report

NAME = "mutation_detection"

# Contract 4.3: mutant -> property that must fail.
MUTANTS = (
    ("mut_no_backoff", "backoff_expiry"),
    ("mut_retry_permanent", "outbound_auth"),
    ("mut_ignore_conn_limit", "receiver_limits"),
    ("mut_ack_before_persist", "no_loss"),
    ("mut_shared_queue", "destination_isolation"),
    ("mut_exceed_warmup", "ip_warmup"),
)
ORIGINAL = "original"
# Not part of the inner suite: this property, and the external MTA's.
EXCLUDED = (NAME, "external_validation")
BINARIES = ("mtaengine", "mtadst")


def inner_properties(names):
    return [n for n in names if n not in EXCLUDED]


def failed_properties(rep):
    return [p["name"] for p in rep.get("properties", []) if p.get("verdict") != "pass"]


def check(original, mutants):
    """original and each mutant: {"name", "exit_code", "report"} (report is the
    parsed report.json or None); mutants also carry "expected". The original
    may carry "leaked" and each mutant "tagged" (binary checks). Returns
    (ok, details, failures)."""
    failures = []
    orig_failed = failed_properties(original["report"]) if original["report"] else None
    if original["exit_code"] != 0:
        failures.append(f"original engine: suite exit code {original['exit_code']}, want 0"
                        f" (failed: {orig_failed})")
    if original["report"] is None:
        failures.append("original engine: no report.json")
    for b, names in sorted(original.get("leaked", {}).items()):
        failures.append(f"original {b} holds the mutant names {names}")
    entries = []
    for m in mutants:
        failed = failed_properties(m["report"]) if m["report"] else None
        entries.append({"name": m["name"], "exit_code": m["exit_code"], "failed_properties": failed,
                        "expected_property": m["expected"], "duration_s": m.get("duration_s")})
        if m["exit_code"] == 0:
            failures.append(f"{m['name']}: suite exit code 0, the mutant went undetected")
        if m["report"] is None:
            failures.append(f"{m['name']}: no report.json (exit code {m['exit_code']})")
        elif m["expected"] not in failed:
            failures.append(f"{m['name']}: {m['expected']} passed (failed: {failed})")
        for b in m.get("untagged", ()):
            failures.append(f"{m['name']}: its {b} does not hold its name; not built with its tag")
    details = {"original_exit_code": original["exit_code"], "original_failed_properties": orig_failed,
               "original_duration_s": original.get("duration_s"), "mutants": entries}
    return not failures, details, failures


def names_in(path, names):
    """The names whose bytes appear in the file at path."""
    with open(path, "rb") as f:
        data = f.read()
    return [n for n in names if n.encode() in data]


def run_suite(ctx, name, bin_dir, properties):
    """python3 -m verifier verify into out/runs/mutation/<name> with bin_dir."""
    out = ctx.fresh_dir(os.path.join("mutation", name))
    cmd = [sys.executable, "-m", "verifier", "verify", "--out", out, "--root", ctx.root,
           "--bin", bin_dir, "--only", ",".join(properties)]
    t0 = time.monotonic()
    with open(os.path.join(out, "verify.log"), "w", encoding="utf-8") as log:
        res = subprocess.run(cmd, cwd=ctx.root, stdout=log, stderr=subprocess.STDOUT)
    path = os.path.join(out, "report.json")
    rep = formats.load_json(path) if os.path.exists(path) else None
    return {"name": name, "exit_code": res.returncode, "report": rep,
            "duration_s": round(time.monotonic() - t0, 3)}


def run(ctx):
    from verifier import cli  # cli imports this module

    properties = inner_properties(cli.PROPERTIES)
    names = [m for m, _ in MUTANTS]
    original = run_suite(ctx, ORIGINAL, ctx.bin, properties)
    original["leaked"] = {b: found for b in BINARIES
                          if (found := names_in(os.path.join(ctx.bin, b), names))}
    mutants = []
    for name, expected in MUTANTS:
        bin_dir = os.path.join(ctx.bin, "mut", name)
        m = run_suite(ctx, name, bin_dir, properties)
        m["expected"] = expected
        m["untagged"] = [b for b in BINARIES if not names_in(os.path.join(bin_dir, b), [name])]
        mutants.append(m)
    ok, details, failures = check(original, mutants)
    details["properties"] = properties
    details["reports"] = {m["name"]: ctx.rel(os.path.join(ctx.run_dir(os.path.join("mutation", m["name"])),
                                                          "report.json")) for m in [original] + mutants}
    return report.result(NAME, ok, details, trace=ctx.rel(ctx.run_dir("mutation")), failures=failures)
