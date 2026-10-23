"""The real-mode lab inside the network namespace (contract 2.3, 4.4, D06).

Processes are only ever subprocesses: `mtasim serve` (lab DNS and MX hosts)
and `mtaengine run`. The lab mounts its own resolv.conf, talks to the
simulator's control socket and reads engine.db read-only to know when the
engine has drained. Nothing here imports engine code.
"""

import json
import os
import signal
import socket
import sqlite3
import subprocess
import threading
import time

READY_TIMEOUT_S = 30
TERMINAL = ("delivered", "bounced", "expired", "suppressed")
RESOLV_CONF = "/etc/resolv.conf"


def now_ms():
    """Wall clock in Unix milliseconds, as the three logs of a real run (3.6)."""
    return int(time.time() * 1000)


def merge(base, over):
    """Scenario-style merge (3.11): objects key by key, anything else replaces."""
    if isinstance(base, dict) and isinstance(over, dict):
        out = dict(base)
        for k, v in over.items():
            out[k] = merge(base.get(k), v) if k in base else v
        return out
    return over


def mount_resolv_conf(run_dir, path=RESOLV_CONF):
    """Bind-mount a resolv.conf pointing at the lab DNS over path (2.3).

    Only possible inside the user and mount namespace of make verify; the
    mount namespace is private, so the host never sees it."""
    conf = os.path.join(run_dir, "resolv.conf")
    with open(conf, "w", encoding="ascii") as f:
        f.write("nameserver 127.0.0.1\n")
    res = subprocess.run(["mount", "--bind", conf, path], capture_output=True, text=True)
    if res.returncode != 0:
        raise RuntimeError(f"mount --bind {conf} {path}: {res.stderr.strip()}; "
                           "the real lab runs only inside make verify's namespace")
    with open(path, encoding="ascii") as f:
        if f.read() != "nameserver 127.0.0.1\n":
            raise RuntimeError(f"{path} does not show the lab resolv.conf after the mount")
    return conf


def umount(path=RESOLV_CONF):
    subprocess.run(["umount", path], capture_output=True)


def _wait_ready(proc, what, log_path):
    """Read stdout until the process prints `ready`."""
    box = {}

    def reader():
        box["line"] = proc.stdout.readline()

    t = threading.Thread(target=reader, daemon=True)
    t.start()
    t.join(READY_TIMEOUT_S)
    if box.get("line", "").strip() != "ready":
        proc.kill()
        proc.wait()
        tail = ""
        if os.path.exists(log_path):
            with open(log_path, encoding="utf-8", errors="replace") as f:
                tail = f.read()[-2000:]
        raise RuntimeError(f"{what} did not print ready (exit {proc.returncode}): {tail}")


class Process:
    """A lab subprocess whose stdout carries only its `ready` line."""

    def __init__(self, what, cmd, cwd, log_path):
        self.what = what
        self.log_path = log_path
        log = open(log_path, "a", encoding="utf-8")
        try:
            self.proc = subprocess.Popen(cmd, cwd=cwd, stdout=subprocess.PIPE, stderr=log,
                                         stdin=subprocess.DEVNULL, text=True)
        finally:
            log.close()
        _wait_ready(self.proc, what, log_path)
        self.pid = self.proc.pid

    def signal(self, sig):
        self.proc.send_signal(sig)

    def kill9(self):
        """SIGKILL and reap; also valid on a SIGSTOPped process."""
        self.proc.send_signal(signal.SIGKILL)
        self.proc.wait()

    def stop(self, timeout=30):
        """SIGTERM and wait; SIGKILL if it does not exit in time. Returns the exit code."""
        if self.proc.poll() is None:
            self.proc.send_signal(signal.SIGTERM)
            try:
                self.proc.wait(timeout)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait()
        if self.proc.stdout:
            self.proc.stdout.close()
        return self.proc.returncode

    def alive(self):
        return self.proc.poll() is None


