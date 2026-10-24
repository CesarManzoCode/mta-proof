"""Synthetic run directories in the formats of contract 3.3 to 3.6."""

import hashlib
import json
import os
import sqlite3

from verifier import formats

_SCHEMA = (
    "CREATE TABLE events (seq INTEGER PRIMARY KEY, at_ms INTEGER, msg_id TEXT, tenant TEXT,"
    " kind TEXT, domain TEXT, provider TEXT, mx_host TEXT, src_ip TEXT, attempt INTEGER,"
    " code INTEGER, enhanced TEXT, text TEXT, class TEXT, subtype TEXT, rule_id TEXT,"
    " msg_type TEXT, detail TEXT)"
)


def event(seq, at_ms, msg_id, kind, detail=None, **cols):
    row = {c: None for c in formats.EVENT_COLUMNS}
    row.update(seq=seq, at_ms=at_ms, msg_id=msg_id, kind=kind, tenant="acme", **cols)
    row["detail"] = json.dumps(detail, sort_keys=True, separators=(",", ":")) if detail else None
    return row


def write_run(run_dir, receiver, events, messages=None, write_trace=True):
    """Write receiver.jsonl, engine.db, events.jsonl, trace.sha256 and messages/."""
    os.makedirs(os.path.join(run_dir, "messages"), exist_ok=True)
    rec = "".join(json.dumps(r, separators=(",", ":")) + "\n" for r in receiver).encode()
    with open(os.path.join(run_dir, "receiver.jsonl"), "wb") as f:
        f.write(rec)
    db = os.path.join(run_dir, "engine.db")
    conn = sqlite3.connect(db)
    conn.execute(_SCHEMA)
    cols = formats.EVENT_COLUMNS
    conn.executemany(f"INSERT INTO events ({','.join(cols)}) VALUES ({','.join('?' * len(cols))})",
                     [tuple(e[c] for c in cols) for e in events])
    conn.commit()
    conn.close()
    canon = formats.canonical_events(db)
    with open(os.path.join(run_dir, "events.jsonl"), "wb") as f:
        f.write(canon)
    if write_trace:
        with open(os.path.join(run_dir, "trace.sha256"), "w") as f:
            f.write(hashlib.sha256(rec + canon).hexdigest() + "\n")
    for data in messages or []:
        sha = hashlib.sha256(data).hexdigest()
        with open(os.path.join(run_dir, "messages", sha + ".eml"), "wb") as f:
            f.write(data)
    return run_dir


def dsn_bytes(orig_id, dsn_id):
    """A minimal RFC 3464 report like the engine's (2.6)."""
    b = f"mtaproof-dsn-{dsn_id}"
    return (
        "From: postmaster@engine.mtaproof.test\r\n"
        "To: notify@baseline.test\r\n"
        "Subject: Undelivered mail\r\n"
        f"X-Mtaproof-Id: {dsn_id}\r\n"
        "MIME-Version: 1.0\r\n"
        f'Content-Type: multipart/report; report-type="delivery-status"; boundary="{b}"\r\n'
        "\r\n"
        f"--{b}\r\n"
        "Content-Type: text/plain; charset=us-ascii\r\n\r\n"
        "Delivery failed.\r\n"
        f"--{b}\r\n"
        "Content-Type: message/delivery-status\r\n\r\n"
        f"Original-Envelope-Id: {orig_id}\r\n"
        "Reporting-MTA: dns; engine.mtaproof.test\r\n"
        "\r\n"
        "Final-Recipient: rfc822; user0@always-defer.test\r\n"
        "Action: failed\r\n"
        "Status: 4.3.0\r\n"
        "\r\n"
        f"--{b}--\r\n"
    ).encode()
