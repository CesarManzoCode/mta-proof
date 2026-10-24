"""Readers for the raw run files of contract sections 3.3 to 3.6."""

import email
import email.policy
import hashlib
import json
import os
import sqlite3

# Columns of the events table, contract 3.3, in schema order.
EVENT_COLUMNS = (
    "seq", "at_ms", "msg_id", "tenant", "kind", "domain", "provider", "mx_host",
    "src_ip", "attempt", "code", "enhanced", "text", "class", "subtype",
    "rule_id", "msg_type", "detail",
)


class FormatError(Exception):
    """A raw file does not follow its contract format."""


def read_jsonl(path):
    """Return the objects of a JSON Lines file, one per non-empty line."""
    out = []
    with open(path, "rb") as f:
        for n, line in enumerate(f, 1):
            if not line.strip():
                continue
            try:
                obj = json.loads(line)
            except ValueError as e:
                raise FormatError(f"{path}:{n}: {e}") from None
            if not isinstance(obj, dict):
                raise FormatError(f"{path}:{n}: not a JSON object")
            out.append(obj)
    return out


def read_receiver(run_dir):
    """receiver.jsonl (3.4), in write order."""
    rows = read_jsonl(os.path.join(run_dir, "receiver.jsonl"))
    for r in rows:
        for key in ("seq", "at_ms", "ev"):
            if key not in r:
                raise FormatError(f"receiver.jsonl: line without {key!r}: {r}")
    return rows


def read_harness(run_dir):
    """harness.jsonl (3.5); a run without faults has none."""
    path = os.path.join(run_dir, "harness.jsonl")
    if not os.path.exists(path):
        return []
    return read_jsonl(path)


def connect_events_db(path):
    """Open engine.db read-only; the verifier never writes to it."""
    if not os.path.exists(path):
        raise FormatError(f"{path}: missing")
    conn = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    conn.row_factory = sqlite3.Row
    return conn


def read_events(db_path):
    """Rows of the events table (3.3) ordered by seq, with detail decoded."""
    conn = connect_events_db(db_path)
    try:
        rows = [dict(r) for r in conn.execute("SELECT * FROM events ORDER BY seq")]
    finally:
        conn.close()
    for r in rows:
        missing = [c for c in EVENT_COLUMNS if c not in r]
        if missing:
            raise FormatError(f"{db_path}: events lacks columns {missing}")
        raw = r["detail"]
        try:
            r["detail"] = json.loads(raw) if raw else {}
        except ValueError:
            raise FormatError(f"{db_path}: seq {r['seq']}: detail is not JSON") from None
    return rows


def canonical_events(db_path):
    """The bytes of events.jsonl as contract 3.6 defines them, rebuilt from engine.db."""
    conn = connect_events_db(db_path)
    try:
        parts = [
            json.dumps(dict(r), sort_keys=True, separators=(",", ":")) + "\n"
            for r in conn.execute("SELECT * FROM events ORDER BY seq")
        ]
    finally:
        conn.close()
    return "".join(parts).encode("ascii")


def trace_sha256(run_dir):
    """sha256 of receiver.jsonl followed by the canonical events, recomputed (3.6)."""
    h = hashlib.sha256()
    with open(os.path.join(run_dir, "receiver.jsonl"), "rb") as f:
        h.update(f.read())
    h.update(canonical_events(os.path.join(run_dir, "engine.db")))
    return h.hexdigest()


def read_recorded_trace(run_dir):
    """The hex in trace.sha256, or None if the file is absent."""
    path = os.path.join(run_dir, "trace.sha256")
    if not os.path.exists(path):
        return None
    with open(path, encoding="ascii") as f:
        return f.read().strip()


def read_eml(run_dir, sha256):
    """The captured message messages/<sha256>.eml, parsed; None if absent or altered."""
    path = os.path.join(run_dir, "messages", sha256 + ".eml")
    if not os.path.exists(path):
        return None
    with open(path, "rb") as f:
        data = f.read()
    if hashlib.sha256(data).hexdigest() != sha256:
        return None
    return email.message_from_bytes(data, policy=email.policy.default)


def load_json(path):
    with open(path, encoding="utf-8") as f:
        return json.load(f)
