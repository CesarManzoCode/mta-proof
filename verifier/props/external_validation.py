"""external_validation: an unmodified Haraka against the simulator (4, D08, D19).

One real run inside the namespace. mtasim serves the lab DNS and MX hosts;
Haraka, installed by `make deps` in external/haraka/ and started with the
project's configuration (external/haraka/config/), takes mail over SMTP on
127.0.0.1:2525, relays it (relay ACL for 127.0.0.1) and resolves MX with the
namespace's resolv.conf. A local route in the namespace makes the kernel
pick SRC_IP as the source of every connection to the MX hosts, so Haraka
sends from the external MTA's range of 2.3 without any change to it.

The verifier injects the campaign's load (lab/campaign.json: its domains,
senders and rate, campaign_messages of lab/params.json; no faults), plus a
few messages to always-defer.test and to unknown mailboxes, with smtplib,
and decides only on receiver.jsonl and the captured messages:

- receiver_limits: zero counted violations per profile and source IP, with
  the run long enough to pass each profile's adaptation period;
- backoff_expiry: against always-defer, the interval between attempts is at
  least the configured temp_fail_intervals step and at most that plus
  RETRY_SLACK_MS, attempts stop after the last step and a DSN with MAIL
  FROM:<> reports the recipient as failed;
- profile_behavior: per profile, a 2xx or a 5xx ends the message (no later
  attempt), and every message ends accepted, rejected or reported in a DSN.

What needs the MTA's internal state is not evaluated (NOT_EVALUATED). No
speed is compared with the engine (D19): the details only carry verdicts and
counts.
"""

import configparser
import datetime
import hashlib
import json
import os
import random
import shutil
import signal
import smtplib
import socket
import subprocess
import time
from email.message import EmailMessage
from email.utils import format_datetime

from verifier import formats, report
from verifier.props import receiver_limits

NAME = "external_validation"
MTA = "Haraka"
HARAKA_DIR = os.path.join("external", "haraka")
LISTEN = ("127.0.0.1", 2525)
# Source of Haraka's connections, in 127.0.2.0/24 (2.3) and inside the
# known_ips of the provider profiles, like the engine's 127.0.2.1.
SRC_IP = "127.0.2.2"
MX_NET = "127.0.1.0/24"
SEED = 1
RUN = f"external-{SEED}"
RETRY_DOMAIN = "always-defer.test"
RETRY_SENDER = "retry@baseline.test"
# Supuesto: enough messages to see the whole retry schedule more than once.
RETRY_MESSAGES = 5
NOUSER_DOMAIN = "gmail-model.test"
# Supuesto: a handful of 5xx at RCPT (gmail.no_such_user, prefix nouser).
NOUSER_MESSAGES = 10
# Haraka fires its retry timers from a TimerQueue that ticks every 1 000 ms
# (outbound/queue.js); a retry also opens a connection. Supuesto: 2 s cover both.
RETRY_SLACK_MS = 2000
READY_TIMEOUT_S = 30
# Supuesto: after the load, the longest retry schedule (155 s) and its DSN fit.
DRAIN_TIMEOUT_S = 300
POLL_S = 2

EVALUATED = ("receiver_limits", "backoff_expiry", "profile_behavior")
# Properties of section 4 about the MTA that the receiver alone cannot decide.
# The suite's own (profile_sources, resources, licenses, offline) are not about
# the MTA under test and are not listed.
NOT_EVALUATED = {
    "no_loss": "needs the MTA's internal state: what it accepted and the state of each message "
               "across kills (engine.db, ingest.jsonl); from the receiver a lost message looks "
               "like a queued one",
    "bounded_duplicates": "needs injected kills and cuts and the MTA's delivered events to bound "
                          "the window of each duplicate",
    "idempotency": "needs the engine's ingest API with Idempotency-Key and its idempotency events; "
                   "Haraka takes mail over SMTP",
    "classification": "decided on the engine's classification table over the corpus (rule_id per "
                      "response); Haraka exposes no class or rule",
    "dns_mx": "needs the MTA's routing events (MX order and tried addresses); the receiver cannot "
              "see a connection to an address where nothing listens",
    "determinism": "simulated mode (mtadst, virtual clock and seeded trace of the engine)",
    "ip_warmup": "simulated mode over the engine's pools and warm-up plan",
    "tenant_pause": "simulated mode over the engine's tenants and its tenant_paused events",
    "destination_isolation": "simulated mode over the engine's accepted and delivered events",
    "tls_policy": "needs the engine's TLS policies and its tls_negotiated events",
    "outbound_auth": "needs the engine's DKIM signing and its classified and bounced events",
    "rfc5322_format": "decides on the messages the engine builds; here the injector builds them "
                      "and Haraka relays them",
    "debug_timeline": "mtaq reads the engine's event log; Haraka has none",
    "debug_aggregates": "mtaq reads the engine's event log; Haraka has none",
    "mutation_detection": "the mutants are builds of the engine",
}


