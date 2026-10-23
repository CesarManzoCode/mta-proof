"""The real kill campaign (contract 3.12, 4.4): seeded load over HTTP with
idempotent resends and conflicts, kill -9 at seeded instants (some in the
post_250 window), connection cuts and a hot profile switch; then the engine
is restarted and left to drain.

Writes, in the run directory of 3.6: receiver.jsonl and messages/ (mtasim),
engine.db (mtaengine), harness.jsonl (3.5), ingest.jsonl (3.5) and
campaign.json (the summary below).
"""

import http.client
import json
import os
import random
import signal
import threading
import time

from verifier import formats
from verifier.lab import Harness, Lab, Pending, merge, mount_resolv_conf, now_ms, umount

HTTP_TIMEOUT_S = 10
RETRY_PAUSE_S = 0.05
HOLD_TIMEOUT_S = 30
# The 250 released to a stopped engine needs a moment to reach its socket.
RELEASE_SETTLE_S = 0.2

SPEC_KEYS = ("name", "description", "engine", "tenants", "domains", "rate_per_s", "workers",
             "replays", "conflicts", "cuts", "profile_switch", "fault_span", "kill_jitter_s",
             "drain_timeout_s")


def load_spec(path):
    """lab/campaign.json (3.12): exactly the keys of SPEC_KEYS."""
    spec = formats.load_json(path)
    if not isinstance(spec, dict) or sorted(spec) != sorted(SPEC_KEYS):
        raise formats.FormatError(f"{path}: needs exactly the keys {list(SPEC_KEYS)}")
    return spec


def _weighted(rng, items):
    return rng.choices(items, weights=[it.get("weight", 1) for it in items])[0]


def plan(spec, params, seed):
    """The seeded plan: every request and every fault, before anything runs.

    Draw order (random.Random seeded with "campaign:<seed>"): per message its
    tenant and domain; then the replayed and the conflicting messages; then
    the fault thresholds (in accepted originals), which kills are post_250,
    the jitter of each kill, the cut hosts and the switch threshold."""
    rng = random.Random(f"campaign:{seed}")
    n = params["campaign_messages"]
    msgs = []
    for i in range(n):
        tenant = _weighted(rng, spec["tenants"])
        domain = _weighted(rng, spec["domains"])["domain"]
        msgs.append({
            "i": i, "key": f"campaign-{seed}-{i:05d}",
            "body": {"tenant": tenant["name"], "from": tenant["from"], "to": f"user{i}@{domain}",
                     "subject": f"Campaign {seed} message {i}",
                     "text": f"Message {i} of the kill campaign with seed {seed}.\n",
                     "msg_type": "transactional"},
        })
    picked = rng.sample(range(n), spec["replays"] + spec["conflicts"])
    replays = set(picked[:spec["replays"]])
    conflicts = set(picked[spec["replays"]:])

    lo, hi = spec["fault_span"]
    kills = params["campaign_kills"]
    post = set(rng.sample(range(kills), params["campaign_kills_post_250"]))
    faults = []
    for k, at in enumerate(sorted(rng.uniform(lo, hi) for _ in range(kills))):
        faults.append({"at": int(at * n), "kind": "kill",
                       "window": "post_250" if k in post else "random",
                       "jitter_s": round(rng.uniform(0, spec["kill_jitter_s"]), 3)})
    for _ in range(spec["cuts"]["count"]):
        faults.append({"at": int(rng.uniform(lo, hi) * n), "kind": "cut",
                       "host": rng.choice(spec["cuts"]["hosts"])})
    sw = spec["profile_switch"]
    faults.append({"at": int(rng.uniform(lo, hi) * n), "kind": "switch", "host": sw["host"],
                   "profile": sw["profile"], "back": sw["back"], "hold_s": sw["hold_s"]})
    faults.sort(key=lambda f: f["at"])
    return {"messages": msgs, "replays": replays, "conflicts": conflicts, "faults": faults}


