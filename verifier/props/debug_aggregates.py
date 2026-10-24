"""debug_aggregates: `mtaq agg` counts exactly what the verifier counts by
SQL in the events table of the real campaign (4, 4.2).

The main query groups by (tenant, domain, provider, src_ip, kind); the
others group by each dimension alone, by all seven, and by the main five
with each filter set to the most frequent value of its column. For each
query the verifier runs its own GROUP BY on engine.db, read-only, and
compares group by group: same groups, same counts, nothing else in a row.
"""

import collections
import json
import os

from verifier import formats, report

NAME = "debug_aggregates"
DIMENSIONS = ("tenant", "domain", "provider", "mx_host", "src_ip", "msg_type", "kind")
MAIN = ("tenant", "domain", "provider", "src_ip", "kind")
# mtaq flag of each filterable column (4.2).
FILTERS = {"tenant": "--tenant", "domain": "--domain", "provider": "--provider",
           "src_ip": "--src-ip", "msg_type": "--msg-type", "kind": "--kind"}


def sql_counts(conn, by, filters):
    """{(values of by): count} of the events matching filters, by SQL."""
    cols = ", ".join(by)
    where = " AND ".join(f"{c} = ?" for c in filters)
    sql = f"SELECT {cols}, COUNT(*) FROM events" + (f" WHERE {where}" if where else "") + f" GROUP BY {cols}"
    return {tuple(r[:-1]): r[-1] for r in conn.execute(sql, tuple(filters.values()))}


def queries(conn):
    """The queries of a run: (by, filters), filters chosen from the data."""
    qs = [(MAIN, {})] + [((d,), {}) for d in DIMENSIONS] + [(DIMENSIONS, {})]
    for col in FILTERS:
        row = conn.execute(f"SELECT {col}, COUNT(*) FROM events WHERE {col} IS NOT NULL "
                           f"GROUP BY {col} ORDER BY 2 DESC, 1 LIMIT 1").fetchone()
        if row is not None:
            qs.append((MAIN, {col: row[0]}))
    return qs


def check(answers, conn):
    """answers: [{"by", "filters", "rows"}] with the rows mtaq printed.
    Returns (ok, details, failures)."""
    failures = []
    compared = mismatched = 0
    for a in answers:
        by, filters = tuple(a["by"]), a["filters"]
        label = ",".join(by) + "".join(f" {FILTERS[c]} {v}" for c, v in filters.items())
        got = collections.Counter()
        for r in a["rows"]:
            if sorted(r) != sorted(by + ("count",)) or not isinstance(r.get("count"), int):
                failures.append(f"{label}: row {r} is not the dimensions and an integer count")
                continue
            key = tuple(r[c] for c in by)
            if key in got:
                failures.append(f"{label}: group {key} printed twice")
            got[key] += r["count"]
        want = sql_counts(conn, by, filters)
        compared += len(want)
        for key in sorted(set(want) | set(got), key=repr):
            if got.get(key) != want.get(key):
                mismatched += 1
                failures.append(f"{label}: group {key}: mtaq {got.get(key)}, SQL {want.get(key)}")
    total = conn.execute("SELECT COUNT(*) FROM events").fetchone()[0]
    main = next((a for a in answers if tuple(a["by"]) == MAIN and not a["filters"]), None)
    if main is None:
        failures.append(f"no query by {','.join(MAIN)} without filters")
    elif sum(r.get("count", 0) for r in main["rows"]) != total:
        failures.append(f"the main query sums {sum(r.get('count', 0) for r in main['rows'])}, "
                        f"events has {total} rows")
    if total == 0:
        failures.append("events is empty")
    details = {
        "events": total,
        "queries": len(answers),
        "groups": compared,
        "mismatched": mismatched,
        "main_by": list(MAIN),
        "main_groups": len(main["rows"]) if main else 0,
    }
    return not failures, details, failures


def run(ctx):
    c = ctx.campaign()
    d = c["dir"]
    db = os.path.join(d, "engine.db")
    conn = formats.connect_events_db(db)
    try:
        answers = []
        for by, filters in queries(conn):
            args = ["agg", "--db", db, "--by", ",".join(by)]
            for col, v in filters.items():
                args += [FILTERS[col], v]
            answers.append({"by": list(by), "filters": filters, "rows": json.loads(ctx.mtaq(*args))})
        ok, details, failures = check(answers, conn)
    finally:
        conn.close()
    out_dir = os.path.join(ctx.lab_dir(c["seed"]), "debug")
    os.makedirs(out_dir, exist_ok=True)
    path = os.path.join(out_dir, "aggregates.json")
    with open(path, "w", encoding="utf-8") as f:
        json.dump(answers, f, indent=1)
        f.write("\n")
    details["aggregates"] = ctx.rel(path)
    return report.result(NAME, ok, details, seed=c["seed"], trace=ctx.rel(d), failures=failures)