def _is_2xx(code):
    return isinstance(code, int) and 200 <= code < 300


def _is_5xx(code):
    return isinstance(code, int) and 500 <= code < 600


# --- configuration -----------------------------------------------------------

def parse_intervals(text):
    """Haraka's temp_fail_intervals (outbound/config.js): comma-separated
    <number><s|m|h|d>[*<count>], each at least 5 s; returns seconds per retry."""
    units = {"s": 1, "m": 60, "h": 3600, "d": 86400}
    out = []
    for item in "".join(text.split()).lower().split(","):
        span, _, count = item.partition("*")
        if len(span) < 2 or span[-1] not in units:
            raise ValueError(f"temp_fail_intervals: bad span {item!r}")
        secs = float(span[:-1]) * units[span[-1]]
        n = int(count) if count else 1
        if secs < 5 or n < 1:
            raise ValueError(f"temp_fail_intervals: {item!r} below 5 s or without retries")
        out += [secs] * n
    if not out:
        raise ValueError("temp_fail_intervals: empty")
    return out


def read_outbound(config_dir):
    """outbound.ini as Haraka reads it: keys before any section are [main]."""
    cp = configparser.ConfigParser(inline_comment_prefixes=(";",))
    with open(os.path.join(config_dir, "outbound.ini"), encoding="utf-8") as f:
        cp.read_string("[main]\n" + f.read())
    return dict(cp["main"])


def config_digest(config_dir):
    """sha256 over the sorted file names of config_dir and the sha256 of each."""
    h = hashlib.sha256()
    files = []
    for name in sorted(os.listdir(config_dir)):
        path = os.path.join(config_dir, name)
        if not os.path.isfile(path):
            continue
        with open(path, "rb") as f:
            digest = hashlib.sha256(f.read()).hexdigest()
        h.update(f"{name}\0{digest}\n".encode())
        files.append(name)
    return h.hexdigest(), files


def installed_version(haraka_dir):
    """Version of the installed Haraka package, or None if it is not installed."""
    path = os.path.join(haraka_dir, "node_modules", "Haraka", "package.json")
    if not os.path.exists(path):
        return None
    return formats.load_json(path).get("version")


def pinned_version(haraka_dir):
    return formats.load_json(os.path.join(haraka_dir, "package.json"))["dependencies"]["Haraka"]


# --- receiver ----------------------------------------------------------------

def dsn_reports(msg):
    """[(final recipient, action)] of a delivery-status report, lowercased."""
    out = []
    if msg is None or msg.get_content_type() != "multipart/report":
        return out
    for part in msg.walk():
        if part.get_content_type() != "message/delivery-status":
            continue
        blocks = part.get_payload()
        for b in blocks if isinstance(blocks, list) else []:
            rcpt = b.get("Final-Recipient")
            if rcpt:
                addr = str(rcpt).split(";", 1)[-1].strip().lower()
                out.append((addr, str(b.get("Action", "")).strip().lower()))
    return out


