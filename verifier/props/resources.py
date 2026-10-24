"""resources: memory.peak of the run's own cgroup within the limit (4, D17).

make verify runs the verifier inside a systemd user scope; the verifier reads
its own cgroup from /proc/self/cgroup, so memory.peak counts every process of
the run (verifier, engine, simulator), not only the largest one.
"""

import os
import time

from verifier import report

NAME = "resources"


def own_cgroup(proc_cgroup="/proc/self/cgroup"):
    """The cgroup v2 path of this process ("0::<path>")."""
    with open(proc_cgroup, encoding="ascii") as f:
        for line in f:
            hier, _, path = line.rstrip("\n").split(":", 2)
            if hier == "0":
                return path
    return None


def memory_peak(cgroup, root="/sys/fs/cgroup"):
    """memory.peak in bytes, or None if the cgroup does not expose it."""
    if cgroup is None:
        return None
    try:
        with open(os.path.join(root, cgroup.lstrip("/"), "memory.peak"), encoding="ascii") as f:
            return int(f.read().strip())
    except (OSError, ValueError):
        return None


def check(peak, limit, duration_s, cgroup):
    failures = []
    if peak is None:
        failures.append(f"memory.peak not readable for cgroup {cgroup!r}; run through make verify")
    elif peak > limit:
        failures.append(f"memory.peak {peak} bytes above the limit {limit}")
    details = {"memory_peak_bytes": peak, "memory_limit_bytes": limit,
               "duration_s": round(duration_s, 3), "cgroup": cgroup}
    return not failures, details, failures


def run(ctx):
    cgroup = own_cgroup()
    peak = memory_peak(cgroup)
    ok, details, failures = check(peak, ctx.params["memory_limit_bytes"],
                                  time.monotonic() - ctx.started, cgroup)
    ctx.max_memory_bytes = peak
    return report.result(NAME, ok, details, trace=ctx.rel(ctx.out), failures=failures)
