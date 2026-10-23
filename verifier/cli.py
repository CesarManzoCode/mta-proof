"""python3 -m verifier verify --out out [--only <property,...>]"""

import argparse
import datetime
import os
import sys
import time
import traceback

from verifier import context, netns, report
from verifier.props import (backoff_expiry, bounded_duplicates, classification, destination_isolation,
                            determinism, dns_mx, idempotency, ip_warmup, no_loss, profile_sources,
                            receiver_limits, resources, tenant_pause, tls_policy)

# Implemented properties, in run order. The first of no_loss,
# bounded_duplicates, idempotency, dns_mx and receiver_limits runs the real
# campaign; the others reuse it. tls_policy runs its own real runs after the
# campaign. resources goes last: it reads the memory peak and the duration
# of everything before it.
PROPERTIES = {
    m.NAME: m for m in (profile_sources, classification, backoff_expiry, determinism, ip_warmup,
                        tenant_pause, destination_isolation, no_loss, bounded_duplicates, idempotency, dns_mx, receiver_limits,
                        tls_policy, resources)
}


def select(only):
    if not only:
        return list(PROPERTIES)
    names = [n.strip() for n in only.split(",") if n.strip()]
    unknown = [n for n in names if n not in PROPERTIES]
    if unknown or not names:
        raise SystemExit(f"verifier: unknown properties {unknown}; implemented: {list(PROPERTIES)}")
    return [n for n in PROPERTIES if n in names]


def evaluate(ctx, name):
    """Run one property; an exception is a failed property, not a crashed suite."""
    try:
        return PROPERTIES[name].run(ctx)
    except Exception as e:  # noqa: BLE001 - every error must end in the report
        return report.result(name, False, {"error": f"{type(e).__name__}: {e}",
                                           "traceback": traceback.format_exc()},
                             failures=[f"{type(e).__name__}: {e}"])


def verify(args):
    names = select(args.only)
    started_at = datetime.datetime.now(datetime.timezone.utc)
    ctx = context.Context(args.root, args.out, args.bin)
    ctx.max_memory_bytes = None
    if netns.ensure_up("lo"):
        print("verifier: brought lo up", file=sys.stderr)
    props = []
    for name in names:
        print(f"verifier: {name} ...", file=sys.stderr, flush=True)
        props.append(evaluate(ctx, name))
    rep = report.build(ctx.root, started_at, time.monotonic() - ctx.started,
                       ctx.max_memory_bytes, ctx.params["memory_limit_bytes"], props)
    path = os.path.join(ctx.out, "report.json")
    report.write(path, rep)
    for p in props:
        print(f"{p['verdict']:4}  {p['name']}")
    failed = [p for p in props if p["verdict"] != "pass"]
    for p in failed:
        print(f"FAILED {p['name']}:")
        for f in p["details"].get("failures", []):
            print(f"  - {f}")
    print(f"verdict {rep['verdict']}: {ctx.rel(path)}")
    return 0 if not failed else 1


def main(argv=None):
    parser = argparse.ArgumentParser(prog="python3 -m verifier")
    sub = parser.add_subparsers(dest="cmd", required=True)
    v = sub.add_parser("verify", help="run the implemented properties and write out/report.json")
    v.add_argument("--out", required=True, help="output directory, relative to --root")
    v.add_argument("--only", help="comma-separated property names")
    v.add_argument("--root", default=".", help="repository root")
    v.add_argument("--bin", help="directory with mtadst (default <root>/bin)")
    args = parser.parse_args(argv)
    return verify(args)