class Ingest:
    """HTTP load against the ingest API (3.10). Every request carries its
    Idempotency-Key and is resent with the same key and payload until it
    gets a final answer, so a kill between commit and reply is a replay."""

    def __init__(self, path, addr):
        host, port = addr.rsplit(":", 1)
        self.host, self.port = host, int(port)
        self.f = open(path, "a", encoding="utf-8")
        self.lock = threading.Lock()
        self.acked = 0  # originals with a final answer
        self.errors = []
        self.stop = False

    def log(self, rec):
        with self.lock:
            self.f.write(json.dumps(rec, sort_keys=True, separators=(",", ":")) + "\n")
            self.f.flush()

    def send(self, conn_box, i, kind, key, body):
        data = json.dumps(body).encode()
        tries = 0
        while not self.stop:
            tries += 1
            conn = conn_box.get("c")
            if conn is None:
                conn = conn_box["c"] = http.client.HTTPConnection(self.host, self.port,
                                                                  timeout=HTTP_TIMEOUT_S)
            try:
                conn.request("POST", "/v1/messages", body=data,
                             headers={"Content-Type": "application/json", "Idempotency-Key": key})
                resp = conn.getresponse()
                payload = resp.read()
                status = resp.status
                replayed = resp.getheader("Idempotent-Replayed") == "true"
            except (OSError, http.client.HTTPException):
                conn.close()
                conn_box["c"] = None
                time.sleep(RETRY_PAUSE_S)
                continue
            if status == 503:
                time.sleep(RETRY_PAUSE_S)
                continue
            try:
                msg_id = json.loads(payload).get("id")
            except ValueError:
                msg_id = None
            rec = {"at_ms": now_ms(), "i": i, "kind": kind, "key": key, "status": status,
                   "msg_id": msg_id, "replayed": replayed, "tries": tries}
            self.log(rec)
            if status not in (202, 409):
                self.errors.append(f"{kind} {key}: HTTP {status} {payload[:200]!r}")
            return rec
        return None

    def close(self):
        self.f.close()


def _worker(ing, pl, next_box, start, rate):
    conn_box = {}
    msgs = pl["messages"]
    while not ing.stop:
        with ing.lock:
            i = next_box["i"]
            if i >= len(msgs):
                break
            next_box["i"] = i + 1
        wait = start + i / rate - time.monotonic()
        if wait > 0:
            time.sleep(wait)
        m = msgs[i]
        rec = ing.send(conn_box, i, "new", m["key"], m["body"])
        if rec is None:
            break
        if rec["status"] == 202:
            if i in pl["replays"]:
                ing.send(conn_box, i, "replay", m["key"], m["body"])
            elif i in pl["conflicts"]:
                changed = dict(m["body"], subject=m["body"]["subject"] + " (changed)")
                ing.send(conn_box, i, "conflict", m["key"], changed)
        with ing.lock:
            ing.acked += 1
    if conn_box.get("c") is not None:
        conn_box["c"].close()


class Faults:
    """Runs the seeded fault plan against the lab while the load goes on."""

    def __init__(self, lab, harness, ingest, faults):
        self.lab, self.h, self.ing, self.faults = lab, harness, ingest, faults
        self.kills = 0
        self.kills_post_250 = 0
        self.errors = []
        self.restart_ms = []
        self.pending_back = []

    def restart(self):
        t0 = time.monotonic()
        self.lab.start_engine()
        self.restart_ms.append(round((time.monotonic() - t0) * 1000))

    def kill_random(self, f):
        time.sleep(f["jitter_s"])
        pid = self.lab.engine.pid
        self.lab.engine.kill9()
        self.h.log("kill", pid=pid, window="random")
        self.kills += 1
        self.restart()

    def kill_post_250(self, f):
        """4.4: hold_250, SIGSTOP, release of the 250, SIGKILL."""
        time.sleep(f["jitter_s"])
        ctl = self.lab.control()
        try:
            pid = self.lab.engine.pid
            ctl.call(cmd="hold_250")
            self.h.log("hold_250", pid=pid, window="post_250")
            held = ctl.wait_notice("held_250", HOLD_TIMEOUT_S)
            if held is None:
                self.errors.append(f"post_250 kill at {f['at']}: no message held in {HOLD_TIMEOUT_S} s")
                return
            self.lab.engine.signal(signal.SIGSTOP)
            res = ctl.call(cmd="release_250", msg_id=held["msg_id"])
            self.h.log("release_250", pid=pid, window="post_250", msg_id=held["msg_id"])
            if res.get("released") != 1:
                self.errors.append(f"release_250 {held['msg_id']} released {res.get('released')}")
            time.sleep(RELEASE_SETTLE_S)
            self.lab.engine.kill9()
            self.h.log("kill", pid=pid, window="post_250", msg_id=held["msg_id"])
            self.kills += 1
            self.kills_post_250 += 1
        finally:
            ctl.close()
        self.restart()

    def cut(self, host):
        ctl = self.lab.control()
        try:
            n = ctl.call(cmd="cut", **({"host": host} if host else {}))["cut"]
        finally:
            ctl.close()
        self.h.log("cut", host=host, cut=n)

    def switch(self, host, profile):
        ctl = self.lab.control()
        try:
            ctl.call(cmd="switch_profile", host=host, profile=profile)
        finally:
            ctl.close()
        self.h.log("profile_switch", host=host, profile=profile)
        # The switch applies to new connections; cutting makes it apply to all (4.4).
        self.cut(host)

    def due_backs(self, force=False):
        now = time.monotonic()
        for b in list(self.pending_back):
            if force or b["at"] <= now:
                self.switch(b["host"], b["profile"])
                self.pending_back.remove(b)

    def run(self):
        for f in self.faults:
            while self.ing.acked < f["at"] and not self.ing.stop:
                self.due_backs()
                self.check_engine()
                time.sleep(0.01)
            if self.ing.stop:
                return
            self.due_backs()
            if f["kind"] == "kill":
                (self.kill_post_250 if f["window"] == "post_250" else self.kill_random)(f)
            elif f["kind"] == "cut":
                self.cut(f["host"])
            elif f["kind"] == "switch":
                self.switch(f["host"], f["profile"])
                self.pending_back.append({"at": time.monotonic() + f["hold_s"], "host": f["host"],
                                          "profile": f["back"]})

    def check_engine(self):
        """An engine that exits on its own is not a harness kill: note it and restart."""
        if not self.lab.engine.alive():
            self.errors.append(f"engine pid {self.lab.engine.pid} exited on its own with "
                               f"{self.lab.engine.proc.returncode}")
            self.restart()