def attempts(receiver, read_eml=lambda sha: None):
    """Per recipient of a non-null sender, its attempts in the receiver's order:
    {"at_ms", "conn", "profile", "src_ip", "stage", "code"}, where stage is
    rcpt (rejected at RCPT), data_end, or no_reply (the transaction ended
    without an answer). Also the DSNs: per reported recipient, the
    {"at_ms", "to", "action"} of every 2xx data_end with MAIL FROM:<>."""
    conns = {}
    out = {}
    dsns = {}
    for r in receiver:
        ev = r["ev"]
        c = conns.get(r.get("conn"))
        if ev == "connect":
            conns[r["conn"]] = {"profile": r.get("profile"), "src_ip": r.get("src_ip"),
                                "mail_from": None, "pending": [], "rcpt_wait": None}
            continue
        if c is None:
            continue
        if ev == "mail":
            for a in c["pending"]:
                a["stage"] = "no_reply"
            c["pending"], c["rcpt_wait"] = [], None
            c["mail_from"] = r.get("mail_from")
        elif ev == "rcpt":
            if c["mail_from"] == "":
                c["rcpt_wait"] = None
                continue
            a = {"at_ms": r["at_ms"], "conn": r["conn"], "profile": c["profile"],
                 "src_ip": c["src_ip"], "stage": None, "code": None}
            out.setdefault(str(r.get("rcpt", "")).lower(), []).append(a)
            c["pending"].append(a)
            c["rcpt_wait"] = a
        elif ev == "reply" and c["rcpt_wait"] is not None:
            a, c["rcpt_wait"] = c["rcpt_wait"], None
            if isinstance(r.get("code"), int) and r["code"] >= 400:
                a["stage"], a["code"] = "rcpt", r["code"]
                c["pending"].remove(a)
        elif ev == "data_end":
            for a in c["pending"]:
                a["stage"], a["code"] = "data_end", r.get("code")
            c["pending"], c["rcpt_wait"] = [], None
            if r.get("mail_from") == "" and _is_2xx(r.get("code")):
                for rcpt, action in dsn_reports(read_eml(r.get("sha256", ""))):
                    dsns.setdefault(rcpt, []).append({"at_ms": r["at_ms"], "to": r.get("rcpt"),
                                                      "action": action})
        elif ev in ("drop", "close"):
            for a in c["pending"]:
                a["stage"] = "no_reply"
            c["pending"], c["rcpt_wait"] = [], None
    for lst in out.values():
        for a in lst:
            if a["stage"] is None:  # still open when the log ends
                a["stage"] = "open"
    return out, dsns


def outcome(atts, dsns):
    """accepted, rejected (5xx), bounced (a DSN after transient attempts) or pending."""
    if any(a["stage"] == "data_end" and _is_2xx(a["code"]) for a in atts):
        return "accepted"
    if any(_is_5xx(a["code"]) for a in atts):
        return "rejected"
    if any(d["action"] == "failed" for d in dsns):
        return "bounced"
    return "pending"


def settled(injected, atts, dsns):
    return all(outcome(atts.get(m["to"], []), dsns.get(m["to"], [])) != "pending" for m in injected)


# --- checks ------------------------------------------------------------------

def check_limits(receiver, adaptation):
    """receiver_limits on the receiver log, plus coverage: every profile with
    an adaptation period saw connections from each source IP for longer than
    that period, so a violation could count. adaptation: {profile: seconds}."""
    ok, details, failures = receiver_limits.check(receiver)
    span = {}
    for r in receiver:
        if r["ev"] == "connect":
            first, _ = span.get((r["profile"], r["src_ip"]), (r["at_ms"], r["at_ms"]))
            span[(r["profile"], r["src_ip"])] = (first, r["at_ms"])
    coverage = {}
    for (prof, ip), (first, last) in sorted(span.items()):
        period = adaptation.get(prof, 0)
        observed = (last - first) / 1000
        coverage.setdefault(prof, {})[ip] = {"adaptation_period_s": period,
                                             "observed_s": round(observed, 3)}
        if period > 0 and observed <= period:
            failures.append(f"{prof}/{ip}: connections over {observed:.1f} s, not past the "
                            f"adaptation period of {period} s: no violation could count")
    details["coverage"] = coverage
    return not failures, details, failures


