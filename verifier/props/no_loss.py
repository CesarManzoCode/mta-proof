"""no_loss: every accepted message ends in a terminal state after the kill
campaign, restart and drain (4).

Two kinds of loss, both counted in `lost`: a msg_id the API answered 202
for (ingest.jsonl, written by the orchestrator) without an accepted event in
engine.db, and an accepted msg_id without a terminal event. A post_250 kill
only counts as such when the receiver logged the held message's 2xx
data_end before the kill and the engine had not recorded it delivered. A
post_202 kill (k3-01) comes while the harness stalls the engine's commit;
the msg_id it carries is a 202 that arrived during the stall, which must be
in ingest.jsonl. Only an engine that answers before its commit
(mut_ack_before_persist) gives one, and the kill loses that message.
"""

from verifier import campaign, formats, report

NAME = "no_loss"
TERMINAL = ("delivered", "bounced", "expired", "suppressed")


def check(events, ingest, harness, receiver, params, errors=(), kills_post_202=0):
    """Returns (ok, details, failures)."""
    failures = [f"campaign: {e}" for e in errors]
    accepted, terminal, delivered_at, accepted_key = set(), set(), {}, {}
    for e in events:
        if e["kind"] == "accepted":
            accepted.add(e["msg_id"])
            accepted_key[e["msg_id"]] = (e.get("detail") or {}).get("idempotency_key")
        elif e["kind"] in TERMINAL:
            terminal.add(e["msg_id"])
            if e["kind"] == "delivered":
                delivered_at.setdefault(e["msg_id"], e["at_ms"])
    acked = {r["msg_id"] for r in ingest if r.get("status") == 202 and r.get("msg_id")}
    # A 202 is kept only if the accepted event of its msg_id is the one of
    # its key: after a crash that lost an answered message, the engine hands
    # the same msg_id to the next message (k3-01, mut_ack_before_persist).
    lost_acked = sorted(acked - accepted)
    reused = sorted({r["msg_id"] for r in ingest if r.get("status") == 202 and r.get("msg_id") in accepted
                     and r.get("key") and accepted_key[r["msg_id"]] != r["key"]})
    failures += [f"{m}: answered 202 to another key than its accepted event's" for m in reused]
    lost_acked = sorted(set(lost_acked) | set(reused))
    unfinished = sorted(accepted - terminal)
    failures += [f"{m}: answered 202 but has no accepted event" for m in sorted(acked - accepted)]
    failures += [f"{m}: accepted without a terminal event" for m in unfinished]

    first_2xx = {}
    for r in receiver:
        if (r["ev"] == "data_end" and isinstance(r.get("code"), int) and 200 <= r["code"] < 300
                and r.get("msg_id")):
            first_2xx.setdefault(r["msg_id"], r["at_ms"])
    kills = [h for h in harness if h.get("action") == "kill"]
    post = post202 = answered_in_stall = 0
    for k in kills:
        if k.get("window") == "post_202":
            if k.get("msg_id") is not None and k["msg_id"] not in acked:
                failures.append(f"post_202 kill at {k['at_ms']}: no 202 for {k['msg_id']} in ingest.jsonl")
            else:
                post202 += 1
                answered_in_stall += k.get("msg_id") is not None
            continue
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
    if post202 < kills_post_202:
        failures.append(f"{post202} confirmed post_202 kills, fewer than kills_post_202 {kills_post_202}")
    details = {
        "messages": len(accepted),
        "kills": len(kills),
        "kills_post_250": post,
        "kills_post_202": post202,
        "answered_during_stall": answered_in_stall,
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
        formats.read_harness(d), formats.read_receiver(d), ctx.params, c["summary"]["errors"],
        campaign.load_spec(ctx.path("lab", "campaign.json"))["kills_post_202"])
    for k in ("load_s", "drain_s", "duration_s", "restart_ms_max"):
        details[k] = c["summary"].get(k)
    return report.result(NAME, ok, details, seed=c["seed"], trace=ctx.rel(d), failures=failures)
