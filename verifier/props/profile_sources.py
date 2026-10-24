"""profile_sources: every profile rule has a source or is a declared assumption (4)."""

import glob
import os
import re

from verifier import formats, report

NAME = "profile_sources"
_ROW = re.compile(r"^\|\s*(R-[A-Z0-9-]+)\s*\|(.*)\|\s*$")


def research_sources(text):
    """Map each source id of vault/RESEARCH.md to 'primary' or 'secondary'."""
    out = {}
    for line in text.splitlines():
        m = _ROW.match(line)
        if not m:
            continue
        cells = [c.strip() for c in m.group(2).split("|")]
        out[m.group(1)] = "secondary" if any(c.startswith("secundaria") for c in cells) else "primary"
    return out


def check(profiles, sources):
    """profiles: {name: parsed JSON}; sources: research_sources() output."""
    failures = []
    rules = 0
    for name in sorted(profiles):
        p = profiles[name]
        declared = p.get("sources") or []
        for s in declared:
            if s not in sources:
                failures.append(f"{name}: source {s!r} is not in RESEARCH")
        secondary = [s for s in declared if sources.get(s) == "secondary"]
        for r in p.get("rules") or []:
            rules += 1
            rid = r.get("id", "?")
            src = r.get("source")
            assumption = r.get("assumption")
            if not isinstance(assumption, bool):
                failures.append(f"{name}/{rid}: assumption is not true or false")
            if not src:
                if assumption is not True:
                    failures.append(f"{name}/{rid}: no source and not marked assumption")
                continue
            if src not in sources:
                failures.append(f"{name}/{rid}: source {src!r} is not in RESEARCH")
            if src not in declared:
                failures.append(f"{name}/{rid}: source {src!r} is not in the profile's sources")
            if sources.get(src) == "secondary" and src not in secondary:
                secondary.append(src)
        if secondary and not (p.get("limitations") or "").strip():
            failures.append(f"{name}: secondary sources {sorted(secondary)} without limitations")
    ok = bool(profiles) and rules > 0 and not failures
    return ok, {"profiles": len(profiles), "rules": rules}, failures


def run(ctx):
    with open(ctx.path("vault", "RESEARCH.md"), encoding="utf-8") as f:
        sources = research_sources(f.read())
    profiles = {}
    for path in sorted(glob.glob(ctx.path("profiles", "*.json"))):
        profiles[os.path.splitext(os.path.basename(path))[0]] = formats.load_json(path)
    ok, details, failures = check(profiles, sources)
    return report.result(NAME, ok, details, trace="profiles", failures=failures)