def check_backoff(injected, atts, dsns, intervals, slack_ms=RETRY_SLACK_MS):
    """The retry schedule against always-defer: injected are the messages to it."""
    failures = []
    fail = failures.append
    observed = []
    excess = []
    expired = n_dsn = 0
    for m in injected:
        rcpt = m["to"]
        a = atts.get(rcpt, [])
        ends = [x for x in a if x["stage"] == "data_end"]
        if len(a) != len(ends):
            fail(f"{rcpt}: attempts without a data_end answer: {[x['stage'] for x in a]}")
        if any(not (isinstance(x["code"], int) and 400 <= x["code"] < 500) for x in ends):
            fail(f"{rcpt}: an answer from always-defer that is not 4xx: {[x['code'] for x in ends]}")
        if len(ends) != len(intervals) + 1:
            fail(f"{rcpt}: {len(ends)} attempts, want {len(intervals) + 1} "
                 f"(the first and one per temp_fail_intervals step)")
        prev = None
        for k, (x, y) in enumerate(zip(ends, ends[1:])):
            gap = y["at_ms"] - x["at_ms"]
            observed.append(gap)
            if k < len(intervals):
                want = int(intervals[k] * 1000)
                excess.append(gap - want)
                if gap < want:
                    fail(f"{rcpt}: retry {k + 1} after {gap} ms, before its {want} ms")
                elif gap > want + slack_ms:
                    fail(f"{rcpt}: retry {k + 1} after {gap} ms, more than {want} + {slack_ms} ms")
            if prev is not None and gap < prev:
                fail(f"{rcpt}: retry {k + 1} after {gap} ms, shorter than the previous {prev} ms")
            prev = gap
        got = [d for d in dsns.get(rcpt, []) if d["to"] == m["from"] and d["action"] == "failed"]
        if not got:
            fail(f"{rcpt}: no DSN with MAIL FROM:<> to {m['from']} reporting it failed")
        else:
            n_dsn += 1
            if ends and got[0]["at_ms"] < ends[-1]["at_ms"]:
                fail(f"{rcpt}: DSN before the last attempt")
            elif len(ends) == len(intervals) + 1:
                expired += 1
    if not injected:
        fail("no message to always-defer")
    details = {"messages": len(injected), "intervals_s": intervals, "slack_ms": slack_ms,
               "max_interval_s": round(max(observed) / 1000, 3) if observed else None,
               "min_interval_s": round(min(observed) / 1000, 3) if observed else None,
               "max_excess_ms": max(excess) if excess else None,
               "expired": expired, "dsn_null_sender": n_dsn}
    return not failures, details, failures


def check_profiles(injected, atts, dsns):
    """Per profile: no attempt after a 2xx or a 5xx, and every message ends."""
    failures = []
    fail = failures.append
    profiles = {}
    for m in injected:
        rcpt = m["to"]
        a = atts.get(rcpt, [])
        d = dsns.get(rcpt, [])
        prof = a[0]["profile"] if a else "(none)"
        p = profiles.setdefault(prof, {"messages": 0, "attempts": 0, "accepted": 0, "rejected": 0,
                                       "bounced": 0, "pending": 0, "dsn": 0, "answers": {}})
        p["messages"] += 1
        p["attempts"] += len(a)
        p["dsn"] += 1 if d else 0
        res = outcome(a, d)
        p[res] += 1
        for x in a:
            key = str(x["code"]) if x["code"] is not None else x["stage"]
            p["answers"][key] = p["answers"].get(key, 0) + 1
        for i, x in enumerate(a):
            final = (x["stage"] == "data_end" and _is_2xx(x["code"])) or _is_5xx(x["code"])
            if final and i + 1 < len(a):
                fail(f"{rcpt}: attempt after the final {x['code']} of {prof} (conn {x['conn']})")
                break
        if res == "accepted" and any(y["action"] == "failed" for y in d):
            fail(f"{rcpt}: accepted by {prof} and reported failed in a DSN")
        if res == "pending":
            fail(f"{rcpt}: no final answer and no DSN at the end of the run")
    if not injected:
        fail("no message injected")
    return not failures, {"profiles": profiles}, failures


