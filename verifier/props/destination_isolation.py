"""destination_isolation: a nearly throttled destination does not slow the others (4).

Decided on the engine's events of the simulated `isolation` scenario (the
baseline) and of its `throttled` variant, with the same seed: an equivalent
comparison (D19), so the ratio between the two runs is a degradation. The
throttled domain is the one whose limits the variant sets.

- Per run, the time from `accepted` to the first `delivered` of every message
  to another domain; p50 and p99 by nearest rank (4.1).
- Both runs carry the same messages to the other domains, and all of them are
  delivered.
- The throttled run's p50 and p99 are at most `isolation_margin` times the
  baseline's, and the variant does slow the throttled domain (its own p99
  grows), so the comparison is not vacuous.
"""

import math
import os

from verifier import formats, report

NAME = "destination_isolation"
SCENARIO = "isolation"
VARIANT = "throttled"
SEED = 1
DSN_KEY = "dsn_for"


def percentile(values, p):
    """Nearest-rank percentile of a non-empty list."""
    s = sorted(values)
    return s[max(1, math.ceil(p / 100 * len(s))) - 1]


def delivery_times(events):
    """{msg_id: (domain, ms from accepted to the first delivered, or None)}; DSNs excluded."""
    accepted, delivered = {}, {}
    for e in events:
        if e["kind"] == "accepted" and not e["detail"].get(DSN_KEY):
            accepted[e["msg_id"]] = (e["domain"], e["at_ms"])
        elif e["kind"] == "delivered" and e["msg_id"] not in delivered:
            delivered[e["msg_id"]] = e["at_ms"]
    return {m: (dom, delivered[m] - at if m in delivered else None) for m, (dom, at) in accepted.items()}


def stats(values):
    return {"p50_ms": percentile(values, 50), "p99_ms": percentile(values, 99), "messages": len(values)}


def throttled_domain(scenario):
    """The single domain whose limits the throttled variant sets."""
    v = scenario["variants"][VARIANT]
    domains = sorted(v["engine"]["set"]["limits"]["domain"])
    if len(domains) != 1:
        raise formats.FormatError(f"variant {VARIANT} must limit exactly one domain, it limits {domains}")
    return domains[0]


def check(base_events, thr_events, domain, margin):
    """Returns (ok, details, failures) for the baseline and throttled runs."""
    failures = []
    runs = {"baseline": delivery_times(base_events), "throttled": delivery_times(thr_events)}
    others = {}
    own = {}
    for name, times in runs.items():
        others[name] = {m: t for m, (d, t) in times.items() if d != domain}
        own[name] = [t for d, t in times.values() if d == domain and t is not None]
        undelivered = sorted(m for m, t in others[name].items() if t is None)
        if undelivered:
            failures.append(f"{name}: {len(undelivered)} messages to other domains were not delivered, "
                            f"first {undelivered[:5]}")
    if sorted(others["baseline"]) != sorted(others["throttled"]):
        failures.append("the runs do not carry the same messages to the other domains: not equivalent")
    dist = {}
    for name in runs:
        values = [t for t in others[name].values() if t is not None]
        if not values:
            failures.append(f"{name}: no delivered message to the other domains")
            return False, {"throttled_domain": domain, "margin": margin}, failures
        dist[name] = stats(values)
    for q in ("p50_ms", "p99_ms"):
        if dist["throttled"][q] > margin * dist["baseline"][q]:
            failures.append(f"{q}: throttled {dist['throttled'][q]} > {margin} x baseline {dist['baseline'][q]}")
    own_p99 = {name: percentile(v, 99) if v else None for name, v in own.items()}
    if own_p99["baseline"] is None or own_p99["throttled"] is None or not own_p99["throttled"] > own_p99["baseline"]:
        failures.append(f"{domain}: p99 {own_p99['throttled']} ms throttled against {own_p99['baseline']} ms "
                        "in the baseline; the variant does not slow it")
    details = {
        "throttled_domain": domain,
        "margin": margin,
        "baseline": dist["baseline"],
        "throttled": dist["throttled"],
        "throttled_domain_p99_ms": own_p99,
    }
    return not failures, details, failures


def run(ctx):
    scenario = formats.load_json(ctx.path("lab", "scenarios", SCENARIO + ".json"))
    domain = throttled_domain(scenario)
    top = ctx.fresh_dir(f"{SCENARIO}-{SEED}")
    base_dir = ctx.simulate(SCENARIO, SEED, os.path.join(top, "baseline"))
    thr_dir = ctx.simulate(SCENARIO, SEED, os.path.join(top, VARIANT), variant=VARIANT)
    ok, details, failures = check(formats.read_events(os.path.join(base_dir, "engine.db")),
                                  formats.read_events(os.path.join(thr_dir, "engine.db")),
                                  domain, ctx.params["isolation_margin"])
    details["runs"] = {"baseline": ctx.rel(base_dir), "throttled": ctx.rel(thr_dir)}
    return report.result(NAME, ok, details, seed=SEED, trace=ctx.rel(top), failures=failures)
