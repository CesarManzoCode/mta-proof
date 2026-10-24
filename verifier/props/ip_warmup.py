"""ip_warmup: warm-up caps per source IP, unknown IPs throttled harder, sends spread as planned (4).

Decided on the receiver log of the simulated `warmup` scenario. The plan is
the one of lab/params.json (contract 5, R-RESEND-WARMUP), not the engine's
configuration; the scenario only says which IPs the pool has and on which
plan day each one starts. Which IPs a receiver knows comes from the
`unknown_ip` rules of the scenario's profiles.

- Caps (4.1): `data_end` lines per `src_ip`, per run day (`at_ms // 86400000`)
  and per hour (`at_ms // 3600000`), never above the cap of that run day:
  plan index `start_day - 1 + day`, the last value past the end, null = no cap.
- Throttling: the fraction of 4xx among the replies with a code (`reply` and
  `data_end`) of the unknown IPs is above that of the known ones.
- Spread: on each run day, every IP's fraction of the day's `data_end` lines
  differs from its fraction of the day's daily caps by at most
  `warmup_share_tolerance`.
- Coverage: data on every day of the plan, at least three IPs on distinct
  plan days and at least one known and one unknown IP.
"""

import ipaddress
import os

from verifier import formats, report

NAME = "ip_warmup"
SCENARIO = "warmup"
SEED = 1
DAY_MS, HOUR_MS = 86_400_000, 3_600_000
MIN_STAGES = 3


def cap_at(caps, start_day, day):
    """The cap of run day `day` for an IP that starts on plan day start_day."""
    if not caps:
        return None
    return caps[max(0, min(start_day - 1 + day, len(caps) - 1))]


def scenario_pools(root, scenario):
    """The pools of the scenario's engine configuration: set.pools replaces the
    base's (3.11: arrays replace)."""
    eng = scenario.get("engine") or {}
    pools = (eng.get("set") or {}).get("pools")
    if pools is None:
        base = formats.load_json(os.path.join(root, eng.get("base") or "lab/engine.json"))
        pools = base.get("pools")
    if not pools:
        raise formats.FormatError("the warmup scenario has no pools")
    return pools


def plan_of(pools, warmup_plan):
    """{ip: {daily, hourly, start_day}} with the plan of lab/params.json."""
    plan = {}
    for pool in pools:
        for entry in pool["ips"]:
            if entry.get("warm") or entry.get("plan") is not None:
                raise formats.FormatError(
                    f"{entry['ip']}: the warmup scenario must use the plan of lab/params.json")
            plan[entry["ip"]] = {"daily": list(warmup_plan["daily"]),
                                 "hourly": list(warmup_plan["hourly"]),
                                 "start_day": entry.get("start_day") or 1}
    return plan


def known_prefixes(profiles):
    """known_ips of every unknown_ip rule, one list per rule."""
    out = []
    for prof in profiles:
        for rule in prof.get("rules", []):
            if rule.get("kind") == "unknown_ip":
                out.append([ipaddress.ip_network(p, strict=False)
                            for p in rule["params"]["known_ips"]])
    return out


def known_ips(ips, rules):
    """The IPs every unknown_ip rule knows."""
    return sorted(ip for ip in ips
                  if rules and all(any(ipaddress.ip_address(ip) in n for n in r) for r in rules))