def check(receiver, injected, intervals, adaptation, read_eml=lambda sha: None):
    """injected: [{"i", "from", "to", "kind"}] with kind retry, nouser or load.
    Returns (ok, details, failures) over the three evaluated checks."""
    injected = [dict(m, to=m["to"].lower()) for m in injected]
    atts, dsns = attempts(receiver, read_eml)
    src_ips = sorted({r["src_ip"] for r in receiver if r["ev"] == "connect"})
    results, failures = {}, []
    runs = (
        ("receiver_limits", lambda: check_limits(receiver, adaptation)),
        ("backoff_expiry", lambda: check_backoff([m for m in injected if m["kind"] == "retry"],
                                                 atts, dsns, intervals)),
        ("profile_behavior", lambda: check_profiles(injected, atts, dsns)),
    )
    for name, fn in runs:
        ok, det, f = fn()
        det["verdict"] = "pass" if ok and not f else "fail"
        results[name] = det
        failures += [f"{name}: {x}" for x in f]
    details = {"evaluated": [n for n, _ in runs], "not_evaluated": dict(NOT_EVALUATED),
               "results": results, "src_ips": src_ips, "messages": len(injected)}
    return not failures, details, failures


# --- the run -----------------------------------------------------------------

def plan(spec, params, seed):
    """The injected messages, decided before the run (random.Random seeded
    with "external:<seed>"): first the retry and nouser messages, then the
    campaign's load, each with its sender and domain drawn by weight."""
    rng = random.Random(f"external:{seed}")
    msgs = []
    for i in range(RETRY_MESSAGES):
        msgs.append({"kind": "retry", "from": RETRY_SENDER, "to": f"retry{i}@{RETRY_DOMAIN}"})
    for i in range(NOUSER_MESSAGES):
        sender = rng.choices(spec["tenants"], weights=[t.get("weight", 1) for t in spec["tenants"]])[0]
        msgs.append({"kind": "nouser", "from": sender["from"], "to": f"nouser{i}@{NOUSER_DOMAIN}"})
    for i in range(params["campaign_messages"]):
        sender = rng.choices(spec["tenants"], weights=[t.get("weight", 1) for t in spec["tenants"]])[0]
        domain = rng.choices(spec["domains"], weights=[d.get("weight", 1) for d in spec["domains"]])[0]
        msgs.append({"kind": "load", "from": sender["from"], "to": f"user{i}@{domain['domain']}"})
    for i, m in enumerate(msgs):
        m["i"] = i
        m["msg_id"] = f"x{i:08d}"
    return msgs


def _message(m, seed):
    msg = EmailMessage()
    msg["From"] = m["from"]
    msg["To"] = m["to"]
    msg["Subject"] = f"External {seed} message {m['i']}"
    msg["Date"] = format_datetime(datetime.datetime.now(datetime.timezone.utc))
    msg["Message-ID"] = f"<{m['msg_id']}.mtaproof@{m['from'].rsplit('@', 1)[1]}>"
    msg["X-Mtaproof-Id"] = m["msg_id"]
    msg.set_content(f"Message {m['i']} of the external validation with seed {seed}.\n")
    return msg


class Haraka:
    """Haraka as a subprocess: `node <bin/haraka> -c external/haraka`, working
    directory <run>/haraka (its queue_dir is relative), output to haraka.log."""

    def __init__(self, haraka_dir, run_dir):
        self.work = os.path.join(run_dir, "haraka")
        os.makedirs(self.work, exist_ok=True)
        self.log_path = os.path.join(run_dir, "haraka.log")
        log = open(self.log_path, "a", encoding="utf-8")
        try:
            self.proc = subprocess.Popen(
                ["node", os.path.join(haraka_dir, "node_modules", "Haraka", "bin", "haraka"),
                 "-c", haraka_dir], cwd=self.work, stdout=log, stderr=subprocess.STDOUT,
                stdin=subprocess.DEVNULL)
        finally:
            log.close()
        self._wait_ready()

    def _wait_ready(self):
        deadline = time.monotonic() + READY_TIMEOUT_S
        while time.monotonic() < deadline:
            if self.proc.poll() is not None:
                break
            try:
                with socket.create_connection(LISTEN, timeout=1) as s:
                    if s.recv(512).startswith(b"220"):
                        return
            except OSError:
                pass
            time.sleep(0.2)
        self.stop()
        with open(self.log_path, encoding="utf-8", errors="replace") as f:
            tail = f.read()[-2000:]
        raise RuntimeError(f"Haraka did not answer on {LISTEN[0]}:{LISTEN[1]} "
                           f"(exit {self.proc.returncode}): {tail}")

    def alive(self):
        return self.proc.poll() is None

    def stop(self, timeout=10):
        if self.proc.poll() is None:
            self.proc.send_signal(signal.SIGTERM)
            try:
                self.proc.wait(timeout)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait()
        return self.proc.returncode


