"""tenant_pause: a tenant whose bounce rate crosses the threshold is paused (4).

Decided on the engine's events of the simulated `tenant-pause` scenario and
of its `baseline` variant (same seed and load, the bad tenant sending only to
existing mailboxes). The threshold, window and bound are those of
lab/params.json (contract 5), not the engine's configuration.

- Crossing (4.1): per tenant, the terminal results (`delivered`, `bounced`)
  in seq order, counting only those of an unpaused tenant and starting a new
  window at each `tenant_resumed`; the crossing is the first result at which
  the last `window` results are complete and their fraction of `bounced` is
  above the threshold.
- Each crossing is followed by a `tenant_paused` of the tenant within
  `pause_bound_s`, whose detail carries `bounce_rate`, `window` and
  `threshold`; there is no pause without a crossing.
- No `attempt_started` of a tenant between its `tenant_paused` and its
  `tenant_resumed`; its accepted messages all reach a terminal state, and it
  makes attempts again after resuming.
- Exactly one tenant is paused, and none in the baseline run.
- Isolation: the other tenants' `delivered` events inside the paused tenant's
  pauses, against the same intervals of the baseline run; the ratio is at
  least 1 / `isolation_margin`.
"""

import os

from verifier import formats, report

NAME = "tenant_pause"
SCENARIO = "tenant-pause"
BASELINE = "baseline"
SEED = 1
RESULTS = ("delivered", "bounced")
TERMINAL = ("delivered", "bounced", "expired", "suppressed")


def monitor(events, threshold, window, bound_ms):
    """Replays the bounce monitor of contract 4.1 over one run.

    Returns (pauses, failures): pauses are dicts with tenant, crossing and
    pause instants, the delay, the rate at the crossing, the resume instant
    (None if the run ended paused) and the attempts after it.
    """
    failures = []
    pauses = []
    state = {}
    last_at = events[-1]["at_ms"] if events else 0

    def tenant(name):
        return state.setdefault(name, {"results": [], "paused": None, "crossing": None})

    for e in events:
        t = tenant(e["tenant"])
        kind = e["kind"]
        if kind in RESULTS:
            if t["paused"] is not None:
                continue
            t["results"].append(kind == "bounced")
            last = t["results"][-window:]
            rate = sum(last) / window
            if t["crossing"] is None and len(last) == window and rate > threshold:
                t["crossing"] = {"seq": e["seq"], "at_ms": e["at_ms"], "rate": rate}
        elif kind == "tenant_paused":
            if t["paused"] is not None:
                failures.append(f"seq {e['seq']}: {e['tenant']} paused again while paused")
                continue
            c = t["crossing"]
            d = e["detail"]
            if c is None:
                failures.append(f"seq {e['seq']}: {e['tenant']} paused without crossing the threshold")
                c = {"seq": None, "at_ms": e["at_ms"], "rate": None}
            delay = e["at_ms"] - c["at_ms"]
            if delay > bound_ms:
                failures.append(f"seq {e['seq']}: {e['tenant']} paused {delay} ms after crossing at "
                                f"seq {c['seq']}, bound {bound_ms} ms")
            if d.get("threshold") != threshold or d.get("window") != window:
                failures.append(f"seq {e['seq']}: tenant_paused detail has threshold {d.get('threshold')} and "
                                f"window {d.get('window')}, want {threshold} and {window}")
            if c["rate"] is not None and d.get("bounce_rate") != c["rate"]:
                failures.append(f"seq {e['seq']}: tenant_paused bounce_rate {d.get('bounce_rate')}, "
                                f"the window had {c['rate']}")
            p = {"tenant": e["tenant"], "crossing_seq": c["seq"], "crossing_at_ms": c["at_ms"],
                 "paused_at_ms": e["at_ms"], "delay_ms": delay, "bounce_rate": d.get("bounce_rate"),
                 "resumed_at_ms": None, "attempts_after_resume": 0}
            pauses.append(p)
            t["paused"], t["crossing"], t["results"] = p, None, []
        elif kind == "tenant_resumed":
            if t["paused"] is None:
                failures.append(f"seq {e['seq']}: {e['tenant']} resumed without a pause")
                continue
            t["paused"]["resumed_at_ms"] = e["at_ms"]
            t["paused"], t["results"] = None, []
        elif kind == "attempt_started":
            if t["paused"] is not None:
                failures.append(f"seq {e['seq']}: attempt of {e['msg_id']} while {e['tenant']} is paused")
            elif pauses:
                own = [p for p in pauses if p["tenant"] == e["tenant"] and p["resumed_at_ms"] is not None]
                if own:
                    own[-1]["attempts_after_resume"] += 1
    for name, t in sorted(state.items()):
        c = t["crossing"]
        if c is not None and last_at - c["at_ms"] > bound_ms:
            failures.append(f"{name} crossed the threshold at seq {c['seq']} and was not paused "
                            f"within {bound_ms} ms")
    return pauses, failures


