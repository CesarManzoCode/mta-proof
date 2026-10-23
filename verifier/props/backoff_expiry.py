"""backoff_expiry: retries back off, stop at the lifetime and end in a DSN (4).

Decided on the receiver log (when each attempt reached the receiver, who got
the DSN and with which MAIL FROM) and on the engine's events only for what
the receiver cannot see: when a message was accepted, that it expired and
which message is its DSN. The retry policy is the one of lab/params.json
(contract 5, R-5321 §4.5.4.1), not the engine's own configuration.
"""

from verifier import formats, report

NAME = "backoff_expiry"
SCENARIO = "backoff"
SEED = 1
DEFER_PROFILE = "always-defer"


def wait_ms(policy, k):
    """Wait after the k-th counted deferral: min(initial · factor^(k−1), max)."""
    return int(min(policy["initial_s"] * policy["factor"] ** (k - 1), policy["max_s"]) * 1000)


def _is_2xx(code):
    return isinstance(code, int) and 200 <= code < 300


def _dsn_ok(msg, orig_id):
    """The captured DSN is a delivery-status report about orig_id (R-3464 §2)."""
    if msg is None or msg.get_content_type() != "multipart/report":
        return False
    if msg.get_param("report-type") != "delivery-status":
        return False
    for part in msg.walk():
        if part.get_content_type() == "message/delivery-status":
            # The payload is a list of header blocks: per message, then per recipient.
            blocks = part.get_payload()
            fields = {}
            for b in blocks if isinstance(blocks, list) else []:
                for k, v in b.items():
                    fields.setdefault(k.lower(), str(v).strip())
            return (fields.get("original-envelope-id") == orig_id
                    and fields.get("action", "").lower() == "failed")
    return False


def check(receiver, events, policy, read_eml=lambda sha: None):
    """Returns (ok, details, failures) for one run against always-defer."""
    failures = []
    accepted = {}
    dsn_of = {}
    expired = {}
    for e in events:
        if e["kind"] == "accepted":
            if e["msg_id"] in accepted:
                failures.append(f"{e['msg_id']}: more than one accepted event")
            accepted[e["msg_id"]] = e
            if e["detail"].get("dsn_for"):
                dsn_of[e["detail"]["dsn_for"]] = e["msg_id"]
        elif e["kind"] == "expired":
            if e["msg_id"] in expired:
                failures.append(f"{e['msg_id']}: more than one expired event")
            expired.setdefault(e["msg_id"], e)
    originals = sorted(m for m, e in accepted.items() if not e["detail"].get("dsn_for"))

    attempts = {}
    data_ends = {}
    for r in receiver:
        if r["ev"] != "data_end":
            continue
        data_ends.setdefault(r.get("msg_id"), []).append(r)
        if r.get("profile") == DEFER_PROFILE:
            attempts.setdefault(r.get("msg_id"), []).append(r)

    intervals_all = []
    n_expired = 0
    n_dsn = 0
    total_attempts = 0
    for m in originals:
        tries = attempts.get(m, [])
        total_attempts += len(tries)
        if not tries:
            failures.append(f"{m}: never reached {DEFER_PROFILE}")
            continue
        if any(_is_2xx(t.get("code")) for t in tries):
            failures.append(f"{m}: {DEFER_PROFILE} accepted it")
        times = [t["at_ms"] for t in tries]
        intervals = [b - a for a, b in zip(times, times[1:])]
        intervals_all.extend(intervals)
        for k, iv in enumerate(intervals, 1):
            if k > 1 and iv < intervals[k - 2]:
                failures.append(f"{m}: interval {k} is {iv} ms, shorter than interval {k - 1}"
                                f" ({intervals[k - 2]} ms)")
            if iv > policy["max_s"] * 1000:
                failures.append(f"{m}: interval {k} is {iv} ms, above max_s")
            if iv < wait_ms(policy, k):
                failures.append(f"{m}: interval {k} is {iv} ms, below the policy wait"
                                f" {wait_ms(policy, k)} ms")
        deadline = accepted[m]["at_ms"] + policy["lifetime_s"] * 1000
        late = [t for t in times if t > deadline]
        if late:
            failures.append(f"{m}: {len(late)} attempts after the lifetime")
        exp = expired.get(m)
        if exp is None:
            failures.append(f"{m}: no expired event")
        else:
            after = [t for t in times if t > exp["at_ms"]]
            if after:
                failures.append(f"{m}: {len(after)} attempts after its expired event")
            elif times[-1] + wait_ms(policy, len(times)) <= deadline:
                failures.append(f"{m}: expired early; attempt {len(times) + 1} still fit"
                                f" in the lifetime")
            else:
                n_expired += 1
        dsn = dsn_of.get(m)
        sender = tries[0].get("mail_from")
        if dsn is None:
            failures.append(f"{m}: no DSN message")
            continue
        got = [r for r in data_ends.get(dsn, [])
               if _is_2xx(r.get("code")) and r.get("mail_from") == "" and r.get("rcpt") == sender]
        if not got:
            failures.append(f"{m}: DSN {dsn} not accepted by {sender!r} with MAIL FROM:<>")
        elif not _dsn_ok(read_eml(got[0].get("sha256", "")), m):
            failures.append(f"{m}: captured DSN {dsn} is not a delivery-status report for it")
        else:
            n_dsn += 1
    details = {
        "messages": len(originals),
        "attempts": total_attempts,
        "max_interval_s": max(intervals_all) / 1000 if intervals_all else None,
        "min_interval_s": min(intervals_all) / 1000 if intervals_all else None,
        "expired": n_expired,
        "dsn_null_sender": n_dsn,
        "retry_policy": dict(policy),
    }
    ok = bool(originals) and n_expired == len(originals) == n_dsn and not failures
    return ok, details, failures


def run(ctx):
    run_dir = ctx.simulate(SCENARIO, SEED, ctx.run_dir(f"{SCENARIO}-{SEED}"))
    ok, details, failures = check(
        formats.read_receiver(run_dir),
        formats.read_events(f"{run_dir}/engine.db"),
        ctx.params["retry_policy"],
        lambda sha: formats.read_eml(run_dir, sha))
    return report.result(NAME, ok, details, seed=SEED, trace=ctx.rel(run_dir), failures=failures)