def _route(action):
    """Make the kernel choose SRC_IP for connections to the MX hosts (2.3)."""
    cmd = ["ip", "route", action, "local", MX_NET, "dev", "lo", "table", "local"]
    if action == "replace":
        cmd += ["src", SRC_IP]
    res = subprocess.run(cmd, capture_output=True, text=True)
    if res.returncode != 0 and action == "replace":
        raise RuntimeError(f"{' '.join(cmd)}: {res.stderr.strip()}; "
                           "the external run needs make verify's namespace")


def inject(msgs, rate, seed, log_path):
    """Send msgs over SMTP to Haraka: the load ones at rate per second, the
    others at once. One connection, reopened if Haraka closes it. Returns the
    injection errors (Haraka refusing or failing a message)."""
    errors = []
    smtp = None
    start = time.monotonic()
    n_load = 0
    with open(log_path, "a", encoding="utf-8") as log:
        for m in msgs:
            if m["kind"] == "load":
                due = start + n_load / rate
                n_load += 1
                wait = due - time.monotonic()
                if wait > 0:
                    time.sleep(wait)
            rec = {"i": m["i"], "msg_id": m["msg_id"], "from": m["from"], "to": m["to"],
                   "kind": m["kind"]}
            for attempt in (1, 2):
                try:
                    if smtp is None:
                        smtp = smtplib.SMTP(*LISTEN, local_hostname="injector.test", timeout=30)
                    refused = smtp.send_message(_message(m, seed))
                    rec["refused"] = {k: list(v) for k, v in refused.items()} if refused else None
                    break
                except smtplib.SMTPServerDisconnected:
                    smtp = None
                    if attempt == 2:
                        rec["error"] = "disconnected"
                except (smtplib.SMTPException, OSError) as e:
                    rec["error"] = f"{type(e).__name__}: {e}"
                    try:
                        smtp.close()
                    except Exception:  # noqa: BLE001 - already broken
                        pass
                    smtp = None
                    break
            rec["at_ms"] = int(time.time() * 1000)
            log.write(json.dumps(rec, sort_keys=True, separators=(",", ":")) + "\n")
            if rec.get("error") or rec.get("refused"):
                errors.append(f"{m['msg_id']} to {m['to']}: {rec.get('error') or rec['refused']}")
    if smtp is not None:
        try:
            smtp.quit()
        except (smtplib.SMTPException, OSError):
            pass
    return errors


def not_installed(haraka_dir):
    """Why Haraka cannot run, or None."""
    if installed_version(haraka_dir) is None:
        return (f"Haraka is not installed: {os.path.join(haraka_dir, 'node_modules', 'Haraka')} "
                "is missing; run make deps")
    if shutil.which("node") is None:
        return "Haraka needs node, which is not in PATH"
    return None


def _node_version():
    res = subprocess.run(["node", "--version"], capture_output=True, text=True)
    return res.stdout.strip() if res.returncode == 0 else None