def check(receiver, plan, known, tolerance):
    """Returns (ok, details, failures) for one run."""
    failures = []
    per_day, per_hour = {}, {}
    replies = {True: [0, 0], False: [0, 0]}  # known? -> [4xx, with a code]
    known = set(known)
    for r in receiver:
        ip = r.get("src_ip")
        if ip not in plan:
            if r["ev"] in ("connect", "data_end"):
                failures.append(f"seq {r['seq']}: {r['ev']} from {ip}, outside the pool")
            continue
        if r["ev"] == "data_end":
            day, hour = r["at_ms"] // DAY_MS, r["at_ms"] // HOUR_MS
            per_day[(ip, day)] = per_day.get((ip, day), 0) + 1
            per_hour[(ip, day, hour)] = per_hour.get((ip, day, hour), 0) + 1
        if r["ev"] in ("reply", "data_end") and isinstance(r.get("code"), int):
            bucket = replies[ip in known]
            bucket[1] += 1
            bucket[0] += 400 <= r["code"] < 500

    over_daily = over_hourly = 0
    for (ip, day), n in sorted(per_day.items()):
        cap = cap_at(plan[ip]["daily"], plan[ip]["start_day"], day)
        if cap is not None and n > cap:
            over_daily += 1
            failures.append(f"{ip} sent {n} on run day {day}, daily cap {cap}")
    for (ip, day, hour), n in sorted(per_hour.items()):
        cap = cap_at(plan[ip]["hourly"], plan[ip]["start_day"], day)
        if cap is not None and n > cap:
            over_hourly += 1
            failures.append(f"{ip} sent {n} in hour {hour} (run day {day}), hourly cap {cap}")

    rate = {}
    for group in (True, False):
        n4, n = replies[group]
        rate[group] = n4 / n if n else None
        if not n:
            failures.append(f"no replies to {'known' if group else 'unknown'} IPs")
    if rate[True] is not None and rate[False] is not None and not rate[False] > rate[True]:
        failures.append(f"unknown IPs 4xx rate {rate[False]:.6f} is not above known IPs {rate[True]:.6f}")

    days = sorted({d for _, d in per_day})
    shares, max_diff = {}, 0.0
    for day in days:
        caps = {ip: cap_at(p["daily"], p["start_day"], day) for ip, p in plan.items()}
        if any(c is None for c in caps.values()):
            failures.append(f"run day {day}: an IP has no daily cap, the planned spread is undefined")
            continue
        total = sum(per_day.get((ip, day), 0) for ip in plan)
        cap_total = sum(caps.values())
        shares[str(day)] = {}
        for ip in sorted(plan):
            sent = per_day.get((ip, day), 0)
            share, planned = sent / total, caps[ip] / cap_total
            diff = abs(share - planned)
            max_diff = max(max_diff, diff)
            shares[str(day)][ip] = {"sent": sent, "share": round(share, 6), "plan_share": round(planned, 6)}
            if diff > tolerance:
                failures.append(f"run day {day}: {ip} sent {share:.4f} of the day, plan {planned:.4f}")

    plan_days = max(len(p["daily"]) for p in plan.values())
    missing = [d for d in range(plan_days) if d not in days]
    if missing:
        failures.append(f"no data_end on run days {missing}: the run does not cover the {plan_days} plan days")
    stages = sorted({p["start_day"] for p in plan.values()})
    if len(stages) < MIN_STAGES:
        failures.append(f"IPs start on plan days {stages}: fewer than {MIN_STAGES} stages")
    if not known or not set(plan) - known:
        failures.append("the pool needs at least one known and one unknown IP")

    details = {
        "plan": plan,
        "known_ips": sorted(known),
        "throttle_rate_known": rate[True],
        "throttle_rate_unknown": rate[False],
        "replies_known": {"4xx": replies[True][0], "total": replies[True][1]},
        "replies_unknown": {"4xx": replies[False][0], "total": replies[False][1]},
        "over_daily": over_daily,
        "over_hourly": over_hourly,
        "share_tolerance": tolerance,
        "max_share_diff": round(max_diff, 6),
        "shares": shares,
    }
    return not failures, details, failures


def run(ctx):
    path = ctx.path("lab", "scenarios", SCENARIO + ".json")
    scenario = formats.load_json(path)
    plan = plan_of(scenario_pools(ctx.root, scenario), ctx.params["warmup_plan"])
    profiles_dir = ctx.path(scenario.get("profiles_dir") or "profiles")
    profiles = [formats.load_json(os.path.join(profiles_dir, name + ".json"))
                for name in scenario["profiles"]]
    known = known_ips(plan, known_prefixes(profiles))
    out = ctx.simulate(SCENARIO, SEED, ctx.run_dir(f"{SCENARIO}-{SEED}"))
    ok, details, failures = check(formats.read_receiver(out), plan, known,
                                  ctx.params["warmup_share_tolerance"])
    return report.result(NAME, ok, details, seed=SEED, trace=ctx.rel(out), failures=failures)
