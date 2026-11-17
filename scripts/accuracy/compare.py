#!/usr/bin/env python3
"""Compare a codewiki index of the Go standard library with go/types.

Usage:
  python3 compare.py calls      INDEX.db TRUTHDIR   (after: go run ./calls TRUTHDIR)
  python3 compare.py implements INDEX.db TRUTHDIR   (after: go run ./implements TRUTHDIR)

Only edges inside the type-checked universe are scored: go/types sees the
files of one GOOS/GOARCH, while codewiki indexes every platform's files.
Uses only the Python standard library.
"""
import collections
import os
import sqlite3
import sys


def lines(path):
    with open(path) as f:
        return {l for l in f.read().splitlines() if l}


def pct(a, b):
    return f"{a}/{b} = {100 * a / b:.2f}%" if b else "n/a"


def calls(db, truthdir):
    truth = lines(os.path.join(truthdir, "calls.txt"))
    callers = lines(os.path.join(truthdir, "callers.txt"))
    callees = callers | {e.split()[1] for e in truth}
    symbols = {q for (q,) in db.execute("SELECT qualified FROM symbols")}
    # A function declared in several files (one per OS) shares one name.
    per_os = {q for (q,) in db.execute("""SELECT qualified FROM symbols
        WHERE kind IN ('func', 'method') GROUP BY qualified HAVING COUNT(*) > 1""")}
    ours = {}
    for a, b, conf in db.execute("""SELECT s.qualified, t.qualified, e.confidence FROM edges e
            JOIN symbols s ON s.id = e.from_id JOIN symbols t ON t.id = e.to_id"""):
        if ours.get(a + " " + b) != "exact":
            ours[a + " " + b] = conf
    scored = {k: v for k, v in ours.items() if k.split()[0] in callers and k.split()[1] in callees}
    correct = {k for k in scored if k in truth}
    # vendor/ is a dependency, not indexed: a call into it is found when it
    # is an external reference to the vendored package's own path.
    external = {f"{a} {r}" for a, r in db.execute("""SELECT s.qualified, e.to_ref FROM edges e
        JOIN symbols s ON s.id = e.from_id WHERE e.to_id IS NULL""")}
    vendored = {e for e in truth if e.split()[1].startswith("vendor/")}
    vendored_found = {e for e in vendored if e.split()[0] + " " + e.split()[1][len("vendor/"):] in external}
    found = correct | vendored_found  # for recall; precision scores internal edges
    wrong = [k for k in scored if k not in truth]
    wrong_os = [k for k in wrong if k.split()[0] in per_os]
    indexed = {e for e in truth if e.split()[0] in symbols and e.split()[1] in symbols}
    print(f"truth: {len(truth)} distinct call edges; codewiki edges scored: {len(scored)}")
    print("precision:", pct(len(correct), len(scored)))
    for conf in ("exact", "name"):
        s = {k for k, v in scored.items() if v == conf}
        print(f"  {conf} edges:", pct(len(s & correct), len(s)))
    print(f"  wrong edges made by another OS's version of the caller: {len(wrong_os)} of {len(wrong)}")
    print("  precision without those:", pct(len(correct), len(scored) - len(wrong_os)))
    print("recall:", pct(len(found), len(truth)))
    print(f"  of which calls into vendor/ found as external references: {len(vendored_found)} of {len(vendored)}")
    print("  where caller and callee are indexed:", pct(len(correct & indexed), len(indexed)))
    kinds = dict(db.execute("SELECT qualified, kind FROM symbols"))
    missed = collections.Counter()
    for e in truth - found:
        a, b = e.split()
        if a not in symbols or b not in symbols:
            missed["caller or callee not indexed (generated file)"] += 1
        else:
            missed["call to a " + kinds[b]] += 1
    print("missed edges:")
    for k, v in missed.most_common():
        print(f"  {v:6d}  {k}")


def implements(db, truthdir):
    truth = lines(os.path.join(truthdir, "implements.txt"))
    ifaces = lines(os.path.join(truthdir, "interfaces.txt"))
    concrete = lines(os.path.join(truthdir, "concrete.txt"))
    symbols = {q for (q,) in db.execute("SELECT qualified FROM symbols")}
    ours = {f"{a} {b}" for a, b in db.execute("""SELECT s.qualified, t.qualified FROM type_edges e
        JOIN symbols s ON s.id = e.sub_id JOIN symbols t ON t.id = e.super_id
        WHERE e.how = 'method set'""")}
    scored = {p for p in ours if p.split()[0] in concrete and p.split()[1] in ifaces}
    correct = scored & truth
    indexed = {p for p in truth if p.split()[0] in symbols and p.split()[1] in symbols}
    print(f"truth: {len(truth)} pairs over {len(ifaces)} interfaces and {len(concrete)} concrete types")
    print("precision:", pct(len(correct), len(scored)))
    print("recall:", pct(len(correct), len(truth)))
    print("  where both types are indexed:", pct(len(correct & indexed), len(indexed)))
    for p in sorted(scored - truth):
        print("  wrong:", p)


if __name__ == "__main__":
    if len(sys.argv) != 4 or sys.argv[1] not in ("calls", "implements"):
        sys.exit(__doc__)
    {"calls": calls, "implements": implements}[sys.argv[1]](sqlite3.connect(sys.argv[2]), sys.argv[3])
