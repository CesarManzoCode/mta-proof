"""receiver_limits: zero counted violations per profile and source IP (3.1, 4).

Only the receiver log decides: a violation is what the simulator wrote as a
`violation` line with `counted: true` (after the profile's adaptation period).
"""

from verifier import formats, report

NAME = "receiver_limits"
SCENARIO = "limits"
SEED = 1


def check(receiver):
    """Returns (ok, details, failures); details.violations is {profile: {ip: n}}."""
    violations = {}
    uncounted = {}
    by_rule = {}
    accepted = 0
    for r in receiver:
        ev = r["ev"]
        if ev == "connect":
            violations.setdefault(r["profile"], {}).setdefault(r["src_ip"], 0)
        elif ev == "data_end" and isinstance(r.get("code"), int) and 200 <= r["code"] < 300:
            accepted += 1
        elif ev == "violation":
            counted = r.get("counted")
            if counted is True:
                violations.setdefault(r["profile"], {}).setdefault(r["src_ip"], 0)
                violations[r["profile"]][r["src_ip"]] += 1
                by_rule[r.get("rule")] = by_rule.get(r.get("rule"), 0) + 1
            elif counted is False:
                u = uncounted.setdefault(r["profile"], {})
                u[r["src_ip"]] = u.get(r["src_ip"], 0) + 1
            else:
                return False, {"violations": violations}, [f"violation seq {r['seq']} without counted"]
    failures = [f"{p}/{ip}: {n} counted violations"
                for p, ips in sorted(violations.items()) for ip, n in sorted(ips.items()) if n]
    if accepted == 0:
        failures.append("the receiver accepted no message: nothing was exercised")
    details = {"violations": violations, "uncounted": uncounted,
               "counted_by_rule": by_rule, "accepted": accepted}
    return not failures, details, failures


def run(ctx):
    run_dir = ctx.simulate(SCENARIO, SEED, ctx.run_dir(f"{SCENARIO}-{SEED}"))
    ok, details, failures = check(formats.read_receiver(run_dir))
    return report.result(NAME, ok, details, seed=SEED, trace=ctx.rel(run_dir), failures=failures)
