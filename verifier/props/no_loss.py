"""no_loss: every accepted message ends in a terminal state after the kill
campaign, restart and drain (4).

Two kinds of loss, both counted in `lost`: a msg_id the API answered 202
for (ingest.jsonl, written by the orchestrator) without an accepted event in
engine.db, and an accepted msg_id without a terminal event. A post_250 kill
only counts as such when the receiver logged the held message's 2xx
data_end before the kill and the engine had not recorded it delivered.
"""

from verifier import formats, report

NAME = "no_loss"
TERMINAL = ("delivered", "bounced", "expired", "suppressed")


def check(events, ingest, harness, receiver, params, errors=()):
    """Returns (ok, details, failures)."""
    failures = [f"campaign: {e}" for e in errors]
    accepted, terminal, delivered_at = set(), set(), {}
    for e in events:
        if e["kind"] == "accepted":
            accepted.add(e["msg_id"])
        elif e["kind"] in TERMINAL:
            terminal.add(e["msg_id"])
            if e["kind"] == "delivered":
                delivered_at.setdefault(e["msg_id"], e["at_ms"])
    acked = {r["msg_id"] for r in ingest if r.get("status") == 202 and r.get("msg_id")}
    lost_acked = sorted(acked - accepted)
    unfinished = sorted(accepted - terminal)
    failures += [f"{m}: answered 202 but has no accepted event" for m in lost_acked]
    failures += [f"{m}: accepted without a terminal event" for m in unfinished]

    first_2xx = {}
    for r in receiver:
        if (r["ev"] == "data_end" and isinstance(r.get("code"), int) and 200 <= r["code"] < 300
                and r.get("msg_id")):
            first_2xx.setdefault(r["msg_id"], r["at_ms"])
    kills = [h for h in harness if h.get("action") == "kill"]
    post = 0
    for k in kills:
        if k.get("window") != "post_250":
            continue
        m = k.get("msg_id")
        got = first_2xx.get(m)
        done = delivered_at.get(m)
        if got is None or got > k["at_ms"]:
            failures.append(f"post_250 kill at {k['at_ms']}: the receiver had not accepted {m}")
        elif done is not None and done < k["at_ms"]:
            failures.append(f"post_250 kill at {k['at_ms']}: {m} was already recorded delivered")
        else:
            post += 1

    if len(accepted) < params["campaign_messages"]:
        failures.append(f"{len(accepted)} accepted messages, fewer than campaign_messages "
                        f"{params['campaign_messages']}")
    if len(kills) < params["campaign_kills"]:
        failures.append(f"{len(kills)} kills, fewer than campaign_kills {params['campaign_kills']}")
    if post < params["campaign_kills_post_250"]:
        failures.append(f"{post} confirmed post_250 kills, fewer than campaign_kills_post_250 "
                        f"{params['campaign_kills_post_250']}")
    details = {
        "messages": len(accepted),
        "kills": len(kills),
        "kills_post_250": post,
        "lost": len(lost_acked) + len(unfinished),
        "lost_acknowledged": len(lost_acked),
        "unfinished": len(unfinished),
        "acknowledged": len(acked),
        "restarts": sum(1 for h in harness if h.get("action") == "start") - 1,
    }
    return not failures, details, failures


def run(ctx):
    c = ctx.campaign()
    d = c["dir"]
    ok, details, failures = check(
        formats.read_events(f"{d}/engine.db"), formats.read_jsonl(f"{d}/ingest.jsonl"),
        formats.read_harness(d), formats.read_receiver(d), ctx.params, c["summary"]["errors"])
    for k in ("load_s", "drain_s", "duration_s", "restart_ms_max"):
        details[k] = c["summary"].get(k)
    return report.result(NAME, ok, details, seed=c["seed"], trace=ctx.rel(d), failures=failures)
