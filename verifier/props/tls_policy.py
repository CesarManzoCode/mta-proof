"""tls_policy: the STARTTLS policies of D16 against the lab's test CA (4).

Three real runs inside the namespace, one per certificate mtasim presents
(`mtasim serve --tls`): signed by another CA, signed by the test CA for a
wrong name, and valid. The test CA is new in every run; the engine trusts
only its PEM (tls.ca_file). Each run sends the same messages to a domain
with the opportunistic policy and to one with the required policy, both
served by mx1.baseline.test.

With an invalid certificate the opportunistic domain must be delivered
(over TLS, without verifying) and the required one deferred with subtype
TLSPolicy, never reaching the receiver's RCPT; with the valid one both are
delivered encrypted. tls_negotiated in engine.db says what the engine saw
and receiver.jsonl backs it: a starttls line with the handshake error, and
the tls field on every data_end received over TLS.
"""

import http.client
import json
import os
import shutil
import sqlite3
import time

from verifier import formats, report

NAME = "tls_policy"
REQUIRED_DOMAIN = "tls-required.test"
OPPORTUNISTIC_DOMAIN = "baseline.test"
INVALID = ("other-ca", "wrong-name")
VALID = "valid"
CERTS = INVALID + (VALID,)
# Messages per domain and run (supuesto: enough to reuse connections and to
# see every required message retried under campaign_retry_policy).
MESSAGES = 10
SEED = 1
RUN_TIMEOUT_S = 60
POLL_S = 0.2


def _messages(events):
    """{msg_id: domain} of the accepted messages."""
    return {e["msg_id"]: e["domain"] for e in events if e["kind"] == "accepted"}


def check_run(cert, events, receiver, required=REQUIRED_DOMAIN, opportunistic=OPPORTUNISTIC_DOMAIN):
    """Returns (details, failures) of one run."""
    failures = []
    fail = failures.append
    msgs = _messages(events)
    by_domain = {required: sorted(m for m, d in msgs.items() if d == required),
                 opportunistic: sorted(m for m, d in msgs.items() if d == opportunistic)}
    delivered = {e["msg_id"] for e in events if e["kind"] == "delivered"}
    deferred_tls = {}
    for e in events:
        if e["kind"] == "deferred" and e["subtype"] == "TLSPolicy":
            deferred_tls[e["msg_id"]] = deferred_tls.get(e["msg_id"], 0) + 1
    encrypted = {}
    for r in receiver:
        if r["ev"] == "data_end" and isinstance(r.get("code"), int) and 200 <= r["code"] < 300:
            tls = r.get("tls") or {}
            encrypted.setdefault(r.get("msg_id"), []).append(bool(tls.get("version")))
        if r["ev"] == "rcpt" and str(r.get("rcpt", "")).lower().endswith("@" + required) and cert != VALID:
            fail(f"{cert}: receiver seq {r['seq']}: RCPT to the required domain with an invalid certificate")
    starttls_failed = sum(1 for r in receiver if r["ev"] == "starttls" and (r.get("tls") or {}).get("error"))
    negotiated = [e for e in events if e["kind"] == "tls_negotiated"]

    for domain, ids in by_domain.items():
        if not ids:
            fail(f"{cert}: no message to {domain}")
    valid = cert == VALID
    for domain, ids in by_domain.items():
        must_deliver = valid or domain == opportunistic
        for m in ids:
            if must_deliver:
                if m not in delivered:
                    fail(f"{cert}: {m} to {domain} not delivered")
                elif not encrypted.get(m) or not all(encrypted[m]):
                    fail(f"{cert}: {m} to {domain} accepted by the receiver without TLS")
                if m in deferred_tls:
                    fail(f"{cert}: {m} to {domain} deferred with TLSPolicy")
            else:
                if m in delivered or m in encrypted:
                    fail(f"{cert}: {m} to the required domain delivered with an invalid certificate")
                if m not in deferred_tls:
                    fail(f"{cert}: {m} to the required domain has no deferral with subtype TLSPolicy")
    for e in negotiated:
        d = e["detail"]
        policy = d.get("policy")
        want_policy = "required" if e["domain"] == required else "opportunistic"
        if policy != want_policy:
            fail(f"{cert}: seq {e['seq']}: tls_negotiated policy {policy} for {e['domain']}")
        if valid:
            if not d.get("verified") or d.get("error") or not d.get("version") or not d.get("cipher"):
                fail(f"{cert}: seq {e['seq']}: tls_negotiated {d} with the valid certificate")
        else:
            if d.get("verified") or not d.get("error"):
                fail(f"{cert}: seq {e['seq']}: tls_negotiated {d} verified an invalid certificate")
            if policy == "required" and d.get("version"):
                fail(f"{cert}: seq {e['seq']}: required policy completed a handshake with an invalid certificate")
            if policy == "opportunistic" and not d.get("version"):
                fail(f"{cert}: seq {e['seq']}: opportunistic policy did not use TLS")
    for domain in by_domain:
        if by_domain[domain] and not any(e["domain"] == domain for e in negotiated):
            fail(f"{cert}: no tls_negotiated event for {domain}")
    if not valid and starttls_failed == 0:
        fail(f"{cert}: the receiver logged no failed handshake for the required policy")
    details = {
        "messages": {d: len(ids) for d, ids in by_domain.items()},
        "delivered": {d: sum(1 for m in ids if m in delivered) for d, ids in by_domain.items()},
        "deferred_tls_policy": sum(deferred_tls.values()),
        "tls_negotiated": len(negotiated),
        "encrypted_data_end": sum(sum(v) for v in encrypted.values()),
        "plain_data_end": sum(len(v) - sum(v) for v in encrypted.values()),
        "starttls_failed": starttls_failed,
    }
    return details, failures


