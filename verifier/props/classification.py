"""classification: the engine's table decides every corpus case as expected (4)."""

import os

from verifier import formats, report

NAME = "classification"
CLASSES = ("permanent", "transient", "undetermined")
UNDETERMINED_RULE = "d14.undetermined"


def check(cases, decisions):
    """Compare the corpus (3.8) with the decisions JSONL of mtadst classify.

    Returns (ok, details, failures). The expected values come only from the
    corpus; the decisions file carries no expected values.
    """
    failures = []
    by_id = {}
    for d in decisions:
        if d.get("id") in by_id:
            failures.append(f"decision for {d.get('id')!r} repeated")
        by_id[d.get("id")] = d
    if [d.get("id") for d in decisions] != [c["id"] for c in cases]:
        failures.append("decisions are not one per corpus case in corpus order")
    matched = 0
    for c in cases:
        d = by_id.get(c["id"])
        if d is None:
            failures.append(f"{c['id']}: no decision")
            continue
        if not d.get("rule_id"):
            failures.append(f"{c['id']}: decision without rule_id")
        if d.get("class") not in CLASSES:
            failures.append(f"{c['id']}: unknown class {d.get('class')!r}")
        if d.get("rule_id") == UNDETERMINED_RULE and d.get("class") == "permanent":
            failures.append(f"{c['id']}: response without rule classified permanent")
        if c["expected_class"] == "undetermined" and d.get("class") == "permanent":
            failures.append(f"{c['id']}: case without rule classified permanent")
        got = (d.get("class"), d.get("subtype") or "")
        want = (c["expected_class"], c["expected_subtype"] or "")
        if got == want:
            matched += 1
        else:
            failures.append(f"{c['id']}: expected {want[0]}/{want[1]}, got {got[0]}/{got[1]}"
                            f" (rule {d.get('rule_id')})")
    ok = bool(cases) and matched == len(cases) and not failures
    return ok, {"cases": len(cases), "matched": matched}, failures


def run(ctx):
    out_dir = ctx.fresh_dir(NAME)
    decisions_path = os.path.join(out_dir, "decisions.jsonl")
    corpus_path = ctx.path("corpus", "classification.json")
    ctx.mtadst("classify", "--corpus", corpus_path, "--out", decisions_path)
    cases = formats.load_json(corpus_path)
    ok, details, failures = check(cases, formats.read_jsonl(decisions_path))
    details["decisions"] = ctx.rel(decisions_path)
    return report.result(NAME, ok, details, trace=ctx.rel(out_dir), failures=failures)
