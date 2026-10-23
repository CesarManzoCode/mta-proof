"""determinism: same seed, same trace; another seed, another trace (4).

The verifier recomputes each trace hash from receiver.jsonl and engine.db
(3.6) instead of trusting trace.sha256, and checks that the files the engine
wrote agree with the recomputation.
"""

import os

from verifier import formats, report

NAME = "determinism"
SCENARIO = "determinism"
SEED = 1
OTHER_SEED = 2


def check(run_a, run_b, run_other):
    """Returns (ok, details, failures) for three run directories."""
    failures = []
    hashes = []
    for label, d in (("a", run_a), ("b", run_b), ("other", run_other)):
        h = formats.trace_sha256(d)
        hashes.append(h)
        recorded = formats.read_recorded_trace(d)
        if recorded != h:
            failures.append(f"{label}: trace.sha256 {recorded} differs from the recomputed {h}")
        events_path = os.path.join(d, "events.jsonl")
        if os.path.exists(events_path):
            with open(events_path, "rb") as f:
                if f.read() != formats.canonical_events(os.path.join(d, "engine.db")):
                    failures.append(f"{label}: events.jsonl differs from engine.db")
        else:
            failures.append(f"{label}: no events.jsonl")
    hash_a, hash_b, hash_other = hashes
    if hash_a != hash_b:
        failures.append("same seed gave different traces")
    if hash_a == hash_other:
        failures.append("another seed gave the same trace")
    details = {"runs": [run_a, run_b, run_other],
               "hash_a": hash_a, "hash_b": hash_b, "hash_other": hash_other}
    return not failures, details, failures


def run(ctx):
    base = ctx.fresh_dir(f"{SCENARIO}-{SEED}")
    dirs = [ctx.simulate(SCENARIO, seed, os.path.join(base, sub))
            for seed, sub in ((SEED, "a"), (SEED, "b"), (OTHER_SEED, f"seed-{OTHER_SEED}"))]
    ok, details, failures = check(*dirs)
    details["runs"] = [ctx.rel(d) for d in details["runs"]]
    return report.result(NAME, ok, details, seed=SEED, trace=ctx.rel(base), failures=failures)
