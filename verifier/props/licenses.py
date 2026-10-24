"""licenses: inventory of the Go and npm dependencies and their licenses (4, D18).

The inventory (licenses/inventory.json, versioned) is generated, never written
by hand: the Go modules come from `go list -m -json all` and the license of
each one from its license file in the module cache; the npm packages from
external/haraka/package-lock.json, whose entries carry the package's own
`license` field. The property regenerates it and fails if it differs from the
versioned one or if any license is not in ALLOWED.

`python3 -m verifier inventory` writes the versioned file.
"""

import json
import os
import re
import subprocess

from verifier import report

NAME = "licenses"
INVENTORY = os.path.join("licenses", "inventory.json")
FORMAT = "mta-proof-licenses/1"
LOCKFILE = os.path.join("external", "haraka", "package-lock.json")
# Open licenses the project accepts. Widen only with OSI-approved licenses,
# noted in the CHANGELOG. "public-domain" is a dedication, not an SPDX id.
ALLOWED = ("MIT", "BSD-2-Clause", "BSD-3-Clause", "Apache-2.0", "ISC", "MPL-2.0", "0BSD",
           "Unlicense", "BlueOak-1.0.0", "Python-2.0", "public-domain")
UNKNOWN = "UNKNOWN"
# The module's license file, first match in this order (case-insensitive).
LICENSE_FILES = ("LICENSE", "LICENSE.txt", "LICENSE.md", "LICENCE", "LICENCE.txt",
                 "LICENCE.md", "COPYING", "COPYING.txt", "UNLICENSE")
# Other files that look like license notices; listed, not classified.
_NOTICE = re.compile(r"^(licen[cs]e|copying|copyright|notice|patents|unlicense)", re.I)
# The go command must never reach the network: the cache is filled by make deps.
GO_ENV = {"GOFLAGS": "-mod=readonly", "GOPROXY": "off"}


def classify(text):
    """SPDX id of a license text, by the distinctive sentences of each license."""
    t = re.sub(r"[\s*#>]+", " ", text.lower())
    if "apache license" in t and "version 2.0" in t:
        return "Apache-2.0"
    if re.search(r"mozilla public license,? version 2\.0", t):
        return "MPL-2.0"
    if "blue oak model license" in t and "1.0.0" in t:
        return "BlueOak-1.0.0"
    if "this is free and unencumbered software released into the public domain" in t:
        return "Unlicense"
    if "python software foundation license version 2" in t:
        return "Python-2.0"
    if "permission is hereby granted, free of charge, to any person obtaining a copy" in t:
        return "MIT"
    if "redistribution and use in source and binary forms" in t:
        if "advertising materials" in t:
            return UNKNOWN  # BSD-4-Clause, not in the list
        if "neither the name" in t or "may be used to endorse or promote" in t:
            return "BSD-3-Clause"
        return "BSD-2-Clause"
    if re.search(r"permission to use, copy, modify, (and/or|and) distribute this software "
                 r"for any purpose with or without fee is hereby granted", t):
        if "provided that the above copyright notice and this permission notice appear" in t:
            return "ISC"
        return "0BSD"
    if re.search(r"dedicated (\w+ ){0,3}to the public domain", t):
        return "public-domain"
    return UNKNOWN


def parse_json_stream(text):
    """The concatenated JSON objects that `go list -m -json` prints."""
    dec, i, out = json.JSONDecoder(), 0, []
    while True:
        while i < len(text) and text[i].isspace():
            i += 1
        if i >= len(text):
            return out
        obj, i = dec.raw_decode(text, i)
        out.append(obj)


def go_entry(module):
    """One inventory entry from a `go list -m -json` object (not the main module)."""
    m = module.get("Replace") or module
    entry = {"name": module["Path"], "version": m.get("Version"), "license": UNKNOWN,
             "license_file": None, "other_files": []}
    d = m.get("Dir")
    if not d or not os.path.isdir(d):
        entry["error"] = "module not in the cache (make deps)"
        return entry
    files = sorted(os.listdir(d))
    by_lower = {f.lower(): f for f in files}
    for name in LICENSE_FILES:
        f = by_lower.get(name.lower())
        if f and os.path.isfile(os.path.join(d, f)):
            with open(os.path.join(d, f), encoding="utf-8", errors="replace") as fh:
                entry["license"] = classify(fh.read())
            entry["license_file"] = f
            break
    entry["other_files"] = [f for f in files if _NOTICE.match(f) and f != entry["license_file"]]
    return entry


def go_inventory(modules):
    entries = [go_entry(m) for m in modules if not m.get("Main")]
    return sorted(entries, key=lambda e: (e["name"], e["version"] or ""))