def run(ctx, seed, run_dir, spec_path=None):
    """One campaign into run_dir (emptied by the caller). Returns the summary."""
    spec = load_spec(spec_path or ctx.path("lab", "campaign.json"))
    params = ctx.params
    base = formats.load_json(ctx.path(spec["engine"]["base"]))
    cfg = merge(base, spec["engine"].get("set", {}))
    cfg["retry_policy"] = dict(params["campaign_retry_policy"])
    cfg["seed"] = seed
    pl = plan(spec, params, seed)

    t0 = time.monotonic()
    harness = Harness(os.path.join(run_dir, "harness.jsonl"))
    lab = Lab(ctx.root, ctx.bin, run_dir, cfg, harness)
    ingest = Ingest(os.path.join(run_dir, "ingest.jsonl"), cfg["http"])
    faults = Faults(lab, harness, ingest, pl["faults"])
    summary = {"seed": seed, "messages_planned": len(pl["messages"]),
               "replays_planned": len(pl["replays"]), "conflicts_planned": len(pl["conflicts"])}
    mount_resolv_conf(run_dir)
    try:
        lab.start_sim()
        lab.start_engine()
        start = time.monotonic()
        next_box = {"i": 0}
        workers = [threading.Thread(target=_worker, args=(ingest, pl, next_box, start,
                                                          spec["rate_per_s"]), daemon=True)
                   for _ in range(spec["workers"])]
        for w in workers:
            w.start()
        fault_err = []

        def fault_main():
            try:
                faults.run()
            except Exception as e:  # noqa: BLE001 - ends in the summary
                fault_err.append(f"{type(e).__name__}: {e}")
                ingest.stop = True

        ft = threading.Thread(target=fault_main, daemon=True)
        ft.start()
        ft.join()
        while any(w.is_alive() for w in workers):
            faults.due_backs()
            faults.check_engine()
            time.sleep(0.05)
        faults.due_backs(force=True)
        summary["load_s"] = round(time.monotonic() - start, 3)

        # Drain: the engine runs until every accepted message is terminal.
        pending = Pending(lab.db)
        drain_start = time.monotonic()
        left = None
        while time.monotonic() - drain_start < spec["drain_timeout_s"]:
            faults.check_engine()
            left = pending.poll()
            if left == 0:
                break
            time.sleep(0.5)
        summary["drain_s"] = round(time.monotonic() - drain_start, 3)
        summary["pending_at_end"] = left
        summary["exit_codes"] = lab.stop()
    finally:
        ingest.stop = True
        if lab.engine is not None and lab.engine.alive():
            lab.engine.stop()
        if lab.sim is not None and lab.sim.alive():
            lab.sim.stop()
        umount()
        harness.close()
        ingest.close()
    summary.update({
        "kills": faults.kills, "kills_post_250": faults.kills_post_250,
        "restart_ms_max": max(faults.restart_ms, default=None),
        "errors": fault_err + faults.errors + ingest.errors,
        "duration_s": round(time.monotonic() - t0, 3),
    })
    with open(os.path.join(run_dir, "campaign.json"), "w", encoding="utf-8") as f:
        json.dump(summary, f, indent=2, sort_keys=True)
        f.write("\n")
    return summary