def check(runs, required=REQUIRED_DOMAIN, opportunistic=OPPORTUNISTIC_DOMAIN):
    """runs: {cert: (events, receiver)}. Returns (ok, details, failures)."""
    failures = []
    per_run = {}
    for cert in CERTS:
        if cert not in runs:
            failures.append(f"no run with the {cert} certificate")
            continue
        events, receiver = runs[cert]
        per_run[cert], f = check_run(cert, events, receiver, required, opportunistic)
        failures += f
    details = {"required_domain": required, "opportunistic_domain": opportunistic, "results": per_run}
    return not failures, details, failures


def _post(addr, key, body):
    host, port = addr.rsplit(":", 1)
    data = json.dumps(body).encode()
    deadline = time.monotonic() + RUN_TIMEOUT_S
    while True:
        conn = http.client.HTTPConnection(host, int(port), timeout=10)
        try:
            conn.request("POST", "/v1/messages", body=data,
                         headers={"Content-Type": "application/json", "Idempotency-Key": key})
            resp = conn.getresponse()
            resp.read()
            if resp.status == 202:
                return
            if resp.status != 503:
                raise RuntimeError(f"ingest {key}: HTTP {resp.status}")
        except (OSError, http.client.HTTPException):
            if time.monotonic() > deadline:
                raise
        finally:
            conn.close()
        time.sleep(0.05)


def _settled(db, cert, n):
    """Whether the run reached what the property decides on: every message
    delivered, or, with an invalid certificate, the opportunistic ones
    delivered and every required one deferred at least once."""
    if not os.path.exists(db):
        return False
    conn = sqlite3.connect(f"file:{db}?mode=ro", uri=True, timeout=10)
    try:
        rows = conn.execute("SELECT msg_id, kind, domain, subtype FROM events").fetchall()
    except sqlite3.OperationalError:
        return False
    finally:
        conn.close()
    delivered = {m for m, k, _, _ in rows if k == "delivered"}
    deferred = {m for m, k, _, s in rows if k == "deferred" and s == "TLSPolicy"}
    domains = {m: d for m, k, d, _ in rows if k == "accepted"}
    opp = [m for m, d in domains.items() if d == OPPORTUNISTIC_DOMAIN]
    req = [m for m, d in domains.items() if d == REQUIRED_DOMAIN]
    if len(opp) < n or len(req) < n or not all(m in delivered for m in opp):
        return False
    if cert == VALID:
        return all(m in delivered for m in req)
    return all(m in deferred for m in req)


def real_run(ctx, cert, run_dir, seed=SEED):
    """One run with mtasim presenting cert; returns a summary."""
    from verifier.lab import Harness, Lab, merge  # the lab needs the namespace

    base = formats.load_json(ctx.path("lab", "engine.json"))
    cfg = merge(base, {"retry_policy": dict(ctx.params["campaign_retry_policy"]), "seed": seed,
                       "tls": {"ca_file": os.path.join(run_dir, "tls", "ca.pem")}})
    harness = Harness(os.path.join(run_dir, "harness.jsonl"))
    lab = Lab(ctx.root, ctx.bin, run_dir, cfg, harness)
    t0 = time.monotonic()
    summary = {"cert": cert}
    try:
        lab.start_sim(["--tls", cert])
        lab.start_engine()
        for i in range(MESSAGES):
            for domain in (OPPORTUNISTIC_DOMAIN, REQUIRED_DOMAIN):
                _post(cfg["http"], f"tls-{cert}-{domain}-{i}", {
                    "tenant": "acme", "from": "notify@baseline.test", "to": f"user{i}@{domain}",
                    "subject": f"TLS {cert} message {i}", "text": f"Message {i} of the {cert} TLS run.\n",
                    "msg_type": "transactional"})
        deadline = time.monotonic() + RUN_TIMEOUT_S
        settled = False
        while time.monotonic() < deadline:
            if _settled(lab.db, cert, MESSAGES):
                settled = True
                break
            time.sleep(POLL_S)
        summary["settled"] = settled
        summary["exit_codes"] = lab.stop()
    finally:
        if lab.engine is not None and lab.engine.alive():
            lab.engine.stop()
        if lab.sim is not None and lab.sim.alive():
            lab.sim.stop()
        harness.close()
    summary["duration_s"] = round(time.monotonic() - t0, 3)
    return summary


def run(ctx):
    trace = ctx.fresh_dir(f"tls-{SEED}")
    runs, summaries, failures = {}, {}, []
    for cert in CERTS:
        d = os.path.join(trace, cert)
        if os.path.lexists(d):
            shutil.rmtree(d)
        os.makedirs(d)
        summaries[cert] = real_run(ctx, cert, d)
        if not summaries[cert]["settled"]:
            failures.append(f"{cert}: the run did not settle in {RUN_TIMEOUT_S} s")
        runs[cert] = (formats.read_events(os.path.join(d, "engine.db")), formats.read_receiver(d))
    ok, details, more = check(runs)
    details["runs"] = {cert: ctx.rel(os.path.join(trace, cert)) for cert in CERTS}
    details["summaries"] = summaries
    return report.result(NAME, ok, details, seed=SEED, trace=ctx.rel(trace), failures=failures + more)
