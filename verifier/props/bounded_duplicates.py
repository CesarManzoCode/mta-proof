"""bounded_duplicates: every duplicate delivery has an injected fault in its
window (4).

A msg_id with more than one 2xx data_end in the receiver is a duplicate. It
is explained by a kill of the orchestrator after the MAIL of the transaction
that got the first 2xx and before the message's delivered event: the window
starts at that MAIL, not at the 2xx, because the receiver can finish DATA
and accept after the sender is already dead (measured in k1-07: kill logged
at ...000 ms, receiver 2xx at ...001 ms on the dead client's connection).
It is also explained by a cut of the control socket that ended the
connection of that first 2xx right after it: the receiver accepted and the
250 never reached the engine (the connection's next line is a `drop` with
rule `control.cut`, which only the control socket produces). Every copy
must carry the same bytes, because the engine sends the message it stored
at ingest.
"""

from verifier import formats, report

NAME = "bounded_duplicates"
CUT_RULE = "control.cut"


def _is_2xx(code):
    return isinstance(code, int) and 200 <= code < 300


def check(receiver, events, harness):
    """Returns (ok, details, failures)."""
    failures = []
    accepted = {}  # msg_id -> 2xx data_end lines in write order
    next_line = {}  # seq of a data_end -> the next line of its connection
    mail_of = {}  # seq of a data_end -> at_ms of the MAIL of its transaction
    last_of_conn, last_mail = {}, {}
    for r in receiver:
        conn = r.get("conn")
        prev = last_of_conn.get(conn)
        if prev is not None and prev["ev"] == "data_end":
            next_line[prev["seq"]] = r
        last_of_conn[conn] = r
        if r["ev"] == "mail":
            last_mail[conn] = r["at_ms"]
        elif r["ev"] == "data_end":
            mail_of[r["seq"]] = last_mail.get(conn, r["at_ms"])
        if r["ev"] == "data_end" and _is_2xx(r.get("code")) and r.get("msg_id"):
            accepted.setdefault(r["msg_id"], []).append(r)
    delivered = {}
    for e in events:
        if e["kind"] == "delivered":
            delivered.setdefault(e["msg_id"], e["at_ms"])
    kills = sorted(h["at_ms"] for h in harness if h.get("action") == "kill")
    cuts = [h for h in harness if h.get("action") == "cut"]

    by_kill = by_cut = 0
    outside = []
    copies = 0
    for m in sorted(accepted):
        lines = accepted[m]
        if len(lines) < 2:
            continue
        copies += len(lines) - 1
        if len({r.get("sha256") for r in lines}) != 1:
            failures.append(f"{m}: duplicate copies with different sha256")
        first = lines[0]
        end = delivered.get(m)
        start = mail_of.get(first["seq"], first["at_ms"])
        nxt = next_line.get(first["seq"])
        if cuts and nxt is not None and nxt["ev"] == "drop" and nxt.get("rule") == CUT_RULE:
            by_cut += 1
            continue
        if end is not None and any(start <= k <= end for k in kills):
            by_kill += 1
            continue
        outside.append(m)
        failures.append(f"{m}: {len(lines)} accepted copies, first at {first['at_ms']}, "
                        f"its transaction from {start}, delivered at {end}, with no kill or cut "
                        "in between")
    details = {
        "duplicates": by_kill + by_cut + len(outside),
        "duplicates_outside_window": len(outside),
        "duplicates_by_kill": by_kill,
        "duplicates_by_cut": by_cut,
        "extra_copies": copies,
        "messages_accepted_by_receiver": len(accepted),
    }
    return not failures, details, failures


def run(ctx):
    c = ctx.campaign()
    d = c["dir"]
    ok, details, failures = check(formats.read_receiver(d), formats.read_events(f"{d}/engine.db"),
                                  formats.read_harness(d))
    return report.result(NAME, ok, details, seed=c["seed"], trace=ctx.rel(d), failures=failures)