def lost(events, tenant):
    """Accepted messages of tenant without a terminal event."""
    accepted = [e["msg_id"] for e in events if e["kind"] == "accepted" and e["tenant"] == tenant]
    done = {e["msg_id"] for e in events if e["kind"] in TERMINAL}
    return [m for m in accepted if m not in done]


def others_delivered(events, tenant, intervals):
    """delivered events of other tenants inside the [start, end) intervals."""
    return sum(1 for e in events
               if e["kind"] == "delivered" and e["tenant"] != tenant
               and any(start <= e["at_ms"] < end for start, end in intervals))


def check(paused_run, base_run, threshold, window, bound_s, margin):
    """Returns (ok, details, failures) for the run with the bad tenant and its baseline."""
    bound_ms = int(bound_s * 1000)
    pauses, failures = monitor(paused_run, threshold, window, bound_ms)
    base_pauses, base_failures = monitor(base_run, threshold, window, bound_ms)
    failures += [f"baseline: {f}" for f in base_failures]
    if base_pauses:
        failures.append(f"baseline: {len(base_pauses)} pauses; the baseline run must have none")

    tenants = sorted({p["tenant"] for p in pauses})
    tenant = tenants[0] if len(tenants) == 1 else None
    if len(tenants) != 1:
        failures.append(f"paused tenants {tenants}: the scenario needs exactly one")
    end = paused_run[-1]["at_ms"] if paused_run else 0
    intervals = [(p["paused_at_ms"], p["resumed_at_ms"] if p["resumed_at_ms"] is not None else end)
                 for p in pauses if p["tenant"] == tenant]
    missing = lost(paused_run, tenant) if tenant else []
    if missing:
        failures.append(f"{len(missing)} accepted messages of {tenant} never reached a terminal state, "
                        f"first {missing[:5]}")
    if tenant and not any(p["attempts_after_resume"] for p in pauses if p["tenant"] == tenant):
        failures.append(f"{tenant} made no attempt after resuming")

    with_bad = others_delivered(paused_run, tenant, intervals)
    base = others_delivered(base_run, tenant, intervals)
    ratio = with_bad / base if base else None
    if ratio is None:
        failures.append("the other tenants delivered nothing during the pauses in the baseline run")
    elif ratio < 1 / margin:
        failures.append(f"the other tenants delivered {with_bad} during the pauses against {base} in the "
                        f"baseline run: ratio {ratio:.4f} < 1/{margin}")

    details = {
        "tenant": tenant,
        "threshold": threshold,
        "window": window,
        "pause_bound_s": bound_s,
        "rate_ratio_others": ratio,
        "margin": margin,
        "pauses": pauses,
        "max_pause_delay_ms": max((p["delay_ms"] for p in pauses), default=None),
        "paused_ms": sum(e - s for s, e in intervals),
        "others_delivered_in_pauses": {"with_tenant": with_bad, "baseline": base},
        "lost": len(missing),
    }
    return not failures, details, failures


def run(ctx):
    top = ctx.fresh_dir(f"{SCENARIO}-{SEED}")
    paused_dir = ctx.simulate(SCENARIO, SEED, os.path.join(top, "paused"))
    base_dir = ctx.simulate(SCENARIO, SEED, os.path.join(top, BASELINE), variant=BASELINE)
    p = ctx.params
    ok, details, failures = check(formats.read_events(os.path.join(paused_dir, "engine.db")),
                                  formats.read_events(os.path.join(base_dir, "engine.db")),
                                  p["tenant_bounce_threshold"], p["tenant_window"],
                                  p["tenant_pause_bound_s"], p["isolation_margin"])
    details["runs"] = {"paused": ctx.rel(paused_dir), "baseline": ctx.rel(base_dir)}
    return report.result(NAME, ok, details, seed=SEED, trace=ctx.rel(top), failures=failures)