class Control:
    """One client of the simulator's control socket (4.4): a JSON line per
    request and per reply; held_250 notices arrive on the same connection."""

    def __init__(self, path, timeout=30):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(timeout)
        self.sock.connect(path)
        self.buf = b""
        self.notices = []

    def _line(self):
        while b"\n" not in self.buf:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise ConnectionError("control socket closed")
            self.buf += chunk
        line, self.buf = self.buf.split(b"\n", 1)
        return json.loads(line)

    def call(self, **req):
        self.sock.sendall(json.dumps(req).encode() + b"\n")
        while True:
            msg = self._line()
            if "ev" in msg:
                self.notices.append(msg)
                continue
            if not msg.get("ok"):
                raise RuntimeError(f"control {req}: {msg.get('error')}")
            return msg

    def wait_notice(self, ev, timeout):
        """The next notice ev (held_250), or None when timeout passes."""
        deadline = time.monotonic() + timeout
        while True:
            for i, n in enumerate(self.notices):
                if n.get("ev") == ev:
                    return self.notices.pop(i)
            left = deadline - time.monotonic()
            if left <= 0:
                return None
            self.sock.settimeout(left)
            try:
                self.notices.append(self._line())
            except socket.timeout:
                return None
            finally:
                self.sock.settimeout(30)

    def close(self):
        self.sock.close()


class Lab:
    """mtasim and mtaengine of one real run in run_dir (3.6)."""

    def __init__(self, root, bin_dir, run_dir, engine_config, harness):
        self.root = root
        self.bin = bin_dir
        self.run_dir = run_dir
        self.db = os.path.join(run_dir, "engine.db")
        self.control_path = os.path.join(run_dir, "control.sock")
        self.engine_config_path = os.path.join(run_dir, "engine.json")
        if "dkim" in engine_config:
            # mtasim writes this run's DKIM signing keys there (3.9).
            engine_config = merge(engine_config, {"dkim": {"key_dir": os.path.join(run_dir, "dkim")}})
        with open(self.engine_config_path, "w", encoding="utf-8") as f:
            json.dump(engine_config, f, indent=2, sort_keys=True)
            f.write("\n")
        self.harness = harness
        self.sim = None
        self.engine = None

    def start_sim(self, extra=()):
        self.sim = Process("mtasim", [
            os.path.join(self.bin, "mtasim"), "serve", "--out", self.run_dir,
            "--dns", "127.0.0.1:53", "--smtp-port", "25",
            "--profiles", "profiles", "--zone", os.path.join("lab", "zone.json"),
            "--research", os.path.join("vault", "RESEARCH.md"),
            "--control", self.control_path, *extra,
        ], self.root, os.path.join(self.run_dir, "mtasim.log"))

    def start_engine(self):
        """Start mtaengine run and log start once it serves (it recovers first)."""
        self.engine = Process("mtaengine", [
            os.path.join(self.bin, "mtaengine"), "run",
            "--config", self.engine_config_path, "--db", self.db,
        ], self.root, os.path.join(self.run_dir, "engine.log"))
        self.harness.log("start", pid=self.engine.pid)
        return self.engine

    def control(self):
        return Control(self.control_path)

    def stop(self):
        codes = {}
        if self.engine is not None:
            codes["engine"] = self.engine.stop()
        if self.sim is not None:
            codes["sim"] = self.sim.stop()
        return codes


class Harness:
    """harness.jsonl (3.5): one line per orchestrator action, flushed at once."""

    def __init__(self, path):
        self.f = open(path, "a", encoding="utf-8")
        self.lock = threading.Lock()

    def log(self, action, **fields):
        rec = {"at_ms": now_ms(), "action": action}
        rec.update({k: v for k, v in fields.items() if v is not None})
        with self.lock:
            self.f.write(json.dumps(rec, sort_keys=True, separators=(",", ":")) + "\n")
            self.f.flush()
        return rec

    def close(self):
        self.f.close()


class Pending:
    """Accepted messages without a terminal event, read incrementally from
    engine.db (read-only) while the engine writes it."""

    def __init__(self, db):
        self.db = db
        self.seq = 0
        self.accepted = set()
        self.done = set()

    def poll(self):
        if not os.path.exists(self.db):
            return None
        conn = sqlite3.connect(f"file:{self.db}?mode=ro", uri=True, timeout=10)
        try:
            rows = conn.execute("SELECT seq, msg_id, kind FROM events WHERE seq > ? ORDER BY seq",
                                (self.seq,)).fetchall()
        except sqlite3.OperationalError:
            return None
        finally:
            conn.close()
        for seq, msg_id, kind in rows:
            self.seq = seq
            if kind == "accepted":
                self.accepted.add(msg_id)
            elif kind in TERMINAL:
                self.done.add(msg_id)
        return len(self.accepted - self.done)
