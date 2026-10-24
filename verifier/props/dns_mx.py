"""dns_mx: MX hosts tried in preference order, moving on when one does not
answer; null MX bounced as NullMX with no connection and no retry (2.6,
R-5321 §5.1, R-7505 §4.1).

Decided on the real campaign: engine.db for what the engine did (routes,
tried addresses, outcomes), receiver.jsonl for what reached the receiver,
and lab/zone.json for the preference order the engine had to follow.
"""

from verifier import formats, report

NAME = "dns_mx"
NULL_MX_DOMAIN = "nullmx.test"
FALLBACK_DOMAIN = "fallback.test"


def zone_mx(zone, domain):
    """[(pref, host)] of domain, by preference; and {host: [addr]}."""
    mx = sorted((r["pref"], r["host"].rstrip(".").lower()) for r in zone["records"]
                if r["type"] == "MX" and r["name"].rstrip(".").lower() == domain)
    addrs = {}
    for r in zone["records"]:
        if r["type"] == "A":
            addrs.setdefault(r["name"].rstrip(".").lower(), []).append(r["addr"])
    return mx, addrs


def check(events, receiver, zone, null_domain=NULL_MX_DOMAIN, fallback_domain=FALLBACK_DOMAIN):
    """Returns (ok, details, failures)."""
    failures = []
    by_msg = {}
    for e in events:
        if e["msg_id"]:
            by_msg.setdefault(e["msg_id"], []).append(e)
    seen_2xx = {}
    for r in receiver:
        if r["ev"] == "rcpt" and str(r.get("rcpt", "")).lower().endswith("@" + null_domain):
            failures.append(f"receiver seq {r['seq']}: RCPT to the null MX domain on conn {r['conn']}")
        if r["ev"] == "data_end" and isinstance(r.get("code"), int) and 200 <= r["code"] < 300:
            seen_2xx.setdefault(r.get("msg_id"), r)

    # Null MX (R-7505 §4.1): bounced NullMX, no attempt, no retry, no connection.
    null_msgs = sorted(m for m, evs in by_msg.items()
                       if any(e["kind"] == "accepted" and e["domain"] == null_domain for e in evs))
    for m in null_msgs:
        kinds = [e["kind"] for e in by_msg[m]]
        bounced = [e for e in by_msg[m] if e["kind"] == "bounced"]
        if not bounced or bounced[0]["subtype"] != "NullMX":
            failures.append(f"{m}: null MX message not bounced as NullMX")
        if "attempt_started" in kinds or "deferred" in kinds:
            failures.append(f"{m}: null MX message has an attempt or a retry")
        if not any(e["kind"] == "classified" and e["rule_id"] == "rfc7505.null_mx"
                   for e in by_msg[m]):
            failures.append(f"{m}: no classified event with rule rfc7505.null_mx")
        if m in seen_2xx:
            failures.append(f"{m}: null MX message reached the receiver")

    # Fallback: the most preferred MX does not answer; the engine moves on.
    mx, addrs = zone_mx(zone, fallback_domain)
    order = [h for _, h in mx]
    preferred, served = (order[0], order[-1]) if len(order) >= 2 else (None, None)
    if preferred is None:
        failures.append(f"zone: {fallback_domain} needs at least two MX hosts")
    dead = set(addrs.get(preferred, []))
    for r in receiver:
        if r["ev"] == "connect" and r.get("dst_ip") in dead:
            failures.append(f"receiver seq {r['seq']}: a connection reached {preferred}, which must not listen")
            break
    fb_msgs = sorted(m for m, evs in by_msg.items()
                     if any(e["kind"] == "accepted" and e["domain"] == fallback_domain for e in evs))
    new_conns = delivered = 0
    for m in fb_msgs:
        evs = by_msg[m]
        for e in evs:
            if e["kind"] != "routed" or "mx" not in e["detail"]:
                continue
            new_conns += 1
            got = [h.rstrip(".").lower() for h in e["detail"]["mx"]]
            if got != order:
                failures.append(f"{m}: MX order {got}, zone preference order is {order}")
            tried = [(t["host"].rstrip(".").lower(), t["addr"].rsplit(":", 1)[0])
                     for t in e["detail"].get("tried", [])]
            want = [(preferred, a) for a in addrs.get(preferred, [])]
            if tried[:len(want)] != want:
                failures.append(f"{m}: tried {tried} before {e['mx_host']}, expected {want} first")
            if (e["mx_host"] or "").rstrip(".").lower() != served:
                failures.append(f"{m}: routed to {e['mx_host']}, expected {served}")
        if any(e["kind"] == "delivered" for e in evs):
            delivered += 1
            r = seen_2xx.get(m)
            if r is None:
                failures.append(f"{m}: delivered but the receiver has no 2xx data_end")
            elif r.get("dst_ip") not in addrs.get(served, []):
                failures.append(f"{m}: accepted at {r.get('dst_ip')}, not at {served}")
        else:
            failures.append(f"{m}: fallback message not delivered")
    if not null_msgs:
        failures.append(f"no message to the null MX domain {null_domain}")
    if new_conns == 0 or delivered == 0:
        failures.append(f"fallback domain {fallback_domain}: {new_conns} new connections, "
                        f"{delivered} delivered")
    details = {
        "null_mx_domain": null_domain,
        "fallback_domain": fallback_domain,
        "null_mx_messages": len(null_msgs),
        "fallback_messages": len(fb_msgs),
        "fallback_delivered": delivered,
        "fallback_new_connections": new_conns,
        "preferred_mx": preferred,
        "served_mx": served,
    }
    return not failures, details, failures


def run(ctx):
    c = ctx.campaign()
    d = c["dir"]
    ok, details, failures = check(formats.read_events(f"{d}/engine.db"), formats.read_receiver(d),
                                  formats.load_json(ctx.path("lab", "zone.json")))
    return report.result(NAME, ok, details, seed=c["seed"], trace=ctx.rel(d), failures=failures)