def run(ctx):
    from verifier.campaign import load_spec  # the lab needs the namespace
    from verifier.lab import Harness, Process, mount_resolv_conf, umount

    haraka_dir = ctx.path(HARAKA_DIR)
    config_dir = os.path.join(haraka_dir, "config")
    details = {"mta": MTA, "version": installed_version(haraka_dir)}
    pinned = pinned_version(haraka_dir)
    details["pinned_version"] = pinned
    details["config_sha256"], details["config_files"] = config_digest(config_dir)
    outbound = read_outbound(config_dir)
    intervals = parse_intervals(outbound["temp_fail_intervals"])
    details["outbound"] = outbound
    why = not_installed(haraka_dir)
    if why or details["version"] != pinned:
        why = why or f"installed Haraka {details['version']}, pinned {pinned}; run make deps"
        details.update({"evaluated": [], "not_evaluated": dict(
            NOT_EVALUATED, **{n: why for n in EVALUATED})})
        return report.result(NAME, False, details, seed=SEED, failures=[why])
    details["node"] = _node_version()

    spec = load_spec(ctx.path("lab", "campaign.json"))
    msgs = plan(spec, ctx.params, SEED)
    run_dir = ctx.fresh_dir(RUN)
    adaptation = {}
    for name in os.listdir(ctx.path("profiles")):
        if name.endswith(".json"):
            prof = formats.load_json(ctx.path("profiles", name))
            adaptation[prof["name"]] = prof.get("adaptation_period_s", 0)
    harness = Harness(os.path.join(run_dir, "harness.jsonl"))
    t0 = time.monotonic()
    summary = {"seed": SEED, "messages": len(msgs), "src_ip": SRC_IP}
    sim = haraka = None
    errors = []
    eml = _eml_reader(run_dir)
    mount_resolv_conf(run_dir)
    try:
        _route("replace")
        # No control socket: this run injects no faults.
        sim = Process("mtasim", [
            os.path.join(ctx.bin, "mtasim"), "serve", "--out", run_dir,
            "--dns", "127.0.0.1:53", "--smtp-port", "25",
            "--profiles", "profiles", "--zone", os.path.join("lab", "zone.json"),
            "--research", os.path.join("vault", "RESEARCH.md"),
        ], ctx.root, os.path.join(run_dir, "mtasim.log"))
        haraka = Haraka(haraka_dir, run_dir)
        harness.log("start", pid=haraka.proc.pid)
        load_start = time.monotonic()
        errors += inject(msgs, spec["rate_per_s"], SEED, os.path.join(run_dir, "inject.jsonl"))
        summary["load_s"] = round(time.monotonic() - load_start, 3)
        drain_start = time.monotonic()
        done = False
        while time.monotonic() - drain_start < DRAIN_TIMEOUT_S:
            if not haraka.alive():
                errors.append(f"Haraka exited by itself with {haraka.proc.returncode}")
                break
            atts, dsns = attempts(formats.read_receiver(run_dir), eml)
            if settled([dict(m, to=m["to"].lower()) for m in msgs], atts, dsns):
                done = True
                break
            time.sleep(POLL_S)
        summary["drain_s"] = round(time.monotonic() - drain_start, 3)
        summary["settled"] = done
        summary["exit_codes"] = {"haraka": haraka.stop(), "sim": sim.stop()}
    finally:
        if haraka is not None and haraka.alive():
            haraka.stop()
        if sim is not None and sim.alive():
            sim.stop()
        _route("del")
        umount()
        harness.close()
    summary["duration_s"] = round(time.monotonic() - t0, 3)
    with open(os.path.join(run_dir, "external.json"), "w", encoding="utf-8") as f:
        json.dump(dict(summary, errors=errors), f, indent=2, sort_keys=True)
        f.write("\n")

    ok, more, failures = check(formats.read_receiver(run_dir), msgs, intervals, adaptation, eml)
    details.update(more)
    details["summary"] = summary
    if not summary.get("settled"):
        failures.append(f"the run did not settle in {DRAIN_TIMEOUT_S} s after the load")
    failures = [f"injection: {e}" for e in errors] + failures
    return report.result(NAME, ok, details, seed=SEED, trace=ctx.rel(run_dir), failures=failures)


def _eml_reader(run_dir):
    cache = {}

    def read(sha):
        if sha not in cache:
            cache[sha] = formats.read_eml(run_dir, sha) if sha else None
        return cache[sha]

    return read