def npm_inventory(lock):
    """One entry per (name, version) of package-lock.json v2/v3, without the root."""
    seen = {}
    for path, pkg in (lock.get("packages") or {}).items():
        if not path:
            continue
        name = pkg.get("name") or path.rsplit("node_modules/", 1)[-1]
        lic = pkg.get("license")
        entry = {"name": name, "version": pkg.get("version"),
                 "license": lic if isinstance(lic, str) and lic else UNKNOWN}
        seen[(name, entry["version"], entry["license"])] = entry
    return [seen[k] for k in sorted(seen, key=lambda k: (k[0], k[1] or "", k[2]))]


def allowed(expr):
    """Whether an SPDX expression is open: OR needs one allowed side, AND both."""
    tokens = re.findall(r"\(|\)|[^\s()]+", expr or "")
    pos = 0

    def disj():
        nonlocal pos
        ok = conj()
        while pos < len(tokens) and tokens[pos].upper() == "OR":
            pos += 1
            ok = conj() or ok
        return ok

    def conj():
        nonlocal pos
        ok = atom()
        while pos < len(tokens) and tokens[pos].upper() == "AND":
            pos += 1
            ok = atom() and ok
        return ok

    def atom():
        nonlocal pos
        if pos >= len(tokens):
            raise ValueError("unexpected end")
        tok = tokens[pos]
        pos += 1
        if tok == "(":
            ok = disj()
            if pos >= len(tokens) or tokens[pos] != ")":
                raise ValueError("missing )")
            pos += 1
            return ok
        return tok in ALLOWED

    try:
        ok = disj()
    except ValueError:
        return False
    return ok and pos == len(tokens)


def generate(root):
    """The inventory of the repository at root, as an object."""
    env = dict(os.environ, **GO_ENV)
    res = subprocess.run(["go", "list", "-m", "-json", "all"], cwd=root, env=env,
                         capture_output=True, text=True)
    if res.returncode != 0:
        raise RuntimeError(f"go list -m -json all: exit {res.returncode}: {res.stderr.strip()} "
                           "(make deps fills the module cache)")
    with open(os.path.join(root, LOCKFILE), encoding="utf-8") as f:
        lock = json.load(f)
    return {"format": FORMAT, "go": go_inventory(parse_json_stream(res.stdout)),
            "npm": npm_inventory(lock)}


def dumps(inventory):
    return json.dumps(inventory, indent=2, sort_keys=True) + "\n"


def write(root):
    path = os.path.join(root, INVENTORY)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        f.write(dumps(generate(root)))
    return path


def _keyed(inv):
    return {(eco, e["name"], e["version"]): e for eco in ("go", "npm") for e in inv.get(eco) or []}


def check(regenerated, versioned):
    """versioned: the parsed licenses/inventory.json, or None if missing."""
    failures = []
    counts = {}
    for eco in ("go", "npm"):
        entries = regenerated.get(eco) or []
        counts[eco] = len(entries)
        for e in entries:
            if not allowed(e["license"]):
                why = f" ({e['error']})" if e.get("error") else ""
                failures.append(f"{eco} {e['name']} {e['version']}: license {e['license']!r} "
                                f"is not allowed{why}")
        if not entries:
            failures.append(f"no {eco} dependencies in the inventory")
    stale = {"added": [], "removed": [], "changed": []}
    if versioned is None:
        failures.append(f"{INVENTORY} is missing; python3 -m verifier inventory writes it")
    elif dumps(versioned) != dumps(regenerated):
        new, old = _keyed(regenerated), _keyed(versioned)
        stale["added"] = [" ".join(map(str, k)) for k in sorted(new.keys() - old.keys(), key=str)]
        stale["removed"] = [" ".join(map(str, k)) for k in sorted(old.keys() - new.keys(), key=str)]
        stale["changed"] = [" ".join(map(str, k)) for k in sorted(new.keys() & old.keys(), key=str)
                            if new[k] != old[k]]
        failures.append(f"{INVENTORY} differs from the regenerated inventory: "
                        f"{len(stale['added'])} added, {len(stale['removed'])} removed, "
                        f"{len(stale['changed'])} changed; python3 -m verifier inventory rewrites it")
    by_license = {}
    for eco in ("go", "npm"):
        for e in regenerated.get(eco) or []:
            by_license[e["license"]] = by_license.get(e["license"], 0) + 1
    details = {"inventory": INVENTORY, "dependencies": counts,
               "licenses": dict(sorted(by_license.items())), "allowed": list(ALLOWED),
               "stale": stale}
    return not failures, details, failures


def run(ctx):
    regenerated = generate(ctx.root)
    out = ctx.fresh_dir("licenses")
    with open(os.path.join(out, "inventory.json"), "w", encoding="utf-8") as f:
        f.write(dumps(regenerated))
    try:
        with open(ctx.path(INVENTORY), encoding="utf-8") as f:
            versioned = json.load(f)
    except FileNotFoundError:
        versioned = None
    ok, details, failures = check(regenerated, versioned)
    details["regenerated"] = ctx.rel(os.path.join(out, "inventory.json"))
    return report.result(NAME, ok, details, trace=INVENTORY, failures=failures)
